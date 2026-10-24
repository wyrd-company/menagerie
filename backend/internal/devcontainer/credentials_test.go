package devcontainer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockerclient "github.com/docker/docker/client"
)

func dockerConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckNoCredentialHelpers(t *testing.T) {
	cases := []struct {
		name, body string
		helper     bool
		fails      bool
	}{
		{"no file", "", false, false},
		{"auths only", `{"auths": {"registry.example.test": {"auth": "c2FtcGxlOnNhbXBsZQ=="}}}`, false, false},
		{"creds store", `{"credsStore": "sample"}`, true, true},
		{"per-host helper", `{"credHelpers": {"registry.example.test": "sample"}}`, true, true},
		{"malformed", `{"credsStore": `, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_CONFIG", dockerConfig(t, tc.body))
			err := CheckNoCredentialHelpers()
			var helperErr *CredentialHelperError
			if errors.As(err, &helperErr) != tc.helper || (err != nil) != tc.fails {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestCheckNoCredentialHelpersUsesHomeWithoutDockerConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"), []byte(`{"credsStore": "sample"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("HOME", home)
	var helperErr *CredentialHelperError
	if err := CheckNoCredentialHelpers(); !errors.As(err, &helperErr) || helperErr.ConfigFile != filepath.Join(home, ".docker", "config.json") {
		t.Fatalf("got %v", err)
	}
}

// TestStartRefusesCredentialHelperBeforeAnyCommand puts a helper named by
// the configuration on PATH. Start must refuse before initializeCommand or
// any Docker call, and the helper must not run.
func TestStartRefusesCredentialHelperBeforeAnyCommand(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "helper-ran")
	script := "#!/bin/sh\ntouch " + marker + "\necho '{}'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker-credential-menagerie-probe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_CONFIG", dockerConfig(t, `{"credsStore": "menagerie-probe"}`))
	_, folder := writeDefinition(t, `{"image": "alpine:3.22", "initializeCommand": "touch init-ran"}`)
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+filepath.Join(t.TempDir(), "absent.sock")), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	rt := New(client, Options{})
	for _, op := range []func(context.Context, string, Progress) error{rt.Start, rt.Rebuild} {
		err := op(context.Background(), folder, &progress{})
		var helperErr *CredentialHelperError
		if !errors.As(err, &helperErr) || !strings.Contains(err.Error(), "does not run credential helpers") {
			t.Fatalf("got %v", err)
		}
	}
	for _, path := range []string{filepath.Join(folder, "init-ran"), marker} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s exists", path)
		}
	}
}
