#!/usr/bin/env python3
"""Prove the workspace service's template registry guards by breaking them
one at a time.

Run from any directory:
  python3 scripts/prove-template-registry-guards.py --report report.json

The guards: the first-start import skips an entry only when the template
service refuses its address (invalid-address); any other error reply, a
fault included, fails the start and records no mark, naming the entry by
its id and not by the reply's message, which could quote the address; each
read of an address is bounded by what remains of the import's wait, whose
end fails the start, and an unreachable service is asked again within it.
A check alone, of a registration or of a template's upgrade, stores nothing
and answers 200 with no Location. Each mutation changes one guard in place,
and its tests must fail; the source is restored even on interruption. The
report starts failed and passes only after every baseline test passes and
every mutant compiles and fails each of its tests.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parent.parent

# Each test: a name, then the package and the one test it runs.
TESTS = {
    "import-fault": ("./backend/internal/registry", "TestATemplateServiceFaultFailsTheStartAndImportsLater"),
    "import-once": ("./backend/internal/registry", "TestTheTemplatesKeyIsImportedOnceAsOneTemplatePerSource"),
    "import-never-answers": ("./backend/internal/registry", "TestATemplateServiceThatNeverAnswersFailsTheStartWithinTheWait"),
    "import-comes-up": ("./backend/internal/registry", "TestATemplateServiceThatComesUpDuringTheWaitIsAskedAgain"),
    "check-register": ("./backend/internal/registry", "TestACheckAloneStoresNothingAndARegistrationChecksAgain"),
    "check-upgrade": ("./backend/internal/registry", "TestACheckOfAnUpgradeStoresNothingAndTheUpgradeChecksAgain"),
    "check-api": ("./backend/internal/api", "TestACheckRegistersAndUpgradesNothing"),
}

IMPORT = "backend/internal/registry/import.go"
REGISTRY = "backend/internal/registry/registry.go"
API = "backend/internal/api/templates.go"
STORE = "backend/internal/store/store.go"

# Each mutant: a name, the source file, the text it replaces (exactly once),
# its replacement, and the tests that must each fail.
MUTANTS = [
    ("skip-only-refusals", IMPORT,
     "\t\tcase errors.As(err, &answered) && answered.Kind == templates.KindInvalidAddress:\n",
     "\t\tcase errors.As(err, &answered):\n", ["import-fault"]),
    ("fault-names-no-message", IMPORT,
     "\t\t\treturn fmt.Errorf(\"importing the templates key: the template service failed reading the address of entry %s (%d %s); the next start imports\", entry.ID, answered.Status, answered.Kind)\n",
     "\t\t\treturn fmt.Errorf(\"importing the templates key: %w\", err)\n", ["import-fault"]),
    ("refusal-is-skipped", IMPORT,
     "\t\t\tlogf(\"menagerie: the templates entry %s is not imported: the template service refuses its address (%s)\", entry.ID, answered.Kind)\n\t\t\tcontinue\n",
     "\t\t\treturn err\n", ["import-once"]),
    ("wait-bounds-each-read", IMPORT,
     "\t\tcall, cancel := context.WithDeadline(ctx, deadline)\n",
     "\t\tcall, cancel := context.WithCancel(ctx)\n", ["import-never-answers"]),
    ("wait-retries-unreachable", IMPORT,
     "\t\tif !errors.As(err, &unavailable) {\n",
     "\t\tif !errors.As(err, &unavailable) || true {\n", ["import-comes-up"]),
    ("wait-end-fails-start", IMPORT,
     "\t\tif !time.Now().Before(deadline) {\n",
     "\t\tif false {\n", ["import-never-answers"]),
    ("check-registers-nothing", REGISTRY,
     "\tif check {\n\t\trecord = r.Data.TryRegisterTemplate\n\t}\n", "", ["check-register", "check-api"]),
    ("check-upgrades-nothing", REGISTRY,
     "\tif check {\n\t\tadvance = r.Data.TryAdvanceTemplate\n\t}\n", "", ["check-upgrade", "check-api"]),
    ("check-answers-no-location", API,
     "\tif req.Check {\n\t\t// The checks passed; nothing was stored.\n\t\twriteJSON(w, http.StatusOK, templateOf(tpl))\n\t\treturn\n\t}\n",
     "", ["check-api"]),
    ("dry-rolls-back", STORE,
     "\treturn fn(&Tx{ctx: ctx, tx: tx})\n}\n\nfunc (t *Tx) exec(",
     "\tif err := fn(&Tx{ctx: ctx, tx: tx}); err != nil {\n\t\treturn err\n\t}\n\treturn tx.Commit()\n}\n\nfunc (t *Tx) exec(",
     ["check-register", "check-upgrade"]),
]


def run(package, test):
    command = ["nice", "-n", "15", "go", "test", "-p", "2", "-count=1", package,
               "-run", "^" + test + "$", "-v"]
    env = dict(os.environ, GOFLAGS=os.environ.get("GOFLAGS", "-p=2"))
    try:
        result = subprocess.run(command, cwd=ROOT, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=900, check=False)
        return {"exitCode": result.returncode, "output": result.stdout[-20000:]}
    except subprocess.TimeoutExpired:
        return {"exitCode": None, "output": "test process timed out"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    report = {"outcome": "failed", "baseline": [], "mutants": []}

    def save():
        args.report.write_text(json.dumps(report, indent=2) + "\n")

    save()
    for name, (package, test) in TESTS.items():
        result = run(package, test)
        report["baseline"].append({"test": name, **result})
        save()
        if result["exitCode"] != 0 or f"--- PASS: {test}" not in result["output"]:
            raise SystemExit(f"Baseline {name} failed: {result['output']}")
    for name, path, before, after, tests in MUTANTS:
        source = ROOT / path
        original = source.read_text()
        if original.count(before) != 1:
            raise SystemExit(f"Mutation anchor {name} must occur exactly once in {path}")
        source.write_text(original.replace(before, after))
        try:
            for test in tests:
                package, function = TESTS[test]
                result = run(package, function)
                killed = (result["exitCode"] is not None and result["exitCode"] != 0
                          and f"--- FAIL: {function}" in result["output"])
                report["mutants"].append({"name": name, "test": test,
                                          "killed": killed, **result})
                save()
                if not killed:
                    raise SystemExit(f"Mutant {name}/{test} survived or failed to compile: "
                                     + result["output"])
        finally:
            source.write_text(original)
    report["outcome"] = "passed"
    save()
    print(f"TEMPLATE_REGISTRY_GUARDS_PASSED: {len(report['mutants'])} mutant runs rejected")


if __name__ == "__main__":
    main()
