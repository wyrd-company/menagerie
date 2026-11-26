package workflows

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
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
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
	// reanswered holds the reanswer flag of each upgrade start.
	reanswered []bool
	// credentials holds the credential each call named, "" for none.
	credentials []string

	startInterview  func(templates.Selector) (*templates.Reply, error)
	answerInterview func(templates.Selector, json.RawMessage, json.RawMessage) (*templates.Reply, error)
	generate        func(templates.Selector) (*templates.Applied, error)
	startUpgrade    func(templates.Selector) (*templates.Reply, error)
	answerUpgrade   func(templates.Selector, json.RawMessage, json.RawMessage) (*templates.Reply, error)
	applyUpgrade    func(templates.Selector) (*templates.Applied, error)
	// checkSince answers a check with since; nil refuses it, as no test
	// expects one.
	checkSince func(address, ref, since string) (*templates.Checked, error)
}

func (f *fakeTemplates) CheckSince(_ context.Context, address, ref, since string, credential *templates.Credential) (*templates.Checked, error) {
	f.record("check-since "+ref+" "+since, nil)
	f.named(credential)
	if f.checkSince == nil {
		return nil, &templates.Error{Status: 500, Kind: "internal-error", Message: "no check was expected"}
	}
	return f.checkSince(address, ref, since)
}

func (f *fakeTemplates) named(c *templates.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c == nil {
		f.credentials = append(f.credentials, "")
		return
	}
	f.credentials = append(f.credentials, c.GitHubApp+c.SSHKey)
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
	f.named(s.Credential)
	return f.startInterview(s)
}
func (f *fakeTemplates) AnswerInterview(_ context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error) {
	f.record("answer-interview", session)
	f.named(s.Credential)
	return f.answerInterview(s, session, submission)
}
func (f *fakeTemplates) Generate(_ context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error) {
	f.record("generate", session)
	f.named(s.Credential)
	f.keepToken("generate", hookToken)
	return f.generate(s)
}
func (f *fakeTemplates) StartUpgrade(_ context.Context, s templates.Selector, reanswer bool) (*templates.Reply, error) {
	f.record("start-upgrade", nil)
	f.named(s.Credential)
	f.mu.Lock()
	f.reanswered = append(f.reanswered, reanswer)
	f.mu.Unlock()
	return f.startUpgrade(s)
}
func (f *fakeTemplates) AnswerUpgrade(_ context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error) {
	f.record("answer-upgrade", session)
	f.named(s.Credential)
	return f.answerUpgrade(s, session, submission)
}
func (f *fakeTemplates) ApplyUpgrade(_ context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error) {
	f.record("apply-upgrade", session)
	f.named(s.Credential)
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
		writeFile(t, s.Target, ".toha.json", appliedRecord(s))
		writeFile(t, s.Target, "hook-marker.txt", "ran\n")
		return appliedReply(templates.Written, []templates.File{{Path: "result.txt", Action: "create"}, {Path: "local.txt", Action: "create"}}, nil), nil
	}
	f.startUpgrade = func(templates.Selector) (*templates.Reply, error) {
		return reply(templates.Questions, sessionAsking, batch), nil
	}
	f.answerUpgrade = f.answerInterview
	f.applyUpgrade = func(s templates.Selector) (*templates.Applied, error) {
		writeFile(t, s.Target, "added.txt", "new in v2\n")
		writeFile(t, s.Target, ".toha.json", appliedRecord(s))
		return appliedReply(templates.Written, []templates.File{{Path: "added.txt", Action: "added"}}, nil), nil
	}
	return f
}

// appliedRecord is the applied record Toha writes for the version s names:
// the template's source, here its address, at s's ref.
func appliedRecord(s templates.Selector) string {
	return fmt.Sprintf(`{"applications":[{"target":".","template":%q,"source":%q,"commit":null}]}`, s.Address+"@"+s.Ref, s.Address)
}

// sampleTemplate is the registered template the tests create from, at its
// first version; newVersion moves it to its second.
var sampleTemplate = store.Template{ID: "sample", Name: "Sample", Address: "https://git.example.invalid/example-org/sample",
	Source: "https://git.example.invalid/example-org/sample", CurrentVersion: "1.0.0", CurrentTag: "1.0.0",
	CurrentCommit: "1000000000000000000000000000000000000001"}

// sampleVersions are the sample template's found versions: 0.9.0, its
// current 1.0.0, 1.1.0 and 2.0.0, which newVersion makes current.
var sampleVersions = []store.Version{
	{Version: "0.9.0", Tag: "0.9.0", Commit: "0900000000000000000000000000000000000009"},
	{Version: "1.0.0", Tag: "1.0.0", Commit: "1000000000000000000000000000000000000001"},
	{Version: "1.1.0", Tag: "1.1.0", Commit: "1100000000000000000000000000000000000011"},
	{Version: "2.0.0", Tag: "v2.0.0", Commit: "2000000000000000000000000000000000000002"},
}

// newVersion makes 2.0.0 the sample template's current version, as a
// manager's upgrade of the template does.
func (w *world) newVersion(t *testing.T) {
	t.Helper()
	_, err := w.data.AdvanceTemplate(context.Background(), "sample", "2.0.0", "v2.0.0", "2000000000000000000000000000000000000002",
		func(a, b string) bool { return a != b })
	if err != nil && !errors.Is(err, store.ErrNotNewer) {
		t.Fatal(err)
	}
}

// fakeRuntime records starts, stops and rebuilds. fail fails every
// call; failOn fails only calls of that operation. onStop runs in a stop,
// as a devcontainer that writes as it stops.
type fakeRuntime struct {
	mu     sync.Mutex
	calls  []string
	fail   error
	failOn map[string]error
	onStop func()
	// onCheck observes each host prerequisites check.
	onCheck func(folder string)
	// state is the devcontainer's state before a stop, running when
	// empty; mounting are the containers Mounting reports.
	state    string
	mounting []string
	// startWaits and rebuildWaits make a start or a rebuild wait for its
	// context to end, or for released to close.
	startWaits, rebuildWaits bool
	released                 chan struct{}
	// data is Menagerie's data, whose workspace a delete removes last.
	data *store.Store
}

func (r *fakeRuntime) Status(context.Context, string) (devcontainer.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failOn["status"]; err != nil {
		return devcontainer.Status{}, err
	}
	if r.state == "" {
		return devcontainer.Status{State: devcontainer.StateRunning}, nil
	}
	return devcontainer.Status{State: r.state}, nil
}

func (r *fakeRuntime) Mounting(context.Context, string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mounting, r.failOn["mounting"]
}

func (r *fakeRuntime) do(op, folder string, p devcontainer.Progress) error {
	p.Phase(op)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, op+" "+filepath.Base(folder))
	if err := r.failOn[op]; err != nil {
		return err
	}
	return r.fail
}
func (r *fakeRuntime) Stop(_ context.Context, folder string, p devcontainer.Progress) error {
	if r.onStop != nil {
		r.onStop()
	}
	return r.do("stop", folder, p)
}

// Start fails, as the runtime does, once its context has ended.
func (r *fakeRuntime) Start(ctx context.Context, folder string, p devcontainer.Progress) error {
	r.mu.Lock()
	waits := r.startWaits
	r.mu.Unlock()
	if waits {
		select {
		case <-ctx.Done():
		case <-r.released:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.do("up", folder, p)
}
func (r *fakeRuntime) Rebuild(ctx context.Context, folder string, p devcontainer.Progress) error {
	r.mu.Lock()
	waits := r.rebuildWaits
	r.mu.Unlock()
	err := r.do("rebuild", folder, p)
	if waits {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

// Create runs as provisioning does without a declaration: the check, the
// save, the start.
func (r *fakeRuntime) Create(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error {
	p.Phase("host-prerequisites")
	r.mu.Lock()
	if r.onCheck != nil {
		r.onCheck(folder)
	}
	err := r.failOn["host-prerequisites"]
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err := save(ctx); err != nil {
		return err
	}
	return r.Start(ctx, folder, p)
}

// Delete runs as provisioning does with no declaration: the stop, the save,
// then what removes the folder.
func (r *fakeRuntime) Delete(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error {
	if err := r.do("stop", folder, p); err != nil {
		return err
	}
	if err := save(ctx); err != nil {
		return err
	}
	if err := r.do("remove-container", folder, p); err != nil {
		return err
	}
	if err := r.do("remove-folder", folder, p); err != nil {
		return err
	}
	if err := os.RemoveAll(folder); err != nil {
		return err
	}
	if r.data == nil {
		return nil
	}
	held, err := r.data.Held(ctx, filepath.Base(folder))
	if err != nil {
		return err
	}
	return r.data.Update(ctx, func(tx *store.Tx) error {
		for _, name := range held.Names {
			if err := tx.ForgetName(filepath.Base(folder), name.Name); err != nil {
				return err
			}
		}
		for _, key := range held.Keys {
			if err := tx.ForgetKey(filepath.Base(folder), key); err != nil {
				return err
			}
		}
		return tx.RemoveWorkspace(filepath.Base(folder))
	})
}

func (r *fakeRuntime) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// world is a workspace root, a bare workspace repository, and the API
// server composed with the workflows.
type world struct {
	root   string
	remote string
	// backups is every workspace's backup repository, at remote until a
	// test points it elsewhere.
	backups *backup.Remote
	tmpl    *fakeTemplates
	rt      *fakeRuntime
	manager *jobs.Manager
	flows   *Workflows
	data    *store.Store
	http    *httptest.Server
	// cancel ends the context of every job, as Menagerie's shutdown does.
	cancel context.CancelFunc
}

func newWorld(t *testing.T) *world { return newWorldWith(t, jobs.Options{}) }

// newWorldWith is newWorld with jobs options, such as time limits.
func newWorldWith(t *testing.T, opts jobs.Options) *world {
	t.Helper()
	w := &world{root: t.TempDir(), remote: filepath.Join(t.TempDir(), "backups.git"), tmpl: defaultTemplates(t), rt: &fakeRuntime{released: make(chan struct{})}}
	if _, err := git.PlainInit(w.remote, true); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(w.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.manager = jobs.NewManager(ctx, opts)
	w.data = testData(t)
	if _, err := w.data.RegisterTemplate(context.Background(), sampleTemplate, nil); err != nil {
		t.Fatal(err)
	}
	// The sample's versions, as its last check found them.
	if _, err := w.data.RecordCheck(context.Background(), "sample", store.Credential{}, sampleVersions, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The only registered backup repository, which a create that names
	// none backs up to.
	if _, err := w.data.RegisterBackupRepository(context.Background(), store.BackupRepository{Name: "Backups", URL: w.remote}); err != nil {
		t.Fatal(err)
	}
	w.backups = &backup.Remote{URL: w.remote}
	w.rt.data = w.data
	w.flows = New(Options{
		Root:      root,
		Data:      w.data,
		Templates: w.tmpl,
		Remotes:   fixedRemote{w.backups},
		Runtime:   w.rt,
	})
	server := api.New(api.Options{Root: root, Data: w.data, Status: w.rt, Jobs: w.manager, Operations: w.flows.Operations(), Interviews: w.flows})
	w.http = httptest.NewServer(server)
	t.Cleanup(w.http.Close)
	// Cleanups run last first: cancel ends jobs still waiting for answers,
	// then Wait lets every runner return.
	t.Cleanup(w.manager.Wait)
	t.Cleanup(cancel)
	// A start still waiting when the test ends returns, so Wait ends.
	t.Cleanup(func() { close(w.rt.released) })
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

// startJob posts a job and returns it: a create of template, and an upgrade
// to the sample template's second version, which takes no template.
func (w *world) startJob(t *testing.T, operation, name, template string) *jobs.Job {
	t.Helper()
	request := fmt.Sprintf(`{"operation":%q,"workspace":%q,"templateId":%q}`, operation, name, template)
	if template == "" {
		if operation == "upgrade" {
			w.newVersion(t)
		}
		request = fmt.Sprintf(`{"operation":%q,"workspace":%q}`, operation, name)
	}
	status, body := w.do(t, http.MethodPost, "/api/jobs", request)
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
	job := w.startJob(t, "create", name, "sample")
	waitWaiting(t, job, 0)
	if status, body := w.answer(t, job, sessionAsking); status != http.StatusOK {
		t.Fatalf("answer %d %s", status, body)
	}
	if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("create %+v %+v", snap, snap.Error)
	}
}

// testData is Menagerie's data in a folder of the test's own.
func testData(t *testing.T) *store.Store {
	t.Helper()
	data, err := store.Open(filepath.Join(t.TempDir(), "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	return data
}

// fixedRemote gives every workspace the same backup repository.
type fixedRemote struct{ remote *backup.Remote }

func (f fixedRemote) For(context.Context, string) (*backup.Remote, error) { return f.remote, nil }

func (f fixedRemote) Remote(repo store.BackupRepository) *backup.Remote {
	return &backup.Remote{URL: repo.URL, Auth: f.remote.Auth}
}
