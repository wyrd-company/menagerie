package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/auth/authtest"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// TestSignedInManagersAnswerInterviewsOfAnySize serves newHandler, the
// composition run serves, with a template service that keeps
// asking. A manager's answer far above the job-request bound reaches the
// template service whole; an anonymous or cross-site answer never does.
func TestSignedInManagersAnswerInterviewsOfAnySize(t *testing.T) {
	sockDir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "template.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	const asking = `{"session":{"step":1},"state":"questions","document":{"status":"questions"},"warnings":[]}`
	var answers atomic.Int32
	var lastBody atomic.Value
	templateService := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/answers") {
			answers.Add(1)
			lastBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(asking))
	}))
	templateService.Listener = l
	templateService.Start()
	t.Cleanup(templateService.Close)

	dir := t.TempDir()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := jobs.NewManager(ctx, jobs.Options{})
	t.Cleanup(manager.Wait)
	t.Cleanup(cancel)
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	issuer := authtest.New(t, clientID, srv.URL+auth.CallbackPath)
	issuer.User = authtest.User{Subject: "subject-manager", Name: "Sample Manager", Groups: []string{auth.ManagersGroup}}
	cfg := &config.Config{
		WorkspaceRoot:   dir,
		Templates:       []config.Template{{ID: "sample", Name: "Sample", Address: "file:///absent"}},
		TemplateService: config.TemplateService{Socket: socket},
		OIDC:            config.OIDC{Issuer: issuer.URL(), ClientID: clientID, RedirectURL: srv.URL + auth.CallbackPath},
	}
	// The composition run serves.
	h, err = newHandler(cfg, root, devcontainer.New(nil, devcontainer.Options{}), manager)
	if err != nil {
		t.Fatal(err)
	}
	m := &menagerie{t: t, srv: srv, issuer: issuer}
	c := m.client()
	if s := m.signIn(c); s != http.StatusSeeOther {
		t.Fatalf("sign-in ended with %d", s)
	}
	sameOrigin := http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}}
	a := m.do(c, http.MethodPost, "/api/jobs", `{"operation":"create","workspace":"alpha","templateId":"sample"}`, sameOrigin)
	if a.status != http.StatusAccepted {
		t.Fatalf("create answered %d %s", a.status, a.body)
	}
	var snap jobs.Snapshot
	_ = json.Unmarshal([]byte(a.body), &snap)
	job, _ := manager.Get(snap.JobID)
	for job.Snapshot().State != jobs.WaitingForAnswers {
		_, ended, changed := job.EventsAfter(0)
		if ended {
			t.Fatalf("job ended: %+v", job.Snapshot())
		}
		<-changed
	}

	big := strings.Repeat("x", 2<<20)
	body := `{"session":{"step":1,"big":"` + big + `"},"submission":{"template":"t","answers":{"label":"` + big + `"}}}`
	path := "/api/jobs/" + job.ID() + "/answers"
	if a := m.do(anonymousClient(), http.MethodPost, path, body, sameOrigin); a.status != http.StatusUnauthorized {
		t.Fatalf("anonymous answer %d %s", a.status, a.body)
	}
	if a := m.do(c, http.MethodPost, path, body, http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"cross-site"}, "Origin": {"http://evil.example"}}); a.status != http.StatusForbidden {
		t.Fatalf("cross-site answer %d %s", a.status, a.body)
	}
	if answers.Load() != 0 {
		t.Fatal("a denied answer reached the template service")
	}
	if a := m.do(c, http.MethodPost, path, body, sameOrigin); a.status != http.StatusOK || a.body != asking {
		t.Fatalf("manager answer %d %.200s", a.status, a.body)
	}
	got, _ := lastBody.Load().(string)
	if answers.Load() != 1 || !strings.Contains(got, `"big":"`+big+`"`) || !strings.Contains(got, `"label":"`+big+`"`) {
		t.Fatalf("the template service received %d answers of %d bytes", answers.Load(), len(got))
	}
}
