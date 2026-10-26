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

// transportFailure is err, from a push or fetch to url. For any URL that
// is not SSH or a local path it is a *TransportError instead.
func transportFailure(ctx context.Context, url string, err error) error {
	if endpoint, perr := transport.NewEndpoint(url); perr == nil && (endpoint.Protocol == "ssh" || endpoint.Protocol == "file") {
		return err
	}
	if cerr := ctx.Err(); cerr != nil {
		return &TransportError{Reason: "the operation ended before the workspace repository answered", kind: cerr}
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
		return &TransportError{Reason: fmt.Sprintf("the workspace repository answered status %d", status.Response.StatusCode)}
	}
	return &TransportError{Reason: "the workspace repository could not be reached or refused the transfer"}
}
