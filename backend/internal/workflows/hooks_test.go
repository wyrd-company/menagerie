package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
)

// markerContainers stands in for throwaway containers: each hook writes a
// marker into its working directory.
type markerContainers struct{ commands []throwaway.Command }

func (m *markerContainers) Run(_ context.Context, c throwaway.Command) (int, error) {
	m.commands = append(m.commands, c)
	return 0, os.WriteFile(filepath.Join(c.Dir, "container-marker.txt"), []byte(c.Image+"\n"), 0o644)
}

// postHook sends one hook as the template service does and returns the
// reply's status and error kind.
func postHook(t *testing.T, runner *throwaway.HookRunner, token, target string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"token": token, "target": target, "program": map[string]any{"run": []string{"sh", "-c", "true"}},
		"cwd": nil, "templateRoot": "/var/lib/sample/cache/t", "capture": map[string]bool{"stdout": false, "stderr": false},
	})
	rec := httptest.NewRecorder()
	runner.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, throwaway.HookPath, bytes.NewReader(body)))
	var reply struct {
		Error struct{ Kind string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	return rec.Code, reply.Error.Kind
}

// TestHooksRunOnlyWhileTheirGenerateOrApplyRuns grants each generate and
// apply its own token: the template service's hooks run with it in the
// job's folder and the template's runner image, and it is refused once the
// request returned.
func TestHooksRunOnlyWhileTheirGenerateOrApplyRuns(t *testing.T) {
	w := newWorld(t)
	containers := &markerContainers{}
	runner := throwaway.NewHookRunner(containers)
	w.flows.opts.Hooks = runner
	w.flows.opts.RunnerImage = func(_ context.Context, id string) (string, error) { return "runner:" + id, nil }
	generate := w.tmpl.generate
	w.tmpl.generate = func(s templates.Selector) (*templates.Applied, error) {
		if status, kind := postHook(t, runner, w.tmpl.token("generate"), s.Target); status != http.StatusOK {
			t.Errorf("generate's hook: %d %s", status, kind)
		}
		return generate(s)
	}
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	if marker, err := os.ReadFile(filepath.Join(folder, "container-marker.txt")); err != nil || string(marker) != "runner:sample-v1\n" {
		t.Fatalf("marker %q %v", marker, err)
	}
	generated := w.tmpl.token("generate")
	if generated == "" {
		t.Fatal("generate carried no hook token")
	}
	if status, kind := postHook(t, runner, generated, folder); status != http.StatusForbidden || kind != throwaway.KindHookTokenRefused {
		t.Fatalf("after generate: %d %s", status, kind)
	}

	w.tmpl.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
		if status, kind := postHook(t, runner, w.tmpl.token("apply-upgrade"), s.Target); status != http.StatusOK {
			t.Errorf("apply's hook: %d %s", status, kind)
		}
		return appliedReply(templates.Written, []templates.File{{Path: "hook-marker.txt", Action: "create"}}, nil), nil
	}
	if _, snap := w.upgraded(t); snap.State != jobs.Succeeded {
		t.Fatalf("upgrade %+v %+v", snap, snap.Error)
	}
	applied := w.tmpl.token("apply-upgrade")
	if applied == "" || applied == generated {
		t.Fatalf("apply token %q, generate token %q", applied, generated)
	}
	if status, _ := postHook(t, runner, applied, folder); status != http.StatusForbidden {
		t.Fatalf("after apply: %d", status)
	}
	if len(containers.commands) != 2 || containers.commands[1].Image != "runner:sample-v2" || containers.commands[0].Job == containers.commands[1].Job {
		t.Fatalf("commands %+v", containers.commands)
	}
}

// TestAHookWithoutARunnerImageFailsAsRunnerImageUnavailable gives the
// template no runner image: the job's error kind names the image, not the
// template.
func TestAHookWithoutARunnerImageFailsAsRunnerImageUnavailable(t *testing.T) {
	w := newWorld(t)
	runner := throwaway.NewHookRunner(&markerContainers{})
	w.flows.opts.Hooks = runner
	w.flows.opts.RunnerImage = func(context.Context, string) (string, error) {
		return "", &throwaway.ImageError{Err: errors.New("no runner-image is configured")}
	}
	w.tmpl.generate = func(s templates.Selector) (*templates.Applied, error) {
		status, kind := postHook(t, runner, w.tmpl.token("generate"), s.Target)
		return nil, &templates.Error{Status: 500, Kind: "hook-io-failed", Message: "hook 0 failed: the workspace service refused the hook (" + kind + ") " + http.StatusText(status)}
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	snap := waitEnded(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != api.KindRunnerImageUnavailable || !strings.Contains(snap.Error.Message, throwaway.KindRunnerUnavailable) {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}
