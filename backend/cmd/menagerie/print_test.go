package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const printConfig = `workspace-root: /srv/sample/workspaces/
workspace-repository: git@example.test:example/workspaces.git
wyrwood-directory: /run/sample/./wyrwood
oidc:
  issuer: https://id.example.test
  client-id: sample-client
  redirect-url: https://menagerie.example.test/auth/callback
templates: []
`

func TestPrintPathsWritesTheCleanedConfiguredPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(printConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		root, wyrwood bool
		want          string
	}{
		{true, false, "/srv/sample/workspaces\n"},
		{false, true, "/run/sample/wyrwood\n"},
		{true, true, "/srv/sample/workspaces\n/run/sample/wyrwood\n"},
	} {
		var out bytes.Buffer
		if err := printPaths(&out, path, c.root, c.wyrwood); err != nil {
			t.Fatal(err)
		}
		if out.String() != c.want {
			t.Errorf("root %v wyrwood %v printed %q, want %q", c.root, c.wyrwood, out.String(), c.want)
		}
	}
}

func TestPrintPathsRefusesAnInvalidConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	invalid := strings.Replace(printConfig, "workspace-root: /srv/sample/workspaces/", "workspace-root: relative", 1)
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printPaths(&out, path, true, true); err == nil || out.Len() != 0 {
		t.Fatalf("printed %q with error %v", out.String(), err)
	}
}
