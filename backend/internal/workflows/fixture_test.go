package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// Sessions hold values a lossy decode would change.
const (
	sessionAsking = `{"step":1,"big":12345678901234567890,"html":"<a>&b"}`
	sessionDone   = `{"step":2,"exp":1e2,"esc":"é"}`
	batch         = `{"status":"questions","context":{"template":"sample@1"},"schema":{"properties":{"label":{"type":"string"}}}}`
)

func reply(state templates.InterviewState, session, document string) *templates.Reply {
	raw := fmt.Sprintf(`{"session":%s,"state":%q,"document":%s,"warnings":[]}`, session, state, document)
	return &templates.Reply{Raw: json.RawMessage(raw), Session: json.RawMessage(session), State: state, Document: json.RawMessage(document)}
}

func appliedReply(outcome templates.Outcome, files []templates.File, conflicts []string) *templates.Applied {
	a := &templates.Applied{Outcome: outcome, Files: files, Messages: []string{}, Hooks: []templates.Hook{}, Conflicts: conflicts}
	if outcome.Wrote() {
		record := ".toha.json"
		a.RecordPath = &record
	}
	if a.Files == nil {
		a.Files = []templates.File{}
	}
	if a.Conflicts == nil {
		a.Conflicts = []string{}
	}
	a.Raw, _ = json.Marshal(a)
	return a
}

// fakeTemplates is a template service in this process. Each step is a
// function the test can replace; calls and the sessions they carry are
// recorded.
type fakeTemplates struct {
	mu       sync.Mutex
	calls    []string
	sessions map[string]string
	// tokens holds the hook token of each generate and apply call.
	tokens map[string]string

	startInterview  func(templates.Selector) (*templates.Reply, error)
	answerInterview func(templates.Selector, json.RawMessage, json.RawMessage) (*templates.Reply, error)
	generate        func(templates.Selector) (*templates.Applied, error)
	startUpgrade    func(templates.Selector) (*templates.Reply, error)
	answerUpgrade   func(templates.Selector, json.RawMessage, json.RawMessage) (*templates.Reply, error)
	applyUpgrade    func(templates.Selector) (*templates.Applied, error)
}

func (f *fakeTemplates) record(call string, session json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if session != nil {
		f.sessions[call] = string(session)
	}
}

func (f *fakeTemplates) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeTemplates) session(call string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[call]
}

func (f *fakeTemplates) StartInterview(_ context.Context, s templates.Selector) (*templates.Reply, error) {
	f.record("start-interview", nil)
	return f.startInterview(s)
}
func (f *fakeTemplates) AnswerInterview(_ context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error) {
	f.record("answer-interview", session)
	return f.answerInterview(s, session, submission)
}
func (f *fakeTemplates) Generate(_ context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error) {
	f.record("generate", session)
	f.keepToken("generate", hookToken)
	return f.generate(s)
}
func (f *fakeTemplates) StartUpgrade(_ context.Context, s templates.Selector) (*templates.Reply, error) {
	f.record("start-upgrade", nil)
	return f.startUpgrade(s)
}
func (f *fakeTemplates) AnswerUpgrade(_ context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error) {
	f.record("answer-upgrade", session)
	return f.answerUpgrade(s, session, submission)
}
func (f *fakeTemplates) ApplyUpgrade(_ context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error) {
	f.record("apply-upgrade", session)
	f.keepToken("apply-upgrade", hookToken)
	return f.applyUpgrade(s)
}

func (f *fakeTemplates) keepToken(call, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens == nil {
		f.tokens = map[string]string{}
	}
	f.tokens[call] = token
}

func (f *fakeTemplates) token(call string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens[call]
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Error(err)
		return
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Error(err)
	}
}

// defaultTemplates asks one batch, completes, and generates a workspace
// with a hook's output.
func defaultTemplates(t *testing.T) *fakeTemplates {
	f := &fakeTemplates{sessions: map[string]string{}}
	f.startInterview = func(templates.Selector) (*templates.Reply, error) {
		return reply(templates.Questions, sessionAsking, batch), nil
	}
	f.answerInterview = func(templates.Selector, json.RawMessage, json.RawMessage) (*templates.Reply, error) {
		return reply(templates.Completed, sessionDone, "null"), nil
	}
	f.generate = func(s templates.Selector) (*templates.Applied, error) {
		writeFile(t, s.Target, "result.txt", "label: first\n")
		writeFile(t, s.Target, "local.txt", "first: template\n")
		writeFile(t, s.Target, ".toha.json", `{"applications":[{"v":1}]}`)
		writeFile(t, s.Target, "hook-marker.txt", "ran\n")
		return appliedReply(templates.Written, []templates.File{{Path: "result.txt", Action: "create"}, {Path: "local.txt", Action: "create"}}, nil), nil
	}
	f.startUpgrade = func(templates.Selector) (*templates.Reply, error) {
		return reply(templates.Questions, sessionAsking, batch), nil
	}
	f.answerUpgrade = f.answerInterview
	f.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
		writeFile(t, s.Target, "added.txt", "new in v2\n")
		writeFile(t, s.Target, ".toha.json", `{"applications":[{"v":2}]}`)
		return appliedReply(templates.Written, []templates.File{{Path: "added.txt", Action: "added"}}, nil), nil
	}
	return f
}

// fakeRuntime records starts and rebuilds.
type fakeRuntime struct {
	mu    sync.Mutex
	calls []string
	fail  error
}

func (r *fakeRuntime) do(op, folder string, p devcontainer.Progress) error {
	p.Phase(op)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, op+" "+filepath.Base(folder))
	return r.fail
}
func (r *fakeRuntime) Start(_ context.Context, folder string, p devcontainer.Progress) error {
	return r.do("up", folder, p)
}
func (r *fakeRuntime) Rebuild(_ context.Context, folder string, p devcontainer.Progress) error {
	return r.do("rebuild", folder, p)
}
func (r *fakeRuntime) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// world is a workspace root, a bare workspace repository, and the API
// server composed with the workflows.
type world struct {
	root    string
	remote  string
	tmpl    *fakeTemplates
	rt      *fakeRuntime
	manager *jobs.Manager
	flows   *Workflows
	http    *httptest.Server
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{root: t.TempDir(), remote: filepath.Join(t.TempDir(), "backups.git"), tmpl: defaultTemplates(t), rt: &fakeRuntime{}}
	if _, err := git.PlainInit(w.remote, true); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(w.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.manager = jobs.NewManager(ctx, jobs.Options{})
	w.flows = New(Options{
		Root:      root,
		Templates: w.tmpl,
		Approved:  map[string]bool{"sample-v1": true, "sample-v2": true},
		Remote:    &backup.Remote{URL: w.remote},
		Runtime:   w.rt,
	})
	server := api.New(api.Options{Root: root, Jobs: w.manager, Operations: w.flows.Operations(), Interviews: w.flows})
	w.http = httptest.NewServer(server)
	t.Cleanup(w.http.Close)
	// Cleanups run last first: cancel ends jobs still waiting for answers,
	// then Wait lets every runner return.
	t.Cleanup(w.manager.Wait)
	t.Cleanup(cancel)
	return w
}

func (w *world) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, w.http.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// startJob posts a job and returns it.
func (w *world) startJob(t *testing.T, operation, name, template string) *jobs.Job {
	t.Helper()
	status, body := w.do(t, http.MethodPost, "/api/jobs", fmt.Sprintf(`{"operation":%q,"workspace":%q,"templateId":%q}`, operation, name, template))
	if status != http.StatusAccepted {
		t.Fatalf("start %s: %d %s", operation, status, body)
	}
	var snap jobs.Snapshot
	_ = json.Unmarshal([]byte(body), &snap)
	job, ok := w.manager.Get(snap.JobID)
	if !ok {
		t.Fatalf("job %s not found", snap.JobID)
	}
	return job
}

func (w *world) answer(t *testing.T, job *jobs.Job, session string) (int, string) {
	t.Helper()
	return w.do(t, http.MethodPost, "/api/jobs/"+job.ID()+"/answers", `{"session":`+session+`,"submission":{"template":"sample@1","answers":{"label":"first"}}}`)
}

// waitEvent blocks until the job has an event matching match and returns
// it.
func waitEvent(t *testing.T, job *jobs.Job, match func(jobs.Event) bool) jobs.Event {
	t.Helper()
	var cursor int64
	for {
		events, ended, changed := job.EventsAfter(cursor)
		for _, e := range events {
			if match(e) {
				return e
			}
			cursor = e.ID
		}
		if ended {
			t.Fatalf("job ended without the awaited event: %+v", job.Snapshot())
		}
		<-changed
	}
}

// waitWaiting blocks until the job's latest state event is waiting.
func waitWaiting(t *testing.T, job *jobs.Job, after int64) jobs.Event {
	t.Helper()
	return waitEvent(t, job, func(e jobs.Event) bool {
		return e.ID > after && e.Type == jobs.EventState && strings.Contains(string(e.Data), `"waiting-for-answers"`)
	})
}

func waitEnded(t *testing.T, job *jobs.Job) jobs.Snapshot {
	t.Helper()
	for {
		_, ended, changed := job.EventsAfter(0)
		if ended {
			return job.Snapshot()
		}
		<-changed
	}
}

func events(job *jobs.Job, eventType string) []string {
	all, _, _ := job.EventsAfter(0)
	var out []string
	for _, e := range all {
		if e.Type == eventType {
			out = append(out, string(e.Data))
		}
	}
	return out
}

func logText(job *jobs.Job) string {
	var b strings.Builder
	for _, d := range events(job, jobs.EventLog) {
		var l struct{ Text string }
		_ = json.Unmarshal([]byte(d), &l)
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

func result(job *jobs.Job) string {
	r := events(job, jobs.EventResult)
	if len(r) != 1 {
		return ""
	}
	return r[0]
}

// remoteHistory returns the remote workspace branch's first-parent chain,
// newest first.
func (w *world) remoteHistory(t *testing.T, name string) []*object.Commit {
	t.Helper()
	repo, err := git.PlainOpen(w.remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), false)
	if err != nil {
		return nil
	}
	var out []*object.Commit
	for hash := ref.Hash(); ; {
		c, err := repo.CommitObject(hash)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
		if c.NumParents() == 0 {
			return out
		}
		hash = c.ParentHashes[0]
	}
}

func fileAt(t *testing.T, c *object.Commit, name string) string {
	t.Helper()
	f, err := c.File(name)
	if err != nil {
		return ""
	}
	body, _ := f.Contents()
	return body
}

func treeNames(t *testing.T, c *object.Commit) []string {
	t.Helper()
	files, _ := c.Files()
	var names []string
	_ = files.ForEach(func(f *object.File) error { names = append(names, f.Name); return nil })
	return names
}

// createWorkspace runs a complete create of name.
func (w *world) createWorkspace(t *testing.T, name string) {
	t.Helper()
	job := w.startJob(t, "create", name, "sample-v1")
	waitWaiting(t, job, 0)
	if status, body := w.answer(t, job, sessionAsking); status != http.StatusOK {
		t.Fatalf("answer %d %s", status, body)
	}
	if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("create %+v %+v", snap, snap.Error)
	}
}
