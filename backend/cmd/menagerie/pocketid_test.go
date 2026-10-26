//go:build pocketid

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// pocketIDImage is the Pocket ID release the gate signs in through.
const pocketIDImage = "ghcr.io/pocket-id/pocket-id:v2.18.0@sha256:323e7ef5bfacf7cf32f22f3d10ccdf48e0c97c94d67e7e40b0b996830deb5165"

// readyAttempts bounds the wait for Pocket ID's discovery document, at
// readyInterval apart. It started in about one second on the shared host.
const (
	readyAttempts = 120
	readyInterval = 500 * time.Millisecond
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func randomHex() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pocketID is a disposable Pocket ID container with synthetic keys.
type pocketID struct {
	t      *testing.T
	docker *dockerclient.Client
	id     string
	url    string
	apiKey string
}

func startPocketID(t *testing.T) *pocketID {
	t.Helper()
	return startPocketIDAt(t, "127.0.0.1", freePort(t))
}

// startPocketIDAt publishes Pocket ID on hostIP:port of the Docker Engine's
// host. Its URL is http://localhost:port either way.
func startPocketIDAt(t *testing.T, hostIP string, port int) *pocketID {
	t.Helper()
	ctx := context.Background()
	docker, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { docker.Close() })
	if _, err := docker.ImageInspect(ctx, pocketIDImage); err != nil {
		rc, err := docker.ImagePull(ctx, pocketIDImage, image.PullOptions{})
		if err != nil {
			t.Fatalf("pull %s: %v", pocketIDImage, err)
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
	p := &pocketID{t: t, docker: docker, url: fmt.Sprintf("http://localhost:%d", port), apiKey: randomHex()}
	created, err := docker.ContainerCreate(ctx,
		&container.Config{
			Image:        pocketIDImage,
			Env:          []string{"APP_URL=" + p.url, "ENCRYPTION_KEY=" + randomHex(), "STATIC_API_KEY=" + p.apiKey, "TRUST_PROXY=false"},
			ExposedPorts: nat.PortSet{"1411/tcp": {}},
			Labels:       map[string]string{"menagerie-test": "pocketid"},
		},
		&container.HostConfig{PortBindings: nat.PortMap{"1411/tcp": {{HostIP: hostIP, HostPort: fmt.Sprint(port)}}}},
		nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	p.id = created.ID
	// Remove by the recorded id, whatever the test's outcome.
	t.Cleanup(func() {
		_ = docker.ContainerRemove(context.Background(), p.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := docker.ContainerStart(ctx, p.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		resp, err := http.Get(p.url + "/.well-known/openid-configuration")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if i == readyAttempts {
			t.Fatalf("Pocket ID did not serve discovery: %v", err)
		}
		time.Sleep(readyInterval)
	}
	return p
}

// admin calls the Pocket ID admin API with the static API key.
func (p *pocketID) admin(method, path string, body any) map[string]any {
	p.t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, p.url+path, bytes.NewReader(data))
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		p.t.Fatalf("%s %s answered %d", method, path, resp.StatusCode)
	}
	return out
}

func (p *pocketID) user(username, first, last string, groupID string) {
	p.t.Helper()
	u := p.admin(http.MethodPost, "/api/users", map[string]any{"username": username, "email": username + "@example.test", "firstName": first, "lastName": last, "isAdmin": false})
	if groupID != "" {
		p.admin(http.MethodPut, "/api/user-groups/"+groupID+"/users", map[string]any{"userIds": []any{u["id"]}})
	}
}

// signInLink returns the token of a one-time sign-in link for username,
// from Pocket ID's own command.
func (p *pocketID) signInLink(username string) string {
	p.t.Helper()
	ctx := context.Background()
	exec, err := p.docker.ContainerExecCreate(ctx, p.id, container.ExecOptions{Cmd: []string{"/app/pocket-id", "one-time-access-token", username}, AttachStdout: true, AttachStderr: true})
	if err != nil {
		p.t.Fatal(err)
	}
	att, err := p.docker.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		p.t.Fatal(err)
	}
	defer att.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil {
		p.t.Fatal(err)
	}
	_, token, ok := strings.Cut(out.String(), "/lc/")
	if !ok {
		p.t.Fatalf("no sign-in link in %q", out.String())
	}
	return strings.TrimSpace(token)
}

type step struct {
	status   int
	location string
	body     string
	url      *url.URL
}

// signInBrowser is a cookie-keeping client that stops at Pocket ID's
// interaction page and follows every other redirect.
func signInBrowser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Path == "/interaction" {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

func fetch(t *testing.T, c *http.Client, method, target string, body io.Reader, header http.Header) step {
	t.Helper()
	req, _ := http.NewRequest(method, target, body)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return step{resp.StatusCode, resp.Header.Get("Location"), string(b), resp.Request.URL}
}

// signIn signs c into Pocket ID as username, opens target on Menagerie and
// grants consent when Pocket ID asks for it, the way its own page does.
// It returns the last step and the callback URL Menagerie answered.
func (p *pocketID) signIn(t *testing.T, c *http.Client, username, target string) (step, string) {
	t.Helper()
	if s := fetch(t, c, http.MethodPost, p.url+"/api/one-time-access-token/"+p.signInLink(username), nil, nil); s.status != http.StatusOK {
		t.Fatalf("one-time sign-in answered %d", s.status)
	}
	var callback string
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if req.URL.Path == auth.CallbackPath {
			callback = req.URL.String()
		}
		if req.URL.Path == "/interaction" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	s := fetch(t, c, http.MethodGet, target, nil, nil)
	if s.status >= 300 && s.status < 400 && strings.HasPrefix(s.location, "/interaction?interaction=") {
		id := strings.TrimPrefix(s.location, "/interaction?interaction=")
		consent := fetch(t, c, http.MethodPost, p.url+"/api/oidc/interactions/"+id+"/complete", strings.NewReader(`{"step":"consent"}`), http.Header{"Content-Type": {"application/json"}})
		if consent.status != http.StatusOK {
			t.Fatalf("consent answered %d %s", consent.status, consent.body)
		}
		s = fetch(t, c, http.MethodGet, p.url+"/authorize?interaction="+id, nil, nil)
	}
	return s, callback
}

func TestSignInThroughPocketID(t *testing.T) {
	p := startPocketID(t)

	// Menagerie: the real API server and a web UI, composed as main does.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := &recordingRuntime{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	manager := jobs.NewManager(ctx, jobs.Options{})
	t.Cleanup(func() { close(rt.release); manager.Wait(); cancel() })
	server := api.New(api.Options{Root: root, Status: rt, Jobs: manager, Operations: api.ContainerOperations(root, rt),
		UI: fstest.MapFS{"index.html": {Data: []byte("ui-index")}}})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	site := fmt.Sprintf("http://localhost:%d", l.Addr().(*net.TCPAddr).Port)

	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	other := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Other", "name": "other"})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	p.user("sample-other", "Sample", "Other", other["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})
	if _, ok := client["createdSecret"]; ok {
		t.Fatal("public client was given a secret")
	}

	h, err := signedIn(server, config.OIDC{Issuer: p.url, ClientID: client["id"].(string), RedirectURL: site + auth.CallbackPath})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)

	t.Run("manager", func(t *testing.T) {
		c := signInBrowser()
		s, callback := p.signIn(t, c, "sample-manager", site+"/workspaces/alpha/upgrade?step=1")
		if s.status != http.StatusOK || s.url.String() != site+"/workspaces/alpha/upgrade?step=1" || s.body != "ui-index" {
			t.Fatalf("sign-in ended at %s with %d %q", s.url, s.status, s.body)
		}
		q, _ := url.ParseQuery(strings.SplitN(callback, "?", 2)[1])
		if q.Get("iss") != p.url {
			t.Errorf("callback iss %q", q.Get("iss"))
		}
		if s := fetch(t, c, http.MethodGet, site+"/api/workspaces", nil, nil); s.status != http.StatusOK || !strings.Contains(s.body, `"name":"alpha"`) {
			t.Fatalf("workspaces answered %d %q", s.status, s.body)
		}
		if s := fetch(t, c, http.MethodGet, callback, nil, nil); s.status != http.StatusBadRequest || !strings.Contains(s.body, `data-kind="sign-in-expired"`) {
			t.Fatalf("replayed callback answered %d", s.status)
		}
		if s := fetch(t, c, http.MethodPost, site+auth.LogoutPath, nil, http.Header{"Origin": {site}, "Sec-Fetch-Site": {"same-origin"}}); s.status != http.StatusOK || s.url.Path != auth.SignedOutPath {
			t.Fatalf("logout answered %d at %s", s.status, s.url)
		}
		if s := fetch(t, c, http.MethodGet, site+"/api/workspaces", nil, nil); s.status != http.StatusUnauthorized {
			t.Fatalf("after logout workspaces answered %d", s.status)
		}
	})

	t.Run("non-manager", func(t *testing.T) {
		c := signInBrowser()
		s, _ := p.signIn(t, c, "sample-other", site+"/")
		if s.status != http.StatusForbidden || !strings.Contains(s.body, `data-kind="not-a-manager"`) || !strings.Contains(s.body, "Sample Other") {
			t.Fatalf("non-manager sign-in answered %d %q", s.status, s.body)
		}
		start := fetch(t, c, http.MethodPost, site+"/api/jobs", strings.NewReader(`{"operation":"start","workspace":"alpha"}`), http.Header{"Content-Type": {"application/json"}})
		if start.status != http.StatusUnauthorized || rt.callCount() != 0 || len(manager.List()) != 0 {
			t.Fatalf("non-manager start answered %d; operations %v", start.status, rt.calls)
		}
	})
}
