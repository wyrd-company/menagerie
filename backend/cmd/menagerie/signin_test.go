package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/auth/authtest"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// recordingRuntime counts container operations and reports no container.
// An operation logs one line and waits for release.
type recordingRuntime struct {
	mu      sync.Mutex
	calls   []string
	release chan struct{}
}

func (r *recordingRuntime) Status(context.Context, string) (devcontainer.Status, error) {
	return devcontainer.Status{State: devcontainer.StateNoContainer}, nil
}

func (r *recordingRuntime) record(name string) func(context.Context, string, devcontainer.Progress) error {
	return func(_ context.Context, folder string, p devcontainer.Progress) error {
		r.mu.Lock()
		r.calls = append(r.calls, name+" "+filepath.Base(folder))
		r.mu.Unlock()
		p.Phase(name)
		fmt.Fprintf(p.Log(""), "operation-log-line %s\n", filepath.Base(folder))
		<-r.release
		return nil
	}
}

func (r *recordingRuntime) Start(ctx context.Context, f string, p devcontainer.Progress) error {
	return r.record("start")(ctx, f, p)
}
func (r *recordingRuntime) Stop(ctx context.Context, f string, p devcontainer.Progress) error {
	return r.record("stop")(ctx, f, p)
}
func (r *recordingRuntime) Rebuild(ctx context.Context, f string, p devcontainer.Progress) error {
	return r.record("rebuild")(ctx, f, p)
}

func (r *recordingRuntime) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// menagerie is the composed workspace service: the real api.Server with the
// web UI, behind signedIn, against a test OIDC issuer.
type menagerie struct {
	t       *testing.T
	srv     *httptest.Server
	issuer  *authtest.Issuer
	root    string
	server  *api.Server
	jobs    *jobs.Manager
	runtime *recordingRuntime
}

const clientID = "sample-client"

func newMenagerie(t *testing.T) *menagerie {
	t.Helper()
	m := &menagerie{t: t, root: t.TempDir(), runtime: &recordingRuntime{release: make(chan struct{})}}
	t.Cleanup(func() { close(m.runtime.release); m.jobs.Wait() })
	for _, name := range []string{"alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(m.root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(m.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.jobs = jobs.NewManager(ctx)
	m.server = api.New(api.Options{
		Root:       root,
		Status:     m.runtime,
		Jobs:       m.jobs,
		Operations: api.ContainerOperations(root, m.runtime),
		Templates:  []api.Template{{ID: "sample-template", Name: "Sample"}},
		UI: fstest.MapFS{
			"index.html":    {Data: []byte("<!doctype html><title>ui-index</title>")},
			"assets/app.js": {Data: []byte("ui-asset")},
		},
	})
	var h http.Handler
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(m.srv.Close)
	m.issuer = authtest.New(t, clientID, m.srv.URL+auth.CallbackPath)
	m.issuer.User = authtest.User{Subject: "subject-manager", Name: "Sample Manager", Groups: []string{auth.ManagersGroup}}
	h, err = signedIn(m.server, config.OIDC{Issuer: m.issuer.URL(), ClientID: clientID, RedirectURL: m.srv.URL + auth.CallbackPath})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *menagerie) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		// Follow the sign-in flow; stop at any other redirect.
		if strings.HasPrefix(req.URL.Path, "/auth/") || strings.HasPrefix(req.URL.String(), m.issuer.URL()) {
			return nil
		}
		return http.ErrUseLastResponse
	}}
}

// anonymousClient keeps cookies and follows no redirect, so it never
// signs in.
func anonymousClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// signIn signs a browser in as the issuer's current user.
func (m *menagerie) signIn(c *http.Client) int {
	m.t.Helper()
	resp, err := c.Get(m.srv.URL + auth.LoginPath)
	if err != nil {
		m.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type answer struct {
	status int
	header http.Header
	body   string
}

func (m *menagerie) do(c *http.Client, method, path, body string, header http.Header) answer {
	m.t.Helper()
	req, err := http.NewRequest(method, m.srv.URL+path, strings.NewReader(body))
	if err != nil {
		m.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header, string(b)}
}

// mountedRoutes lists every route the composed service serves, with path
// values filled in: the API routes the server reports, the web UI pages
// and assets, and the sign-in routes.
func (m *menagerie) mountedRoutes(jobID string) []api.Route {
	routes := m.server.Routes()
	routes = append(routes,
		api.Route{Method: http.MethodGet, Pattern: "/"},
		api.Route{Method: http.MethodGet, Pattern: "/create"},
		api.Route{Method: http.MethodGet, Pattern: "/workspaces/alpha/upgrade"},
		api.Route{Method: http.MethodGet, Pattern: "/assets/app.js"},
		api.Route{Method: http.MethodGet, Pattern: auth.LoginPath},
		api.Route{Method: http.MethodGet, Pattern: auth.CallbackPath},
		api.Route{Method: http.MethodPost, Pattern: auth.LogoutPath},
		api.Route{Method: http.MethodGet, Pattern: auth.SignedOutPath},
	)
	for i, r := range routes {
		routes[i].Pattern = strings.NewReplacer("{id}", jobID, "{name}", "alpha").Replace(r.Pattern)
	}
	return routes
}

// public are the routes that answer without a session.
var public = []string{"GET /healthz", "GET " + auth.LoginPath, "GET " + auth.CallbackPath, "POST " + auth.LogoutPath, "GET " + auth.SignedOutPath}

const startAlpha = `{"operation":"start","workspace":"alpha"}`

// startJob starts a held job on alpha as a manager and returns its id.
func (m *menagerie) startJob(c *http.Client) string {
	m.t.Helper()
	a := m.do(c, http.MethodPost, "/api/jobs", startAlpha, http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}})
	if a.status != http.StatusAccepted {
		m.t.Fatalf("manager start answered %d %s", a.status, a.body)
	}
	var job struct{ JobID string }
	_ = json.Unmarshal([]byte(a.body), &job)
	return job.JobID
}

func TestEveryMountedRouteRequiresAManagerSession(t *testing.T) {
	m := newMenagerie(t)
	managerClient := m.client()
	if s := m.signIn(managerClient); s != http.StatusSeeOther {
		t.Fatalf("sign-in ended with %d", s)
	}
	jobID := m.startJob(managerClient)
	calls := m.runtime.callCount()

	anonymous := anonymousClient()
	for _, r := range m.mountedRoutes(jobID) {
		key := r.Method + " " + r.Pattern
		if slices.Contains(public, key) {
			continue
		}
		a := m.do(anonymous, r.Method, r.Pattern, startAlpha, nil)
		isAPI := strings.HasPrefix(r.Pattern, "/api/")
		switch {
		case isAPI && (a.status != http.StatusUnauthorized || !strings.Contains(a.body, `"kind":"unauthenticated"`)):
			t.Errorf("%s answered %d %q", key, a.status, a.body)
		case !isAPI && r.Method == http.MethodGet && (a.status != http.StatusSeeOther || a.header.Get("Location") != auth.LoginPath+"?return="+url.QueryEscape(r.Pattern)):
			t.Errorf("%s answered %d Location %q", key, a.status, a.header.Get("Location"))
		case strings.Contains(a.body, "operation-log-line") || strings.Contains(a.body, "ui-"):
			t.Errorf("%s leaked %q", key, a.body)
		}
	}
	if got := m.do(anonymous, http.MethodGet, "/healthz", "", nil); got.status != http.StatusOK {
		t.Errorf("health answered %d", got.status)
	}
	if got := m.do(anonymous, http.MethodGet, "/api/no-such-route", "", nil); got.status != http.StatusUnauthorized {
		t.Errorf("unknown API route answered %d", got.status)
	}
	if m.runtime.callCount() != calls || len(m.jobs.List()) != 1 {
		t.Fatalf("anonymous requests ran operations: %v", m.runtime.calls)
	}

	// The manager reaches every protected route.
	for _, r := range m.mountedRoutes(jobID) {
		key := r.Method + " " + r.Pattern
		if slices.Contains(public, key) || key == "GET /api/jobs/"+jobID+"/events" || r.Method != http.MethodGet {
			continue
		}
		if a := m.do(managerClient, r.Method, r.Pattern, "", nil); a.status != http.StatusOK {
			t.Errorf("manager %s answered %d %q", key, a.status, a.body)
		}
	}
}

func TestDeniedRequestsChangeNothing(t *testing.T) {
	m := newMenagerie(t)
	before, _ := os.ReadDir(m.root)
	jsonHeader := http.Header{"Content-Type": {"application/json"}}

	anonymous := anonymousClient()
	if a := m.do(anonymous, http.MethodPost, "/api/jobs", startAlpha, jsonHeader); a.status != http.StatusUnauthorized {
		t.Fatalf("anonymous start answered %d %s", a.status, a.body)
	}

	m.issuer.Set(func(is *authtest.Issuer) {
		is.User = authtest.User{Subject: "subject-other", Name: "Sample Other", Groups: []string{"other"}}
	})
	other := m.client()
	if s := m.signIn(other); s != http.StatusForbidden {
		t.Fatalf("non-manager sign-in answered %d", s)
	}
	if a := m.do(other, http.MethodPost, "/api/jobs", startAlpha, jsonHeader); a.status != http.StatusUnauthorized {
		t.Fatalf("non-manager start answered %d %s", a.status, a.body)
	}

	m.issuer.Set(func(is *authtest.Issuer) {
		is.User = authtest.User{Subject: "subject-manager", Name: "Sample Manager", Groups: []string{auth.ManagersGroup}}
	})
	managerClient := m.client()
	m.signIn(managerClient)
	forged := http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"cross-site"}, "Origin": {"http://evil.example"}}
	if a := m.do(managerClient, http.MethodPost, "/api/jobs", startAlpha, forged); a.status != http.StatusForbidden {
		t.Fatalf("cross-site start answered %d %s", a.status, a.body)
	}
	if m.runtime.callCount() != 0 || len(m.jobs.List()) != 0 {
		t.Fatalf("denied requests ran operations: %v, jobs %v", m.runtime.calls, m.jobs.List())
	}
	after, _ := os.ReadDir(m.root)
	if len(after) != len(before) {
		t.Fatalf("workspace root changed: %v", after)
	}

	// Once a job exists, its state, events and logs stay hidden from
	// anyone without a manager session.
	jobID := m.startJob(managerClient)
	for _, p := range []string{"/api/jobs", "/api/jobs/" + jobID, "/api/jobs/" + jobID + "/events", "/api/jobs/" + jobID + "/events?lastEventId=0", "/api/workspaces"} {
		for name, c := range map[string]*http.Client{"anonymous": anonymous, "non-manager": other} {
			a := m.do(c, http.MethodGet, p, "", nil)
			if a.status != http.StatusUnauthorized || strings.Contains(a.body, jobID) || strings.Contains(a.body, "operation-log-line") {
				t.Errorf("%s GET %s answered %d %q", name, p, a.status, a.body)
			}
		}
	}
	if m.runtime.callCount() != 1 {
		t.Fatalf("operations %v", m.runtime.calls)
	}
}

func TestManagerStreamsJobEventsThroughTheGate(t *testing.T) {
	m := newMenagerie(t)
	c := m.client()
	m.signIn(c)
	jobID := m.startJob(c)

	resp, err := c.Get(m.srv.URL + "/api/jobs/" + jobID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events answered %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The log line arrives while the operation is still held, so the
	// gate passes the stream through unbuffered.
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		if strings.Contains(lines.Text(), "operation-log-line alpha") {
			return
		}
	}
	t.Fatalf("stream ended without the log line: %v", lines.Err())
}

func TestLogoutEndsAccess(t *testing.T) {
	m := newMenagerie(t)
	c := m.client()
	m.signIn(c)
	if a := m.do(c, http.MethodGet, "/api/workspaces", "", nil); a.status != http.StatusOK {
		t.Fatalf("workspaces answered %d", a.status)
	}
	// A plain form post from the UI carries Origin and Sec-Fetch-Site.
	a := m.do(c, http.MethodPost, auth.LogoutPath, "", http.Header{"Origin": {m.srv.URL}, "Sec-Fetch-Site": {"same-origin"}, "Content-Type": {"application/x-www-form-urlencoded"}})
	if a.status != http.StatusOK || !strings.Contains(a.body, "signed out") {
		t.Fatalf("logout answered %d %q", a.status, a.body)
	}
	if a := m.do(c, http.MethodGet, "/api/workspaces", "", nil); a.status != http.StatusUnauthorized {
		t.Fatalf("after logout workspaces answered %d", a.status)
	}
}
