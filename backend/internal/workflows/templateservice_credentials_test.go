//go:build templateservice

package workflows

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// TestAJobLimitEndsTheTemplateServicesCredentialRequest creates a
// workspace from an https template whose host challenges every request,
// while the credential route holds the template service's request. The
// interview's limit ends the job; the template service then ends its
// credential request, so the route's request context ends.
func TestAJobLimitEndsTheTemplateServicesCredentialRequest(t *testing.T) {
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="sample"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	t.Cleanup(host.Close)
	authority := filepath.Join(t.TempDir(), "authority.pem")
	if err := os.WriteFile(authority, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: host.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{}, 8)
	ended := make(chan struct{}, 8)
	// Released when the test ends, so a request the template service never
	// ends cannot keep it from stopping.
	release := make(chan struct{})
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http sees the connection close only once the body is read,
		// as the workspace service's route reads it.
		_, _ = io.Copy(io.Discard, r.Body)
		held <- struct{}{}
		select {
		case <-r.Context().Done():
			ended <- struct{}{}
		case <-release:
			http.Error(w, `{"reason":"the token could not be minted"}`, http.StatusBadGateway)
		}
	})
	a := newAcceptWith(t, &fakeRuntime{}, acceptOptions{
		containers: localContainers{}, image: "runner:local",
		limits:      jobs.Limits{Phases: map[string]time.Duration{"interview": 2 * time.Second}},
		templates:   map[string]string{"private": host.URL + "/example-org/sample-kit.git@v1"},
		credentials: route,
		// Both of curl's TLS backends read it.
		env: []string{"GIT_SSL_CAINFO=" + authority},
	})
	t.Cleanup(func() { close(release) })
	job, _ := a.drive(t, "create", "alpha", "private", nil)
	if snap := job.Snapshot(); snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != jobs.KindTimedOut {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	select {
	case <-held:
	default:
		t.Fatal("the template service never asked the credential route")
	}
	// Test-only bound: the request ends as soon as the template service
	// sees its caller go away.
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the credential request outlived its job")
	}
}
