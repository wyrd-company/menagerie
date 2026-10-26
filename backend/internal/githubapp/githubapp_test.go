package githubapp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var sampleRepo = Repository{Owner: "example-org", Name: "sample-repo"}

func TestTokenSignsTheAppJWTAndScopesTheTokenToOneRepository(t *testing.T) {
	g := newGitHub(t, newClock())
	m, signer := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	for _, perm := range []Permission{Write, Read} {
		token, err := m.Token(context.Background(), sampleRepo, perm)
		if err != nil || !strings.HasPrefix(token, "sample-token-") {
			t.Fatalf("%s: %q %v", perm, token, err)
		}
	}
	seen := g.seen()
	if len(seen) != 2 {
		t.Fatalf("requests %+v", seen)
	}
	for i, perm := range []string{"write", "read"} {
		r := seen[i]
		if r.method != http.MethodPost || r.path != fmt.Sprintf("/api/v3/app/installations/%d/access_tokens", sampleInstallation) {
			t.Fatalf("request %s %s", r.method, r.path)
		}
		want := map[string]any{"repositories": []any{"sample-repo"}, "permissions": map[string]any{"contents": perm}}
		if !reflect.DeepEqual(r.body, want) {
			t.Fatalf("token request %v, want %v", r.body, want)
		}
		if r.header.Get("Accept") != "application/vnd.github+json" || r.header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers %v", r.header)
		}
	}
	// The signer signed exactly the JWT's first two segments.
	jwt := strings.TrimPrefix(seen[0].header.Get("Authorization"), "Bearer ")
	if input := jwt[:strings.LastIndex(jwt, ".")]; len(signer.inputs) != 2 || signer.inputs[0] != input {
		t.Fatalf("signed %q, sent %q", signer.inputs, input)
	}
}

func TestOwnersMatchTheConfiguredInstallationsInAnyCase(t *testing.T) {
	g := newGitHub(t, newClock())
	m, _ := g.minter(t, map[string]int64{"Example-Org": sampleInstallation})
	if _, err := m.Token(context.Background(), Repository{Owner: "EXAMPLE-org", Name: "sample-repo"}, Read); err != nil {
		t.Fatal(err)
	}
	if g.count(http.MethodGet) != 0 || g.count(http.MethodPost) != 1 {
		t.Fatalf("requests %+v", g.seen())
	}
}

func TestAnUnlistedOwnersInstallationIsLookedUpOnce(t *testing.T) {
	g := newGitHub(t, newClock())
	g.installed["example-org/sample-repo"] = 555
	m, _ := g.minter(t, map[string]int64{"another-org": sampleInstallation})
	for _, perm := range []Permission{Write, Read} {
		if _, err := m.Token(context.Background(), sampleRepo, perm); err != nil {
			t.Fatal(err)
		}
	}
	seen := g.seen()
	if len(seen) != 3 || seen[0].path != "/api/v3/repos/example-org/sample-repo/installation" || seen[1].path != "/api/v3/app/installations/555/access_tokens" {
		t.Fatalf("requests %+v", seen)
	}
}

func TestALookedUpInstallationThatIsGoneIsLookedUpAgain(t *testing.T) {
	g := newGitHub(t, newClock())
	g.installed["example-org/sample-repo"] = 555
	gone := true
	g.answer = func(w http.ResponseWriter, r *http.Request) bool {
		if gone && tokenRoute.MatchString(r.URL.Path) {
			gone = false
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return true
		}
		return false
	}
	m, _ := g.minter(t, nil)
	var e *Error
	if _, err := m.Token(context.Background(), sampleRepo, Write); !errors.As(err, &e) || e.Reason != reasonInstallationNotFound {
		t.Fatalf("first token %v", err)
	}
	if _, err := m.Token(context.Background(), sampleRepo, Write); err != nil {
		t.Fatal(err)
	}
	if g.count(http.MethodGet) != 2 {
		t.Fatalf("requests %+v", g.seen())
	}
}

func TestTokensAreCachedUntilFiveMinutesBeforeTheyExpire(t *testing.T) {
	c := newClock()
	g := newGitHub(t, c)
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	ctx := context.Background()
	first, err := m.Token(ctx, sampleRepo, Write)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour - refreshBefore - time.Second)
	if again, err := m.Token(ctx, sampleRepo, Write); err != nil || again != first {
		t.Fatalf("token before the refresh %q %v", again, err)
	}
	if g.count(http.MethodPost) != 1 {
		t.Fatalf("a cached token was minted again: %+v", g.seen())
	}
	c.advance(time.Second)
	if fresh, err := m.Token(ctx, sampleRepo, Write); err != nil || fresh == first {
		t.Fatalf("token at the refresh %q %v", fresh, err)
	}
	// Each repository and permission has its own token.
	for _, k := range []struct {
		repo Repository
		perm Permission
	}{{sampleRepo, Read}, {Repository{Owner: "example-org", Name: "other-repo"}, Write}} {
		if _, err := m.Token(ctx, k.repo, k.perm); err != nil {
			t.Fatal(err)
		}
	}
	// Names differ only in case: one repository.
	if _, err := m.Token(ctx, Repository{Owner: "Example-Org", Name: "Sample-Repo"}, Write); err != nil {
		t.Fatal(err)
	}
	if g.count(http.MethodPost) != 4 {
		t.Fatalf("token requests %+v", g.seen())
	}
	// Entries due for a refresh leave the cache when a token is stored.
	c.advance(time.Hour)
	if _, err := m.Token(ctx, sampleRepo, Write); err != nil {
		t.Fatal(err)
	}
	if len(m.tokens) != 1 {
		t.Fatalf("cache holds %d tokens", len(m.tokens))
	}
}

func TestATokenThatExpiresWithinFiveMinutesIsNotCached(t *testing.T) {
	g := newGitHub(t, newClock())
	g.expires = refreshBefore
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	for range 2 {
		if _, err := m.Token(context.Background(), sampleRepo, Read); err != nil {
			t.Fatal(err)
		}
	}
	if g.count(http.MethodPost) != 2 {
		t.Fatalf("token requests %+v", g.seen())
	}
}

func TestConcurrentCallersShareOneRequest(t *testing.T) {
	g := newGitHub(t, newClock())
	g.gate = make(chan struct{})
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	const callers = 8
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() { tokens[i], errs[i] = m.Token(context.Background(), sampleRepo, Write) })
	}
	<-g.arrived
	// Without sharing, the other callers' requests arrive while the first
	// is held.
	select {
	case <-g.arrived:
		t.Error("a second token request arrived while the first was held")
	case <-time.After(200 * time.Millisecond):
	}
	close(g.gate)
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || tokens[i] != tokens[0] {
			t.Fatalf("caller %d: %q %v", i, tokens[i], errs[i])
		}
	}
	if g.count(http.MethodPost) != 1 {
		t.Fatalf("token requests %d", g.count(http.MethodPost))
	}
}

func TestAWaiterRetriesWhenTheCallItWaitedForEndedWithItsCaller(t *testing.T) {
	g := newGitHub(t, newClock())
	g.gate = make(chan struct{})
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	go func() {
		_, err := m.Token(leaderCtx, sampleRepo, Write)
		leader <- err
	}()
	<-g.arrived
	waiter := make(chan error, 1)
	go func() {
		_, err := m.Token(context.Background(), sampleRepo, Write)
		waiter <- err
	}()
	// Let the waiter join the flight, then end the leader's context.
	time.Sleep(100 * time.Millisecond)
	cancelLeader()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader %v", err)
	}
	close(g.gate)
	if err := <-waiter; err != nil {
		t.Fatalf("waiter %v", err)
	}
}

func TestAWaiterWhoseContextEndsStopsWaiting(t *testing.T) {
	g := newGitHub(t, newClock())
	g.gate = make(chan struct{})
	defer close(g.gate)
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	go func() { _, _ = m.Token(context.Background(), sampleRepo, Write) }()
	<-g.arrived
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var e *Error
	if _, err := m.Token(ctx, sampleRepo, Write); !errors.As(err, &e) || e.Reason != reasonEnded || !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter %v", err)
	}
}

func TestTokenRefusesAPermissionOtherThanReadOrWrite(t *testing.T) {
	g := newGitHub(t, newClock())
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	if _, err := m.Token(context.Background(), sampleRepo, "admin"); err == nil || len(g.seen()) != 0 {
		t.Fatalf("admin: %v", err)
	}
}

func TestRepositoryAcceptsOnlyRepositoryURLsOnTheServer(t *testing.T) {
	m, err := New(Options{AppID: 1, Signer: &localSigner{}})
	if err != nil {
		t.Fatal(err)
	}
	accepted := map[string]Repository{
		"https://github.com/example-org/sample-repo":     sampleRepo,
		"https://github.com/example-org/sample-repo.git": sampleRepo,
		"https://GitHub.com/example-org/sample-repo":     sampleRepo,
		"https://github.com:443/example-org/sample-repo": sampleRepo,
		"https://github.com/a/b.c_d-e":                   {Owner: "a", Name: "b.c_d-e"},
	}
	for url, want := range accepted {
		if got, ok := m.Repository(url); !ok || got != want {
			t.Errorf("%s: %+v %v", url, got, ok)
		}
	}
	for _, url := range []string{
		"http://github.com/example-org/sample-repo",
		"ssh://git@github.com/example-org/sample-repo.git",
		"git@github.com:example-org/sample-repo.git",
		"https://x-access-token:secret@github.com/example-org/sample-repo",
		"https://user@github.com/example-org/sample-repo",
		"https://github.com/example-org/sample-repo?ref=main",
		"https://github.com/example-org/sample-repo?",
		"https://github.com/example-org/sample-repo#main",
		"https://github.com/example-org/sample-repo/",
		"https://github.com/example-org/sample-repo/tree/main",
		"https://github.com/example-org",
		"https://github.com/example-org/",
		"https://github.com//sample-repo",
		"https://github.com/example-org/.git",
		"https://github.com/example-org/..",
		"https://github.com/example-org/%2e%2e",
		"https://github.com/example%2Dorg/sample-repo",
		"https://github.com/-org/sample-repo",
		"https://github.com/example_org/sample-repo",
		"https://github.com:8443/example-org/sample-repo",
		"https://github.com.example.invalid/example-org/sample-repo",
		"https://gist.github.com/example-org/sample-repo",
		"https://api.github.com/example-org/sample-repo",
		"https:github.com/example-org/sample-repo",
		"/srv/example-org/sample-repo",
		"",
	} {
		if got, ok := m.Repository(url); ok {
			t.Errorf("%s accepted as %+v", url, got)
		}
	}
}

func TestServerOrigins(t *testing.T) {
	for server, want := range map[string][2]string{
		"":                                 {"github.com", "https://api.github.com"},
		"https://github.com":               {"github.com", "https://api.github.com"},
		"https://GITHUB.com/":              {"github.com", "https://api.github.com"},
		"https://git.example.invalid":      {"git.example.invalid", "https://git.example.invalid/api/v3"},
		"https://git.example.invalid:8443": {"git.example.invalid:8443", "https://git.example.invalid:8443/api/v3"},
	} {
		m, err := New(Options{AppID: 1, Signer: &localSigner{}, Server: server})
		if err != nil || m.host != want[0] || m.api != want[1] {
			t.Errorf("%q: %v", server, err)
			continue
		}
	}
	for _, server := range []string{
		"http://github.com",
		"github.com",
		"https://",
		"https://:443",
		"https://user@git.example.invalid",
		"https://git.example.invalid/github",
		"https://git.example.invalid?x=1",
		"https://git.example.invalid?",
		"https://git.example.invalid#x",
		"https://git.example.invalid:0",
		"https://git.example.invalid:99999",
		"https://git.example.invalid:x",
		"https:git.example.invalid",
	} {
		if err := ValidServer(server); err == nil {
			t.Errorf("%q accepted", server)
		} else if strings.Contains(err.Error(), "example.invalid") {
			t.Errorf("the refusal names the value: %v", err)
		}
		if _, err := New(Options{AppID: 1, Signer: &localSigner{}, Server: server}); err == nil {
			t.Errorf("New accepted %q", server)
		}
	}
	if _, err := New(Options{Signer: &localSigner{}}); err == nil {
		t.Error("New accepted no app id")
	}
	if _, err := New(Options{AppID: 1}); err == nil {
		t.Error("New accepted no signer")
	}
}

func TestTheGitHubClientFollowsNoRedirect(t *testing.T) {
	g := newGitHub(t, newClock())
	elsewhere := newGitHub(t, newClock())
	g.answer = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, elsewhere.server.URL+r.URL.Path, http.StatusTemporaryRedirect)
		return true
	}
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	var e *Error
	if _, err := m.Token(context.Background(), sampleRepo, Write); !errors.As(err, &e) || !strings.Contains(e.Reason, "status 307") {
		t.Fatalf("redirected token %v", err)
	}
	if n := len(elsewhere.seen()); n != 0 {
		t.Fatalf("the redirect's target received %d requests", n)
	}
}

func TestTokensServeOnlyRepositoryURLsOnTheServer(t *testing.T) {
	g := newGitHub(t, newClock())
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	tokens := m.Tokens(Write)
	url := g.server.URL + "/example-org/sample-repo.git"
	if !tokens.Serves(url) || tokens.Serves("https://elsewhere.example.invalid/example-org/sample-repo.git") {
		t.Fatal("Serves")
	}
	user, token, err := tokens.Token(context.Background(), url)
	if err != nil || user != Username || token == "" {
		t.Fatalf("%q %q %v", user, token, err)
	}
	if body := g.seen()[0].body; !reflect.DeepEqual(body["permissions"], map[string]any{"contents": "write"}) {
		t.Fatalf("permissions %v", body)
	}
	if _, _, err := tokens.Token(context.Background(), "http://elsewhere.example.invalid/o/r"); err == nil || strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("an unserved URL: %v", err)
	}
}

func TestSigningInputClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := signingInput(12345, now)
	want := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9." + strings.TrimRight(b64(`{"iat":1699999940,"exp":1700000540,"iss":"12345"}`), "=")
	if got != want {
		t.Fatalf("signing input %s, want %s", got, want)
	}
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
