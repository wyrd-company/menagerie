package workflows

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// upgraded runs an upgrade of alpha to completion of its interview and
// returns the job when it ends.
func (w *world) upgraded(t *testing.T) (*jobs.Job, jobs.Snapshot) {
	t.Helper()
	job := w.startJob(t, "upgrade", "alpha", "sample-v2")
	waitWaiting(t, job, 0)
	if status, body := w.answer(t, job, sessionAsking); status != http.StatusOK {
		t.Fatalf("answer %d %s", status, body)
	}
	return job, waitEnded(t, job)
}

func TestUpgradeBacksUpLocalChangesBeforeTheInterview(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	writeFile(t, folder, "local.txt", "first: operator\n")
	writeFile(t, folder, "notes.txt", "untracked operator notes\n")

	var atStart []string
	w.tmpl.startUpgrade = func(templates.Selector) (*templates.Reply, error) {
		// The tracked edit is committed and pushed before Toha is asked.
		history := w.remoteHistory(t, "alpha")
		atStart = []string{fileAt(t, history[0], "local.txt"), fileAt(t, history[0], "notes.txt")}
		if len(history) != 2 || history[0].ParentHashes[0] != history[1].Hash {
			t.Errorf("pre-upgrade backup history %v", history)
		}
		// Toha's own clean check would refuse the untracked file; this
		// fixture removes it as the operator would.
		_ = os.Remove(filepath.Join(folder, "notes.txt"))
		return reply(templates.Questions, sessionAsking, batch), nil
	}
	job, snap := w.upgraded(t)
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if !reflect.DeepEqual(atStart, []string{"first: operator\n", ""}) {
		t.Fatalf("remote at the interview start: %q", atStart)
	}
	if got := w.tmpl.seen()[3:]; !reflect.DeepEqual(got, []string{"start-upgrade", "answer-upgrade", "apply-upgrade"}) {
		t.Fatalf("template calls %v", got)
	}
	if w.tmpl.session("apply-upgrade") != sessionDone {
		t.Fatalf("apply session %s", w.tmpl.session("apply-upgrade"))
	}
	history := w.remoteHistory(t, "alpha")
	if len(history) != 3 || fileAt(t, history[0], "added.txt") != "new in v2\n" || fileAt(t, history[0], ".toha.json") != `{"applications":[{"v":2}]}` || fileAt(t, history[0], "local.txt") != "first: operator\n" {
		t.Fatalf("upgraded backup %v", history)
	}
	if history[2].NumParents() != 0 {
		t.Fatal("the branch lost its independent root")
	}
	if got := w.rt.seen(); !reflect.DeepEqual(got, []string{"up alpha", "rebuild alpha"}) {
		t.Fatalf("runtime %v", got)
	}
	if !strings.HasPrefix(result(job), `{"state":"succeeded","error":null,"apply":{"outcome":"written"`) {
		t.Fatalf("result %s", result(job))
	}
}

func TestAnUpgradeWithoutLocalChangesStillPushes(t *testing.T) {
	w := newWorld(t)
	// The create's backup fails: the remote branch is missing.
	w.flows.opts.Remote.URL = filepath.Join(t.TempDir(), "absent.git")
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	if snap := waitEnded(t, job); snap.Error == nil || snap.Error.Kind != KindBackupFailed {
		t.Fatalf("%+v", snap)
	}
	w.flows.opts.Remote.URL = w.remote
	if w.remoteHistory(t, "alpha") != nil {
		t.Fatal("remote branch exists")
	}
	var atStart int
	w.tmpl.startUpgrade = func(templates.Selector) (*templates.Reply, error) {
		atStart = len(w.remoteHistory(t, "alpha"))
		return reply(templates.Questions, sessionAsking, batch), nil
	}
	job, snap := w.upgraded(t)
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if !strings.Contains(logText(job), "no local changes to tracked files") || atStart != 1 {
		t.Fatalf("pushed %d commits before the interview; log:\n%s", atStart, logText(job))
	}
	if history := w.remoteHistory(t, "alpha"); len(history) != 2 || history[1].NumParents() != 0 {
		t.Fatalf("history %v", history)
	}
}

func TestAConflictedUpgradeIsBackedUpButNotRebuilt(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	w.tmpl.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
		writeFile(t, s.Target, "local.txt", "<<<<<<< ours\nfirst: operator\n=======\nfirst: v2\n>>>>>>> theirs\n")
		writeFile(t, s.Target, ".toha.json", `{"applications":[{"v":2}]}`)
		return appliedReply(templates.Conflicted, []templates.File{{Path: "local.txt", Action: "conflicted"}}, []string{"local.txt"}), nil
	}
	job, snap := w.upgraded(t)
	if snap.State != jobs.Failed || snap.Error.Kind != KindMergeConflicted || !strings.Contains(snap.Error.Message, "merge conflict markers into local.txt") || !strings.Contains(snap.Error.Message, "not rebuilt") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if !strings.Contains(result(job), `"apply":{"outcome":"conflicted"`) || !strings.Contains(result(job), `"conflicts":["local.txt"]`) {
		t.Fatalf("result %s", result(job))
	}
	if got := w.rt.seen(); !reflect.DeepEqual(got, []string{"up alpha"}) {
		t.Fatalf("rebuilt a conflicted workspace: %v", got)
	}
	if history := w.remoteHistory(t, "alpha"); !strings.Contains(fileAt(t, history[0], "local.txt"), "<<<<<<<") {
		t.Fatal("the conflicted state was not backed up")
	}
}

func TestAnAlreadyCurrentUpgradeChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	w.tmpl.startUpgrade = func(templates.Selector) (*templates.Reply, error) {
		return reply(templates.Completed, sessionDone, "null"), nil
	}
	w.tmpl.applyUpgrade = func(templates.Selector) (*templates.Applied, error) {
		return appliedReply(templates.AlreadyCurrent, nil, nil), nil
	}
	job := w.startJob(t, "upgrade", "alpha", "sample-v1")
	snap := waitEnded(t, job)
	if snap.State != jobs.Succeeded || !strings.Contains(logText(job), "already current") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if len(w.remoteHistory(t, "alpha")) != 1 || len(w.rt.seen()) != 1 {
		t.Fatal("an already-current upgrade committed or rebuilt")
	}
}

func TestUpgradeFailuresAreVisibleAndReleaseTheSlot(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, w *world)
		kind    string
		message string
		pushes  int
		// before is true when the job fails before its interview.
		before bool
	}{
		{"pre-upgrade backup", func(t *testing.T, w *world) {
			w.flows.opts.Remote.URL = filepath.Join(t.TempDir(), "absent.git")
		}, KindBackupFailed, "nothing was upgraded", 1, true},
		{"refused start", func(t *testing.T, w *world) {
			w.tmpl.startUpgrade = func(templates.Selector) (*templates.Reply, error) {
				return nil, &templates.Error{Status: 409, Kind: "dirty-target", Message: "the workspace has uncommitted changes"}
			}
		}, KindTemplateRefused, "(dirty-target)", 1, true},
		{"failed apply", func(t *testing.T, w *world) {
			w.tmpl.applyUpgrade = func(templates.Selector) (*templates.Applied, error) {
				return nil, &templates.Error{Status: 422, Kind: "hook-failed", Message: "hook exited 3"}
			}
		}, KindUpgradeFailed, "the upgrade failed (hook-failed)", 1, false},
		{"needs trust", func(t *testing.T, w *world) {
			w.tmpl.applyUpgrade = func(templates.Selector) (*templates.Applied, error) {
				return appliedReply(templates.NeedsTrust, nil, nil), nil
			}
		}, KindNotApplied, "reported needs-trust and wrote nothing", 1, false},
		{"backup after the apply", func(t *testing.T, w *world) {
			apply := w.tmpl.applyUpgrade
			w.tmpl.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
				w.flows.opts.Remote.URL = filepath.Join(t.TempDir(), "absent.git")
				return apply(s)
			}
		}, KindBackupFailed, "its backup failed", 1, false},
		{"rebuild", func(t *testing.T, w *world) {
			w.rt.fail = &devcontainer.InitializeError{Err: errors.New("exit status 3")}
		}, "initialize-command-failed", "upgraded and backed up as", 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.createWorkspace(t, "alpha")
			tc.prepare(t, w)
			job := w.startJob(t, "upgrade", "alpha", "sample-v2")
			var snap jobs.Snapshot
			if tc.before {
				snap = waitEnded(t, job)
			} else {
				waitWaiting(t, job, 0)
				w.answer(t, job, sessionAsking)
				snap = waitEnded(t, job)
			}
			if snap.State != jobs.Failed || snap.Error.Kind != tc.kind || !strings.Contains(snap.Error.Message, tc.message) {
				t.Fatalf("%+v %+v", snap, snap.Error)
			}
			if _, ok := w.manager.Active("alpha"); ok {
				t.Fatal("the failed upgrade kept the slot")
			}
			for _, call := range w.rt.seen() {
				if call == "rebuild alpha" && tc.kind != "initialize-command-failed" {
					t.Fatal("rebuilt after a failed upgrade")
				}
			}
			w.flows.opts.Remote.URL = w.remote
			if got := len(w.remoteHistory(t, "alpha")); got != tc.pushes {
				t.Fatalf("%d backup commits", got)
			}
		})
	}
}

func TestAnUpgradeWaitingForAnswersHoldsOnlyItsWorkspace(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	job := w.startJob(t, "upgrade", "alpha", "sample-v2")
	waitWaiting(t, job, 0)
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"upgrade","workspace":"alpha","templateId":"sample-v2"}`); status != 409 || !strings.Contains(body, "workspace-busy") {
		t.Fatalf("%d %s", status, body)
	}
	w.createWorkspace(t, "beta")
	submission := json.RawMessage(`{"template":"sample@1","answers":{}}`)
	if _, err := w.flows.Answer(t.Context(), "absent", json.RawMessage(`{}`), submission); err == nil {
		t.Fatal("answer to an unknown interview accepted")
	}
}
