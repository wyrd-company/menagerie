// ---
// relationships:
//   implements: menagerie
//   references: workspace-api
// ---

package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Run is a runner's handle to its job.
type Run struct {
	job *Job

	mu      sync.Mutex
	writers []*lineWriter
}

// JobID returns the identifier of the run's job.
func (r *Run) JobID() string { return r.job.id }

// Phase records the step the job is in. It keeps the job running, and the
// phase's time limit counts from this call.
func (r *Run) Phase(name string) { r.job.setState(Running, name, true) }

// SetState records a nonterminal state, such as waiting-for-answers, with
// the current phase. Time limits pause while the job waits. Terminal states are set by the runner's return.
func (r *Run) SetState(state State) error {
	if state.Terminal() {
		return fmt.Errorf("state %s is terminal; return from the runner instead", state)
	}
	r.job.mu.Lock()
	phase := r.job.phase
	r.job.mu.Unlock()
	r.job.setState(state, phase, false)
	return nil
}

// Emit appends an event of any type with an opaque JSON payload, for
// example an interview batch. The payload is compacted to one line, as an
// event stream data field requires; invalid JSON is refused.
func (r *Run) Emit(eventType string, data json.RawMessage) error {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return fmt.Errorf("event %s: %w", eventType, err)
	}
	r.job.mu.Lock()
	defer r.job.mu.Unlock()
	r.job.appendLocked(eventType, compact.Bytes())
	return nil
}

// SetApply records the template apply outcome reported as the result
// event's apply member. The payload is opaque JSON, compacted to one line;
// invalid JSON is refused. A later call replaces an earlier one.
func (r *Run) SetApply(data json.RawMessage) error {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return fmt.Errorf("apply result: %w", err)
	}
	r.job.mu.Lock()
	defer r.job.mu.Unlock()
	r.job.apply = compact.Bytes()
	return nil
}

// Log returns a writer that appends each line written to it as a log
// event. A line ends at a newline, a carriage return, or both together,
// so each progress update a carriage return ends is an event of its own.
// Every caller gets its own line buffer, so concurrent writers do not
// interleave within a line. prefix is added to each line.
func (r *Run) Log(prefix string) io.Writer {
	w := &lineWriter{job: r.job, prefix: prefix}
	r.mu.Lock()
	r.writers = append(r.writers, w)
	r.mu.Unlock()
	return w
}

// flush writes every unterminated line before the job ends.
func (r *Run) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.writers {
		w.flush()
	}
}

type logData struct {
	Text string `json:"text"`
}

type lineWriter struct {
	job    *Job
	prefix string

	mu  sync.Mutex
	buf []byte
	// cr is set when the last line ended at a carriage return, so a
	// newline that follows it, in this write or the next, ends no line.
	// Every line end sets it again; until then the buffer starts with the
	// same bytes, so a stale cr finds no newline to remove.
	cr bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		if w.cr {
			w.buf = bytes.TrimPrefix(w.buf, []byte("\n"))
		}
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			return len(p), nil
		}
		w.cr = w.buf[i] == '\r'
		w.job.appendEvent(EventLog, logData{Text: w.prefix + string(w.buf[:i])})
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.job.appendEvent(EventLog, logData{Text: w.prefix + string(w.buf)})
		w.buf = nil
	}
}
