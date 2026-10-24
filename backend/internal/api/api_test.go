package api

import (
	"bufio"
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
	"testing/fstest"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// fakeRuntime records the folders it is asked about and can hold an
// operation until released.
type fakeRuntime struct {
	mu        sync.Mutex
	statuses  map[string]devcontainer.Status
	statusErr error
	asked     []string
	hold      chan struct{}
	entered   chan string
	ctxErr    chan error
	fail      error
}

func (f *fakeRuntime) Status(_ context.Context, folder string) (devcontainer.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, folder)
	if f.statusErr != nil {
		return devcontainer.Status{}, f.statusErr
	}
	if s, ok := f.statuses[folder]; ok {
		return s, nil
	}
	return devcontainer.Status{State: devcontainer.StateNoContainer}, nil
}

func (f *fakeRuntime) op(name string) func(context.Context, string, devcontainer.Progress) error {
	return func(ctx context.Context, folder string, p devcontainer.Progress) error {
		p.Phase(name)
		fmt.Fprintf(p.Log(""), "%s %s\n", name, filepath.Base(folder))
		if f.entered != nil {
			f.entered <- folder
		}
		if f.hold != nil {
			<-f.hold
		}
		if f.ctxErr != nil {
			f.ctxErr <- ctx.Err()
		}
		return f.fail
	}
}

func (f *fakeRuntime) Start(ctx context.Context, folder string, p devcontainer.Progress) error {
	return f.op("start")(ctx, folder, p)
}
func (f *fakeRuntime) Stop(ctx context.Context, folder string, p devcontainer.Progress) error {
	return f.op("stop")(ctx, folder, p)
}
func (f *fakeRuntime) Rebuild(ctx context.Context, folder string, p devcontainer.Progress) error {
	return f.op("rebuild")(ctx, folder, p)
}

type fixture struct {
	root    string
	rt      *fakeRuntime
	manager *jobs.Manager
	server  *Server
	http    *httptest.Server
}

func newFixture(t *testing.T, ui fsFS) *fixture {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"beta", "alpha", "gamma"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{statuses: map[string]devcontainer.Status{
		dir + "/alpha": {State: devcontainer.StateRunning, ContainerID: "c-alpha"},
		dir + "/beta":  {State: devcontainer.StateStopped, ContainerID: "c-beta"},
		// A container labelled for a path outside the root is never asked for.
		"/elsewhere/outsider": {State: devcontainer.StateRunning, ContainerID: "c-outsider"},
	}}
	manager := jobs.NewManager(context.Background())
	opts := Options{
		Root:       root,
		Status:     rt,
		Jobs:       manager,
		Operations: ContainerOperations(root, rt),
		Templates:  []Template{{ID: "sample-template", Name: "Sample template"}},
	}
	if ui != nil {
		opts.UI = ui
	}
	server := New(opts)
	hs := httptest.NewServer(server)
	t.Cleanup(hs.Close)
	return &fixture{root: dir, rt: rt, manager: manager, server: server, http: hs}
}

type fsFS = fstest.MapFS

func (f *fixture) do(t *testing.T, method, path, body string, header ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return v
}

type errorBody struct {
	Error struct {
		Kind        string `json:"kind"`
		Message     string `json:"message"`
		ActiveJobID string `json:"activeJobId"`
	} `json:"error"`
}

func TestListWorkspacesJoinsFoldersOnly(t *testing.T) {
	f := newFixture(t, nil)
	resp, data := f.do(t, "GET", "/api/workspaces", "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	want := `{"workspaces":[` +
		`{"name":"alpha","state":"running","containerId":"c-alpha","activeJob":null},` +
		`{"name":"beta","state":"stopped","containerId":"c-beta","activeJob":null},` +
		`{"name":"gamma","state":"no-container","containerId":null,"activeJob":null}]}`
	if strings.TrimSpace(string(data)) != want {
		t.Fatalf("got %s", data)
	}
	if got := strings.Join(f.rt.asked, ","); got != f.root+"/alpha,"+f.root+"/beta,"+f.root+"/gamma" {
		t.Fatalf("asked %s", got)
	}
}

func TestListWorkspacesReportsDockerFailure(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.statusErr = errors.New("cannot connect")
	resp, data := f.do(t, "GET", "/api/workspaces", "")
	body := decode[errorBody](t, data)
	if resp.StatusCode != 503 || body.Error.Kind != "docker-unavailable" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
}

func TestTemplates(t *testing.T) {
	f := newFixture(t, nil)
	_, data := f.do(t, "GET", "/api/templates", "")
	if strings.TrimSpace(string(data)) != `{"templates":[{"id":"sample-template","name":"Sample template"}]}` {
		t.Fatalf("got %s", data)
	}
}

func TestStartJobAndStream(t *testing.T) {
	f := newFixture(t, nil)
	resp, data := f.do(t, "POST", "/api/jobs", `{"operation":"rebuild","workspace":"alpha"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	snap := decode[jobs.Snapshot](t, data)
	if resp.Header.Get("Location") != "/api/jobs/"+snap.JobID || snap.Workspace != "alpha" || snap.Operation != "rebuild" {
		t.Fatalf("job %+v location %s", snap, resp.Header.Get("Location"))
	}
	events := readStream(t, f, snap.JobID, "")
	got := describe(events)
	want := `state {"state":"running","phase":""}|state {"state":"running","phase":"rebuild"}|log {"text":"rebuild alpha"}|result {"state":"succeeded","error":null}`
	if got != want {
		t.Fatalf("events\n%s\nwant\n%s", got, want)
	}
	// Replay after event 2 by header and by query parameter.
	if got := describe(readStream(t, f, snap.JobID, "2")); !strings.HasPrefix(got, "log ") {
		t.Fatalf("header replay %s", got)
	}
	resp, data = f.do(t, "GET", "/api/jobs/"+snap.JobID+"/events?lastEventId=3", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(string(data), "id: 4\nevent: result\n") {
		t.Fatalf("query replay %d %q", resp.StatusCode, data)
	}

	_, data = f.do(t, "GET", "/api/jobs/"+snap.JobID, "")
	done := decode[jobs.Snapshot](t, data)
	if done.State != jobs.Succeeded || done.LastEventID != 4 || done.FinishedAt == nil {
		t.Fatalf("snapshot %+v", done)
	}
	_, data = f.do(t, "GET", "/api/jobs", "")
	list := decode[struct{ Jobs []jobs.Snapshot }](t, data)
	if len(list.Jobs) != 1 || list.Jobs[0].JobID != snap.JobID {
		t.Fatalf("list %s", data)
	}
}

type sseEvent struct{ id, event, data string }

func readStream(t *testing.T, f *fixture, id, last string) []sseEvent {
	t.Helper()
	req, _ := http.NewRequest("GET", f.http.URL+"/api/jobs/"+id+"/events", nil)
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			out = append(out, cur)
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		default:
			t.Fatalf("unexpected line %q", line)
		}
	}
	for i, e := range out {
		if last == "" && e.id != fmt.Sprint(i+1) {
			t.Fatalf("event %d id %s", i, e.id)
		}
	}
	return out
}

func describe(events []sseEvent) string {
	var parts []string
	for _, e := range events {
		parts = append(parts, e.event+" "+e.data)
	}
	return strings.Join(parts, "|")
}

// TestHeldJobExcludesSameWorkspace holds a job inside the runtime, then
// proves a competing request for the same workspace is refused, another
// workspace runs, and the list shows the active job.
func TestHeldJobExcludesSameWorkspace(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.hold = make(chan struct{})
	f.rt.entered = make(chan string, 4)
	resp, data := f.do(t, "POST", "/api/jobs", `{"operation":"start","workspace":"alpha"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	held := decode[jobs.Snapshot](t, data)
	if folder := <-f.rt.entered; folder != f.root+"/alpha" {
		t.Fatalf("held %s", folder)
	}

	resp, data = f.do(t, "POST", "/api/jobs", `{"operation":"stop","workspace":"alpha"}`)
	body := decode[errorBody](t, data)
	if resp.StatusCode != 409 || body.Error.Kind != "workspace-busy" || body.Error.ActiveJobID != held.JobID {
		t.Fatalf("competing %d %s", resp.StatusCode, data)
	}

	_, data = f.do(t, "GET", "/api/workspaces", "")
	if !strings.Contains(string(data), `"name":"alpha","state":"running","containerId":"c-alpha","activeJob":{"jobId":"`+held.JobID+`","operation":"start","state":"running"}`) {
		t.Fatalf("list %s", data)
	}

	resp, data = f.do(t, "POST", "/api/jobs", `{"operation":"stop","workspace":"beta"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("other workspace %d %s", resp.StatusCode, data)
	}
	if folder := <-f.rt.entered; folder != f.root+"/beta" {
		t.Fatalf("entered %s", folder)
	}
	f.rt.hold <- struct{}{} // releases whichever op reads first
	f.rt.hold <- struct{}{}
	f.manager.Wait()
	resp, _ = f.do(t, "POST", "/api/jobs", `{"operation":"stop","workspace":"alpha"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("slot not released: %d", resp.StatusCode)
	}
	<-f.rt.entered
	close(f.rt.hold)
	f.manager.Wait()
}

func TestJobOutlivesTheRequest(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.hold = make(chan struct{})
	f.rt.ctxErr = make(chan error, 1)
	resp, _ := f.do(t, "POST", "/api/jobs", `{"operation":"rebuild","workspace":"gamma"}`)
	if resp.StatusCode != 202 {
		t.Fatal(resp.StatusCode)
	}
	// The POST has returned and its request context is done.
	close(f.rt.hold)
	if err := <-f.rt.ctxErr; err != nil {
		t.Fatalf("job context ended with the request: %v", err)
	}
	f.manager.Wait()
}

func TestFailureKinds(t *testing.T) {
	cases := map[string]error{
		"initialize-command-failed":     &devcontainer.InitializeError{Err: errors.New("exit status 3")},
		"credential-helper-unsupported": fmt.Errorf("start: %w", &devcontainer.CredentialHelperError{ConfigFile: "/sample/config.json"}),
		"operation-failed":              errors.New("up: boom"),
	}
	for kind, failure := range cases {
		f := newFixture(t, nil)
		f.rt.fail = failure
		_, data := f.do(t, "POST", "/api/jobs", `{"operation":"start","workspace":"alpha"}`)
		snap := decode[jobs.Snapshot](t, data)
		f.manager.Wait()
		_, data = f.do(t, "GET", "/api/jobs/"+snap.JobID, "")
		done := decode[jobs.Snapshot](t, data)
		if done.State != jobs.Failed || done.Error == nil || done.Error.Kind != kind {
			t.Errorf("%s: %s", kind, data)
		}
	}
}

func TestStartJobRejects(t *testing.T) {
	f := newFixture(t, nil)
	cases := []struct {
		body       string
		status     int
		kind       string
		mentionsOp string
	}{
		{`{"operation":"start","workspace":"missing"}`, 404, "workspace-not-found", ""},
		{`{"operation":"start","workspace":".."}`, 400, "invalid-workspace-name", ""},
		{`{"operation":"start","workspace":"alpha/x"}`, 400, "invalid-workspace-name", ""},
		{`{"operation":"create","workspace":"delta","templateId":"sample-template"}`, 400, "unknown-operation", `"create"`},
		{`{"operation":"start","workspace":"alpha","extra":1}`, 400, "invalid-request", ""},
		{`not json`, 400, "invalid-request", ""},
		{`{"operation":"start","workspace":"alpha"} {"operation":"stop"}`, 400, "invalid-request", "exactly one JSON document"},
	}
	for _, tc := range cases {
		resp, data := f.do(t, "POST", "/api/jobs", tc.body)
		body := decode[errorBody](t, data)
		if resp.StatusCode != tc.status || body.Error.Kind != tc.kind || !strings.Contains(body.Error.Message, tc.mentionsOp) {
			t.Errorf("%s: %d %s", tc.body, resp.StatusCode, data)
		}
	}
	if len(f.manager.List()) != 0 {
		t.Fatal("a rejected request created a job")
	}
}

func TestUnknownJobAndRoutes(t *testing.T) {
	f := newFixture(t, nil)
	for _, path := range []string{"/api/jobs/unknown", "/api/jobs/unknown/events"} {
		resp, data := f.do(t, "GET", path, "")
		if body := decode[errorBody](t, data); resp.StatusCode != 404 || body.Error.Kind != "job-not-found" {
			t.Errorf("%s: %d %s", path, resp.StatusCode, data)
		}
	}
	resp, data := f.do(t, "GET", "/api/nothing", "")
	if body := decode[errorBody](t, data); resp.StatusCode != 404 || body.Error.Kind != "not-found" {
		t.Errorf("unknown route: %d %s", resp.StatusCode, data)
	}
	job, _ := f.manager.Start("alpha", "start", func(context.Context, *jobs.Run) error { return nil })
	f.manager.Wait()
	resp, data = f.do(t, "GET", "/api/jobs/"+job.ID()+"/events", "", "Last-Event-ID", "x")
	if body := decode[errorBody](t, data); resp.StatusCode != 400 || body.Error.Kind != "invalid-request" {
		t.Errorf("bad last event id: %d %s", resp.StatusCode, data)
	}
	resp, _ = f.do(t, "GET", "/", "")
	if resp.StatusCode != 404 {
		t.Errorf("UI route served without UI: %d", resp.StatusCode)
	}
	resp, data = f.do(t, "GET", "/healthz", "")
	if resp.StatusCode != 200 || string(data) != "ok\n" {
		t.Errorf("healthz %d %q", resp.StatusCode, data)
	}
}

func TestUIRoutes(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html>index")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	f := newFixture(t, ui)
	for _, path := range []string{"/", "/create", "/workspaces/alpha/upgrade"} {
		resp, data := f.do(t, "GET", path, "")
		if resp.StatusCode != 200 || string(data) != "<!doctype html>index" {
			t.Errorf("%s: %d %q", path, resp.StatusCode, data)
		}
	}
	resp, data := f.do(t, "GET", "/assets/app.js", "")
	if resp.StatusCode != 200 || string(data) != "console.log(1)" {
		t.Errorf("asset %d %q", resp.StatusCode, data)
	}
	if resp, _ := f.do(t, "GET", "/elsewhere", ""); resp.StatusCode != 404 {
		t.Errorf("unknown page %d", resp.StatusCode)
	}
}

func TestHandleAddsRoutes(t *testing.T) {
	f := newFixture(t, nil)
	f.server.Handle("POST /api/jobs/{id}/answers", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	if resp, _ := f.do(t, "POST", "/api/jobs/x/answers", "{}"); resp.StatusCode != http.StatusTeapot {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

// wrapped hides http.Flusher, as authentication middleware can.
type wrapped struct{ http.ResponseWriter }

func (w wrapped) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestEventsStreamThroughWrappingMiddleware(t *testing.T) {
	f := newFixture(t, nil)
	job, _ := f.manager.Start("alpha", "start", func(context.Context, *jobs.Run) error { return nil })
	f.manager.Wait()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/jobs/"+job.ID()+"/events", nil)
	f.server.ServeHTTP(wrapped{rec}, req)
	if rec.Code != 200 || !rec.Flushed || !strings.Contains(rec.Body.String(), "event: result") {
		t.Fatalf("%d flushed=%t %q", rec.Code, rec.Flushed, rec.Body.String())
	}
}

func TestWriteEventFramesEveryPayloadLine(t *testing.T) {
	var b strings.Builder
	err := writeEvent(&b, jobs.Event{ID: 7, Type: "interview", Data: json.RawMessage("{\r\n  \"n\": 12345678901234567890,\n  \"s\": \"a\"\n}")})
	if err != nil {
		t.Fatal(err)
	}
	want := "id: 7\nevent: interview\ndata: {\ndata:   \"n\": 12345678901234567890,\ndata:   \"s\": \"a\"\ndata: }\n\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

// TestEmittedMultilinePayloadStreamsAsOneEvent sends a pretty-printed
// payload with an integer beyond float64 precision through Emit and the
// stream: it arrives as one event with its numeric text unchanged.
func TestEmittedMultilinePayloadStreamsAsOneEvent(t *testing.T) {
	f := newFixture(t, nil)
	payload := "{\n  \"session\": {\"count\": 12345678901234567890},\n  \"state\": \"questions\"\n}"
	job, _ := f.manager.Start("alpha", "create", func(_ context.Context, run *jobs.Run) error {
		return run.Emit("interview", json.RawMessage(payload))
	})
	f.manager.Wait()
	events := readStream(t, f, job.ID(), "")
	if len(events) != 3 || events[1].event != "interview" {
		t.Fatalf("events %+v", events)
	}
	if events[1].data != `{"session":{"count":12345678901234567890},"state":"questions"}` {
		t.Fatalf("data %q", events[1].data)
	}
}

// TestStartJobRejectsTrailingInput sends a valid job request followed by
// extra input: each must be refused before a job or runtime call exists.
func TestStartJobRejectsTrailingInput(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.entered = make(chan string, 8)
	valid := `{"operation":"start","workspace":"alpha"}`
	for _, trailing := range []string{"}", "]", ` {"operation":"stop"}`, " x", ",", " 1", ` "s"`} {
		resp, data := f.do(t, "POST", "/api/jobs", valid+trailing)
		if body := decode[errorBody](t, data); resp.StatusCode != 400 || body.Error.Kind != "invalid-request" {
			t.Errorf("trailing %q: %d %s", trailing, resp.StatusCode, data)
		}
	}
	if n := len(f.manager.List()); n != 0 {
		t.Fatalf("%d jobs created from rejected bodies", n)
	}
	if len(f.rt.entered) != 0 {
		t.Fatal("runtime called for a rejected body")
	}
	// Trailing whitespace is still one document.
	resp, data := f.do(t, "POST", "/api/jobs", valid+" \n\t")
	if resp.StatusCode != 202 {
		t.Fatalf("trailing whitespace: %d %s", resp.StatusCode, data)
	}
	<-f.rt.entered
	f.manager.Wait()
}
