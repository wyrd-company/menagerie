// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

// Package xolotl issues and revokes a workspace's Xolotl client key over
// Xolotl's HTTP API. Issuing writes the client key to OpenBao and answers
// without it, so Menagerie never holds a workspace's key. The scoped key that
// authorizes each operation is read from OpenBao for that operation and stays
// in memory. No error holds the scoped key or text from an answer, and this
// package logs nothing.
package xolotl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/openbaokv"
)

const (
	// keyField is the secret's field that holds the scoped key.
	keyField = "api_key"
	// keyDescription is what Xolotl's web interface shows for an issued key.
	keyDescription = "Workspace key issued by Menagerie"
	// maxAnswer bounds the answer read from Xolotl.
	maxAnswer = 1 << 20
	// attempts is how many times one operation sends a request that Xolotl
	// answers with "send it again".
	attempts = 3
	// requestTimeout bounds each read of the scoped key and each request, so
	// that an unresponsive Xolotl reads as unreachable when a job has no
	// time limit.
	requestTimeout = 30 * time.Second
	// retryDelay is the wait before a request is sent again.
	retryDelay = time.Second
)

// namePattern is the form Xolotl gives a policy or key name, which also keeps
// each name to one path segment.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidName reports whether name can be a Xolotl policy or key name.
func ValidName(name string) bool { return len(name) <= 63 && namePattern.MatchString(name) }

// ValidURL accepts an http or https URL with a host and no user information,
// query or fragment. Its error names no value.
func ValidURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("must be an http or https URL with a host and no user information, query or fragment")
	}
	return nil
}

// Error says why an operation on a client key did not finish.
type Error struct {
	// Operation is "issue" or "revoke".
	Operation string
	// Key is the key's policy and name, once they are valid.
	Key string
	// Reason is a fixed text that holds no secret.
	Reason string
	// Unreachable is true when no answer came from Xolotl itself: no
	// connection, no answer in time, or a gateway's answer that Xolotl is
	// not available.
	Unreachable bool
	cause       error
}

func (e *Error) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("Xolotl key %s failed: %s", e.Operation, e.Reason)
	}
	return fmt.Sprintf("Xolotl key %s of %s failed: %s", e.Operation, e.Key, e.Reason)
}

// Unwrap returns the context error of the caller's context that ended the
// operation, if any.
func (e *Error) Unwrap() error { return e.cause }

// SecretReader reads the fields of a secret, as openbaokv.Reader does.
type SecretReader interface {
	Read(ctx context.Context, path string) (map[string]any, error)
}

// Client issues and revokes client keys.
type Client struct {
	base       *url.URL
	secrets    SecretReader
	secretPath string
	http       *http.Client
	timeout    time.Duration
	delay      time.Duration
}

// New returns a Client for the Xolotl at rawURL, whose scoped key is the
// api_key field of the secret at secretPath.
func New(rawURL string, secrets SecretReader, secretPath string) (*Client, error) {
	if err := ValidURL(rawURL); err != nil {
		return nil, fmt.Errorf("xolotl url %w", err)
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("xolotl url could not be read")
	}
	return &Client{
		base:       base,
		secrets:    secrets,
		secretPath: secretPath,
		// A redirect ends the request before net/http reads its Location,
		// so the scoped key goes only to the configured URL.
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		timeout: requestTimeout,
		delay:   retryDelay,
	}, nil
}

// Issued is what Xolotl says about a key after an issue.
type Issued struct {
	// Created is true for a new key, and false for a live key that this
	// call left as it was, so a repeat never rotates a key.
	Created bool
	// Disabled and ExpiresAt describe the stored key, which is not usable
	// if either says so. ExpiresAt is nil for a key with no expiry.
	Disabled  bool
	ExpiresAt *time.Time
}

// Issue issues the key policy/name with no expiry. When the key exists it
// reports so and changes nothing.
func (c *Client) Issue(ctx context.Context, policy, name string) (Issued, error) {
	op := operation{client: c, name: "issue", key: policy + "/" + name}
	if !ValidName(policy) || !ValidName(name) {
		return Issued{}, invalidName("issue")
	}
	body, err := json.Marshal(map[string]string{"policy": policy, "name": name, "description": keyDescription})
	if err != nil {
		return Issued{}, op.fail("the request could not be written")
	}
	code, answer, opErr := op.send(ctx, http.MethodPost, c.base.JoinPath("admin", "keys"), body)
	if opErr != nil {
		return Issued{}, opErr
	}
	var doc struct {
		Status    string     `json:"status"`
		Disabled  bool       `json:"disabled"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if json.Unmarshal(answer, &doc) != nil {
		return Issued{}, op.fail(unexpectedAnswer)
	}
	switch {
	case code == http.StatusCreated && doc.Status == "created":
		return Issued{Created: true, Disabled: doc.Disabled, ExpiresAt: doc.ExpiresAt}, nil
	case code == http.StatusOK && doc.Status == "exists":
		return Issued{Disabled: doc.Disabled, ExpiresAt: doc.ExpiresAt}, nil
	}
	return Issued{}, op.fail(unexpectedAnswer)
}

// Revocation is what a revoke found.
type Revocation string

const (
	// Revoked means that this call revoked a live key.
	Revoked Revocation = "revoked"
	// AlreadyRevoked means that the key was revoked before.
	AlreadyRevoked Revocation = "already_revoked"
	// Absent means that Xolotl has no such key.
	Absent Revocation = "absent"
)

// Revoke revokes the key policy/name. Every Revocation means that no version
// in OpenBao holds the key, so a repeat is safe.
func (c *Client) Revoke(ctx context.Context, policy, name string) (Revocation, error) {
	op := operation{client: c, name: "revoke", key: policy + "/" + name}
	if !ValidName(policy) || !ValidName(name) {
		return "", invalidName("revoke")
	}
	code, answer, opErr := op.send(ctx, http.MethodDelete, c.base.JoinPath("admin", "keys", policy, name), nil)
	if opErr != nil {
		return "", opErr
	}
	var doc struct {
		Status Revocation `json:"status"`
	}
	if code != http.StatusOK || json.Unmarshal(answer, &doc) != nil {
		return "", op.fail(unexpectedAnswer)
	}
	switch doc.Status {
	case Revoked, AlreadyRevoked, Absent:
		return doc.Status, nil
	}
	return "", op.fail(unexpectedAnswer)
}

const unexpectedAnswer = "the answer is not one the Xolotl keys API gives; Xolotl 0.6.0 or later is required"

func invalidName(operation string) *Error {
	return &Error{Operation: operation, Reason: "a policy or key name must be lowercase letters, digits, '-' or '_', at most 63 long, starting with a letter or digit"}
}

// operation is one issue or revoke of one key.
type operation struct {
	client *Client
	name   string
	key    string
}

func (o operation) fail(reason string) *Error {
	return &Error{Operation: o.name, Key: o.key, Reason: reason}
}

// send reads the scoped key and sends the request, again while Xolotl answers
// that it should be sent again. It returns a success status and its body.
func (o operation) send(ctx context.Context, method string, target *url.URL, body []byte) (int, []byte, *Error) {
	key, err := o.scopedKey(ctx)
	if err != nil {
		return 0, nil, err
	}
	for attempt := 1; ; attempt++ {
		code, answer, err := o.request(ctx, method, target, key, body)
		if err != nil {
			return 0, nil, err
		}
		reason, again := o.refusal(code)
		switch {
		case reason == "":
			return code, answer, nil
		case !again || attempt == attempts:
			refused := o.fail(reason)
			refused.Unreachable = code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
			return 0, nil, refused
		}
		select {
		case <-ctx.Done():
			return 0, nil, o.ended(ctx)
		case <-time.After(o.client.delay):
		}
	}
}

// scopedKey reads the scoped key. A failure here says nothing about Xolotl, so
// it is never Unreachable.
func (o operation) scopedKey(ctx context.Context) (string, *Error) {
	read, cancel := context.WithTimeout(ctx, o.client.timeout)
	defer cancel()
	fields, err := o.client.secrets.Read(read, o.client.secretPath)
	if ctx.Err() != nil {
		return "", o.ended(ctx)
	}
	if err != nil {
		var failed *openbaokv.Error
		if errors.As(err, &failed) {
			return "", o.fail("the scoped key could not be read: " + failed.Reason)
		}
		return "", o.fail("the scoped key could not be read")
	}
	key, ok := fields[keyField].(string)
	if !ok || !validBearer(key) {
		return "", o.fail("the scoped key's secret needs a field api_key, a string of printable characters without spaces")
	}
	return key, nil
}

// validBearer refuses a value net/http would refuse to send, so that it does
// not read as an unreachable Xolotl.
func validBearer(key string) bool {
	if key == "" {
		return false
	}
	return strings.IndexFunc(key, func(r rune) bool { return r <= ' ' || r > '~' }) < 0
}

// request sends one request and reads the answer. The transport's error text
// names the URL and can quote what the server sent, so it is not reported.
func (o operation) request(ctx context.Context, method string, target *url.URL, key string, body []byte) (int, []byte, *Error) {
	timed, cancel := context.WithTimeout(ctx, o.client.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(timed, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, o.fail("the request could not be written")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.client.http.Do(req)
	if err != nil {
		return 0, nil, o.unreachable(ctx, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return 0, nil, o.unreachable(ctx, err)
	}
	return resp.StatusCode, answer, nil
}

// refusal is the fixed reason for a status that is not a success, and whether
// Xolotl says to send the request again. Empty is a success.
func (o operation) refusal(code int) (string, bool) {
	switch code {
	case http.StatusOK, http.StatusCreated:
		return "", false
	case http.StatusBadRequest:
		return "Xolotl refused the request: the key policy does not exist, or the name is not valid", false
	case http.StatusUnauthorized:
		return "Xolotl did not accept the scoped key", false
	case http.StatusForbidden:
		return "Xolotl denied the scoped key for this key policy: the scoped key's policy must have the keys scope and list the key policy, and the key policy must grant only proxy and mcp. A policy change can take up to 60 seconds to reach every Xolotl instance", false
	case http.StatusConflict:
		return "another Xolotl instance kept changing this key", true
	case http.StatusInternalServerError:
		if o.name == "revoke" {
			return "Xolotl answered status 500: OpenBao failed, and a version may still hold the key. Revoke it again", true
		}
		return "Xolotl answered status 500: OpenBao failed", true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return fmt.Sprintf("Xolotl at %s is not available: status %d", o.client.base.Host, code), false
	}
	return fmt.Sprintf("Xolotl answered status %d", code), false
}

// unreachable is the error of a request that got no complete answer.
func (o operation) unreachable(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		return o.ended(ctx)
	}
	reason := fmt.Sprintf("Xolotl at %s could not be reached", o.client.base.Host)
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		reason = fmt.Sprintf("Xolotl at %s did not answer within %s", o.client.base.Host, o.client.timeout)
	}
	failed := o.fail(reason)
	failed.Unreachable = true
	return failed
}

// ended is the error of an operation whose caller's context ended.
func (o operation) ended(ctx context.Context) *Error {
	failed := o.fail("the operation ended before Xolotl answered")
	failed.cause = ctx.Err()
	return failed
}
