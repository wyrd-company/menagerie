#!/usr/bin/env python3
"""Prove the backup repository credential guards by breaking them one at a time.

Run from any directory:
  python3 scripts/prove-backup-credential-guards.py --report report.json

The guards: a transfer with a key offers the Git host that key alone, and
one with an App sends only that App's token, refusing a URL it does not
serve; a registration or a change refuses a credential that does not fit
its URL (an SSH URL with no key, an App that does not serve the URL, the
operator key), records a key without its comment, and checks writing with
the credential given, not every key; a checked change stores nothing; a
change lands only at the URL it checked; the key list leaves out the
operator key; the backups screen offers an https URL only the Apps that
serve it. Each mutation changes one guard in place, and its tests must
fail; the source is restored even on interruption. The report starts
failed and passes only after every baseline test passes and every mutant
compiles and fails each of its tests.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parent.parent

# Each test: a name, then the package and the one test it runs; a package
# under web/ is a Vitest file, run from web/.
TESTS = {
    "key-alone": ("./backend/internal/backup", "TestAgentKeyAuthOffersOnlyTheRepositorysKey"),
    "fit": ("./backend/internal/gitcreds", "TestFitRefusesACredentialThatDoesNotFitTheURL"),
    "app-alone": ("./backend/internal/gitcreds", "TestAuthUsesOnlyTheCredentialsApp"),
    "keys-listed": ("./backend/internal/gitcreds", "TestKeysListsTheAgentsKeysWithoutTheOperatorKey"),
    "register-own-credential": ("./backend/internal/backuprepos", "TestRegisterChecksWritingWithItsOwnCredential"),
    "change-checks": ("./backend/internal/backuprepos", "TestSetCredentialChecksBeforeItRecords"),
    "change-checked-url": ("./backend/internal/store", "TestABackupRepositoryKeepsItsCredential"),
    "screen-apps": ("web/src/screens/BackupRepositories.test.tsx",
                    "offers an https URL only the Apps that serve it, and none off their servers"),
}

PUSH = "backend/internal/backup/push.go"
CREDS = "backend/internal/gitcreds/gitcreds.go"
REGISTRY = "backend/internal/backuprepos/backuprepos.go"
BACKUPS = "backend/internal/store/backups.go"
PICKER = "web/src/ui/CredentialPicker.tsx"

# Each mutant: a name, the source file, the text it replaces (exactly once),
# its replacement, and the tests that must each fail.
MUTANTS = [
    ("key-offers-only-its-key", PUSH,
     "\t\t\tsigners = func() ([]ssh.Signer, error) { return []ssh.Signer{signer}, nil }\n",
     "\t\t\t_ = signer\n", ["key-alone"]),
    ("ssh-needs-a-key", CREDS,
     "\t\tcase c.Key == \"\":\n", "\t\tcase false:\n", ["fit"]),
    ("app-must-serve-the-url", CREDS,
     "\tif _, ok := app.Repository(url); !ok {\n", "\tif false {\n", ["fit"]),
    ("operator-key-refused", CREDS,
     "\t\tif s.excluded(ctx, parsed) {\n", "\t\tif s.excluded(ctx, parsed) && false {\n", ["fit"]),
    ("app-token-only-where-served", CREDS,
     "\t\tif !tokens.Serves(url) {\n", "\t\tif false {\n", ["app-alone"]),
    ("keys-leave-out-the-operator-key", CREDS,
     "\t\tif s.Excluded != \"\" && selects(s.Excluded, k) {\n", "\t\tif false {\n", ["keys-listed"]),
    ("key-recorded-without-comment", REGISTRY,
     "\t\tc.Key = key\n", "\t\t_ = key\n", ["register-own-credential"]),
    ("write-check-with-the-credential", REGISTRY,
     "\treturn &backup.Remote{URL: repo.URL, Auth: r.Credentials.Auth(repo.Credential)}\n",
     "\treturn &backup.Remote{URL: repo.URL, Auth: r.Credentials.Auth(store.Credential{})}\n",
     ["change-checks"]),
    ("change-checks-credential", REGISTRY,
     "\tif err := r.Credentials.Fit(ctx, repo.URL, repo.Credential); err != nil {\n",
     "\tif err := error(nil); err != nil {\n", ["change-checks", "register-own-credential"]),
    ("checked-change-stores-nothing", REGISTRY,
     "\tif check {\n\t\treturn repo, nil\n\t}\n", "", ["change-checks"]),
    ("change-at-the-checked-url", BACKUPS,
     "\t\tif r.URL != url {\n\t\t\treturn fmt.Errorf(\"%w: %s\", ErrBackupRepositoryChanged, id)\n\t\t}\n\t\tif _, err := t.exec(`UPDATE backup_repositories SET credential_app",
     "\t\tif r.URL != url && false {\n\t\t\treturn fmt.Errorf(\"%w: %s\", ErrBackupRepositoryChanged, id)\n\t\t}\n\t\tif _, err := t.exec(`UPDATE backup_repositories SET credential_app",
     ["change-checked-url"]),
    ("screen-only-serving-apps", PICKER,
     "    apps: kind === \"https\" ? servingApps(url, credentials?.githubApps ?? []) : [],\n",
     "    apps: kind === \"https\" ? (credentials?.githubApps ?? []) : [],\n", ["screen-apps"]),
]


def run(package, test):
    cwd = ROOT
    if package.startswith("web/"):
        cwd = ROOT / "web"
        command = ["nice", "-n", "15", "pnpm", "exec", "vitest", "run",
                   package.removeprefix("web/"), "-t", re.escape(test)]
    else:
        command = ["nice", "-n", "15", "go", "test", "-p", "2", "-count=1", package,
                   "-run", "^" + test + "$", "-v"]
    env = dict(os.environ, GOFLAGS=os.environ.get("GOFLAGS", "-p=2"), NO_COLOR="1", FORCE_COLOR="0")
    try:
        result = subprocess.run(command, cwd=cwd, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=900, check=False)
        return {"exitCode": result.returncode, "output": result.stdout[-20000:]}
    except subprocess.TimeoutExpired:
        return {"exitCode": None, "output": "test process timed out"}


def outcome(package, test, output):
    """The marker of the one test's own pass and fail, not a build's."""
    output = re.sub(r"\x1b\[[0-9;]*m", "", output)
    if package.startswith("web/"):
        return (re.search(r"Tests +1 passed", output) is not None,
                re.search(r"Tests +1 failed", output) is not None)
    return f"--- PASS: {test}" in output, f"--- FAIL: {test}" in output


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
        if result["exitCode"] != 0 or not outcome(package, test, result["output"])[0]:
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
                          and outcome(package, function, result["output"])[1])
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
    print(f"BACKUP_CREDENTIAL_GUARDS_PASSED: {len(report['mutants'])} mutant runs rejected")


if __name__ == "__main__":
    main()
