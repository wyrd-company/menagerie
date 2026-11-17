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
source = root / "backend/internal/provisioning/policies.go"
original = source.read_text()
mutations = [
    ("grant-before-mutation", "if e = store.CheckAllowed(wanted.Policies, allowed); e != nil {", "if false {", "TestPolicyApplyRefreshesGrantBeforeMutation"),
    ("confirmed-selection-and-applied", "if found && matches(binding, wanted) && binding.Applied {", "if found {", "TestPolicyApplyFinishesPendingAndBoundsRetries"),
    ("missing-declaration-empty", "if declaration == nil || declaration.ContainerName != name {", "if len(wanted.Policies) != 0 && (declaration == nil || declaration.ContainerName != name) {", "TestMissingEmptyBindingWithoutCurrentDeclarationSendsNothing"),
    ("once-only-stale-retry", "if changed >= 1 {", "if changed >= 0 {", "TestSuccessfulChangeWithMissingConfirmingReadResolvesAgain"),
]
for name, before, after, test in mutations:
    # The stale retry occurs in several branches; mutate its first occurrence.
    if before not in original:
        raise SystemExit(f"FAIL {name}: source marker absent")
    try:
        source.write_text(original.replace(before, after, 1))
        result = subprocess.run(
            ["nice", "-n", "15", "go", "test", "-p", "2", "-count=1", "./backend/internal/provisioning", "-run", "^" + test + "$", "-v"],
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
