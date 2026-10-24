//go:build pocketid && image

package main

// TestImage runs the built Menagerie image, MENAGERIE_IMAGE, on the Docker
// Engine named by DOCKER_HOST. That engine must see the test's temporary
// folder at the same path and share the test's loopback, as
// scripts/isolated-docker.sh arranges: the container mounts the folder at
// its own path, mounts the engine's socket, and uses the engine's host
// network so that it and the test reach the same disposable Pocket ID.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// Test-only bounds, imageReadyInterval apart: the workspace service
// answered /healthz about 1 s after start or restart on the shared host,
// and an interview's first batch, which fetches the template, arrived in
// under 1 s.
const (
	imageReadyAttempts = 240
	imageReadyInterval = 250 * time.Millisecond
)

// imageContainer is the Menagerie container under test.
type imageContainer struct {
	t      *testing.T
	docker *dockerclient.Client
	id     string
	site   string
}

// exec runs cmd in the container as user ("" for the image's user) and
// returns its output and exit code.
func (c *imageContainer) exec(user string, cmd ...string) (string, int) {
	c.t.Helper()
	ctx := context.Background()
	created, err := c.docker.ContainerExecCreate(ctx, c.id, container.ExecOptions{User: user, Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		c.t.Fatal(err)
	}
	att, err := c.docker.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	defer att.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil {
		c.t.Fatal(err)
	}
	inspected, err := c.docker.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	return strings.TrimSpace(out.String()), inspected.ExitCode
}

func (c *imageContainer) mustExec(cmd ...string) string {
	c.t.Helper()
	out, code := c.exec("", cmd...)
	if code != 0 {
		c.t.Fatalf("%v exited %d: %s", cmd, code, out)
	}
	return out
}

func (c *imageContainer) inspect() container.InspectResponse {
	c.t.Helper()
	info, err := c.docker.ContainerInspect(context.Background(), c.id)
	if err != nil {
		c.t.Fatal(err)
	}
	return info
}

// logs returns the container's output since a time, across restarts.
func (c *imageContainer) logs(since time.Time) string {
	c.t.Helper()
	opts := container.LogsOptions{ShowStdout: true, ShowStderr: true}
	if !since.IsZero() {
		opts.Since = since.Format(time.RFC3339Nano)
	}
	rc, err := c.docker.ContainerLogs(context.Background(), c.id, opts)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rc.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, rc)
	return out.String()
}

// waitReady waits until the container runs, has restarted restarts times,
// and its workspace service answers /healthz.
func (c *imageContainer) waitReady(restarts int) {
	c.t.Helper()
	for i := 0; ; i++ {
		info := c.inspect()
		if info.State.Running && info.RestartCount == restarts {
			if resp, err := http.Get(c.site + "/healthz"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
		if i == imageReadyAttempts {
			c.t.Fatalf("not ready after %d restarts: running %v, restarts %d\n%s", restarts, info.State.Running, info.RestartCount, c.logs(time.Time{}))
		}
		time.Sleep(imageReadyInterval)
	}
}

// dies subscribes to the container's die events and returns their exit codes.
func (c *imageContainer) dies(ctx context.Context) <-chan string {
	msgs, _ := c.docker.Events(ctx, events.ListOptions{Filters: filters.NewArgs(
		filters.Arg("container", c.id), filters.Arg("event", "die"))})
	codes := make(chan string, 4)
	go func() {
		for m := range msgs {
			codes <- m.Actor.Attributes["exitCode"]
		}
	}()
	return codes
}

func nextDie(t *testing.T, codes <-chan string) string {
	t.Helper()
	select {
	case code := <-codes:
		return code
	case <-time.After(imageReadyAttempts * imageReadyInterval):
		t.Fatal("the container did not stop")
		return ""
	}
}

// startInterview starts a create job through the workspace API and waits
// until the template service has answered it with the first question batch.
func startInterview(t *testing.T, c *http.Client, site, workspace string) {
	t.Helper()
	header := http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}}
	s := fetch(t, c, http.MethodPost, site+"/api/jobs", strings.NewReader(fmt.Sprintf(`{"operation":"create","workspace":%q,"templateId":"sample-kit"}`, workspace)), header)
	if s.status != http.StatusAccepted {
		t.Fatalf("create answered %d %s", s.status, s.body)
	}
	var snap jobs.Snapshot
	if err := json.Unmarshal([]byte(s.body), &snap); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		s := fetch(t, c, http.MethodGet, site+"/api/jobs/"+snap.JobID, nil, nil)
		if err := json.Unmarshal([]byte(s.body), &snap); err != nil {
			t.Fatalf("job answered %d %s", s.status, s.body)
		}
		switch snap.State {
		case jobs.WaitingForAnswers:
			return
		case jobs.Failed, jobs.Succeeded:
			t.Fatalf("the create job ended before asking: %s", s.body)
		}
		if i == imageReadyAttempts {
			t.Fatalf("the create job never asked: %s", s.body)
		}
		time.Sleep(imageReadyInterval)
	}
}

func TestImage(t *testing.T) {
	ref := requireEnv(t, "MENAGERIE_IMAGE")
	p := startPocketID(t)
	ctx := context.Background()

	base := t.TempDir()
	rootDir := filepath.Join(base, "workspaces")
	repo := filepath.Join(base, "templates", "sample-kit")
	wyrwood := filepath.Join(base, "wyrwood")
	for _, d := range []string{filepath.Join(rootDir, "alpha"), repo, wyrwood} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	browserTemplate(t, repo, 1)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v1")
	gitRun(t, repo, "tag", "v1")

	port := freePort(t)
	site := fmt.Sprintf("http://localhost:%d", port)
	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	other := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Other", "name": "other"})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	p.user("sample-other", "Sample", "Other", other["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})

	// The default template service socket and data folder: the image's.
	writeTo(t, base, "config.yml", fmt.Sprintf(`listen: 127.0.0.1:%d
workspace-root: %s
workspace-repository: ssh://git@localhost:1/backups.git
wyrwood-directory: %s
oidc:
  issuer: %s
  client-id: %s
  redirect-url: %s
templates:
  - {id: sample-kit, name: Sample kit, address: 'file://%s@v1'}
`, port, rootDir, wyrwood, p.url, client["id"], site+auth.CallbackPath, repo))

	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image:  ref,
			Env:    []string{"SSH_AUTH_SOCK=" + filepath.Join(wyrwood, "ssh-agent.sock"), fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port)},
			Labels: map[string]string{"menagerie-test": "image"},
		},
		&container.HostConfig{
			NetworkMode:   "host",
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				base + ":" + base,
				filepath.Join(base, "config.yml") + ":/etc/menagerie/config.yml:ro",
			},
		}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c := &imageContainer{t: t, docker: p.docker, id: created.ID, site: site}
	// Remove by the recorded id, whatever the test's outcome.
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), c.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	eventsCtx, stopEvents := context.WithCancel(ctx)
	t.Cleanup(stopEvents)
	died := c.dies(eventsCtx)
	if err := p.docker.ContainerStart(ctx, c.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	c.waitReady(0)
	if t.Failed() {
		return
	}

	t.Run("supervised services", func(t *testing.T) {
		c.t = t
		for _, service := range []string{"template-service", "workspace-service"} {
			if out := c.mustExec("/command/s6-svstat", "-o", "up", "/run/service/"+service); out != "true" {
				t.Errorf("%s up: %q", service, out)
			}
		}
		if out := c.mustExec("stat", "-c", "%a %U %G", "/run/menagerie"); out != "700 root root" {
			t.Errorf("socket folder: %q", out)
		}
		if out := c.mustExec("stat", "-c", "%F", "/run/menagerie/template.sock"); out != "socket" {
			t.Errorf("template.sock: %q", out)
		}
		// The workspace service's user reaches the socket; any other does not.
		if out := c.mustExec("curl", "-sS", "--unix-socket", "/run/menagerie/template.sock", "http://template-service/"); !strings.Contains(out, `"not-found"`) {
			t.Errorf("template service answered %q", out)
		}
		if out, code := c.exec("65534:65534", "curl", "-sS", "--unix-socket", "/run/menagerie/template.sock", "http://template-service/"); code == 0 {
			t.Errorf("an unprivileged user reached the template service: %q", out)
		}
		c.mustExec("menagerie-healthcheck")
		// Only the workspace service's port is declared, and the template
		// service listens on no TCP socket.
		info := c.inspect()
		if len(info.Config.ExposedPorts) != 1 {
			t.Errorf("exposed ports %v", info.Config.ExposedPorts)
		}
		if _, ok := info.Config.ExposedPorts["8080/tcp"]; !ok {
			t.Errorf("exposed ports %v", info.Config.ExposedPorts)
		}
		listening := `pid=$(/command/s6-svstat -o pid /run/service/template-service)
for fd in /proc/$pid/fd/*; do
  inode=$(readlink "$fd" | sed -n 's/^socket:\[\([0-9]*\)\]$/\1/p')
  [ -n "$inode" ] || continue
  awk -v inode="$inode" '$4 == "0A" && $10 == inode { print "tcp", $2 }' /proc/net/tcp /proc/net/tcp6
done`
		if out := c.mustExec("sh", "-c", listening); out != "" {
			t.Errorf("the template service listens on TCP: %s", out)
		}
	})

	t.Run("a manager signs in and starts an interview", func(t *testing.T) {
		c.t = t
		b := signInBrowser()
		s, _ := p.signIn(t, b, "sample-manager", site+"/")
		if s.status != http.StatusOK || !strings.Contains(s.body, `<div id="root">`) {
			t.Fatalf("sign-in ended at %s with %d %.200q", s.url, s.status, s.body)
		}
		if s := fetch(t, b, http.MethodGet, site+"/api/workspaces", nil, nil); s.status != http.StatusOK || !strings.Contains(s.body, `"name":"alpha"`) {
			t.Fatalf("workspaces answered %d %s", s.status, s.body)
		}
		startInterview(t, b, site, "beta")
	})

	t.Run("a non-manager is denied", func(t *testing.T) {
		c.t = t
		s, _ := p.signIn(t, signInBrowser(), "sample-other", site+"/")
		if s.status != http.StatusForbidden || !strings.Contains(s.body, `data-kind="not-a-manager"`) {
			t.Fatalf("non-manager sign-in answered %d %.200q", s.status, s.body)
		}
	})

	// Either service stopping stops the container with its status, and the
	// restart policy brings the whole of Menagerie back.
	for i, service := range []string{"template-service", "workspace-service"} {
		t.Run(service+" failure restarts the container", func(t *testing.T) {
			c.t = t
			killed := time.Now()
			c.mustExec("sh", "-c", "kill -KILL $(/command/s6-svstat -o pid /run/service/"+service+")")
			if code := nextDie(t, died); code != "137" {
				t.Errorf("the container stopped with %q, not 137", code)
			}
			c.waitReady(i + 1)
			if want := "menagerie: " + service + " stopped (signal 9); stopping the container"; !strings.Contains(c.logs(killed), want) {
				t.Errorf("logs lack %q", want)
			}
			b := signInBrowser()
			if s, _ := p.signIn(t, b, "sample-manager", site+"/"); s.status != http.StatusOK {
				t.Fatalf("sign-in after restart answered %d", s.status)
			}
			startInterview(t, b, site, fmt.Sprintf("gamma%d", i))
		})
	}

	t.Run("docker stop stops both services cleanly", func(t *testing.T) {
		c.t = t
		restarts := c.inspect().RestartCount
		started := time.Now()
		if err := p.docker.ContainerStop(ctx, c.id, container.StopOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Logf("docker stop took %s", time.Since(started).Round(time.Millisecond))
		if code := nextDie(t, died); code != "0" {
			t.Errorf("the container stopped with %q, not 0", code)
		}
		info := c.inspect()
		if info.State.Running || info.RestartCount != restarts {
			t.Errorf("after stop: running %v, restarts %d of %d", info.State.Running, info.RestartCount, restarts)
		}
		logs := c.logs(started)
		for _, service := range []string{"template-service", "workspace-service"} {
			if want := "menagerie: " + service + " stopped (exit status 0)"; !strings.Contains(logs, want) {
				t.Errorf("logs lack %q", want)
			}
		}
	})
}
