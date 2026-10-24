package workflows

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

func TestARejectedOrRefusedAnswerKeepsTheJobWaiting(t *testing.T) {
	w := newWorld(t)
	answers := 0
	w.tmpl.answerInterview = func(_ templates.Selector, session, _ json.RawMessage) (*templates.Reply, error) {
		answers++
		switch answers {
		case 1:
			return reply(templates.Questions, sessionAsking, `{"status":"questions","errors":{"label":"required"}}`), nil
		case 2:
			return nil, &templates.Error{Status: 409, Kind: "session-mismatch", Message: "the session was not started for this target"}
		case 3:
			return nil, &templates.UnavailableError{Err: errors.New("connection refused")}
		}
		return reply(templates.Completed, sessionDone, "null"), nil
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	last := waitWaiting(t, job, 0)
	if status, body := w.answer(t, job, sessionAsking); status != 200 || !strings.Contains(body, `"errors":{"label":"required"}`) {
		t.Fatalf("rejected %d %s", status, body)
	}
	last = waitWaiting(t, job, last.ID)
	status, body := w.answer(t, job, `{"tampered":true}`)
	if status != 409 || !strings.Contains(body, `"kind":"session-mismatch"`) {
		t.Fatalf("refused %d %s", status, body)
	}
	last = waitWaiting(t, job, last.ID)
	if status, body := w.answer(t, job, sessionAsking); status != 503 || !strings.Contains(body, `"kind":"template-service-unavailable"`) {
		t.Fatalf("unavailable %d %s", status, body)
	}
	waitWaiting(t, job, last.ID)
	if job.Snapshot().State != jobs.WaitingForAnswers {
		t.Fatalf("%+v", job.Snapshot())
	}
	if status, _ := w.answer(t, job, sessionAsking); status != 200 {
		t.Fatalf("final answer %d", status)
	}
	if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if got := len(events(job, EventInterview)); got != 3 {
		t.Fatalf("%d interview events", got)
	}
}

func TestOnlyOneAnswerAtATimeAndTheSlotIsHeldThroughout(t *testing.T) {
	w := newWorld(t)
	entered, hold := make(chan struct{}), make(chan struct{})
	w.tmpl.answerInterview = func(templates.Selector, json.RawMessage, json.RawMessage) (*templates.Reply, error) {
		entered <- struct{}{}
		<-hold
		return reply(templates.Completed, sessionDone, "null"), nil
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)

	// The waiting job holds the workspace against every operation.
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"create","workspace":"alpha","templateId":"sample-v1"}`); status != 409 || !strings.Contains(body, `"activeJobId":"`+job.ID()+`"`) {
		t.Fatalf("second create %d %s", status, body)
	}
	other := w.startJob(t, "create", "beta", "sample-v1")
	waitWaiting(t, other, 0)

	first := make(chan string, 1)
	go func() {
		status, body := w.answer(t, job, sessionAsking)
		first <- http.StatusText(status) + " " + body
	}()
	<-entered
	if job.Snapshot().State != jobs.Running {
		t.Fatalf("state while answering %+v", job.Snapshot())
	}
	if status, body := w.answer(t, job, sessionAsking); status != 409 || !strings.Contains(body, `"kind":"job-not-waiting"`) {
		t.Fatalf("concurrent answer %d %s", status, body)
	}
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"create","workspace":"alpha","templateId":"sample-v1"}`); status != 409 || !strings.Contains(body, "workspace-busy") {
		t.Fatalf("operation during an answer %d %s", status, body)
	}
	close(hold)
	if got := <-first; !strings.HasPrefix(got, "OK ") {
		t.Fatalf("held answer %s", got)
	}
	if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if status, body := w.answer(t, job, sessionAsking); status != 409 || !strings.Contains(body, "job-not-waiting") {
		t.Fatalf("answer after the end %d %s", status, body)
	}
	go func() { <-entered }()
	if status, _ := w.answer(t, other, sessionAsking); status != 200 {
		t.Fatalf("other workspace %d", status)
	}
	waitEnded(t, other)
}

func TestAnswersRouteChecksTheJobAndBody(t *testing.T) {
	w := newWorld(t)
	if status, body := w.do(t, http.MethodPost, "/api/jobs/absent/answers", `{"session":{},"submission":{}}`); status != 404 || !strings.Contains(body, "job-not-found") {
		t.Fatalf("unknown job %d %s", status, body)
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	for _, body := range []string{
		``, `{}`, `{"session":{}}`, `{"session":null,"submission":{}}`, `{"session":{},"submission":[]}`,
		`{"session":{},"submission":{},"extra":1}`, `{"session":{},"submission":{}} {}`, `not json`,
	} {
		if status, got := w.do(t, http.MethodPost, "/api/jobs/"+job.ID()+"/answers", body); status != 400 || !strings.Contains(got, "invalid-request") {
			t.Errorf("body %q: %d %s", body, status, got)
		}
	}
	if job.Snapshot().State != jobs.WaitingForAnswers || len(w.tmpl.seen()) != 1 {
		t.Fatalf("a malformed answer reached the job: %+v %v", job.Snapshot(), w.tmpl.seen())
	}
}

func TestAFlowEndEndsTheJobWithoutWriting(t *testing.T) {
	w := newWorld(t)
	w.tmpl.answerInterview = func(templates.Selector, json.RawMessage, json.RawMessage) (*templates.Reply, error) {
		return reply(templates.Ended, sessionDone, `{"status":"stopped"}`), nil
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	if status, body := w.answer(t, job, sessionAsking); status != 200 || !strings.Contains(body, `"state":"ended"`) {
		t.Fatalf("%d %s", status, body)
	}
	snap := waitEnded(t, job)
	if snap.State != jobs.Ended || snap.Error != nil || result(job) != `{"state":"ended","error":null}` {
		t.Fatalf("%+v %s", snap, result(job))
	}
	if _, err := os.Lstat(filepath.Join(w.root, "alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an ended create wrote the folder")
	}
	w.tmpl.startInterview = func(templates.Selector) (*templates.Reply, error) {
		return reply(templates.Ended, sessionDone, `{"status":"aborted"}`), nil
	}
	again := w.startJob(t, "create", "alpha", "sample-v1")
	if snap := waitEnded(t, again); snap.State != jobs.Ended {
		t.Fatalf("immediate end %+v", snap)
	}
}

func TestAnAdmittedAnswerIsRefusedWhenTheInterviewCloses(t *testing.T) {
	w := New(Options{})
	iv := w.open("job")
	iv.mu.Lock()
	iv.waiting = true
	iv.mu.Unlock()
	refused := make(chan error, 1)
	go func() {
		_, err := w.Answer(t.Context(), "job", json.RawMessage(`{}`), json.RawMessage(`{}`))
		refused <- err
	}()
	// The answer is admitted once the mailbox holds its turn.
	for len(iv.turns) == 0 {
		select {
		case err := <-refused:
			t.Fatalf("answer returned before admission: %v", err)
		default:
		}
		runtime.Gosched()
	}
	w.close("job", iv)
	if err := <-refused; err != api.ErrNotWaiting {
		t.Fatalf("admitted answer after close: %v", err)
	}
	if _, err := w.Answer(t.Context(), "job", json.RawMessage(`{}`), json.RawMessage(`{}`)); err != api.ErrNotWaiting {
		t.Fatalf("answer after close: %v", err)
	}
}
