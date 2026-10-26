package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manual clock with timers that fire when Advance passes
// their due time.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock by d and runs, in due order, every timer that
// is due, including timers armed by the ones it runs.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(target) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		next.stopped = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

type recordedAlerts struct {
	mu          sync.Mutex
	failed      []Snapshot
	longRunning []Snapshot
}

func (a *recordedAlerts) JobFailed(s Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failed = append(a.failed, s)
}

func (a *recordedAlerts) JobLongRunning(s Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.longRunning = append(a.longRunning, s)
}

func (a *recordedAlerts) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.failed), len(a.longRunning)
}

func limitedManager(ctx context.Context, clock *fakeClock, opts Options) (*Manager, *recordedAlerts) {
	alerts := &recordedAlerts{}
	opts.Alerts, opts.Now, opts.AfterFunc = alerts, clock.Now, clock.AfterFunc
	return NewManager(ctx, opts), alerts
}

// steps lets a test drive a runner: the runner calls serve, which runs on
// the runner's goroutine each step the test sends with do, until ctx ends.
type steps chan func(*Run)

func (s steps) serve(ctx context.Context, run *Run) error {
	for {
		select {
		case f := <-s:
			f(run)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// do runs f on the runner's goroutine and waits for it.
func (s steps) do(f func(*Run)) {
	done := make(chan struct{})
	s <- func(r *Run) { f(r); close(done) }
	<-done
}

func waitPhase(t *testing.T, job *Job, phase string) {
	t.Helper()
	waitFor(t, job, func(e Event) bool {
		return e.Type == EventState && strings.Contains(string(e.Data), `"phase":"`+phase+`"`)
	})
}

func TestPhaseLimitCancelsTheJobAndFreesTheWorkspace(t *testing.T) {
	clock := newFakeClock()
	m, alerts := limitedManager(context.Background(), clock, Options{Limits: Limits{Default: 2 * time.Hour, Phases: map[string]time.Duration{"build": 45 * time.Minute}}})
	var cause error
	job, err := m.Start("alpha", "rebuild", func(ctx context.Context, run *Run) error {
		run.Phase("build")
		<-ctx.Done()
		cause = context.Cause(ctx)
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, job, "build")
	start := clock.Now()
	snap := job.Snapshot()
	if snap.Deadline == nil || !snap.Deadline.Equal(start.Add(45*time.Minute)) || *snap.Limit != (Limit{Seconds: 2700, Scope: ScopePhase}) {
		t.Fatalf("deadline %v limit %+v", snap.Deadline, snap.Limit)
	}
	clock.Advance(44 * time.Minute)
	if s := job.Snapshot(); s.State != Running {
		t.Fatalf("ended before its limit: %+v", s)
	}
	clock.Advance(time.Minute)
	snap = waitEnded(t, job)
	want := Error{Kind: KindTimedOut, Message: "the build phase passed its 45m limit"}
	if snap.State != Failed || snap.Error == nil || *snap.Error != want {
		t.Fatalf("snapshot %+v error %+v", snap, snap.Error)
	}
	if snap.Deadline != nil || snap.Limit != nil {
		t.Fatalf("ended job keeps deadline %v limit %+v", snap.Deadline, snap.Limit)
	}
	if !errors.Is(cause, ErrTimedOut) {
		t.Fatalf("cause %v", cause)
	}
	events, _, _ := job.EventsAfter(0)
	var logged bool
	for _, e := range events {
		logged = logged || e.Type == EventLog && eventText(t, e) == want.Message+"; cancelling the job"
	}
	if !logged {
		t.Fatal("no log line at the cancel")
	}
	if failed, _ := alerts.counts(); failed != 1 || alerts.failed[0].Error.Kind != KindTimedOut {
		t.Fatalf("failed alerts %+v", alerts.failed)
	}
	next, err := m.Start("alpha", "start", func(context.Context, *Run) error { return nil })
	if err != nil {
		t.Fatalf("workspace not freed: %v", err)
	}
	waitEnded(t, next)
}

func TestJobLimitNamesThePhaseAndFollowsTheOperation(t *testing.T) {
	clock := newFakeClock()
	m, _ := limitedManager(context.Background(), clock, Options{Limits: Limits{
		Default:    2 * time.Hour,
		Operations: map[string]time.Duration{"rebuild": time.Hour},
	}})
	hang := func(ctx context.Context, run *Run) error {
		run.Phase("build")
		<-ctx.Done()
		return nil
	}
	start, _ := m.Start("beta", "start", hang)
	rebuild, _ := m.Start("alpha", "rebuild", hang)
	waitPhase(t, start, "build")
	waitPhase(t, rebuild, "build")
	if l := start.Snapshot().Limit; l == nil || *l != (Limit{Seconds: 7200, Scope: ScopeJob}) {
		t.Fatalf("start limit %+v", l)
	}
	clock.Advance(time.Hour)
	snap := waitEnded(t, rebuild)
	if snap.Error == nil || snap.Error.Kind != KindTimedOut || snap.Error.Message != "the job passed its 1h limit in the build phase" {
		t.Fatalf("rebuild %+v %+v", snap, snap.Error)
	}
	if s := start.Snapshot(); s.State != Running {
		t.Fatalf("start ended with the rebuild limit: %+v", s)
	}
	clock.Advance(time.Hour)
	if snap := waitEnded(t, start); snap.Error == nil || snap.Error.Message != "the job passed its 2h limit in the build phase" {
		t.Fatalf("start %+v %+v", snap, snap.Error)
	}
}

// TestWaitingPausesEveryLimit proves the rule that a limit times
// Menagerie's work and never the manager answering an interview.
func TestWaitingPausesEveryLimit(t *testing.T) {
	clock := newFakeClock()
	m, alerts := limitedManager(context.Background(), clock, Options{
		Limits:      Limits{Default: 10 * time.Minute, Phases: map[string]time.Duration{"interview": 8 * time.Minute}},
		LongRunning: 9 * time.Minute,
	})
	steps := make(steps)
	job, _ := m.Start("alpha", "create", steps.serve)
	steps.do(func(run *Run) { run.Phase("interview") })
	clock.Advance(4 * time.Minute)
	setState := func(s State) {
		t.Helper()
		steps.do(func(run *Run) {
			if err := run.SetState(s); err != nil {
				t.Error(err)
			}
		})
	}
	setState(WaitingForAnswers)
	snap := job.Snapshot()
	if snap.Deadline != nil || snap.Limit == nil || *snap.Limit != (Limit{Seconds: 480, Scope: ScopePhase}) {
		t.Fatalf("waiting deadline %v limit %+v", snap.Deadline, snap.Limit)
	}
	clock.Advance(24 * time.Hour)
	if s := job.Snapshot(); s.State != WaitingForAnswers {
		t.Fatalf("waiting job ended: %+v %+v", s, s.Error)
	}
	if _, lr := alerts.counts(); lr != 0 {
		t.Fatal("long-running alert while waiting")
	}
	// The answer resumes the interview phase: its limit starts again, the
	// job limit keeps the four minutes already run.
	steps.do(func(run *Run) { run.Phase("interview") })
	if s := job.Snapshot(); s.Deadline == nil || !s.Deadline.Equal(clock.Now().Add(6*time.Minute)) {
		t.Fatalf("resumed deadline %v", s.Deadline)
	}
	clock.Advance(5 * time.Minute)
	if _, lr := alerts.counts(); lr != 1 {
		t.Fatalf("long-running alerts %d after nine running minutes", lr)
	}
	if s := job.Snapshot(); s.State != Running {
		t.Fatalf("ended after nine running minutes: %+v %+v", s, s.Error)
	}
	clock.Advance(time.Minute)
	snap = waitEnded(t, job)
	if snap.Error == nil || snap.Error.Message != "the job passed its 10m limit in the interview phase" {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if _, lr := alerts.counts(); lr != 1 {
		t.Fatalf("long-running alerts %d", lr)
	}
}

func TestPhaseLimitCountsFromEachEntry(t *testing.T) {
	clock := newFakeClock()
	m, _ := limitedManager(context.Background(), clock, Options{Limits: Limits{Phases: map[string]time.Duration{"interview": 10 * time.Minute}}})
	steps := make(steps)
	job, _ := m.Start("alpha", "upgrade", steps.serve)
	enter := func() { steps.do(func(run *Run) { run.Phase("interview") }) }
	enter()
	clock.Advance(9 * time.Minute)
	enter()
	clock.Advance(9 * time.Minute)
	if s := job.Snapshot(); s.State != Running {
		t.Fatalf("phase limit not restarted: %+v %+v", s, s.Error)
	}
	clock.Advance(time.Minute)
	if snap := waitEnded(t, job); snap.Error == nil || snap.Error.Message != "the interview phase passed its 10m limit" {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

// TestTimedOutRunnerHoldsTheSlotUntilItReturns covers a runner that ignores
// its cancelled context: the job stays unfinished and the workspace busy
// until it returns, and its success is still reported as timed-out.
func TestTimedOutRunnerHoldsTheSlotUntilItReturns(t *testing.T) {
	clock := newFakeClock()
	m, _ := limitedManager(context.Background(), clock, Options{Limits: Limits{Default: time.Minute}})
	release := make(chan struct{})
	job, _ := m.Start("alpha", "start", func(_ context.Context, run *Run) error {
		run.Phase("up")
		<-release
		return nil
	})
	waitPhase(t, job, "up")
	clock.Advance(time.Minute)
	waitFor(t, job, func(e Event) bool { return e.Type == EventLog })
	if s := job.Snapshot(); s.State.Terminal() {
		t.Fatalf("job ended before its runner returned: %+v", s)
	}
	var busy *BusyError
	if _, err := m.Start("alpha", "stop", func(context.Context, *Run) error { return nil }); !errors.As(err, &busy) {
		t.Fatalf("second job beside an abandoned runner: %v", err)
	}
	close(release)
	if snap := waitEnded(t, job); snap.State != Failed || snap.Error.Kind != KindTimedOut || snap.Error.Message != "the job passed its 1m limit in the up phase" {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

func TestShutdownIsNotATimeout(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	m, _ := limitedManager(ctx, clock, Options{Limits: Limits{Default: time.Minute}})
	release := make(chan struct{})
	job, _ := m.Start("alpha", "start", func(ctx context.Context, run *Run) error {
		run.Phase("up")
		<-release
		return ctx.Err()
	})
	waitPhase(t, job, "up")
	cancel()
	clock.Advance(time.Hour)
	close(release)
	if snap := waitEnded(t, job); snap.State != Failed || snap.Error.Kind != "operation-failed" {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

func TestOnlyFailedJobsAlert(t *testing.T) {
	clock := newFakeClock()
	m, alerts := limitedManager(context.Background(), clock, Options{LongRunning: time.Hour})
	for i, runner := range []Runner{
		func(context.Context, *Run) error { return nil },
		func(context.Context, *Run) error { return ErrEnded },
		func(context.Context, *Run) error { return errors.New("boom") },
	} {
		job, _ := m.Start("alpha", "start", runner)
		if snap := waitEnded(t, job); i == 2 && snap.State != Failed {
			t.Fatalf("%+v", snap)
		}
	}
	clock.Advance(2 * time.Hour)
	if failed, lr := alerts.counts(); failed != 1 || lr != 0 || alerts.failed[0].Error.Message != "boom" {
		t.Fatalf("failed %+v long-running %d", alerts.failed, lr)
	}
}

func TestNoLimitMeansNoDeadline(t *testing.T) {
	clock := newFakeClock()
	m, _ := limitedManager(context.Background(), clock, Options{})
	release := make(chan struct{})
	job, _ := m.Start("alpha", "start", func(_ context.Context, run *Run) error {
		run.Phase("build")
		<-release
		return nil
	})
	waitPhase(t, job, "build")
	clock.Advance(1000 * time.Hour)
	if s := job.Snapshot(); s.State != Running || s.Deadline != nil || s.Limit != nil {
		t.Fatalf("%+v", s)
	}
	events, _, _ := job.EventsAfter(1)
	if string(events[0].Data) != `{"state":"running","phase":"build","deadline":null,"limit":null}` {
		t.Fatalf("state event %s", events[0].Data)
	}
	close(release)
	waitEnded(t, job)
}

func TestFormatLimit(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Hour:                       "2h",
		45 * time.Minute:                    "45m",
		90 * time.Minute:                    "1h30m",
		time.Hour + 30*time.Second:          "1h30s",
		10 * time.Second:                    "10s",
		100 * time.Millisecond:              "100ms",
		time.Minute + 1500*time.Millisecond: "1m1.5s",
	} {
		if got := FormatLimit(d); got != want {
			t.Errorf("FormatLimit(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestPhasesAreThePublishedPhases checks that Phases names exactly the
// phases the operations publish, so a phase limit can name each of them.
func TestPhasesAreThePublishedPhases(t *testing.T) {
	published := map[string]bool{}
	call := regexp.MustCompile(`\.Phase\("([a-z-]+)"\)`)
	for _, dir := range []string{"../devcontainer", "../workflows"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range call.FindAllStringSubmatch(string(src), -1) {
				published[m[1]] = true
			}
		}
	}
	var got []string
	for p := range published {
		got = append(got, p)
	}
	sort.Strings(got)
	want := slices.Clone(Phases)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("published phases %v, Phases %v", got, want)
	}
}
