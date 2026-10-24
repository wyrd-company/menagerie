package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// TestRegistryCredentialsReadTheWyrwoodVaultSocket serves a synthetic
// OpenBao API at vault.sock in the configured Wyrwood directory and reads a
// configured registry's credentials through the composed provider.
func TestRegistryCredentialsReadTheWyrwoodVaultSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "wyrwood")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "vault.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var path string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"username": "sample-user", "password": "sample-password"}}})
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()

	cfg := &config.Config{WyrwoodDirectory: dir, RegistryCredentials: &config.RegistryCredentials{
		KVMount:    "kv",
		Registries: []config.Registry{{Registry: "ghcr.io", Path: "registries/ghcr"}},
	}}
	credentials, err := registryCredentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	auth := credentials(context.Background())
	creds, err := auth.Credentials("ghcr.io")
	if err != nil || creds.Username != "sample-user" || creds.Password != "sample-password" || path != "/v1/kv/data/registries/ghcr" {
		t.Fatalf("%+v %v path %s", creds, err, path)
	}
	if creds, err := auth.Credentials("registry.example.test"); err != nil || !creds.Anonymous() {
		t.Fatalf("unconfigured registry: %+v %v", creds, err)
	}

	none, err := registryCredentials(&config.Config{WyrwoodDirectory: dir})
	if err != nil || none != nil {
		t.Fatalf("absent section: %v", err)
	}
}

// TestBackupsUseTheWyrwoodAgentSocket checks that the composed backup
// remote authenticates SSH pushes through ssh-agent.sock in the configured
// Wyrwood directory.
func TestBackupsUseTheWyrwoodAgentSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "wyrwood")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "ssh-agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	dialed := make(chan struct{}, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			dialed <- struct{}{}
			c.Close()
		}
	}()
	remote := backupRemote(&config.Config{WyrwoodDirectory: dir, WorkspaceRepository: "git@example.invalid:sample/backups.git"})
	if remote.URL != "git@example.invalid:sample/backups.git" {
		t.Fatalf("url %s", remote.URL)
	}
	auth, release, err := remote.Auth(context.Background(), remote.URL)
	if err != nil || auth == nil {
		t.Fatalf("auth %v %v", auth, err)
	}
	release()
	<-dialed
}

// TestServerServesCreateAndUpgrade checks the composed server: the
// interview operations reach their request checks, and the answer route
// exists.
func TestServerServesCreateAndUpgrade(t *testing.T) {
	dir := t.TempDir()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		WorkspaceRoot:   dir,
		Templates:       []config.Template{{ID: "sample", Name: "Sample", Address: "file:///absent"}},
		TemplateService: config.TemplateService{Socket: filepath.Join(dir, "absent.sock")},
	}
	server := httptest.NewServer(newServer(cfg, root, devcontainer.New(nil, devcontainer.Options{}), jobs.NewManager(context.Background())))
	defer server.Close()
	for _, tc := range []struct{ path, body, kind string }{
		{"/api/jobs", `{"operation":"create","workspace":"a..b","templateId":"sample"}`, "invalid-workspace-name"},
		{"/api/jobs", `{"operation":"upgrade","workspace":"absent","templateId":"sample"}`, "workspace-not-found"},
		{"/api/jobs", `{"operation":"create","workspace":"fresh","templateId":"other"}`, "unknown-template"},
		{"/api/jobs/absent/answers", `{"session":{},"submission":{}}`, "job-not-found"},
	} {
		resp, err := http.Post(server.URL+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Error struct{ Kind string } }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if body.Error.Kind != tc.kind {
			t.Errorf("%s %s: kind %q, want %q", tc.path, tc.body, body.Error.Kind, tc.kind)
		}
	}
}
