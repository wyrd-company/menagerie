#!/usr/bin/env python3
"""Prove the template service's admission guards with compiling Rust mutants
in a disposable copy.

Run from any directory:
  python3 scripts/prove-template-admission-guards.py --report report.json

The guards: an address is a remote Git address of a documented form, holds
no dashed host or user, reads one way (no `:` before an `@` after the
scheme of an https or ssh address, each part gix and Toha read in its
strict, non-empty character set, the strict writer writing the parts back
as the address, and a ref that could end a password a version's tag),
loses its user name in an `https` base, names no ref where a route names
the version beside it, whose ref is checked alone and read back as the
admitted repository and folder at exactly that ref; a template folder and its links stay in the
checkout, checked before a start, a generate or an apply reads the
template; a ref that resolves to another commit than the recorded one is
stale, and so is a tag that moves between an upgrade start's two
resolutions; a session's signature covers its address and ref. The source stays
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
    "unit-rule": ("--lib", "address::tests::every_address_the_rule_refuses_is_refused_and_every_plain_one_admitted"),
    "unit-strict": ("--lib", "address::tests::a_strict_part_is_never_empty_and_a_host_is_lower_case"),
    "unit-readback": ("--lib", "address::tests::the_ref_beside_an_address_reads_back_as_that_repository_folder_and_ref"),
    "shorthand": ("--lib", "address::tests::a_shorthand_must_stay_https_once_expanded"),
    "versions": ("--test", "templates", "versions_are_the_release_tags_in_ascending_order"),
    "refs": ("--lib", "address::tests::a_version_is_named_by_its_ref_beside_an_address_without_one"),
    "check-links": ("--test", "templates", "a_check_refuses_a_folder_or_link_that_leaves_the_checkout"),
    "unit-links": ("--lib", "links::tests::a_folder_or_link_that_leaves_the_checkout_is_refused"),
    "route-links": ("--test", "templates", "every_route_that_reads_a_template_refuses_a_link_that_leaves_the_checkout"),
    "stale": ("--test", "requests", "a_ref_that_resolves_to_another_commit_than_the_recorded_one_is_stale"),
    "moved-tag": ("--test", "upgrade", "a_tag_moved_between_the_upgrade_starts_two_fetches_is_stale"),
    "signature": ("--test", "requests", "a_session_value_is_signed_for_its_record_workspace_template_and_flow"),
}

# Each mutant: a name, the source file, the text it replaces (exactly once),
# its replacement, and the tests that must each fail.
MUTANTS = [
    ('remote-forms', 'src/address.rs',
     '    } else {\n        return Err(refused(NOT_REMOTE));\n    };\n',
     '    } else {\n        Form::Url("https")\n    };\n', ['unit-forms']),
    ('colon-before-at', 'src/address.rs',
     '    if matches!(form, Form::Url(_)) && colon_before_at(address) {\n',
     '    if false {\n', ['unit-forms', 'unit-rule']),
    ('colon-before-at-order', 'src/address.rs',
     "        .is_some_and(|colon| rest[colon..].contains('@'))\n",
     "        .is_some_and(|colon| rest[..colon].contains('@'))\n", ['unit-admits', 'unit-forms']),
    ('dashed', 'src/address.rs',
     "    if host_name.starts_with('-') || url.user().is_some_and(|user| user.starts_with('-')) {\n",
     '    if false {\n', ['unit-forms']),
    ('shorthand-https', 'src/address.rs',
     '    if !(https || ssh) || (form == Form::Shorthand && !https) {\n',
     '    if !(https || ssh) {\n', ['shorthand']),
    ('strict-user', 'src/address.rs',
     '    if url.user().is_some_and(|user| !strictly(user, user_char)) {\n',
     '    if false {\n', ['unit-forms']),
    ('strict-host', 'src/address.rs',
     '    if !strictly(host_name, host_char) {\n',
     '    if false {\n', ['unit-forms']),
    ('strict-path', 'src/address.rs',
     '    if !strictly(&String::from_utf8_lossy(url.path.as_ref()), path_char) {\n',
     '    if false {\n', ['unit-forms']),
    ('strict-ref', 'src/address.rs',
     '    if reference.is_some_and(|reference| !strictly(reference, ref_char)) {\n',
     '    if false {\n', ['unit-forms']),
    ('strict-folder', 'src/address.rs',
     '    if folder.is_some_and(|folder| !strictly(folder, folder_char)) {\n',
     '    if false {\n', ['unit-forms']),
    ('strict-nonempty', 'src/address.rs',
     '    !part.is_empty() && part.chars().all(allowed)\n',
     '    part.chars().all(allowed)\n', ['unit-strict']),
    ('host-lower-case', 'src/address.rs',
     "    c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '.' | '-')\n",
     "    c.is_ascii_alphabetic() || c.is_ascii_digit() || matches!(c, '.' | '-')\n", ['unit-strict']),
    ('tilde-slash', 'src/address.rs',
     "            if !path.starts_with('/') {\n                written.push('/');\n            }\n",
     '', ['unit-admits']),
    ('rebuild', 'src/address.rs',
     '    if write(form, &url, true, reference, folder).as_deref() != Some(address) {\n',
     '    if false {\n', ['unit-forms']),
    ('ref-host-like', 'src/address.rs',
     "    let host_like = reference.is_some_and(|reference| reference.contains('.'));\n",
     '    let host_like = false;\n', ['unit-forms']),
    ('ref-after-scp', 'src/address.rs',
     '    let after_scp = form == Form::Scp;\n',
     '    let after_scp = false;\n', ['unit-forms']),
    ('base-without-user', 'src/address.rs',
     '    let base = write(form, &url, !https || form == Form::Shorthand, None, folder)\n',
     '    let base = write(form, &url, true, None, folder)\n', ['unit-admits']),
    ('ref-beside-strict', 'src/address.rs',
     "    if !strictly(reference, ref_char) || reference.starts_with('-') {\n",
     "    if reference.starts_with('-') {\n", ['refs']),
    ('ref-beside-dash', 'src/address.rs',
     "    if !strictly(reference, ref_char) || reference.starts_with('-') {\n",
     '    if !strictly(reference, ref_char) {\n', ['refs']),
    ('readback-repo', 'src/address.rs',
     '    if at_repo != repo {\n        return false;\n    }\n',
     '', ['unit-readback']),
    ('readback-folder', 'src/address.rs',
     '    if at_folder != folder {\n        return false;\n    }\n',
     '', ['unit-readback']),
    ('readback-ref', 'src/address.rs',
     '    at_reference.as_deref() == Some(reference)\n',
     '    true\n', ['unit-readback']),
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
    ("upgrade-moved-between", "src/upgrade.rs",
     "    if prepared.resolved.commit != checked.commit {\n", "    if false {\n", ["moved-tag"]),
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
