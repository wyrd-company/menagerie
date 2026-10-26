package throwaway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capturedValue is what an earlier hook captured. A later hook can carry
// it in its arguments or working directory, and the Engine's messages can
// repeat both.
const capturedValue = "captured-sample-value"

// TestNoEngineErrorTextLeavesThePackage fails each stage of a run with an
// Engine message that carries a captured value: the error names the stage,
// and neither it nor the console carries the value.
func TestNoEngineErrorTextLeavesThePackage(t *testing.T) {
	for stage, want := range map[string]Stage{
		"inspect": StageInspect, "pull": StagePull, "pull-stream": StagePull, "create": StageCreate,
		"copy": StageCopy, "attach": StageAttach, "start": StageStart, "wait": StageWait,
		"wait-result": StageWait, "remove": StageRemove, "list": StageCreate, "copy-back": StageCopyBack,
	} {
		t.Run(stage, func(t *testing.T) {
			e, client := newEngine(t)
			message := fmt.Sprintf("/%s/work: no such file or directory", capturedValue)
			e.fail = map[string]string{stage: message}
			if stage == "list" {
				// The search for a container whose create failed.
				e.fail["create"] = message
			}
			var log bytes.Buffer
			c := sample(t)
			c.Dir, c.Copy, c.Log = "/"+capturedValue, t.TempDir(), &log
			if stage == "copy-back" {
				c.Folder, c.Target = "", t.TempDir()
			}
			_, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), c)
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Stage != want {
				t.Fatalf("error %v, want a %s RunError", err, want)
			}
			if strings.Contains(err.Error(), capturedValue) || strings.Contains(log.String(), capturedValue) {
				t.Fatalf("the captured value left the package: error %q, console %q", err, log.String())
			}
		})
	}
}

// TestLeftoverRemovalNamesNoEngineText fails the start-up listing and
// removal of leftovers with Engine messages that carry a captured value.
func TestLeftoverRemovalNamesNoEngineText(t *testing.T) {
	message := fmt.Sprintf("/%s/work: no such file or directory", capturedValue)
	for stage, want := range map[string]Stage{"list": StageList, "remove": StageRemove} {
		t.Run(stage, func(t *testing.T) {
			e, client := newEngine(t)
			e.live["left"] = map[string]string{LabelKind: KindHook, LabelRoot: "/srv"}
			e.fail = map[string]string{stage: message}
			_, err := NewDocker(client, DockerOptions{Root: "/srv"}).RemoveLeftovers(context.Background())
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Stage != want || strings.Contains(err.Error(), capturedValue) {
				t.Fatalf("error %v, want a %s RunError without the captured value", err, want)
			}
		})
	}
}

// TestNoCapturedValueReachesAHookReplyOrTheConsole drives a captured value
// through each way a hook can fail: as a missing, symlinked or file
// working directory, in a container failure's text, and in the output of
// a hook that fails. No refusal and no console line carries it.
func TestNoCapturedValueReachesAHookReplyOrTheConsole(t *testing.T) {
	outside := t.TempDir()
	cases := []struct {
		name  string
		setup func(folder string)
		play  func(context.Context, Command) (int, error)
		body  map[string]any
		kind  string
	}{
		{name: "missing working directory", body: map[string]any{"cwd": capturedValue}, kind: KindInvalidHook},
		{
			name:  "working directory linked outside the folder",
			setup: func(folder string) { link(t, outside, filepath.Join(folder, capturedValue)) },
			body:  map[string]any{"cwd": capturedValue}, kind: KindInvalidHook,
		},
		{
			name: "working directory linked to a missing folder",
			setup: func(folder string) {
				link(t, filepath.Join(folder, "absent"), filepath.Join(folder, capturedValue))
			},
			body: map[string]any{"cwd": capturedValue}, kind: KindInvalidHook,
		},
		{
			name: "working directory that is a file",
			setup: func(folder string) {
				if err := os.WriteFile(filepath.Join(folder, capturedValue), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			body: map[string]any{"cwd": capturedValue}, kind: KindInvalidHook,
		},
		{
			name: "container failure",
			play: func(_ context.Context, c Command) (int, error) {
				return -1, fmt.Errorf("exec %s in %s: no such file or directory", c.Argv[0], c.Dir)
			},
			body: map[string]any{"program": map[string]any{"run": []string{"/" + capturedValue}}},
			kind: KindHookContainer,
		},
		{
			name: "runner image failure",
			play: func(_ context.Context, c Command) (int, error) {
				return -1, &ImageError{Image: c.Image, Err: errors.New(capturedValue)}
			},
			kind: KindRunnerUnavailable,
		},
		{
			name: "failing hook with captured output",
			play: func(_ context.Context, c Command) (int, error) {
				fmt.Fprintln(c.Stdout, capturedValue)
				return 3, nil
			},
			body: map[string]any{"capture": map[string]bool{"stdout": true, "stderr": false}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.play)
			if tc.setup != nil {
				tc.setup(f.folder)
			}
			r := f.post(f.request(tc.body))
			if tc.kind == "" {
				// The captured stream returns to Toha, and only there.
				out, _ := base64.StdEncoding.DecodeString(r.body["stdout"].(string))
				if r.status != http.StatusOK || string(out) != capturedValue+"\n" {
					t.Fatalf("reply %d %s", r.status, r.raw)
				}
			} else {
				if r.kind() != tc.kind || strings.Contains(r.raw, capturedValue) {
					t.Fatalf("reply %d %s, want %s without the captured value", r.status, r.raw, tc.kind)
				}
				if tc.play == nil && len(f.fake.ran()) != 0 {
					t.Fatal("a refused hook ran")
				}
			}
			if strings.Contains(f.console.String(), capturedValue) {
				t.Fatalf("console %q", f.console.String())
			}
		})
	}
}

// TestALinkedWorkingDirectoryInsideTheFolderRunsWhereItLeads follows a
// link to a folder inside the workspace folder: the container's working
// directory is that folder's own path under the mounted folder.
func TestALinkedWorkingDirectoryInsideTheFolderRunsWhereItLeads(t *testing.T) {
	f := newFixture(t, nil)
	if err := os.MkdirAll(filepath.Join(f.folder, "real", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	link(t, "real", filepath.Join(f.folder, "short"))
	if r := f.post(f.request(map[string]any{"cwd": "short/dir"})); r.status != http.StatusOK {
		t.Fatalf("reply %d %s", r.status, r.raw)
	}
	if c := f.fake.ran()[0]; c.Dir != filepath.Join(f.folder, "real", "dir") {
		t.Fatalf("working directory %s", c.Dir)
	}
}

// TestAWorkingDirectoryKeepsTheFoldersOwnPath runs a hook for a folder
// reached through a link: the container mounts the folder at that path,
// so the working directory is named under it, not under the link's target.
func TestAWorkingDirectoryKeepsTheFoldersOwnPath(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(t.TempDir(), "alpha")
	link(t, real, folder)
	fake := &fakeContainers{}
	runner := NewHookRunner(fake, "")
	grant, err := runner.Grant(context.Background(), HookJob{
		ID: "job-1", Folder: folder,
		Image: func(context.Context) (string, error) { return "runner:sample", nil },
		Log:   (&console{}).Log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer grant.Revoke()
	cwd := "sub"
	if _, herr := runner.run(context.Background(), &hookRequest{
		Token: grant.Token(), Target: real, Cwd: &cwd, Program: hookProgram{Run: []string{"true"}},
	}); herr != nil {
		t.Fatalf("refused: %s", herr.message)
	}
	if dir := fake.ran()[0].Dir; dir != filepath.Join(folder, "sub") {
		t.Fatalf("working directory %s", dir)
	}
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}
