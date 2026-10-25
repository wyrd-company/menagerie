// ---
// relationships:
//   implements: workspace-api
//   references: [menagerie, browser-session-value-tampering]
// ---

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Interviews takes the answers of interview jobs.
type Interviews interface {
	// Answer submits one batch to the job's interview and returns the next
	// interview reply's exact text. A job that is not waiting for answers
	// returns a *RequestError with job-not-waiting.
	Answer(ctx context.Context, jobID string, session, submission json.RawMessage) (json.RawMessage, error)
}

// ErrNotWaiting is the job-not-waiting refusal.
var ErrNotWaiting = &RequestError{Status: http.StatusConflict, Kind: "job-not-waiting", Message: "the job is not waiting for answers"}

// AnswerRequest is the body of POST /api/jobs/{id}/answers. Both members
// are opaque JSON objects, forwarded as received.
type AnswerRequest struct {
	Session    json.RawMessage `json:"session"`
	Submission json.RawMessage `json:"submission"`
}

func (s *Server) answerJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.opts.Jobs.Get(id); !ok {
		writeError(w, http.StatusNotFound, apiError{Kind: "job-not-found", Message: "no job with this id in this process"})
		return
	}
	// The body carries the whole interview session, which the template
	// service accepts at any size, so it is not held to the bound of a job
	// request.
	var req AnswerRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: err.Error()})
		return
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "request body must hold exactly one JSON document"})
		return
	}
	if !isObject(req.Session) || !isObject(req.Submission) {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "session and submission must be JSON objects"})
		return
	}
	if s.opts.Interviews == nil {
		writeError(w, ErrNotWaiting.Status, apiError{Kind: ErrNotWaiting.Kind, Message: ErrNotWaiting.Message})
		return
	}
	reply, err := s.opts.Interviews.Answer(r.Context(), id, req.Session, req.Submission)
	var re *RequestError
	switch {
	case errors.As(err, &re):
		writeError(w, re.Status, apiError{Kind: re.Kind, Message: re.Message})
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply)
}

func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}
