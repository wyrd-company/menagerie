package templates

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeService serves canned replies on a Unix socket and records each
// request's path and exact body.
type fakeService struct {
	socket string

	mu       sync.Mutex
	paths    []string
	bodies   []string
	status   int
	ctype    string
	response string
}

func newFake(t *testing.T) *fakeService {
	t.Helper()
	f := &fakeService{socket: filepath.Join(t.TempDir(), "template.sock"), status: 200, ctype: "application/json"}
	l, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.bodies = append(f.bodies, string(body))
		status, ctype, response := f.status, f.ctype, f.response
		f.mu.Unlock()
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeService) answer(status int, response string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.response = status, response
}

func (f *fakeService) last() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[len(f.paths)-1], f.bodies[len(f.bodies)-1]
}

var sel = Selector{Workspace: "alpha", Target: "/srv/workspaces/alpha", TemplateID: "sample"}

// session holds values a lossy decode would change: a 20-digit integer,
// an exponent, an escaped character, HTML-significant text and member order.
const session = `{"z":1,"big":12345678901234567890,"exp":1e2,"esc":"é","html":"<a>&b","a":[1.50]}`

func TestAnswersForwardTheSessionAndSubmissionText(t *testing.T) {
	f := newFake(t)
	f.answer(200, `{"session": `+session+`, "state": "questions", "document": {"status":"questions","n":1.0}, "warnings": []}`)
	c := New(f.socket)
	submission := `{"template":"t","answers":{"n":0.10,"s":"<x>"}}`
	for _, call := range []struct {
		path string
		do   func() (*Reply, error)
	}{
		{"/v1/interviews/answers", func() (*Reply, error) {
			return c.AnswerInterview(context.Background(), sel, json.RawMessage(session), json.RawMessage(submission))
		}},
		{"/v1/upgrades/answers", func() (*Reply, error) {
			return c.AnswerUpgrade(context.Background(), sel, json.RawMessage(session), json.RawMessage(submission))
		}},
	} {
		reply, err := call.do()
		if err != nil {
			t.Fatal(err)
		}
		path, body := f.last()
		want := `{"workspace":"alpha","target":"/srv/workspaces/alpha","templateId":"sample","session":` + session + `,"submission":` + submission + "}\n"
		if path != call.path || body != want {
			t.Fatalf("request %s %s\nwant %s", path, body, want)
		}
		if string(reply.Session) != session {
			t.Fatalf("reply session %s", reply.Session)
		}
		if string(reply.Raw) != `{"session":`+session+`,"state":"questions","document":{"status":"questions","n":1.0},"warnings":[]}` {
			t.Fatalf("reply text %s", reply.Raw)
		}
		if reply.State != Questions || string(reply.Document) != `{"status":"questions","n":1.0}` {
			t.Fatalf("reply %+v", reply)
		}
	}
}

func TestStartRoutesCarryOnlyTheSelector(t *testing.T) {
	f := newFake(t)
	f.answer(200, `{"session":{},"state":"completed","document":null,"warnings":["w"]}`)
	c := New(f.socket)
	for path, start := range map[string]func(context.Context, Selector) (*Reply, error){
		"/v1/interviews": c.StartInterview,
		"/v1/upgrades":   c.StartUpgrade,
	} {
		reply, err := start(context.Background(), sel)
		if err != nil {
			t.Fatal(err)
		}
		got, body := f.last()
		if got != path || body != `{"workspace":"alpha","target":"/srv/workspaces/alpha","templateId":"sample"}`+"\n" {
			t.Fatalf("%s %s", got, body)
		}
		if reply.State != Completed || string(reply.Document) != "null" || reply.Warnings[0] != "w" {
			t.Fatalf("%+v", reply)
		}
	}
}

func TestApplyOutcomesAreTyped(t *testing.T) {
	f := newFake(t)
	c := New(f.socket)
	cases := []struct {
		reply   string
		outcome Outcome
		wrote   bool
	}{
		{`{"outcome":"written","files":[{"path":"a.txt","action":"create"}],"recordPath":".toha.json","messages":[],"hooks":[{"id":null,"exitCode":0,"success":true}],"conflicts":[]}`, Written, true},
		{`{"outcome":"conflicted","files":[{"path":"b.txt","action":"conflicted"}],"recordPath":".toha.json","messages":[],"hooks":[],"conflicts":["b.txt"]}`, Conflicted, true},
		{`{"outcome":"already-current","files":[],"recordPath":null,"messages":[],"hooks":[],"conflicts":[]}`, AlreadyCurrent, false},
		{`{"outcome":"needs-trust","files":[],"recordPath":null,"messages":[],"hooks":[],"conflicts":[]}`, NeedsTrust, false},
		{`{"outcome":"planned","files":[{"path":"a.txt","action":"create"}],"recordPath":null,"messages":["m"],"hooks":[],"conflicts":[]}`, Planned, false},
	}
	for _, tc := range cases {
		f.answer(200, tc.reply)
		for path, apply := range map[string]func(context.Context, Selector, json.RawMessage) (*Applied, error){
			"/v1/workspaces/generate": c.Generate,
			"/v1/upgrades/apply":      c.ApplyUpgrade,
		} {
			applied, err := apply(context.Background(), sel, json.RawMessage(session))
			if err != nil {
				t.Fatal(err)
			}
			got, body := f.last()
			if got != path || body != `{"workspace":"alpha","target":"/srv/workspaces/alpha","templateId":"sample","session":`+session+"}\n" {
				t.Fatalf("%s %s", got, body)
			}
			if applied.Outcome != tc.outcome || applied.Outcome.Wrote() != tc.wrote || string(applied.Raw) != tc.reply {
				t.Fatalf("%+v", applied)
			}
		}
	}
}

func TestRefusalsKeepTheirKindAndPaths(t *testing.T) {
	f := newFake(t)
	f.answer(422, `{"error":{"kind":"hook-failed","message":"hook exited 3","paths":["out.txt"],"warnings":["w"]}}`)
	_, err := New(f.socket).Generate(context.Background(), sel, json.RawMessage(session))
	var refusal *Error
	if !errors.As(err, &refusal) {
		t.Fatalf("error %T %v", err, err)
	}
	if refusal.Status != 422 || refusal.Kind != "hook-failed" || refusal.Message != "hook exited 3" || refusal.Paths[0] != "out.txt" || refusal.Warnings[0] != "w" {
		t.Fatalf("%+v", refusal)
	}
}

func TestMalformedRepliesAreUnavailable(t *testing.T) {
	f := newFake(t)
	c := New(f.socket)
	interview := func() error { _, err := c.StartInterview(context.Background(), sel); return err }
	apply := func() error {
		_, err := c.ApplyUpgrade(context.Background(), sel, json.RawMessage(session))
		return err
	}
	cases := []struct {
		name   string
		status int
		ctype  string
		body   string
		call   func() error
	}{
		{"unknown state", 200, "application/json", `{"session":{},"state":"paused","document":null,"warnings":[]}`, interview},
		{"session not an object", 200, "application/json", `{"session":null,"state":"questions","document":{},"warnings":[]}`, interview},
		{"unknown member", 200, "application/json", `{"session":{},"state":"questions","document":{},"warnings":[],"extra":1}`, interview},
		{"trailing data", 200, "application/json", `{"session":{},"state":"questions","document":{},"warnings":[]} {}`, interview},
		{"unknown outcome", 200, "application/json", `{"outcome":"merged","files":[],"recordPath":null,"messages":[],"hooks":[],"conflicts":[]}`, apply},
		{"written without record", 200, "application/json", `{"outcome":"written","files":[],"recordPath":null,"messages":[],"hooks":[],"conflicts":[]}`, apply},
		{"error without envelope", 500, "application/json", `{"oops":true}`, apply},
		{"not json", 200, "text/plain", `ok`, apply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.mu.Lock()
			f.status, f.ctype, f.response = tc.status, tc.ctype, tc.body
			f.mu.Unlock()
			err := tc.call()
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("error %T %v", err, err)
			}
		})
	}
}

func TestAbsentSocketIsUnavailable(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "absent.sock")).StartInterview(context.Background(), sel)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "absent.sock") {
		t.Fatalf("error %T %v", err, err)
	}
}
