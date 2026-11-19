#!/usr/bin/env python3
"""Prove the guards of template version checks, applied versions and
forward-only applies by breaking them one at a time.

Run from any directory, with the web package installed and, for the
template service's guards, cargo:
  python3 scripts/prove-template-version-guards.py --report report.json

The guards, by the requirement they keep:
- checks: Menagerie finds a new version on its schedule, at start and at
  each tick, and on request; a check replaces the found versions, and a
  failed one keeps them and records why, template-service-unavailable for a
  fault, an unreachable service or no answer within the check's wait; two
  checks of one template share one run that a leaving caller does not end;
  a check on request answers 200 with the failure recorded.
- moved: a check that finds the current version's tag at another commit
  shows it moved, never replacing the recorded commit; a create and an
  upgrade are refused template-version-moved while it is; the template
  takes that version again, with the new commit, only while it is moved,
  and the screen offers it through Update version.
- descent: an upgrade of a workspace whose applied version is unknown goes
  on only when the template service answers that the current version's
  commit is, or descends from, its applied commit, at the recorded commit,
  and fails as template-refused otherwise, also when no commit is known.
- apply: Apply branch refuses, as not-applicable with nothing changed, a
  record of a workspace with no registered template, a removed, linked or
  unreadable record, another source, a record no version gives, an older
  version, and an unknown applied version whose record's commit does not
  descend or is not known; and records a forward record's version.
- import: the first start records each workspace's applied version from
  its record once, keeping a known one, and completes an unknown one from
  the found versions.

Each mutation changes one guard in place, and every test it names must
fail; the report names, for each, the assertion that failed. The source is
restored even on interruption. The report starts failed and passes only
after every baseline test passes and every mutant compiles and fails each
of its tests.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parent.parent

# Each test: a name, then the package and the one test it runs. A package
# under web/ is a Vitest file, run from web/; one under template-service/ is
# a Cargo integration test target, run from template-service/.
TESTS = {
    "schedule": ("./backend/internal/registry", "TestTheScheduleChecksEveryTemplateAtStartAndEachTick"),
    "check-finds": ("./backend/internal/registry", "TestACheckFindsANewVersionThatTheTemplateCanTake"),
    "check-fails": ("./backend/internal/registry", "TestAFailedCheckKeepsTheFoundVersionsAndRecordsWhy"),
    "check-wait": ("./backend/internal/registry", "TestACheckThatGetsNoAnswerFailsWithinItsWait"),
    "check-shared": ("./backend/internal/registry", "TestChecksOfOneTemplateAtOnceShareOneRun"),
    "check-api": ("./backend/internal/api", "TestACheckOnRequestAnswersTheTemplateWhateverTheOutcome"),
    "store-failure": ("./backend/internal/store", "TestAFailedCheckKeepsTheFoundVersions"),
    "moved-registry": ("./backend/internal/registry", "TestAMovedTagIsTakenAgainOnlyByUpgradingToIt"),
    "moved-older": ("./backend/internal/registry", "TestAMovedTagTakesNoOlderVersion"),
    "moved-store": ("./backend/internal/store", "TestAMovedCurrentVersionIsTakenAgainOnlyWhileMoved"),
    "moved-imported": ("./backend/internal/store", "TestAnImportedTemplateTakesItsCommitFromItsFirstCheck"),
    "moved-jobs": ("./backend/internal/workflows", "TestAMovedTagRefusesCreatesAndUpgradesUntilTakenAgain"),
    "moved-screen": ("web/src/screens/Templates.test.tsx",
                     "takes a moved current version again through Update version"),
    "descent": ("./backend/internal/workflows", "TestAnUnknownAppliedVersionIsUpgradedOnlyByDescent"),
    "descent-service": ("template-service/templates", "a_check_answers_whether_the_version_descends_from_a_commit"),
    "job-forward": ("./backend/internal/workflows", "TestAnUpgradeJobRefusesAnAppliedVersionNotOlderThanTheCurrent"),
    "stored-version": ("./backend/internal/workflows", "TestAnUpgradeComparesTheStoredAppliedVersion"),
    "upgrade-records": ("./backend/internal/workflows", "TestAnAppliedUpgradeRecordsThePinnedVersion"),
    "apply-unavailable": ("./backend/internal/workflows", "TestApplyBranchWithNoAnswerOnDescentAppliesNothing"),
    "apply-forward": ("./backend/internal/workflows", "TestApplyBranchMovesTheAppliedRecordOnlyForward"),
    "import-once": ("./backend/internal/registry", "TestTheFirstStartImportsEachWorkspacesAppliedVersionOnce"),
    "import-completes": ("./backend/internal/registry", "TestTheAppliedImportCompletesAnUnknownVersionFromFoundVersions"),
    "unknown-completes": ("./backend/internal/registry", "TestACheckGivesAnUnknownAppliedVersionTheVersionAtItsCommit"),
}

VERSIONS = "backend/internal/registry/versions.go"
REGISTRY = "backend/internal/registry/registry.go"
APPLIED = "backend/internal/registry/applied.go"
IMPORT = "backend/internal/registry/appliedimport.go"
TEMPLATES = "backend/internal/store/templates.go"
API = "backend/internal/api/templates.go"
FLOWS = "backend/internal/workflows/workflows.go"
UPGRADE = "backend/internal/workflows/upgrade.go"
APPLY = "backend/internal/workflows/applybranch.go"
PLAN = "backend/internal/backup/apply.go"
CHECK = "template-service/src/templates.rs"
CHOICES = "web/src/api/versions.ts"

# Each mutant: a name, the requirement it breaks, the source file, the text
# it replaces (exactly once), its replacement, and the tests that must each
# fail.
MUTANTS = [
    # checks
    ("schedule-checks-at-start", "checks", VERSIONS,
     "\tfor {\n\t\tr.checkAll(ctx, logf)\n\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\treturn\n\t\tcase <-ticks:\n\t\t}\n\t}\n",
     "\tfor {\n\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\treturn\n\t\tcase <-ticks:\n\t\t}\n\t\tr.checkAll(ctx, logf)\n\t}\n",
     ["schedule"]),
    ("schedule-checks-each-tick", "checks", VERSIONS,
     "\t\tcase <-ticks:\n\t\t}\n", "\t\tcase <-ticks:\n\t\t\treturn\n\t\t}\n", ["schedule"]),
    ("check-replaces-found-versions", "checks", TEMPLATES,
     "\t\tif err := t.replaceVersions(id, versions, at); err != nil {\n",
     "\t\tif err := t.replaceVersions(id, current.Versions, at); err != nil {\n", ["check-finds"]),
    ("failed-check-is-recorded", "checks", VERSIONS,
     "\t\treturn r.Data.RecordCheckFailure(record, id, CheckFailure(err), r.now())\n",
     "\t\treturn store.Template{}, err\n", ["check-fails", "check-api"]),
    ("failure-keeps-found-versions", "checks", TEMPLATES,
     "\t\tif _, err := t.exec(`UPDATE templates SET checked_at = ?, check_failure = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), failure, id); err != nil {\n",
     "\t\tif _, err := t.exec(`DELETE FROM template_versions WHERE template = ?`, id); err != nil {\n\t\t\treturn err\n\t\t}\n"
     "\t\tif _, err := t.exec(`UPDATE templates SET checked_at = ?, check_failure = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), failure, id); err != nil {\n",
     ["check-fails", "store-failure", "check-api"]),
    ("fault-is-unavailable", "checks", VERSIONS,
     "\tif errors.As(err, &refused) && refused.Status < http.StatusInternalServerError {\n",
     "\tif errors.As(err, &refused) && refused.Status < http.StatusContinue+10000 {\n", ["check-fails"]),
    ("check-wait-bounds", "checks", VERSIONS,
     "\t\tbounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)\n",
     "\t\tbounded, cancel := context.WithCancel(context.WithoutCancel(ctx))\n\t\t_ = wait\n", ["check-wait"]),
    ("checks-share-one-run", "checks", VERSIONS,
     "\trun, ok := r.running[id]\n", "\trun, ok := r.running[id]\n\tok = false\n", ["check-shared"]),
    ("leaving-caller-keeps-the-run", "checks", VERSIONS,
     "\t\tbounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)\n",
     "\t\tbounded, cancel := context.WithTimeout(ctx, wait)\n", ["check-shared"]),
    ("check-on-request-answers-200", "checks", API,
     "\ttpl, err := s.opts.Registry.CheckVersions(r.Context(), r.PathValue(\"id\"))\n\tif err != nil {\n\t\twriteRegistryError(w, err)\n\t\treturn\n\t}\n",
     "\ttpl, err := s.opts.Registry.CheckVersions(r.Context(), r.PathValue(\"id\"))\n\tif err == nil && tpl.CheckFailure != \"\" {\n"
     "\t\terr = &templates.UnavailableError{Err: errors.New(tpl.CheckFailure)}\n\t}\n\tif err != nil {\n\t\twriteRegistryError(w, err)\n\t\treturn\n\t}\n",
     ["check-api"]),
    # moved
    ("moved-is-shown", "moved", TEMPLATES,
     "\treturn ok && t.CurrentCommit != \"\" && found.Commit != t.CurrentCommit\n",
     "\treturn false && ok && t.CurrentCommit != \"\" && found.Commit != t.CurrentCommit\n",
     ["moved-registry", "moved-store", "moved-jobs"]),
    ("check-keeps-recorded-commit", "moved", TEMPLATES,
     "\t\tif current.CurrentCommit == \"\" {\n", "\t\tif true {\n", ["moved-imported"]),
    ("create-refused-while-moved", "moved", FLOWS,
     "\tif tpl.CurrentMoved() {\n\t\treturn nil, moved(tpl)\n\t}\n\tif _, err := os.Lstat(folder)",
     "\tif false && tpl.CurrentMoved() {\n\t\treturn nil, moved(tpl)\n\t}\n\tif _, err := os.Lstat(folder)", ["moved-jobs"]),
    ("upgrade-refused-while-moved", "moved", FLOWS,
     "\tif tpl.CurrentMoved() {\n\t\treturn nil, moved(tpl)\n\t}\n\treturn (&upgrading{",
     "\tif false && tpl.CurrentMoved() {\n\t\treturn nil, moved(tpl)\n\t}\n\treturn (&upgrading{", ["moved-jobs"]),
    ("store-takes-current-only-while-moved", "moved", TEMPLATES,
     "\tagain := version == current.CurrentVersion && current.CurrentMoved()\n",
     "\tagain := version == current.CurrentVersion\n", ["moved-store"]),
    ("registry-takes-current-only-while-moved", "moved", REGISTRY,
     "\tagain := version == tpl.CurrentVersion && tpl.CurrentMoved()\n",
     "\tagain := version == tpl.CurrentVersion\n", ["moved-registry"]),
    ("moved-takes-no-older-version", "moved", REGISTRY,
     "\tif !ok || !again && !Newer(target.Version, tpl.CurrentVersion) {\n",
     "\tif !ok || !tpl.CurrentMoved() && !again && !Newer(target.Version, tpl.CurrentVersion) {\n",
     ["moved-older"]),
    ("screen-offers-the-moved-version", "moved", CHOICES,
     "  if (t.currentMoved) choices.push(t.currentVersion);\n", "", ["moved-screen"]),
    # descent
    ("descent-refuses-off-history", "descent", UPGRADE,
     "\tcase !*checked.Descends:\n", "\tcase false && !*checked.Descends:\n", ["descent"]),
    ("descent-at-the-recorded-commit", "descent", UPGRADE,
     "\tcase tpl.CurrentCommit != \"\" && checked.Commit != tpl.CurrentCommit:\n",
     "\tcase false && tpl.CurrentCommit != \"\" && checked.Commit != tpl.CurrentCommit:\n", ["descent"]),
    ("descent-needs-a-commit", "descent", UPGRADE,
     "\tcase applied.Commit == \"\":\n", "\tcase false && applied.Commit == \"\":\n", ["descent"]),
    ("descent-asks-the-template-service", "descent", UPGRADE,
     "\tchecked, err := u.w.opts.Templates.CheckSince(ctx, tpl.Address, tpl.CurrentTag, applied.Commit)\n",
     "\tchecked, err := &templates.Checked{Commit: tpl.CurrentCommit, Descends: new(bool)}, error(nil)\n\t*checked.Descends = true\n",
     ["descent"]),
    ("upgrade-compares-stored-version", "descent", FLOWS,
     "\tif applied.Version != \"\" && !registry.Newer(tpl.CurrentVersion, applied.Version) {\n",
     "\tif false && applied.Version != \"\" && !registry.Newer(tpl.CurrentVersion, applied.Version) {\n",
     ["stored-version"]),
    ("job-refuses-a-version-not-older", "descent", UPGRADE,
     "\t\tif !registry.Newer(tpl.CurrentVersion, applied.Version) {\n",
     "\t\tif false && !registry.Newer(tpl.CurrentVersion, applied.Version) {\n", ["job-forward"]),
    ("upgrade-records-applied-version", "descent", UPGRADE,
     "\t\tu.w.recordApplied(ctx, run, u.name, u.folder, tpl)\n", "", ["upgrade-records", "descent"]),
    ("service-descends-from-ancestors", "descent", CHECK,
     "        if ancestor.map_err(|e| failed(&e))?.id == since {\n",
     "        if ancestor.map_err(|e| failed(&e))?.id == since && false {\n", ["descent-service"]),
    ("service-off-history-does-not-descend", "descent", CHECK,
     "    for ancestor in repo.rev_walk([tip]).all().map_err(|e| failed(&e))? {\n",
     "    for ancestor in repo.rev_walk([tip]).all().map_err(|e| failed(&e))? {\n        let _ = &ancestor;\n        return Ok(true);\n",
     ["descent-service"]),
    ("service-commit-is-its-own-descendant", "descent", CHECK,
     "    // The walk yields the tip itself first, then each of its ancestors.\n    for ancestor in repo.rev_walk([tip]).all().map_err(|e| failed(&e))? {\n",
     "    // The walk yields the tip itself first, then each of its ancestors.\n    for ancestor in repo.rev_walk([tip]).all().map_err(|e| failed(&e))?.skip(1) {\n",
     ["descent-service"]),
    ("service-refuses-a-malformed-since", "descent", CHECK,
     "        && !crate::revision::is_commit(since)\n", "        && since.is_empty()\n", ["descent-service"]),
    # apply
    ("apply-reads-the-record-change", "apply", PLAN,
     "\tout := Change{Changed: before != after}\n", "\tout := Change{Changed: false && before != after}\n",
     ["apply-forward"]),
    ("apply-needs-a-template", "apply", APPLY,
     "\tif id == \"\" {\n\t\treturn nil, refuse(", "\tif false {\n\t\treturn nil, refuse(", ["apply-forward"]),
    ("apply-refuses-a-removed-record", "apply", APPLY,
     "\tif !change.After.Present {\n", "\tif false && !change.After.Present {\n", ["apply-forward"]),
    ("apply-refuses-a-linked-record", "apply", APPLY,
     "\tif !change.After.Regular {\n", "\tif false && !change.After.Regular {\n", ["apply-forward"]),
    ("apply-refuses-an-unreadable-record", "apply", APPLY,
     "\trecord, err := registry.ParseApplied(change.After.Data)\n\tif err != nil {\n",
     "\trecord, err := registry.ParseApplied(change.After.Data)\n\tif err != nil && false {\n", ["apply-forward"]),
    ("apply-refuses-another-source", "apply", APPLY,
     "\tif record.Source != tpl.Source {\n", "\tif false && record.Source != tpl.Source {\n", ["apply-forward"]),
    ("apply-refuses-an-untagged-record", "apply", APPLY,
     "\tif version == \"\" {\n\t\treturn nil, refuse(", "\tif false {\n\t\treturn nil, refuse(", ["apply-forward"]),
    ("apply-refuses-an-older-version", "apply", APPLY,
     "\t\tif registry.Newer(applied.Version, version) {\n",
     "\t\tif false && registry.Newer(applied.Version, version) {\n", ["apply-forward"]),
    ("apply-needs-commits-to-descend", "apply", APPLY,
     "\tif applied.Commit == \"\" || record.Commit == \"\" {\n",
     "\tif false && (applied.Commit == \"\" || record.Commit == \"\") {\n", ["apply-forward"]),
    ("apply-refuses-off-history", "apply", APPLY,
     "\tcase !*checked.Descends:\n", "\tcase false && !*checked.Descends:\n", ["apply-forward"]),
    ("apply-unavailable-applies-nothing", "apply", APPLY,
     "\tcase errors.As(err, &unavailable):\n\t\treturn nil, &jobs.Failure{Kind: KindTemplateUnavailable,",
     "\tcase false && errors.As(err, &unavailable):\n\t\treturn nil, &jobs.Failure{Kind: KindTemplateUnavailable,",
     ["apply-unavailable"]),
    ("apply-at-the-records-commit", "apply", APPLY,
     "\tcase checked.Commit != record.Commit:\n", "\tcase false && checked.Commit != record.Commit:\n",
     ["apply-forward"]),
    ("apply-records-the-records-version", "apply", APPLY,
     "\tif a.record != nil {\n\t\tif err := a.w.opts.Data.SetAppliedVersion(", "\tif false {\n\t\tif err := a.w.opts.Data.SetAppliedVersion(",
     ["apply-forward"]),
    ("apply-checks-every-plan", "apply", APPLY,
     "\tif a.record, err = a.forward(ctx, plan); err != nil {\n",
     "\tif a.record, err = nil, error(nil); err != nil {\n", ["apply-forward"]),
    # import
    ("import-takes-a-version-ref", "import", APPLIED,
     "\tif v, ok := ParseVersion(a.Ref); ok {\n", "\tif v, ok := ParseVersion(a.Ref); ok && false {\n",
     ["import-once"]),
    ("import-keeps-a-known-version", "import", IMPORT,
     "\t\t\tif current != (store.AppliedVersion{}) {\n\t\t\t\tcontinue\n\t\t\t}\n",
     "\t\t\tif false && current != (store.AppliedVersion{}) {\n\t\t\t\tcontinue\n\t\t\t}\n", ["import-once"]),
    ("import-completes-from-found-versions", "import", IMPORT,
     "\t\treturn tx.CompleteApplied()\n", "\t\treturn nil\n", ["import-completes"]),
    ("import-runs-once", "import", IMPORT,
     "\tran, err := data.Import(ctx, AppliedImportID, func(tx *store.Tx) error {\n",
     "\tran, err := true, data.Update(ctx, func(tx *store.Tx) error {\n", ["import-once"]),
    ("check-completes-unknown-versions", "import", TEMPLATES,
     "\t\tif err := t.completeApplied(id); err != nil {\n\t\t\treturn err\n\t\t}\n\t\ttpl, err = t.Template(id)\n",
     "\t\ttpl, err = t.Template(id)\n", ["unknown-completes"]),
]


def run(package, test):
    cwd = ROOT
    env = dict(os.environ, GOFLAGS=os.environ.get("GOFLAGS", "-p=2"), NO_COLOR="1", FORCE_COLOR="0")
    if package.startswith("web/"):
        cwd = ROOT / "web"
        command = ["nice", "-n", "15", "pnpm", "exec", "vitest", "run",
                   package.removeprefix("web/"), "-t", re.escape(test)]
    elif package.startswith("template-service/"):
        cwd = ROOT / "template-service"
        command = ["nice", "-n", "15", "cargo", "test", "--locked", "--test",
                   package.removeprefix("template-service/"), test, "--", "--exact"]
    else:
        # A guard broken into a hang fails the test at this bound.
        command = ["nice", "-n", "15", "go", "test", "-p", "2", "-count=1", "-timeout", "120s", package,
                   "-run", "^" + test + "$", "-v"]
    try:
        result = subprocess.run(command, cwd=cwd, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=1200, check=False)
        return {"exitCode": result.returncode, "output": result.stdout[-20000:]}
    except subprocess.TimeoutExpired:
        return {"exitCode": None, "output": "test process timed out"}


def outcome(package, test, output):
    """The marker of the one test's own pass and fail, not a build's."""
    output = re.sub(r"\x1b\[[0-9;]*m", "", output)
    if package.startswith("web/"):
        return (re.search(r"Tests +1 passed", output) is not None,
                re.search(r"Tests +1 failed", output) is not None)
    if package.startswith("template-service/"):
        return (re.search(rf"test {re.escape(test)} \.\.\. ok", output) is not None,
                re.search(rf"test {re.escape(test)} \.\.\. FAILED", output) is not None)
    hung = re.search(rf"panic: test timed out after [^\n]+\n\s+running tests:\n(?:\s+[^\n]+\n)*?\s+{re.escape(test)}\b", output)
    return f"--- PASS: {test}" in output, f"--- FAIL: {test}" in output or hung is not None


def assertion(package, output):
    """The first failing assertion the output names: its place and message."""
    output = re.sub(r"\x1b\[[0-9;]*m", "", output)
    if package.startswith("web/"):
        found = re.search(r"((?:AssertionError|TestingLibraryElementError|Error): [^\n]*)", output)
        place = re.search(r"❯ (src/[^\s]+:\d+:\d+)", output)
        return " at ".join(x.group(1) for x in (found, place) if x) or None
    if package.startswith("template-service/"):
        found = re.search(r"panicked at ([^\n]+):\n([^\n]+(?:\n  left: [^\n]+\n right: [^\n]+)?)", output)
        return f"{found.group(1)}: {found.group(2)}" if found else None
    found = re.search(r"^\s+(\w+_test\.go:\d+: [^\n]*)", output, re.MULTILINE)
    if found:
        return found.group(1)
    hung = re.search(r"panic: (test timed out after [^\n]+)", output)
    return hung.group(1) + ": the test never ended" if hung else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--only", help="run only the mutants whose name holds this text")
    parser.add_argument("--start", help="skip the mutants before the one of this name")
    args = parser.parse_args()
    report = {"outcome": "failed", "baseline": [], "mutants": []}

    def save():
        args.report.write_text(json.dumps(report, indent=2) + "\n")

    save()
    mutants = [m for m in MUTANTS if not args.only or args.only in m[0]]
    if args.start:
        names = [m[0] for m in mutants]
        mutants = mutants[names.index(args.start):]
    needed = {test for m in mutants for test in m[5]}
    for name, (package, test) in TESTS.items():
        if name not in needed:
            continue
        result = run(package, test)
        report["baseline"].append({"test": name, **result})
        save()
        if result["exitCode"] != 0 or not outcome(package, test, result["output"])[0]:
            raise SystemExit(f"Baseline {name} failed: {result['output']}")
    for name, requirement, path, before, after, tests in mutants:
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
                          and outcome(package, function, result["output"])[1])
                report["mutants"].append({"name": name, "requirement": requirement, "test": test,
                                          "killed": killed,
                                          "assertion": assertion(package, result["output"]) if killed else None,
                                          **result})
                save()
                if not killed:
                    raise SystemExit(f"Mutant {name}/{test} survived or failed to compile: "
                                     + result["output"])
                print(f"killed {name} by {test}: {report['mutants'][-1]['assertion']}", flush=True)
        finally:
            source.write_text(original)
    report["outcome"] = "passed" if not (args.only or args.start) else "partial"
    save()
    print(f"TEMPLATE_VERSION_GUARDS_PASSED: {len(report['mutants'])} mutant runs rejected over {len(mutants)} mutants")


if __name__ == "__main__":
    main()
