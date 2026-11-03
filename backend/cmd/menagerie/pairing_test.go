package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/auth/authtest"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// The composition run serves mints only for a same-origin manager. A daemon
// that does not answer shows the request got past the gate to the minter's
// status read; anything refused earlier never reaches Docker.
func TestPairingTokenRouteIsWiredBehindManagerSignIn(t *testing.T) {
	const clientID = "sample-client"
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	docker, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://" + filepath.Join(dir, "absent.sock")))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	issuer := authtest.New(t, clientID, srv.URL+auth.CallbackPath)
	issuer.User = authtest.User{Subject: "subject-manager", Name: "Sample Manager", Groups: []string{auth.ManagersGroup}}
	cfg := &config.Config{
		WorkspaceRoot:   dir,
		TemplateService: config.TemplateService{Socket: filepath.Join(dir, "absent.sock")},
		OIDC:            config.OIDC{Issuer: issuer.URL(), ClientID: clientID, RedirectURL: srv.URL + auth.CallbackPath},
	}
	h, err = newHandler(cfg, root, backupRemote(cfg, nil), devcontainer.New(docker, devcontainer.Options{}), jobs.NewManager(context.Background(), jobs.Options{}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/api/workspaces/alpha/t3-code-pairing-tokens"
	m := &menagerie{t: t, srv: srv, issuer: issuer}
	c := m.client()
	if a := m.do(c, http.MethodPost, path, "", http.Header{"Sec-Fetch-Site": {"same-origin"}}); a.status != http.StatusUnauthorized {
		t.Errorf("signed out: %d %s", a.status, a.body)
	}
	if s := m.signIn(c); s != http.StatusSeeOther {
		t.Fatalf("sign-in ended with %d", s)
	}
	if a := m.do(c, http.MethodPost, path, "", http.Header{"Sec-Fetch-Site": {"cross-site"}}); a.status != http.StatusForbidden || !strings.Contains(a.body, "cross-origin-request") {
		t.Errorf("cross-site: %d %s", a.status, a.body)
	}
	if a := m.do(c, http.MethodPost, path, "", http.Header{"Sec-Fetch-Site": {"same-origin"}}); a.status != http.StatusServiceUnavailable || !strings.Contains(a.body, "docker-unavailable") {
		t.Errorf("same-origin manager: %d %s", a.status, a.body)
	}
}
