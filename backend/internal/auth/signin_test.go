package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/auth/authtest"
)

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
		"///evil.example/":       "/",
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
