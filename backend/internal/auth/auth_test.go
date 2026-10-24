package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wyrd-company/menagerie/backend/internal/auth/authtest"
)

const testClientID = "sample-client"

var (
	manager    = authtest.User{Subject: "subject-manager", Name: "Sample Manager", Groups: []string{"other", ManagersGroup}}
	nonManager = authtest.User{Subject: "subject-other", Name: "Sample Other", Groups: []string{"other"}}
)

// site is Menagerie's handler behind the Authenticator, with a protected
// page and API route that count their hits.
type site struct {
	t      *testing.T
	srv    *httptest.Server
	issuer *authtest.Issuer
	auth   *Authenticator
	hits   atomic.Int64
}

func newSite(t *testing.T) *site {
	t.Helper()
	s := &site{t: t}
	var handler http.Handler
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(s.srv.Close)
	redirect := s.srv.URL + CallbackPath
	s.issuer = authtest.New(t, testClientID, redirect)
	s.issuer.User = manager
	a, err := New(Config{Issuer: s.issuer.URL(), ClientID: testClientID, RedirectURL: redirect})
	if err != nil {
		t.Fatal(err)
	}
	s.auth = a
	mux := http.NewServeMux()
	a.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	protected := func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		_, _ = io.WriteString(w, "protected "+r.Method+" "+r.URL.RequestURI())
	}
	mux.HandleFunc("/page/", protected)
	mux.HandleFunc("/api/thing", protected)
	handler = a.Wrap(mux)
	return s
}

// browser is a cookie-keeping client. It follows redirects unless stop
// names the path at which to stop.
type browser struct {
	t      *testing.T
	jar    *cookiejar.Jar
	client *http.Client
	stop   string
}

func (s *site) browser() *browser {
	jar, _ := cookiejar.New(nil)
	b := &browser{t: s.t, jar: jar}
	b.client = &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if b.stop != "" && req.URL.Path == b.stop {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return b
}

type reply struct {
	status   int
	body     string
	header   http.Header
	finalURL *url.URL
}

func (b *browser) do(method, target string, header http.Header) reply {
	b.t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, body: string(body), header: resp.Header, finalURL: resp.Request.URL}
}

func (b *browser) get(target string) reply { return b.do(http.MethodGet, target, nil) }

func (b *browser) cookie(s *site, name string) string {
	u, _ := url.Parse(s.srv.URL)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (b *browser) setCookie(s *site, name, value string) {
	u, _ := url.Parse(s.srv.URL)
	c := &http.Cookie{Name: name, Value: value, Path: "/"}
	if value == "" {
		c.MaxAge = -1
	}
	b.jar.SetCookies(u, []*http.Cookie{c})
}

// signIn runs the whole flow from a protected page and returns the
// session cookie.
func (b *browser) signIn(s *site) string {
	b.t.Helper()
	r := b.get(s.srv.URL + "/page/home")
	if r.status != http.StatusOK || r.body != "protected GET /page/home" {
		b.t.Fatalf("sign-in ended with %d %q at %s", r.status, r.body, r.finalURL)
	}
	id := b.cookie(s, "menagerie-session")
	if id == "" {
		b.t.Fatal("no session cookie after sign-in")
	}
	return id
}

// pendingCallback starts a sign-in and returns the callback URL the
// provider sends the browser to, without following it.
func (b *browser) pendingCallback(s *site, returnTo string) string {
	b.t.Helper()
	b.stop = CallbackPath
	defer func() { b.stop = "" }()
	r := b.get(s.srv.URL + LoginPath + "?return=" + url.QueryEscape(returnTo))
	if r.status != http.StatusFound {
		b.t.Fatalf("authorization answered %d %q", r.status, r.body)
	}
	loc := r.header.Get("Location")
	if !strings.HasPrefix(loc, s.srv.URL+CallbackPath+"?") {
		b.t.Fatalf("provider redirected to %q", loc)
	}
	return loc
}

func pageKind(t *testing.T, r reply) string {
	t.Helper()
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
		t.Fatalf("not a page: %d %q", r.status, r.body)
	}
	_, after, ok := strings.Cut(r.body, `data-kind="`)
	if !ok {
		return ""
	}
	kind, _, _ := strings.Cut(after, `"`)
	return kind
}

func errorKind(t *testing.T, r reply) string {
	t.Helper()
	var env struct {
		Error struct{ Kind, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("not an error envelope: %d %q", r.status, r.body)
	}
	return env.Error.Kind
}

func TestManagerSignsInAndReturnsToThePage(t *testing.T) {
	s := newSite(t)
	b := s.browser()

	b.stop = LoginPath
	r := b.get(s.srv.URL + "/page/list?sort=name")
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/auth/login?return=%2Fpage%2Flist%3Fsort%3Dname" {
		t.Fatalf("unauthenticated page answered %d Location %q", r.status, r.header.Get("Location"))
	}
	b.stop = ""
	r = b.get(s.srv.URL + "/page/list?sort=name")
	if r.status != http.StatusOK || r.body != "protected GET /page/list?sort=name" {
		t.Fatalf("after sign-in: %d %q at %s", r.status, r.body, r.finalURL)
	}
	if r := b.get(s.srv.URL + "/api/thing"); r.status != http.StatusOK {
		t.Fatalf("API with session answered %d", r.status)
	}

	// The authorization request is a PKCE S256 code request with state
	// and nonce; the token request proves the verifier and sends no secret.
	az := s.issuer.Authorizations()[0]
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(az.Get(k)) < 43 {
			t.Errorf("authorization %s = %q", k, az.Get(k))
		}
	}
	if az.Get("code_challenge_method") != "S256" || az.Get("response_type") != "code" || az.Get("scope") != "openid profile groups" {
		t.Errorf("authorization request %v", az)
	}
	tf := s.issuer.TokenForms()[0]
	if tf.Get("code_verifier") == "" || tf.Has("client_secret") || tf.Get("client_id") != testClientID {
		t.Errorf("token request %v", tf)
	}
}

func TestHeadRequestRedirectsToSignIn(t *testing.T) {
	s := newSite(t)
	b := s.browser()
	b.stop = LoginPath
	r := b.do(http.MethodHead, s.srv.URL+"/page/x", nil)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/auth/login?return=%2Fpage%2Fx" {
		t.Fatalf("HEAD answered %d %q", r.status, r.header.Get("Location"))
	}
	if s.hits.Load() != 0 {
		t.Fatal("protected handler ran")
	}
}

func TestAPIWithoutSessionAnswers401(t *testing.T) {
	s := newSite(t)
	b := s.browser()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := b.do(method, s.srv.URL+"/api/thing", nil)
		if r.status != http.StatusUnauthorized || errorKind(t, r) != "unauthenticated" {
			t.Errorf("%s answered %d %q", method, r.status, r.body)
		}
	}
	// A session id the server never issued is no session.
	b.setCookie(s, "menagerie-session", randomToken())
	if r := b.get(s.srv.URL + "/api/thing"); r.status != http.StatusUnauthorized {
		t.Errorf("made-up session answered %d", r.status)
	}
	if s.hits.Load() != 0 {
		t.Fatal("protected handler ran")
	}
}

func TestOnlyExactPublicPathsSkipTheSession(t *testing.T) {
	s := newSite(t)
	b := s.browser()
	b.stop = LoginPath
	if r := b.get(s.srv.URL + "/healthz"); r.status != http.StatusOK {
		t.Fatalf("health answered %d", r.status)
	}
	for _, p := range []string{"/healthz/", "/auth%2Flogin", "/auth/login/", "/api/../auth/login"} {
		req, _ := http.NewRequest(http.MethodGet, s.srv.URL, nil)
		req.URL.Opaque = "//" + req.URL.Host + p
		resp, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// The gate, not the login handler, answers: a page redirect to
		// sign-in or an API 401.
		gated := resp.StatusCode == http.StatusUnauthorized ||
			(resp.StatusCode == http.StatusSeeOther && strings.HasPrefix(resp.Header.Get("Location"), LoginPath+"?return="))
		if !gated {
			t.Errorf("%s answered %d %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestNonManagerGetsNoSession(t *testing.T) {
	s := newSite(t)
	s.issuer.User = nonManager
	b := s.browser()
	r := b.get(s.srv.URL + "/page/home")
	if r.status != http.StatusForbidden || pageKind(t, r) != "not-a-manager" || !strings.Contains(r.body, "Sample Other") {
		t.Fatalf("non-manager sign-in answered %d %q", r.status, r.body)
	}
	if b.cookie(s, "menagerie-session") != "" {
		t.Fatal("non-manager got a session cookie")
	}
	if r := b.get(s.srv.URL + "/api/thing"); r.status != http.StatusUnauthorized {
		t.Fatalf("non-manager API answered %d", r.status)
	}
	if s.hits.Load() != 0 {
		t.Fatal("protected handler ran")
	}
}

func TestGroupsClaimShapes(t *testing.T) {
	cases := []struct {
		name   string
		user   authtest.User
		set    func(*authtest.Issuer)
		status int
		kind   string
	}{
		{"empty groups", authtest.User{Subject: "s", Groups: []string{}}, nil, http.StatusForbidden, "not-a-manager"},
		{"group name differs in case", authtest.User{Subject: "s", Groups: []string{"Menagerie_Managers"}}, nil, http.StatusForbidden, "not-a-manager"},
		{"groups is a string", authtest.User{Subject: "s", Groups: ManagersGroup}, nil, http.StatusUnauthorized, "sign-in-rejected"},
		{"groups from userinfo", authtest.User{Subject: "s"}, func(is *authtest.Issuer) { is.UserinfoGroups = []string{ManagersGroup} }, http.StatusOK, ""},
		{"userinfo has no groups", authtest.User{Subject: "s"}, nil, http.StatusForbidden, "not-a-manager"},
		{"no userinfo endpoint", authtest.User{Subject: "s"}, func(is *authtest.Issuer) {
			is.OmitUserinfo = true
			is.UserinfoGroups = []string{ManagersGroup}
		}, http.StatusForbidden, "not-a-manager"},
		{"userinfo for another subject", authtest.User{Subject: "s"}, func(is *authtest.Issuer) {
			is.UserinfoGroups = []string{ManagersGroup}
			is.UserinfoSubject = "someone-else"
		}, http.StatusUnauthorized, "sign-in-rejected"},
		{"id token groups win over userinfo", authtest.User{Subject: "s", Groups: []string{"other"}}, func(is *authtest.Issuer) {
			is.UserinfoGroups = []string{ManagersGroup}
		}, http.StatusForbidden, "not-a-manager"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSite(t)
			s.issuer.User = c.user
			if c.set != nil {
				s.issuer.Set(c.set)
			}
			b := s.browser()
			r := b.get(s.srv.URL + "/page/home")
			if r.status != c.status {
				t.Fatalf("answered %d %q", r.status, r.body)
			}
			if c.status == http.StatusOK {
				if b.cookie(s, "menagerie-session") == "" {
					t.Fatal("no session")
				}
				return
			}
			if got := pageKind(t, r); got != c.kind {
				t.Fatalf("kind %q, want %q", got, c.kind)
			}
			if b.cookie(s, "menagerie-session") != "" || s.hits.Load() != 0 {
				t.Fatal("refused sign-in admitted the browser")
			}
		})
	}
}

func TestCallbackRejectsInvalidIDTokens(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*authtest.Issuer){
		"other issuer": func(is *authtest.Issuer) {
			is.Tamper = func(c map[string]any) { c["iss"] = "https://issuer.example.invalid" }
		},
		"other audience":  func(is *authtest.Issuer) { is.Tamper = func(c map[string]any) { c["aud"] = "other-client" } },
		"bad signature":   func(is *authtest.Issuer) { is.SignWith = other },
		"symmetric HS256": func(is *authtest.Issuer) { is.SignHS256 = true },
		"other nonce":     func(is *authtest.Issuer) { is.Tamper = func(c map[string]any) { c["nonce"] = randomToken() } },
		"no nonce":        func(is *authtest.Issuer) { is.Tamper = func(c map[string]any) { delete(c, "nonce") } },
		"expired": func(is *authtest.Issuer) {
			is.Tamper = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSite(t)
			s.issuer.Set(set)
			b := s.browser()
			r := b.get(s.srv.URL + "/page/home")
			if r.status != http.StatusUnauthorized || pageKind(t, r) != "sign-in-rejected" {
				t.Fatalf("answered %d %q", r.status, r.body)
			}
			if b.cookie(s, "menagerie-session") != "" || s.hits.Load() != 0 {
				t.Fatal("rejected token admitted the browser")
			}
		})
	}
}

func TestCallbackRequiresTheBrowsersPendingState(t *testing.T) {
	s := newSite(t)

	t.Run("unknown state", func(t *testing.T) {
		b := s.browser()
		r := b.get(s.srv.URL + CallbackPath + "?code=x&state=" + randomToken())
		if r.status != http.StatusBadRequest || pageKind(t, r) != "sign-in-expired" {
			t.Fatalf("answered %d %q", r.status, r.body)
		}
	})

	t.Run("other browser", func(t *testing.T) {
		victim := s.browser()
		callback := s.browser().pendingCallback(s, "/page/home")
		r := victim.get(callback)
		if r.status != http.StatusBadRequest || pageKind(t, r) != "sign-in-expired" {
			t.Fatalf("answered %d %q", r.status, r.body)
		}
		if victim.cookie(s, "menagerie-session") != "" {
			t.Fatal("victim got the attacker's session")
		}
	})

	t.Run("replayed callback", func(t *testing.T) {
		b := s.browser()
		callback := b.pendingCallback(s, "/page/home")
		if r := b.get(callback); r.status != http.StatusOK {
			t.Fatalf("first callback answered %d %q", r.status, r.body)
		}
		b.setCookie(s, "menagerie-session", "")
		r := b.get(callback)
		if r.status != http.StatusBadRequest || pageKind(t, r) != "sign-in-expired" {
			t.Fatalf("replay answered %d %q", r.status, r.body)
		}
	})

	t.Run("two tabs", func(t *testing.T) {
		b := s.browser()
		first := b.pendingCallback(s, "/page/first")
		second := b.pendingCallback(s, "/page/second")
		if r := b.get(second); r.status != http.StatusOK || r.body != "protected GET /page/second" {
			t.Fatalf("second tab answered %d %q", r.status, r.body)
		}
		b.setCookie(s, "menagerie-session", "")
		if r := b.get(first); r.status != http.StatusOK || r.body != "protected GET /page/first" {
			t.Fatalf("first tab answered %d %q", r.status, r.body)
		}
	})

	t.Run("no code", func(t *testing.T) {
		b := s.browser()
		callback, _ := url.Parse(b.pendingCallback(s, "/"))
		q := callback.Query()
		q.Del("code")
		callback.RawQuery = q.Encode()
		r := b.get(callback.String())
		if r.status != http.StatusBadRequest || pageKind(t, r) != "sign-in-invalid" {
			t.Fatalf("answered %d %q", r.status, r.body)
		}
	})

	t.Run("code without its verifier", func(t *testing.T) {
		// A code issued to one pending sign-in, presented with another
		// sign-in's state, fails PKCE at the token endpoint.
		b := s.browser()
		stolen, _ := url.Parse(s.browser().pendingCallback(s, "/"))
		own, _ := url.Parse(b.pendingCallback(s, "/"))
		q := own.Query()
		q.Set("code", stolen.Query().Get("code"))
		own.RawQuery = q.Encode()
		r := b.get(own.String())
		if r.status != http.StatusUnauthorized || pageKind(t, r) != "sign-in-rejected" {
			t.Fatalf("answered %d %q", r.status, r.body)
		}
	})
}

func TestCallbackChecksTheIssParameter(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*authtest.Issuer)
		status int
		kind   string
	}{
		{"names our issuer", func(is *authtest.Issuer) { is.AdvertiseIss = true; is.Iss = is.URL() }, http.StatusOK, ""},
		{"absent and not advertised", func(*authtest.Issuer) {}, http.StatusOK, ""},
		{"names another issuer", func(is *authtest.Issuer) { is.Iss = "https://issuer.example.invalid" }, http.StatusUnauthorized, "sign-in-rejected"},
		{"another issuer on an error response", func(is *authtest.Issuer) {
			is.Iss = "https://issuer.example.invalid"
			is.Refuse = "access_denied"
		}, http.StatusUnauthorized, "sign-in-rejected"},
		{"advertised but absent", func(is *authtest.Issuer) { is.AdvertiseIss = true }, http.StatusUnauthorized, "sign-in-rejected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSite(t)
			s.issuer.Set(c.set)
			b := s.browser()
			r := b.get(s.srv.URL + "/page/home")
			if r.status != c.status {
				t.Fatalf("answered %d %q", r.status, r.body)
			}
			if c.status == http.StatusOK {
				return
			}
			if pageKind(t, r) != c.kind || b.cookie(s, "menagerie-session") != "" {
				t.Fatalf("answered %q", r.body)
			}
			if n := s.issuer.RequestCount("POST /token"); n != 0 {
				t.Fatalf("code sent to the token endpoint %d times", n)
			}
		})
	}
}

func TestProviderRefusalAndOutage(t *testing.T) {
	s := newSite(t)
	s.issuer.Set(func(is *authtest.Issuer) { is.Refuse = "access_denied" })
	b := s.browser()
	r := b.get(s.srv.URL + "/page/home")
	if r.status != http.StatusUnauthorized || pageKind(t, r) != "sign-in-refused" || !strings.Contains(r.body, "access_denied") {
		t.Fatalf("refusal answered %d %q", r.status, r.body)
	}

	s.issuer.Set(func(is *authtest.Issuer) { is.Refuse = ""; is.Down = true })
	r = b.get(s.srv.URL + "/page/home")
	if r.status != http.StatusBadGateway || pageKind(t, r) != "identity-provider-unavailable" {
		t.Fatalf("outage answered %d %q", r.status, r.body)
	}
	if s.hits.Load() != 0 {
		t.Fatal("protected handler ran")
	}
}

func TestSignInReturnsOnlyToLocalPaths(t *testing.T) {
	cases := map[string]string{
		"":                       "/",
		"/page/x?a=b":            "/page/x?a=b",
		"/workspaces/a/upgrade":  "/workspaces/a/upgrade",
		"https://evil.example/":  "/",
		"//evil.example/":        "/",
		"/\\evil.example/":       "/",
		"javascript:alert(1)":    "/",
		"page/relative":          "/",
		"/auth/logout":           "/",
		"/auth":                  "/",
		"/authors":               "/authors",
		"/%2F%2Fevil.example":    "/%2F%2Fevil.example",
		"/page\r\nSet-Cookie: x": "/",
	}
	for in, want := range cases {
		if got := localReturn(in); got != want {
			t.Errorf("localReturn(%q) = %q, want %q", in, got, want)
		}
	}

	s := newSite(t)
	b := s.browser()
	callback := b.pendingCallback(s, "//evil.example/")
	b.stop = "/"
	r := b.get(callback)
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/" {
		t.Fatalf("callback answered %d Location %q", r.status, r.header.Get("Location"))
	}
}

func TestSignInRotatesTheSession(t *testing.T) {
	s := newSite(t)

	t.Run("pre-set id is not adopted", func(t *testing.T) {
		b := s.browser()
		chosen := randomToken()
		b.setCookie(s, "menagerie-session", chosen)
		got := b.signIn(s)
		if got == chosen {
			t.Fatal("sign-in adopted the browser's session id")
		}
	})

	for _, next := range []authtest.User{manager, nonManager} {
		t.Run("earlier session ends for "+next.Subject, func(t *testing.T) {
			s.issuer.Set(func(is *authtest.Issuer) { is.User = manager })
			b := s.browser()
			old := b.signIn(s)
			s.issuer.Set(func(is *authtest.Issuer) { is.User = next })
			b.setCookie(s, "menagerie-session", "")
			callback := b.pendingCallback(s, "/page/home")
			b.setCookie(s, "menagerie-session", old)
			b.get(callback)
			if got := b.cookie(s, "menagerie-session"); got == old {
				t.Fatal("session id survived sign-in")
			}
			stale := s.browser()
			stale.setCookie(s, "menagerie-session", old)
			if r := stale.get(s.srv.URL + "/api/thing"); r.status != http.StatusUnauthorized {
				t.Fatalf("earlier session answered %d", r.status)
			}
		})
	}
}

func TestLoginWithSessionReturnsWithoutSignIn(t *testing.T) {
	s := newSite(t)
	b := s.browser()
	b.signIn(s)
	before := s.issuer.RequestCount("GET /authorize")
	b.stop = "/page/again"
	r := b.get(s.srv.URL + LoginPath + "?return=%2Fpage%2Fagain")
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/page/again" {
		t.Fatalf("answered %d %q", r.status, r.header.Get("Location"))
	}
	if s.issuer.RequestCount("GET /authorize") != before {
		t.Fatal("signed-in login went to the provider")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	s := newSite(t)
	b := s.browser()
	id := b.signIn(s)
	b.stop = SignedOutPath
	r := b.do(http.MethodPost, s.srv.URL+LogoutPath, http.Header{"Sec-Fetch-Site": {"same-origin"}, "Origin": {s.srv.URL}})
	if r.status != http.StatusSeeOther || r.header.Get("Location") != SignedOutPath {
		t.Fatalf("logout answered %d %q", r.status, r.header.Get("Location"))
	}
	if b.cookie(s, "menagerie-session") != "" {
		t.Fatal("session cookie kept")
	}
	b.stop = ""
	if r := b.get(s.srv.URL + SignedOutPath); r.status != http.StatusOK || !strings.Contains(r.body, "signed out") {
		t.Fatalf("signed-out page %d %q", r.status, r.body)
	}
	stale := s.browser()
	stale.setCookie(s, "menagerie-session", id)
	if r := stale.get(s.srv.URL + "/api/thing"); r.status != http.StatusUnauthorized {
		t.Fatalf("ended session answered %d", r.status)
	}
	// Signing out again, without a session, still succeeds.
	b.stop = SignedOutPath
	if r := b.do(http.MethodPost, s.srv.URL+LogoutPath, nil); r.status != http.StatusSeeOther {
		t.Fatalf("second logout answered %d", r.status)
	}
	if r := b.get(s.srv.URL + LogoutPath); r.status != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout answered %d", r.status)
	}
}

func TestCrossOriginProtection(t *testing.T) {
	cases := []struct {
		name    string
		header  http.Header
		allowed bool
	}{
		{"same-origin fetch", http.Header{"Sec-Fetch-Site": {"same-origin"}}, true},
		{"user-initiated", http.Header{"Sec-Fetch-Site": {"none"}}, true},
		{"cross-site", http.Header{"Sec-Fetch-Site": {"cross-site"}}, false},
		{"same-site", http.Header{"Sec-Fetch-Site": {"same-site"}}, false},
		{"cross-site naming our origin", http.Header{"Sec-Fetch-Site": {"cross-site"}, "Origin": {"http://evil.example"}}, false},
		{"origin only, other host", http.Header{"Origin": {"http://evil.example"}}, false},
		{"origin null", http.Header{"Origin": {"null"}}, false},
		{"origin only, this host", nil, true}, // Origin set per site below
		{"no browser headers", http.Header{}, true},
	}
	for _, path := range []string{"/api/thing", LogoutPath} {
		for _, c := range cases {
			t.Run(path+" "+c.name, func(t *testing.T) {
				s := newSite(t)
				b := s.browser()
				id := b.signIn(s)
				s.hits.Store(0)
				header := c.header
				if header == nil {
					header = http.Header{"Origin": {s.srv.URL}}
				}
				b.stop = SignedOutPath
				r := b.do(http.MethodPost, s.srv.URL+path, header)
				if !c.allowed {
					if r.status != http.StatusForbidden || errorKind(t, r) != "cross-origin-request" {
						t.Fatalf("answered %d %q", r.status, r.body)
					}
					if s.hits.Load() != 0 {
						t.Fatal("protected handler ran")
					}
					if _, ok := s.auth.sessions.get(id); !ok {
						t.Fatal("refused request ended the session")
					}
					return
				}
				if r.status >= 400 {
					t.Fatalf("answered %d %q", r.status, r.body)
				}
				if path == LogoutPath {
					if _, ok := s.auth.sessions.get(id); ok {
						t.Fatal("logout kept the session")
					}
				} else if s.hits.Load() != 1 {
					t.Fatal("protected handler did not run")
				}
			})
		}
	}
}

func TestConfiguredOriginIsTrustedBehindAProxy(t *testing.T) {
	a, err := New(Config{Issuer: "https://issuer.example.invalid", ClientID: testClientID, RedirectURL: "https://menagerie.example.invalid" + CallbackPath})
	if err != nil {
		t.Fatal(err)
	}
	var reached bool
	h := a.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080"+LogoutPath, nil)
	req.Header.Set("Origin", "https://menagerie.example.invalid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !reached {
		t.Fatalf("proxied same-origin logout answered %d", rec.Code)
	}
}

func TestCookieAttributes(t *testing.T) {
	for _, c := range []struct {
		scheme, session, login string
		secure                 bool
	}{
		{"https", "__Host-menagerie-session", "__Host-menagerie-login", true},
		{"http", "menagerie-session", "menagerie-login", false},
	} {
		t.Run(c.scheme, func(t *testing.T) {
			is := authtest.New(t, testClientID, c.scheme+"://menagerie.example.invalid"+CallbackPath)
			is.User = manager
			a, err := New(Config{Issuer: is.URL(), ClientID: testClientID, RedirectURL: c.scheme + "://menagerie.example.invalid" + CallbackPath})
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			a.Register(mux)
			h := a.Wrap(mux)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, LoginPath, nil))
			login := findCookie(t, rec.Result().Cookies(), c.login)
			checkCookie(t, login, c.secure)

			// Follow the provider to the callback, carrying the login cookie.
			resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(rec.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			cb := httptest.NewRequest(http.MethodGet, resp.Header.Get("Location"), nil)
			cb.AddCookie(&http.Cookie{Name: c.login, Value: login.Value})
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, cb)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("callback answered %d %s", rec.Code, rec.Body)
			}
			checkCookie(t, findCookie(t, rec.Result().Cookies(), c.session), c.secure)
		})
	}
}

func findCookie(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", name, cookies)
	return nil
}

func checkCookie(t *testing.T, c *http.Cookie, secure bool) {
	t.Helper()
	if c.Secure != secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" ||
		c.MaxAge != 0 || !c.Expires.IsZero() || len(c.Value) != tokenLength {
		t.Fatalf("cookie %s", c.String())
	}
}

func TestPendingLoginsAreBounded(t *testing.T) {
	p := newPendingLogins(2)
	p.add("a", pendingLogin{nonce: "1"})
	p.add("b", pendingLogin{nonce: "2"})
	p.add("c", pendingLogin{nonce: "3"})
	if _, ok := p.take("a"); ok {
		t.Fatal("oldest pending sign-in kept")
	}
	if l, ok := p.take("c"); !ok || l.nonce != "3" {
		t.Fatal("newest pending sign-in lost")
	}
	if _, ok := p.take("c"); ok {
		t.Fatal("pending sign-in taken twice")
	}
	if l, ok := p.take("b"); !ok || l.nonce != "2" {
		t.Fatal("pending sign-in lost")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]string{
		"ftp://menagerie.example.invalid/auth/callback":          "must use http or https",
		"https:///auth/callback":                                 "must name a host",
		"https://user@menagerie.example.invalid/auth/callback":   "must not hold user information",
		"https://menagerie.example.invalid/auth/callback?x=1":    "must not have a query or fragment",
		"https://menagerie.example.invalid/auth/callback?":       "must not have a query or fragment",
		"https://menagerie.example.invalid/auth/callback#x":      "must not have a query or fragment",
		"https://menagerie.example.invalid/auth/callback/":       "must have the path /auth/callback",
		"https://menagerie.example.invalid/prefix/auth/callback": "must have the path /auth/callback",
		"menagerie.example.invalid/auth/callback":                "must use http or https",
	}
	for raw, want := range bad {
		if err := ValidateRedirectURL(raw); err == nil || err.Error() != want {
			t.Errorf("ValidateRedirectURL(%q) = %v, want %q", raw, err, want)
		}
	}
	for _, ok := range []string{"https://menagerie.example.invalid/auth/callback", "http://127.0.0.1:8080/auth/callback"} {
		if err := ValidateRedirectURL(ok); err != nil {
			t.Errorf("ValidateRedirectURL(%q) = %v", ok, err)
		}
	}
	if err := ValidateScopes([]string{"profile"}); err == nil {
		t.Error("scopes without openid accepted")
	}
	if err := ValidateScopes([]string{"openid", "two words"}); err == nil {
		t.Error("scope with a space accepted")
	}
	if err := ValidateScopes(nil); err != nil {
		t.Error(err)
	}
	if _, err := New(Config{Issuer: "issuer.example.invalid", ClientID: "", RedirectURL: "x"}); err == nil ||
		!strings.Contains(err.Error(), "issuer must be") || !strings.Contains(err.Error(), "client id") || !strings.Contains(err.Error(), "redirect URL") {
		t.Errorf("New accepted an invalid configuration: %v", err)
	}
}

func TestSpecificationNamesThePublicRoutes(t *testing.T) {
	data, err := os.ReadFile("../../../docs/specifications/authentication.yml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(spec.Description), " ")
	_, after, ok := strings.Cut(text, "Public routes are exactly ")
	if !ok {
		t.Fatal("specification does not list the public routes")
	}
	list, _, _ := strings.Cut(after, ", compared on")
	var documented []string
	for _, item := range strings.Split(strings.ReplaceAll(list, " and ", ", "), ", ") {
		route := strings.Trim(item, "`")
		_, path, _ := strings.Cut(route, " ")
		documented = append(documented, path)
	}
	var code []string
	for p := range publicPaths {
		code = append(code, p)
	}
	slices.Sort(documented)
	slices.Sort(code)
	if !slices.Equal(documented, code) {
		t.Fatalf("specification lists %q, code exempts %q", documented, code)
	}
}
