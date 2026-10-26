// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

// Package gitclient is go-git's HTTP transport with one addition: a
// request whose context WithoutRedirects marked follows no redirect, so
// credentials never reach a redirect's target, on the same origin or
// another. Importing the package installs the transport for http and
// https: go-git chooses its HTTP transport from a process-wide registry
// by scheme, a push or fetch takes no transport or redirect policy of its
// own, and the HTTP client's CheckRedirect is the only hook that runs
// before a redirected hop is sent. A request without the mark keeps
// go-git's own redirect policy.
package gitclient

import (
	"context"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func init() { Install(nil) }

type noRedirects struct{}

// WithoutRedirects marks ctx so that the requests made with it follow no
// redirect: the redirect's response is the answer.
func WithoutRedirects(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRedirects{}, true)
}

// refuseMarked keeps a marked request at the redirect's response.
func refuseMarked(req *http.Request, _ []*http.Request) error {
	if marked, _ := req.Context().Value(noRedirects{}).(bool); marked {
		return http.ErrUseLastResponse
	}
	return nil
}

// Transport is go-git's HTTP transport over c, nil for one over
// http.DefaultTransport, whose marked requests follow no redirect.
func Transport(c *http.Client) transport.Transport {
	var cl http.Client
	if c != nil {
		cl = *c
	}
	// go-git clones this *http.Transport for an endpoint's TLS and proxy
	// options, so it must be set, as go-git's own default client sets it.
	if cl.Transport == nil {
		cl.Transport = http.DefaultTransport
	}
	cl.CheckRedirect = refuseMarked
	return githttp.NewClient(&cl)
}

// Install makes Transport(c) go-git's transport for http and https.
func Install(c *http.Client) {
	t := Transport(c)
	client.InstallProtocol("http", t)
	client.InstallProtocol("https", t)
}
