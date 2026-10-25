package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wyrd-company/menagerie/backend/internal/auth/authtest"
)

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
