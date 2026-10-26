// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

package backup

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// TransportError is a push or fetch failure over HTTP(S). Reason is a
// fixed description: go-git's HTTP errors quote the repository URL and
// the server's answer, so none of their text is kept.
type TransportError struct {
	Reason string
	kind   error
}

func (e *TransportError) Error() string { return e.Reason }

// Unwrap returns the transport's error kind, such as
// transport.ErrAuthenticationRequired, or the context error that ended the
// transfer.
func (e *TransportError) Unwrap() error { return e.kind }

// keepsText reports whether url is an SSH URL or a local path, whose
// failures keep go-git's text. It is false for a URL go-git cannot read.
func keepsText(url string) bool {
	endpoint, err := transport.NewEndpoint(url)
	return err == nil && (endpoint.Protocol == "ssh" || endpoint.Protocol == "file")
}

// fixedFailure is the *TransportError for an ended context or a URL go-git
// cannot read, or nil.
func fixedFailure(ctx context.Context, url string) error {
	if cerr := ctx.Err(); cerr != nil {
		return &TransportError{Reason: "the operation ended before the workspace repository answered", kind: cerr}
	}
	if _, err := transport.NewEndpoint(url); err != nil {
		return &TransportError{Reason: "the workspace repository address is not a valid Git URL"}
	}
	return nil
}

// credentialFailure is err, from getting the credentials of url. For any
// URL that is not SSH or a local path it is a *TransportError instead,
// except a *CredentialsError, whose text names no value.
func credentialFailure(ctx context.Context, url string, err error) error {
	var credentials *CredentialsError
	if keepsText(url) || errors.As(err, &credentials) {
		return err
	}
	if fixed := fixedFailure(ctx, url); fixed != nil {
		return fixed
	}
	return &TransportError{Reason: "the workspace repository's credentials could not be prepared"}
}

// transportFailure is err, from a push or fetch to url. For any URL that
// is not SSH or a local path it is a *TransportError instead.
func transportFailure(ctx context.Context, url string, err error) error {
	if keepsText(url) {
		return err
	}
	if fixed := fixedFailure(ctx, url); fixed != nil {
		return fixed
	}
	for _, k := range []struct {
		kind   error
		reason string
	}{
		{transport.ErrAuthenticationRequired, "the workspace repository did not accept the credentials"},
		{transport.ErrAuthorizationFailed, "the workspace repository denied access"},
		{transport.ErrRepositoryNotFound, "the workspace repository was not found"},
		{transport.ErrEmptyRemoteRepository, "the workspace repository is empty"},
	} {
		if errors.Is(err, k.kind) {
			return &TransportError{Reason: k.reason, kind: k.kind}
		}
	}
	// go-git wraps an unexpected status in a *plumbing.UnexpectedError,
	// which has no Unwrap.
	var unexpected *plumbing.UnexpectedError
	if errors.As(err, &unexpected) {
		err = unexpected.Err
	}
	var status *githttp.Err
	if errors.As(err, &status) && status.Response != nil {
		// A 2xx answer is no error, so below 400 is a redirect.
		if code := status.Response.StatusCode; code < 400 {
			return &TransportError{Reason: "the workspace repository answered with a redirect; set workspace-repository to its current address"}
		}
		return &TransportError{Reason: fmt.Sprintf("the workspace repository answered status %d", status.Response.StatusCode)}
	}
	return &TransportError{Reason: "the workspace repository could not be reached or refused the transfer"}
}
