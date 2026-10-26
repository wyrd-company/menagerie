package workspacesocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/commits"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// secret stands for values that must never reach the container: the
// remote's address, agent errors and Menagerie's paths.
const secret = "ssh://operator@remote.example.test/private-path"

// fakeRemote fails with a text holding secret while fail is set, and
// blocks while gate is open.
type fakeRemote struct {
	mu   sync.Mutex
	fail bool
	gate chan struct{}
}

func (r *fakeRemote) Push(ctx context.Context, _, _ string, progress io.Writer) (bool, error) {
	r.mu.Lock()
	fail, gate := r.fail, r.gate
	r.mu.Unlock()
	fmt.Fprintf(progress, "pushing to %s\n", secret)
	if gate != nil {
		<-gate
	}
	if fail {
		return false, errors.New("push to " + secret + " refused")
	}
	return true, nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t       *testing.T
	root    *workspace.Root
	folder  string
	remote  *fakeRemote
	jobs    *jobs.Manager
	clock   *clock
	sockets *Sockets
	client  []byte
}

type option func(*Options)

func newFixture(t *testing.T, opts ...option) *fixture {
	t.Helper()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Socket paths are short: a Unix socket address has about 100 bytes.
	folder, err := os.MkdirTemp("", "ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(folder) })
	f := &fixture{t: t, root: root, folder: folder, remote: &fakeRemote{}, jobs: jobs.NewManager(context.Background(), jobs.Options{}), clock: &clock{now: time.Unix(1_700_000_000, 0)}, client: []byte("sample client\n")}
	clientPath := filepath.Join(t.TempDir(), ClientName)
	if err := os.WriteFile(clientPath, f.client, 0o755); err != nil {
		t.Fatal(err)
	}
	o := Options{
		Folder:        folder,
		WorkspaceRoot: root.Path(),
		MinInterval:   time.Minute,
		Client:        clientPath,
		Commits:       commits.New(commits.Options{Root: root, Remote: f.remote, Jobs: f.jobs}),
		Now:           f.clock.Now,
	}
	for _, opt := range opts {
		opt(&o)
	}
	f.sockets, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.jobs.Wait)
	t.Cleanup(f.sockets.Close)
	return f
}

// workspace creates a workspace whose branch has a root commit and a
// pending change, and its socket.
func (f *fixture) workspace(name string) string {
	f.t.Helper()
	folder, _ := f.root.FolderPath(name)
	if err := os.Mkdir(folder, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := backup.Init(folder, name); err != nil {
		f.t.Fatal(err)
	}
	write(f.t, folder, ".toha.json", "{}\n")
	if _, err := backup.CommitRoot(folder, name, backup.Output{Record: ".toha.json"}, "root", io.Discard); err != nil {
		f.t.Fatal(err)
	}
	write(f.t, folder, "notes.txt", "from the container\n")
	if _, err := f.sockets.Ensure(name); err != nil {
		f.t.Fatal(err)
	}
	return folder
}

func write(t *testing.T, folder, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(folder, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) socket(name string) string {
	return filepath.Join(f.folder, name, devcontainer.WorkspaceSocketName)
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func (r answer) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
}

func (r answer) kind(t *testing.T) string {
	t.Helper()
	var e struct{ Error Error }
	r.decode(t, &e)
	return e.Error.Kind
}

// call sends one request to name's socket.
func (f *fixture) call(ctx context.Context, name, method, path string, body []byte) (answer, error) {
	socket := f.socket(name)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://workspace"+path, bytes.NewReader(body))
	if err != nil {
		return answer{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	return answer{status: res.StatusCode, header: res.Header, body: data}, err
}

func (f *fixture) post(name string, body string) answer {
	f.t.Helper()
	r, err := f.call(context.Background(), name, http.MethodPost, CommitsPath, []byte(body))
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func commitBody(message string) string {
	b, _ := json.Marshal(map[string]string{"message": message})
	return string(b)
}

func head(t *testing.T, folder string) (string, string) {
	t.Helper()
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return ref.Hash().String(), c.Message
}

func TestACommitThroughASocketCommitsOnlyItsOwnWorkspace(t *testing.T) {
	f := newFixture(t)
	alpha := f.workspace("alpha")
	beta := f.workspace("beta")
	betaHead, _ := head(t, beta)

	r := f.post("alpha", commitBody("docs: notes from the container"))
	if r.status != http.StatusOK {
		t.Fatalf("status %d %s", r.status, r.body)
	}
	var res Result
	r.decode(t, &res)
	hash, message := head(t, alpha)
	if res.State != jobs.Succeeded || res.Commit == nil || *res.Commit != hash || !res.Changed || !res.Pushed || res.Error != nil {
		t.Fatalf("result %s, head %s", r.body, hash)
	}
	if message != "docs: notes from the container" {
		t.Fatalf("message %q", message)
	}
	job, ok := f.jobs.Get(res.JobID)
	if !ok || job.Snapshot().Workspace != "alpha" {
		t.Fatalf("job %s not alpha's", res.JobID)
	}
	if h, _ := head(t, beta); h != betaHead {
		t.Fatal("beta's branch moved")
	}
	for _, j := range f.jobs.List() {
		if j.Workspace != "alpha" {
			t.Fatalf("a job ran on %s", j.Workspace)
		}
	}
}

func TestTheSocketFolderLayout(t *testing.T) {
	f := newFixture(t)
	f.workspace("alpha")
	mode := func(path string) os.FileMode {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode()
	}
	if m := mode(f.folder); m != os.ModeDir|0o700 {
		t.Errorf("folder %v", m)
	}
	dir := filepath.Join(f.folder, "alpha")
	if m := mode(dir); m != os.ModeDir|0o755 {
		t.Errorf("workspace folder %v", m)
	}
	if m := mode(f.socket("alpha")); m&os.ModeSocket == 0 || m.Perm() != 0o666 {
		t.Errorf("socket %v", m)
	}
	client := filepath.Join(dir, "bin", ClientName)
	if m := mode(client); !m.IsRegular() || m.Perm() != 0o755 {
		t.Errorf("client %v", m)
	}
	if data, _ := os.ReadFile(client); !bytes.Equal(data, f.client) {
		t.Errorf("client holds %q", data)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "bin"))
	if len(entries) != 1 {
		t.Errorf("bin holds %v", entries)
	}
}

func TestCommitsWithinTheMinimumIntervalAreRefused(t *testing.T) {
	f := newFixture(t)
	f.workspace("alpha")
	f.workspace("beta")
	if r := f.post("alpha", commitBody("first")); r.status != http.StatusOK {
		t.Fatalf("first %d %s", r.status, r.body)
	}
	f.clock.advance(15 * time.Second)
	r := f.post("alpha", commitBody("second"))
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") != "45" || r.kind(t) != KindRateLimited {
		t.Fatalf("second %d %v %s", r.status, r.header, r.body)
	}
	var e struct{ Error Error }
	r.decode(t, &e)
	if e.Error.RetryAfterSeconds != 45 {
		t.Fatalf("retry after %d", e.Error.RetryAfterSeconds)
	}
	// Each workspace has its own interval.
	if r := f.post("beta", commitBody("other")); r.status != http.StatusOK {
		t.Fatalf("beta %d %s", r.status, r.body)
	}
	f.clock.advance(45 * time.Second)
	if r := f.post("alpha", commitBody("third")); r.status != http.StatusOK {
		t.Fatalf("after the interval %d %s", r.status, r.body)
	}
}

func TestAZeroMinimumIntervalAcceptsEveryCommit(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MinInterval = 0 })
	f.workspace("alpha")
	for i := range 3 {
		if r := f.post("alpha", commitBody(fmt.Sprint("commit ", i))); r.status != http.StatusOK {
			t.Fatalf("commit %d: %d %s", i, r.status, r.body)
		}
	}
}

// hold starts a job on name that runs until the returned function is
// called.
func (f *fixture) hold(name string) (string, func()) {
	f.t.Helper()
	release := make(chan struct{})
	job, err := f.jobs.Start(name, "start", func(context.Context, *jobs.Run) error {
		<-release
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { close(release) }) }
	// A test that fails before it releases the job still ends.
	f.t.Cleanup(stop)
	return job.ID(), stop
}

func TestABusyWorkspaceIsRefusedAndNotCounted(t *testing.T) {
	f := newFixture(t)
	f.workspace("alpha")
	id, release := f.hold("alpha")
	r := f.post("alpha", commitBody("while busy"))
	var e struct{ Error Error }
	r.decode(t, &e)
	if r.status != http.StatusConflict || e.Error.Kind != KindWorkspaceBusy || e.Error.ActiveJobID != id {
		t.Fatalf("busy %d %s", r.status, r.body)
	}
	release()
	f.jobs.Wait()
	if r := f.post("alpha", commitBody("after")); r.status != http.StatusOK {
		t.Fatalf("a refused request counted toward the interval: %d %s", r.status, r.body)
	}
}

func TestMessagesAndRequestsAreChecked(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MinInterval = 0 })
	f.workspace("alpha")
	cases := []struct {
		name, body, kind string
	}{
		{"missing", `{}`, KindInvalidMessage},
		{"null", `{"message":null}`, KindInvalidMessage},
		{"empty", commitBody(""), KindInvalidMessage},
		{"blank", commitBody(" \n\t"), KindInvalidMessage},
		{"too long", commitBody(strings.Repeat("a", MaxMessage+1)), KindInvalidMessage},
		{"nul", commitBody("a\x00b"), KindInvalidMessage},
		{"escaped nul", `{"message":"a\u0000b"}`, KindInvalidMessage},
		{"invalid utf-8", "{\"message\":\"a\xffb\"}", KindInvalidMessage},
		{"not a string", `{"message":1}`, KindInvalidRequest},
		{"unknown field", `{"message":"x","workspace":"beta"}`, KindInvalidRequest},
		{"two documents", `{"message":"x"}{"message":"y"}`, KindInvalidRequest},
		{"not JSON", `message=x`, KindInvalidRequest},
		// Valid but for its size: only the body's bound refuses it.
		{"too large", `{"message":"x"}` + strings.Repeat(" ", maxBody), KindInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := f.post("alpha", tc.body)
			if r.status != http.StatusBadRequest || r.kind(t) != tc.kind {
				t.Fatalf("%d %s, want %s", r.status, r.body, tc.kind)
			}
		})
	}
	if n := len(f.jobs.List()); n != 0 {
		t.Fatalf("%d jobs started by refused requests", n)
	}
	if r := f.post("alpha", commitBody(strings.Repeat("é", MaxMessage/2))); r.status != http.StatusOK {
		t.Fatalf("a message of exactly the limit: %d %s", r.status, r.body)
	}
}

func TestOtherRoutesHaveFixedReplies(t *testing.T) {
	f := newFixture(t)
	f.workspace("alpha")
	for _, tc := range []struct {
		method, path string
		status       int
		kind         string
	}{
		{http.MethodGet, CommitsPath, http.StatusMethodNotAllowed, KindMethodNotAllowed},
		{http.MethodPut, CommitsPath, http.StatusMethodNotAllowed, KindMethodNotAllowed},
		{http.MethodPost, "/v1/commits/", http.StatusNotFound, KindNotFound},
		{http.MethodPost, "//v1/commits", http.StatusNotFound, KindNotFound},
		{http.MethodGet, "/", http.StatusNotFound, KindNotFound},
		{http.MethodPost, "/v1/workspaces/beta/commits", http.StatusNotFound, KindNotFound},
	} {
		r, err := f.call(context.Background(), "alpha", tc.method, tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.status != tc.status || r.kind(t) != tc.kind {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, r.status, r.body)
		}
	}
}

func TestAClientThatLeavesDoesNotCancelItsJob(t *testing.T) {
	f := newFixture(t)
	folder := f.workspace("alpha")
	gate := make(chan struct{})
	f.remote.gate = gate
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.call(ctx, "alpha", http.MethodPost, CommitsPath, []byte(commitBody("left early")))
		done <- err
	}()
	var job jobs.Snapshot
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if list := f.jobs.List(); len(list) == 1 && list[0].Phase == "backup" {
			job = list[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job did not reach its backup")
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the cancelled request returned a answer")
	}
	close(gate)
	f.jobs.Wait()
	got, _ := f.jobs.Get(job.JobID)
	if s := got.Snapshot(); s.State != jobs.Succeeded {
		t.Fatalf("job %+v", s)
	}
	if _, message := head(t, folder); message != "left early" {
		t.Fatalf("head %q", message)
	}
}

// allowedKeys and allowedValue are everything a socket answer may hold.
var (
	allowedKeys  = map[string]bool{"error": true, "kind": true, "activeJobId": true, "retryAfterSeconds": true, "jobId": true, "state": true, "commit": true, "changed": true, "pushed": true}
	allowedValue = regexp.MustCompile(`^([a-z]+(-[a-z]+)*|[0-9a-f]{32}|[0-9a-f]{40})$`)
)

// valueFree fails unless v holds only allowed keys, kinds, ids, hashes,
// numbers, booleans and nulls.
func valueFree(t *testing.T, label string, v any) {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			if !allowedKeys[k] {
				t.Errorf("%s: member %q", label, k)
			}
			valueFree(t, label, item)
		}
	case string:
		if !allowedValue.MatchString(v) {
			t.Errorf("%s: value %q", label, v)
		}
	case float64, bool, nil:
	default:
		t.Errorf("%s: %T %v", label, v, v)
	}
}

// failingStarter fails with an error that quotes values.
type failingStarter struct{}

func (failingStarter) Start(string, string) (*jobs.Job, *commits.Outcome, error) {
	return nil, nil, errors.New("open " + secret + ": permission denied")
}

// TestNoReplyCarriesAValue drives every answer the socket gives, including
// each failure, and checks that none carries more than kinds, ids, hashes,
// numbers and booleans.
func TestNoReplyCarriesAValue(t *testing.T) {
	f := newFixture(t)
	f.workspace("alpha")
	f.workspace("gone")
	plain, _ := f.root.FolderPath("plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sockets.Ensure("plain"); err != nil {
		t.Fatal(err)
	}
	f.workspace("failing")

	var replies []answer
	add := func(label string, want int, r answer) {
		t.Helper()
		if r.status != want {
			t.Errorf("%s: status %d, want %d: %s", label, r.status, want, r.body)
		}
		replies = append(replies, r)
	}
	add("succeeded", http.StatusOK, f.post("alpha", commitBody("x")))
	add("rate-limited", http.StatusTooManyRequests, f.post("alpha", commitBody("y")))
	for _, body := range []string{`{}`, `nope`, `{"message":"x","extra":1}`, commitBody(strings.Repeat("a", MaxMessage+1))} {
		add("invalid", http.StatusBadRequest, f.post("gone", body))
	}
	r, _ := f.call(context.Background(), "gone", http.MethodGet, CommitsPath, nil)
	add("method", http.StatusMethodNotAllowed, r)
	r, _ = f.call(context.Background(), "gone", http.MethodPost, "/"+secret, nil)
	add("route", http.StatusNotFound, r)
	_, release := f.hold("gone")
	add("busy", http.StatusConflict, f.post("gone", commitBody("x")))
	release()
	f.jobs.Wait()
	add("commit-failed", http.StatusOK, f.post("plain", commitBody("x")))
	f.remote.fail = true
	add("backup-failed", http.StatusOK, f.post("failing", commitBody("x")))
	folder, _ := f.root.FolderPath("gone")
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	add("workspace-not-found", http.StatusNotFound, f.post("gone", commitBody("x")))
	f.sockets.opts.Commits = failingStarter{}
	add("internal", http.StatusInternalServerError, f.post("gone", commitBody("x")))

	kinds := map[string]bool{}
	for i, r := range replies {
		var v any
		if err := json.Unmarshal(r.body, &v); err != nil {
			t.Fatalf("answer %d %q: %v", i, r.body, err)
		}
		valueFree(t, string(r.body), v)
		if strings.Contains(string(r.body), "example") || strings.Contains(string(r.body), f.folder) {
			t.Errorf("answer %q quotes a value", r.body)
		}
		var res Result
		_ = json.Unmarshal(r.body, &res)
		if res.Error != nil {
			kinds[res.Error.Kind] = true
		}
	}
	for _, want := range []string{commits.KindCommitFailed, commits.KindBackupFailed} {
		if !kinds[want] {
			t.Errorf("no answer had kind %s: %v", want, kinds)
		}
	}
}

func TestAStaleSocketFileIsReplacedWithoutFollowingALink(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.folder, "alpha")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, devcontainer.WorkspaceSocketName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "bin", ClientName)); err != nil {
		t.Fatal(err)
	}
	f.workspace("alpha")
	if data, err := os.ReadFile(outside); err != nil || string(data) != "kept" {
		t.Fatalf("the link's target changed: %q %v", data, err)
	}
	if r := f.post("alpha", commitBody("x")); r.status != http.StatusOK {
		t.Fatalf("status %d %s", r.status, r.body)
	}
}

func TestAWorkspaceFolderThatIsNotOursIsRefused(t *testing.T) {
	f := newFixture(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(f.folder, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.folder, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"linked", "file"} {
		if _, err := f.sockets.Ensure(name); !errors.Is(err, errNotOurs) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("written through the link: %v", entries)
	}
	for _, name := range []string{"", "..", "a/b"} {
		if _, err := f.sockets.Ensure(name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestEnsureKeepsALiveListenerAndReplacesAMissingOne(t *testing.T) {
	f := newFixture(t)
	f.workspace("alpha")
	before, _ := os.Lstat(f.socket("alpha"))
	if _, err := f.sockets.Ensure("alpha"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Lstat(f.socket("alpha")); !os.SameFile(before, after) {
		t.Fatal("a live listener was replaced")
	}
	if err := os.Remove(f.socket("alpha")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sockets.Ensure("alpha"); err != nil {
		t.Fatal(err)
	}
	if r := f.post("alpha", commitBody("x")); r.status != http.StatusOK {
		t.Fatalf("status %d %s", r.status, r.body)
	}
}

func TestOpenRefusesAFolderThatIsNotOurs(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{link, filepath.Join(dir, "missing")} {
		if _, err := Open(Options{Folder: folder, Client: os.Args[0]}); err == nil {
			t.Errorf("%s accepted", folder)
		}
	}
	if _, err := Open(Options{Folder: dir, Client: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing client accepted")
	}
}

// TestOpenRefusesALinkInThePathOrAnOverlapWithTheWorkspaceRoot checks the
// socket folder's path itself, not only its last part: a link above it
// makes the host mount another folder, and a workspace root reached
// through a link can still hold the socket folder or sit inside it.
func TestOpenRefusesALinkInThePathOrAnOverlapWithTheWorkspaceRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), sentinel)
	mkdir := func(rel string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(rel, target string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	linked := mkdir("real")
	mkdir("real/sockets")
	alias := link("alias", linked)
	// The workspace root is reached through a link; the socket folder is
	// inside the folder it names, in a workspace.
	rootInside := mkdir("root-a")
	mkdir("root-a/alpha/sockets")
	rootInsideLink := link("root-a-link", rootInside)
	// The socket folder holds the folder a linked workspace root names.
	mkdir("outer/root-b")
	rootContainedLink := link("root-b-link", filepath.Join(dir, "outer", "root-b"))
	client := filepath.Join(dir, ClientName)
	if err := os.WriteFile(client, []byte("sample client\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for label, c := range map[string]struct{ folder, root, want string }{
		"link above the folder":     {filepath.Join(alias, "sockets"), "", "workspace-socket.folder must have no symbolic link in its path"},
		"inside the resolved root":  {filepath.Join(rootInside, "alpha", "sockets"), rootInsideLink, "workspace-socket.folder must be neither inside the workspace root nor contain it"},
		"holds the resolved root":   {filepath.Join(dir, "outer"), rootContainedLink, "workspace-socket.folder must be neither inside the workspace root nor contain it"},
		"is the resolved root":      {rootInside, rootInsideLink, "workspace-socket.folder must be neither inside the workspace root nor contain it"},
		"apart from the root, kept": {filepath.Join(linked, "sockets"), rootInsideLink, ""},
	} {
		t.Run(label, func(t *testing.T) {
			s, err := Open(Options{Folder: c.folder, WorkspaceRoot: c.root, Client: client})
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), sentinel) {
				t.Fatalf("%v, want %q without the path", err, c.want)
			}
		})
	}
}
