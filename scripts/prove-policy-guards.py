#!/usr/bin/env python3
"""Prove policy guards with compiling Go overlays, leaving source unchanged.

Run from any directory: python3 scripts/prove-policy-guards.py --report report.json
A report starts failed and passes only after the baseline and every mutant finish.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent.parent
SOURCE = ROOT / "backend/internal/store/policies.go"
TESTS = "TestPolicySettings"
MUTANTS = [
    ("stale-settings", "if old.Revision != next.Revision {", "if false {"),
    ("outside-grant", "if len(outside) > 0 {", "if false {"),
    (
        "required-propagation",
        "SELECT name, ? FROM workspaces WHERE NOT EXISTS",
        "SELECT name, ? FROM workspaces WHERE 0 AND NOT EXISTS",
    ),
    (
        "exclusive-classification",
        "if previous, ok := assigned[p]; ok {",
        "if previous, ok := assigned[p]; false && ok {",
    ),
]


def run(overlay=None):
    command = ["nice", "-n", "15", "go", "test", "-count=1", "-run", TESTS]
    if overlay:
        command.append("-overlay=" + str(overlay))
    command.append("./backend/internal/store")
    env = dict(os.environ, GOFLAGS="-p=2")
    return subprocess.run(command, cwd=ROOT, env=env, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    report = {"outcome": "failed", "baseline": None, "mutants": []}

    def save():
        args.report.write_text(json.dumps(report, indent=2) + "\n")

    save()
    baseline = run()
    report["baseline"] = {"exitCode": baseline.returncode, "output": baseline.stdout}
    save()
    if baseline.returncode:
        raise SystemExit("Baseline failed: " + baseline.stdout)
    source = SOURCE.read_text()
    with tempfile.TemporaryDirectory(prefix="menagerie-policy-guards-") as scratch:
        for name, before, after in MUTANTS:
            if source.count(before) != 1:
                raise SystemExit(f"Mutation anchor {name} must occur exactly once")
            changed = Path(scratch) / (name + ".go")
            changed.write_text(source.replace(before, after))
            overlay = Path(scratch) / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(SOURCE): str(changed)}}))
            result = run(overlay)
            killed = result.returncode != 0 and "--- FAIL: TestPolicySettings" in result.stdout
            killed = killed and "build failed" not in result.stdout
            report["mutants"].append({"name": name, "killed": killed,
                                      "exitCode": result.returncode, "output": result.stdout})
            save()
            if not killed:
                raise SystemExit(f"Mutant {name} survived or did not compile: {result.stdout}")
    report["outcome"] = "passed"
    save()
    print(f"POLICY_GUARDS_PASSED: {len(MUTANTS)} compiling mutants rejected")


if __name__ == "__main__":
    main()
