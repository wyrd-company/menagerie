//go:build docker

// Docker smoke tests for the devcontainer runtime. They need a Docker
// Engine whose bind sources are this process's filesystem and the
// alpine:3.22 image. Every resource is labelled with, or named after, the
// test's own temporary folder and removed by that marker. Feature builds
// use busybox:1.37, whose layers must build with the daemon's BuildKit.
//
//	go test -tags docker ./backend/internal/devcontainer/
package devcontainer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/wyrd-company/devcontainer-builder/pkg/host/dockercompose"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

const (
	baseImage    = "alpine:3.22"
	featureImage = "busybox:1.37"
)

type logProgress struct {
	t      *testing.T
	phases []string
}

func (p *logProgress) Phase(name string) { p.phases = append(p.phases, name) }
func (p *logProgress) Log(prefix string) io.Writer {
	return writerFunc(func(b []byte) (int, error) {
		p.t.Logf("%s%s", prefix, strings.TrimRight(string(b), "\n"))
		return len(b), nil
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

type harness struct {
	t   *testing.T
	ctx context.Context
	// client observes and cleans up: the Engine named by
	// MENAGERIE_TEST_ENGINE, when the suite runs through the Docker proxy,
	// which redacts what a test inspects; else proxied.
	client *dockerclient.Client
	// proxied is the client the runtimes and throwaway containers use,
	// for the Engine or proxy DOCKER_HOST names.
	proxied *dockerclient.Client
	rt      *devcontainer.Runtime
	// prefix starts the folder of every workspace of this test.
	prefix string
	root   string
	runID  string
	// images are built images no remaining container references.
	images []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	return newHarnessAt(t, root, filepath.Base(filepath.Dir(root))+"-"+filepath.Base(root))
}

// newHarnessAt is a harness whose workspaces are folders of root and whose
// resources carry runID.
func newHarnessAt(t *testing.T, root, runID string) *harness {
	t.Helper()
	proxied, err := dockerproxy.NewClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := proxied
	if engine := os.Getenv("MENAGERIE_TEST_ENGINE"); engine != "" {
		if client, err = dockerclient.NewClientWithOpts(dockerclient.WithHost(engine), dockerclient.WithAPIVersionNegotiation()); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	h := &harness{t: t, ctx: ctx, client: client, proxied: proxied, root: root, runID: runID, prefix: root + "/"}
	h.rt = devcontainer.New(proxied, h.options(devcontainer.Options{}))
	t.Cleanup(h.cleanup)
	ensureImage(t, h, baseImage)
	return h
}

// options runs initializeCommand in throwaway containers of baseImage that
// carry the test's run label, and names each operation's workspace to the
// Docker proxy as the workspace service does.
func (h *harness) options(opts devcontainer.Options) devcontainer.Options {
	opts.Client = dockerproxy.NewClient
	opts.Containers = throwaway.NewDocker(h.proxied, throwaway.DockerOptions{Root: h.root, User: throwaway.ServiceUser(), Labels: map[string]string{"menagerie.test.run": h.runID}, Client: dockerproxy.NewClient})
	opts.RunnerImage = func(context.Context) (string, error) { return baseImage, nil }
	return opts
}

// cleanup removes only resources marked by this test's root or run label.
func (h *harness) cleanup() {
	ctx := context.Background()
	list, err := h.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "devcontainer.local_folder"))})
	if err != nil {
		h.t.Errorf("cleanup list: %v", err)
	}
	projects := map[string]bool{}
	images := append([]string(nil), h.images...)
	for _, c := range list {
		if strings.HasPrefix(c.Labels["devcontainer.local_folder"], h.prefix) {
			images = append(images, c.Image)
			if p := c.Labels[dockercompose.LabelProject]; p != "" {
				projects[p] = true
			}
		}
	}
	// Compose sidecars and project resources follow their project label.
	for p := range projects {
		args := filters.NewArgs(filters.Arg("label", dockercompose.LabelProject+"="+p))
		cs, _ := h.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
		for _, c := range cs {
			_ = h.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
		nets, _ := h.client.NetworkList(ctx, network.ListOptions{Filters: args})
		for _, n := range nets {
			_ = h.client.NetworkRemove(ctx, n.ID)
		}
		vols, _ := h.client.VolumeList(ctx, volume.ListOptions{Filters: args})
		for _, v := range vols.Volumes {
			_ = h.client.VolumeRemove(ctx, v.Name, true)
		}
	}
	for _, c := range list {
		if strings.HasPrefix(c.Labels["devcontainer.local_folder"], h.prefix) {
			_ = h.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
	}
	outsiders, _ := h.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "menagerie.test.run="+h.runID))})
	for _, c := range outsiders {
		_ = h.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}
	shared := map[string]bool{}
	for _, ref := range []string{baseImage, featureImage} {
		if info, err := h.client.ImageInspect(ctx, ref); err == nil {
			shared[info.ID] = true
		}
	}
	for _, ref := range images {
		if info, err := h.client.ImageInspect(ctx, ref); err != nil || shared[info.ID] {
			continue
		}
		_, _ = h.client.ImageRemove(ctx, ref, image.RemoveOptions{Force: true})
	}
	entries, _ := os.ReadDir(h.root)
	for _, e := range entries {
		_, _ = h.client.ImageRemove(ctx, devcontainer.ImageName(filepath.Join(h.root, e.Name())), image.RemoveOptions{Force: true})
	}
	_ = h.client.Close()
}

func (h *harness) workspace(name string, files map[string]string) string {
	h.t.Helper()
	folder := filepath.Join(h.root, name)
	h.write(folder, files)
	return folder
}

func (h *harness) write(folder string, files map[string]string) {
	h.t.Helper()
	for rel, body := range files {
		path := filepath.Join(folder, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) status(folder string) devcontainer.Status {
	h.t.Helper()
	s, err := h.rt.Status(h.ctx, folder)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) mustRun(op func(context.Context, string, devcontainer.Progress) error, folder string) {
	h.t.Helper()
	if err := op(h.ctx, folder, &logProgress{t: h.t}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) inspect(id string) container.InspectResponse {
	h.t.Helper()
	info, err := h.client.ContainerInspect(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return info
}

func (h *harness) exec(id string, cmd ...string) string {
	h.t.Helper()
	created, err := h.client.ContainerExecCreate(h.ctx, id, container.ExecOptions{Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		h.t.Fatal(err)
	}
	att, err := h.client.ContainerExecAttach(h.ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	defer att.Close()
	var out, errOut bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errOut, att.Reader); err != nil {
		h.t.Fatal(err)
	}
	return strings.TrimSpace(out.String() + errOut.String())
}

func envValue(info container.InspectResponse, key string) string {
	for _, kv := range info.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func lines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

const singleDefinition = `{
  // A Dockerfile build exercises the builder path.
  "build": {"dockerfile": "Dockerfile"},
  "initializeCommand": {
    "id": "echo \"${devcontainerId}\" > init-id",
    "count": ["sh", "-c", "echo run >> init-count"]
  },
  "containerEnv": {"DEVCONTAINER_ID": "${devcontainerId}", "SAMPLE_ENV": "%s"},
  "postCreateCommand": "echo created >> /tmp/post-create-count",
  "runArgs": ["--restart=unless-stopped"],
  "privileged": true
}`

func single(value string) string { return strings.Replace(singleDefinition, "%s", value, 1) }

func TestSingleContainerStartStopRebuild(t *testing.T) {
	// The definition asks for privileged and a restart policy, which the
	// proxy admits only through an exception for a workspace of its root.
	waitsOn(t, "1814", "1571")
	h := newHarness(t)
	folder := h.workspace("single-space", map[string]string{
		".devcontainer/devcontainer.json": single("one"),
		".devcontainer/Dockerfile":        "FROM " + baseImage + "\n",
	})
	if s := h.status(folder); s.State != devcontainer.StateNoContainer {
		t.Fatalf("before start: %+v", s)
	}

	h.mustRun(h.rt.Start, folder)
	first := h.status(folder)
	if first.State != devcontainer.StateRunning {
		t.Fatalf("after start: %+v", first)
	}
	info := h.inspect(first.ContainerID)
	initID, _ := os.ReadFile(filepath.Join(folder, "init-id"))
	if got := strings.TrimSpace(string(initID)); got == "" || got != envValue(info, "DEVCONTAINER_ID") {
		t.Fatalf("initializeCommand ${devcontainerId} %q, container %q", got, envValue(info, "DEVCONTAINER_ID"))
	}
	if !info.HostConfig.Privileged || info.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("privileged %t restart %q", info.HostConfig.Privileged, info.HostConfig.RestartPolicy.Name)
	}
	if info.Config.Labels["devcontainer.local_folder"] != folder || info.Config.Labels["devcontainer.config_file"] != filepath.Join(folder, ".devcontainer", "devcontainer.json") {
		t.Fatalf("labels %v", info.Config.Labels)
	}
	if info.Config.Image != devcontainer.ImageName(folder) {
		t.Fatalf("image %s", info.Config.Image)
	}

	h.mustRun(h.rt.Stop, folder)
	if s := h.status(folder); s.State != devcontainer.StateStopped || s.ContainerID != first.ContainerID {
		t.Fatalf("after stop: %+v", s)
	}

	h.mustRun(h.rt.Start, folder)
	if s := h.status(folder); s.State != devcontainer.StateRunning || s.ContainerID != first.ContainerID {
		t.Fatalf("restart created a new container: %+v", s)
	}
	if got := h.exec(first.ContainerID, "sh", "-c", "wc -l < /tmp/post-create-count"); got != "1" {
		t.Fatalf("postCreateCommand ran %s times across stop/start", got)
	}
	if n := lines(t, filepath.Join(folder, "init-count")); n != 2 {
		t.Fatalf("initializeCommand ran %d times for two starts", n)
	}

	h.write(folder, map[string]string{".devcontainer/devcontainer.json": single("two")})
	h.mustRun(h.rt.Rebuild, folder)
	rebuilt := h.status(folder)
	if rebuilt.State != devcontainer.StateRunning || rebuilt.ContainerID == first.ContainerID {
		t.Fatalf("after rebuild: %+v", rebuilt)
	}
	if got := envValue(h.inspect(rebuilt.ContainerID), "SAMPLE_ENV"); got != "two" {
		t.Fatalf("rebuild used the old definition: SAMPLE_ENV=%q", got)
	}
	if _, err := h.client.ContainerInspect(h.ctx, first.ContainerID); !dockerclient.IsErrNotFound(err) {
		t.Fatalf("old container remains: %v", err)
	}
	if got := h.exec(rebuilt.ContainerID, "sh", "-c", "wc -l < /tmp/post-create-count"); got != "1" {
		t.Fatalf("rebuilt container postCreate count %s", got)
	}
	if n := lines(t, filepath.Join(folder, "init-count")); n != 3 {
		t.Fatalf("initializeCommand ran %d times for start, start, rebuild", n)
	}

	// Stop does not read the definition: an unreadable one still stops.
	h.write(folder, map[string]string{".devcontainer/devcontainer.json": "{ broken"})
	h.mustRun(h.rt.Stop, folder)
	if s := h.status(folder); s.State != devcontainer.StateStopped {
		t.Fatalf("stop with a broken definition: %+v", s)
	}
}

func TestFailingInitializeCommandPreventsStartAndRebuild(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("init-space", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "initializeCommand": "exit 7"}`,
	})
	err := h.rt.Start(h.ctx, folder, &logProgress{t: t})
	var initErr *devcontainer.InitializeError
	if !errors.As(err, &initErr) {
		t.Fatalf("start: %v", err)
	}
	if s := h.status(folder); s.State != devcontainer.StateNoContainer {
		t.Fatalf("container created despite failure: %+v", s)
	}

	h.write(folder, map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "initializeCommand": "true"}`})
	h.mustRun(h.rt.Start, folder)
	running := h.status(folder)
	h.write(folder, map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "initializeCommand": ["sh", "-c", "exit 7"]}`})
	if err := h.rt.Rebuild(h.ctx, folder, &logProgress{t: t}); !errors.As(err, &initErr) {
		t.Fatalf("rebuild: %v", err)
	}
	if s := h.status(folder); s != running {
		t.Fatalf("failed rebuild changed the container: %+v -> %+v", running, s)
	}
}

// TestLocalEnvReadsOnlyAllowedVariables sets variables in the test
// process's own environment, as Menagerie's would be, and allows one.
func TestLocalEnvReadsOnlyAllowedVariables(t *testing.T) {
	h := newHarness(t)
	t.Setenv("MENAGERIE_TEST_ALLOWED", "allowed-value")
	t.Setenv("MENAGERIE_TEST_SECRET", "secret-value")
	rt := devcontainer.New(h.proxied, h.options(devcontainer.Options{LocalEnv: []string{"MENAGERIE_TEST_ALLOWED"}}))
	folder := h.workspace("env-space", map[string]string{
		".devcontainer/devcontainer.json": `{
  "image": "` + baseImage + `",
  "initializeCommand": "env > init-env; echo ${localEnv:MENAGERIE_TEST_SECRET} > init-arg",
  "containerEnv": {
    "ALLOWED": "${localEnv:MENAGERIE_TEST_ALLOWED}",
    "SECRET": "${localEnv:MENAGERIE_TEST_SECRET}",
    "SECRET_ALIAS": "${env:MENAGERIE_TEST_SECRET}",
    "SECRET_DEFAULT": "${localEnv:MENAGERIE_TEST_SECRET:fallback}"
  },
  "remoteEnv": {"REMOTE_SECRET": "${localEnv:MENAGERIE_TEST_SECRET}"}
}`,
	})
	var log strings.Builder
	p := &recordProgress{logProgress: logProgress{t: t}, out: &log}
	if err := rt.Start(h.ctx, folder, p); err != nil {
		t.Fatal(err)
	}
	info := h.inspect(h.status(folder).ContainerID)
	for key, want := range map[string]string{"ALLOWED": "allowed-value", "SECRET": "", "SECRET_ALIAS": "", "SECRET_DEFAULT": "fallback"} {
		if got := envValue(info, key); got != want {
			t.Errorf("containerEnv %s=%q want %q", key, got, want)
		}
	}
	if got := h.exec(info.ID, "sh", "-c", "env"); strings.Contains(got, "secret-value") {
		t.Errorf("devcontainer environment holds the refused value:\n%s", got)
	}
	for key, value := range info.Config.Labels {
		if strings.Contains(value, "secret-value") {
			t.Errorf("label %s holds the refused value: %s", key, value)
		}
	}
	initEnv, _ := os.ReadFile(filepath.Join(folder, "init-env"))
	if !strings.Contains(string(initEnv), "MENAGERIE_TEST_ALLOWED=allowed-value") || !strings.Contains(string(initEnv), "\n"+imagePath(t, h)+"\n") || strings.Contains(string(initEnv), "secret-value") {
		t.Errorf("initializeCommand environment:\n%s", initEnv)
	}
	if arg, _ := os.ReadFile(filepath.Join(folder, "init-arg")); strings.TrimSpace(string(arg)) != "" {
		t.Errorf("initializeCommand substitution read the refused value: %q", arg)
	}
	if got := strings.Count(log.String(), "localEnv MENAGERIE_TEST_SECRET is not in local-env"); got != 1 {
		t.Errorf("job log notes the refused variable %d times, want once:\n%s", got, log.String())
	}
	if strings.Contains(log.String(), "MENAGERIE_TEST_ALLOWED is not") {
		t.Errorf("job log notes the allowed variable:\n%s", log.String())
	}
}

// imagePath is baseImage's own PATH variable, PATH=...
func imagePath(t *testing.T, h *harness) string {
	t.Helper()
	info, err := h.client.ImageInspect(h.ctx, baseImage)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range info.Config.Env {
		if strings.HasPrefix(pair, "PATH=") {
			return pair
		}
	}
	t.Fatalf("%s has no PATH", baseImage)
	return ""
}

// initializeProbe reports, one line each, what initializeCommand must not
// reach: the Docker socket, the agent socket at $AGENT_PATH substituted
// in, SSH_AUTH_SOCK and the variables local-env does not allow. It fails
// when it found any, and writes into the workspace folder.
const initializeProbe = `found=$(for p in /var/run/docker.sock /run/docker.sock "$1"; do [ -e "$p" ] && echo "reachable $p"; done
[ -n "${SSH_AUTH_SOCK:-}" ] && echo "SSH_AUTH_SOCK is set"
env | grep MENAGERIE_TEST_SECRET)
env > init-env
echo written > init-written
[ -z "$found" ] || { echo "$found" > init-found; exit 1; }`

// TestInitializeCommandCannotReachDockerTheAgentOrMenageriesEnvironment is
// the done-when test of initializeCommand: in each of its parallel
// containers it finds no Docker socket, no ssh-agent socket and no
// Menagerie variable beyond local-env, while it still writes into the
// workspace folder.
func TestInitializeCommandCannotReachDockerTheAgentOrMenageriesEnvironment(t *testing.T) {
	h := newHarness(t)
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	agent := filepath.Join(dir, "ssh-agent.sock")
	l, err := net.Listen("unix", agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	t.Setenv("SSH_AUTH_SOCK", agent)
	t.Setenv("MENAGERIE_TEST_SECRET", "secret-value")
	t.Setenv("MENAGERIE_TEST_ALLOWED", "allowed-value")
	rt := devcontainer.New(h.proxied, h.options(devcontainer.Options{LocalEnv: []string{"MENAGERIE_TEST_ALLOWED"}}))
	probe, _ := json.Marshal([]string{"/bin/sh", "-c", initializeProbe, "sh", agent})
	folder := h.workspace("init-space", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "initializeCommand": {"probe": ` + string(probe) + `, "write": "echo parallel > init-parallel"}}`,
	})
	if err := rt.Start(h.ctx, folder, &logProgress{t: t}); err != nil {
		found, _ := os.ReadFile(filepath.Join(folder, "init-found"))
		t.Fatalf("start: %v\n%s", err, found)
	}
	initEnv, _ := os.ReadFile(filepath.Join(folder, "init-env"))
	for _, unwanted := range []string{"secret-value", "SSH_AUTH_SOCK", "DOCKER_HOST", "TMPDIR="} {
		if strings.Contains(string(initEnv), unwanted) {
			t.Errorf("initializeCommand environment holds %q:\n%s", unwanted, initEnv)
		}
	}
	if !strings.Contains(string(initEnv), "MENAGERIE_TEST_ALLOWED=allowed-value") || !strings.Contains(string(initEnv), imagePath(t, h)) {
		t.Errorf("initializeCommand environment:\n%s", initEnv)
	}
	for file, want := range map[string]string{"init-written": "written\n", "init-parallel": "parallel\n"} {
		if got, err := os.ReadFile(filepath.Join(folder, file)); err != nil || string(got) != want {
			t.Errorf("%s %q %v", file, got, err)
		}
	}
	left, err := h.client.ContainerList(h.ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", throwaway.LabelKind+"="+throwaway.KindInitializeCommand), filters.Arg("label", throwaway.LabelRoot+"="+h.root))})
	if err != nil || len(left) != 0 {
		t.Fatalf("%d initializeCommand containers remain: %v", len(left), err)
	}
}

// recordProgress also keeps the operation's output.
type recordProgress struct {
	logProgress
	out *strings.Builder
}

func (p *recordProgress) Log(prefix string) io.Writer {
	inner := p.logProgress.Log(prefix)
	return writerFunc(func(b []byte) (int, error) {
		p.out.Write(b)
		return inner.Write(b)
	})
}

const composeFile = `services:
  app:
    image: ` + baseImage + `
    command: ["sleep", "infinity"]
    volumes:
      - data:/data
  sidecar:
    image: ` + baseImage + `
    command: ["sleep", "infinity"]
    volumes:
      - data:/data
volumes:
  data: {}
`

func TestComposeRebuildKeepsVolumesAndSidecars(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("compose-space", map[string]string{
		".devcontainer/compose.yml":       composeFile,
		".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data", "initializeCommand": "echo run >> init-count", "containerEnv": {"SAMPLE_ENV": "one"}}`,
	})
	h.mustRun(h.rt.Start, folder)
	primary := h.status(folder)
	if primary.State != devcontainer.StateRunning {
		t.Fatalf("after start %+v", primary)
	}
	project := h.inspect(primary.ContainerID).Config.Labels[dockercompose.LabelProject]
	sidecar := h.service(project, "sidecar")
	if got := h.exec(sidecar, "sh", "-c", "echo kept > /data/keep && cat /data/keep"); got != "kept" {
		t.Fatalf("write to volume: %q", got)
	}

	h.write(folder, map[string]string{".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data", "initializeCommand": "echo run >> init-count", "containerEnv": {"SAMPLE_ENV": "two"}}`})
	h.mustRun(h.rt.Rebuild, folder)
	rebuilt := h.status(folder)
	if rebuilt.State != devcontainer.StateRunning || rebuilt.ContainerID == primary.ContainerID {
		t.Fatalf("primary not recreated: %+v -> %+v", primary, rebuilt)
	}
	if got := envValue(h.inspect(rebuilt.ContainerID), "SAMPLE_ENV"); got != "two" {
		t.Fatalf("rebuild used the old definition: %q", got)
	}
	if got := h.service(project, "sidecar"); got != sidecar {
		t.Fatalf("sidecar recreated: %s -> %s", sidecar, got)
	}
	if got := h.exec(rebuilt.ContainerID, "cat", "/data/keep"); got != "kept" {
		t.Fatalf("named volume data lost: %q", got)
	}
	if n := lines(t, filepath.Join(folder, "init-count")); n != 2 {
		t.Fatalf("initializeCommand ran %d times", n)
	}

	// Stop follows the running project even after the definition stops
	// describing Compose.
	h.write(folder, map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `"}`})
	h.mustRun(h.rt.Stop, folder)
	for _, id := range []string{rebuilt.ContainerID, sidecar} {
		if h.inspect(id).State.Running {
			t.Fatalf("container %s still running after stop", id)
		}
	}
}

func (h *harness) service(project, service string) string {
	h.t.Helper()
	list, err := h.client.ContainerList(h.ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", dockercompose.LabelProject+"="+project),
		filters.Arg("label", dockercompose.LabelService+"="+service),
	)})
	if err != nil || len(list) != 1 {
		h.t.Fatalf("service %s: %v (%d containers)", service, err, len(list))
	}
	return list[0].ID
}

// TestWorkspaceListShowsOnlyRootFolders runs the API over the real runtime:
// a started workspace is running, a folder without a container is listed,
// and containers labelled outside the root, for a missing folder, or not
// labelled at all never appear.
func TestWorkspaceListShowsOnlyRootFolders(t *testing.T) {
	h := newHarness(t)
	running := h.workspace("listed-running", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `"}`,
	})
	h.workspace("listed-empty", map[string]string{"README": "no definition\n"})
	h.mustRun(h.rt.Start, running)
	for _, labels := range []map[string]string{
		{"devcontainer.local_folder": "/elsewhere/outsider"},
		{"devcontainer.local_folder": h.root + "/no-folder"},
		{"devcontainer.local_folder": h.root},
		{},
	} {
		labels["menagerie.test.run"] = h.runID
		created, err := h.client.ContainerCreate(h.ctx, &container.Config{Image: baseImage, Cmd: []string{"sleep", "infinity"}, Labels: labels}, nil, nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.client.ContainerStart(h.ctx, created.ID, container.StartOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	root, err := workspace.NewRoot(h.root)
	if err != nil {
		t.Fatal(err)
	}
	manager := jobs.NewManager(h.ctx, jobs.Options{})
	srv := httptest.NewServer(api.New(api.Options{Root: root, Status: h.rt, Jobs: manager, Operations: api.ContainerOperations(root, h.rt)}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/workspaces")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Workspaces []struct {
			Name        string  `json:"name"`
			State       string  `json:"state"`
			ContainerID *string `json:"containerId"`
		} `json:"workspaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Workspaces) != 2 ||
		body.Workspaces[0].Name != "listed-empty" || body.Workspaces[0].State != "no-container" || body.Workspaces[0].ContainerID != nil ||
		body.Workspaces[1].Name != "listed-running" || body.Workspaces[1].State != "running" || body.Workspaces[1].ContainerID == nil {
		t.Fatalf("list %+v", body.Workspaces)
	}
}

// featureFiles is a definition with a local feature and an empty lockfile
// initialization marker.
func featureFiles(definition string) map[string]string {
	return map[string]string{
		".devcontainer/devcontainer.json":                       definition,
		".devcontainer/devcontainer-lock.json":                  "",
		".devcontainer/local-feature/devcontainer-feature.json": `{"id": "local-feature", "version": "1.0.0", "name": "Local feature"}`,
		".devcontainer/local-feature/install.sh":                "#!/bin/sh\necho installed > /feature-marker\n",
		".devcontainer/compose.yml":                             "services:\n  app:\n    image: " + featureImage + "\n    command: [\"sleep\", \"infinity\"]\n",
	}
}

func readLock(t *testing.T, folder string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(folder, ".devcontainer", "devcontainer-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestFeatureLockfilePublishedOnlyAfterSuccess runs a feature build whose
// postCreateCommand fails, then one that succeeds, in both modes. The empty
// lockfile marker stays empty after the failure and holds the resolved
// feature document after the success.
func TestFeatureLockfilePublishedOnlyAfterSuccess(t *testing.T) {
	waitsOn(t, "1814")
	h := newHarness(t)
	ensureImage(t, h, featureImage)
	definitions := map[string]string{
		"single":  `{"image": "` + featureImage + `", "features": {"./local-feature": {}}, "postCreateCommand": "%s"}`,
		"compose": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/", "features": {"./local-feature": {}}, "postCreateCommand": "%s"}`,
	}
	for mode, definition := range definitions {
		t.Run(mode, func(t *testing.T) {
			folder := h.workspace("lock-"+mode, featureFiles(strings.Replace(definition, "%s", "exit 3", 1)))
			err := h.rt.Start(h.ctx, folder, &logProgress{t: t})
			// Rebuild replaces this container; keep its image for cleanup.
			if s, statusErr := h.rt.Status(h.ctx, folder); statusErr == nil && s.ContainerID != "" {
				h.images = append(h.images, h.inspect(s.ContainerID).Config.Image)
			}
			if err == nil {
				t.Fatal("start with a failing postCreateCommand succeeded")
			}
			if got := readLock(t, folder); got != "" {
				t.Fatalf("lockfile published by a failed start: %q", got)
			}

			h.write(folder, map[string]string{".devcontainer/devcontainer.json": strings.Replace(definition, "%s", "true", 1)})
			h.mustRun(h.rt.Rebuild, folder)
			s := h.status(folder)
			if got := h.exec(s.ContainerID, "cat", "/feature-marker"); got != "installed" {
				t.Fatalf("feature not installed: %q", got)
			}
			var lock struct {
				Features map[string]json.RawMessage `json:"features"`
			}
			got := readLock(t, folder)
			if err := json.Unmarshal([]byte(got), &lock); err != nil || lock.Features == nil {
				t.Fatalf("lockfile after success: %q (%v)", got, err)
			}
		})
	}
}

func TestStopReachesTheComposeProjectWithoutItsPrimary(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("compose-left", map[string]string{
		".devcontainer/compose.yml":       composeFile,
		".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data"}`,
	})
	h.mustRun(h.rt.Start, folder)
	primary := h.status(folder)
	project := h.inspect(primary.ContainerID).Config.Labels[dockercompose.LabelProject]
	sidecar := h.service(project, "sidecar")
	if err := h.client.ContainerRemove(h.ctx, primary.ContainerID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if !h.inspect(sidecar).State.Running {
		t.Fatal("the sidecar stopped with its primary")
	}
	h.mustRun(h.rt.Stop, folder)
	if h.inspect(sidecar).State.Running {
		t.Fatal("the sidecar still runs after the stop")
	}
}

func TestMountingListsTheRunningContainersThatMountTheFolder(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("mounted", map[string]string{"file.txt": "x\n"})
	// Through the Docker proxy, Menagerie sees only the containers created
	// through it, so the test creates them there.
	creator := h.client
	if proxied() {
		creator = h.proxied
	}
	run := func(source string) string {
		created, err := creator.ContainerCreate(h.ctx,
			&container.Config{Image: baseImage, Cmd: []string{"sleep", "infinity"}, Labels: map[string]string{"menagerie.test.run": h.runID}},
			&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: source, Target: "/mnt"}}}, nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := creator.ContainerStart(h.ctx, created.ID, container.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		return created.ID[:12]
	}
	inside, around := run(folder), run(h.root)
	run(t.TempDir())
	stopped := run(folder)
	if err := h.client.ContainerStop(h.ctx, stopped, container.StopOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := h.rt.Mounting(h.ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{inside, around}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mounting %v, want %v", got, want)
	}
}

// TestComposeConfigsAndSecretsStageInTheWorkspacesFolder brings up a
// Compose workspace whose service mounts a config and a secret: both are
// written into the workspace's own folder of the staging folder, a host
// path, and the service reads them.
func TestComposeConfigsAndSecretsStageInTheWorkspacesFolder(t *testing.T) {
	h := newHarness(t)
	staging := t.TempDir()
	h.rt = devcontainer.New(h.proxied, h.options(devcontainer.Options{ComposeStaging: staging}))
	folder := h.workspace("staged-space", map[string]string{
		".devcontainer/compose.yml": `services:
  app:
    image: ` + baseImage + `
    command: ["sleep", "infinity"]
    configs: [sample-config]
    secrets: [sample-secret]
configs:
  sample-config:
    content: config-value
secrets:
  sample-secret:
    file: ./secret.txt
`,
		".devcontainer/secret.txt":        "secret-value\n",
		".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/"}`,
	})
	h.mustRun(h.rt.Start, folder)
	id := h.status(folder).ContainerID
	if got := h.exec(id, "cat", "/sample-config"); got != "config-value" {
		t.Errorf("config %q", got)
	}
	if got := h.exec(id, "cat", "/run/secrets/sample-secret"); got != "secret-value" {
		t.Errorf("secret %q", got)
	}
	var staged []string
	for _, m := range h.inspect(id).Mounts {
		if m.Type == "bind" && strings.HasPrefix(m.Source, staging+"/") {
			staged = append(staged, m.Source)
		}
	}
	own := filepath.Join(staging, filepath.Base(folder)) + "/"
	if len(staged) == 0 {
		t.Fatalf("no mount comes from the staging folder %s", staging)
	}
	for _, src := range staged {
		if !strings.HasPrefix(src, own) {
			t.Errorf("mount source %s is outside the workspace's staging folder %s", src, own)
		}
	}
}
