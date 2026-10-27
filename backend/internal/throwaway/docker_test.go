//go:build docker

// Throwaway containers on a real Docker Engine, whose bind sources are this
// process's filesystem, with the alpine:3.22 image. Every container a test
// creates carries the test's run label and is removed by it.
//
//	go test -tags docker ./backend/internal/throwaway/
package throwaway_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
)

const runnerImage = "alpine:3.22"

type harness struct {
	t      *testing.T
	client *dockerclient.Client
	root   string
	runID  string
	docker *throwaway.Docker
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.ImageInspect(ctx, runnerImage); err != nil {
		rc, err := client.ImagePull(ctx, runnerImage, image.PullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
	h := &harness{t: t, client: client, root: t.TempDir(), runID: random(t)}
	// Each command names its workspace to the Docker proxy, as the
	// workspace service's do.
	h.docker = throwaway.NewDocker(client, throwaway.DockerOptions{Root: h.root, User: throwaway.ServiceUser(), Labels: map[string]string{"menagerie.test.run": h.runID}, Client: dockerproxy.NewClient})
	t.Cleanup(func() {
		for _, c := range h.containers(filters.Arg("label", "menagerie.test.run="+h.runID)) {
			_ = client.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
		_ = client.Close()
	})
	return h
}

func random(t *testing.T) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func (h *harness) containers(args ...filters.KeyValuePair) []container.Summary {
	h.t.Helper()
	list, err := h.client.ContainerList(context.Background(), container.ListOptions{All: true, Filters: filters.NewArgs(args...)})
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func (h *harness) folder(name string) string {
	h.t.Helper()
	folder := filepath.Join(h.root, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		h.t.Fatal(err)
	}
	return folder
}

// console is the job console of a grant.
type console struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *console) Log(prefix string) io.Writer {
	return writer(func(b []byte) (int, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.buf.WriteString(prefix)
		c.buf.Write(b)
		return len(b), nil
	})
}

func (c *console) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type writer func([]byte) (int, error)

func (w writer) Write(b []byte) (int, error) { return w(b) }

// grant registers a generate's job for folder on a new hook runner.
func (h *harness) grant(ctx context.Context, folder string) (*throwaway.HookRunner, *throwaway.HookGrant, *console) {
	h.t.Helper()
	return h.grantOf(ctx, folder, throwaway.TargetFolder, "")
}

// grantOf registers a job for folder whose hooks run against targets of
// kind, on a new hook runner whose candidate folder is candidates.
func (h *harness) grantOf(ctx context.Context, folder string, kind throwaway.TargetKind, candidates string) (*throwaway.HookRunner, *throwaway.HookGrant, *console) {
	h.t.Helper()
	runner := throwaway.NewHookRunner(h.docker, candidates)
	out := &console{}
	g, err := runner.Grant(ctx, throwaway.HookJob{
		ID:     "job-" + random(h.t),
		Folder: folder,
		Target: kind,
		Image:  func(context.Context) (string, error) { return runnerImage, nil },
		Log:    out.Log,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return runner, g, out
}

type hookReply struct {
	status   int
	ExitCode int     `json:"exitCode"`
	Success  bool    `json:"success"`
	Stdout   *string `json:"stdout"`
	Stderr   *string `json:"stderr"`
	Error    *struct {
		Kind, Message string
	} `json:"error"`
}

func post(t *testing.T, runner *throwaway.HookRunner, body map[string]any) hookReply {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	runner.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, throwaway.HookPath, bytes.NewReader(raw)))
	var r hookReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Errorf("reply %q: %v", rec.Body.String(), err)
	}
	r.status = rec.Code
	return r
}

func hook(g *throwaway.HookGrant, folder string, program map[string]any, captureStdout bool) map[string]any {
	return map[string]any{
		"token": g.Token(), "target": folder, "program": program, "cwd": nil,
		"templateRoot": "/var/lib/sample/cache/t", "capture": map[string]bool{"stdout": captureStdout, "stderr": false},
	}
}

func decoded(t *testing.T, s *string) string {
	t.Helper()
	if s == nil {
		t.Fatal("a captured stream is null")
	}
	b, err := base64.StdEncoding.DecodeString(*s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// agentSocket listens on a Unix socket outside every workspace, as
// Wyrwood's ssh-agent.sock is, and names it in SSH_AUTH_SOCK.
func agentSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "ssh-agent.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	t.Setenv("SSH_AUTH_SOCK", path)
	return path
}

// probe reports, one line each, what a confined command must not reach:
// the Docker socket, the agent socket at $1, SSH_AUTH_SOCK, any MENAGERIE
// variable and new privileges. It then writes into its working directory.
const probe = `for p in /var/run/docker.sock /run/docker.sock "$1"; do [ -e "$p" ] && echo "reachable $p"; done
[ -n "${SSH_AUTH_SOCK:-}" ] && echo "SSH_AUTH_SOCK is set"
env | grep MENAGERIE && echo "Menagerie's environment"
grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/self/status || echo "new privileges allowed"
env > env.txt
id -u > uid.txt
echo written > written.txt
echo to-console >&2
exit 4`

// TestAHookCannotReachDockerTheAgentOrMenageriesEnvironment is the done-when
// test of template hooks: a hook that looks for the Docker socket, the
// ssh-agent socket and Menagerie's environment finds none of them and
// cannot gain privileges, while it still writes into the workspace folder
// as the services' user.
func TestAHookCannotReachDockerTheAgentOrMenageriesEnvironment(t *testing.T) {
	h := newHarness(t)
	agent := agentSocket(t)
	sentinel := random(t)
	t.Setenv("MENAGERIE_TEST_SENTINEL", sentinel)
	folder := h.folder("alpha")
	runner, g, out := h.grant(context.Background(), folder)
	r := post(t, runner, hook(g, folder, map[string]any{"run": []string{"/bin/sh", "-c", probe, "sh", agent}}, true))
	if r.status != http.StatusOK || r.ExitCode != 4 || r.Success {
		t.Fatalf("reply %+v %+v", r, r.Error)
	}
	if found := decoded(t, r.Stdout); found != "" {
		t.Fatalf("the hook reached:\n%s", found)
	}
	env, err := os.ReadFile(filepath.Join(folder, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), sentinel) || strings.Contains(string(env), "SSH_AUTH_SOCK") || strings.Contains(string(env), "DOCKER_HOST") {
		t.Fatalf("the hook saw Menagerie's environment:\n%s", env)
	}
	for file, want := range map[string]string{"written.txt": "written\n", "uid.txt": fmt.Sprintf("%d\n", os.Getuid())} {
		if got, err := os.ReadFile(filepath.Join(folder, file)); err != nil || string(got) != want {
			t.Errorf("%s %q %v", file, got, err)
		}
	}
	if !strings.Contains(out.String(), "hook: to-console\n") {
		t.Errorf("console %q", out.String())
	}
	g.Revoke()
	if left := h.containers(filters.Arg("label", "menagerie.test.run="+h.runID)); len(left) != 0 {
		t.Fatalf("%d hook containers remain", len(left))
	}
}

// TestAScriptHookRunsFromItsCopiedTemplate runs a template script: the
// template root is copied in at its own path, with its modes, for the
// services' user, and nothing of it is mounted.
func TestAScriptHookRunsFromItsCopiedTemplate(t *testing.T) {
	h := newHarness(t)
	root, err := os.MkdirTemp("", "template")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for rel, body := range map[string]string{
		"hooks/run.sh": "#!/bin/sh\nhere=$(dirname \"$0\")\necho \"script $1\" > from-script.txt\ncat \"$here/../private.txt\" >> from-script.txt\ntouch \"$here/../changed\" 2>/dev/null\nprintf captured\n",
		"private.txt":  "private\n",
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "hooks/run.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	folder := h.folder("alpha")
	runner, g, _ := h.grant(context.Background(), folder)
	body := hook(g, folder, map[string]any{"script": map[string]any{"path": filepath.Join(root, "hooks/run.sh"), "args": []string{"a b"}}}, true)
	body["templateRoot"] = root
	r := post(t, runner, body)
	if r.status != http.StatusOK || r.ExitCode != 0 || decoded(t, r.Stdout) != "captured" {
		t.Fatalf("reply %+v %+v", r, r.Error)
	}
	if got, err := os.ReadFile(filepath.Join(folder, "from-script.txt")); err != nil || string(got) != "script a b\nprivate\n" {
		t.Fatalf("from-script.txt %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "changed")); err == nil {
		t.Fatal("the hook wrote into Menagerie's template copy")
	}
}

// TestAnUpgradeHookChangesOnlyItsCandidate runs an upgrade's hook, which
// fails after it changed its candidate: the workspace folder is not in the
// container, and the candidate on disk then holds exactly what the hook
// left, links as links and entries as the services' user.
func TestAnUpgradeHookChangesOnlyItsCandidate(t *testing.T) {
	h := newHarness(t)
	folder := h.folder("alpha")
	if err := os.WriteFile(filepath.Join(folder, "live.txt"), []byte("live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	candidates := filepath.Join(base, "candidates")
	candidate := filepath.Join(candidates, ".tmpSample")
	for rel, body := range map[string]string{"marker.txt": "v1\n", "removed.txt": "x\n", "sub/kept.txt": "kept\n"} {
		path := filepath.Join(candidate, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner, g, _ := h.grantOf(context.Background(), folder, throwaway.TargetCandidate, candidates)
	script := `[ -e "$1" ] && echo "the workspace folder is in the container"
echo v2 > marker.txt
rm removed.txt
mkdir added
echo added > added/run.sh
chmod 755 added/run.sh
ln -s ../outside link
id -u > uid.txt
exit 3`
	r := post(t, runner, hook(g, candidate, map[string]any{"run": []string{"/bin/sh", "-c", script, "sh", folder}}, true))
	if r.status != http.StatusOK || r.ExitCode != 3 || decoded(t, r.Stdout) != "" {
		t.Fatalf("reply %+v %+v", r, r.Error)
	}
	want := map[string]string{
		"marker.txt": "v2\n", "sub": "/", "sub/kept.txt": "kept\n", "added": "/", "added/run.sh": "added\n",
		"link": "-> ../outside", "uid.txt": fmt.Sprintf("%d\n", os.Getuid()),
	}
	if got := tree(t, candidate); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate %v, want %v", got, want)
	}
	if info, err := os.Lstat(filepath.Join(candidate, "added/run.sh")); err != nil || info.Mode().Perm() != 0o755 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		t.Fatalf("added/run.sh %v %v", info.Mode(), err)
	}
	if got := tree(t, folder); !reflect.DeepEqual(got, map[string]string{"live.txt": "live\n"}) {
		t.Fatalf("the workspace folder changed: %v", got)
	}
	if entries, _ := os.ReadDir(candidates); len(entries) != 1 {
		t.Fatalf("the copy back left %d entries in the candidate folder", len(entries))
	}
	g.Revoke()
	if left := h.containers(filters.Arg("label", "menagerie.test.run="+h.runID)); len(left) != 0 {
		t.Fatalf("%d hook containers remain", len(left))
	}
}

// tree is folder's tree by relative path: a file's content, a folder's
// "/", a link's "-> " and its target.
func tree(t *testing.T, folder string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == folder {
			return err
		}
		rel, _ := filepath.Rel(folder, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			out[rel] = "-> " + target
			return err
		case d.IsDir():
			out[rel] = "/"
		default:
			body, err := os.ReadFile(path)
			out[rel] = string(body)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestACancelledHookLeavesNoContainerAndStopsWriting revokes a grant
// while its hook writes to the workspace folder in a loop: when Revoke
// returns, the hook's container is gone and the folder no longer changes.
func TestACancelledHookLeavesNoContainerAndStopsWriting(t *testing.T) {
	h := newHarness(t)
	folder := h.folder("alpha")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner, g, _ := h.grant(ctx, folder)
	replied := make(chan hookReply, 1)
	go func() {
		replied <- post(t, runner, hook(g, folder, map[string]any{"run": []string{"/bin/sh", "-c", "while :; do date >> ticks.txt; sleep 0.1; done"}}, false))
	}()
	ticks := filepath.Join(folder, "ticks.txt")
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(ticks); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the hook did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(h.containers(filters.Arg("label", throwaway.LabelRoot+"="+h.root))) != 1 {
		t.Fatal("the running hook has no labelled container")
	}
	cancel()
	if !g.Revoke() {
		t.Fatal("revoke timed out")
	}
	if left := h.containers(filters.Arg("label", throwaway.LabelRoot+"="+h.root)); len(left) != 0 {
		t.Fatalf("%d hook containers remain after Revoke", len(left))
	}
	before, err := os.ReadFile(ticks)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if after, _ := os.ReadFile(ticks); !bytes.Equal(before, after) {
		t.Fatal("the folder changed after Revoke returned")
	}
	if r := <-replied; r.status != http.StatusConflict || r.Error == nil || r.Error.Kind != throwaway.KindHookCancelled {
		t.Fatalf("reply %+v %+v", r, r.Error)
	}
}

// TestLeftoversOfThisRootAreRemoved removes the throwaway containers this
// workspace root left, and keeps another root's and unlabelled ones.
func TestLeftoversOfThisRootAreRemoved(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	create := func(name string, labels map[string]string) string {
		labels["menagerie.test.run"] = h.runID
		c, err := h.client.ContainerCreate(ctx, &container.Config{Image: runnerImage, Cmd: []string{"sleep", "300"}, Labels: labels}, nil, nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.client.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	ours := create("ours", map[string]string{throwaway.LabelKind: throwaway.KindHook, throwaway.LabelRoot: h.root})
	theirs := create("theirs", map[string]string{throwaway.LabelKind: throwaway.KindHook, throwaway.LabelRoot: h.root + "-other"})
	unlabelled := create("unlabelled", map[string]string{throwaway.LabelRoot: h.root})
	n, err := h.docker.RemoveLeftovers(ctx)
	if err != nil || n != 1 {
		t.Fatalf("removed %d: %v", n, err)
	}
	left := map[string]bool{}
	for _, c := range h.containers(filters.Arg("label", "menagerie.test.run="+h.runID)) {
		left[c.ID] = true
	}
	if left[ours] || !left[theirs] || !left[unlabelled] {
		t.Fatalf("left %v; ours %s theirs %s unlabelled %s", left, ours, theirs, unlabelled)
	}
}

// TestNoCapturedValueLeavesARealRun runs hooks that carry a captured value
// as their program or their linked working directory on the Engine. The
// Engine's start failure names the program; neither the refusal nor the
// console carries it, and no container remains.
func TestNoCapturedValueLeavesARealRun(t *testing.T) {
	const captured = "captured-sample-value"
	h := newHarness(t)
	folder := h.folder("alpha")
	if err := os.Symlink(t.TempDir(), filepath.Join(folder, captured)); err != nil {
		t.Fatal(err)
	}
	runner, g, out := h.grant(context.Background(), folder)
	missing := post(t, runner, hook(g, folder, map[string]any{"run": []string{"/" + captured}}, true))
	linked := hook(g, folder, map[string]any{"run": []string{"true"}}, false)
	linked["cwd"] = captured
	outside := post(t, runner, linked)
	for _, r := range []struct {
		reply hookReply
		kind  string
	}{{missing, throwaway.KindHookContainer}, {outside, throwaway.KindInvalidHook}} {
		if r.reply.Error == nil || r.reply.Error.Kind != r.kind || strings.Contains(r.reply.Error.Message, captured) {
			t.Errorf("reply %d %+v, want %s without the captured value", r.reply.status, r.reply.Error, r.kind)
		}
	}
	if strings.Contains(out.String(), captured) {
		t.Errorf("console %q", out.String())
	}
	g.Revoke()
	if left := h.containers(filters.Arg("label", "menagerie.test.run="+h.runID)); len(left) != 0 {
		t.Fatalf("%d hook containers remain", len(left))
	}
}

// TestCopiedLinksThroughTheDockerProxy copies folders that hold symbolic
// links into throwaway containers: a template with an absolute link, as a
// script hook's template root, and an upgrade candidate, a checkout of the
// workspace's committed tree, with a virtual environment's absolute link.
// A relative link that stays in its folder is the control.
func TestCopiedLinksThroughTheDockerProxy(t *testing.T) {
	h := newHarness(t)
	proxied := os.Getenv("MENAGERIE_TEST_PROXY_ROOT") != ""
	for _, tc := range []struct {
		name, link, to string
		copied         bool // a template root, else a candidate target
		refused        bool // through the proxy
	}{
		{"template-absolute", "link", "/usr/bin/env", true, true},
		{"candidate-venv-absolute", ".venv/bin/python", "/usr/bin/python3", false, true},
		{"candidate-relative", ".venv/bin/python3", "python", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := h.folder(tc.name)
			if err := os.MkdirAll(filepath.Join(src, filepath.Dir(tc.link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.to, filepath.Join(src, tc.link)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "marker"), []byte("v1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			c := throwaway.Command{Kind: throwaway.KindHook, Job: "job-" + random(t), Image: runnerImage, Argv: []string{"true"}, Env: []string{}}
			if tc.copied {
				ws := h.folder(tc.name + "-workspace")
				c.Workspace, c.Folder, c.Dir, c.Copy = ws, ws, ws, src
			} else {
				c.Workspace, c.Target, c.Dir = h.folder(tc.name+"-workspace"), src, src
			}
			code, err := h.docker.Run(context.Background(), c)
			var re *throwaway.RunError
			switch {
			case proxied && tc.refused:
				if !errors.As(err, &re) || re.Stage != throwaway.StageCopy || re.Denial == "" {
					t.Fatalf("run %d %v, want the proxy to refuse the copy", code, err)
				}
				t.Logf("refused: %v", err)
			case err != nil || code != 0:
				t.Fatalf("run %d %v, want the copy admitted", code, err)
			}
		})
	}
}
