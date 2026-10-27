//go:build pocketid && browser && image && acceptance

package main

// TestAcceptanceFixture holds the built image, MENAGERIE_IMAGE, open for the
// web UI's acceptance run (web/test/integrated/acceptance.sh). It is the
// integrated run's image topology: the same control server and browser
// story, against Menagerie as an operator deploys it.
//
//   - the image on the Docker Engine named by DOCKER_HOST, which mounts its
//     own socket, the workspace root at the same path, owned by an operator
//     (uid 1000), and its Wyrwood consumer directory
//   - a real Wyrwood agent binding the container (wyrwood_test.go): Toha
//     fetches the two-version template over SSH and Menagerie pushes the
//     backups over SSH, both signed by Wyrwood's Transit key
//   - a disposable Pocket ID with a manager and a non-manager
//   - a third template version whose hook waits for a release file, so a
//     job can be held at a real boundary
//
// The test runs in scripts/isolated-docker.sh's runner, beside the Engine;
// the Engine publishes the site, Pocket ID and the control server on the
// host's loopback, so a browser on the host uses the same URLs. It ends
// when the control server receives POST /shutdown.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

// acceptanceName is the Menagerie container Wyrwood binds by name.
const acceptanceName = "sample-acceptance"

func envPort(t *testing.T, name string) int {
	t.Helper()
	port, err := strconv.Atoi(requireEnv(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return port
}

// heldTemplate turns version 1 of the sample template into one whose hook
// waits until heldRelease exists in the workspace folder.
func heldTemplate(t *testing.T, dir string) {
	t.Helper()
	browserTemplate(t, dir, 1)
	path := filepath.Join(dir, "template.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	held := strings.Replace(string(body), "'echo ran > hook-marker.txt'",
		fmt.Sprintf("'echo ran > hook-marker.txt; while [ ! -e %[1]s ]; do sleep 0.2; done; rm %[1]s'", heldRelease), 1)
	if held == string(body) {
		t.Fatal("the sample template has no hook to hold")
	}
	writeTo(t, dir, "template.yml", held)
}

func TestAcceptanceFixture(t *testing.T) {
	ref := requireEnv(t, "MENAGERIE_IMAGE")
	info := requireEnv(t, "MENAGERIE_BROWSER_INFO")
	port := envPort(t, "MENAGERIE_BROWSER_PORT")
	controlPort := envPort(t, "MENAGERIE_BROWSER_CONTROL_PORT")
	pocketPort := envPort(t, "MENAGERIE_POCKETID_PORT")
	ctx := context.Background()
	tmp := os.TempDir()

	// Published from the Engine's host, so on every address there.
	p := startPocketIDAt(t, "0.0.0.0", pocketPort)
	w := startWyrwood(t, p)
	if _, err := w.bao.Logical().Write("transit/keys/sample-git", map[string]any{"type": "ed25519"}); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	openTo(t, base, tmp)

	// The templates and the workspace repository on an SSH Git host that
	// admits Wyrwood's Transit key.
	host := backuptest.New(t)
	host.Authorize(transitKey(t, w, "sample-git"))
	if err := os.Chmod(host.KnownHosts, 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "template-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "-q", "-b", "main")
	browserTemplate(t, work, 1)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "v1")
	gitRun(t, work, "tag", "v1")
	browserTemplate(t, work, 2)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "v2")
	gitRun(t, work, "tag", "v2")
	gitRun(t, work, "checkout", "-q", "-b", "held", "v1")
	heldTemplate(t, work)
	gitRun(t, work, "commit", "-q", "-am", "held")
	gitRun(t, work, "tag", "held")
	gitRun(t, host.Dir, "clone", "-q", "--bare", work, filepath.Join(host.Dir, "sample-kit.git"))
	bare := filepath.Join(host.Dir, "backups.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}

	rootDir := filepath.Join(base, "workspaces")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chownTree(t, rootDir)

	site := fmt.Sprintf("http://localhost:%d", port)
	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	other := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Other", "name": "other"})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	p.user("sample-other", "Sample", "Other", other["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})
	template := host.URL("sample-kit.git")
	writeTo(t, base, "config.yml", fmt.Sprintf(`listen: 0.0.0.0:%d
workspace-root: %s
workspace-repository: %s
wyrwood-directory: /run/wyrwood
oidc:
  issuer: %s
  client-id: %s
  redirect-url: %s
templates:
  - {id: sample-kit-v1, name: "Sample kit, version 1", address: '%s@v1'}
  - {id: sample-kit-v2, name: "Sample kit, version 2", address: '%s@v2'}
  - {id: sample-kit-held, name: "Sample kit, held hook", address: '%s@held'}
`, port, rootDir, host.URL("backups.git"), p.url, client["id"], site+auth.CallbackPath, template, template, template))

	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: ref,
			Env: []string{
				"SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock",
				fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port),
			},
			Labels: map[string]string{"menagerie-test": "image"},
		},
		&container.HostConfig{
			NetworkMode:   "host",
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				tmp + ":" + tmp,
				filepath.Join(base, "config.yml") + ":/etc/menagerie/config.yml:ro",
				host.KnownHosts + ":/etc/ssh/ssh_known_hosts:ro",
				wyrwoodConsumers + "/" + acceptanceName + ":/run/wyrwood",
			},
		}, nil, nil, acceptanceName)
	if err != nil {
		t.Fatal(err)
	}
	c := &imageContainer{t: t, docker: p.docker, id: created.ID, site: site}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), c.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	// Every devcontainer of a folder under the root goes with the run.
	t.Cleanup(func() {
		entries, _ := os.ReadDir(rootDir)
		for _, e := range entries {
			removeDevcontainers(p, filepath.Join(rootDir, e.Name()))
		}
	})
	// The control server's outsider container goes too.
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), "sample-outsider", container.RemoveOptions{Force: true})
	})

	w.savePolicy("sample-menagerie", `path "transit/keys/sample-git" { capabilities = ["read"] }
path "transit/sign/sample-git" { capabilities = ["update"] }
`, true, []string{"sample-git"}, "/run/wyrwood")
	w.bind(acceptanceName, []string{"sample-menagerie"}, "1h", "8h", wyrwoodGID)
	waitFor(t, "a token for Menagerie", func() bool { return w.token(acceptanceName) == "valid" })
	if err := p.docker.ContainerStart(ctx, c.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	c.waitReady(0)
	if t.Failed() {
		return
	}

	healthy := func() bool {
		resp, err := http.Get(site + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	restart := func() error {
		if err := p.docker.ContainerRestart(context.Background(), c.id, container.StopOptions{}); err != nil {
			return err
		}
		for i := 0; !healthy(); i++ {
			if i == imageReadyAttempts {
				return fmt.Errorf("Menagerie did not answer after the restart")
			}
			time.Sleep(imageReadyInterval)
		}
		return nil
	}

	done := make(chan struct{})
	control := controlServer(t, controlFixture{p: p, rootDir: rootDir, bare: bare, docker: p.docker, done: done,
		restart: restart, release: true})
	csrv := &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", controlPort), Handler: control}
	go func() { _ = csrv.ListenAndServe() }()
	t.Cleanup(func() { _ = csrv.Shutdown(context.Background()) })

	data, _ := json.Marshal(map[string]string{
		"mode":    "image",
		"site":    site,
		"pocket":  p.url,
		"control": fmt.Sprintf("http://127.0.0.1:%d", controlPort),
		"root":    rootDir,
	})
	if err := os.WriteFile(info, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture ready: %s", data)
	<-done
	t.Logf("Menagerie logs:\n%s", c.logs(time.Time{}))
}
