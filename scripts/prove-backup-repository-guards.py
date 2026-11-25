#!/usr/bin/env python3
"""Prove the backup repository guards by breaking them one at a time.

Run from any directory:
  python3 scripts/prove-backup-repository-guards.py --report report.json

The guards: a write check deletes the ref it pushed; a registration refuses
a local path, a file URL and a URL with a password, runs the write check
before it records anything, and with check stores nothing; the data's
foreign key and the store's own check keep a backup repository that a
workspace backs up to; adopting the imported repository gives it only to
workspaces that have none; a create records its repository from its start
and takes the only registered one only when exactly one is; a backup goes
to the workspace's own repository and fails when it has none; the create
screen chooses the only registered repository, never one of several. A
removed repository's id is never given to another; a reply's URL holds no
password, query or fragment, nor an https URL's user name; a create
refuses a repository removed since it was read; the first start gives the
imported repository to every workspace folder, one with no data included,
and removes nothing, so an absent one keeps its data. A
move runs the destination's write check, refuses a branch that holds
another history, pushes without force so a branch another writer made
since the check is never overwritten, requires the pushed branch to read
back at its commit, and records the destination only after that, and only
while its id is still registered at the URL it checked, as the job's final
step: a time limit that passed refuses it, one that passes while it waits
cancels it, and none overturns it once it succeeds. Each mutation changes one guard in place, and its tests must fail; the source is
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

# Each test: a name, then the package and the one test it runs; a package
# under web/ is a Vitest file, run from web/.
TESTS = {
    "check-leaves-nothing": ("./backend/internal/backup", "TestCheckWriteLeavesTheRepositoryAsItWas"),
    "register-checks": ("./backend/internal/backuprepos", "TestRegisterChecksTheRepositoryBeforeItRecordsIt"),
    "register-refusals": ("./backend/internal/backuprepos", "TestRegisterRefusals"),
    "push-own-repository": ("./backend/internal/backuprepos", "TestPushGoesToTheWorkspacesOwnRepository"),
    "kept-registered": ("./backend/internal/store", "TestAWorkspaceKeepsItsBackupRepositoryRegistered"),
    "adopt-only-none": ("./backend/internal/store", "TestSettingAndAdoptingBackupRepositories"),
    "create-records": ("./backend/internal/workflows", "TestACreateRecordsItsBackupRepository"),
    "id-never-reused": ("./backend/internal/store", "TestARemovedBackupRepositorysIDIsNeverReused"),
    "public-url": ("./backend/internal/backuprepos", "TestPublicURLHoldsNoCredential"),
    "replies-no-credential": ("./backend/internal/api", "TestRepliesNameNoBackupCredential"),
    "first-start": ("./backend/cmd/menagerie", "TestAFirstStartGivesEveryWorkspaceTheConfiguredRepository"),
    "first-start-absent": ("./backend/cmd/menagerie", "TestAFirstStartKeepsAnAbsentWorkspace"),
    "create-refuses-removed": ("./backend/internal/store", "TestACreateRefusesARemovedBackupRepository"),
    "move-switches-last": ("./backend/internal/workflows", "TestAMoveWritesAndVerifiesABackupBeforeItSwitchesRepositories"),
    "move-conflict": ("./backend/internal/workflows", "TestAMoveRefusesABranchThatHoldsAnotherHistory"),
    "move-unwritable": ("./backend/internal/workflows", "TestAFailedMoveKeepsTheWorkspacesRepository"),
    "move-not-landed": ("./backend/internal/workflows", "TestAMoveWhoseBackupDoesNotLandKeepsTheWorkspacesRepository"),
    "extend-forward-only": ("./backend/internal/backup", "TestExtendNeverOverwritesAnotherHistory"),
    "switch-checked-url": ("./backend/internal/store", "TestASwitchNeedsTheCheckedURL"),
    "final-step": ("./backend/internal/jobs", "TestAFinalStepDecidesTheOutcome"),
    "final-waits": ("./backend/internal/jobs", "TestAFinalStepThatWaitsIsCancelledAtTheLimit"),
    "screen-several": ("web/src/screens/BackupRepositories.test.tsx",
                       "asks for a backup repository on create when several are registered"),
}

CHECK = "backend/internal/backup/check.go"
REGISTRY = "backend/internal/backuprepos/backuprepos.go"
STORE = "backend/internal/store/store.go"
BACKUPS = "backend/internal/store/backups.go"
CREATE = "backend/internal/workflows/create.go"
FLOWS = "backend/internal/workflows/workflows.go"
PICKER = "web/src/ui/BackupPicker.tsx"
MOVE = "backend/internal/workflows/move.go"
PUSH = "backend/internal/backup/push.go"

# Each mutant: a name, the source file, the text it replaces (exactly once),
# its replacement, and the tests that must each fail.
MUTANTS = [
    ("check-deletes-its-ref", CHECK,
     "\tif err := push(config.RefSpec(\":\" + probe.String())); err != nil {\n",
     "\tif false {\n", ["check-leaves-nothing"]),
    ("refuse-local-urls", REGISTRY,
     "\tcase \"ssh\", \"https\":\n", "\tcase \"ssh\", \"https\", \"file\":\n", ["register-refusals"]),
    ("refuse-passwords", REGISTRY,
     "\tif endpoint.Password != \"\" {\n", "\tif false {\n", ["register-refusals"]),
    ("write-check-runs", REGISTRY,
     "\tif err := r.Remote(url).CheckWrite(checkCtx, io.Discard); err != nil {\n",
     "\tif err := error(nil); err != nil {\n", ["register-refusals"]),
    ("check-stores-nothing", REGISTRY,
     "\tif check {\n\t\treturn tried, nil\n\t}\n", "\t_ = tried\n", ["register-checks"]),
    ("push-needs-a-repository", REGISTRY,
     "\tif !ok {\n\t\treturn nil, &NoRepositoryError{Workspace: workspace}\n\t}\n",
     "\tif !ok {\n\t\treturn &backup.Remote{}, nil\n\t}\n", ["push-own-repository"]),
    ("foreign-key-keeps-used", STORE,
     "\tALTER TABLE workspaces ADD COLUMN backup_repository TEXT REFERENCES backup_repositories(id);\n",
     "\tALTER TABLE workspaces ADD COLUMN backup_repository TEXT;\n", ["kept-registered"]),
    ("remove-refuses-used", BACKUPS,
     "\t\tif len(r.Workspaces) > 0 {\n\t\t\treturn &BackupRepositoryInUseError{Workspaces: r.Workspaces}\n\t\t}\n",
     "\t\t_ = r\n", ["kept-registered"]),
    ("adopt-only-none", BACKUPS,
     "UPDATE workspaces SET backup_repository = ? WHERE backup_repository IS NULL",
     "UPDATE workspaces SET backup_repository = ?", ["adopt-only-none"]),
    ("create-records-from-start", CREATE,
     "\tif err := c.w.opts.Data.BeginCreateFrom(ctx, c.name, tpl.ID, backups.ID); err != nil {\n",
     "\tif err := c.w.opts.Data.BeginCreateFrom(ctx, c.name, tpl.ID, \"\"); err != nil {\n", ["create-records"]),
    ("only-the-only-one", FLOWS,
     "\t\tif len(registered) != 1 {\n", "\t\tif len(registered) == 0 {\n", ["create-records"]),
    ("removal-keeps-the-id", BACKUPS,
     "\t\t_, err = t.exec(`UPDATE backup_repositories SET removed_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)\n",
     "\t\t_ = time.Now\n\t\t_, err = t.exec(`DELETE FROM backup_repositories WHERE id = ?`, id)\n", ["id-never-reused"]),
    ("public-url-drops-password", REGISTRY,
     "\t\tu.User = neturl.User(u.User.Username())\n", "\t\t_ = u.User\n", ["public-url"]),
    ("reply-uses-public-url", "backend/internal/api/backups.go",
     "URL: backuprepos.PublicURL(r.URL)", "URL: r.URL", ["replies-no-credential"]),
    ("public-url-drops-https-user", REGISTRY,
     "\t\tu.User = nil\n", "\t\t_ = u\n", ["public-url"]),
    ("start-lists-folders-first", "backend/cmd/menagerie/main.go",
     "\tif err := discoverWorkspaces(ctx, root, data); err != nil {\n", "\tif err := error(nil); err != nil {\n",
     ["first-start"]),
    ("first-start-removes-nothing", "backend/cmd/menagerie/main.go",
     "\tif err := data.AddFolders(ctx, unknown); err != nil {\n",
     "\tif _, err := data.Discover(ctx, names); err != nil {\n", ["first-start-absent"]),
    ("create-refuses-removed", "backend/internal/store/workspaces.go",
     "\t\tif backups != \"\" {\n\t\t\tif _, err := t.backupRepository(backups); err != nil {\n",
     "\t\tif false {\n\t\t\tif _, err := t.backupRepository(backups); err != nil {\n",
     ["create-refuses-removed"]),
    ("move-checks-write", MOVE,
     "\tif err := destination.CheckWrite(ctx, run.Log(\"verify: \")); err != nil {\n",
     "\tif err := error(nil); err != nil {\n", ["move-unwritable"]),
    ("move-refuses-other-history", MOVE,
     "\tif failure := m.conflict(ctx, destination, to, fail); failure != nil {\n",
     "\tif failure := m.conflict(ctx, destination, to, fail); false && failure != nil {\n", ["move-conflict"]),
    ("move-reads-back", MOVE,
     "\tcase landed != commit.Hash:\n", "\tcase false:\n", ["move-not-landed"]),
    ("move-records-after-push", MOVE,
     "\trun.Phase(\"commit\")\n",
     "\t_ = m.w.opts.Data.SetWorkspaceBackupRepository(ctx, m.name, to.ID)\n\trun.Phase(\"commit\")\n",
     ["move-not-landed"]),
    ("move-records", MOVE,
     "\terr = run.Final(func() error { return m.w.opts.Data.SwitchWorkspaceBackupRepository(ctx, m.name, to.ID, to.URL) })\n",
     "\terr = run.Final(func() error { return nil })\n", ["move-switches-last"]),
    ("extend-without-force", PUSH,
     "\treturn r.push(ctx, repo, branch, false, progress)\n",
     "\treturn r.push(ctx, repo, branch, true, progress)\n", ["extend-forward-only"]),
    ("switch-checks-url", BACKUPS,
     "\t\tif r.URL != url {\n", "\t\tif r.URL != url && false {\n", ["switch-checked-url"]),
    ("final-stops-limits", "backend/internal/jobs/run.go",
     "\tj.final = true\n", "", ["final-step"]),
    ("final-settles-first", "backend/internal/jobs/run.go",
     "\tj.settleLocked(now)\n\ttimedOut := j.timedOut\n", "\t_ = now\n\ttimedOut := j.timedOut\n", ["final-step"]),
    ("final-counts-during-step", "backend/internal/jobs/run.go",
     "\tj.mu.Unlock()\n\tif timedOut != nil {\n",
     "\tj.final = true\n\tj.stopTimerLocked()\n\tj.mu.Unlock()\n\tif timedOut != nil {\n", ["final-waits"]),
    ("final-outcome-is-the-steps", "backend/internal/jobs/limits.go",
     "\tcase j.timedOut != nil && !j.final:\n", "\tcase j.timedOut != nil:\n", ["final-step"]),
    ("screen-chooses-only-one", PICKER,
     "        if (list.length === 1 && list[0]) onChange(list[0].id);\n",
     "        if (list[0]) onChange(list[0].id);\n", ["screen-several"]),
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
    print(f"BACKUP_REPOSITORY_GUARDS_PASSED: {len(report['mutants'])} mutant runs rejected")


if __name__ == "__main__":
    main()
