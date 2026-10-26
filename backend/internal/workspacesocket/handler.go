// ---
// relationships:
//   implements: workspace-api
//   references: [menagerie, workspace-fills-its-backup-branch]
// ---

package workspacesocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wyrd-company/menagerie/backend/internal/commits"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// CommitsPath is the socket API's one route: POST starts a commit job.
const CommitsPath = "/v1/commits"

// Limits of a commit request.
const (
	// MaxMessage bounds a commit message, in bytes.
	MaxMessage = 4096
	// maxBody bounds a request body.
	maxBody = 16 << 10
)

// Error kinds of the socket API.
const (
	KindInvalidRequest   = "invalid-request"
	KindInvalidMessage   = "invalid-message"
	KindRateLimited      = "rate-limited"
	KindWorkspaceBusy    = "workspace-busy"
	KindWorkspaceMissing = "workspace-not-found"
	KindNotFound         = "not-found"
	KindMethodNotAllowed = "method-not-allowed"
	KindInternal         = "internal-error"
)

// ValidMessage reports whether message is a commit message the socket
// accepts: not blank, at most MaxMessage bytes of UTF-8, with no NUL.
func ValidMessage(message string) bool {
	return strings.TrimSpace(message) != "" && len(message) <= MaxMessage &&
		utf8.ValidString(message) && !strings.ContainsRune(message, 0)
}

// Result is the reply to an accepted commit request: the job's id and end
// state, the workspace branch's commit after the commit phase (null when
// it failed), whether that phase committed, whether the backup changed
// the workspace repository, and the error kind of a failed job.
//
// Every reply of the socket carries kinds, ids, hashes, numbers and
// booleans only: never a message, a path or the remote's address. The
// job's console has the detail.
type Result struct {
	JobID   string     `json:"jobId"`
	State   jobs.State `json:"state"`
	Commit  *string    `json:"commit"`
	Changed bool       `json:"changed"`
	Pushed  bool       `json:"pushed"`
	Error   *Error     `json:"error"`
}

// Error is a refusal or a failed job's kind.
type Error struct {
	Kind string `json:"kind"`
	// ActiveJobID is the busy workspace's job.
	ActiveJobID string `json:"activeJobId,omitempty"`
	// RetryAfterSeconds is when a rate-limited request may be repeated.
	RetryAfterSeconds int `json:"retryAfterSeconds,omitempty"`
}

type request struct {
	Message *string `json:"message"`
}

// handler serves one workspace's socket. name is bound here, when the
// listener is created; no part of a request selects a workspace.
func (s *Sockets) handler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CommitsPath {
			refuse(w, http.StatusNotFound, Error{Kind: KindNotFound})
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			refuse(w, http.StatusMethodNotAllowed, Error{Kind: KindMethodNotAllowed})
			return
		}
		message, kind := readMessage(w, r)
		if kind != "" {
			refuse(w, http.StatusBadRequest, Error{Kind: kind})
			return
		}
		job, outcome, wait, err := s.start(name, message)
		var busy *jobs.BusyError
		switch {
		case wait > 0:
			seconds := int(math.Ceil(wait.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			refuse(w, http.StatusTooManyRequests, Error{Kind: KindRateLimited, RetryAfterSeconds: seconds})
			return
		case errors.As(err, &busy):
			refuse(w, http.StatusConflict, Error{Kind: KindWorkspaceBusy, ActiveJobID: busy.ActiveJobID})
			return
		case errors.Is(err, workspace.ErrNotFound):
			refuse(w, http.StatusNotFound, Error{Kind: KindWorkspaceMissing})
			return
		case err != nil:
			refuse(w, http.StatusInternalServerError, Error{Kind: KindInternal})
			return
		}
		// A client that goes away does not cancel the job.
		if !ended(r.Context(), job) {
			return
		}
		snap := job.Snapshot()
		res := Result{JobID: snap.JobID, State: snap.State}
		commit, changed, pushed := outcome.Result()
		if commit != "" {
			res.Commit = &commit
		}
		res.Changed, res.Pushed = changed, pushed
		if snap.Error != nil {
			res.Error = &Error{Kind: snap.Error.Kind}
		}
		reply(w, http.StatusOK, res)
	})
}

// start starts the commit job unless the workspace's last accepted commit
// is within the minimum interval, in which case wait is how long is left.
// The check and the record are one step, so two requests cannot both pass.
func (s *Sockets) start(name, message string) (*jobs.Job, *commits.Outcome, time.Duration, error) {
	s.limits.mu.Lock()
	defer s.limits.mu.Unlock()
	if wait := s.limits.wait(name); wait > 0 {
		return nil, nil, wait, nil
	}
	job, outcome, err := s.opts.Commits.Start(name, message)
	if err != nil {
		return nil, nil, 0, err
	}
	s.limits.accept(name)
	return job, outcome, 0, nil
}

// readMessage decodes the request's message, or names why it cannot.
func readMessage(w http.ResponseWriter, r *http.Request) (string, string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return "", KindInvalidRequest
	}
	if !utf8.Valid(body) {
		return "", KindInvalidMessage
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req request
	if err := dec.Decode(&req); err != nil {
		return "", KindInvalidRequest
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return "", KindInvalidRequest
	}
	if req.Message == nil || !ValidMessage(*req.Message) {
		return "", KindInvalidMessage
	}
	return *req.Message, ""
}

// ended waits until job ends, or reports false when ctx ends first.
func ended(ctx context.Context, job *jobs.Job) bool {
	for {
		_, done, changed := job.EventsAfter(math.MaxInt64)
		if done {
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

func refuse(w http.ResponseWriter, status int, e Error) {
	reply(w, status, map[string]Error{"error": e})
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// limiter keeps each workspace's last accepted commit.
type limiter struct {
	mu   sync.Mutex
	min  time.Duration
	now  func() time.Time
	last map[string]time.Time
}

func newLimiter(min time.Duration, now func() time.Time) *limiter {
	return &limiter{min: min, now: now, last: map[string]time.Time{}}
}

// wait is how long name must wait for its next commit; zero when it may
// commit now. The caller holds mu.
func (l *limiter) wait(name string) time.Duration {
	last, ok := l.last[name]
	if l.min <= 0 || !ok {
		return 0
	}
	if left := l.min - l.now().Sub(last); left > 0 {
		return left
	}
	return 0
}

// accept records an accepted commit of name. The caller holds mu.
func (l *limiter) accept(name string) { l.last[name] = l.now() }
