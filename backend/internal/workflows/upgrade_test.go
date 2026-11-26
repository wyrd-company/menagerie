package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// upgraded runs an upgrade of alpha to completion of its interview and
// returns the job when it ends.
func (w *world) upgraded(t *testing.T) (*jobs.Job, jobs.Snapshot) {
	t.Helper()
	job := w.startJob(t, "upgrade", "alpha", "")
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
	if len(history) != 3 || fileAt(t, history[0], "added.txt") != "new in v2\n" || !strings.Contains(fileAt(t, history[0], ".toha.json"), `@v2.0.0"`) || fileAt(t, history[0], "local.txt") != "first: operator\n" {
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
	w.backups.URL = filepath.Join(t.TempDir(), "absent.git")
	job := w.startJob(t, "create", "alpha", "sample")
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	if snap := waitEnded(t, job); snap.Error == nil || snap.Error.Kind != KindBackupFailed {
		t.Fatalf("%+v", snap)
	}
	w.backups.URL = w.remote
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
	if snap.State != jobs.Failed || snap.Error.Kind != KindMergeConflicted {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	history := w.remoteHistory(t, "alpha")
	for _, part := range []string{
		"the upgrade is not complete",
		"merge conflict markers into local.txt, which need manual resolution",
		"committed and backed up as " + history[0].Hash.String(),
		"the state before the upgrade as " + history[1].Hash.String(),
		"the devcontainer was not rebuilt: resolve the conflicts, then rebuild",
	} {
		if !strings.Contains(snap.Error.Message, part) {
			t.Errorf("message lacks %q: %s", part, snap.Error.Message)
		}
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
	job := w.startJob(t, "upgrade", "alpha", "")
	snap := waitEnded(t, job)
	if snap.State != jobs.Succeeded || !strings.Contains(logText(job), "already current") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if len(w.remoteHistory(t, "alpha")) != 1 || len(w.rt.seen()) != 1 {
		t.Fatal("an already-current upgrade committed or rebuilt")
	}
}

func TestAReansweredUpgradeAsksTheTemplateServiceToReanswer(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	w.tmpl.startUpgrade = func(templates.Selector) (*templates.Reply, error) {
		return reply(templates.Completed, sessionDone, "null"), nil
	}
	w.tmpl.applyUpgrade = func(templates.Selector) (*templates.Applied, error) {
		return appliedReply(templates.AlreadyCurrent, nil, nil), nil
	}
	w.newVersion(t)
	for _, body := range []string{
		`{"operation":"upgrade","workspace":"alpha","reanswer":true}`,
		`{"operation":"upgrade","workspace":"alpha"}`,
	} {
		// Toha's already-current records the pinned version; the next
		// upgrade starts from the first version again.
		if err := w.data.SetAppliedVersion(context.Background(), "alpha", store.AppliedVersion{Version: "1.0.0", Commit: sampleTemplate.CurrentCommit}); err != nil {
			t.Fatal(err)
		}
		status, reply := w.do(t, http.MethodPost, "/api/jobs", body)
		if status != http.StatusAccepted {
			t.Fatalf("start: %d %s", status, reply)
		}
		var snap jobs.Snapshot
		_ = json.Unmarshal([]byte(reply), &snap)
		job, _ := w.manager.Get(snap.JobID)
		if ended := waitEnded(t, job); ended.State != jobs.Succeeded {
			t.Fatalf("%+v %+v", ended, ended.Error)
		}
		asked := strings.Contains(logText(job), "asking every question again")
		if asked != strings.Contains(body, "reanswer") {
			t.Fatalf("%s logged:\n%s", body, logText(job))
		}
	}
	w.tmpl.mu.Lock()
	defer w.tmpl.mu.Unlock()
	if !reflect.DeepEqual(w.tmpl.reanswered, []bool{true, false}) {
		t.Fatalf("reanswer of the upgrade starts %v", w.tmpl.reanswered)
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
			w.backups.URL = filepath.Join(t.TempDir(), "absent.git")
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
				w.backups.URL = filepath.Join(t.TempDir(), "absent.git")
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
			job := w.startJob(t, "upgrade", "alpha", "")
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
			w.backups.URL = w.remote
			if got := len(w.remoteHistory(t, "alpha")); got != tc.pushes {
				t.Fatalf("%d backup commits", got)
			}
		})
	}
}

func TestAnUpgradeWaitingForAnswersHoldsOnlyItsWorkspace(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	job := w.startJob(t, "upgrade", "alpha", "")
	waitWaiting(t, job, 0)
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"upgrade","workspace":"alpha"}`); status != 409 || !strings.Contains(body, "workspace-busy") {
		t.Fatalf("%d %s", status, body)
	}
	w.createWorkspace(t, "beta")
	submission := json.RawMessage(`{"template":"sample@1","answers":{}}`)
	if _, err := w.flows.Answer(t.Context(), "absent", json.RawMessage(`{}`), submission); err == nil {
		t.Fatal("answer to an unknown interview accepted")
	}
}

func TestAnUpgradeAppliesItsTemplatesCurrentVersionAndMovesOnlyForward(t *testing.T) {
	w := newWorld(t)
	var created, upgraded templates.Selector
	generate := w.tmpl.generate
	w.tmpl.generate = func(s templates.Selector) (*templates.Applied, error) {
		created = s
		return generate(s)
	}
	w.createWorkspace(t, "alpha")
	if created.Address != sampleTemplate.Address || created.Ref != "1.0.0" || created.Commit != sampleTemplate.CurrentCommit {
		t.Fatalf("create pinned %+v", created)
	}
	// The workspace has the template's current version.
	status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"upgrade","workspace":"alpha"}`)
	if status != http.StatusConflict || !strings.Contains(body, `"kind":"`+KindUpgradeNotNeeded+`"`) || !strings.Contains(body, "1.0.0") {
		t.Fatalf("%d %s", status, body)
	}
	apply := w.tmpl.applyUpgrade
	w.tmpl.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
		upgraded = s
		return apply(s)
	}
	if _, snap := w.upgraded(t); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if upgraded.Address != sampleTemplate.Address || upgraded.Ref != "v2.0.0" || upgraded.Commit != "2000000000000000000000000000000000000002" {
		t.Fatalf("upgrade pinned %+v", upgraded)
	}
	// Its record now names the current version, so a second upgrade is not needed.
	status, body = w.do(t, http.MethodPost, "/api/jobs", `{"operation":"upgrade","workspace":"alpha"}`)
	if status != http.StatusConflict || !strings.Contains(body, KindUpgradeNotNeeded) {
		t.Fatalf("%d %s", status, body)
	}
}

// An upgrade compares the applied version Menagerie keeps, not the
// record's ref: a record edited to name a branch changes nothing.
func TestAnUpgradeComparesTheStoredAppliedVersion(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	writeFile(t, folder, ".toha.json", appliedRecord(templates.Selector{Address: sampleTemplate.Address, Ref: "main"}))
	status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"upgrade","workspace":"alpha"}`)
	if status != http.StatusConflict || !strings.Contains(body, `"kind":"`+KindUpgradeNotNeeded+`"`) || !strings.Contains(body, "version 1.0.0") {
		t.Fatalf("%d %s", status, body)
	}
}

// An upgrade of a workspace whose applied version is unknown goes on only
// when the template service answers that the current version's commit
// descends from the applied commit; otherwise it fails as template-refused
// before the interview, naming both commits.
func TestAnUnknownAppliedVersionIsUpgradedOnlyByDescent(t *testing.T) {
	const applied = "0000000000000000000000000000000000000abc"
	current := "2000000000000000000000000000000000000002"
	cases := []struct {
		name    string
		applied store.AppliedVersion
		answer  func(address, ref, since string) (*templates.Checked, error)
		kind    string
		names   []string
	}{
		{"off the history", store.AppliedVersion{Commit: applied}, func(_, _, _ string) (*templates.Checked, error) {
			return &templates.Checked{Source: sampleTemplate.Source, Commit: current, Definition: true, Descends: ptr(false)}, nil
		}, KindTemplateRefused, []string{applied, current, "does not descend"}},
		{"tag elsewhere", store.AppliedVersion{Commit: applied}, func(_, _, _ string) (*templates.Checked, error) {
			return &templates.Checked{Source: sampleTemplate.Source, Commit: "3000000000000000000000000000000000000003", Definition: true, Descends: ptr(true)}, nil
		}, KindTemplateRefused, []string{"3000000000000000000000000000000000000003", current}},
		{"refused", store.AppliedVersion{Commit: applied}, func(_, _, _ string) (*templates.Checked, error) {
			return nil, &templates.Error{Status: 422, Kind: "template-resolve-failed", Message: "https://git.example.invalid/secret"}
		}, KindTemplateRefused, []string{applied, "template-resolve-failed"}},
		{"unavailable", store.AppliedVersion{Commit: applied}, func(_, _, _ string) (*templates.Checked, error) {
			return nil, &templates.UnavailableError{Err: errors.New("no socket")}
		}, KindTemplateUnavailable, []string{applied}},
		{"nothing known", store.AppliedVersion{}, nil, KindTemplateRefused, []string{"neither"}},
		{"descends", store.AppliedVersion{Commit: applied}, func(_, ref, since string) (*templates.Checked, error) {
			if ref != "v2.0.0" || since != applied {
				return nil, fmt.Errorf("asked %s since %s", ref, since)
			}
			return &templates.Checked{Source: sampleTemplate.Source, Commit: current, Definition: true, Descends: ptr(true)}, nil
		}, "", []string{"descends from its applied commit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.createWorkspace(t, "alpha")
			if err := w.data.SetAppliedVersion(context.Background(), "alpha", tc.applied); err != nil {
				t.Fatal(err)
			}
			w.tmpl.checkSince = tc.answer
			before := len(w.tmpl.seen())
			job := w.startJob(t, "upgrade", "alpha", "")
			if tc.kind == "" {
				waitWaiting(t, job, 0)
				w.answer(t, job, sessionAsking)
				if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
					t.Fatalf("%+v %+v", snap, snap.Error)
				}
				if got, _ := w.data.AppliedVersion(context.Background(), "alpha"); got.Version != "2.0.0" || got.Commit != current {
					t.Fatalf("%+v", got)
				}
				return
			}
			// A refusal ends the job before the interview; a job that goes
			// on to ask fails here, by name, not at the test binary's limit.
			snap := refusedWithin(t, job, 10*time.Second)
			if snap.State != jobs.Failed || snap.Error.Kind != tc.kind {
				t.Fatalf("%+v %+v", snap, snap.Error)
			}
			for _, named := range tc.names {
				if !strings.Contains(snap.Error.Message, named) {
					t.Errorf("the error does not name %s: %s", named, snap.Error.Message)
				}
			}
			if strings.Contains(snap.Error.Message, "example.invalid") {
				t.Errorf("the error quotes the template service's message: %s", snap.Error.Message)
			}
			// Nothing was backed up or asked: at most the descent check.
			for _, call := range w.tmpl.seen()[before:] {
				if !strings.HasPrefix(call, "check-since ") {
					t.Errorf("the template service was asked %s", call)
				}
			}
			if got, _ := w.data.AppliedVersion(context.Background(), "alpha"); got != tc.applied {
				t.Errorf("the applied version changed: %+v", got)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// refusedWithin waits up to limit for a job that must end on its own, and
// fails the test by name when it is still running then, as one that went on
// to wait for answers is.
func refusedWithin(t *testing.T, job *jobs.Job, limit time.Duration) jobs.Snapshot {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !job.Snapshot().State.Terminal() {
		if time.Now().After(deadline) {
			t.Fatalf("the job did not end within %s: it is %s in phase %s, where a refusal ends it before the interview", limit, job.Snapshot().State, job.Snapshot().Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return job.Snapshot()
}

// A written upgrade, a conflicted one and Toha's already-current each record
// the pinned version and commit as the workspace's applied version; a plan
// that writes nothing records nothing.
func TestAnAppliedUpgradeRecordsThePinnedVersion(t *testing.T) {
	for outcome, records := range map[templates.Outcome]bool{
		templates.Written: true, templates.Conflicted: true, templates.AlreadyCurrent: true, templates.Planned: false,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			w := newWorld(t)
			w.createWorkspace(t, "alpha")
			w.tmpl.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
				var conflicts []string
				if outcome == templates.Conflicted {
					writeFile(t, s.Target, "local.txt", "<<<<<<< conflict\n")
					conflicts = []string{"local.txt"}
				}
				if outcome != templates.Planned {
					writeFile(t, s.Target, ".toha.json", appliedRecord(s))
				}
				return appliedReply(outcome, nil, conflicts), nil
			}
			job := w.startJob(t, "upgrade", "alpha", "")
			waitWaiting(t, job, 0)
			w.answer(t, job, sessionAsking)
			waitEnded(t, job)
			got, _ := w.data.AppliedVersion(context.Background(), "alpha")
			want := store.AppliedVersion{Version: "1.0.0", Commit: sampleTemplate.CurrentCommit}
			if records {
				want = store.AppliedVersion{Version: "2.0.0", Commit: "2000000000000000000000000000000000000002"}
			}
			if got != want {
				t.Fatalf("%+v, want %+v", got, want)
			}
		})
	}
}

// While a check has found the current version's tag at another commit, a
// create and an upgrade from the template are refused 409
// template-version-moved before any job; once the template is upgraded to
// that version again, with the new commit, both go on.
func TestAMovedTagRefusesCreatesAndUpgradesUntilTakenAgain(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.createWorkspace(t, "alpha")
	w.newVersion(t)
	moved := "2b00000000000000000000000000000000000002"
	tpl, err := w.data.RecordCheck(ctx, "sample", 0, []store.Version{
		{Version: "1.0.0", Tag: "1.0.0", Commit: sampleTemplate.CurrentCommit},
		{Version: "2.0.0", Tag: "v2.0.0", Commit: moved},
	}, time.Now())
	if err != nil || !tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
	for _, body := range []string{
		`{"operation":"create","workspace":"beta","templateId":"sample"}`,
		`{"operation":"upgrade","workspace":"alpha"}`,
	} {
		status, reply := w.do(t, http.MethodPost, "/api/jobs", body)
		if status != http.StatusConflict || !strings.Contains(reply, `"kind":"`+KindTemplateVersionMoved+`"`) || !strings.Contains(reply, "2.0.0") {
			t.Fatalf("%s: %d %s", body, status, reply)
		}
	}
	if got := w.tmpl.seen(); len(got) != 3 {
		t.Fatalf("the template service was asked %v", got)
	}
	if _, err := w.data.AdvanceTemplate(ctx, "sample", "2.0.0", "v2.0.0", moved, registry.Newer); err != nil {
		t.Fatal(err)
	}
	job := w.startJobBody(t, `{"operation":"upgrade","workspace":"alpha"}`)
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if got, _ := w.data.AppliedVersion(ctx, "alpha"); got != (store.AppliedVersion{Version: "2.0.0", Commit: moved}) {
		t.Fatalf("%+v", got)
	}
}

// startJobBody posts a job request as given and returns its job.
func (w *world) startJobBody(t *testing.T, body string) *jobs.Job {
	t.Helper()
	status, reply := w.do(t, http.MethodPost, "/api/jobs", body)
	if status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, reply)
	}
	var snap jobs.Snapshot
	_ = json.Unmarshal([]byte(reply), &snap)
	job, ok := w.manager.Get(snap.JobID)
	if !ok {
		t.Fatalf("job %s not found", snap.JobID)
	}
	return job
}

// A job re-checks the stored applied version it starts from: one the
// workspace took after the request was checked, the current version or a
// newer one, refuses the upgrade as template-refused before anything runs.
func TestAnUpgradeJobRefusesAnAppliedVersionNotOlderThanTheCurrent(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.createWorkspace(t, "alpha")
	w.newVersion(t)
	tpl, err := w.data.Template(ctx, "sample")
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2.0.0", "3.0.0"} {
		if err := w.data.SetAppliedVersion(ctx, "alpha", store.AppliedVersion{Version: version, Commit: "3000000000000000000000000000000000000003"}); err != nil {
			t.Fatal(err)
		}
		_, _, err := (&upgrading{w: w.flows, name: "alpha"}).forward(ctx, nil, tpl)
		var failure *jobs.Failure
		if !errors.As(err, &failure) || failure.Kind != KindTemplateRefused || !strings.Contains(err.Error(), "already has version "+version) {
			t.Fatalf("%s: %v", version, err)
		}
	}
	if err := w.data.SetAppliedVersion(ctx, "alpha", store.AppliedVersion{Version: "1.0.0", Commit: sampleTemplate.CurrentCommit}); err != nil {
		t.Fatal(err)
	}
	if from, _, err := (&upgrading{w: w.flows, name: "alpha"}).forward(ctx, nil, tpl); err != nil || from != "version 1.0.0" {
		t.Fatalf("%q %v", from, err)
	}
}

// uncheckedTemplate forgets the sample template's current commit, as an
// imported template has none before its first check.
func (w *world) uncheckedTemplate(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+w.data.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE templates SET current_commit = NULL WHERE id = 'sample'`); err != nil {
		t.Fatal(err)
	}
}

// selectors records the commit each upgrade start and apply names, and
// has the apply write a record whose commit is recordCommit.
func (w *world) selectors(t *testing.T, recordCommit string) *[]string {
	t.Helper()
	var mu sync.Mutex
	commits := &[]string{}
	keep := func(s templates.Selector) {
		mu.Lock()
		defer mu.Unlock()
		*commits = append(*commits, s.Commit)
	}
	w.tmpl.startUpgrade = func(s templates.Selector) (*templates.Reply, error) {
		keep(s)
		return reply(templates.Questions, sessionAsking, batch), nil
	}
	w.tmpl.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
		keep(s)
		record := fmt.Sprintf(`{"applications":[{"target":".","template":%q,"source":%q,"commit":%q}]}`, s.Address+"@"+s.Ref, s.Address, recordCommit)
		writeFile(t, s.Target, ".toha.json", record)
		return appliedReply(templates.Written, nil, nil), nil
	}
	return commits
}

// With no commit recorded for the current version, an upgrade of an
// unknown applied version pins the commit the descent check approved, so
// its start and apply name it and the template service refuses a tag
// moved since; the workspace records that commit.
func TestAnUpgradePinsTheCommitItsDescentCheckApproved(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.createWorkspace(t, "alpha")
	w.newVersion(t)
	w.uncheckedTemplate(t)
	w.unknown(t, "alpha")
	approved := "2000000000000000000000000000000000000002"
	w.tmpl.checkSince = func(_, ref, since string) (*templates.Checked, error) {
		return &templates.Checked{Source: sampleTemplate.Source, Commit: approved, Definition: true, Descends: ptr(true)}, nil
	}
	commits := w.selectors(t, approved)
	job := w.startJobBody(t, `{"operation":"upgrade","workspace":"alpha"}`)
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	if snap := endsWithin(t, job, 10*time.Second); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if !reflect.DeepEqual(*commits, []string{approved, approved}) {
		t.Fatalf("the start and apply named the commits %q, not the approved %s", *commits, approved)
	}
	if got, _ := w.data.AppliedVersion(ctx, "alpha"); got != (store.AppliedVersion{Version: "2.0.0", Commit: approved}) {
		t.Fatalf("the workspace recorded %+v, not version 2.0.0 at the approved commit", got)
	}
}

// An upgrade of a template whose current commit is not known yet records
// the commit the applied record names, when it is a full object id, and
// none otherwise.
func TestAnUpgradeOfAnUncheckedTemplateRecordsTheRecordsCommit(t *testing.T) {
	for recordCommit, want := range map[string]string{
		"2b00000000000000000000000000000000000002": "2b00000000000000000000000000000000000002",
		"not-a-commit": "",
	} {
		t.Run(recordCommit, func(t *testing.T) {
			w := newWorld(t)
			w.createWorkspace(t, "alpha")
			w.newVersion(t)
			w.uncheckedTemplate(t)
			commits := w.selectors(t, recordCommit)
			job := w.startJobBody(t, `{"operation":"upgrade","workspace":"alpha"}`)
			waitWaiting(t, job, 0)
			w.answer(t, job, sessionAsking)
			if snap := endsWithin(t, job, 10*time.Second); snap.State != jobs.Succeeded {
				t.Fatalf("%+v %+v", snap, snap.Error)
			}
			if !reflect.DeepEqual(*commits, []string{"", ""}) {
				t.Fatalf("the start and apply named the commits %q, with none recorded", *commits)
			}
			if got, _ := w.data.AppliedVersion(context.Background(), "alpha"); got != (store.AppliedVersion{Version: "2.0.0", Commit: want}) {
				t.Fatalf("the workspace recorded %+v, not version 2.0.0 at commit %q", got, want)
			}
		})
	}
}

// Every template service request of a create and an upgrade, the descent
// check included, names the template's credential.
func TestEveryTemplateRequestNamesTheTemplatesCredential(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.data.SetTemplateCredential(ctx, "sample", store.Credential{App: "sample-org"}, sampleVersions, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.createWorkspace(t, "alpha")
	w.newVersion(t)
	w.uncheckedTemplate(t)
	w.unknown(t, "alpha")
	approved := "2000000000000000000000000000000000000002"
	w.tmpl.checkSince = func(_, ref, since string) (*templates.Checked, error) {
		return &templates.Checked{Source: sampleTemplate.Source, Commit: approved, Definition: true, Descends: ptr(true)}, nil
	}
	w.selectors(t, approved)
	job := w.startJobBody(t, `{"operation":"upgrade","workspace":"alpha"}`)
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	if snap := endsWithin(t, job, 10*time.Second); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	w.tmpl.mu.Lock()
	named := append([]string(nil), w.tmpl.credentials...)
	w.tmpl.mu.Unlock()
	if len(named) < 5 {
		t.Fatalf("calls %v", w.tmpl.seen())
	}
	for _, c := range named {
		if c != "sample-org" {
			t.Fatalf("a request named %q, not the template's App: %v", c, named)
		}
	}
}

// An upgrade's and an Apply branch's descent check that the template
// service refuses names the template's credential, so a manager knows
// which one the Git host refused.
func TestARefusedDescentCheckNamesTheTemplatesCredential(t *testing.T) {
	w := newWorld(t)
	if _, err := w.data.SetTemplateCredential(context.Background(), "sample", store.Credential{App: "sample-org"}, sampleVersions, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.createWorkspace(t, "alpha")
	w.newVersion(t)
	w.uncheckedTemplate(t)
	w.unknown(t, "alpha")
	w.tmpl.checkSince = func(_, _, _ string) (*templates.Checked, error) {
		return nil, &templates.Error{Status: 422, Kind: "template-resolve-failed", Message: "refused"}
	}
	const named = " with GitHub App sample-org (template-resolve-failed)"
	job := w.startJobBody(t, `{"operation":"upgrade","workspace":"alpha"}`)
	if snap := endsWithin(t, job, 10*time.Second); snap.State != jobs.Failed || snap.Error.Kind != KindTemplateRefused || !strings.Contains(snap.Error.Message, named) {
		t.Fatalf("upgrade: %+v %+v", snap, snap.Error)
	}
	w.contributeRecord(t, "alpha", "review/record", toha(sampleTemplate.Source, "1.1.0", laterCommit))
	job, snap := w.applyBranch(t, "alpha", "review/record")
	if snap.State != jobs.Failed || !strings.Contains(snap.Error.Message, named) {
		t.Fatalf("apply branch: %+v %+v\n%s", snap, snap.Error, logText(job))
	}
}
