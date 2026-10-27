package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/commits"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
	"github.com/wyrd-company/menagerie/backend/internal/workspacesocket"
)

// TestWorkspaceSocketsReachTheRuntime checks that a configured socket
// folder gives every existing workspace its socket at start-up and the
// runtime a mount for the root's folders only.
func TestWorkspaceSocketsReachTheRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := os.MkdirTemp("", "ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(folder) })
	client := filepath.Join(t.TempDir(), workspacesocket.ClientName)
	if err := os.WriteFile(client, []byte("sample client"), 0o755); err != nil {
		t.Fatal(err)
	}
	starter := commits.New(commits.Options{Root: root, Jobs: jobs.NewManager(context.Background(), jobs.Options{})})

	var none devcontainer.Options
	if s, err := workspaceSockets(context.Background(), &config.Config{}, root, starter, client, &none); s != nil || err != nil || none.WorkspaceSocket != nil {
		t.Fatalf("without workspace-socket: %v %v", s, err)
	}

	var opts devcontainer.Options
	cfg := &config.Config{WorkspaceSocket: &config.WorkspaceSocket{Folder: folder}}
	sockets, err := workspaceSockets(context.Background(), cfg, root, starter, client, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer sockets.Close()
	if info, err := os.Lstat(filepath.Join(folder, "alpha", devcontainer.WorkspaceSocketName)); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("alpha has no socket at start-up: %v", err)
	}
	if got, err := opts.WorkspaceSocket(filepath.Join(dir, "beta")); err != nil || got != filepath.Join(folder, "beta") {
		t.Fatalf("mount for beta %q %v", got, err)
	}
	if _, err := opts.WorkspaceSocket(filepath.Join(t.TempDir(), "alpha")); err == nil {
		t.Fatal("a folder outside the root got a socket")
	}
	if _, err := workspaceSockets(context.Background(), cfg, root, starter, filepath.Join(dir, "missing"), &opts); err == nil {
		t.Fatal("a missing client was accepted")
	}
}

func TestServerServesCommit(t *testing.T) {
	dir := t.TempDir()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{WorkspaceRoot: dir, TemplateService: config.TemplateService{Socket: filepath.Join(dir, "absent.sock")}}
	server := httptest.NewServer(newServer(cfg, root, backupRemote(cfg, nil), devcontainer.New(nil, devcontainer.Options{}), jobs.NewManager(context.Background(), jobs.Options{}), nil, nil))
	defer server.Close()
	resp, err := http.Post(server.URL+"/api/jobs", "application/json", strings.NewReader(`{"operation":"commit","workspace":"absent"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Error struct{ Kind string } }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusNotFound || body.Error.Kind != "workspace-not-found" {
		t.Fatalf("status %d kind %q", resp.StatusCode, body.Error.Kind)
	}
}

// TestWorkspaceSocketStartUpErrorsNameNoPath gives the configuration and
// the runtime paths holding a sentinel; no diagnostic may hold it.
func TestWorkspaceSocketStartUpErrorsNameNoPath(t *testing.T) {
	const sentinel = "s3ntinel-private-path"
	dir := filepath.Join(t.TempDir(), sentinel)
	if err := os.MkdirAll(filepath.Join(dir, "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(dir, workspacesocket.ClientName)
	if err := os.WriteFile(client, []byte("sample client"), 0o755); err != nil {
		t.Fatal(err)
	}
	starter := commits.New(commits.Options{Root: root, Jobs: jobs.NewManager(context.Background(), jobs.Options{})})
	clean := func(label string, err error) {
		t.Helper()
		if err == nil || strings.Contains(err.Error(), sentinel) {
			t.Errorf("%s: %v", label, err)
		}
	}
	var opts devcontainer.Options
	_, err = workspaceSockets(context.Background(), &config.Config{WorkspaceSocket: &config.WorkspaceSocket{Folder: filepath.Join(dir, "missing")}}, root, starter, client, &opts)
	clean("missing folder", err)
	// Socket paths are short: a Unix socket address has about 100 bytes.
	folder, err := os.MkdirTemp("", sentinel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(folder) })
	if err := os.Mkdir(filepath.Join(dir, "root", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = workspaceSockets(context.Background(), &config.Config{WorkspaceSocket: &config.WorkspaceSocket{Folder: folder}}, root, starter, filepath.Join(dir, "missing-client"), &opts)
	clean("missing client", err)
	var logged strings.Builder
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	sockets, err := workspaceSockets(context.Background(), &config.Config{WorkspaceSocket: &config.WorkspaceSocket{Folder: folder}}, root, starter, client, &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer sockets.Close()
	if !strings.Contains(logged.String(), "workspace sockets are open") || strings.Contains(logged.String(), sentinel) {
		t.Errorf("start-up logged %q", logged.String())
	}
	_, err = opts.WorkspaceSocket(filepath.Join(dir, "elsewhere", "alpha"))
	clean("folder outside the root", err)
}
