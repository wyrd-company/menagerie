//go:build pocketid && browser

package main

// TestBrowserFixture holds a real Menagerie open for the web UI's integrated
// browser run (web/test/integrated). It is test-only: everything it starts
// is disposable and every key is generated here; no credential is read.
//
//   - the workspace service composed by newHandler from a configuration file
//     loaded by config.Load, serving the built UI from web.FS
//   - the real template service binary (MENAGERIE_TEMPLATE_SERVICE) on its
//     Unix socket, with a two-version generic template in local repositories
//   - an isolated SSH Git host and agent (backuptest) holding the workspace
//     repository
//   - a disposable Pocket ID with a manager and a non-manager, and a public
//     client whose redirect URL is the service's browser-facing origin
//   - workspace devcontainers on the Docker Engine named by
//     MENAGERIE_BROWSER_DOCKER_HOST, which must see MENAGERIE_BROWSER_ROOT at
//     the same path
//
// A separate control server on MENAGERIE_BROWSER_CONTROL_PORT lets the
// browser run read the fixture: sign-in links, the backup branch, workspace
// files and containers. It ends when the control server receives
// POST /shutdown.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
	"github.com/wyrd-company/menagerie/web"
)

// runTemplateService runs the template service binary until the test ends.
func runTemplateService(t *testing.T, bin, cfg, data, socket string) {
	t.Helper()
	cmd := exec.Command(bin, "-config", cfg, "-data", data)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			t.Logf("template service: %s", s.Text())
		}
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-exited
	})
	for {
		if c, err := net.Dial("unix", socket); err == nil {
			_ = c.Close()
			return
		}
		select {
		case <-exited:
			t.Fatal("the template service exited before listening")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestBrowserFixture(t *testing.T) {
	// The integrated run proves the real UI; a placeholder-only build is refused.
	if _, err := fs.Stat(web.FS, "index.html"); err != nil {
		t.Fatalf("web.FS has no index.html: build the UI (pnpm --dir web build) before this test: %v", err)
	}
	bin := requireEnv(t, "MENAGERIE_TEMPLATE_SERVICE")
	base := requireEnv(t, "MENAGERIE_BROWSER_ROOT")
	dockerHost := requireEnv(t, "MENAGERIE_BROWSER_DOCKER_HOST")
	info := requireEnv(t, "MENAGERIE_BROWSER_INFO")
	port := requireEnv(t, "MENAGERIE_BROWSER_PORT")
	controlPort := requireEnv(t, "MENAGERIE_BROWSER_CONTROL_PORT")

	p := startPocketID(t)
	site := "http://localhost:" + port

	// Templates, the workspace root and the Git host.
	rootDir := filepath.Join(base, "workspaces")
	repo := filepath.Join(base, "templates", "sample-kit")
	for _, d := range []string{rootDir, repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	browserTemplate(t, repo, 1)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v1")
	gitRun(t, repo, "tag", "v1")
	browserTemplate(t, repo, 2)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v2")
	gitRun(t, repo, "tag", "v2")

	host := backuptest.New(t)
	bare := filepath.Join(host.Dir, "backups.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_KNOWN_HOSTS", host.KnownHosts)

	// Pocket ID: managers, a non-manager and the public client.
	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	other := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Other", "name": "other"})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	p.user("sample-other", "Sample", "Other", other["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})

	// The configuration file, read by both services.
	sockDir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "template.sock")
	cfgPath := filepath.Join(base, "config.yml")
	writeTo(t, base, "config.yml", fmt.Sprintf(`listen: 127.0.0.1:%s
workspace-root: %s
workspace-repository: %s
wyrwood-directory: %s
oidc:
  issuer: %s
  client-id: %s
  redirect-url: %s
template-service: {socket: %s}
templates:
  - {id: sample-kit-v1, name: "Sample kit, version 1", address: 'file://%s@v1'}
  - {id: sample-kit-v2, name: "Sample kit, version 2", address: 'file://%s@v2'}
`, port, rootDir, host.URL("backups.git"), filepath.Dir(host.AgentPath), p.url, client["id"], site+auth.CallbackPath, socket, repo, repo))
	runTemplateService(t, bin, cfgPath, filepath.Join(base, "template-data"), socket)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(cfg.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	docker, err := dockerclient.NewClientWithOpts(dockerclient.WithHost(dockerHost), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	manager := jobs.NewManager(ctx)
	handler, err := newHandler(cfg, root, devcontainer.New(docker, devcontainer.Options{}), manager)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: handler, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { cancel(); _ = srv.Shutdown(context.Background()); manager.Wait() })

	done := make(chan struct{})
	control := controlServer(t, controlFixture{p: p, rootDir: rootDir, bare: bare, docker: docker, done: done})
	csrv := &http.Server{Addr: "127.0.0.1:" + controlPort, Handler: control}
	go func() { _ = csrv.ListenAndServe() }()
	t.Cleanup(func() { _ = csrv.Shutdown(context.Background()) })

	data, _ := json.Marshal(map[string]string{
		"site":    site,
		"pocket":  p.url,
		"control": "http://127.0.0.1:" + controlPort,
		"root":    rootDir,
	})
	if err := os.WriteFile(info, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture ready: %s", data)
	<-done
}

type controlFixture struct {
	p       *pocketID
	rootDir string
	bare    string
	docker  *dockerclient.Client
	done    chan struct{}
}

func controlServer(t *testing.T, f controlFixture) http.Handler {
	mux := http.NewServeMux()
	// Which run this is: run.sh down shuts down only the run whose root it saved.
	mux.HandleFunc("GET /identity", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"root": f.rootDir})
	})
	// A fresh one-time sign-in link for a user.
	mux.HandleFunc("GET /signin-link", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"url": f.p.url + "/lc/" + f.p.signInLink(r.URL.Query().Get("user"))})
	})
	// The backup branch: its commits, newest first, and the head's files.
	mux.HandleFunc("GET /backup", func(w http.ResponseWriter, r *http.Request) {
		repo, err := git.PlainOpen(f.bare)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		ref, err := repo.Reference(plumbing.NewBranchReferenceName(r.URL.Query().Get("branch")), true)
		if err != nil {
			writeJSON(w, map[string]any{"exists": false})
			return
		}
		commits := []map[string]any{}
		iter, _ := repo.Log(&git.LogOptions{From: ref.Hash()})
		_ = iter.ForEach(func(c *object.Commit) error {
			commits = append(commits, map[string]any{"hash": c.Hash.String(), "message": strings.TrimSpace(c.Message), "parents": c.NumParents()})
			return nil
		})
		files := map[string]string{}
		head, _ := repo.CommitObject(ref.Hash())
		tree, _ := head.Tree()
		_ = tree.Files().ForEach(func(file *object.File) error {
			body, _ := file.Contents()
			files[file.Name] = body
			return nil
		})
		writeJSON(w, map[string]any{"exists": true, "commits": commits, "files": files})
	})
	// A workspace file as the operator sees it; PUT replaces it (an operator edit).
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		rel := filepath.Clean(r.URL.Query().Get("path"))
		dir := filepath.Clean(r.URL.Query().Get("workspace"))
		if strings.Contains(rel, "..") || strings.Contains(dir, "/") || strings.Contains(dir, "..") {
			http.Error(w, "bad path", 400)
			return
		}
		target := filepath.Join(f.rootDir, dir, rel)
		if r.Method == http.MethodPut {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				err = os.WriteFile(target, body, 0o644)
			}
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		body, err := os.ReadFile(target)
		writeJSON(w, map[string]any{"exists": err == nil, "body": string(body)})
	})
	// Whether a folder exists directly under the workspace root.
	mux.HandleFunc("GET /folder", func(w http.ResponseWriter, r *http.Request) {
		info, err := os.Stat(filepath.Join(f.rootDir, filepath.Base(r.URL.Query().Get("name"))))
		writeJSON(w, map[string]any{"exists": err == nil && info.IsDir()})
	})
	// A folder directly under the workspace root, with no container.
	mux.HandleFunc("POST /folder", func(w http.ResponseWriter, r *http.Request) {
		err := os.Mkdir(filepath.Join(f.rootDir, filepath.Base(r.URL.Query().Get("name"))), 0o755)
		writeJSON(w, map[string]any{"ok": err == nil || errors.Is(err, os.ErrExist)})
	})
	// An unrelated container whose label points outside the workspace root.
	// It fails unless the container runs, so a check of its absence from the
	// list cannot pass because it was never there.
	mux.HandleFunc("POST /outsider", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		const outsiderImage = "busybox:1.37"
		var err error
		if _, inspectErr := f.docker.ImageInspect(ctx, outsiderImage); inspectErr != nil {
			var rc io.ReadCloser
			if rc, err = f.docker.ImagePull(ctx, outsiderImage, image.PullOptions{}); err == nil {
				_, _ = io.Copy(io.Discard, rc)
				rc.Close()
			}
		}
		var c container.CreateResponse
		if err == nil {
			c, err = f.docker.ContainerCreate(ctx, &container.Config{Image: outsiderImage, Cmd: []string{"sleep", "3600"},
				Labels: map[string]string{"devcontainer.local_folder": "/elsewhere/sample-outsider", "menagerie-test": "outsider"}}, nil, nil, nil, "sample-outsider")
		}
		if err == nil {
			err = f.docker.ContainerStart(ctx, c.ID, container.StartOptions{})
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": c.ID})
	})
	// Containers on the workspace Docker Engine with the discovery label.
	mux.HandleFunc("GET /containers", func(w http.ResponseWriter, r *http.Request) {
		list, err := f.docker.ContainerList(r.Context(), container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "devcontainer.local_folder"))})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := []map[string]string{}
		for _, c := range list {
			out = append(out, map[string]string{"id": c.ID, "state": c.State, "folder": c.Labels["devcontainer.local_folder"]})
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		close(f.done)
	})
	return mux
}
