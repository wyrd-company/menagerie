package devcontainer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/wyrd-company/devcontainer-builder/pkg/spec"
	"github.com/wyrd-company/devcontainer-builder/pkg/substitute"

	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
)

// writeDefinition writes .devcontainer/devcontainer.json in a new
// workspace folder and loads it with the library.
func writeDefinition(t *testing.T, body string) (*spec.DevContainer, string) {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "sample-space")
	if err := os.MkdirAll(filepath.Join(folder, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, ".devcontainer", "devcontainer.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		t.Fatal(err)
	}
	return dc, folder
}

func TestInitializeCommandForms(t *testing.T) {
	cases := []struct {
		name, field string
		want        spec.Command
	}{
		{"absent", ``, spec.Command{}},
		{"null", `"initializeCommand": null,`, spec.Command{}},
		{"string", `"initializeCommand": "echo hi", // comment`, spec.Command{Shell: "echo hi"}},
		{"array", `"initializeCommand": ["echo", "a b"],`, spec.Command{Args: []string{"echo", "a b"}}},
		{"object", `"initializeCommand": {"one": "echo 1", "two": ["echo", "2"]},`, spec.Command{Parallel: map[string]spec.Command{
			"one": {Shell: "echo 1"}, "two": {Args: []string{"echo", "2"}},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dc, _ := writeDefinition(t, "{\n"+tc.field+"\n\"image\": \"alpine:3.22\"\n}")
			got, err := InitializeCommand(dc)
			if err != nil {
				t.Fatal(err)
			}
			if !commandsEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestInitializeCommandRejectsUnsupportedValue(t *testing.T) {
	dc, _ := writeDefinition(t, `{"image": "alpine:3.22", "initializeCommand": 3}`)
	if _, err := InitializeCommand(dc); err == nil || !strings.Contains(err.Error(), "initializeCommand") {
		t.Fatalf("got %v", err)
	}
}

func commandsEqual(a, b spec.Command) bool {
	if a.Shell != b.Shell || strings.Join(a.Args, "\x00") != strings.Join(b.Args, "\x00") || len(a.Parallel) != len(b.Parallel) {
		return false
	}
	for k, v := range a.Parallel {
		if w, ok := b.Parallel[k]; !ok || !commandsEqual(v, w) {
			return false
		}
	}
	return true
}

func TestSubstituteCommandResolvesEveryForm(t *testing.T) {
	dc, folder := writeDefinition(t, `{"image": "alpine:3.22", "workspaceFolder": "/work/${localWorkspaceFolderBasename}"}`)
	env := map[string]string{"SAMPLE_VALUE": "v1"}
	ctx := SubstitutionContext(dc, folder, env)
	cmd := spec.Command{Parallel: map[string]spec.Command{
		"shell": {Shell: "echo ${localWorkspaceFolder} ${localEnv:SAMPLE_VALUE} ${containerWorkspaceFolder}"},
		"argv":  {Args: []string{"echo", "${devcontainerId}", "${containerEnv:LATER}"}},
	}}
	got := SubstituteCommand(ctx, cmd)
	id := substitute.Substitute(substitute.Context{IDLabels: map[string]string{
		"devcontainer.local_folder": folder,
		"devcontainer.config_file":  filepath.Join(folder, ".devcontainer", "devcontainer.json"),
	}}, "${devcontainerId}")
	if strings.Contains(id, "$") {
		t.Fatalf("expected id resolved, got %s", id)
	}
	if want := "echo " + folder + " v1 /work/sample-space"; got.Parallel["shell"].Shell != want {
		t.Errorf("shell %q want %q", got.Parallel["shell"].Shell, want)
	}
	if args := got.Parallel["argv"].Args; len(args) != 3 || args[1] != id || args[2] != "${containerEnv:LATER}" {
		t.Errorf("argv %q", args)
	}
	if cmd.Parallel["shell"].Shell == got.Parallel["shell"].Shell {
		t.Error("input command was modified in place")
	}
}

func TestContainerWorkspaceDefaults(t *testing.T) {
	single, folder := writeDefinition(t, `{"image": "alpine:3.22"}`)
	if got := containerWorkspace(single, folder, nil); got != "/workspaces/sample-space" {
		t.Errorf("single %q", got)
	}
	compose, folder := writeDefinition(t, `{"dockerComposeFile": "compose.yml", "service": "app"}`)
	if got := containerWorkspace(compose, folder, nil); got != "/" {
		t.Errorf("compose %q", got)
	}
}

// recorder collects command output per prefix.
type recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recorder) writer(prefix string) io.Writer { return prefixWriter{r: r, prefix: prefix} }

type prefixWriter struct {
	r      *recorder
	prefix string
}

func (w prefixWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	w.r.buf.WriteString(w.prefix)
	w.r.buf.Write(p)
	return len(p), nil
}

// localContainers stands in for throwaway containers: it runs each
// command as a local process in its working directory with exactly its
// environment, and records the commands.
type localContainers struct {
	mu       sync.Mutex
	commands []throwaway.Command
}

func (l *localContainers) Run(ctx context.Context, c throwaway.Command) (int, error) {
	l.mu.Lock()
	l.commands = append(l.commands, c)
	l.mu.Unlock()
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = c.Dir, append(c.Env, "PATH="+os.Getenv("PATH")), c.Stdout, c.Stderr
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func newRunner(dir string, env []string, output func(string) io.Writer) (CommandRunner, *localContainers) {
	l := &localContainers{}
	return CommandRunner{Containers: l, Image: "runner:sample", Folder: dir, Env: env, Job: "job-1", Output: output}, l
}

func TestCommandRunnerShellRunsInTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{}
	runner, l := newRunner(dir, []string{"SAMPLE_VALUE=v1"}, rec.writer)
	if err := runner.Run(context.Background(), spec.Command{Shell: `pwd > where; echo "$SAMPLE_VALUE"`}); err != nil {
		t.Fatal(err)
	}
	where, _ := os.ReadFile(filepath.Join(dir, "where"))
	if strings.TrimSpace(string(where)) != dir {
		t.Errorf("cwd %q", where)
	}
	if rec.buf.String() != "v1\n" {
		t.Errorf("output %q", rec.buf.String())
	}
	c := l.commands[0]
	if c.Kind != throwaway.KindInitializeCommand || c.Image != "runner:sample" || c.Folder != dir || c.Dir != dir || c.Job != "job-1" ||
		strings.Join(c.Argv, " ") != `/bin/sh -c pwd > where; echo "$SAMPLE_VALUE"` || strings.Join(c.Env, ",") != "SAMPLE_VALUE=v1" || c.Copy != "" {
		t.Errorf("command %+v", c)
	}
}

func TestCommandRunnerArgvSkipsTheShell(t *testing.T) {
	dir := t.TempDir()
	runner, l := newRunner(dir, nil, nil)
	// With a shell, "; touch injected" would run as a second command.
	err := runner.Run(context.Background(), spec.Command{Args: []string{"touch", "a b; touch injected"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a b; touch injected")); err != nil {
		t.Errorf("argv not passed literally: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "injected")); err == nil {
		t.Error("argv ran through a shell")
	}
	if c := l.commands[0]; strings.Join(c.Argv, "|") != "touch|a b; touch injected" || c.Env == nil || len(c.Env) != 0 {
		t.Errorf("command %q env %q", c.Argv, c.Env)
	}
}

// TestCommandRunnerParallelMembersRunConcurrently makes member "a" wait for
// a file member "b" writes; sequential execution in name order fails.
func TestCommandRunnerParallelMembersRunConcurrently(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{}
	runner, l := newRunner(dir, nil, rec.writer)
	cmd := spec.Command{Parallel: map[string]spec.Command{
		"a": {Shell: `i=0; while [ ! -e from-b ]; do i=$((i+1)); [ $i -gt 200 ] && exit 9; sleep 0.05; done; echo saw-b`},
		"b": {Args: []string{"sh", "-c", "touch from-b; echo wrote-b"}},
	}}
	if err := runner.Run(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	out := rec.buf.String()
	if !strings.Contains(out, "[a] saw-b") || !strings.Contains(out, "[b] wrote-b") {
		t.Fatalf("output %q", out)
	}
	if len(l.commands) != 2 {
		t.Fatalf("members ran in %d containers", len(l.commands))
	}
}

func TestCommandRunnerWaitsForAllMembersAndReportsFailures(t *testing.T) {
	dir := t.TempDir()
	runner, _ := newRunner(dir, nil, nil)
	cmd := spec.Command{Parallel: map[string]spec.Command{
		"fails":    {Shell: "exit 4"},
		"finishes": {Shell: "sleep 0.2; touch finished"},
	}}
	err := runner.Run(context.Background(), cmd)
	if err == nil || !strings.Contains(err.Error(), "fails: exit status 4") {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "finished")); statErr != nil {
		t.Errorf("other member did not complete: %v", statErr)
	}
}

// TestCancelledCommandReturns cancels a running command: the runner
// returns the containers' cancellation error.
func TestCancelledCommandReturns(t *testing.T) {
	runner, _ := newRunner(t.TempDir(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runner.Run(ctx, spec.Command{Shell: "sleep 20"})
	if elapsed := time.Since(start); err == nil || elapsed > 5*time.Second {
		t.Fatalf("returned %v after %v", err, elapsed)
	}
}

type progress struct{ rec recorder }

func (p *progress) Phase(string)                {}
func (p *progress) Log(prefix string) io.Writer { return p.rec.writer(prefix) }

// TestStartFailsAtInitializeCommandBeforeTheDevcontainer uses a Docker
// client for an unreachable daemon: any devcontainer call before
// initializeCommand would report a connection error instead of an
// InitializeError. initializeCommand gets the allowed variables only, and
// the runner image of the workspace.
func TestStartFailsAtInitializeCommandBeforeTheDevcontainer(t *testing.T) {
	_, folder := writeDefinition(t, `{"image": "alpine:3.22", "initializeCommand": "echo marker > init-ran; exit 3"}`)
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+filepath.Join(t.TempDir(), "absent.sock")), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	l := &localContainers{}
	rt := New(client, Options{
		Environ:     func() []string { return []string{"PATH=/menagerie/bin", "SAMPLE_ALLOWED=a1", "SAMPLE_SECRET=s1"} },
		LocalEnv:    []string{"SAMPLE_ALLOWED"},
		Containers:  l,
		RunnerImage: func(context.Context) (string, error) { return "runner:sample", nil },
	})
	for _, op := range []func(context.Context, string, Progress) error{rt.Start, rt.Rebuild} {
		p := &progress{}
		err := op(context.Background(), folder, p)
		var initErr *InitializeError
		if !errors.As(err, &initErr) || !strings.Contains(err.Error(), "exit status 3") {
			t.Fatalf("got %v", err)
		}
		if _, err := os.Stat(filepath.Join(folder, "init-ran")); err != nil {
			t.Fatalf("initializeCommand did not run in the workspace: %v", err)
		}
	}
	for _, c := range l.commands {
		if c.Image != "runner:sample" || strings.Join(c.Env, ",") != "SAMPLE_ALLOWED=a1" {
			t.Fatalf("command image %s env %q", c.Image, c.Env)
		}
	}
}

func TestInitializeCommandWithoutARunnerImageFails(t *testing.T) {
	_, folder := writeDefinition(t, `{"image": "alpine:3.22", "initializeCommand": "true"}`)
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+filepath.Join(t.TempDir(), "absent.sock")), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	l := &localContainers{}
	missing := &throwaway.ImageError{Err: errors.New("no image")}
	rt := New(client, Options{Containers: l, RunnerImage: func(context.Context) (string, error) { return "", missing }})
	err = rt.Start(context.Background(), folder, &progress{})
	var imageErr *throwaway.ImageError
	if !errors.As(err, &imageErr) || len(l.commands) != 0 {
		t.Fatalf("got %v after %d commands", err, len(l.commands))
	}
	if err := New(client, Options{}).Start(context.Background(), folder, &progress{}); err == nil || !strings.Contains(err.Error(), "no container runner") {
		t.Fatalf("without containers: %v", err)
	}
}

func TestImageNameMatchesDevcontainerBuilderDefault(t *testing.T) {
	// Vector from devcontainer-builder pkg/cli/config_test.go.
	if got := ImageName("/workspace"); got != "dcb-c52ddf65534b7b46:latest" {
		t.Fatalf("got %s", got)
	}
}

func TestNeedsBuild(t *testing.T) {
	cases := map[string]bool{
		`{"image": "alpine:3.22"}`: false,
		`{"image": "alpine:3.22", "features": {"ghcr.io/example/feature:1": {}}}`: true,
		`{"build": {"dockerfile": "Dockerfile"}}`:                                 true,
		`{"dockerFile": "Dockerfile"}`:                                            true,
	}
	for body, want := range cases {
		dc, _ := writeDefinition(t, body)
		if got := needsBuild(dc); got != want {
			t.Errorf("%s: got %t", body, got)
		}
	}
}

func TestFolderOwnerUsesTheFolderOwner(t *testing.T) {
	dir := t.TempDir()
	upd := folderOwner(dir)
	if upd == nil || upd.HostUID != os.Getuid() || upd.HostGID != os.Getgid() {
		t.Fatalf("got %+v", upd)
	}
	if folderOwner(filepath.Join(dir, "missing")) != nil {
		t.Fatal("missing folder produced an update")
	}
}

// TestTheWorkspaceSocketNeverReachesInitializeCommand gives the runtime a
// workspace socket: initializeCommand's throwaway container still has only
// the workspace folder and the allowed variables, so the socket is the
// devcontainer's alone.
func TestTheWorkspaceSocketNeverReachesInitializeCommand(t *testing.T) {
	_, folder := writeDefinition(t, `{"image": "alpine:3.22", "initializeCommand": "exit 3"}`)
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+filepath.Join(t.TempDir(), "absent.sock")), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	sockets := filepath.Join(t.TempDir(), "sockets")
	var asked []string
	l := &localContainers{}
	rt := New(client, Options{
		Environ:     func() []string { return []string{"SAMPLE_ALLOWED=a1"} },
		LocalEnv:    []string{"SAMPLE_ALLOWED"},
		Containers:  l,
		RunnerImage: func(context.Context) (string, error) { return "runner:sample", nil },
		WorkspaceSocket: func(f string) (string, error) {
			asked = append(asked, f)
			return filepath.Join(sockets, filepath.Base(f)), nil
		},
	})
	if err := rt.Start(context.Background(), folder, &progress{}); err == nil {
		t.Fatal("initializeCommand did not fail")
	}
	if len(asked) != 1 || asked[0] != folder {
		t.Fatalf("socket asked for %v", asked)
	}
	if len(l.commands) != 1 {
		t.Fatalf("%d commands", len(l.commands))
	}
	c := l.commands[0]
	if c.Folder != folder || c.Copy != "" || strings.Join(c.Env, ",") != "SAMPLE_ALLOWED=a1" {
		t.Fatalf("command folder %s copy %s env %q", c.Folder, c.Copy, c.Env)
	}
}

// TestAWorkspaceSocketFailureStopsTheStart fails the socket: the start
// fails before initializeCommand or the devcontainer.
func TestAWorkspaceSocketFailureStopsTheStart(t *testing.T) {
	_, folder := writeDefinition(t, `{"image": "alpine:3.22", "initializeCommand": "touch init-ran"}`)
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+filepath.Join(t.TempDir(), "absent.sock")), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("workspace socket refused")
	l := &localContainers{}
	rt := New(client, Options{
		Containers:      l,
		RunnerImage:     func(context.Context) (string, error) { return "runner:sample", nil },
		WorkspaceSocket: func(string) (string, error) { return "", refused },
	})
	if err := rt.Start(context.Background(), folder, &progress{}); !errors.Is(err, refused) || len(l.commands) != 0 {
		t.Fatalf("got %v after %d commands", err, len(l.commands))
	}
}
