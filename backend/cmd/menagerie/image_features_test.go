//go:build pocketid && image

package main

// TestImageWorkspaceFeatures runs the built image, MENAGERIE_IMAGE, and takes
// a workspace whose devcontainer uses a published feature through create,
// stop, start and rebuild jobs. The Go feature's containerEnv extends PATH
// with ${PATH}, so the feature installs and the workspace runs only when
// that reference is expanded: after each job the running devcontainer
// finds the feature's go and the base image's chmod on its PATH.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

const (
	// goFeature is the published Go feature; its containerEnv sets
	// PATH=/usr/local/go/bin:/go/bin:${PATH}.
	goFeature  = "ghcr.io/devcontainers/features/go:1"
	goVersion  = "1.27.3"
	goPathPart = "/usr/local/go/bin"
)

// featureTemplate writes version 1 of the sample template with a
// devcontainer that installs the Go feature on the MCR Debian base.
func featureTemplate(t *testing.T, dir string) {
	t.Helper()
	browserTemplate(t, dir, 1)
	writeTo(t, dir, "template/.devcontainer/devcontainer.json", fmt.Sprintf(`{
  "name": "sample",
  "image": "%s",
  "features": { "%s": { "version": "%s" } },
  "initializeCommand": "mkdir -p markers && echo initialized > markers/initialize.txt"
}
`, devcontainerBase, goFeature, goVersion))
}

// runningDevcontainer returns the folder's one running devcontainer.
func runningDevcontainer(t *testing.T, p *pocketID, folder string) container.Summary {
	t.Helper()
	list, err := p.docker.ContainerList(context.Background(), container.ListOptions{Filters: labelFilter(folder)})
	if err != nil || len(list) != 1 {
		t.Fatalf("running devcontainers for %s: %d, %v", folder, len(list), err)
	}
	return list[0]
}

// checkGoOnPath runs the feature's tool and a base tool in the running
// devcontainer, with the environment the container was created with, and
// checks the image's configured PATH.
func checkGoOnPath(t *testing.T, p *pocketID, folder string) string {
	t.Helper()
	devc := runningDevcontainer(t, p, folder)
	c := &imageContainer{t: t, docker: p.docker, id: devc.ID}
	out, code := c.exec("", "sh", "-c", `go version && command -v chmod && printf 'PATH=%s\n' "$PATH"`)
	if code != 0 {
		t.Fatalf("go in the devcontainer exited %d: %q", code, out)
	}
	if !strings.Contains(out, "go version go"+goVersion+" ") {
		t.Errorf("go version: %q", out)
	}
	path := out[strings.LastIndex(out, "PATH=")+len("PATH="):]
	for _, part := range []string{goPathPart, "/usr/bin", "/bin"} {
		if !strings.Contains(":"+path+":", ":"+part+":") {
			t.Errorf("the devcontainer's PATH %q lacks %s", path, part)
		}
	}
	if strings.Contains(path, "${") || strings.Contains(path, "$PATH") {
		t.Errorf("the devcontainer's PATH holds an unexpanded reference: %q", path)
	}
	img, err := p.docker.ImageInspect(context.Background(), devc.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	configured := ""
	for _, e := range img.Config.Env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			configured = v
		}
	}
	if !strings.Contains(":"+configured+":", ":"+goPathPart+":") || !strings.Contains(":"+configured+":", ":/bin:") || strings.Contains(configured, "${") {
		t.Errorf("the image's configured PATH is %q", configured)
	}
	return devc.ID
}

func TestImageWorkspaceFeatures(t *testing.T) {
	ref := requireEnv(t, "MENAGERIE_IMAGE")
	p := startPocketID(t)
	ctx := context.Background()
	tmp := os.TempDir()

	base := t.TempDir()
	rootDir := filepath.Join(base, "workspaces")
	repo := filepath.Join(base, "templates", "sample-kit")
	for _, d := range []string{rootDir, repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	featureTemplate(t, repo)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v1")
	gitRun(t, repo, "tag", "v1")

	host := backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(host.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	site := fmt.Sprintf("http://localhost:%d", port)
	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})
	writeTo(t, base, "config.yml", fmt.Sprintf(`listen: 127.0.0.1:%d
workspace-root: %s
workspace-repository: %s
wyrwood-directory: %s
oidc:
  issuer: %s
  client-id: %s
  redirect-url: %s
templates:
  - {id: sample-kit-go, name: "Sample kit with Go", address: 'file://%s@v1'}
`, port, rootDir, host.URL("backups.git"), filepath.Dir(host.AgentPath), p.url, client["id"], site+auth.CallbackPath, repo))

	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: ref,
			Env: []string{
				"SSH_AUTH_SOCK=" + host.AgentPath,
				"SSH_KNOWN_HOSTS=" + host.KnownHosts,
				fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port),
			},
			Labels: map[string]string{"menagerie-test": "image"},
		},
		&container.HostConfig{
			NetworkMode: "host",
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				tmp + ":" + tmp,
				filepath.Join(base, "config.yml") + ":/etc/menagerie/config.yml:ro",
			},
		}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c := &imageContainer{t: t, docker: p.docker, id: created.ID, site: site}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), c.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	folder := filepath.Join(rootDir, "alpha")
	t.Cleanup(func() { removeDevcontainers(p, folder) })
	if err := p.docker.ContainerStart(ctx, c.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	c.waitReady(0)
	if t.Failed() {
		return
	}
	b := signInBrowser()
	if s, _ := p.signIn(t, b, "sample-manager", site+"/"); s.status != http.StatusOK {
		t.Fatalf("sign-in answered %d", s.status)
	}

	var first string
	t.Run("create installs the feature and the workspace runs with its tool", func(t *testing.T) {
		c.t = t
		snap := runInterviewJob(t, b, site, "create", "alpha", "sample-kit-go",
			map[string]any{"label": "Sample label", "size": "small", "verbose": false, "tags": []string{"alpha"}})
		if snap.State != jobs.Succeeded {
			t.Fatalf("create ended %s: %+v\n%s", snap.State, snap.Error, c.logs(time.Time{}))
		}
		first = checkGoOnPath(t, p, folder)
	})
	if t.Failed() {
		return
	}

	t.Run("stop and start keep the feature's tool", func(t *testing.T) {
		c.t = t
		if snap := runJob(t, b, site, "stop", "alpha"); snap.State != jobs.Succeeded {
			t.Fatalf("stop ended %s: %+v", snap.State, snap.Error)
		}
		list, err := p.docker.ContainerList(ctx, container.ListOptions{Filters: labelFilter(folder)})
		if err != nil || len(list) != 0 {
			t.Fatalf("running devcontainers after stop: %d, %v", len(list), err)
		}
		if snap := runJob(t, b, site, "start", "alpha"); snap.State != jobs.Succeeded {
			t.Fatalf("start ended %s: %+v", snap.State, snap.Error)
		}
		if id := checkGoOnPath(t, p, folder); id != first {
			t.Errorf("start made container %s, not the stopped %s", id, first)
		}
	})

	t.Run("rebuild builds the feature again", func(t *testing.T) {
		c.t = t
		if snap := runJob(t, b, site, "rebuild", "alpha"); snap.State != jobs.Succeeded {
			t.Fatalf("rebuild ended %s: %+v\n%s", snap.State, snap.Error, c.logs(time.Time{}))
		}
		if id := checkGoOnPath(t, p, folder); id == first {
			t.Errorf("rebuild kept container %s", id)
		}
	})
}
