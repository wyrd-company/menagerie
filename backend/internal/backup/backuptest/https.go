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

	"github.com/wyrd-company/menagerie/backend/internal/backup/gitclient"
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
	// Home is the server-side Git's home folder: a .gitconfig there
	// configures it.
	Home string
	// Handle, when set, answers a request before the host's Git does;
	// true means it answered.
	Handle func(w http.ResponseWriter, r *http.Request) bool

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
	h := &HTTPSHost{Dir: dir, Home: t.TempDir(), username: username, password: password}
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + dir,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME=" + h.Home,
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
		if h.Handle != nil && h.Handle(w, r) {
			return
		}
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

// Trust makes go-git's https transport, the one Menagerie installs, trust
// server's certificate until the test ends.
func Trust(t *testing.T, server *httptest.Server) {
	t.Helper()
	gitclient.Install(server.Client())
	t.Cleanup(func() { gitclient.Install(nil) })
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
