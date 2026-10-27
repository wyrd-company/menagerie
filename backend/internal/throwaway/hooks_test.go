package throwaway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeContainers records each command and plays it with play.
type fakeContainers struct {
	mu       sync.Mutex
	commands []Command
	play     func(ctx context.Context, c Command) (int, error)
}

func (f *fakeContainers) Run(ctx context.Context, c Command) (int, error) {
	f.mu.Lock()
	f.commands = append(f.commands, c)
	f.mu.Unlock()
	if f.play == nil {
		return 0, nil
	}
	return f.play(ctx, c)
}

func (f *fakeContainers) ran() []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Command(nil), f.commands...)
}

// console collects the job console.
type console struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *console) Log(prefix string) io.Writer {
	return writerFunc(func(b []byte) (int, error) {
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

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

type fixture struct {
	t       *testing.T
	fake    *fakeContainers
	runner  *HookRunner
	grant   *HookGrant
	console *console
	folder  string
	// candidates is the hook runner's candidate folder, and candidate a
	// candidate target inside it.
	candidates, candidate string
	// target is the target the fixture's requests name.
	target string
	cancel context.CancelFunc
}

// newFixture grants a generate's hooks: their target is the folder.
func newFixture(t *testing.T, play func(context.Context, Command) (int, error)) *fixture {
	t.Helper()
	return newFixtureOf(t, TargetFolder, play)
}

// newFixtureOf grants hooks against targets of kind; the requests name
// the folder, or for TargetCandidate the candidate.
func newFixtureOf(t *testing.T, kind TargetKind, play func(context.Context, Command) (int, error)) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, fake: &fakeContainers{play: play}, console: &console{}, folder: filepath.Join(base, "alpha"),
		candidates: filepath.Join(base, "candidates"), candidate: filepath.Join(base, "candidates", ".tmpSample")}
	for _, d := range []string{f.folder, f.candidate} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.target = f.folder
	if kind == TargetCandidate {
		f.target = f.candidate
	}
	f.runner = NewHookRunner(f.fake, f.candidates)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	t.Cleanup(cancel)
	grant, err := f.runner.Grant(ctx, HookJob{
		ID:     "job-1",
		Folder: f.folder,
		Target: kind,
		Image:  func(context.Context) (string, error) { return "runner:sample", nil },
		Log:    f.console.Log,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.grant = grant
	return f
}

// request is a hook request body for the fixture's grant.
func (f *fixture) request(members map[string]any) map[string]any {
	body := map[string]any{
		"token":        f.grant.Token(),
		"target":       f.target,
		"program":      map[string]any{"run": []string{"sh", "-c", "echo out"}},
		"cwd":          nil,
		"templateRoot": "/var/lib/sample/cache/t",
		"capture":      map[string]bool{"stdout": false, "stderr": false},
	}
	for k, v := range members {
		body[k] = v
	}
	return body
}

type reply struct {
	status int
	body   map[string]any
	raw    string
}

func (f *fixture) post(body any) reply {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.runner.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, HookPath, bytes.NewReader(raw)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("reply %d %q is not JSON", rec.Code, rec.Body.String())
	}
	return reply{status: rec.Code, body: out, raw: rec.Body.String()}
}

func (r reply) kind() string {
	e, _ := r.body["error"].(map[string]any)
	k, _ := e["kind"].(string)
	return k
}

func TestAHookRunsInItsJobsFolderImageAndConsole(t *testing.T) {
	f := newFixture(t, func(_ context.Context, c Command) (int, error) {
		fmt.Fprint(c.Stdout, "out line\n")
		fmt.Fprint(c.Stderr, "err line\n")
		return 3, nil
	})
	if err := os.MkdirAll(filepath.Join(f.folder, "sub/dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := f.post(f.request(map[string]any{"cwd": "sub/dir"}))
	if r.status != http.StatusOK || r.body["exitCode"] != 3.0 || r.body["success"] != false || r.body["stdout"] != nil || r.body["stderr"] != nil {
		t.Fatalf("reply %d %s", r.status, r.raw)
	}
	c := f.fake.ran()[0]
	if c.Kind != KindHook || c.Job != "job-1" || c.Image != "runner:sample" || c.Folder != f.folder || c.Target != "" || c.Dir != filepath.Join(f.folder, "sub/dir") ||
		strings.Join(c.Argv, "|") != "sh|-c|echo out" || c.Copy != "" {
		t.Fatalf("command %+v", c)
	}
	if c.Env == nil || len(c.Env) != 0 {
		t.Fatalf("hook environment %#v", c.Env)
	}
	if out := f.console.String(); !strings.Contains(out, "hook: out line\n") || !strings.Contains(out, "hook: err line\n") || !strings.Contains(out, "runner:sample") {
		t.Fatalf("console %q", out)
	}
}

// TestCapturedStreamsReturnToTohaOnly captures stdout: its bytes return
// as base64 and never reach the console; stderr goes to the console.
func TestCapturedStreamsReturnToTohaOnly(t *testing.T) {
	const secret = "captured-secret-\x00\xff"
	f := newFixture(t, func(_ context.Context, c Command) (int, error) {
		fmt.Fprint(c.Stdout, secret)
		fmt.Fprint(c.Stderr, "visible\n")
		return 0, nil
	})
	r := f.post(f.request(map[string]any{"capture": map[string]bool{"stdout": true, "stderr": false}}))
	got, _ := base64.StdEncoding.DecodeString(fmt.Sprint(r.body["stdout"]))
	if r.status != http.StatusOK || string(got) != secret || r.body["stderr"] != nil || r.body["success"] != true {
		t.Fatalf("reply %d %s", r.status, r.raw)
	}
	if out := f.console.String(); strings.Contains(out, "captured-secret") || !strings.Contains(out, "hook: visible") {
		t.Fatalf("console %q", out)
	}
	f = newFixture(t, func(_ context.Context, c Command) (int, error) {
		fmt.Fprint(c.Stderr, secret)
		return 0, nil
	})
	r = f.post(f.request(map[string]any{"capture": map[string]bool{"stdout": true, "stderr": true}}))
	got, _ = base64.StdEncoding.DecodeString(fmt.Sprint(r.body["stderr"]))
	if string(got) != secret || r.body["stdout"] != "" {
		t.Fatalf("reply %s", r.raw)
	}
}

func TestUnknownAndRevokedTokensAreRefused(t *testing.T) {
	f := newFixture(t, nil)
	for name, token := range map[string]any{"unknown": strings.Repeat("0", 64), "empty": "", "absent": nil} {
		body := f.request(map[string]any{"token": token})
		if token == nil {
			delete(body, "token")
		}
		if r := f.post(body); r.status != http.StatusForbidden || r.kind() != KindHookTokenRefused {
			t.Fatalf("%s token: %d %s", name, r.status, r.raw)
		}
	}
	if r := f.post(f.request(nil)); r.status != http.StatusOK {
		t.Fatalf("granted token: %d %s", r.status, r.raw)
	}
	if !f.grant.Revoke() {
		t.Fatal("revoke timed out")
	}
	if r := f.post(f.request(nil)); r.status != http.StatusForbidden || r.kind() != KindHookTokenRefused {
		t.Fatalf("revoked token: %d %s", r.status, r.raw)
	}
	if n := len(f.fake.ran()); n != 1 {
		t.Fatalf("%d hooks ran", n)
	}
}

func TestAHookOutsideItsJobsFolderIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	if err := os.MkdirAll(filepath.Join(f.folder, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Dir(f.folder), f.folder + "-other", "alpha", "", f.folder + "/", f.folder + "/absent"} {
		if r := f.post(f.request(map[string]any{"target": target})); r.status != http.StatusForbidden || r.kind() != KindHookTokenRefused {
			t.Fatalf("target %q: %d %s", target, r.status, r.raw)
		}
	}
	if r := f.post(f.request(nil)); r.status != http.StatusOK {
		t.Fatalf("the job's folder: %d %s", r.status, r.raw)
	}
	if r := f.post(f.request(map[string]any{"target": filepath.Join(f.folder, "sub")})); r.status != http.StatusOK {
		t.Fatalf("a folder in the job's folder: %d %s", r.status, r.raw)
	}
	if c := f.fake.ran()[1]; c.Folder != f.folder || c.Dir != filepath.Join(f.folder, "sub") {
		t.Fatalf("command %+v", c)
	}
}

// TestAnUpgradeHookRunsInItsCandidateAndNeverTheFolder runs an upgrade's
// hook: its candidate is copied in and back, and the workspace folder is
// not mounted.
func TestAnUpgradeHookRunsInItsCandidateAndNeverTheFolder(t *testing.T) {
	f := newFixtureOf(t, TargetCandidate, nil)
	if err := os.MkdirAll(filepath.Join(f.candidate, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := f.post(f.request(map[string]any{"cwd": "sub"})); r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if c := f.fake.ran()[0]; c.Folder != "" || c.Target != f.candidate || c.Dir != filepath.Join(f.candidate, "sub") {
		t.Fatalf("command %+v", c)
	}
}

// TestTargetsAreBoundToTheirGrantsKind refuses a candidate target on a
// generate's grant, the workspace folder on an upgrade's, and any upgrade
// target that is not a folder inside the candidate folder by its own path.
func TestTargetsAreBoundToTheirGrantsKind(t *testing.T) {
	generate := newFixture(t, nil)
	if r := generate.post(generate.request(map[string]any{"target": generate.candidate})); r.status != http.StatusForbidden || r.kind() != KindHookTokenRefused {
		t.Fatalf("a candidate on a generate's grant: %d %s", r.status, r.raw)
	}
	upgrade := newFixtureOf(t, TargetCandidate, nil)
	base := filepath.Dir(upgrade.folder)
	elsewhere := filepath.Join(base, "elsewhere")
	linked := filepath.Join(base, "linked")
	inFolder := filepath.Join(upgrade.folder, "sub")
	for _, d := range []string{elsewhere, inFolder} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(upgrade.candidates, linked); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"the workspace folder":     upgrade.folder,
		"a folder in it":           inFolder,
		"the candidate folder":     upgrade.candidates,
		"a folder outside":         elsewhere,
		"a candidate through link": filepath.Join(linked, filepath.Base(upgrade.candidate)),
		"a missing candidate":      filepath.Join(upgrade.candidates, "absent"),
	} {
		if r := upgrade.post(upgrade.request(map[string]any{"target": target})); r.status != http.StatusForbidden || r.kind() != KindHookTokenRefused {
			t.Errorf("%s on an upgrade's grant: %d %s", name, r.status, r.raw)
		}
	}
	// A candidate folder inside the workspace folder still never admits it.
	inside := newFixtureOf(t, TargetCandidate, nil)
	inside.runner.candidates = filepath.Dir(inside.folder)
	if r := inside.post(inside.request(map[string]any{"target": inside.folder})); r.status != http.StatusForbidden {
		t.Errorf("the workspace folder inside the candidate folder: %d %s", r.status, r.raw)
	}
	if n := len(generate.fake.ran()) + len(upgrade.fake.ran()) + len(inside.fake.ran()); n != 0 {
		t.Fatalf("%d refused hooks ran", n)
	}
}

// TestAWorkingDirectoryStaysInsideItsTarget refuses a cwd that resolves
// outside the hook's target, even when it stays in the workspace or
// candidate folder.
func TestAWorkingDirectoryStaysInsideItsTarget(t *testing.T) {
	generate := newFixture(t, nil)
	sub := filepath.Join(generate.folder, "sub")
	if err := os.MkdirAll(filepath.Join(generate.folder, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../other", filepath.Join(sub, "up")); err != nil {
		t.Fatal(err)
	}
	if r := generate.post(generate.request(map[string]any{"target": sub, "cwd": "up"})); r.status != http.StatusBadRequest || r.kind() != KindInvalidHook {
		t.Fatalf("a cwd outside a generate's target: %d %s", r.status, r.raw)
	}
	upgrade := newFixtureOf(t, TargetCandidate, nil)
	sibling := filepath.Join(upgrade.candidates, "sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../sibling", filepath.Join(upgrade.candidate, "up")); err != nil {
		t.Fatal(err)
	}
	if r := upgrade.post(upgrade.request(map[string]any{"cwd": "up"})); r.status != http.StatusBadRequest || r.kind() != KindInvalidHook {
		t.Fatalf("a cwd outside an upgrade's candidate: %d %s", r.status, r.raw)
	}
	if n := len(generate.fake.ran()) + len(upgrade.fake.ran()); n != 0 {
		t.Fatalf("%d refused hooks ran", n)
	}
}

func TestInvalidHooksAreRefused(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "hooks", "run.sh")
	f := newFixture(t, nil)
	cases := map[string]map[string]any{
		"parent cwd":         {"cwd": "../beta"},
		"missing cwd":        {"cwd": "absent"},
		"absolute cwd":       {"cwd": "/etc"},
		"empty run":          {"program": map[string]any{"run": []string{}}},
		"empty program name": {"program": map[string]any{"run": []string{""}}},
		"no program":         {"program": map[string]any{}},
		"both programs":      {"program": map[string]any{"run": []string{"sh"}, "script": map[string]any{"path": script, "args": []string{}}}},
		"script outside":     {"templateRoot": root, "program": map[string]any{"script": map[string]any{"path": "/etc/passwd", "args": []string{}}}},
		"script escapes":     {"templateRoot": root, "program": map[string]any{"script": map[string]any{"path": root + "/../x.sh", "args": []string{}}}},
		"relative root":      {"templateRoot": "cache/t", "program": map[string]any{"script": map[string]any{"path": "cache/t/run.sh", "args": []string{}}}},
		"filesystem root":    {"templateRoot": "/", "program": map[string]any{"script": map[string]any{"path": "/run.sh", "args": []string{}}}},
		"missing root":       {"templateRoot": filepath.Join(root, "absent"), "program": map[string]any{"script": map[string]any{"path": filepath.Join(root, "absent", "run.sh"), "args": []string{}}}},
	}
	for name, members := range cases {
		if r := f.post(f.request(members)); r.status != http.StatusBadRequest || r.kind() != KindInvalidHook {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	for name, body := range map[string]string{"unknown member": `{"token":"x","extra":1}`, "not JSON": `{`, "trailing": `[]`} {
		rec := httptest.NewRecorder()
		f.runner.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, HookPath, strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), KindMalformedHook) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	f.runner.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HookPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	if n := len(f.fake.ran()); n != 0 {
		t.Fatalf("%d invalid hooks ran", n)
	}
}

func TestAScriptRunsWithItsTemplateCopiedIn(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "hooks", "run.sh")
	f := newFixture(t, nil)
	r := f.post(f.request(map[string]any{"templateRoot": root, "program": map[string]any{"script": map[string]any{"path": script, "args": []string{"a b"}}}}))
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	if c := f.fake.ran()[0]; c.Copy != root || strings.Join(c.Argv, "|") != script+"|a b" {
		t.Fatalf("command %+v", c)
	}
}

// TestRevokeKillsRunningHooksAndWaitsForThem revokes a grant while its
// hook runs: the hook's context ends, Revoke returns only after the
// container's removal, and the template service hears hook-cancelled.
func TestRevokeKillsRunningHooksAndWaitsForThem(t *testing.T) {
	started := make(chan struct{})
	var removed sync.WaitGroup
	removed.Add(1)
	var removedAt time.Time
	f := newFixture(t, func(ctx context.Context, c Command) (int, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // the container's removal
		removedAt = time.Now()
		removed.Done()
		return -1, fmt.Errorf("cancelled: %w", ctx.Err())
	})
	replied := make(chan reply, 1)
	go func() { replied <- f.post(f.request(nil)) }()
	<-started
	if !f.grant.Revoke() {
		t.Fatal("revoke timed out")
	}
	revokedAt := time.Now()
	if removedAt.IsZero() || revokedAt.Before(removedAt) {
		t.Fatal("Revoke returned before the hook's container was removed")
	}
	r := <-replied
	if r.status != http.StatusConflict || r.kind() != KindHookCancelled {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

// TestJobCancellationStopsItsHooks cancels the job's context: its running
// hook stops, and the template service hears hook-cancelled.
func TestJobCancellationStopsItsHooks(t *testing.T) {
	started := make(chan struct{})
	f := newFixture(t, func(ctx context.Context, _ Command) (int, error) {
		close(started)
		<-ctx.Done()
		return -1, ctx.Err()
	})
	replied := make(chan reply, 1)
	go func() { replied <- f.post(f.request(nil)) }()
	<-started
	f.cancel()
	select {
	case r := <-replied:
		if r.status != http.StatusConflict || r.kind() != KindHookCancelled {
			t.Fatalf("%d %s", r.status, r.raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hook outlived its job")
	}
	if r := f.post(f.request(nil)); r.status != http.StatusConflict || r.kind() != KindHookCancelled {
		t.Fatalf("after cancellation: %d %s", r.status, r.raw)
	}
	if n := len(f.fake.ran()); n != 1 {
		t.Fatalf("%d hooks ran", n)
	}
}

// TestACancellationWhileTheImageIsChosenIsNotAnImageFailure cancels the
// job while its runner image is looked up: the hook is cancelled, and no
// image failure is recorded for the job.
func TestACancellationWhileTheImageIsChosenIsNotAnImageFailure(t *testing.T) {
	f := newFixture(t, nil)
	f.grant.job.Image = func(ctx context.Context) (string, error) {
		f.cancel()
		<-ctx.Done()
		return "", &ImageError{Err: ctx.Err()}
	}
	if r := f.post(f.request(nil)); r.status != http.StatusConflict || r.kind() != KindHookCancelled || f.grant.ImageFailure() != nil {
		t.Fatalf("%d %s %v", r.status, r.raw, f.grant.ImageFailure())
	}
	var g *fixture
	g = newFixture(t, func(ctx context.Context, _ Command) (int, error) {
		g.cancel()
		<-ctx.Done()
		return -1, &ImageError{Image: "runner:sample", Err: errors.New("pull: cancelled")}
	})
	if r := g.post(g.request(nil)); r.status != http.StatusConflict || g.grant.ImageFailure() != nil {
		t.Fatalf("cancelled pull: %d %s", r.status, r.raw)
	}
}

func TestAMissingRunnerImageIsRecorded(t *testing.T) {
	f := newFixture(t, nil)
	f.grant.job.Image = func(context.Context) (string, error) {
		return "", &ImageError{Err: errors.New("no own container")}
	}
	if r := f.post(f.request(nil)); r.status != http.StatusServiceUnavailable || r.kind() != KindRunnerUnavailable {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	var imageErr *ImageError
	if err := f.grant.ImageFailure(); !errors.As(err, &imageErr) {
		t.Fatalf("image failure %v", err)
	}
	g := newFixture(t, func(context.Context, Command) (int, error) {
		return -1, &ImageError{Image: "runner:sample", Err: errors.New("pull: denied")}
	})
	if r := g.post(g.request(nil)); r.status != http.StatusServiceUnavailable || r.kind() != KindRunnerUnavailable || g.grant.ImageFailure() == nil {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	h := newFixture(t, func(context.Context, Command) (int, error) { return -1, errors.New("engine refused") })
	if r := h.post(h.request(nil)); r.status != http.StatusInternalServerError || r.kind() != KindHookContainer || h.grant.ImageFailure() != nil {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

// TestAFailedHookNamesItsStageInTheConsole: the job console names the
// image, the stage and the Engine error's class of a hook whose image or
// container failed; the reply carries only the kind and a fixed sentence.
func TestAFailedHookNamesItsStageInTheConsole(t *testing.T) {
	for _, tc := range []struct {
		err          error
		kind, inLine string
	}{
		{&ImageError{Image: "runner:sample", Err: &RunError{Stage: StagePull, Class: "not-found"}}, KindRunnerUnavailable,
			"a template hook found no runner image: runner image runner:sample unavailable: the runner image could not be pulled (not-found)\n"},
		{&RunError{Stage: StageStart, Class: "invalid-argument", Container: "0123456789ab"}, KindHookContainer,
			"a template hook's throwaway container failed: the throwaway container could not start (container 0123456789ab, invalid-argument)\n"},
	} {
		f := newFixture(t, func(context.Context, Command) (int, error) { return -1, tc.err })
		r := f.post(f.request(nil))
		if r.kind() != tc.kind || strings.Contains(r.raw, "runner:sample") || strings.Contains(r.raw, "0123456789ab") {
			t.Fatalf("reply %d %s", r.status, r.raw)
		}
		if out := f.console.String(); !strings.Contains(out, tc.inLine) {
			t.Fatalf("console %q, want %q", out, tc.inLine)
		}
	}
}

func TestTheHookRunnerSocketIsPrivate(t *testing.T) {
	dir, err := os.MkdirTemp("", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "run", "hook-runner.sock")
	l, err := ListenHookRunner(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", info.Mode(), err)
	}
	if _, err := ListenHookRunner(path); err == nil || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("a live socket was replaced: %v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	l, err = ListenHookRunner(path)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	l.Close()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenHookRunner(plain); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("a plain file was replaced: %v", err)
	}
}
