package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `
workspace-root: /srv/workspaces/
workspace-repository: git@example.test:example/workspaces.git
wyrwood-directory: /run/wyrwood/containers/example
oidc:
  issuer: https://id.example.test
  client-id: example-client
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
	if cfg.OIDC.Issuer != "https://id.example.test" || cfg.OIDC.ClientID != "example-client" {
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
		{"bad template id", strings.Replace(valid, "id: sample-template", "id: Sample_Template", 1), "templates[0].id must be"},
		{"duplicate template", valid + "  - id: sample-template\n    name: Again\n    address: a\n", `templates[1].id "sample-template" is not unique`},
		{"template name", strings.Replace(valid, "    name: Sample template\n", "", 1), "templates[0].name is required"},
		{"template address", strings.Replace(valid, "    address: git@example.test:example/sample-template.git\n", "", 1), "templates[0].address is required"},
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
