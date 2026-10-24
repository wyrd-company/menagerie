package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

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
