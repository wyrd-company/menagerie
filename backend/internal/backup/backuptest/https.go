// ---
// relationships:
//   references: [menagerie]
// ---

package backuptest

import (
	"crypto/subtle"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// HTTPSHost is an isolated Git host over HTTPS: a TLS server on the
// loopback address that runs git http-backend for the repositories under
// Dir, behind HTTP basic authentication with one user name and password.
// The Git subprocess runs only on the fixture's server side.
type HTTPSHost struct {
	// Dir holds the host's repositories.
	Dir string
	// Server is the TLS server; its URL is the host's origin.
	Server *httptest.Server

	username, password string

	mu   sync.Mutex
	seen []string
}

// NewHTTPS starts an HTTPSHost for the test serving the repositories
// under dir, accepting username and password, and makes go-git's https
// transport trust the host's certificate until the test ends. Tests that
// use it must not run in parallel with other go-git https use.
func NewHTTPS(t *testing.T, dir, username, password string) *HTTPSHost {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("the HTTPS fixture's server needs git on PATH")
	}
	h := &HTTPSHost{Dir: dir, username: username, password: password}
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + dir,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME=" + t.TempDir(),
			"REMOTE_USER=" + username,
		},
		// http-backend's complaints, such as a missing repository, reach
		// the client as its answer.
		Stderr: io.Discard,
	}
	h.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Get("Authorization"))
		h.mu.Unlock()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(h.username)) != 1 || subtle.ConstantTimeCompare([]byte(pass), []byte(h.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "credentials refused", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Server.Close)
	Trust(t, h.Server)
	return h
}

// Trust makes go-git's https transport trust server's certificate until
// the test ends.
func Trust(t *testing.T, server *httptest.Server) {
	t.Helper()
	client.InstallProtocol("https", githttp.NewClient(server.Client()))
	t.Cleanup(func() { client.InstallProtocol("https", githttp.DefaultClient) })
}

// URL is the https URL of the repository at name under Dir.
func (h *HTTPSHost) URL(name string) string {
	return h.Server.URL + "/" + strings.TrimPrefix(name, "/")
}

// Authorizations lists the Authorization header of every request the
// host received, empty for none.
func (h *HTTPSHost) Authorizations() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}
