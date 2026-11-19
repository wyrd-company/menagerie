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
  fault, an unreachable service, a malformed listing or no answer within
  the check's wait; two checks of one template share one run that a
  leaving caller does not end; a check on request answers 200 with the
  failure recorded; a template takes a version only at the commit its
  listing found; a listing out of ascending order, listing a version twice
  or a number above 2^64-1, and a check reply whose commit is not a full
  object id, are no answer.
- moved: a check that finds the current version's tag at another commit
  shows it moved, never replacing the recorded commit; a create and an
  upgrade are refused template-version-moved while it is; the template
  takes that version again, with the new commit, only while it is moved,
  and the screen offers it through Update version; versions compare by
  their whole numbers, and the screen sends only a version it offers, the
  newest when the picked one is no longer offered.
- descent: an upgrade of a workspace whose applied version is unknown goes
  on only when the template service answers that the current version's
  commit is, or descends from, its applied commit, at the recorded commit,
  and fails as template-refused otherwise, also when no commit is known or
  the reply has no descent; with no recorded commit it pins the commit the
  answer approved, and records the applied record's commit only when it is
  a full object id.
- apply: Apply branch refuses, as not-applicable with nothing changed, a
  record of a workspace with no registered template, a removed, linked,
  oversized or unreadable record, a record the one rule gives no version,
  the moved current version, an older version, and an unknown applied
  version whose record's commit does not descend or is not known; and
  records a forward record's version.
- record: one rule gives every applied version taken from a record: of the
  template's source, at a full object id, the found version whose tag is
  at that commit, the ref's when the ref is a version, else the newest
  there; a join or the import keeps the commit alone otherwise, only of the
  template's source and a full object id; a refusal quotes the record's
  ref and commit short.
- import: the first start records each workspace's applied version from
  its record once, keeping a known one; a start imports the applied
  versions and checks every template with no request.

Each mutation changes one guard in place, and every test it names must
fail by its own assertion: for a Go test, the last _test.go:N: line of the
innermost failing test or subtest, read from go test -json. A panic, a
test that never ends, or a run that times out names none and leaves the
mutant a survivor. The report names its head commit and, for each mutant,
the assertion that failed and the review sweep's id of a mutant it also
ran. --self-test checks how the assertion is read and runs nothing.
The source is restored even on interruption. The report starts failed and
passes only after every baseline test passes and every mutant compiles and
fails each of its tests.
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
    "check-listed": ("./backend/internal/registry", "TestATemplateTakesAVersionOnlyAtTheCommitItsListingFound"),
    "check-malformed": ("./backend/internal/registry", "TestAMalformedListingIsRecordedAsUnavailable"),
    "client-replies": ("./backend/internal/templates", "TestTemplateRoutesNameTheAddressAndDecodeStrictly"),
    "store-failure": ("./backend/internal/store", "TestAFailedCheckKeepsTheFoundVersions"),
    "moved-registry": ("./backend/internal/registry", "TestAMovedTagIsTakenAgainOnlyByUpgradingToIt"),
    "moved-older": ("./backend/internal/registry", "TestAMovedTagTakesNoOlderVersion"),
    "moved-store": ("./backend/internal/store", "TestAMovedCurrentVersionIsTakenAgainOnlyWhileMoved"),
    "moved-imported": ("./backend/internal/store", "TestAnImportedTemplateTakesItsCommitFromItsFirstCheck"),
    "moved-jobs": ("./backend/internal/workflows", "TestAMovedTagRefusesCreatesAndUpgradesUntilTakenAgain"),
    "moved-screen": ("web/src/screens/Templates.test.tsx",
                     "takes a moved current version again through Update version"),
    "screen-offered": ("web/src/screens/Templates.test.tsx",
                       "offers the newest version in place of a picked one a check no longer offers"),
    "compare-whole": ("web/src/api/versions.test.ts",
                      "tells apart numbers above 2^53 that a Number rounds together"),
    "descent": ("./backend/internal/workflows", "TestAnUnknownAppliedVersionIsUpgradedOnlyByDescent"),
    "descent-service": ("template-service/templates", "a_check_answers_whether_the_version_descends_from_a_commit"),
    "descent-pin": ("./backend/internal/workflows", "TestAnUpgradePinsTheCommitItsDescentCheckApproved"),
    "record-commit": ("./backend/internal/workflows", "TestAnUpgradeOfAnUncheckedTemplateRecordsTheRecordsCommit"),
    "job-forward": ("./backend/internal/workflows", "TestAnUpgradeJobRefusesAnAppliedVersionNotOlderThanTheCurrent"),
    "stored-version": ("./backend/internal/workflows", "TestAnUpgradeComparesTheStoredAppliedVersion"),
    "upgrade-records": ("./backend/internal/workflows", "TestAnAppliedUpgradeRecordsThePinnedVersion"),
    "apply-unavailable": ("./backend/internal/workflows", "TestApplyBranchWithNoAnswerOnDescentAppliesNothing"),
    "apply-forward": ("./backend/internal/workflows", "TestApplyBranchMovesTheAppliedRecordOnlyForward"),
    "one-rule": ("./backend/internal/registry", "TestOneRuleGivesTheVersionOfARecord"),
    "quote-short": ("./backend/internal/registry", "TestARecordsValuesAreQuotedShort"),
    "registration-join": ("./backend/internal/registry", "TestARegistrationAndACheckGiveTheVersionAtTheRecordsCommit"),
    "discovery-join": ("./backend/internal/api", "TestAWorkspaceListedForTheFirstTimeJoinsItsTemplateWhichItsUseKeeps"),
    "import-once": ("./backend/internal/registry", "TestTheFirstStartImportsEachWorkspacesAppliedVersionOnce"),
    "import-found": ("./backend/internal/registry", "TestTheAppliedImportTakesTheFoundVersionAtTheRecordsCommit"),
    "unknown-completes": ("./backend/internal/store", "TestAnUnknownAppliedVersionIsCompletedFromItsCommit"),
    "start-imports": ("./backend/cmd/menagerie", "TestAFirstStartImportsTheAppliedVersions"),
    "start-checks": ("./backend/cmd/menagerie", "TestAStartChecksTheTemplatesWithNoRequest"),
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
CLIENT = "backend/internal/templates/client.go"
MAIN = "backend/cmd/menagerie/main.go"
CHECK = "template-service/src/templates.rs"
CHOICES = "web/src/api/versions.ts"
SCREEN = "web/src/screens/Templates.tsx"

# The review sweep's id of a mutant this script also runs, by its name.
SWEEP = {
    "check-wait-bounds": "R05",
    "descent-at-the-recorded-commit": "W06",
    "record-version-at-the-records-commit": "P10",
    "apply-asks-by-the-found-tag": "P18",
    "upgrade-records-the-records-commit": "W12",
    "client-refuses-a-check-with-no-descent": "C01",
    "start-checks-versions": "G04",
    "screen-falls-back-to-the-newest": "V15",
    "start-imports-applied-versions": "G05",
}

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
    ("upgrade-pins-the-approved-commit", "descent", UPGRADE,
     "\tif tpl.CurrentCommit == \"\" && approved != \"\" {\n",
     "\tif false && tpl.CurrentCommit == \"\" && approved != \"\" {\n", ["descent-pin"]),
    ("descent-approves-its-commit", "descent", UPGRADE,
     "\treturn \"commit \" + applied.Commit, checked.Commit, nil\n",
     "\treturn \"commit \" + applied.Commit, \"\", nil\n", ["descent-pin"]),
    ("upgrade-records-the-records-commit", "descent", UPGRADE,
     "\tif commit == \"\" {\n", "\tif false {\n", ["record-commit"]),
    ("upgrade-records-only-a-full-commit", "descent", UPGRADE,
     "err == nil && registry.IsCommit(applied.Commit) {\n", "err == nil {\n", ["record-commit"]),
    ("client-refuses-a-check-with-no-descent", "descent", CLIENT,
     " || !fullCommit.MatchString(out.Commit) || out.Descends == nil {\n",
     " || !fullCommit.MatchString(out.Commit) {\n", ["client-replies"]),
    ("client-descent-commit-is-full", "descent", CLIENT,
     " || !fullCommit.MatchString(out.Commit) || out.Descends == nil {\n",
     " || out.Commit == \"\" || out.Descends == nil {\n", ["client-replies"]),
    ("client-check-commit-is-full", "checks", CLIENT,
     "\tif out.Source == \"\" || !fullCommit.MatchString(out.Commit) {\n",
     "\tif out.Source == \"\" || out.Commit == \"\" {\n", ["client-replies"]),
    # checks: the listing
    ("client-checks-every-listed-version", "checks", CLIENT,
     "\t\tif !listedVersion.MatchString(v.Version) || v.Tag == \"\" || !fullCommit.MatchString(v.Commit) {\n",
     "\t\tif _ = v; false {\n", ["client-replies", "check-malformed"]),
    ("client-listed-tag-is-named", "checks", CLIENT,
     " || v.Tag == \"\" || ", " || ", ["client-replies"]),
    ("client-listed-commit-is-full", "checks", CLIENT,
     " || !fullCommit.MatchString(v.Commit) {\n", " {\n", ["client-replies"]),
    ("client-lists-in-ascending-order", "checks", CLIENT,
     "\t\tif i > 0 && !after(numbers, last) {\n", "\t\tif false && i > 0 && !after(numbers, last) {\n",
     ["client-replies"]),
    ("client-lists-each-version-once", "checks", CLIENT,
     "\t\t\treturn a[i] > b[i]\n\t\t}\n\t}\n\treturn false\n}\n",
     "\t\t\treturn a[i] > b[i]\n\t\t}\n\t}\n\treturn true\n}\n", ["client-replies"]),
    ("client-numbers-within-u64", "checks", CLIENT,
     "\t\tif !ok {\n", "\t\tif false && !ok {\n", ["client-replies"]),
    ("template-takes-the-listed-commit", "checks", REGISTRY,
     "\tif checked.Commit != v.Commit {\n\t\treturn nil, &CheckError{Check: CheckVersion,",
     "\tif false {\n\t\treturn nil, &CheckError{Check: CheckVersion,", ["check-listed"]),
    # apply
    ("apply-reads-the-record-change", "apply", PLAN,
     "\tout := Change{Changed: p.head[path] != after}\n", "\tout := Change{Changed: false && p.head[path] != after}\n",
     ["apply-forward"]),
    ("apply-bounds-the-record", "apply", PLAN,
     "\tif blob.Size > limit {\n", "\tif false {\n", ["apply-forward"]),
    ("apply-needs-a-template", "apply", APPLY,
     "\tif id == \"\" {\n\t\treturn nil, refuse(", "\tif false {\n\t\treturn nil, refuse(", ["apply-forward"]),
    ("apply-refuses-a-removed-record", "apply", APPLY,
     "\tcase !change.After.Present:\n", "\tcase false && !change.After.Present:\n", ["apply-forward"]),
    ("apply-refuses-a-linked-record", "apply", APPLY,
     "\tcase !change.After.Regular:\n", "\tcase false && !change.After.Regular:\n", ["apply-forward"]),
    ("apply-refuses-an-oversized-record", "apply", APPLY,
     "\tcase change.After.TooLarge:\n", "\tcase false && change.After.TooLarge:\n", ["apply-forward"]),
    ("apply-refuses-an-unreadable-record", "apply", APPLY,
     "\trecord, err := registry.ParseApplied(change.After.Data)\n\tif err != nil {\n",
     "\trecord, err := registry.ParseApplied(change.After.Data)\n\tif err != nil && false {\n", ["apply-forward"]),
    ("apply-takes-the-one-rule", "apply", APPLY,
     "\tfound, err := registry.RecordVersion(record, tpl)\n\tif err != nil {\n",
     "\tfound, err := registry.RecordVersion(record, tpl)\n\tif false && err != nil {\n", ["apply-forward"]),
    ("apply-refuses-the-moved-current-version", "apply", APPLY,
     "\tif found.Version == tpl.CurrentVersion && tpl.CurrentMoved() {\n",
     "\tif false && found.Version == tpl.CurrentVersion && tpl.CurrentMoved() {\n", ["apply-forward"]),
    ("apply-refuses-an-older-version", "apply", APPLY,
     "\t\tif registry.Newer(applied.Version, found.Version) {\n",
     "\t\tif false && registry.Newer(applied.Version, found.Version) {\n", ["apply-forward"]),
    ("apply-needs-a-commit-to-descend", "apply", APPLY,
     "\tif applied.Commit == \"\" {\n\t\treturn nil, refuse(",
     "\tif false {\n\t\treturn nil, refuse(", ["apply-forward"]),
    ("apply-asks-by-the-found-tag", "apply", APPLY,
     "CheckSince(ctx, tpl.Address, found.Tag, applied.Commit)",
     "CheckSince(ctx, tpl.Address, record.Ref, applied.Commit)", ["apply-forward"]),
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
    # record: the one rule
    ("record-of-the-templates-source", "record", APPLIED,
     "\tif rec.Source != tpl.Source {\n\t\treturn store.Version{}",
     "\tif false && rec.Source != tpl.Source {\n\t\treturn store.Version{}", ["one-rule", "apply-forward"]),
    ("record-at-a-full-commit", "record", APPLIED,
     "\tif !IsCommit(rec.Commit) {\n\t\treturn store.Version{}",
     "\tif false && !IsCommit(rec.Commit) {\n\t\treturn store.Version{}", ["one-rule"]),
    ("full-commit-has-its-length", "record", APPLIED,
     "\tif len(commit) != 40 && len(commit) != 64 {\n", "\tif len(commit) == 0 {\n", ["one-rule"]),
    ("full-commit-is-lower-case-hex", "record", APPLIED,
     "\t\tif (c < '0' || c > '9') && (c < 'a' || c > 'f') {\n",
     "\t\tif (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {\n", ["one-rule"]),
    ("record-version-at-the-records-commit", "record", APPLIED,
     "\t\tif found.Commit == rec.Commit {\n", "\t\tif true {\n", ["one-rule", "apply-forward", "import-found"]),
    ("record-ref-must-be-the-version", "record", APPLIED,
     "\tcase !isVersion:\n", "\tcase true || !isVersion:\n", ["one-rule", "apply-forward"]),
    ("record-takes-the-newest-at-the-commit", "record", APPLIED,
     "\t\treturn at[len(at)-1], nil\n", "\t\treturn at[0], nil\n", ["one-rule"]),
    ("quote-bounds-a-value", "record", APPLIED,
     "\tif len(value) <= QuoteLimit {\n", "\tif true {\n", ["quote-short"]),
    ("quote-cuts-at-a-rune", "record", APPLIED,
     "\tfor cut > 0 && !utf8.RuneStart(value[cut]) {\n", "\tfor false && cut > 0 && !utf8.RuneStart(value[cut]) {\n",
     ["quote-short"]),
    ("rule-quotes-a-commit-short", "record", APPLIED,
     "is not a full Git object id\", Quoted(rec.Commit))", "is not a full Git object id\", strconv.Quote(rec.Commit))",
     ["quote-short"]),
    ("rule-quotes-a-ref-short", "record", APPLIED,
     "(ref %s)\", tpl.ID, Quoted(rec.Commit), Quoted(rec.Ref))", "(ref %s)\", tpl.ID, Quoted(rec.Commit), strconv.Quote(rec.Ref))",
     ["quote-short"]),
    ("join-takes-the-one-rule", "record", APPLIED,
     "\tif found, err := RecordVersion(rec, tpl); err == nil {\n",
     "\tif found, err := RecordVersion(rec, tpl); err == nil && false {\n",
     ["one-rule", "import-found", "discovery-join"]),
    ("join-keeps-only-its-sources-commit", "record", APPLIED,
     "\tif rec.Source == tpl.Source && IsCommit(rec.Commit) {\n", "\tif IsCommit(rec.Commit) {\n",
     ["one-rule", "import-once"]),
    ("join-keeps-only-a-full-commit", "record", APPLIED,
     "\tif rec.Source == tpl.Source && IsCommit(rec.Commit) {\n", "\tif rec.Source == tpl.Source {\n",
     ["one-rule", "import-once"]),
    ("registration-joins-by-the-rule", "record", REGISTRY,
     "\t\t\tout[name] = version(tpl)\n", "\t\t\tout[name], _ = store.AppliedVersion{}, version\n", ["registration-join"]),
    ("discovery-joins-by-the-rule", "record", TEMPLATES,
     "\t\t\tif err := t.SetAppliedVersion(name, r.version(tpl)); err != nil {\n",
     "\t\t\t_ = tpl\n\t\t\tif err := t.SetAppliedVersion(name, AppliedVersion{}); err != nil {\n", ["discovery-join"]),
    # import
    ("import-keeps-a-known-version", "import", IMPORT,
     "\t\t\tif current != (store.AppliedVersion{}) {\n\t\t\t\tcontinue\n\t\t\t}\n",
     "\t\t\tif false && current != (store.AppliedVersion{}) {\n\t\t\t\tcontinue\n\t\t\t}\n", ["import-once"]),
    ("import-takes-the-records-version", "import", IMPORT,
     "\t\t\tif applied := version(tpl); applied != (store.AppliedVersion{}) {\n",
     "\t\t\t_, _ = version, tpl\n\t\t\tif applied := (store.AppliedVersion{}); applied != (store.AppliedVersion{}) {\n",
     ["import-once", "import-found"]),
    ("import-runs-once", "import", IMPORT,
     "\tran, err := data.Import(ctx, AppliedImportID, func(tx *store.Tx) error {\n",
     "\tran, err := true, data.Update(ctx, func(tx *store.Tx) error {\n", ["import-once"]),
    ("check-completes-unknown-versions", "import", TEMPLATES,
     "\t\tif err := t.completeApplied(id); err != nil {\n\t\t\treturn err\n\t\t}\n\t\ttpl, err = t.Template(id)\n",
     "\t\ttpl, err = t.Template(id)\n", ["unknown-completes", "registration-join", "import-once"]),
    ("start-imports-applied-versions", "import", MAIN,
     "\treturn registry.ImportApplied(ctx, data, root, log.Printf)\n", "\treturn nil\n", ["start-imports"]),
    ("start-checks-versions", "checks", MAIN,
     "\tgo versions.Schedule(ctx, ticks.C, log.Printf)\n", "\t_ = versions\n", ["start-checks"]),
    # the screen
    ("versions-compare-whole-numbers", "moved", CHOICES,
     "? BigInt(n) : 0n", "? BigInt(Number(n)) : 0n", ["compare-whole"]),
    ("screen-sends-an-offered-version", "moved", SCREEN,
     "  const version = choices.includes(picked) ? picked : (choices[0] ?? \"\");\n",
     "  const version = picked;\n", ["screen-offered"]),
    ("screen-falls-back-to-the-newest", "moved", SCREEN,
     ": (choices[0] ?? \"\");\n", ": (choices[choices.length - 1] ?? \"\");\n", ["screen-offered"]),
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
        # A guard broken into a hang ends here, a survivor: each test that
        # waits holds its own, shorter deadline with a named failure.
        command = ["nice", "-n", "15", "go", "test", "-p", "2", "-count=1", "-timeout", "120s", package,
                   "-run", "^" + test + "$", "-json"]
    try:
        result = subprocess.run(command, cwd=cwd, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=1200, check=False)
    except subprocess.TimeoutExpired:
        return {"exitCode": None, "output": "test process timed out", "raw": ""}
    # The report keeps the text; outcome and assertion read the raw output,
    # go test's events for a Go package.
    output = result.stdout
    if package.startswith("./"):
        output = "".join(e.get("Output", "") for e in go_events(result.stdout)) or result.stdout
    return {"exitCode": result.returncode, "output": output[-20000:], "raw": result.stdout}


def go_events(raw):
    """The events of go test -json; a line that is not one, a build error
    for instance, is left out."""
    events = []
    for line in raw.splitlines():
        if line.startswith("{"):
            try:
                events.append(json.loads(line))
            except ValueError:
                pass
    return events


def outcome(package, test, output):
    """The marker of the one test's own pass and fail, not a build's."""
    output = re.sub(r"\x1b\[[0-9;]*m", "", output)
    if package.startswith("web/"):
        return (re.search(r"Tests +1 passed", output) is not None,
                re.search(r"Tests +1 failed", output) is not None)
    if package.startswith("template-service/"):
        return (re.search(rf"test {re.escape(test)} \.\.\. ok", output) is not None,
                re.search(rf"test {re.escape(test)} \.\.\. FAILED", output) is not None)
    events = go_events(output)
    return (any(e.get("Action") == "pass" and e.get("Test") == test for e in events),
            any(e.get("Action") == "fail" and e.get("Test") == test for e in events))


def assertion(package, test, output):
    """The failing assertion the output names: its place and message."""
    output = re.sub(r"\x1b\[[0-9;]*m", "", output)
    if package.startswith("web/"):
        found = re.search(r"((?:AssertionError|TestingLibraryElementError|Error): [^\n]*)", output)
        place = re.search(r"❯ (src/[^\s]+:\d+:\d+)", output)
        return " at ".join(x.group(1) for x in (found, place) if x) or None
    if package.startswith("template-service/"):
        found = re.search(r"panicked at ([^\n]+):\n([^\n]+(?:\n  left: [^\n]+\n right: [^\n]+)?)", output)
        return f"{found.group(1)}: {found.group(2)}" if found else None
    return go_assertion(test, go_events(output))


def go_assertion(test, events):
    """The assertion of the innermost failing test or subtest of test: the
    last _test.go:N: line of its own output. A panic, the go test timeout's
    included, names no assertion."""
    if any(re.match(r"panic: ", e.get("Output", "")) for e in events):
        return None
    failed = [e["Test"] for e in events if e.get("Action") == "fail" and e.get("Test")
              and (e["Test"] == test or e["Test"].startswith(test + "/"))]
    for name in failed:
        if any(other.startswith(name + "/") for other in failed):
            continue
        own = [e["Output"].strip() for e in events if e.get("Test") == name and e.get("Action") == "output"
               and re.match(r"\s+\w+_test\.go:\d+: ", e["Output"])]
        if own:
            return own[-1]
    return None


def self_test():
    """Checks go_assertion against go test -json events of known shape."""
    def out(name, text):
        return {"Action": "output", "Test": name, "Output": text}

    def fail(name):
        return {"Action": "fail", "Test": name}

    cases = [
        ("a setup log, then a panic", [out("TestA", "    a_test.go:10: setup\n"),
                                       out("TestA", "panic: boom\n"), fail("TestA")], None),
        ("the timeout's panic", [out("TestA", "panic: test timed out after 2m0s\n"), fail("TestA")], None),
        ("a parent's log and a subtest's failure",
         [out("TestA", "    a_test.go:10: setup\n"), out("TestA/sub", "    a_test.go:20: want 1\n"),
          fail("TestA/sub"), fail("TestA")], "a_test.go:20: want 1"),
        ("a log, then the fatal assertion",
         [out("TestA", "    a_test.go:30: note\n"), out("TestA", "    a_test.go:31: boom\n"), fail("TestA")],
         "a_test.go:31: boom"),
        ("another test's line", [out("TestB", "    b_test.go:5: other\n"), fail("TestA")], None),
        ("a subtest failing with no line of its own, under a parent's log",
         [out("TestA", "    a_test.go:10: setup\n"), fail("TestA/sub"), fail("TestA")], None),
        ("a pass", [out("TestA", "    a_test.go:10: note\n"), {"Action": "pass", "Test": "TestA"}], None),
    ]
    wrong = [(name, go_assertion("TestA", events), want) for name, events, want in cases
             if go_assertion("TestA", events) != want]
    for name, got, want in wrong:
        print(f"self-test {name}: named {got!r}, want {want!r}")
    if wrong:
        raise SystemExit(1)
    print("SELF_TEST_PASSED")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path)
    parser.add_argument("--only", help="run only the mutants whose name holds this text")
    parser.add_argument("--start", help="skip the mutants before the one of this name")
    parser.add_argument("--self-test", action="store_true",
                        help="check how a Go test's failing assertion is read, and run nothing")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return
    if args.report is None:
        parser.error("--report is required")
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True, capture_output=True).stdout.strip()
    changed = subprocess.run(["git", "status", "--porcelain", "--untracked-files=no"], cwd=ROOT,
                             text=True, capture_output=True).stdout.strip()
    report = {"outcome": "failed", "head": head, "changedFromHead": changed.splitlines(),
              "baseline": [], "mutants": []}

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
        raw = result.pop("raw")
        report["baseline"].append({"test": name, **result})
        save()
        if result["exitCode"] != 0 or not outcome(package, test, raw)[0]:
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
                raw = result.pop("raw")
                named = assertion(package, function, raw)
                # Killed: the test's own failure, naming its assertion.
                killed = (result["exitCode"] is not None and result["exitCode"] != 0
                          and outcome(package, function, raw)[1] and named is not None)
                report["mutants"].append({"name": name, "requirement": requirement, "test": test,
                                          "sweep": SWEEP.get(name), "killed": killed,
                                          "assertion": named if killed else None,
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
