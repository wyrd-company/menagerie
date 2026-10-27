package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitEnded blocks until job publishes its result event.
func waitEnded(t *testing.T, job *Job) Snapshot {
	t.Helper()
	for {
		_, ended, changed := job.EventsAfter(0)
		if ended {
			return job.Snapshot()
		}
		<-changed
	}
}

// waitFor blocks until job has an event satisfying match.
func waitFor(t *testing.T, job *Job, match func(Event) bool) {
	t.Helper()
	var cursor int64
	for {
		events, ended, changed := job.EventsAfter(cursor)
		for _, e := range events {
			if match(e) {
				return
			}
			cursor = e.ID
		}
		if ended {
			t.Fatalf("job ended without the awaited event")
		}
		<-changed
	}
}

func eventText(t *testing.T, e Event) string {
	t.Helper()
	var d struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.Text
}

func TestJobSucceedsWithOrderedEvents(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	job, err := m.Start("alpha", "start", func(_ context.Context, run *Run) error {
		run.Phase("build")
		fmt.Fprintln(run.Log(""), "built")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitEnded(t, job)
	if snap.State != Succeeded || snap.Error != nil || snap.FinishedAt == nil {
		t.Fatalf("snapshot %+v", snap)
	}
	events, _, _ := job.EventsAfter(0)
	var kinds []string
	for i, e := range events {
		if e.ID != int64(i+1) {
			t.Fatalf("event %d has id %d", i, e.ID)
		}
		kinds = append(kinds, e.Type)
	}
	if got := strings.Join(kinds, ","); got != "state,state,log,result" {
		t.Fatalf("event types %s", got)
	}
	if snap.LastEventID != 4 {
		t.Fatalf("lastEventId %d", snap.LastEventID)
	}
	if string(events[3].Data) != `{"state":"succeeded","error":null}` {
		t.Fatalf("result %s", events[3].Data)
	}
}

// TestSameWorkspaceExclusion holds one job at its runner, proves a competing
// job for the same workspace is refused while another workspace completes,
// and that the slot is free once the held job ends.
func TestSameWorkspaceExclusion(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	release := make(chan struct{})
	held, err := m.Start("alpha", "rebuild", func(_ context.Context, run *Run) error {
		run.Phase("held")
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, held, func(e Event) bool { return strings.Contains(string(e.Data), `"held"`) })

	_, err = m.Start("alpha", "stop", func(context.Context, *Run) error {
		t.Error("competing runner ran")
		return nil
	})
	var busy *BusyError
	if !errors.As(err, &busy) || busy.ActiveJobID != held.ID() || busy.Workspace != "alpha" {
		t.Fatalf("competing start: %v", err)
	}

	other, err := m.Start("beta", "stop", func(context.Context, *Run) error { return nil })
	if err != nil {
		t.Fatalf("other workspace refused: %v", err)
	}
	if snap := waitEnded(t, other); snap.State != Succeeded {
		t.Fatalf("other workspace %+v", snap)
	}
	if active, ok := m.Active("alpha"); !ok || active != held {
		t.Fatalf("held job lost its slot")
	}
	if snap := held.Snapshot(); snap.State != Running {
		t.Fatalf("held job state %s", snap.State)
	}

	close(release)
	waitEnded(t, held)
	next, err := m.Start("alpha", "start", func(context.Context, *Run) error { return nil })
	if err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	waitEnded(t, next)
}

func TestFailureAndPanicReleaseTheWorkspace(t *testing.T) {
	cases := []struct {
		name   string
		runner Runner
		kind   string
		msg    string
	}{
		{"error", func(context.Context, *Run) error { return errors.New("boom") }, "operation-failed", "boom"},
		{"failure-kind", func(context.Context, *Run) error {
			return &Failure{Kind: "initialize-command-failed", Err: errors.New("exit status 3")}
		}, "initialize-command-failed", "exit status 3"},
		{"panic", func(context.Context, *Run) error { panic("bad state") }, "internal-error", "operation panicked: bad state"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(context.Background(), Options{})
			job, err := m.Start("alpha", "start", tc.runner)
			if err != nil {
				t.Fatal(err)
			}
			snap := waitEnded(t, job)
			if snap.State != Failed || snap.Error == nil || snap.Error.Kind != tc.kind || snap.Error.Message != tc.msg {
				t.Fatalf("snapshot %+v error %+v", snap, snap.Error)
			}
			if _, ok := m.Active("alpha"); ok {
				t.Fatal("failed job kept the workspace")
			}
			if _, err := m.Start("alpha", "start", func(context.Context, *Run) error { return nil }); err != nil {
				t.Fatalf("retry refused: %v", err)
			}
			m.Wait()
		})
	}
}

// TestResultImpliesReleased checks, for many jobs, that a client that has
// seen the result event can start the next job on the same workspace.
func TestResultImpliesReleased(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	for i := range 500 {
		job, err := m.Start("alpha", "start", func(context.Context, *Run) error { return nil })
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		waitEnded(t, job)
	}
}

func TestWaitingForAnswersHoldsTheSlot(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	release := make(chan struct{})
	job, _ := m.Start("alpha", "create", func(_ context.Context, run *Run) error {
		run.Phase("interview")
		if err := run.SetState(Succeeded); err == nil {
			t.Error("terminal SetState accepted")
		}
		if err := run.SetState(WaitingForAnswers); err != nil {
			t.Error(err)
		}
		<-release
		return nil
	})
	waitFor(t, job, func(e Event) bool { return strings.Contains(string(e.Data), `"waiting-for-answers"`) })
	snap := job.Snapshot()
	if snap.State != WaitingForAnswers || snap.Phase != "interview" {
		t.Fatalf("snapshot %+v", snap)
	}
	if _, err := m.Start("alpha", "stop", func(context.Context, *Run) error { return nil }); err == nil {
		t.Fatal("waiting job did not hold the workspace")
	}
	close(release)
	waitEnded(t, job)
}

func TestEmitCompactsAndRefusesInvalidJSON(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	job, _ := m.Start("alpha", "create", func(_ context.Context, run *Run) error {
		if err := run.Emit("interview", json.RawMessage("{\n  \"state\": \"questions\",\n  \"n\": 12345678901234567890\n}")); err != nil {
			return err
		}
		if err := run.Emit("interview", json.RawMessage("{not json")); err == nil {
			return errors.New("invalid payload accepted")
		}
		return nil
	})
	snap := waitEnded(t, job)
	if snap.State != Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	events, _, _ := job.EventsAfter(1)
	if events[0].Type != "interview" || string(events[0].Data) != `{"state":"questions","n":12345678901234567890}` {
		t.Fatalf("event %s %s", events[0].Type, events[0].Data)
	}
}

func TestLogWriterSplitsLinesAndFlushesTheTail(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	job, _ := m.Start("alpha", "start", func(_ context.Context, run *Run) error {
		w := run.Log("[init] ")
		fmt.Fprint(w, "one\r\ntw")
		fmt.Fprint(w, "o\nunterminated")
		return nil
	})
	waitEnded(t, job)
	events, _, _ := job.EventsAfter(0)
	var lines []string
	for _, e := range events {
		if e.Type == EventLog {
			lines = append(lines, eventText(t, e))
		}
	}
	if got := strings.Join(lines, "|"); got != "[init] one|[init] two|[init] unterminated" {
		t.Fatalf("lines %q", got)
	}
	if events[len(events)-1].Type != EventResult {
		t.Fatal("tail written after the result")
	}
}

func TestEventsAfterReplaysFromCursor(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	job, _ := m.Start("alpha", "start", func(_ context.Context, run *Run) error {
		fmt.Fprintln(run.Log(""), "a")
		fmt.Fprintln(run.Log(""), "b")
		return nil
	})
	waitEnded(t, job)
	events, ended, _ := job.EventsAfter(2)
	if !ended || len(events) != 2 || events[0].ID != 3 || eventText(t, events[0]) != "b" {
		t.Fatalf("replay %+v", events)
	}
	if events, _, _ := job.EventsAfter(99); len(events) != 0 {
		t.Fatalf("cursor past end returned %d events", len(events))
	}
}

func TestRunnerUsesManagerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, Options{})
	job, _ := m.Start("alpha", "start", func(ctx context.Context, _ *Run) error {
		<-ctx.Done()
		return ctx.Err()
	})
	cancel()
	if snap := waitEnded(t, job); snap.State != Failed {
		t.Fatalf("%+v", snap)
	}
}

func TestListNewestFirst(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	var tick atomic.Int64
	m.now = func() time.Time { return time.Unix(tick.Add(1), 0) }
	a, _ := m.Start("alpha", "start", func(context.Context, *Run) error { return nil })
	b, _ := m.Start("beta", "start", func(context.Context, *Run) error { return nil })
	m.Wait()
	list := m.List()
	if len(list) != 2 || list[0].JobID != b.ID() || list[1].JobID != a.ID() {
		t.Fatalf("list %+v", list)
	}
	if _, ok := m.Get(a.ID()); !ok {
		t.Fatal("get")
	}
	if _, ok := m.Get("unknown"); ok {
		t.Fatal("unknown id found")
	}
}

func TestEndedRunnerEndsTheJobAndReleasesTheWorkspace(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	job, err := m.Start("alpha", "create", func(context.Context, *Run) error {
		return fmt.Errorf("flow stop: %w", ErrEnded)
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitEnded(t, job)
	if snap.State != Ended || snap.Error != nil || snap.FinishedAt == nil {
		t.Fatalf("snapshot %+v error %+v", snap, snap.Error)
	}
	events, _, _ := job.EventsAfter(0)
	if last := events[len(events)-1]; last.Type != EventResult || string(last.Data) != `{"state":"ended","error":null}` {
		t.Fatalf("result %s %s", last.Type, last.Data)
	}
	if _, err := m.Start("alpha", "start", func(context.Context, *Run) error { return nil }); err != nil {
		t.Fatalf("ended job kept the workspace: %v", err)
	}
	m.Wait()
}

func TestResultCarriesTheApplyOutcomeText(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	var id string
	job, _ := m.Start("alpha", "upgrade", func(_ context.Context, run *Run) error {
		id = run.JobID()
		if err := run.SetApply(json.RawMessage("{not json")); err == nil {
			return errors.New("invalid apply accepted")
		}
		if err := run.SetApply(json.RawMessage(`{"outcome":"planned"}`)); err != nil {
			return err
		}
		return run.SetApply(json.RawMessage("{\n \"outcome\": \"conflicted\", \"n\": 1e2, \"path\": \"a<b>&c\"\n}"))
	})
	waitEnded(t, job)
	if id != job.ID() {
		t.Fatalf("run job id %q, job id %q", id, job.ID())
	}
	events, _, _ := job.EventsAfter(0)
	want := `{"state":"succeeded","error":null,"apply":{"outcome":"conflicted","n":1e2,"path":"a<b>&c"}}`
	if last := events[len(events)-1]; string(last.Data) != want {
		t.Fatalf("result %s\nwant   %s", last.Data, want)
	}
}

func TestFailedResultKeepsTheApplyOutcome(t *testing.T) {
	m := NewManager(context.Background(), Options{})
	job, _ := m.Start("alpha", "upgrade", func(_ context.Context, run *Run) error {
		if err := run.SetApply(json.RawMessage(`{"outcome":"conflicted"}`)); err != nil {
			return err
		}
		return &Failure{Kind: "merge-conflicted", Err: errors.New("conflicts")}
	})
	waitEnded(t, job)
	events, _, _ := job.EventsAfter(0)
	want := `{"state":"failed","error":{"kind":"merge-conflicted","message":"conflicts"},"apply":{"outcome":"conflicted"}}`
	if last := events[len(events)-1]; string(last.Data) != want {
		t.Fatalf("result %s", last.Data)
	}
}
