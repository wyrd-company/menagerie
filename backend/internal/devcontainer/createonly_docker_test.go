//go:build docker

// The create-only tests: a job creates a workspace's container, does its
// own work while the container has never started, and then starts it.
package devcontainer_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
)

// buildAndCreate runs the steps up to a created container.
func (h *harness) buildAndCreate(b *devcontainer.BringUp) string {
	h.t.Helper()
	if err := b.Build(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	id, err := b.Create(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) begin(folder string, p devcontainer.Progress) *devcontainer.BringUp {
	h.t.Helper()
	b, err := h.rt.BeginStart(folder, p)
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

// neverStarted fails unless the container exists and has never run.
func (h *harness) neverStarted(id string) {
	h.t.Helper()
	if state := h.inspect(id).State; state.Status != "created" || state.StartedAt != "0001-01-01T00:00:00Z" {
		h.t.Fatalf("container %s: status %s, started at %s", id, state.Status, state.StartedAt)
	}
}

// inContainer says whether a path exists in the container, which may be
// stopped.
func (h *harness) inContainer(id, path string) bool {
	h.t.Helper()
	reader, _, err := h.client.CopyFromContainer(h.ctx, id, path)
	if dockerclient.IsErrNotFound(err) {
		return false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	reader.Close()
	return true
}

func TestCreateThenStartSingleContainer(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("create-single", map[string]string{
		".devcontainer/devcontainer.json": single("one"),
		".devcontainer/Dockerfile":        "FROM " + baseImage + "\n",
	})

	p := &logProgress{t: t}
	b := h.begin(folder, p)
	created := h.buildAndCreate(b)
	h.neverStarted(created)
	if s := h.status(folder); s.State != devcontainer.StateStopped || s.ContainerID != created {
		t.Fatalf("after create: %+v, want stopped %s", s, created)
	}
	if h.inContainer(created, "/tmp/post-create-count") {
		t.Fatal("create ran a lifecycle command")
	}
	if got := envValue(h.inspect(created), "SAMPLE_ENV"); got != "one" {
		t.Fatalf("SAMPLE_ENV=%q", got)
	}

	// A repeat keeps the container and does not start it.
	again, err := b.Create(h.ctx)
	if err != nil || again != created {
		t.Fatalf("repeat: %s, %v; want %s", again, err, created)
	}
	h.neverStarted(created)

	if err := b.Up(h.ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.status(folder); s.State != devcontainer.StateRunning || s.ContainerID != created {
		t.Fatalf("after up: %+v, want running %s", s, created)
	}
	if got := h.exec(created, "sh", "-c", "wc -l < /tmp/post-create-count"); got != "1" {
		t.Fatalf("postCreateCommand ran %s times", got)
	}
	if n := lines(t, folder+"/init-count"); n != 1 {
		t.Fatalf("initializeCommand ran %d times for one job", n)
	}
	want := []string{"read-definition", "initialize-command", "build", "create-container", "create-container", "up"}
	if !reflect.DeepEqual(p.phases, want) {
		t.Fatalf("phases %v, want %v", p.phases, want)
	}

	// Create never stops a running container that it keeps.
	kept := h.buildAndCreate(h.begin(folder, &logProgress{t: t}))
	if s := h.status(folder); s.State != devcontainer.StateRunning || kept != created {
		t.Fatalf("create on a running container: %s, %+v; want running %s", kept, s, created)
	}

	// A rebuild removes the old container once, and only the first step
	// after the build does.
	r, err := h.rt.BeginRebuild(folder, &logProgress{t: t})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := h.buildAndCreate(r)
	if rebuilt == created {
		t.Fatal("a rebuild's create kept the old container")
	}
	if _, err := h.client.ContainerInspect(h.ctx, created); !dockerclient.IsErrNotFound(err) {
		t.Fatalf("old container remains: %v", err)
	}
	h.neverStarted(rebuilt)
	if err := r.Up(h.ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.status(folder); s.State != devcontainer.StateRunning || s.ContainerID != rebuilt {
		t.Fatalf("rebuild's up replaced the created container: %+v, want running %s", s, rebuilt)
	}
}

// A job that stopped after its create leaves a container that the next
// plain start takes over.
func TestStartTakesOverACreatedContainer(t *testing.T) {
	h := newHarness(t)
	socket := t.TempDir()
	h.rt = devcontainer.New(h.client, h.options(devcontainer.Options{WorkspaceSocket: func(string) (string, error) { return socket, nil }}))
	folder := h.workspace("create-take-over", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "postCreateCommand": "echo created >> /tmp/post-create-count"}`,
	})
	created := h.buildAndCreate(h.begin(folder, &logProgress{t: t}))
	h.neverStarted(created)

	out := &strings.Builder{}
	if err := h.rt.Start(h.ctx, folder, &recordProgress{logProgress: logProgress{t: t}, out: out}); err != nil {
		t.Fatal(err)
	}
	if s := h.status(folder); s.State != devcontainer.StateRunning || s.ContainerID != created {
		t.Fatalf("after start: %+v, want running %s", s, created)
	}
	if strings.Contains(out.String(), "attaches at the next rebuild") {
		t.Fatalf("a created container counted as one without the socket:\n%s", out)
	}
	info := h.inspect(created)
	if envValue(info, devcontainer.WorkspaceSocketEnv) != devcontainer.WorkspaceSocketPath {
		t.Fatalf("env %v", info.Config.Env)
	}
	mounted := false
	for _, m := range info.Mounts {
		mounted = mounted || (m.Type == mount.TypeBind && m.Destination == devcontainer.WorkspaceSocketTarget && !m.RW)
	}
	if !mounted {
		t.Fatalf("mounts %+v", info.Mounts)
	}
	if got := h.exec(created, "sh", "-c", "wc -l < /tmp/post-create-count"); got != "1" {
		t.Fatalf("postCreateCommand ran %s times", got)
	}
}

func TestCreateThenStartCompose(t *testing.T) {
	h := newHarness(t)
	socket := t.TempDir()
	h.rt = devcontainer.New(h.client, h.options(devcontainer.Options{WorkspaceSocket: func(string) (string, error) { return socket, nil }}))
	definition := func(value string) string {
		return `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data", "containerEnv": {"SAMPLE_ENV": "` + value + `"}, "postCreateCommand": "echo created >> /tmp/post-create-count"}`
	}
	folder := h.workspace("create-compose", map[string]string{
		".devcontainer/compose.yml":       composeFile,
		".devcontainer/devcontainer.json": definition("one"),
	})

	b := h.begin(folder, &logProgress{t: t})
	primary := h.buildAndCreate(b)
	h.neverStarted(primary)
	project := h.inspect(primary).Config.Labels["com.docker.compose.project"]
	sidecar := h.service(project, "sidecar")
	h.neverStarted(sidecar)
	if h.inContainer(primary, "/tmp/post-create-count") {
		t.Fatal("create ran a lifecycle command")
	}

	if err := b.Up(h.ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.status(folder); s.State != devcontainer.StateRunning || s.ContainerID != primary {
		t.Fatalf("after up: %+v, want running %s", s, primary)
	}
	if got := h.service(project, "sidecar"); got != sidecar || !h.inspect(sidecar).State.Running {
		t.Fatalf("sidecar %s, want running %s", got, sidecar)
	}
	if got := h.exec(primary, "sh", "-c", "wc -l < /tmp/post-create-count"); got != "1" {
		t.Fatalf("postCreateCommand ran %s times", got)
	}

	// A changed definition replaces the container under Compose's rule,
	// and the new one does not start.
	h.write(folder, map[string]string{".devcontainer/devcontainer.json": definition("two")})
	changed := h.buildAndCreate(h.begin(folder, &logProgress{t: t}))
	if changed == primary {
		t.Fatal("a changed definition kept the container")
	}
	h.neverStarted(changed)
	if got := envValue(h.inspect(changed), "SAMPLE_ENV"); got != "two" {
		t.Fatalf("SAMPLE_ENV=%q", got)
	}
}

func TestCreateRefusesASELinuxRelabel(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("create-relabel", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "runArgs": ["-v", "${localWorkspaceFolder}:/relabel:z"]}`,
	})
	b := h.begin(folder, &logProgress{t: t})
	if err := b.Build(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(h.ctx); err == nil || !strings.Contains(err.Error(), "SELinux") {
		t.Fatalf("create: %v", err)
	}
	if s := h.status(folder); s.State != devcontainer.StateNoContainer {
		t.Fatalf("after refusal: %+v", s)
	}
}

// Create does not publish the feature lockfile; a job's Up does.
func TestCreateLeavesTheLockfileToUp(t *testing.T) {
	h := newHarness(t)
	ensureImage(t, h, featureImage)
	folder := h.workspace("create-lock", featureFiles(`{"image": "`+featureImage+`", "features": {"./local-feature": {}}}`))
	b := h.begin(folder, &logProgress{t: t})
	h.buildAndCreate(b)
	if got := readLock(t, folder); got != "" {
		t.Fatalf("lockfile published by create: %q", got)
	}
	if err := b.Up(h.ctx); err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Features map[string]json.RawMessage `json:"features"`
	}
	got := readLock(t, folder)
	if err := json.Unmarshal([]byte(got), &lock); err != nil || lock.Features == nil {
		t.Fatalf("lockfile after up: %q (%v)", got, err)
	}
}
