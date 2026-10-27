package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"

	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
	"github.com/wyrd-company/menagerie/web"
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

// TestRuntimeOptionsCarryTheLocalEnvAllowlist checks that the configured
// local-env list reaches the devcontainer runtime, and that none is none.
func TestRuntimeOptionsCarryTheLocalEnvAllowlist(t *testing.T) {
	opts, err := runtimeOptions(&config.Config{LocalEnv: []string{"TZ", "LANG"}})
	if err != nil || strings.Join(opts.LocalEnv, ",") != "TZ,LANG" || opts.Environ != nil {
		t.Fatalf("options %+v %v", opts, err)
	}
	if opts, err := runtimeOptions(&config.Config{}); err != nil || opts.LocalEnv != nil {
		t.Fatalf("absent local-env %+v %v", opts, err)
	}
}

// TestRuntimeOptionsNameWorkspacesAndStageCompose checks that every
// devcontainer operation gets a client of its own, for the Docker proxy's
// workspace header, and that compose-staging-folder reaches the runtime.
func TestRuntimeOptionsNameWorkspacesAndStageCompose(t *testing.T) {
	opts, err := runtimeOptions(&config.Config{ComposeStagingFolder: "/srv/compose-staging"})
	if err != nil || opts.Client == nil || opts.ComposeStaging != "/srv/compose-staging" {
		t.Fatalf("options %+v %v", opts, err)
	}
}

// TestThrowawayContainersNameTheirWorkspaces checks that each throwaway
// command gets a client of its own, for the Docker proxy's header.
func TestThrowawayContainersNameTheirWorkspaces(t *testing.T) {
	if o := throwawayOptions("/srv/workspaces", &devcontainer.Options{}); o.Client == nil || o.Root != "/srv/workspaces" {
		t.Fatalf("options %+v", o)
	}
}

type fakeInfo struct {
	info system.Info
	err  error
}

func (f fakeInfo) Info(context.Context) (system.Info, error) { return f.info, f.err }

// TestASwarmManagerHostIsWarnedAbout logs one warning, and only for a
// Swarm manager.
func TestASwarmManagerHostIsWarnedAbout(t *testing.T) {
	manager := system.Info{Swarm: swarm.Info{ControlAvailable: true, LocalNodeState: swarm.LocalNodeStateActive}}
	worker := system.Info{Swarm: swarm.Info{ControlAvailable: false, LocalNodeState: swarm.LocalNodeStateActive}}
	for name, tc := range map[string]struct {
		engine fakeInfo
		want   []string
	}{
		"manager":     {fakeInfo{info: manager}, []string{swarmManagerWarning}},
		"worker":      {fakeInfo{info: worker}, nil},
		"no swarm":    {fakeInfo{}, nil},
		"info failed": {fakeInfo{info: manager, err: errors.New("unreachable")}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			var logged []string
			warnSwarmManager(context.Background(), tc.engine, func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
			if !slices.Equal(logged, tc.want) {
				t.Errorf("logged %q, want %q", logged, tc.want)
			}
		})
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
	remote := backupRemote(&config.Config{WyrwoodDirectory: dir, WorkspaceRepository: "git@example.invalid:sample/backups.git"}, nil)
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
	server := httptest.NewServer(newServer(cfg, root, backupRemote(cfg, nil), devcontainer.New(nil, devcontainer.Options{}), jobs.NewManager(context.Background(), jobs.Options{}), nil, nil))
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

// TestServerTakesInterviewAnswers checks that the composed server hands an
// answer to a waiting create job's interview: a template service on the
// configured socket keeps asking, and each answer gets its reply.
func TestServerTakesInterviewAnswers(t *testing.T) {
	dir := t.TempDir()
	sockDir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "template.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	const asking = `{"session":{"step":1},"state":"questions","document":{"status":"questions"},"warnings":[]}`
	templateService := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(asking))
	}))
	templateService.Listener = l
	templateService.Start()
	defer templateService.Close()

	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := jobs.NewManager(ctx, jobs.Options{})
	defer manager.Wait()
	defer cancel()
	cfg := &config.Config{
		WorkspaceRoot:   dir,
		Templates:       []config.Template{{ID: "sample", Name: "Sample", Address: "file:///absent"}},
		TemplateService: config.TemplateService{Socket: socket},
	}
	server := httptest.NewServer(newServer(cfg, root, backupRemote(cfg, nil), devcontainer.New(nil, devcontainer.Options{}), manager, nil, nil))
	defer server.Close()
	resp, err := http.Post(server.URL+"/api/jobs", "application/json", strings.NewReader(`{"operation":"create","workspace":"alpha","templateId":"sample"}`))
	if err != nil {
		t.Fatal(err)
	}
	var snap jobs.Snapshot
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	job, ok := manager.Get(snap.JobID)
	if !ok {
		t.Fatalf("no job: %+v", snap)
	}
	for {
		_, ended, changed := job.EventsAfter(0)
		if job.Snapshot().State == jobs.WaitingForAnswers {
			break
		}
		if ended {
			t.Fatalf("job ended: %+v", job.Snapshot())
		}
		<-changed
	}
	resp, err = http.Post(server.URL+"/api/jobs/"+job.ID()+"/answers", "application/json", strings.NewReader(`{"session":{"step":1},"submission":{"template":"t","answers":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != asking {
		t.Fatalf("answer %d %s", resp.StatusCode, body)
	}
}

// TestServerServesTheEmbeddedUI checks that the composed server serves
// web.FS: its index for each page route and its assets.
func TestServerServesTheEmbeddedUI(t *testing.T) {
	saved := web.FS
	t.Cleanup(func() { web.FS = saved })
	web.FS = fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>ui-index</title>")},
		"assets/app.js": {Data: []byte("ui-asset")},
	}
	dir := t.TempDir()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{WorkspaceRoot: dir, TemplateService: config.TemplateService{Socket: filepath.Join(dir, "absent.sock")}}
	server := httptest.NewServer(newServer(cfg, root, backupRemote(cfg, nil), devcontainer.New(nil, devcontainer.Options{}), jobs.NewManager(context.Background(), jobs.Options{}), nil, nil))
	defer server.Close()
	for path, want := range map[string]string{
		"/":                         "ui-index",
		"/create":                   "ui-index",
		"/workspaces/alpha/upgrade": "ui-index",
		"/assets/app.js":            "ui-asset",
	} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s answered %d %q, want %q", path, resp.StatusCode, body, want)
		}
	}
}
