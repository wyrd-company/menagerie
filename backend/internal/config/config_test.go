package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

const valid = `
workspace-root: /srv/workspaces/
workspace-repository: git@example.test:example/workspaces.git
wyrwood-directory: /run/wyrwood/containers/example
oidc:
  issuer: https://id.example.test
  client-id: example-client
  redirect-url: https://menagerie.example.test/auth/callback
templates:
  - id: sample-template
    name: Sample template
    address: git@example.test:example/sample-template.git
`

func TestParseValidDocument(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceRoot != "/srv/workspaces" {
		t.Errorf("workspace root not cleaned: %q", cfg.WorkspaceRoot)
	}
	if cfg.Listen != DefaultListen || cfg.TemplateService.Socket != DefaultTemplateSocket {
		t.Errorf("defaults: %q %q", cfg.Listen, cfg.TemplateService.Socket)
	}
	if len(cfg.Templates) != 1 || cfg.Templates[0].ID != "sample-template" || cfg.Templates[0].Address == "" {
		t.Errorf("templates %+v", cfg.Templates)
	}
	if cfg.OIDC.Issuer != "https://id.example.test" || cfg.OIDC.ClientID != "example-client" ||
		cfg.OIDC.RedirectURL != "https://menagerie.example.test/auth/callback" || cfg.OIDC.Scopes != nil {
		t.Errorf("oidc %+v", cfg.OIDC)
	}
}

func TestLoadReadsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(valid+"listen: 127.0.0.1:9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("listen %q", cfg.Listen)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Error("missing file accepted")
	}
}

const registryCredentials = `
registry-credentials:
  kv-mount: kv
  registries:
    - registry: ghcr.io
      path: menagerie/registries/ghcr
    - registry: Registry-1.Docker.io
      path: menagerie/registries/hub
`

func TestParseRegistryCredentials(t *testing.T) {
	cfg, err := Parse([]byte(valid + registryCredentials))
	if err != nil {
		t.Fatal(err)
	}
	rc := cfg.RegistryCredentials
	if rc == nil || rc.KVMount != "kv" || len(rc.Registries) != 2 || rc.Registries[1].Registry != "Registry-1.Docker.io" || rc.Registries[0].Path != "menagerie/registries/ghcr" {
		t.Fatalf("registry credentials %+v", rc)
	}
	cfg, err = Parse([]byte(valid))
	if err != nil || cfg.RegistryCredentials != nil {
		t.Fatalf("absent section: %+v %v", cfg.RegistryCredentials, err)
	}
	cfg, err = Parse([]byte(valid + "registry-credentials:\n  kv-mount: kv\n"))
	if err != nil || len(cfg.RegistryCredentials.Registries) != 0 {
		t.Fatalf("section without registries: %v", err)
	}
}

func TestParseLocalEnv(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || cfg.LocalEnv != nil {
		t.Fatalf("absent local-env: %q %v", cfg.LocalEnv, err)
	}
	cfg, err = Parse([]byte(valid + "local-env: [TZ, LANG, _UNDERSCORE_1]\n"))
	if err != nil || strings.Join(cfg.LocalEnv, ",") != "TZ,LANG,_UNDERSCORE_1" {
		t.Fatalf("local-env %q %v", cfg.LocalEnv, err)
	}
}

const limitsAndAlerts = `
job-limits:
  default: 2h
  operations:
    rebuild: 1h30m
  phases:
    initialize-command: 10m
    build: 45m
alerts:
  webhook: https://hooks.example.test/menagerie/tok-0123456789
  long-running: 45s
`

func TestParseJobLimitsAndAlerts(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || cfg.JobLimits != nil || cfg.Alerts != nil {
		t.Fatalf("absent sections: %+v %+v %v", cfg.JobLimits, cfg.Alerts, err)
	}
	cfg, err = Parse([]byte(valid + limitsAndAlerts))
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.JobLimits
	if l.Default.Value() != 2*time.Hour || l.Operations.Rebuild.Value() != 90*time.Minute || l.Operations.Start.Value() != 0 ||
		l.Phases["build"].Value() != 45*time.Minute || len(l.Phases) != 2 {
		t.Fatalf("limits %+v", l)
	}
	if cfg.Alerts.Webhook != "https://hooks.example.test/menagerie/tok-0123456789" || cfg.Alerts.LongRunning.Value() != 45*time.Second {
		t.Fatalf("alerts %+v", cfg.Alerts)
	}
	if _, err := Parse([]byte(valid + "alerts:\n  webhook: http://hooks.example.test\n")); err != nil {
		t.Fatalf("webhook without long-running: %v", err)
	}
}

// TestInvalidWebhookIsNotEchoed keeps a webhook credential out of the
// configuration error, whatever is wrong with the webhook.
func TestInvalidWebhookIsNotEchoed(t *testing.T) {
	const secret = "tok-0123456789"
	hook := "https://hooks.example.test/" + secret
	cases := map[string]struct{ doc, want string }{
		"int tag":     {"alerts:\n  webhook: !!int " + hook + "\n", "alerts.webhook must be an http or https URL"},
		"bool tag":    {"alerts:\n  webhook: !!bool " + hook + "\n", "alerts.webhook must be an http or https URL"},
		"mapping":     {"alerts:\n  webhook: {url: " + hook + "}\n", "alerts.webhook must be an http or https URL"},
		"sequence":    {"alerts:\n  webhook: [" + hook + "]\n", "alerts.webhook must be an http or https URL"},
		"scheme":      {"alerts:\n  webhook: ftp://hooks.example.test/" + secret + "\n", "alerts.webhook must be an http or https URL"},
		"unparsable":  {"alerts:\n  webhook: https://hooks.example.test/%zz" + secret + "\n", "alerts.webhook must be an http or https URL"},
		"no host":     {"alerts:\n  webhook: https:///" + secret + "\n", "alerts.webhook must be an http or https URL with a host"},
		"empty":       {"alerts:\n  webhook: ''\n", "alerts.webhook is required"},
		"null":        {"alerts:\n  webhook:\n", "alerts.webhook is required"},
		"shared node": {"local-env: [&hook !!int " + hook + "]\nalerts:\n  webhook: *hook\n", "a value tagged !!int is not one"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(valid + tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), secret) {
				t.Fatalf("got %v, want %q without the credential", err, tc.want)
			}
		})
	}
}

// TestSharedWebhookIsNotEchoed shares a webhook node through an anchor
// with each duration field, and uses it as a key, in each bad form of the
// webhook. No error may carry the credential or a piece of it, escaped or
// truncated.
func TestSharedWebhookIsNotEchoed(t *testing.T) {
	const marker = "s3cr3t\"tok-01\\bk`9z8y7\nqwerty-\u00e9\u6f22\u5b57ab"
	q := strconv.Quote
	forms := map[string]string{
		"accepted":   q("https://hooks.example.test/" + marker),
		"bare":       q(marker),
		"int tag":    "!!int " + q("https://hooks.example.test/"+marker),
		"bool tag":   "!!bool " + q("https://hooks.example.test/"+marker),
		"mapping":    "{url: " + q("https://hooks.example.test/"+marker) + "}",
		"sequence":   "[" + q("https://hooks.example.test/"+marker) + "]",
		"scheme":     q("ftp://hooks.example.test/" + marker),
		"unparsable": q("https://hooks.example.test/%zz" + marker),
		"no host":    q("https:///" + marker),
	}
	fields := []string{"job-limits.default", "alerts.long-running"}
	for _, name := range slices.Sorted(maps.Keys(OperationLimits{}.ByName())) {
		fields = append(fields, "job-limits.operations."+name)
	}
	for _, name := range jobs.Phases {
		fields = append(fields, "job-limits.phases."+name)
	}
	nest := func(path, value string) string {
		var b strings.Builder
		parts := strings.Split(path, ".")
		for i, part := range parts {
			b.WriteString(strings.Repeat("  ", i) + part + ":")
			if i == len(parts)-1 {
				b.WriteString(" " + value)
			}
			b.WriteString("\n")
		}
		return b.String()
	}
	docs := map[string]string{}
	for form, value := range forms {
		for _, field := range fields {
			if field == "alerts.long-running" {
				docs[form+" webhook then "+field] = "alerts:\n  webhook: &hook " + value + "\n  long-running: *hook\n"
				docs[form+" "+field+" then webhook"] = "alerts:\n  long-running: &hook " + value + "\n  webhook: *hook\n"
				continue
			}
			docs[form+" webhook then "+field] = "alerts:\n  webhook: &hook " + value + "\n" + nest(field, "*hook")
			docs[form+" "+field+" then webhook"] = nest(field, "&hook "+value) + "alerts:\n  webhook: *hook\n"
		}
		for _, mapping := range []string{"job-limits.phases", "job-limits.operations", "job-limits", "alerts"} {
			docs[form+" key of "+mapping] = "alerts:\n  webhook: &hook " + value + "\n" +
				strings.TrimSuffix(nest(mapping, ""), " \n") + "\n" + strings.Repeat("  ", strings.Count(mapping, ".")+1) + "*hook : 1h\n"
			docs[form+" empty key of "+mapping] = strings.TrimSuffix(docs[form+" key of "+mapping], " 1h\n") + "\n"
			docs[form+" list under key of "+mapping] = strings.TrimSuffix(docs[form+" key of "+mapping], " 1h\n") + " [1h]\n"
		}
	}
	runes := []rune(marker)
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(valid + doc))
			if err == nil {
				t.Fatalf("accepted:\n%s", doc)
			}
			for i := 0; i+6 <= len(runes); i++ {
				if piece := string(runes[i : i+6]); strings.Contains(err.Error(), piece) {
					t.Fatalf("error carries %q of the credential: %v", piece, err)
				}
			}
		})
	}
}

// TestEmptyDurationsAreFoundThroughAliasesAndMerges gives an empty
// duration through an alias or a merge key, which the decoder resolves.
func TestEmptyDurationsAreFoundThroughAliasesAndMerges(t *testing.T) {
	// Both anchored fields fall back to their defaults when empty.
	const anchors = "listen: &empty ''\ntemplate-service:\n  socket: &null ~\n"
	cases := map[string]struct{ doc, want string }{
		"alias to empty default":   {"job-limits:\n  default: *empty\n", "job-limits.default must be a duration"},
		"alias to null default":    {"job-limits:\n  default: *null\n", "job-limits.default must be a duration"},
		"alias to empty phase":     {"job-limits:\n  phases:\n    build: *empty\n", "job-limits.phases.build must be a duration"},
		"alias to null operation":  {"job-limits:\n  operations:\n    rebuild: *null\n", "job-limits.operations.rebuild must be a duration"},
		"alias to empty threshold": {"alerts:\n  webhook: https://hooks.example.test\n  long-running: *empty\n", "alerts.long-running must be a duration"},
		"merged default":           {"job-limits:\n  <<: {default: ''}\n", "job-limits.default must be a duration"},
		"merged operation":         {"job-limits:\n  operations:\n    <<: {rebuild: ''}\n", "job-limits.operations.rebuild must be a duration"},
		"merged null phase":        {"job-limits:\n  phases:\n    <<: [{build: ~}]\n", "job-limits.phases.build must be a duration"},
		"merged threshold":         {"alerts:\n  <<: {long-running: ''}\n  webhook: https://hooks.example.test\n", "alerts.long-running must be a duration"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(valid, "workspace-root:", anchors+"workspace-root:", 1) + tc.doc
			_, err := Parse([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	cfg, err := Parse([]byte(valid + "job-limits:\n  <<: {default: ''}\n  default: 2h\n"))
	if err != nil || cfg.JobLimits.Default.Value() != 2*time.Hour {
		t.Fatalf("a given key overrides a merged empty one: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"unknown field", valid + "workspace-rooot: /x\n", "line 13: the document has no field workspace-rooot"},
		{"empty", "", "document is empty"},
		{"two documents", valid + "---\n" + valid, "single YAML document"},
		{"relative root", strings.Replace(valid, "/srv/workspaces/", "srv/workspaces", 1), "workspace-root must be an absolute path"},
		{"filesystem root", strings.Replace(valid, "/srv/workspaces/", "/", 1), "workspace-root must not be the filesystem root"},
		{"missing root", strings.Replace(valid, "workspace-root: /srv/workspaces/\n", "", 1), "workspace-root is required"},
		{"missing repository", strings.Replace(valid, "workspace-repository: git@example.test:example/workspaces.git\n", "", 1), "workspace-repository is required"},
		{"relative wyrwood", strings.Replace(valid, "/run/wyrwood/containers/example", "wyrwood", 1), "wyrwood-directory must be an absolute path"},
		{"relative socket", valid + "template-service:\n  socket: template.sock\n", "template-service.socket must be an absolute path"},
		{"missing issuer", strings.Replace(valid, "  issuer: https://id.example.test\n", "", 1), "oidc.issuer is required"},
		{"missing client", strings.Replace(valid, "  client-id: example-client\n", "", 1), "oidc.client-id is required"},
		{"issuer not a URL", strings.Replace(valid, "issuer: https://id.example.test", "issuer: id.example.test", 1), "oidc.issuer must be an http or https URL"},
		{"missing redirect", strings.Replace(valid, "  redirect-url: https://menagerie.example.test/auth/callback\n", "", 1), "oidc.redirect-url is required"},
		{"redirect path", strings.Replace(valid, "example.test/auth/callback", "example.test/callback", 1), "oidc.redirect-url must have the path /auth/callback"},
		{"redirect scheme", strings.Replace(valid, "redirect-url: https:", "redirect-url: ftp:", 1), "oidc.redirect-url must use http or https"},
		{"empty scopes", strings.Replace(valid, "templates:", "  scopes: []\ntemplates:", 1), "oidc.scopes must not be empty when given"},
		{"scopes without openid", strings.Replace(valid, "templates:", "  scopes: [profile, groups]\ntemplates:", 1), "oidc.scopes must include openid"},
		{"client secret", strings.Replace(valid, "templates:", "  client-secret: x\ntemplates:", 1), "line 9: oidc has no field client-secret"},
		{"bad template id", strings.Replace(valid, "id: sample-template", "id: Sample_Template", 1), "templates[0].id must be"},
		{"duplicate template", valid + "  - id: sample-template\n    name: Again\n    address: a\n", `templates[1].id "sample-template" is not unique`},
		{"template name", strings.Replace(valid, "    name: Sample template\n", "", 1), "templates[0].name is required"},
		{"template address", strings.Replace(valid, "    address: git@example.test:example/sample-template.git\n", "", 1), "templates[0].address is required"},
		{"missing kv mount", valid + strings.Replace(registryCredentials, "  kv-mount: kv\n", "", 1), "registry-credentials.kv-mount is required"},
		{"kv mount slashes", valid + strings.Replace(registryCredentials, "kv-mount: kv", "kv-mount: /kv/", 1), "registry-credentials.kv-mount \"/kv/\" must be"},
		{"duplicate hub alias", valid + strings.Replace(registryCredentials, "ghcr.io", "docker.io", 1), "registries[1].registry names docker.io, as registries[0] does"},
		{"registry with scheme", valid + strings.Replace(registryCredentials, "ghcr.io", "https://ghcr.io", 1), "registries[0].registry: registry \"https://ghcr.io\" is not a host"},
		{"registry with path", valid + strings.Replace(registryCredentials, "ghcr.io", "ghcr.io/team", 1), "registries[0].registry: registry \"ghcr.io/team\" is not a host"},
		{"dot path", valid + strings.Replace(registryCredentials, "menagerie/registries/ghcr", "menagerie/../ghcr", 1), "registries[0].path \"menagerie/../ghcr\" must be"},
		{"missing path", valid + strings.Replace(registryCredentials, "      path: menagerie/registries/ghcr\n", "", 1), "registries[0].path is required"},
		{"unknown registry field", valid + strings.Replace(registryCredentials, "      path: menagerie/registries/ghcr\n", "      path: menagerie/registries/ghcr\n      token: x\n", 1), "registry-credentials.registries[0] has no field token"},
		{"local-env name", valid + "local-env: [TZ, 1LANG]\n", "local-env[1] must be a variable name"},
		{"local-env with value", valid + "local-env: [TZ=UTC]\n", "local-env[0] must be a variable name"},
		{"duplicate local-env", valid + "local-env: [TZ, LANG, TZ]\n", "local-env[2] names TZ, as local-env[0] does"},
		{"limit units", valid + "job-limits:\n  default: 90ms\n", "job-limits.default must be whole hours, minutes and seconds"},
		{"limit fraction", valid + "job-limits:\n  default: 1.5h\n", "job-limits.default must be whole hours"},
		{"limit number", valid + "job-limits:\n  default: 45\n", "job-limits.default must be whole hours"},
		{"limit order", valid + "job-limits:\n  default: 5m1h\n", "job-limits.default must be whole hours"},
		{"zero limit", valid + "job-limits:\n  default: 0s\n", "job-limits.default must be a positive duration"},
		{"operation limit", valid + "job-limits:\n  operations:\n    stop: 1d\n", "job-limits.operations.stop must be whole hours"},
		{"unknown operation", valid + "job-limits:\n  operations:\n    deploy: 1h\n", "job-limits.operations has no field deploy"},
		{"unknown phase", valid + "job-limits:\n  phases:\n    hook: 1h\n", "job-limits.phases.hook is not a phase; phases are read-definition"},
		{"empty phase limit", valid + "job-limits:\n  phases:\n    build:\n", "job-limits.phases.build must be a duration"},
		{"empty-string phase limit", valid + "job-limits:\n  phases:\n    build: \"\"\n", "job-limits.phases.build must be a duration"},
		{"empty default", valid + "job-limits:\n  default: \"\"\n", "job-limits.default must be a duration"},
		{"null default", valid + "job-limits:\n  default: ~\n", "job-limits.default must be a duration"},
		{"empty operation limit", valid + "job-limits:\n  default: 2h\n  operations:\n    rebuild:\n", "job-limits.operations.rebuild must be a duration"},
		{"empty-string operation limit", valid + "job-limits:\n  operations:\n    rebuild: ''\n", "job-limits.operations.rebuild must be a duration"},
		{"empty long-running", valid + "alerts:\n  webhook: https://hooks.example.test\n  long-running: \"\"\n", "alerts.long-running must be a duration"},
		{"null long-running", valid + "alerts:\n  webhook: https://hooks.example.test\n  long-running:\n", "alerts.long-running must be a duration"},
		{"phase limit", valid + "job-limits:\n  phases:\n    build: 0m\n", "job-limits.phases.build must be a positive duration"},
		{"unknown limit field", valid + "job-limits:\n  maximum: 1h\n", "line 14: job-limits has no field maximum"},
		{"long-running without webhook", valid + "alerts:\n  long-running: 45m\n", "alerts.webhook is required"},
		{"long-running units", valid + "alerts:\n  webhook: https://hooks.example.test\n  long-running: 2d\n", "alerts.long-running must be whole hours"},
		{"webhook host", valid + "alerts:\n  webhook: https:///hook\n", "alerts.webhook must be an http or https URL with a host"},
		{"limit list", valid + "job-limits:\n  default: [2h]\n", "line 14: job-limits.default must be a duration such as 45m"},
		{"limits scalar", valid + "job-limits: 2h\n", "line 13: job-limits must be a mapping"},
		{"local-env mapping", valid + "local-env: {}\n", "line 13: local-env must be a list"},
		{"unknown tagged value", valid + "listen: !!int x\n", "a value tagged !!int is not one"},
		{"duplicate key", valid + "job-limits:\n  default: 1h\n  default: 2h\n", "line 15: a key is given again, first at line 14"},
		{"syntax", valid + "listen: : x\n", "line 13: mapping values are not allowed in this context"},
		{"flow syntax", valid + "local-env: [TZ\n", "did not find expected ',' or ']'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseCandidateFolder(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || cfg.TemplateService.CandidateFolder != DefaultCandidateFolder {
		t.Fatalf("absent: %+v %v", cfg.TemplateService, err)
	}
	cfg, err = Parse([]byte(valid + "template-service: {candidate-folder: /var/sample/candidates}\n"))
	if err != nil || cfg.TemplateService.CandidateFolder != "/var/sample/candidates" {
		t.Fatalf("set: %+v %v", cfg.TemplateService, err)
	}
	root := filepath.Clean(cfg.WorkspaceRoot)
	for folder, want := range map[string]string{
		"candidates":                "must be an absolute clean folder",
		"/var/sample/../candidates": "must be an absolute clean folder",
		"/":                         "must be an absolute clean folder",
		root + "/candidates":        "neither inside workspace-root nor contain it",
		root:                        "neither inside workspace-root nor contain it",
		filepath.Dir(root):          "neither inside workspace-root nor contain it",
	} {
		doc := valid + "template-service: {candidate-folder: '" + folder + "'}\n"
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", folder, err, want)
		}
	}
}

func TestParseRunnerImages(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || cfg.RunnerImage != "" || cfg.Templates[0].RunnerImage != "" || cfg.TemplateService.HookRunnerSocket != DefaultHookRunnerSocket {
		t.Fatalf("absent: %+v %v", cfg, err)
	}
	doc := valid + "    runner-image: ghcr.io/example/runner:1\nrunner-image: alpine:3.22\ntemplate-service: {hook-runner-socket: /run/sample/hooks.sock}\n"
	cfg, err = Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerImage != "alpine:3.22" || cfg.Templates[0].RunnerImage != "ghcr.io/example/runner:1" || cfg.TemplateService.HookRunnerSocket != "/run/sample/hooks.sock" {
		t.Fatalf("parsed %+v %+v", cfg.Templates[0], cfg.TemplateService)
	}
	for doc, want := range map[string]string{
		valid + "runner-image: 'Not An Image'\n":                                         "runner-image must be an image reference",
		valid + "    runner-image: 'bad image'\n":                                        "templates[0].runner-image must be an image reference",
		valid + "template-service: {hook-runner-socket: hooks.sock}\n":                   "template-service.hook-runner-socket must be an absolute path",
		valid + "template-service: {hook-runner-socket: /run/menagerie/template.sock}\n": "must differ from template-service.socket",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", doc, err, want)
		}
	}
}
