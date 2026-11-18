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
	pathAddresses        = "/v1/templates/addresses"
	pathVersions         = "/v1/templates/versions"
	pathChecks           = "/v1/templates/checks"
	pathInterviews       = "/v1/interviews"
	pathInterviewAnswers = "/v1/interviews/answers"
	pathGenerate         = "/v1/workspaces/generate"
	pathUpgrades         = "/v1/upgrades"
	pathUpgradeAnswers   = "/v1/upgrades/answers"
	pathUpgradeApply     = "/v1/upgrades/apply"
)

// Selector names the workspace, its folder and the template version every
// interview request carries. The workspace service admits the template
// before it names it: a registered template's address with the tag and
// commit of its current version, which the job holds.
type Selector struct {
	// Workspace is the workspace folder name.
	Workspace string `json:"workspace"`
	// Target is the workspace folder: the workspace root joined with
	// Workspace.
	Target string `json:"target"`
	// Address is the template's address without a ref.
	Address string `json:"address"`
	// Ref is the version's tag.
	Ref string `json:"ref"`
	// Commit is the commit recorded for Ref, or "" when none is recorded
	// yet. A ref that resolves to another commit is refused as
	// template-stale-revision.
	Commit string `json:"commit,omitempty"`
}

// Address is what the template service's reading of an address gives,
// reaching no network.
type Address struct {
	// Source is Toha's formal name of the repository and folder without a
	// ref, the source an applied record names.
	Source string `json:"source"`
	// Reference is the ref the address names, or nil.
	Reference *string `json:"reference"`
	// Base is the address as given without its ref; an https address also
	// loses its user information.
	Base string `json:"base"`
}

// Version is one version of a repository: its numbers, its tag and the
// commit the tag names.
type Version struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Commit  string `json:"commit"`
}

// Versions is a repository's versions in ascending order.
type Versions struct {
	Source   string    `json:"source"`
	Versions []Version `json:"versions"`
}

// Checked is one checked version: the commit its ref resolved to, and
// whether the template's content folder holds the devcontainer definition.
type Checked struct {
	Source     string `json:"source"`
	Commit     string `json:"commit"`
	Definition bool   `json:"definition"`
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

// ReadAddress reads an address with Toha's grammar, reaching no network.
func (c *Client) ReadAddress(ctx context.Context, address string) (*Address, error) {
	var out Address
	if err := c.call(ctx, pathAddresses, map[string]string{"address": address}, &out); err != nil {
		return nil, err
	}
	if out.Source == "" || out.Base == "" {
		return nil, &UnavailableError{Err: errors.New("address reply names no source or base")}
	}
	return &out, nil
}

// ListVersions lists the versions of the repository address, which names
// no ref, names.
func (c *Client) ListVersions(ctx context.Context, address string) (*Versions, error) {
	var out Versions
	if err := c.call(ctx, pathVersions, map[string]string{"address": address}, &out); err != nil {
		return nil, err
	}
	if out.Source == "" || out.Versions == nil {
		return nil, &UnavailableError{Err: errors.New("versions reply names no source or versions")}
	}
	return &out, nil
}

// Check fetches address at ref and reads it, running nothing of it.
func (c *Client) Check(ctx context.Context, address, ref string) (*Checked, error) {
	var out Checked
	if err := c.call(ctx, pathChecks, map[string]string{"address": address, "ref": ref}, &out); err != nil {
		return nil, err
	}
	if out.Source == "" || out.Commit == "" {
		return nil, &UnavailableError{Err: errors.New("check reply names no source or commit")}
	}
	return &out, nil
}

// call posts body and strictly decodes a 200 reply into out.
func (c *Client) call(ctx context.Context, path string, body, out any) error {
	raw, err := c.post(ctx, path, body)
	if err != nil {
		return err
	}
	if err := strictDecode(raw, out); err != nil {
		return &UnavailableError{Err: fmt.Errorf("%s reply: %w", path, err)}
	}
	return nil
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
// workspace folder. hookToken is the hook grant the template service runs
// the template's hooks with; empty refuses every hook.
func (c *Client) Generate(ctx context.Context, s Selector, session json.RawMessage, hookToken string) (*Applied, error) {
	return c.apply(ctx, pathGenerate, applyRequest{s, session, hookToken})
}

// StartUpgrade starts an upgrade of an existing workspace to the version s
// names.
// With reanswer it asks every question again, with the workspace's recorded
// answers as the defaults; without it, only what they do not settle.
func (c *Client) StartUpgrade(ctx context.Context, s Selector, reanswer bool) (*Reply, error) {
	return c.interview(ctx, pathUpgrades, upgradeRequest{s, reanswer})
}

type upgradeRequest struct {
	Selector
	Reanswer bool `json:"reanswer,omitempty"`
}

// AnswerUpgrade submits one batch of an upgrade interview.
func (c *Client) AnswerUpgrade(ctx context.Context, s Selector, session, submission json.RawMessage) (*Reply, error) {
	return c.interview(ctx, pathUpgradeAnswers, answerRequest{s, session, submission})
}

// ApplyUpgrade merges a completed upgrade session into the workspace,
// running the template's hooks with hookToken.
func (c *Client) ApplyUpgrade(ctx context.Context, s Selector, session json.RawMessage, hookToken string) (*Applied, error) {
	return c.apply(ctx, pathUpgradeApply, applyRequest{s, session, hookToken})
}

type answerRequest struct {
	Selector
	Session    json.RawMessage `json:"session"`
	Submission json.RawMessage `json:"submission"`
}

type applyRequest struct {
	Selector
	Session   json.RawMessage `json:"session"`
	HookToken string          `json:"hookToken,omitempty"`
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
	// A reply carries the whole interview session, which has no size
	// bound in the template service; it is read whole.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("read reply: %w", err)}
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

// strictDecode decodes a compacted reply into v, refusing unknown members.
// Compacting has already refused anything after the reply's JSON value.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
