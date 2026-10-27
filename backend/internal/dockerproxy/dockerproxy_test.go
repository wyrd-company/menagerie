package dockerproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// engine is a fake Engine behind a fake proxy: it denies container
// creation with body and records each request's workspace header.
type engine struct {
	body string

	mu      sync.Mutex
	headers []string
}

func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.headers = append(e.headers, r.URL.Path+" "+r.Header.Get(HeaderWorkspace))
	e.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/_ping") {
		w.Header().Set("Api-Version", "1.47")
		_, _ = io.WriteString(w, "OK")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, e.body)
}

func create(t *testing.T, body string, headers map[string]string) (error, *engine) {
	t.Helper()
	e := &engine{body: body}
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+srv.Listener.Addr().String())
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	client, err := NewClient(headers)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.ContainerCreate(context.Background(), &container.Config{Image: "alpine:3.22", Env: []string{"SECRET=env-value"}}, nil, nil, nil, "")
	return err, e
}

func denialBody(reason string) string {
	raw, _ := json.Marshal(map[string]string{"message": proxyMessage, "method": "POST", "path": "/containers/create", "reason": reason})
	return string(raw)
}

func TestDenialKeepsTheProxyReason(t *testing.T) {
	err, _ := create(t, denialBody("container create denied: privileged mode is not allowed"), nil)
	d := Denial(err)
	if d == nil {
		t.Fatalf("Denial(%v) = nil, want the proxy's denial", err)
	}
	if d.Reason != "container create denied: privileged mode is not allowed" {
		t.Errorf("reason = %q", d.Reason)
	}
	// Wrapping text after the reason does not join it.
	wrapped := fmt.Errorf("up: %w (container abc)", err)
	if d := Denial(wrapped); d == nil || d.Reason != "container create denied: privileged mode is not allowed" {
		t.Errorf("Denial(wrapped) = %+v", d)
	}
	if strings.Contains(Describe(d).Error(), "env-value") {
		t.Errorf("Describe names the request's environment: %v", Describe(d))
	}
}

func TestDenialReasonStaysOnOneLine(t *testing.T) {
	err, _ := create(t, denialBody("bind source\n/srv/other is outside\tthe workspace"), nil)
	d := Denial(err)
	if d == nil || d.Reason != "bind source /srv/other is outside the workspace" {
		t.Fatalf("Denial = %+v, want one line", d)
	}
}

func TestOtherForbiddenResponsesAreNotDenials(t *testing.T) {
	for name, body := range map[string]string{
		"engine message": `{"message":"forbidden by the daemon"}`,
		"not json":       `denied`,
	} {
		t.Run(name, func(t *testing.T) {
			err, _ := create(t, body, nil)
			if err == nil {
				t.Fatal("create succeeded")
			}
			if d := Denial(err); d != nil {
				t.Errorf("Denial = %+v, want nil", d)
			}
		})
	}
}

func TestADenialWithoutAReasonSaysSo(t *testing.T) {
	err, _ := create(t, `{"message":"request denied by sockguard policy"}`, nil)
	if d := Denial(err); d == nil || d.Reason != noReason {
		t.Fatalf("Denial = %+v, want the no-reason denial", d)
	}
}

func TestOversizedDenialPassesUnchanged(t *testing.T) {
	err, _ := create(t, denialBody(strings.Repeat("x", maxDenial)), nil)
	if d := Denial(err); d != nil {
		t.Errorf("Denial = %+v, want nil for a body over the bound", d)
	}
}

func TestDenialOfNoError(t *testing.T) {
	if Denial(nil) != nil {
		t.Error("Denial(nil) is not nil")
	}
	// Only a 403 from the Engine API carries a denial: text that reads like
	// one elsewhere is not.
	if d := Denial(errors.New("up: " + marker + `"made up"`)); d != nil {
		t.Errorf("Denial of a plain error = %+v", d)
	}
	if d := Denial(fmt.Errorf("x: %w", &Denied{Reason: "r"})); d == nil || d.Reason != "r" {
		t.Errorf("Denial of a wrapped *Denied = %+v", d)
	}
}

func TestNewClientSendsTheWorkspaceHeader(t *testing.T) {
	_, e := create(t, denialBody("r"), map[string]string{HeaderWorkspace: "/srv/workspaces/alpha"})
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.headers) == 0 {
		t.Fatal("no request reached the engine")
	}
	for _, h := range e.headers {
		if !strings.HasSuffix(h, " /srv/workspaces/alpha") {
			t.Errorf("request %q lacks the workspace header", h)
		}
	}
}

func TestWorkspaceResolvesTheRoot(t *testing.T) {
	real := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Workspace(filepath.Join(link, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(resolved, "alpha") {
		t.Errorf("Workspace = %q, want %q", got, filepath.Join(resolved, "alpha"))
	}
	if _, err := Workspace(filepath.Join(t.TempDir(), "absent", "alpha")); err == nil {
		t.Error("Workspace of an absent root succeeded")
	}
}
