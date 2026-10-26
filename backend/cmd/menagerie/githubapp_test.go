package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/githubapp"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
)

// keySigner signs App JWTs with a key generated for the test.
type keySigner struct{ key *rsa.PrivateKey }

func (s keySigner) Sign(_ context.Context, input []byte) ([]byte, error) {
	sum := sha256.Sum256(input)
	return rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
}

// sampleMinter is a Minter for a TLS fake of GitHub's token route that
// records the permission of every token it mints.
func sampleMinter(t *testing.T) (*githubapp.Minter, *httptest.Server, *[]string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var permissions []string
	github := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		permissions = append(permissions, body.Permissions["contents"])
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"sample-token-%d","expires_at":%q}`, len(permissions), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(github.Close)
	m, err := githubapp.New(githubapp.Options{AppID: 1, Signer: keySigner{key}, Installations: map[string]int64{"example-org": 2}, Server: github.URL, Client: github.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return m, github, &permissions
}

// hookSocket serves the hook runner socket with app and returns a client
// for it.
func hookSocket(t *testing.T, app *githubapp.Minter) *http.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "hooks") // short path: socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "hook-runner.sock")
	_, stop, err := serveHooks(context.Background(), path, throwaway.NewHookRunner(nil, filepath.Join(dir, "candidates")), app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
}

func call(t *testing.T, c *http.Client, path, body string) (int, string) {
	t.Helper()
	resp, err := c.Post("http://hook-runner"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func TestTheHookRunnerSocketServesGitHubCredentials(t *testing.T) {
	m, github, permissions := sampleMinter(t)
	c := hookSocket(t, m)
	status, body := call(t, c, githubapp.CredentialPath, `{"url":"`+github.URL+`/example-org/sample-repo.git"}`)
	if status != http.StatusOK || body != `{"password":"sample-token-1","username":"x-access-token"}` {
		t.Fatalf("%d %s", status, body)
	}
	if strings.Join(*permissions, ",") != "read" {
		t.Fatalf("permissions %v", *permissions)
	}
	// The hook route and the hook runner's own refusals are unchanged.
	if status, body := call(t, c, throwaway.HookPath, `{"unknown":1}`); status != http.StatusBadRequest || !strings.Contains(body, `"kind":"malformed-request"`) {
		t.Fatalf("hook route %d %s", status, body)
	}
	if status, body := call(t, c, "/v1/credentials/other", `{}`); status != http.StatusNotFound || !strings.Contains(body, `"kind":"not-found"`) {
		t.Fatalf("another path %d %s", status, body)
	}
}

func TestWithoutAGitHubAppTheCredentialRouteServesNoURL(t *testing.T) {
	status, body := call(t, hookSocket(t, nil), githubapp.CredentialPath, `{"url":"https://github.com/example-org/sample-repo"}`)
	if status != http.StatusNotFound || body != `{"reason":"no GitHub App is configured"}` {
		t.Fatalf("%d %s", status, body)
	}
}

func TestBackupsUseAWriteTokenOnTheAppsServerAndSSHElsewhere(t *testing.T) {
	m, github, permissions := sampleMinter(t)
	dir, err := os.MkdirTemp("", "wyrwood")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	https := github.URL + "/example-org/backups.git"
	auth, release, err := backupRemote(&config.Config{WyrwoodDirectory: dir, WorkspaceRepository: https}, m).Auth(context.Background(), https)
	if err != nil {
		t.Fatal(err)
	}
	release()
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok || basic.Username != "x-access-token" || basic.Password != "sample-token-1" || strings.Join(*permissions, ",") != "write" {
		t.Fatalf("auth %T %v, permissions %v", auth, auth, *permissions)
	}
	// An SSH URL keeps the agent, even with an App: the agent socket is
	// dialed.
	l, err := net.Listen("unix", filepath.Join(dir, "ssh-agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ssh := "git@example.invalid:sample/backups.git"
	auth, release, err = backupRemote(&config.Config{WyrwoodDirectory: dir, WorkspaceRepository: ssh}, m).Auth(context.Background(), ssh)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, isBasic := auth.(*githttp.BasicAuth); isBasic || auth == nil || len(*permissions) != 1 {
		t.Fatalf("ssh auth %T, permissions %v", auth, *permissions)
	}
}

func TestGitHubAppIsComposedFromConfiguration(t *testing.T) {
	if m, err := gitHubApp(&config.Config{WyrwoodDirectory: "/run/wyrwood"}); m != nil || err != nil {
		t.Fatalf("absent: %v %v", m, err)
	}
	m, err := gitHubApp(&config.Config{WyrwoodDirectory: "/run/wyrwood", GitHubApp: &config.GitHubApp{AppID: 1, TransitKey: "sample-app-key", Server: "https://git.example.invalid"}})
	if err != nil || m == nil {
		t.Fatalf("configured: %v", err)
	}
	if _, ok := m.Repository("https://git.example.invalid/example-org/sample-repo"); !ok {
		t.Fatal("the configured server is not the minter's")
	}
	if _, ok := m.Repository("https://github.com/example-org/sample-repo"); ok {
		t.Fatal("github.com served for another server")
	}
}
