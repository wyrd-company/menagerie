// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, job-without-timeout-blocks-workspace]
// ---

// Package jobs runs workspace operations as in-memory jobs. A workspace has
// at most one job at a time. Jobs, their events and their logs live only in
// this process: a restart drops them. No timeout is applied to a job.
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
	ctx context.Context
	now func() time.Time

	mu     sync.Mutex
	active map[string]*Job
	byID   map[string]*Job
	all    []*Job
	wg     sync.WaitGroup
}

// NewManager returns a Manager whose jobs run with ctx. Cancelling ctx is
// the only way a running job is asked to stop (process shutdown).
func NewManager(ctx context.Context) *Manager {
	return &Manager{ctx: ctx, now: time.Now, active: map[string]*Job{}, byID: map[string]*Job{}}
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
	job := &Job{
		manager:   m,
		id:        newID(),
		workspace: workspace,
		operation: operation,
		state:     Running,
		created:   m.now(),
		changed:   make(chan struct{}),
	}
	m.active[workspace] = job
	m.byID[job.id] = job
	m.all = append(m.all, job)
	m.wg.Add(1)
	m.mu.Unlock()

	job.appendEvent(EventState, stateData{State: Running})
	go m.run(job, runner)
	return job, nil
}

func (m *Manager) run(job *Job, runner Runner) {
	defer m.wg.Done()
	run := &Run{job: job}
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = &Failure{Kind: "internal-error", Err: fmt.Errorf("operation panicked: %v", p)}
			}
		}()
		err = runner(m.ctx, run)
	}()
	run.flush()
	switch {
	case errors.Is(err, ErrEnded):
		job.finish(Ended, nil)
	case err != nil:
		job.finish(Failed, toError(err))
	default:
		job.finish(Succeeded, nil)
	}
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
}

// ID returns the job's identifier.
func (j *Job) ID() string { return j.id }

// Snapshot returns the job's current public view.
func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
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
	State State  `json:"state"`
	Phase string `json:"phase"`
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

// setState records a nonterminal state or phase change. It is ignored once
// the job has ended.
func (j *Job) setState(state State, phase string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Terminal() {
		return
	}
	j.state, j.phase = state, phase
	raw, _ := json.Marshal(stateData{State: state, Phase: phase})
	j.appendLocked(EventState, raw)
}

// finish ends the job once. The workspace slot is released and the result
// event published under the manager lock, so no observer can see the result
// while the workspace still looks busy.
func (j *Job) finish(state State, jobErr *Error) {
	m := j.manager
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Terminal() {
		return
	}
	raw, err := marshalCompact(resultData{State: state, Error: jobErr, Apply: j.apply})
	if err != nil {
		raw, _ = json.Marshal(resultData{State: state, Error: jobErr})
	}
	if m.active[j.workspace] == j {
		delete(m.active, j.workspace)
	}
	j.state, j.err, j.finished = state, jobErr, &now
	j.appendLocked(EventResult, raw)
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
