// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, configuration, job-without-timeout-blocks-workspace]
// ---

package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Phases are the phase names that operations publish through Run.Phase. A
// phase time limit names one of them.
var Phases = []string{
	"read-definition",
	"initialize-command",
	"build",
	"remove-container",
	"up",
	"stop",
	"interview",
	"initialize-repository",
	"generate",
	"fetch",
	"plan",
	"apply",
	"commit",
	"backup-local-changes",
	"backup",
}

// Limits are the time limits on a job's running time. A zero or missing
// duration is no limit. Time in waiting-for-answers does not count, and no
// deadline applies while a job waits.
type Limits struct {
	// Default limits every operation without an entry in Operations.
	Default time.Duration
	// Operations limits a job by its operation name.
	Operations map[string]time.Duration
	// Phases limits each stay in a phase, from the Run.Phase call that
	// enters it, inside any operation.
	Phases map[string]time.Duration
}

func (l Limits) forOperation(operation string) time.Duration {
	if d, ok := l.Operations[operation]; ok {
		return d
	}
	return l.Default
}

// Limit scopes.
const (
	ScopeJob   = "job"
	ScopePhase = "phase"
)

// Limit is the limit whose deadline a snapshot reports.
type Limit struct {
	Seconds float64 `json:"seconds"`
	Scope   string  `json:"scope"`
}

// KindTimedOut is the error kind of a job cancelled at a time limit.
const KindTimedOut = "timed-out"

// ErrTimedOut is the cause of a runner's context cancelled at a time limit.
var ErrTimedOut = errors.New("job time limit passed")

// Alerts receives the jobs worth telling someone about. Its methods are
// called with the job's lock held, so that a job's alerts arrive in the
// order of the changes that raised them: they must not block and must not
// call the Manager or the Job.
type Alerts interface {
	// JobFailed is called once for a job that ends failed.
	JobFailed(Snapshot)
	// JobLongRunning is called once for a job whose running time passes
	// Options.LongRunning.
	JobLongRunning(Snapshot)
}

// Timer is a pending call scheduled by Options.AfterFunc.
type Timer interface {
	Stop() bool
}

// Options configure a Manager. The zero value applies no limit and sends
// no alert.
type Options struct {
	Limits Limits
	// LongRunning, when positive, is the running time after which Alerts
	// hears of a job once.
	LongRunning time.Duration
	Alerts      Alerts
	// Now and AfterFunc replace time.Now and time.AfterFunc in tests.
	Now       func() time.Time
	AfterFunc func(time.Duration, func()) Timer
}

// clock counts a job's running time and the running time of its current
// phase. since is the start of the current running stretch, zero while
// the job waits.
type clock struct {
	total time.Duration
	phase time.Duration
	since time.Time

	timer       Timer
	longRunning bool
}

func (c *clock) pause(now time.Time) {
	if !c.since.IsZero() {
		d := now.Sub(c.since)
		c.total += d
		c.phase += d
	}
	c.since = time.Time{}
}

func (c *clock) resume(now time.Time) { c.since = now }

// due is the earliest pending limit: its remaining running time, its
// scope and its length. ok is false when no limit applies.
type due struct {
	remaining time.Duration
	limit     time.Duration
	scope     string
}

// firstDueLocked returns the limit that ends first, preferring the phase
// limit on a tie. The clock must be paused or just resumed at now.
func (j *Job) firstDueLocked() (due, bool) {
	var first due
	ok := false
	if d := j.manager.opts.Limits.Phases[j.phase]; d > 0 {
		first, ok = due{remaining: d - j.clock.phase, limit: d, scope: ScopePhase}, true
	}
	if d := j.jobLimit; d > 0 {
		if r := d - j.clock.total; !ok || r < first.remaining {
			first, ok = due{remaining: r, limit: d, scope: ScopeJob}, true
		}
	}
	return first, ok
}

func (j *Job) stopTimerLocked() {
	if j.clock.timer != nil {
		j.clock.timer.Stop()
		j.clock.timer = nil
	}
}

// scheduleLocked sets the limit and deadline and arms one timer for the
// earliest of the first limit and the long-running threshold. The clock
// must have been brought up to now. A waiting job keeps its limit but has
// neither deadline nor timer.
func (j *Job) scheduleLocked(now time.Time) {
	if j.timedOut != nil {
		return
	}
	j.stopTimerLocked()
	j.deadline, j.limit = nil, nil
	first, limited := j.firstDueLocked()
	if limited {
		j.limit = &Limit{Seconds: first.limit.Seconds(), Scope: first.scope}
	}
	if j.clock.since.IsZero() {
		return
	}
	next, armed := time.Duration(0), false
	if limited {
		at := now.Add(first.remaining)
		j.deadline = &at
		next, armed = first.remaining, true
	}
	if lr := j.manager.opts.LongRunning; lr > 0 && !j.clock.longRunning {
		if r := lr - j.clock.total; !armed || r < next {
			next, armed = r, true
		}
	}
	if !armed {
		return
	}
	j.clock.timer = j.manager.afterFunc(max(next, 0), j.tick)
}

// tick runs when a scheduled limit or threshold may have passed. It reads
// the clock again, so a tick that a later schedule superseded only
// reschedules.
func (j *Job) tick() {
	now := j.manager.now()
	j.mu.Lock()
	if !j.countingLocked() {
		j.mu.Unlock()
		return
	}
	j.settleLocked(now)
	j.scheduleLocked(now)
	j.mu.Unlock()
}

// countingLocked reports whether the job's limits still count: it is
// running, has not ended or timed out, and the process is not shutting
// down, so shutdown is never a timeout.
func (j *Job) countingLocked() bool {
	return !j.state.Terminal() && j.timedOut == nil && !j.clock.since.IsZero() && j.manager.ctx.Err() == nil
}

// settleLocked brings the clock up to now and settles what has passed by
// then, whether or not its timer has run: a passed long-running threshold
// is alerted, and a passed limit times the job out. It runs before every
// change that ends the running stretch, its phase or the job, so that a
// late timer loses nothing.
func (j *Job) settleLocked(now time.Time) {
	if !j.countingLocked() {
		return
	}
	j.clock.pause(now)
	j.clock.resume(now)
	if lr := j.manager.opts.LongRunning; lr > 0 && !j.clock.longRunning && j.clock.total >= lr {
		j.clock.longRunning = true
		if a := j.manager.opts.Alerts; a != nil {
			a.JobLongRunning(j.snapshotLocked())
		}
	}
	if first, ok := j.firstDueLocked(); ok && first.remaining <= 0 {
		j.timeOutLocked(first)
	}
}

// timeOutLocked records the timed-out outcome, logs it, and cancels the
// runner's context. The job ends when the runner returns.
func (j *Job) timeOutLocked(first due) {
	j.stopTimerLocked()
	var msg string
	if first.scope == ScopePhase {
		msg = fmt.Sprintf("the %s phase passed its %s limit", j.phase, FormatLimit(first.limit))
	} else {
		msg = fmt.Sprintf("the job passed its %s limit in the %s phase", FormatLimit(first.limit), j.phase)
		if j.phase == "" {
			msg = fmt.Sprintf("the job passed its %s limit", FormatLimit(first.limit))
		}
	}
	j.timedOut = &Error{Kind: KindTimedOut, Message: msg}
	raw, _ := json.Marshal(logData{Text: msg + "; cancelling the job"})
	j.appendLocked(EventLog, raw)
	j.cancel(fmt.Errorf("%w: %s", ErrTimedOut, msg))
}

// outcomeLocked chooses how the job ends from its runner's error. A job
// timed out before it ends fails as timed-out whatever its runner
// returned.
func (j *Job) outcomeLocked(err error) (State, *Error) {
	switch {
	case j.timedOut != nil:
		return Failed, j.timedOut
	case errors.Is(err, ErrEnded):
		return Ended, nil
	case err != nil:
		return Failed, toError(err)
	}
	return Succeeded, nil
}

// FormatLimit writes d with whole units only, such as 2h, 1h30m or 45s; a
// duration below a second keeps Go's form, such as 100ms.
func FormatLimit(d time.Duration) string {
	if d < time.Second || d%time.Second != 0 {
		return d.String()
	}
	s := d.String()
	s = strings.Replace(s, "m0s", "m", 1)
	s = strings.Replace(s, "h0m", "h", 1)
	return s
}
