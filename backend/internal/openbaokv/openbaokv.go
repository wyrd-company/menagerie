// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

// Package openbaokv reads secrets from a KV version 2 mount through the
// Wyrwood vault socket. Wyrwood replaces the request's token with the
// container's own and owns its renewal, so the client sends no token and no
// namespace, and reads no OpenBao settings from the environment. No error
// holds a secret's value or text from OpenBao's answer.
package openbaokv

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	openbao "github.com/openbao/openbao/api/v2"
)

// maxResponse bounds the body read from the vault socket.
const maxResponse = 1 << 20

// Error says why a secret could not be read.
type Error struct {
	Reason string
	cause  error
}

func (e *Error) Error() string { return "OpenBao secret is unavailable: " + e.Reason }

// Unwrap returns the context error that ended the read, if any.
func (e *Error) Unwrap() error { return e.cause }

// Reader reads secrets from one KV version 2 mount.
type Reader struct {
	client *openbao.Client
	mount  string
}

// New returns a Reader for the KV version 2 mount at address.
func New(address, mount string) (*Reader, error) {
	cfg := openbao.NewConfig()
	cfg.Address = address
	// NewConfig reads no environment, so no ambient token, namespace,
	// address or TLS setting applies.
	client, err := openbao.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("OpenBao client: %w", err)
	}
	return &Reader{client: client, mount: mount}, nil
}

// SetMaxRetries sets how many times the client repeats a request that OpenBao
// answered with a server error.
func (r *Reader) SetMaxRetries(retries int) { r.client.SetMaxRetries(retries) }

// Read returns the fields of the current version of the secret at path.
// Every failure is an *Error.
func (r *Reader) Read(ctx context.Context, path string) (map[string]any, error) {
	// The answer's status decides the reason. The client's error text can
	// quote the response body, so it is not reported.
	resp, _ := r.client.Logical().ReadRawWithContext(ctx, r.mount+"/data/"+path)
	if resp != nil {
		defer resp.Body.Close()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, &Error{Reason: "the operation ended before OpenBao answered", cause: ctxErr}
	}
	if resp == nil {
		return nil, &Error{Reason: "the Wyrwood vault socket could not be reached"}
	}
	body := io.LimitReader(resp.Body, maxResponse)
	switch resp.StatusCode {
	case http.StatusOK:
		return fields(body)
	case http.StatusNotFound:
		return nil, &Error{Reason: "the secret does not exist"}
	case http.StatusForbidden:
		return nil, &Error{Reason: "OpenBao denied reading the secret"}
	case http.StatusServiceUnavailable:
		if state := wyrwoodState(body); state != "" {
			return nil, &Error{Reason: "Wyrwood holds no OpenBao token for Menagerie (state " + state + ")"}
		}
	}
	return nil, &Error{Reason: fmt.Sprintf("OpenBao answered status %d", resp.StatusCode)}
}

// fields reads a KV version 2 read response. A deleted or destroyed version
// has no data and counts as missing.
func fields(body io.Reader) (map[string]any, error) {
	var doc struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return nil, &Error{Reason: "OpenBao returned a malformed response"}
	}
	if doc.Data.Data == nil {
		return nil, &Error{Reason: "the secret has no current version"}
	}
	return doc.Data.Data, nil
}

// wyrwoodState is the state of a Wyrwood unavailable answer, or "".
func wyrwoodState(body io.Reader) string {
	var doc struct {
		Wyrwood struct {
			State string `json:"state"`
		} `json:"wyrwood"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return ""
	}
	switch doc.Wyrwood.State {
	case "not-bound", "no-token":
		return doc.Wyrwood.State
	}
	return ""
}
