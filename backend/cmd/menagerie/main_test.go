package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/config"
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
