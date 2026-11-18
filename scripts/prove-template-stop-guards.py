#!/usr/bin/env python3
"""Prove request stop guards with compiling Rust mutants in a disposable copy.

Run from any directory: python3 scripts/prove-template-stop-guards.py --report report.json
The source stays unchanged. The report starts failed and passes only after
all baseline tests pass and every compiling mutant fails its regression test.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent.parent
TESTS = [
    ("files", "--lib", "dropping_a_request_between_files"),
    ("git", "--test", "credentials", "a_stalled_interview_fetch"),
    ("schema", "--test", "credentials", "every_route_returns_its_worker"),
]
MUTANTS = [
    ("stop-scope", "toha::stop::with_stop_signal(self.1.clone(), operation)", "operation()"),
    ("stop-on-drop", "self.stop.stop();", "let _ = &self.stop;"),
]


def run(manifest, test):
    command = ["nice", "-n", "15", "cargo", "test", "--locked",
               "--manifest-path", str(manifest), *test[1:], "--", "--nocapture"]
    env = dict(os.environ, CARGO_BUILD_JOBS="2",
               CARGO_TARGET_DIR=str(ROOT / "template-service/target"))
    try:
        result = subprocess.run(command, cwd=ROOT, env=env, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=600, check=False)
        return {"exitCode": result.returncode, "output": result.stdout}
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
    with tempfile.TemporaryDirectory(prefix="menagerie-template-stop-") as scratch:
        copy = Path(scratch) / "template-service"
        shutil.copytree(ROOT / "template-service", copy,
                        ignore=shutil.ignore_patterns("target"))
        manifest = copy / "Cargo.toml"
        source = copy / "src/credentials.rs"
        original = source.read_text()
        for test in TESTS:
            result = run(manifest, test)
            report["baseline"].append({"test": test[0], **result})
            save()
            if result["exitCode"] != 0 or "test result: ok. 1 passed" not in result["output"]:
                raise SystemExit(f"Baseline {test[0]} failed: {result['output']}")
        for name, before, after in MUTANTS:
            if original.count(before) != 1:
                raise SystemExit(f"Mutation anchor {name} must occur exactly once")
            source.write_text(original.replace(before, after))
            for test in TESTS:
                result = run(manifest, test)
                killed = (result["exitCode"] is not None and result["exitCode"] != 0
                          and "test result: FAILED" in result["output"]
                          and "could not compile" not in result["output"])
                report["mutants"].append({"name": name, "test": test[0],
                                          "killed": killed, **result})
                save()
                if not killed:
                    raise SystemExit(f"Mutant {name}/{test[0]} survived or failed to compile: "
                                     + result["output"])
            source.write_text(original)
    report["outcome"] = "passed"
    save()
    print(f"TEMPLATE_STOP_GUARDS_PASSED: {len(report['mutants'])} compiling mutants rejected")


if __name__ == "__main__":
    main()
