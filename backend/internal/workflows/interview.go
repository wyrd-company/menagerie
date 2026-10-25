// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, template-service-api, browser-session-value-tampering]
// ---

package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

type jobsRunner = jobs.Runner

// interview is a job's answer mailbox. The runner opens it while it waits
// for answers; one answer at a time is admitted and handed to the runner,
// which calls the template service and returns the reply.
type interview struct {
	mu      sync.Mutex
	waiting bool
	turns   chan turn
}

type turn struct {
	session, submission json.RawMessage
	result              chan turnResult
}

type turnResult struct {
	reply json.RawMessage
	err   error
}

// EventInterview is the job event type that carries interview replies.
const EventInterview = "interview"

// Answer submits one batch to a waiting interview job and returns the next
// reply's exact text. It implements api.Interviews. A job that is not
// waiting, or that is already handling an answer, is refused with
// job-not-waiting; a template service refusal keeps its status and kind,
// and the job keeps waiting.
func (w *Workflows) Answer(ctx context.Context, jobID string, session, submission json.RawMessage) (json.RawMessage, error) {
	w.mu.Lock()
	iv := w.interviews[jobID]
	w.mu.Unlock()
	if iv == nil {
		return nil, api.ErrNotWaiting
	}
	iv.mu.Lock()
	if !iv.waiting {
		iv.mu.Unlock()
		return nil, api.ErrNotWaiting
	}
	iv.waiting = false
	t := turn{session: session, submission: submission, result: make(chan turnResult, 1)}
	iv.turns <- t
	iv.mu.Unlock()
	select {
	case r := <-t.result:
		return r.reply, r.err
	case <-ctx.Done():
		// The runner still handles the answer and appends its reply as an
		// interview event.
		return nil, ctx.Err()
	}
}

func (w *Workflows) open(jobID string) *interview {
	iv := &interview{turns: make(chan turn, 1)}
	w.mu.Lock()
	w.interviews[jobID] = iv
	w.mu.Unlock()
	return iv
}

// close removes the mailbox and refuses an answer admitted but not taken.
func (w *Workflows) close(jobID string, iv *interview) {
	w.mu.Lock()
	delete(w.interviews, jobID)
	w.mu.Unlock()
	iv.mu.Lock()
	defer iv.mu.Unlock()
	iv.waiting = false
	select {
	case t := <-iv.turns:
		t.result <- turnResult{err: api.ErrNotWaiting}
	default:
	}
}

// interviewSteps are the template service calls of one interview kind.
type interviewSteps struct {
	start  func(ctx context.Context) (*templates.Reply, error)
	answer func(ctx context.Context, session, submission json.RawMessage) (*templates.Reply, error)
}

// interview runs a template interview to its completed reply. Each reply
// is appended as an interview event. While questions remain the job waits
// for answers, with no expiry. A flow stop or abort returns jobs.ErrEnded:
// nothing has been written.
func (w *Workflows) interview(ctx context.Context, run *jobs.Run, steps interviewSteps) (*templates.Reply, error) {
	run.Phase("interview")
	reply, err := steps.start(ctx)
	if err != nil {
		return nil, templateFailure("the template service refused the interview", err)
	}
	if err := run.Emit(EventInterview, reply.Raw); err != nil {
		return nil, err
	}
	iv := w.open(run.JobID())
	defer w.close(run.JobID(), iv)
	for {
		switch reply.State {
		case templates.Completed:
			return reply, nil
		case templates.Ended:
			fmt.Fprintln(run.Log(""), "the interview ended; nothing was written")
			return nil, jobs.ErrEnded
		}
		iv.mu.Lock()
		iv.waiting = true
		err := run.SetState(jobs.WaitingForAnswers)
		iv.mu.Unlock()
		if err != nil {
			return nil, err
		}
		var t turn
		select {
		case t = <-iv.turns:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		run.Phase("interview")
		next, err := steps.answer(ctx, t.session, t.submission)
		if err != nil {
			t.result <- turnResult{err: answerRefusal(err)}
			continue
		}
		if err := run.Emit(EventInterview, next.Raw); err != nil {
			t.result <- turnResult{err: err}
			return nil, err
		}
		t.result <- turnResult{reply: next.Raw}
		reply = next
	}
}

// answerRefusal maps a template service error on an answer to the API
// error the browser receives. The job keeps waiting.
func answerRefusal(err error) error {
	var refusal *templates.Error
	if errors.As(err, &refusal) {
		return &api.RequestError{Status: refusal.Status, Kind: refusal.Kind, Message: refusal.Message}
	}
	var unavailable *templates.UnavailableError
	if errors.As(err, &unavailable) {
		return &api.RequestError{Status: http.StatusServiceUnavailable, Kind: KindTemplateUnavailable, Message: unavailable.Error()}
	}
	return err
}

// templateFailure fails a job with the template service's refusal.
func templateFailure(what string, err error) error {
	var refusal *templates.Error
	if errors.As(err, &refusal) {
		return &jobs.Failure{Kind: KindTemplateRefused, Err: fmt.Errorf("%s (%s): %s", what, refusal.Kind, refusal.Message)}
	}
	var unavailable *templates.UnavailableError
	if errors.As(err, &unavailable) {
		return &jobs.Failure{Kind: KindTemplateUnavailable, Err: fmt.Errorf("%s: %w", what, err)}
	}
	return err
}
