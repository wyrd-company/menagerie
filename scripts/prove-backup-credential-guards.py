#!/usr/bin/env python3
"""Prove the backup repository and template credential guards by breaking them one at a time.

Run from any directory:
  python3 scripts/prove-backup-credential-guards.py --report report.json

The guards: a transfer with a key offers the Git host that key alone, and
one with an App sends only that App's token, refusing a URL it does not
serve; a registration or a change refuses a credential that does not fit
its URL (an SSH URL with no key, an App that does not serve the URL, the
operator key), records a key without its comment, and checks writing with
the credential given, not every key; a checked change stores nothing; a
change lands only at the URL it checked; the key list leaves out the
operator key; a transfer waits for a slow agent and refuses its key by
the listing that gives its signer, under every name it lists the key; the backups screen offers an https URL
only the Apps that serve it, reads hosts as Go does, matches App names in
any case and never swaps a saved credential for another. A template
records its credential, which must fit its URL; its registration, change,
version check, create and upgrade name it to the template service; a
change stores nothing with check; the credential route mints from a named
App only; and the templates screen offers the credentials that fit the URL
its address names, once that is read, and changes one only after a
Validate. Each mutation changes one guard in place, and its tests must
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
# under web/ is a Vitest file, run from web/, and its name selects every
# test whose name holds it, such as each row of the URL table.
TESTS = {
    "key-alone": ("./backend/internal/backup", "TestAgentKeyAuthOffersOnlyTheRepositorysKey"),
    "fit": ("./backend/internal/gitcreds", "TestFitRefusesACredentialThatDoesNotFitTheURL"),
    "app-alone": ("./backend/internal/gitcreds", "TestAuthUsesOnlyTheCredentialsApp"),
    "keys-listed": ("./backend/internal/gitcreds", "TestKeysListsTheAgentsKeysWithoutTheOperatorKey"),
    "register-own-credential": ("./backend/internal/backuprepos", "TestRegisterChecksWritingWithItsOwnCredential"),
    "change-checks": ("./backend/internal/backuprepos", "TestSetCredentialChecksBeforeItRecords"),
    "change-checked-url": ("./backend/internal/store", "TestABackupRepositoryKeepsItsCredential"),
    "stalled-transfer": ("./backend/internal/backup", "TestAStalledAgentFailsTheTransferInTime"),
    "stalled-discovery": ("./backend/internal/gitcreds", "TestAStalledAgentFailsDiscoveryAndTheFitInTime"),
    "write-check-names-credential": ("./backend/internal/backuprepos", "TestAFailedWriteCheckNamesItsCredential"),
    "wait-per-request": ("./backend/internal/backup", "TestAgentWaitBoundsEachRequestAlone"),
    "slow-transfer": ("./backend/internal/backup", "TestATransferWaitsForASlowAgent"),
    "one-listing": ("./backend/internal/backup", "TestTheRefusalChecksTheListingThatGivesTheSigner"),
    "every-name": ("./backend/internal/backup", "TestTheRefusalChecksEveryNameOfTheKey"),
    "keys-every-name": ("./backend/internal/gitcreds", "TestKeysLeaveOutTheOperatorKeyUnderEveryName"),
    "keys-once": ("./backend/internal/gitcreds", "TestKeysListAKeyUnderTwoNamesOnce"),
    "saved-operator-key": ("./backend/internal/gitcreds", "TestAuthRefusesASavedKeyThatIsNowTheOperatorKey"),
    "service-url-table": ("./backend/internal/gitcreds", "TestTheServiceReadsURLFormsAsThePickerDoes"),
    "picker-url-table": ("web/test/contract/credential-urls.test.ts", "URL table: "),
    "picker-app-case": ("web/src/ui/CredentialPicker.test.ts", "matches a saved App's name in any case"),
    "picker-app-moved": ("web/src/ui/CredentialPicker.test.ts", "says when a saved App no longer serves the repository"),
    "picker-key-kept": ("web/src/ui/CredentialPicker.test.ts", "does not swap a saved key"),
    "picker-kind": ("web/src/ui/credentialUrls.test.ts", "reads a URL's kind as the service does"),
    "picker-apps": ("web/src/ui/credentialUrls.test.ts", "finds the Apps that serve an https URL as the service does"),
    "screen-url-forms": ("web/src/screens/BackupRepositories.test.tsx",
                         "offers credentials for the URL forms the service accepts"),
    "screen-apps": ("web/src/screens/BackupRepositories.test.tsx",
                    "offers an https URL only the Apps that serve it, and none off their servers"),
    "template-fetches": ("./backend/internal/registry", "TestATemplateFetchesWithItsCredential"),
    "template-change": ("./backend/internal/registry", "TestATemplatesCredentialChangesOnceItsChecksPass"),
    "template-kept": ("./backend/internal/store", "TestATemplateKeepsItsCredential"),
    "template-requests": ("./backend/internal/workflows", "TestEveryTemplateRequestNamesTheTemplatesCredential"),
    "named-app": ("./backend/internal/githubapp", "TestTheCredentialRouteMintsFromTheNamedAppOnly"),
    "authenticate": ("./backend/internal/gitcreds", "TestAuthenticateMintsFromTheNamedApp"),
    "refusal-named": ("./backend/internal/workflows", "TestARefusedDescentCheckNamesTheTemplatesCredential"),
    "check-line": ("web/src/api/versions.test.ts", "names the credential the Git host refused"),
    "template-form": ("web/src/screens/Templates.test.tsx",
                      "is chosen from those that fit the URL the address names"),
    "template-change-form": ("web/src/screens/Templates.test.tsx",
                             "changes only after a Validate with it passes"),
}

PUSH = "backend/internal/backup/push.go"
CREDS = "backend/internal/gitcreds/gitcreds.go"
REGISTRY = "backend/internal/backuprepos/backuprepos.go"
BACKUPS = "backend/internal/store/backups.go"
PICKER = "web/src/ui/CredentialPicker.tsx"
TEMPLATES = "backend/internal/registry/registry.go"
VERSIONS = "backend/internal/registry/versions.go"
TEMPLATE_DATA = "backend/internal/store/templates.go"
FLOWS = "backend/internal/workflows/workflows.go"
DESCENT = "backend/internal/workflows/upgrade.go"
ROUTE = "backend/internal/githubapp/handler.go"
APPLY = "backend/internal/workflows/applybranch.go"
CHECK_LINE = "web/src/api/versions.ts"
TEMPLATES_SCREEN = "web/src/screens/Templates.tsx"
URLS = "web/src/ui/credentialUrls.ts"

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
     "\t\tif excluded {\n", "\t\tif excluded && false {\n", ["fit"]),
    ("operator-key-unchecked-refused", CREDS,
     "\t\tif err != nil {\n\t\t\treturn &FitError{Message: err.Error() + \", so Menagerie",
     "\t\tif false {\n\t\t\treturn &FitError{Message: err.Error() + \", so Menagerie",
     ["stalled-discovery"]),
    ("agent-request-waits", PUSH,
     "\t\tif err := c.conn.SetDeadline(time.Now().Add(c.wait)); err != nil {\n",
     "\t\tif err := error(nil); err != nil {\n", ["stalled-discovery"]),
    ("transfer-has-no-wait", PUSH,
     "\t\tkeyring, release, err := DialAgent(ctx, socket, 0)\n",
     "\t\tkeyring, release, err := DialAgent(ctx, socket, time.Second)\n", ["slow-transfer"]),
    ("refusal-reads-the-signers-listing", PUSH,
     "\tsigners, err := keyring.Signers()\n\tif err != nil {\n\t\treturn nil, fmt.Errorf(\"ssh agent: %w\", err)\n\t}\n"
     "\tvar found ssh.Signer\n\tfor _, signer := range signers {\n\t\tif !bytes.Equal(signer.PublicKey().Marshal(), wanted.Marshal()) {\n"
     "\t\t\tcontinue\n\t\t}\n\t\tif refuse != nil {\n",
     "\tfirst, _ := keyring.List()\n"
     "\tsigners, err := keyring.Signers()\n\tif err != nil {\n\t\treturn nil, fmt.Errorf(\"ssh agent: %w\", err)\n\t}\n"
     "\tvar found ssh.Signer\n\tfor _, signer := range signers {\n\t\tif !bytes.Equal(signer.PublicKey().Marshal(), wanted.Marshal()) {\n"
     "\t\t\tcontinue\n\t\t}\n"
     "\t\tif refuse != nil && func() bool {\n\t\t\tfor _, k := range first {\n"
     "\t\t\t\tif bytes.Equal(k.Marshal(), wanted.Marshal()) {\n\t\t\t\t\treturn true\n\t\t\t\t}\n"
     "\t\t\t}\n\t\t\treturn false\n\t\t}() {\n",
     ["one-listing"]),
    ("agent-idle-between-requests", PUSH,
     "\treturn agent.NewClient(&agentRequests{conn: conn, wait: wait}), release, nil\n",
     "\treturn agent.NewClient(struct {\n\t\t*agentRequests\n\t\tio.Closer\n\t}{&agentRequests{conn: conn, wait: wait}, conn}), release, nil\n",
     ["wait-per-request"]),
    ("saved-key-refused-when-excluded", CREDS,
     "\tif s.Excluded != \"\" && selects(s.Excluded, key) {\n", "\tif false {\n", ["saved-operator-key"]),
    ("transfer-applies-the-refusal", PUSH,
     "\t\t\tif err := refuse(listed); err != nil {\n", "\t\t\tif err := refuse(listed); err != nil && false {\n",
     ["saved-operator-key"]),
    ("agent-closes-with-its-operation", PUSH,
     "\tstop := context.AfterFunc(ctx, func() { _ = conn.Close() })\n",
     "\tstop := func() bool { return false }\n", ["stalled-transfer"]),
    ("app-token-only-where-served", CREDS,
     "\t\tif !tokens.Serves(url) {\n", "\t\tif false {\n", ["app-alone"]),
    ("keys-leave-out-the-operator-key", CREDS,
     "\t\tif s.Excluded != \"\" && selects(s.Excluded, k) {\n", "\t\tif false {\n", ["keys-listed"]),
    ("key-recorded-without-comment", CREDS,
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
    ("write-check-names-its-credential", REGISTRY,
     "\t\treturn &CheckError{Check: CheckWrite, Message: \"Menagerie could not write to the repository\" + gitcreds.Phrase(repo.Credential) + \": \"",
     "\t\treturn &CheckError{Check: CheckWrite, Message: \"Menagerie could not write to the repository\" + \": \"",
     ["write-check-names-credential"]),
    ("picker-scp-like-without-user", URLS,
     "const scpLikePattern = /^(?:[^@]+@)?",
     "const scpLikePattern = /^(?:[^@]+@)", ["picker-kind", "screen-url-forms"]),
    ("picker-default-port", URLS,
     "    host: goLower(host).replace(/:443$/, \"\"),\n",
     "    host: goLower(host),\n", ["picker-url-table", "screen-url-forms"]),
    ("picker-unescaped-host", URLS,
     "  const host = decodeHost(m[2]!);\n", "  const host = m[2]!;\n", ["picker-url-table"]),
    ("picker-unescapes-every-byte", URLS,
     "    if (raw[i] === \"%\" && /^[0-9A-Fa-f]{2}$/.test(hex)) {\n",
     "    if (raw[i] === \"%\" && /^[89A-Fa-f][0-9A-Fa-f]$/.test(hex)) {\n", ["picker-url-table"]),
    ("picker-lower-case-as-go", URLS,
     "    host: goLower(host).replace(/:443$/, \"\"),\n",
     "    host: host.toLowerCase().replace(/:443$/, \"\"),\n", ["picker-url-table"]),
    ("picker-keeps-a-leading-bom", URLS,
     "{ ignoreBOM: true }", "{ ignoreBOM: false }", ["picker-url-table"]),
    ("picker-go-regexp-classes", URLS,
     "[^\\\\][^\\n]*$/;", "[^\\\\].*$/;", ["picker-url-table"]),
    ("keys-one-choice-per-key", CREDS,
     "\t\tif i, ok := at[blob]; ok {\n", "\t\tif i, ok := at[blob]; ok && false {\n", ["keys-once"]),
    ("refusal-checks-every-name", PUSH,
     "\t\tif found == nil {\n\t\t\tfound = signer\n\t\t}\n", "\t\treturn signer, nil\n", ["every-name"]),
    ("keys-leave-out-every-name", CREDS,
     "\t\tif excluded[blob] {\n",
     "\t\tif s.Excluded != \"\" && selects(s.Excluded, k) {\n", ["keys-every-name"]),
    ("picker-app-names-in-any-case", PICKER,
     "const sameApp = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();\n",
     "const sameApp = (a: string, b: string) => a === b;\n", ["picker-app-case"]),
    ("picker-keeps-a-saved-app", PICKER,
     "    return app ? { githubApp: app.name } : null;\n",
     "    if (app) return { githubApp: app.name };\n", ["picker-app-moved"]),
    ("picker-keeps-a-saved-key", PICKER,
     "    return keys.some((k) => k.publicKey === chosen.sshKey) ? chosen : null;\n",
     "    if (keys.some((k) => k.publicKey === chosen.sshKey)) return chosen;\n", ["picker-key-kept"]),
    ("picker-says-an-app-moved", PICKER,
     "    if (url !== null && servingApps(url, [app]).length === 0)\n", "    if (false)\n", ["picker-app-moved"]),
    ("picker-empty-fragment", URLS,
     "  if (u.rest.startsWith(\"?\") || u.rest.length > 1) return false;\n",
     "  if (u.rest.length > 0) return false;\n", ["picker-url-table"]),
    ("picker-raw-path", URLS,
     "    path: end < 0 ? tail : tail.slice(0, end),\n",
     "    path: new URL(\"https://h\" + (end < 0 ? tail : tail.slice(0, end))).pathname,\n",
     ["picker-url-table"]),
    ("template-lists-with-its-credential", TEMPLATES,
     "ListVersions(ctx, read.Base, requested(credential))", "ListVersions(ctx, read.Base, nil)",
     ["template-fetches"]),
    ("template-checks-with-its-credential", TEMPLATES,
     "r.Templates.Check(ctx, address, v.Tag, requested(credential))",
     "r.Templates.Check(ctx, address, v.Tag, nil)", ["template-fetches", "template-change"]),
    ("template-credential-must-fit", TEMPLATES,
     "\t\tif err = r.Credentials.Fit(ctx, url, credential); err == nil {\n",
     "\t\tif err = nil; err == nil {\n", ["template-fetches"]),
    ("template-app-must-authenticate", TEMPLATES,
     "\t\t\terr = r.Credentials.Authenticate(ctx, url, credential)\n",
     "\t\t\terr = nil\n", ["template-fetches"]),
    ("template-checked-change-stores-nothing", TEMPLATES,
     "\tif check {\n\t\treturn tpl, nil\n\t}\n", "", ["template-change"]),
    ("version-check-with-its-credential", VERSIONS,
     "ListVersions(ctx, tpl.Address, requested(tpl.Credential))", "ListVersions(ctx, tpl.Address, nil)",
     ["template-fetches"]),
    ("template-credential-recorded", TEMPLATE_DATA,
     "nullable(tpl.Credential.App), nullable(tpl.Credential.Key)); err != nil {",
     "nil, nil); err != nil {", ["template-kept"]),
    ("job-requests-name-the-credential", FLOWS,
     ",\n\t\tCredential: templates.CredentialOf(tpl.Credential.App, tpl.Credential.Key)}", "}",
     ["template-requests"]),
    ("descent-check-names-the-credential", DESCENT,
     "applied.Commit, templates.CredentialOf(tpl.Credential.App, tpl.Credential.Key))",
     "applied.Commit, nil)", ["template-requests"]),
    ("route-mints-from-the-named-app", ROUTE,
     "\t\tcase req.GitHubApp != nil:\n", "\t\tcase false:\n", ["named-app"]),
    ("route-mints-from-the-named-app-alone", ROUTE,
     "\t\ttoken, err := app.Token(r.Context(), repo, Read)\n",
     "\t\tif len(apps) > 0 {\n\t\t\tapp = apps[0]\n\t\t}\n\t\ttoken, err := app.Token(r.Context(), repo, Read)\n",
     ["named-app"]),
    ("authenticate-mints-a-token", CREDS,
     "\tif _, err := app.Token(ctx, repo, githubapp.Read); err != nil {\n",
     "\tif _, err := repo, error(nil); err != nil {\n", ["authenticate"]),
    ("upgrade-refusal-names-the-credential", DESCENT,
     "applied.Commit, gitcreds.Phrase(tpl.Credential), refusalKind(err))",
     "applied.Commit, gitcreds.Phrase(store.Credential{}), refusalKind(err))", ["refusal-named"]),
    ("apply-refusal-names-the-credential", APPLY,
     "applied.Commit, gitcreds.Phrase(tpl.Credential), refusalKind(err))",
     "applied.Commit, gitcreds.Phrase(store.Credential{}), refusalKind(err))", ["refusal-named"]),
    ("check-line-names-the-credential", CHECK_LINE,
     "hostRefused.has(t.lastCheck.failure) ? phrase(t.credential) : \"\"",
     "\"\"", ["check-line"]),
    ("template-form-fits-the-address-url", TEMPLATES_SCREEN,
     "  const credential = effectiveChoice(url, credentials, chosen);\n",
     "  const credential = effectiveChoice(\"\", credentials, chosen);\n", ["template-form"]),
    ("template-validate-waits-for-the-url", TEMPLATES_SCREEN,
     " && !pending;\n", ";\n", ["template-form"]),
    ("template-change-validates-first", TEMPLATES_SCREEN,
     "setTemplateCredential(t.id, credential, check)", "setTemplateCredential(t.id, credential, false)",
     ["template-change-form"]),
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
        failed = re.search(r"Tests +\d+ failed", output) is not None
        return (not failed and re.search(r"Tests +\d+ passed", output) is not None, failed)
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
    print(f"CREDENTIAL_GUARDS_PASSED: {len(report['mutants'])} mutant runs rejected")


if __name__ == "__main__":
    main()
