package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// configuration error.
func TestInvalidWebhookIsNotEchoed(t *testing.T) {
	for _, hook := range []string{"ftp://hooks.example.test/tok-0123456789", "https://hooks.example.test/%zztok-0123456789"} {
		_, err := Parse([]byte(valid + "alerts:\n  webhook: " + hook + "\n"))
		if err == nil || !strings.Contains(err.Error(), "alerts.webhook must be an http or https URL") || strings.Contains(err.Error(), "tok-0123456789") {
			t.Fatalf("%s: %v", hook, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"unknown field", valid + "workspace-rooot: /x\n", "field workspace-rooot not found"},
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
		{"client secret", strings.Replace(valid, "templates:", "  client-secret: x\ntemplates:", 1), "field client-secret not found"},
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
		{"unknown registry field", valid + strings.Replace(registryCredentials, "      path: menagerie/registries/ghcr\n", "      path: menagerie/registries/ghcr\n      token: x\n", 1), "field token not found"},
		{"local-env name", valid + "local-env: [TZ, 1LANG]\n", "local-env[1] must be a variable name"},
		{"local-env with value", valid + "local-env: [TZ=UTC]\n", "local-env[0] must be a variable name"},
		{"duplicate local-env", valid + "local-env: [TZ, LANG, TZ]\n", "local-env[2] names TZ, as local-env[0] does"},
		{"limit units", valid + "job-limits:\n  default: 90ms\n", `job-limits.default "90ms" must be whole hours, minutes and seconds`},
		{"limit fraction", valid + "job-limits:\n  default: 1.5h\n", `job-limits.default "1.5h" must be whole hours`},
		{"limit number", valid + "job-limits:\n  default: 45\n", `job-limits.default "45" must be whole hours`},
		{"limit order", valid + "job-limits:\n  default: 5m1h\n", `job-limits.default "5m1h" must be whole hours`},
		{"zero limit", valid + "job-limits:\n  default: 0s\n", `job-limits.default "0s" must be positive`},
		{"operation limit", valid + "job-limits:\n  operations:\n    stop: 1d\n", `job-limits.operations.stop "1d" must be whole hours`},
		{"unknown operation", valid + "job-limits:\n  operations:\n    deploy: 1h\n", "field deploy not found"},
		{"unknown phase", valid + "job-limits:\n  phases:\n    hook: 1h\n", "job-limits.phases.hook is not a phase; phases are read-definition"},
		{"empty phase limit", valid + "job-limits:\n  phases:\n    build:\n", "job-limits.phases.build must be a duration"},
		{"phase limit", valid + "job-limits:\n  phases:\n    build: 0m\n", `job-limits.phases.build "0m" must be positive`},
		{"unknown limit field", valid + "job-limits:\n  maximum: 1h\n", "field maximum not found"},
		{"long-running without webhook", valid + "alerts:\n  long-running: 45m\n", "alerts.webhook is required"},
		{"long-running units", valid + "alerts:\n  webhook: https://hooks.example.test\n  long-running: 2d\n", `alerts.long-running "2d" must be whole hours`},
		{"webhook host", valid + "alerts:\n  webhook: https:///hook\n", "alerts.webhook must be an http or https URL with a host"},
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
