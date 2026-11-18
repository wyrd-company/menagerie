#!/usr/bin/env python3
"""Prove the template service's admission guards with compiling Rust mutants
in a disposable copy.

Run from any directory:
  python3 scripts/prove-template-admission-guards.py --report report.json

The guards: an address is a remote Git address of a documented form, holds
no password and no dashed host or user, reads one way for its user
information (one `@` after the host, starting a ref with no `/` or `:`, and
a user name the fetcher reads as typed), loses its user name in an `https`
base, names no ref where a route names the version beside it; a template folder and its links stay in the
checkout, checked before a start, a generate or an apply reads the
template; a ref that resolves to another commit than the recorded one is
stale; a session's signature covers its address and ref. The source stays
unchanged. The report starts failed and passes only after every baseline
test passes and every mutant compiles and fails each of its tests.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent.parent

# Each test: a name, then the cargo test arguments that select exactly one test.
TESTS = {
    "forms": ("--test", "templates", "every_route_refuses_an_address_it_must_never_read_without_quoting_it"),
    "routes": ("--test", "requests", "every_route_refuses_an_address_it_must_never_read"),
    "unit-forms": ("--lib", "address::tests::refuses_every_other_address_without_quoting_it"),
    "unit-admits": ("--lib", "address::tests::admits_each_documented_form"),
    "shorthand": ("--lib", "address::tests::a_shorthand_must_stay_https_once_expanded"),
    "versions": ("--test", "templates", "versions_are_the_release_tags_in_ascending_order"),
    "refs": ("--lib", "address::tests::a_version_is_named_by_its_ref_beside_an_address_without_one"),
    "check-links": ("--test", "templates", "a_check_refuses_a_folder_or_link_that_leaves_the_checkout"),
    "unit-links": ("--lib", "links::tests::a_folder_or_link_that_leaves_the_checkout_is_refused"),
    "route-links": ("--test", "templates", "every_route_that_reads_a_template_refuses_a_link_that_leaves_the_checkout"),
    "stale": ("--test", "requests", "a_ref_that_resolves_to_another_commit_than_the_recorded_one_is_stale"),
    "signature": ("--test", "requests", "a_session_value_is_signed_for_its_record_workspace_template_and_flow"),
}

# Each mutant: a name, the source file, the text it replaces (exactly once),
# its replacement, and the tests that must each fail.
MUTANTS = [
    ("remote-forms", "src/address.rs", "    if !remote {\n", "    if false {\n",
     ["forms", "unit-forms"]),
    ("password", "src/address.rs", "    if source::has_password(address) {\n",
     "    if false {\n", ["unit-forms"]),
    ("dashed", "src/address.rs",
     "    if host_name.starts_with('-') || url.user().is_some_and(|user| user.starts_with('-')) {\n",
     "    if false {\n", ["unit-forms"]),
    ("user-info-one-at", "src/address.rs", "    if after_host.matches('@').count() > 1 {\n",
     "    if false {\n", ["unit-forms", "forms"]),
    # The routes that take a ref beside the address already refuse an
    # address in which Toha finds one, so the address route observes this.
    ("user-info-ref", "src/address.rs", "        && reference.contains(['/', ':'])\n",
     "        && false\n", ["unit-forms", "forms"]),
    ("user-info-typed-user", "src/address.rs",
     "    if !shorthand && url.user() != typed_user {\n", "    if false {\n",
     ["unit-forms", "forms", "routes"]),
    ("base-without-user", "src/address.rs",
     "        Some(user) if https && !shorthand => {\n            written.replacen(&format!(\"https://{user}@\"), \"https://\", 1)\n        }\n",
     "        Some(_) if https && !shorthand => written.clone(),\n", ["unit-admits"]),
    ("shorthand-https", "src/address.rs", "    if !(https || ssh) || (shorthand && !https) {\n",
     "    if !(https || ssh) {\n", ["shorthand"]),
    ("versions-ref", "src/templates.rs", "    if admitted.reference.is_some() {\n",
     "    if false {\n", ["versions"]),
    ("ref-on-address", "src/address.rs", "    if plain.reference.is_some() {\n",
     "    if false {\n", ["refs"]),
    ("link-target", "src/links.rs", "                if !resolves_inside(&path, &root) {\n",
     "                if false {\n", ["check-links", "unit-links", "route-links"]),
    ("folder-in-checkout", "src/links.rs", "    if !folder.starts_with(&root) {\n",
     "    if false {\n", ["unit-links"]),
    ("generate-links", "src/create.rs",
     "    crate::links::check_session(&record, &service.host, &config, &registry)?;\n", "",
     ["route-links"]),
    ("apply-links", "src/upgrade.rs",
     "    crate::links::check_session(&record, &service.host, &config, &registry)?;\n", "",
     ["route-links"]),
    ("start-links", "src/create.rs",
     "    crate::links::check_resolved(&resolved, &service.host, &config)?;\n", "",
     ["route-links"]),
    ("stale-commit", "src/checks.rs", "            Some(recorded) if recorded != commit => Err(",
     "            Some(recorded) if false && recorded != commit => Err(", ["stale"]),
    ("signature-ref", "src/seal.rs", "        binding.reference.as_bytes(),\n", "",
     ["signature"]),
]


def run(manifest, target, test):
    command = ["nice", "-n", "15", "cargo", "test", "--locked",
               "--manifest-path", str(manifest), *test, "--", "--exact", "--nocapture"]
    env = dict(os.environ, CARGO_BUILD_JOBS=os.environ.get("CARGO_BUILD_JOBS", "2"),
               CARGO_TARGET_DIR=str(target))
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
    with tempfile.TemporaryDirectory(prefix="menagerie-template-admission-") as scratch:
        copy = Path(scratch) / "template-service"
        # Copies get the current time, so cargo rebuilds the baseline even
        # when the target folder holds a mutant an earlier run built later
        # than the sources were last changed.
        shutil.copytree(ROOT / "template-service", copy,
                        ignore=shutil.ignore_patterns("target"), copy_function=shutil.copy)
        # The contract test reads the specification beside the copy.
        shutil.copytree(ROOT / "docs", Path(scratch) / "docs")
        manifest = copy / "Cargo.toml"
        target = Path(os.environ.get("CARGO_TARGET_DIR", ROOT / "template-service/target"))
        for name, test in TESTS.items():
            result = run(manifest, target, test)
            report["baseline"].append({"test": name, **result})
            save()
            if result["exitCode"] != 0 or "test result: ok. 1 passed" not in result["output"]:
                raise SystemExit(f"Baseline {name} failed: {result['output']}")
        for name, path, before, after, tests in MUTANTS:
            source = copy / path
            original = source.read_text()
            if original.count(before) != 1:
                raise SystemExit(f"Mutation anchor {name} must occur exactly once in {path}")
            source.write_text(original.replace(before, after))
            try:
                for test in tests:
                    result = run(manifest, target, TESTS[test])
                    killed = (result["exitCode"] is not None and result["exitCode"] != 0
                              and "test result: FAILED" in result["output"]
                              and "could not compile" not in result["output"])
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
    print(f"TEMPLATE_ADMISSION_GUARDS_PASSED: {len(report['mutants'])} mutant runs rejected")


if __name__ == "__main__":
    main()
