// ---
// relationships:
//   implements: menagerie
//   references: [template-service-api, browser-session-value-tampering]
// ---

// Package templates is the workspace service's client of the template
// service: JSON over HTTP/1.1 on the template service's Unix socket. Toha's
// session and documents are opaque: they travel as the exact JSON text the
// template service produced, never decoded into a lossy form.
package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Template service routes.
const (
	pathInterviews       = "/v1/interviews"
	pathInterviewAnswers = "/v1/interviews/answers"
	pathGenerate         = "/v1/workspaces/generate"
	pathUpgrades         = "/v1/upgrades"
	pathUpgradeAnswers   = "/v1/upgrades/answers"
	pathUpgradeApply     = "/v1/upgrades/apply"
)

// maxReply bounds a reply body read from the template service.
const maxReply = 64 << 20

// Selector names the workspace, its folder and the approved template every
// request carries.
type Selector struct {
	// Workspace is the workspace folder name.
	Workspace string `json:"workspace"`
	// Target is the workspace folder: the workspace root joined with
	// Workspace.
	Target string `json:"target"`
	// TemplateID is an approved template's id.
	TemplateID string `json:"templateId"`
}

// InterviewState is the step an interview reply reports.
type InterviewState string

// Interview states.
const (
	Questions InterviewState = "questions"
	Completed InterviewState = "completed"
	Ended     InterviewState = "ended"
)

// Reply is an interview reply. Raw is the reply's whole JSON text as the
// template service sent it, compacted; Session and Document are the same
// text's members.
type Reply struct {
	Raw      json.RawMessage
	Session  json.RawMessage
	State    InterviewState
	Document json.RawMessage
	Warnings []string
}

// Outcome is a generation's or an upgrade apply's result.
type Outcome string

// Apply outcomes. Only Written and Conflicted wrote files and the applied
// record; Conflicted wrote conflict markers into Applied.Conflicts.
const (
	Written        Outcome = "written"
	Conflicted     Outcome = "conflicted"
	AlreadyCurrent Outcome = "already-current"
	NeedsTrust     Outcome = "needs-trust"
	Planned        Outcome = "planned"
)

// Wrote reports whether the outcome wrote files and the applied record.
func (o Outcome) Wrote() bool { return o == Written || o == Conflicted }

// File is one path a generation or apply reports, with its action.
type File struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// Hook is one template hook that ran. Hook output is not returned.
type Hook struct {
	ID       *string `json:"id"`
	ExitCode *int    `json:"exitCode"`
	Success  bool    `json:"success"`
}

// Applied is a generation's or an upgrade apply's reply. Raw is the reply's
// whole JSON text, compacted.
type Applied struct {
	Raw        json.RawMessage `json:"-"`
	Outcome    Outcome         `json:"outcome"`
	Files      []File          `json:"files"`
	RecordPath *string         `json:"recordPath"`
	Messages   []string        `json:"messages"`
	Hooks      []Hook          `json:"hooks"`
	Conflicts  []string        `json:"conflicts"`
}

// Error is a template service refusal.
type Error struct {
	Status    int      `json:"-"`
	Kind      string   `json:"kind"`
	Message   string   `json:"message"`
	Paths     []string `json:"paths"`
	Locations []string `json:"locations"`
	Questions []string `json:"questions"`
	Warnings  []string `json:"warnings"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("template service refused the request (%d %s): %s", e.Status, e.Kind, e.Message)
}

// UnavailableError reports that the template service could not be reached
// or did not answer with a well-formed reply.
type UnavailableError struct{ Err error }

func (e *UnavailableError) Error() string { return "template service unavailable: " + e.Err.Error() }
func (e *UnavailableError) Unwrap() error { return e.Err }

// Client calls the template service on its Unix socket.
type Client struct {
	http *http.Client
}

// New returns a Client of the template service listening on socket.
func New(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// StartInterview starts a create interview for a workspace folder that
// does not exist yet.
func (c *Client) StartInterview(ctx context.Context, s Selector) (*Reply, error) {
	return c.interview(ctx, pathInterviews, s)
}

// AnswerInterview submits one batch of a create interview. session is the
// latest reply's session and submission Toha's answers envelope; both are
// forwarded as received.
func (c *Client) AnswerInterview(ctx context.Context, s Selector, session, submission json.RawMessage) (*Reply, error) {
	return c.interview(ctx, pathInterviewAnswers, answerRequest{s, session, submission})
}

// Generate applies a completed create session into the initialized
// workspace folder.
func (c *Client) Generate(ctx context.Context, s Selector, session json.RawMessage) (*Applied, error) {
	return c.apply(ctx, pathGenerate, applyRequest{s, session})
}

// StartUpgrade starts an upgrade of an existing workspace to s.TemplateID.
func (c *Client) StartUpgrade(ctx context.Context, s Selector) (*Reply, error) {
	return c.interview(ctx, pathUpgrades, s)
}

// AnswerUpgrade submits one batch of an upgrade interview.
func (c *Client) AnswerUpgrade(ctx context.Context, s Selector, session, submission json.RawMessage) (*Reply, error) {
	return c.interview(ctx, pathUpgradeAnswers, answerRequest{s, session, submission})
}

// ApplyUpgrade merges a completed upgrade session into the workspace.
func (c *Client) ApplyUpgrade(ctx context.Context, s Selector, session json.RawMessage) (*Applied, error) {
	return c.apply(ctx, pathUpgradeApply, applyRequest{s, session})
}

type answerRequest struct {
	Selector
	Session    json.RawMessage `json:"session"`
	Submission json.RawMessage `json:"submission"`
}

type applyRequest struct {
	Selector
	Session json.RawMessage `json:"session"`
}

func (c *Client) interview(ctx context.Context, path string, body any) (*Reply, error) {
	raw, err := c.post(ctx, path, body)
	if err != nil {
		return nil, err
	}
	var members struct {
		Session  json.RawMessage `json:"session"`
		State    InterviewState  `json:"state"`
		Document json.RawMessage `json:"document"`
		Warnings []string        `json:"warnings"`
	}
	if err := strictDecode(raw, &members); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("interview reply: %w", err)}
	}
	switch members.State {
	case Questions, Completed, Ended:
	default:
		return nil, &UnavailableError{Err: fmt.Errorf("interview reply has state %q", members.State)}
	}
	if len(members.Session) == 0 || members.Session[0] != '{' {
		return nil, &UnavailableError{Err: errors.New("interview reply has no session object")}
	}
	return &Reply{Raw: raw, Session: members.Session, State: members.State, Document: members.Document, Warnings: members.Warnings}, nil
}

func (c *Client) apply(ctx context.Context, path string, body any) (*Applied, error) {
	raw, err := c.post(ctx, path, body)
	if err != nil {
		return nil, err
	}
	var applied Applied
	if err := strictDecode(raw, &applied); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("apply reply: %w", err)}
	}
	switch applied.Outcome {
	case Written, Conflicted:
		if applied.RecordPath == nil || *applied.RecordPath == "" {
			return nil, &UnavailableError{Err: fmt.Errorf("apply reply %s names no applied record", applied.Outcome)}
		}
	case AlreadyCurrent, NeedsTrust, Planned:
	default:
		return nil, &UnavailableError{Err: fmt.Errorf("apply reply has outcome %q", applied.Outcome)}
	}
	applied.Raw = raw
	return &applied, nil
}

// post sends body and returns the compacted reply text of a 200 answer, or
// the template service's *Error, or an *UnavailableError.
func (c *Client) post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// The session and submission keep their text: no HTML escaping.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, fmt.Errorf("encode template service request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://template-service"+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &UnavailableError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("read reply: %w", err)}
	}
	if len(data) > maxReply {
		return nil, &UnavailableError{Err: fmt.Errorf("reply exceeds %d bytes", maxReply)}
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return nil, &UnavailableError{Err: fmt.Errorf("reply %d is not JSON", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error *Error `json:"error"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil || envelope.Error == nil || envelope.Error.Kind == "" {
			return nil, &UnavailableError{Err: fmt.Errorf("reply %d has no error envelope", resp.StatusCode)}
		}
		envelope.Error.Status = resp.StatusCode
		return nil, envelope.Error
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("reply is not JSON: %w", err)}
	}
	return compact.Bytes(), nil
}

// strictDecode decodes one JSON object into v, refusing unknown members.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the reply")
	}
	return nil
}
