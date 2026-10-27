package workflows

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

func TestOperationsAreCreateUpgradeAndApplyBranch(t *testing.T) {
	var names []string
	for name := range New(Options{}).Operations() {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{api.OperationApplyBranch, api.OperationCreate, api.OperationUpgrade}) {
		t.Fatalf("operations %v", names)
	}
}

func TestCreateInterviewsGeneratesBacksUpAndStarts(t *testing.T) {
	w := newWorld(t)
	var initialized []string
	generate := w.tmpl.generate
	w.tmpl.generate = func(s templates.Selector) (*templates.Applied, error) {
		// Generation sees the folder holding only an unborn repository.
		entries, _ := os.ReadDir(s.Target)
		for _, e := range entries {
			initialized = append(initialized, e.Name())
		}
		if repo, err := git.PlainOpen(s.Target); err != nil {
			t.Errorf("no repository before generation: %v", err)
		} else if _, err := repo.Head(); err == nil {
			t.Error("branch born before generation")
		}
		return generate(s)
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	if _, err := os.Lstat(filepath.Join(w.root, "alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the folder exists while the interview waits")
	}
	status, body := w.answer(t, job, sessionAsking)
	want := `{"session":` + sessionDone + `,"state":"completed","document":null,"warnings":[]}`
	if status != http.StatusOK || body != want {
		t.Fatalf("answer %d %s", status, body)
	}
	snap := waitEnded(t, job)
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if got := w.tmpl.seen(); !reflect.DeepEqual(got, []string{"start-interview", "answer-interview", "generate"}) {
		t.Fatalf("template calls %v", got)
	}
	if w.tmpl.session("answer-interview") != sessionAsking || w.tmpl.session("generate") != sessionDone {
		t.Fatalf("sessions %v", w.tmpl.sessions)
	}
	if !reflect.DeepEqual(initialized, []string{".git"}) {
		t.Fatalf("folder before generation %v", initialized)
	}
	interviews := events(job, EventInterview)
	if len(interviews) != 2 || interviews[0] != `{"session":`+sessionAsking+`,"state":"questions","document":`+batch+`,"warnings":[]}` || interviews[1] != want {
		t.Fatalf("interview events %q", interviews)
	}
	if r := result(job); !strings.HasPrefix(r, `{"state":"succeeded","error":null,"apply":{"outcome":"written",`) {
		t.Fatalf("result %s", r)
	}
	history := w.remoteHistory(t, "alpha")
	if len(history) != 1 || history[0].NumParents() != 0 {
		t.Fatalf("remote branch %v", history)
	}
	names := treeNames(t, history[0])
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{".toha.json", "hook-marker.txt", "local.txt", "result.txt"}) {
		t.Fatalf("backed-up tree %v", names)
	}
	if got := w.rt.seen(); !reflect.DeepEqual(got, []string{"up alpha"}) {
		t.Fatalf("runtime %v", got)
	}
	log := logText(job)
	for _, line := range []string{"create result.txt", "staged hook output: hook-marker.txt", "pushed branch alpha"} {
		if !strings.Contains(log, line) {
			t.Errorf("log lacks %q:\n%s", line, log)
		}
	}
}

func TestAFailedHookKeepsThePartialFolderWithManualRecovery(t *testing.T) {
	w := newWorld(t)
	w.tmpl.generate = func(s templates.Selector) (*templates.Applied, error) {
		writeFile(t, s.Target, "out.txt", "written before the hook\n")
		return nil, &templates.Error{Status: 422, Kind: "hook-failed", Message: "hook exited with status 3"}
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	snap := waitEnded(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != KindGenerationFailed {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	msg := snap.Error.Message
	for _, part := range []string{"generation failed (hook-failed)", "holds the files written before the failure and no applied record", "recovery is manual: inspect the folder, then remove it before creating workspace alpha again"} {
		if !strings.Contains(msg, part) {
			t.Errorf("message lacks %q: %s", part, msg)
		}
	}
	if strings.Contains(strings.ToLower(msg), "retry") {
		t.Errorf("message claims a retry: %s", msg)
	}
	entries, _ := os.ReadDir(filepath.Join(w.root, "alpha"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{".git", "out.txt"}) {
		t.Fatalf("folder %v", names)
	}
	if w.remoteHistory(t, "alpha") != nil || len(w.rt.seen()) != 0 {
		t.Fatal("a failed generation was backed up or started")
	}
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"create","workspace":"alpha","templateId":"sample-v1"}`); status != 409 || !strings.Contains(body, KindWorkspaceExists) {
		t.Fatalf("create over the kept folder %d %s", status, body)
	}
	if _, ok := w.manager.Active("alpha"); ok {
		t.Fatal("the failed job kept the slot")
	}
}

func TestAGenerationThatWroteNothingFails(t *testing.T) {
	for _, outcome := range []templates.Outcome{templates.Planned, templates.NeedsTrust} {
		t.Run(string(outcome), func(t *testing.T) {
			w := newWorld(t)
			w.tmpl.generate = func(templates.Selector) (*templates.Applied, error) { return appliedReply(outcome, nil, nil), nil }
			job := w.startJob(t, "create", "alpha", "sample-v1")
			waitWaiting(t, job, 0)
			w.answer(t, job, sessionAsking)
			snap := waitEnded(t, job)
			if snap.State != jobs.Failed || snap.Error.Kind != KindNotApplied || !strings.Contains(snap.Error.Message, "reported "+string(outcome)+" and wrote nothing") {
				t.Fatalf("%+v %+v", snap, snap.Error)
			}
			if !strings.Contains(result(job), `"apply":{"outcome":"`+string(outcome)+`"`) {
				t.Fatalf("result %s", result(job))
			}
			if w.remoteHistory(t, "alpha") != nil || len(w.rt.seen()) != 0 {
				t.Fatal("nothing written, yet backed up or started")
			}
		})
	}
}

func TestABackupFailureFailsTheCreateBeforeStarting(t *testing.T) {
	w := newWorld(t)
	w.flows.opts.Remote.URL = filepath.Join(t.TempDir(), "absent.git")
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	snap := waitEnded(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupFailed || !strings.Contains(snap.Error.Message, "its backup failed") || !strings.Contains(snap.Error.Message, "was not started") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if len(w.rt.seen()) != 0 {
		t.Fatal("started after a failed backup")
	}
	repo, _ := git.PlainOpen(filepath.Join(w.root, "alpha"))
	if head, err := repo.Head(); err != nil || head.Name() != "refs/heads/alpha" {
		t.Fatalf("local commit missing: %v %v", head, err)
	}
	if _, ok := w.manager.Active("alpha"); ok {
		t.Fatal("the failed job kept the slot")
	}
}

func TestAStartFailureKeepsItsKind(t *testing.T) {
	w := newWorld(t)
	w.rt.fail = &devcontainer.InitializeError{Err: errors.New("exit status 3")}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	snap := waitEnded(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != "initialize-command-failed" || !strings.Contains(snap.Error.Message, "generated and backed up as") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if len(w.remoteHistory(t, "alpha")) != 1 {
		t.Fatal("not backed up before the start")
	}
}

func TestATemplateRefusalAtTheStartFailsTheJob(t *testing.T) {
	w := newWorld(t)
	w.tmpl.startInterview = func(templates.Selector) (*templates.Reply, error) {
		return nil, &templates.Error{Status: 422, Kind: "template-not-found", Message: "no such template"}
	}
	snap := waitEnded(t, w.startJob(t, "create", "alpha", "sample-v1"))
	if snap.State != jobs.Failed || snap.Error.Kind != KindTemplateRefused || !strings.Contains(snap.Error.Message, "(template-not-found): no such template") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	w.tmpl.startInterview = func(templates.Selector) (*templates.Reply, error) {
		return nil, &templates.UnavailableError{Err: errors.New("connection refused")}
	}
	snap = waitEnded(t, w.startJob(t, "create", "alpha", "sample-v1"))
	if snap.State != jobs.Failed || snap.Error.Kind != KindTemplateUnavailable {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

func TestAFolderCreatedDuringTheInterviewIsNotTouched(t *testing.T) {
	w := newWorld(t)
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	writeFile(t, w.root, "alpha/mine.txt", "someone else's\n")
	w.answer(t, job, sessionAsking)
	snap := waitEnded(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != KindWorkspaceExists {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	entries, _ := os.ReadDir(filepath.Join(w.root, "alpha"))
	if len(entries) != 1 || entries[0].Name() != "mine.txt" {
		t.Fatalf("folder touched: %v", entries)
	}
	if got := w.tmpl.seen(); got[len(got)-1] == "generate" {
		t.Fatal("generated into a folder Menagerie did not create")
	}
}

func TestCreateAndUpgradeRequestsAreCheckedBeforeAnyJob(t *testing.T) {
	w := newWorld(t)
	writeFile(t, w.root, "existing/file", "x")
	cases := []struct {
		body, kind string
		status     int
	}{
		{`{"operation":"create","workspace":"a..b","templateId":"sample-v1"}`, "invalid-workspace-name", 400},
		{`{"operation":"create","workspace":".hidden","templateId":"sample-v1"}`, "invalid-workspace-name", 400},
		{`{"operation":"create","workspace":"a/b","templateId":"sample-v1"}`, "invalid-workspace-name", 400},
		{`{"operation":"create","workspace":"fresh"}`, "invalid-request", 400},
		{`{"operation":"create","workspace":"fresh","templateId":"other"}`, "unknown-template", 404},
		{`{"operation":"create","workspace":"existing","templateId":"sample-v1"}`, KindWorkspaceExists, 409},
		{`{"operation":"upgrade","workspace":"absent","templateId":"sample-v2"}`, "workspace-not-found", 404},
		{`{"operation":"upgrade","workspace":"existing","templateId":"other"}`, "unknown-template", 404},
		{`{"operation":"upgrade","workspace":"x.lock","templateId":"sample-v2"}`, "invalid-workspace-name", 400},
	}
	for _, tc := range cases {
		status, body := w.do(t, http.MethodPost, "/api/jobs", tc.body)
		if status != tc.status || !strings.Contains(body, `"kind":"`+tc.kind+`"`) {
			t.Errorf("%s: %d %s", tc.body, status, body)
		}
	}
	if len(w.manager.List()) != 0 || len(w.tmpl.seen()) != 0 {
		t.Fatal("a refused request started a job")
	}
}
