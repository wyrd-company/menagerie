package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

// Synthetic values only.
const (
	sampleToken    = "sentinel-token-5d1e"
	sentinelRemote = "sentinel-remote-73af"
	sentinelBody   = "sentinel-body-2c4f"
)

// fakeTokens serves the URLs that start with prefix with token, and counts
// its calls.
type fakeTokens struct {
	prefix string
	token  string
	err    error

	mu    sync.Mutex
	calls int
}

func (f *fakeTokens) Serves(url string) bool { return strings.HasPrefix(url, f.prefix) }

func (f *fakeTokens) Token(context.Context, string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", "", f.err
	}
	return "x-access-token", f.token, nil
}

// otherwiseAuth records that a URL was handed to the fallback.
type otherwiseAuth struct{ urls []string }

func (o *otherwiseAuth) auth(_ context.Context, url string) (transport.AuthMethod, func(), error) {
	o.urls = append(o.urls, url)
	return nil, func() {}, nil
}

func committedWorkspace(t *testing.T) string {
	t.Helper()
	folder := newWorkspace(t, "alpha")
	if _, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return folder
}

func TestTokenAuthPushesAndFetchesOverHTTPSWithTheToken(t *testing.T) {
	host := backuptest.NewHTTPS(t, t.TempDir(), "x-access-token", sampleToken)
	remoteRepo := bare(t, host.Dir, "backups.git")
	folder := committedWorkspace(t)
	tokens := &fakeTokens{prefix: host.Server.URL, token: sampleToken}
	other := &otherwiseAuth{}
	remote := &Remote{URL: host.URL("backups.git"), Auth: TokenAuth(tokens, other.auth)}

	pushed, err := remote.Push(context.Background(), folder, "alpha", nil)
	if err != nil || !pushed {
		t.Fatalf("push over https: %v %v", pushed, err)
	}
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := repo.Reference("refs/heads/alpha", false)
	if ref, err := remoteRepo.Reference("refs/heads/alpha", false); err != nil || ref.Hash() != local.Hash() {
		t.Fatalf("remote branch %v %v", ref, err)
	}
	fetched, err := remote.Fetch(context.Background(), folder, "alpha", "alpha", nil)
	if err != nil || fetched != local.Hash() {
		t.Fatalf("fetch over https: %s %v", fetched, err)
	}
	if tokens.calls != 2 || len(other.urls) != 0 {
		t.Fatalf("token calls %d, fallback %v", tokens.calls, other.urls)
	}
	for _, a := range host.Authorizations() {
		if a == "" {
			continue
		}
		if !strings.HasPrefix(a, "Basic ") {
			t.Fatalf("authorization %q", a)
		}
	}
}

func TestTokenAuthHandsOtherURLsToTheFallback(t *testing.T) {
	tokens := &fakeTokens{prefix: "https://git.example.invalid/", token: sampleToken}
	other := &otherwiseAuth{}
	auth := TokenAuth(tokens, other.auth)
	for _, url := range []string{"git@example.invalid:sample/backups.git", "https://elsewhere.example.invalid/o/r.git", "/srv/backups.git"} {
		method, release, err := auth(context.Background(), url)
		if err != nil || method != nil {
			t.Fatalf("%s: %v %v", url, method, err)
		}
		release()
	}
	if tokens.calls != 0 || len(other.urls) != 3 {
		t.Fatalf("token calls %d, fallback %v", tokens.calls, other.urls)
	}
	// No fallback: no credentials.
	if method, release, err := TokenAuth(tokens, nil)(context.Background(), "/srv/backups.git"); err != nil || method != nil || release == nil {
		t.Fatalf("no fallback: %v %v", method, err)
	}
}

// TestHTTPSFailuresNameNeitherTheRemoteNorTheToken feeds a remote URL and
// a token carrying sentinels, and server answers carrying one, through
// every failure of a push and a fetch over https.
func TestHTTPSFailuresNameNeitherTheRemoteNorTheToken(t *testing.T) {
	folder := committedWorkspace(t)
	answer := func(status int) *httptest.Server {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, sentinelBody+" "+sentinelRemote+" "+sampleToken, status)
		}))
		t.Cleanup(s.Close)
		return s
	}
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()
	type setup struct {
		url    string
		server *httptest.Server
		tokens *fakeTokens
		ctx    context.Context
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]func(t *testing.T) setup{
		"wrong token": func(t *testing.T) setup {
			host := backuptest.NewHTTPS(t, t.TempDir(), "x-access-token", "another-token")
			bare(t, host.Dir, sentinelRemote+".git")
			return setup{url: host.URL(sentinelRemote + ".git"), server: host.Server}
		},
		"missing repository": func(t *testing.T) setup {
			host := backuptest.NewHTTPS(t, t.TempDir(), "x-access-token", sampleToken)
			return setup{url: host.URL(sentinelRemote + ".git"), server: host.Server}
		},
		"forbidden": func(t *testing.T) setup {
			s := answer(http.StatusForbidden)
			return setup{url: s.URL + "/" + sentinelRemote + ".git", server: s}
		},
		"server error": func(t *testing.T) setup {
			s := answer(http.StatusInternalServerError)
			return setup{url: s.URL + "/" + sentinelRemote + ".git", server: s}
		},
		"unexpected status": func(t *testing.T) setup {
			s := answer(http.StatusTeapot)
			return setup{url: s.URL + "/" + sentinelRemote + ".git", server: s}
		},
		"unreachable": func(t *testing.T) setup {
			return setup{url: closed.URL + "/" + sentinelRemote + ".git", server: closed}
		},
		"token unavailable": func(t *testing.T) setup {
			s := answer(http.StatusInternalServerError)
			return setup{url: s.URL + "/" + sentinelRemote + ".git", server: s, tokens: &fakeTokens{err: errors.New("GitHub App credentials are unavailable: GitHub could not be reached")}}
		},
		"context ended": func(t *testing.T) setup {
			s := answer(http.StatusInternalServerError)
			return setup{url: s.URL + "/" + sentinelRemote + ".git", server: s, ctx: ended}
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := c(t)
			backuptest.Trust(t, s.server)
			tokens := s.tokens
			if tokens == nil {
				tokens = &fakeTokens{token: sampleToken}
			}
			tokens.prefix = s.server.URL
			ctx := s.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			remote := &Remote{URL: s.url, Auth: TokenAuth(tokens, nil)}
			_, pushErr := remote.Push(ctx, folder, "alpha", nil)
			_, fetchErr := remote.Fetch(ctx, folder, "alpha", "alpha", nil)
			host := strings.TrimPrefix(s.server.URL, "https://")
			for op, err := range map[string]error{"push": pushErr, "fetch": fetchErr} {
				if err == nil {
					t.Fatalf("%s succeeded", op)
				}
				for _, secret := range []string{sentinelRemote, sampleToken, host, sentinelBody} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("%s error carries %q: %v", op, secret, err)
					}
				}
			}
			if name == "context ended" && (!errors.Is(pushErr, context.Canceled) || !errors.Is(fetchErr, context.Canceled)) {
				t.Fatalf("an ended context is not reported as such: %v / %v", pushErr, fetchErr)
			}
		})
	}
}

func TestHTTPSFailuresHaveFixedReasons(t *testing.T) {
	cases := []struct {
		status int
		kind   error
		reason string
	}{
		{http.StatusUnauthorized, transport.ErrAuthenticationRequired, "the workspace repository did not accept the credentials"},
		{http.StatusForbidden, transport.ErrAuthorizationFailed, "the workspace repository denied access"},
		{http.StatusNotFound, transport.ErrRepositoryNotFound, "the workspace repository was not found"},
		{http.StatusBadGateway, nil, "the workspace repository answered status 502"},
	}
	folder := committedWorkspace(t)
	for _, c := range cases {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", c.status)
		}))
		backuptest.Trust(t, s)
		_, err := (&Remote{URL: s.URL + "/r.git"}).Push(context.Background(), folder, "alpha", nil)
		s.Close()
		var te *TransportError
		if !errors.As(err, &te) || te.Reason != c.reason || (c.kind != nil && !errors.Is(err, c.kind)) {
			t.Fatalf("status %d: %v", c.status, err)
		}
	}
}

// redirectReason is the fixed reason of a refused redirect.
const redirectReason = "the workspace repository answered with a redirect; set workspace-repository to its current address"

// TestATokenNeverFollowsARedirect: a push and a fetch with a token are
// refused at the first redirect, to another repository on the same origin
// or to another origin, and the target receives nothing.
func TestATokenNeverFollowsARedirect(t *testing.T) {
	folder := committedWorkspace(t)
	for _, sameOrigin := range []bool{true, false} {
		for _, op := range []string{"push", "fetch"} {
			t.Run(fmt.Sprintf("%s same origin %t", op, sameOrigin), func(t *testing.T) {
				host := backuptest.NewHTTPS(t, t.TempDir(), "x-access-token", sampleToken)
				bare(t, host.Dir, "target/other.git")
				var mu sync.Mutex
				var reached []string
				elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					reached = append(reached, r.Header.Get("Authorization"))
					mu.Unlock()
					http.NotFound(w, r)
				}))
				t.Cleanup(elsewhere.Close)
				target := host.Server.URL
				if !sameOrigin {
					target = elsewhere.URL
				}
				host.Handle = func(w http.ResponseWriter, r *http.Request) bool {
					if rest, ok := strings.CutPrefix(r.URL.RequestURI(), "/source/repo.git"); ok {
						http.Redirect(w, r, target+"/target/other.git"+rest, http.StatusFound)
						return true
					}
					mu.Lock()
					reached = append(reached, r.Header.Get("Authorization"))
					mu.Unlock()
					return false
				}
				remote := &Remote{URL: host.URL("source/repo.git"), Auth: TokenAuth(&fakeTokens{prefix: host.Server.URL, token: sampleToken}, nil)}
				var err error
				if op == "push" {
					_, err = remote.Push(context.Background(), folder, "alpha", nil)
				} else {
					_, err = remote.Fetch(context.Background(), folder, "alpha", "alpha", nil)
				}
				var te *TransportError
				if !errors.As(err, &te) || te.Reason != redirectReason {
					t.Fatalf("%s through a redirect: %v", op, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(reached) != 0 {
					t.Fatalf("the redirect's target received %d requests", len(reached))
				}
			})
		}
	}
}

// TestAPushWithoutCredentialsKeepsGoGitsRedirectPolicy: only a transfer
// with credentials refuses redirects.
func TestAPushWithoutCredentialsKeepsGoGitsRedirectPolicy(t *testing.T) {
	host := backuptest.NewHTTPS(t, t.TempDir(), "x-access-token", sampleToken)
	var followed bool
	host.Handle = func(w http.ResponseWriter, r *http.Request) bool {
		if rest, ok := strings.CutPrefix(r.URL.RequestURI(), "/source/repo.git"); ok {
			http.Redirect(w, r, host.Server.URL+"/target/other.git"+rest, http.StatusFound)
			return true
		}
		followed = true
		return false
	}
	folder := committedWorkspace(t)
	if _, err := (&Remote{URL: host.URL("source/repo.git")}).Push(context.Background(), folder, "alpha", nil); err == nil || !followed {
		t.Fatalf("anonymous push: %v, redirect followed %t", err, followed)
	}
}

// TestSSHAndLocalFailuresKeepGoGitsText: only remotes other than SSH and
// local paths get fixed reasons.
func TestSSHAndLocalFailuresKeepGoGitsText(t *testing.T) {
	folder := committedWorkspace(t)
	f := backuptest.New(t)
	for _, url := range []string{filepath.Join(t.TempDir(), "absent.git"), f.URL("absent.git")} {
		_, err := (&Remote{URL: url, Auth: AgentAuth(f.AgentPath, f.KnownHosts)}).Push(context.Background(), folder, "alpha", nil)
		var te *TransportError
		if err == nil || errors.As(err, &te) {
			t.Fatalf("%s: %v", url, err)
		}
	}
}

// TestMalformedURLsNameNoValue: a URL go-git cannot read fails with a
// fixed reason, whether the credentials or the transfer reads it first.
func TestMalformedURLsNameNoValue(t *testing.T) {
	folder := committedWorkspace(t)
	agent := AgentAuth(filepath.Join(t.TempDir(), "absent.sock"))
	for _, url := range []string{
		"https://git.example.invalid/owner/" + sentinelRemote + "%zz",
		"https://git.example.invalid:" + sentinelRemote + "/owner/repo.git",
		"https://[" + sentinelRemote + "/owner/repo.git",
	} {
		for name, auth := range map[string]Auth{
			"token or agent": TokenAuth(&fakeTokens{prefix: "https://elsewhere.example.invalid/", token: sampleToken}, agent),
			"none":           nil,
		} {
			remote := &Remote{URL: url, Auth: auth}
			_, pushErr := remote.Push(context.Background(), folder, "alpha", nil)
			_, fetchErr := remote.Fetch(context.Background(), folder, "alpha", "alpha", nil)
			for op, err := range map[string]error{"push": pushErr, "fetch": fetchErr} {
				var te *TransportError
				if !errors.As(err, &te) || te.Reason != "the workspace repository address is not a valid Git URL" || strings.Contains(err.Error(), sentinelRemote) {
					t.Errorf("%s %s with %s: %v", op, url, name, err)
				}
			}
		}
	}
}

// TestCredentialFailuresKeepTheAppsReasonAndNameNoValue: the token
// source's own reason is kept; any other credential failure of a URL that
// is not SSH or local is a fixed reason.
func TestCredentialFailuresKeepTheAppsReasonAndNameNoValue(t *testing.T) {
	folder := committedWorkspace(t)
	url := "https://git.example.invalid/owner/" + sentinelRemote + ".git"
	appReason := "GitHub App credentials are unavailable: GitHub could not be reached"
	_, err := (&Remote{URL: url, Auth: TokenAuth(&fakeTokens{prefix: url, err: errors.New(appReason)}, nil)}).Push(context.Background(), folder, "alpha", nil)
	var ce *CredentialsError
	if !errors.As(err, &ce) || err.Error() != "workspace repository credentials: "+appReason {
		t.Fatalf("token source failure: %v", err)
	}
	failing := func(_ context.Context, u string) (transport.AuthMethod, func(), error) {
		return nil, nil, errors.New("no credentials for " + u)
	}
	_, err = (&Remote{URL: url, Auth: failing}).Push(context.Background(), folder, "alpha", nil)
	var te *TransportError
	if !errors.As(err, &te) || te.Reason != "the workspace repository's credentials could not be prepared" {
		t.Fatalf("other credential failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = (&Remote{URL: url, Auth: failing}).Fetch(ctx, folder, "alpha", "alpha", nil); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), sentinelRemote) {
		t.Fatalf("ended credential failure: %v", err)
	}
	// SSH keeps its text.
	if _, err = (&Remote{URL: "git@example.invalid:o/r.git", Auth: failing}).Push(context.Background(), folder, "alpha", nil); err == nil || !strings.Contains(err.Error(), "no credentials for git@example.invalid") {
		t.Fatalf("ssh credential failure: %v", err)
	}
}
