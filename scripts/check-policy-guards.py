#!/usr/bin/env python3
"""Prove the policy guards by breaking them one at a time (run on Boat).

Each mutation must make its focused behavioral test fail. The original file is
restored even on interruption; an unexpected crash or a non-running test never
counts as a killed mutation. Uses only Python and the project's Go toolchain.
"""
import os
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parent.parent
mutations = [
    ("grant-before-mutation", "if e = store.CheckAllowed(wanted.Policies, allowed); e != nil {", "if false {", "TestPolicyApplyRefreshesGrantBeforeMutation"),
    ("confirmed-selection-and-applied", "if found && matches(binding, wanted) && binding.Applied {", "if found {", "TestPolicyApplyFinishesPendingAndBoundsRetries"),
    ("missing-declaration-empty", "if declaration == nil || declaration.ContainerName != name {", "if len(wanted.Policies) != 0 && (declaration == nil || declaration.ContainerName != name) {", "TestMissingEmptyBindingWithoutCurrentDeclarationSendsNothing"),
    ("once-only-stale-retry", "if changed >= 1 {", "if changed >= 0 {", "TestSuccessfulChangeWithMissingConfirmingReadResolvesAgain"),
]
mutations = [
    (name, "backend/internal/provisioning/policies.go", before, after, "provisioning", test)
    for name, before, after, test in mutations
] + [
    ("failed-revision-cleared", "backend/internal/store/selection.go", "THEN NULL ELSE applied_revision END, apply_failure", "THEN applied_revision ELSE applied_revision END, apply_failure", "store", "TestFailedApplyClearsOnlyItsOwnAppliedRevision"),
    ("unapplied-before-write", "backend/internal/provisioning/policies.go", "if e = s.opts.Data.Applying(ctx, workspace, current.Revision); e != nil {", "if false {", "provisioning", "TestInterruptedPolicyWriteIsUnappliedBeforeMutation"),
    ("import-before-retirement", "backend/internal/provisioning/steps.go", "if err := s.EnsureImported(ctx, folder); err != nil {", "if false {", "provisioning", "TestPendingImportPrecedesRetirementWithoutDeclaration"),
    ("stored-key-to-bringup", "backend/internal/provisioning/steps.go", "naming.SetXolotlKey(d.Xolotl.String())", "_ = naming", "provisioning", "TestProvisioningPassesStoredXolotlKeyToBringUp"),
    ("noncurrent-legacy-selection", "backend/internal/provisioning/policies.go", "tx.ImportSelection(name, policies, values, key)", "tx.ImportSelection(name, policies[:0], values, key)", "provisioning", "TestSelectionImportWithoutCurrentDeclaredNameUsesLegacyMembers"),
    ("frozen-absent-key", "backend/internal/provisioning/policies.go", "s.opts.Data.ImportSelection(ctx, workspace, policies, values, current.Key)", "s.opts.Data.ImportSelection(ctx, workspace, policies, values, func() *Key { if current.Key != nil { return current.Key }; _, _, key := legacySelection(def); return key }())", "provisioning", "TestDelayedSelectionImportKeepsOriginallyAbsentKey"),
]
for name, relative, before, after, package, test in mutations:
    source = root / relative
    original = source.read_text()
    # The stale retry occurs in several branches; mutate its first occurrence.
    if before not in original:
        raise SystemExit(f"FAIL {name}: source marker absent")
    try:
        source.write_text(original.replace(before, after, 1))
        result = subprocess.run(
            ["nice", "-n", "15", "go", "test", "-p", "2", "-count=1", "./backend/internal/" + package, "-run", "^" + test + "$", "-v"],
            cwd=root, env={**os.environ, "GOFLAGS": "-p=2"},
            capture_output=True, text=True, check=False,
        )
        output = result.stdout + result.stderr
        print(output, end="")
        if result.returncode == 0 or f"--- FAIL: {test}" not in output:
            raise SystemExit(f"FAIL {name}: mutation survived or check did not run")
        print(f"KILLED {name}")
    finally:
        source.write_text(original)
print(f"PASS: {len(mutations)} policy guards killed by behavioral tests")
