// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, job-without-timeout-blocks-workspace]
// ---

// Package jobs runs workspace operations as in-memory jobs. A workspace has
// at most one job at a time. Jobs, their events and their logs live only in
// this process: a restart drops them. Optional time limits count a job's
// running time: a job past a limit is cancelled and fails as timed-out.
// Time spent waiting for answers is never counted.
package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// State is the lifecycle state of a job.
type State string

// Job states. Terminal states release the workspace.
const (
	Running           State = "running"
	WaitingForAnswers State = "waiting-for-answers"
	Succeeded         State = "succeeded"
	Failed            State = "failed"
	Ended             State = "ended"
)

// Terminal reports whether s ends a job.
func (s State) Terminal() bool {
	return s == Succeeded || s == Failed || s == Ended
}

// Event types written to a job's event stream.
const (
	EventState  = "state"
	EventLog    = "log"
	EventResult = "result"
)

// Event is one entry in a job's event stream. IDs start at 1 and increase
// by one per event within a job.
type Event struct {
	ID   int64
	Type string
	Data json.RawMessage
}

// Error describes why a job failed.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Failure is an error that names its kind. A runner returns it to choose
// the kind reported for a failed job; other errors use "operation-failed".
type Failure struct {
	Kind string
	Err  error
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

// ErrEnded is returned by a runner to end its job in the terminal state
// ended, as an interview's flow stop or abort does. It is not a failure.
var ErrEnded = errors.New("job ended")

// Snapshot is the public view of a job.
type Snapshot struct {
	JobID       string     `json:"jobId"`
	Workspace   string     `json:"workspace"`
	Operation   string     `json:"operation"`
	State       State      `json:"state"`
	Phase       string     `json:"phase"`
	CreatedAt   time.Time  `json:"createdAt"`
	FinishedAt  *time.Time `json:"finishedAt"`
	LastEventID int64      `json:"lastEventId"`
	Error       *Error     `json:"error"`
	// Deadline is when the first applying limit ends; nil when no limit
	// applies, while the job waits for answers, and once it has ended.
	Deadline *time.Time `json:"deadline"`
	// Limit is the limit that ends first; nil when none applies and once
	// the job has ended.
	Limit *Limit `json:"limit"`
}

// Runner performs a job's work. It runs on a goroutine owned by the
// Manager with the Manager's context, not a request's. Returning nil ends
// the job as succeeded, ErrEnded as ended, and any other error as failed.
type Runner func(ctx context.Context, run *Run) error

// BusyError reports that a workspace already has an unfinished job.
type BusyError struct {
	Workspace   string
	ActiveJobID string
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("workspace %s already has job %s", e.Workspace, e.ActiveJobID)
}

// Manager owns all jobs of the process.
type Manager struct {
	ctx       context.Context
	now       func() time.Time
	afterFunc func(time.Duration, func()) Timer
	opts      Options

	mu     sync.Mutex
	active map[string]*Job
	byID   map[string]*Job
	all    []*Job
	wg     sync.WaitGroup
}

// NewManager returns a Manager whose jobs run with a context derived from
// ctx. Cancelling ctx stops every job (process shutdown); a job past one of
// opts.Limits is cancelled alone.
func NewManager(ctx context.Context, opts Options) *Manager {
	m := &Manager{ctx: ctx, now: opts.Now, afterFunc: opts.AfterFunc, opts: opts, active: map[string]*Job{}, byID: map[string]*Job{}}
	if m.now == nil {
		m.now = time.Now
	}
	if m.afterFunc == nil {
		m.afterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	return m
}

// Start acquires the workspace's job slot and runs runner. It returns a
// *BusyError, without running anything, when the workspace has an
// unfinished job.
func (m *Manager) Start(workspace, operation string, runner Runner) (*Job, error) {
	m.mu.Lock()
	if current, ok := m.active[workspace]; ok {
		m.mu.Unlock()
		return nil, &BusyError{Workspace: workspace, ActiveJobID: current.id}
	}
	ctx, cancel := context.WithCancelCause(m.ctx)
	job := &Job{
		manager:   m,
		id:        newID(),
		workspace: workspace,
		operation: operation,
		state:     Running,
		created:   m.now(),
		changed:   make(chan struct{}),
		cancel:    cancel,
		jobLimit:  m.opts.Limits.forOperation(operation),
	}
	m.active[workspace] = job
	m.byID[job.id] = job
	m.all = append(m.all, job)
	m.wg.Add(1)
	m.mu.Unlock()

	job.begin()
	go m.run(ctx, job, runner)
	return job, nil
}

func (m *Manager) run(ctx context.Context, job *Job, runner Runner) {
	defer m.wg.Done()
	defer job.cancel(nil)
	run := &Run{job: job}
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = &Failure{Kind: "internal-error", Err: fmt.Errorf("operation panicked: %v", p)}
			}
		}()
		err = runner(ctx, run)
	}()
	run.flush()
	job.finish(err)
}

func toError(err error) *Error {
	kind := "operation-failed"
	var f *Failure
	if errors.As(err, &f) {
		kind = f.Kind
	}
	return &Error{Kind: kind, Message: err.Error()}
}

// Get returns the job with id.
func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.byID[id]
	return job, ok
}

// Active returns the workspace's unfinished job.
func (m *Manager) Active(workspace string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.active[workspace]
	return job, ok
}

// List returns every job of this process, newest first.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	jobs := append([]*Job(nil), m.all...)
	m.mu.Unlock()
	out := make([]Snapshot, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Snapshot())
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// Wait blocks until every started job has finished.
func (m *Manager) Wait() { m.wg.Wait() }

// Job is one operation on one workspace.
type Job struct {
	manager   *Manager
	id        string
	workspace string
	operation string
	created   time.Time

	mu       sync.Mutex
	state    State
	phase    string
	finished *time.Time
	err      *Error
	apply    json.RawMessage
	events   []Event
	changed  chan struct{}

	cancel   context.CancelCauseFunc
	jobLimit time.Duration
	clock    clock
	deadline *time.Time
	limit    *Limit
	timedOut *Error
}

// ID returns the job's identifier.
func (j *Job) ID() string { return j.id }

// Snapshot returns the job's current public view.
func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotLocked()
}

func (j *Job) snapshotLocked() Snapshot {
	return Snapshot{
		JobID:       j.id,
		Workspace:   j.workspace,
		Operation:   j.operation,
		State:       j.state,
		Phase:       j.phase,
		CreatedAt:   j.created,
		FinishedAt:  j.finished,
		LastEventID: int64(len(j.events)),
		Error:       j.err,
		Deadline:    j.deadline,
		Limit:       j.limit,
	}
}

// EventsAfter returns the events whose ID is greater than after, whether
// the job has ended (so no event follows the returned ones), and a channel
// closed when the next event is appended.
func (j *Job) EventsAfter(after int64) ([]Event, bool, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if after < 0 {
		after = 0
	}
	var events []Event
	if after < int64(len(j.events)) {
		events = append(events, j.events[after:]...)
	}
	return events, j.state.Terminal(), j.changed
}

type stateData struct {
	State    State      `json:"state"`
	Phase    string     `json:"phase"`
	Deadline *time.Time `json:"deadline"`
	Limit    *Limit     `json:"limit"`
}

type resultData struct {
	State State           `json:"state"`
	Error *Error          `json:"error"`
	Apply json.RawMessage `json:"apply,omitempty"`
}

func (j *Job) appendEvent(eventType string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		raw, _ = json.Marshal(map[string]string{"marshalError": err.Error()})
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendLocked(eventType, raw)
}

func (j *Job) appendLocked(eventType string, raw json.RawMessage) {
	j.events = append(j.events, Event{ID: int64(len(j.events)) + 1, Type: eventType, Data: raw})
	close(j.changed)
	j.changed = make(chan struct{})
}

// begin publishes the first state event with the job's first deadline.
func (j *Job) begin() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.clock.resume(j.created)
	j.scheduleLocked(j.created)
	j.appendStateLocked()
}

// setState records a nonterminal state or phase change. enter restarts the
// phase's running time, as entering a phase does. A limit or threshold
// that the outgoing stretch passed is settled first. It is ignored once
// the job has ended.
func (j *Job) setState(state State, phase string, enter bool) {
	now := j.manager.now()
	j.mu.Lock()
	if j.state.Terminal() {
		j.mu.Unlock()
		return
	}
	alert := j.settleLocked(now)
	j.clock.pause(now)
	if enter {
		j.clock.phase = 0
	}
	if state == Running {
		j.clock.resume(now)
	}
	j.state, j.phase = state, phase
	j.scheduleLocked(now)
	j.appendStateLocked()
	j.mu.Unlock()
	j.manager.alertLongRunning(alert)
}

func (j *Job) appendStateLocked() {
	raw, _ := json.Marshal(stateData{State: j.state, Phase: j.phase, Deadline: j.deadline, Limit: j.limit})
	j.appendLocked(EventState, raw)
}

// finish ends the job once, with the outcome of its runner's error. The
// outcome is chosen under the job lock that timers take, after settling
// what the running time passed, so a limit that passed before the end
// always wins. The workspace slot is released and the result event
// published under the manager lock, so no observer can see the result
// while the workspace still looks busy.
func (j *Job) finish(runErr error) {
	m := j.manager
	now := m.now()
	m.mu.Lock()
	j.mu.Lock()
	if j.state.Terminal() {
		j.mu.Unlock()
		m.mu.Unlock()
		return
	}
	longRunning := j.settleLocked(now)
	state, jobErr := j.outcomeLocked(runErr)
	j.stopTimerLocked()
	j.deadline, j.limit = nil, nil
	raw, err := marshalCompact(resultData{State: state, Error: jobErr, Apply: j.apply})
	if err != nil {
		raw, _ = json.Marshal(resultData{State: state, Error: jobErr})
	}
	if m.active[j.workspace] == j {
		delete(m.active, j.workspace)
	}
	j.state, j.err, j.finished = state, jobErr, &now
	j.appendLocked(EventResult, raw)
	snap := j.snapshotLocked()
	j.mu.Unlock()
	m.mu.Unlock()
	m.alertLongRunning(longRunning)
	if state == Failed && m.opts.Alerts != nil {
		m.opts.Alerts.JobFailed(snap)
	}
}

// marshalCompact encodes v without HTML escaping, so opaque payloads keep
// their text.
func marshalCompact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("job id: %v", err))
	}
	return hex.EncodeToString(b[:])
}
