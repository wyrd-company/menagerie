package commits

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"gopkg.in/yaml.v3"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// fakeRemote records pushes and fails while fail is set.
type fakeRemote struct {
	mu     sync.Mutex
	pushes []string
	fail   bool
}

func (r *fakeRemote) Push(_ context.Context, _, name string, _ io.Writer) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pushes = append(r.pushes, name)
	if r.fail {
		return false, errors.New("remote unreachable")
	}
	return true, nil
}

func (r *fakeRemote) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pushes)
}

type fixture struct {
	root    *workspace.Root
	remote  *fakeRemote
	jobs    *jobs.Manager
	commits *Commits
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root: root, remote: &fakeRemote{}, jobs: jobs.NewManager(context.Background(), jobs.Options{})}
	f.commits = New(Options{Root: root, Remote: f.remote, Jobs: f.jobs})
	t.Cleanup(f.jobs.Wait)
	return f
}

// workspace creates a workspace whose branch has a root commit.
func (f *fixture) workspace(t *testing.T, name string) string {
	t.Helper()
	folder, err := f.root.FolderPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := backup.Init(folder, name); err != nil {
		t.Fatal(err)
	}
	write(t, folder, ".toha.json", "{}\n")
	write(t, folder, ".gitignore", "*.local\n")
	if _, err := backup.CommitRoot(folder, name, backup.Output{Record: ".toha.json", Reported: []string{".gitignore"}}, "root", io.Discard); err != nil {
		t.Fatal(err)
	}
	return folder
}

func write(t *testing.T, folder, rel, body string) {
	t.Helper()
	p := filepath.Join(folder, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wait blocks until job ends and returns its snapshot.
func wait(t *testing.T, job *jobs.Job) jobs.Snapshot {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		_, ended, changed := job.EventsAfter(1 << 40)
		if ended {
			return job.Snapshot()
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("job did not end")
		}
	}
}

func tip(t *testing.T, folder, name string) string {
	t.Helper()
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), false)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Hash().String()
}

func TestACommitJobCommitsThenBacksUp(t *testing.T) {
	f := newFixture(t)
	folder := f.workspace(t, "alpha")
	before := tip(t, folder, "alpha")
	write(t, folder, "notes/new.txt", "new\n")

	job, outcome, err := f.commits.Start("alpha", "docs: add notes")
	if err != nil {
		t.Fatal(err)
	}
	snap := wait(t, job)
	if snap.State != jobs.Succeeded || snap.Operation != api.OperationCommit {
		t.Fatalf("job %+v", snap)
	}
	commit, changed, pushed := outcome.Result()
	if !changed || !pushed || commit == before || commit != tip(t, folder, "alpha") {
		t.Fatalf("outcome %s %t %t, tip before %s", commit, changed, pushed, before)
	}
	if f.remote.count() != 1 {
		t.Fatalf("pushes %v", f.remote.pushes)
	}

	// Nothing changed: the branch is pushed again, so a failed backup is
	// retried.
	job, outcome, err = f.commits.Start("alpha", "docs: again")
	if err != nil {
		t.Fatal(err)
	}
	if snap := wait(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("job %+v", snap)
	}
	if again, changed, _ := outcome.Result(); changed || again != commit {
		t.Fatalf("second outcome %s %t", again, changed)
	}
	if f.remote.count() != 2 {
		t.Fatalf("an unchanged workspace was not pushed: %v", f.remote.pushes)
	}
}

func TestAFailedBackupKeepsTheCommit(t *testing.T) {
	f := newFixture(t)
	folder := f.workspace(t, "alpha")
	write(t, folder, "new.txt", "new\n")
	f.remote.fail = true
	job, outcome, err := f.commits.Start("alpha", "chore: x")
	if err != nil {
		t.Fatal(err)
	}
	snap := wait(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupFailed || snap.Phase != "backup" {
		t.Fatalf("job %+v", snap)
	}
	if commit, changed, pushed := outcome.Result(); commit != tip(t, folder, "alpha") || !changed || pushed {
		t.Fatalf("outcome %s %t %t", commit, changed, pushed)
	}
}

func TestAFailedCommitBacksNothingUp(t *testing.T) {
	f := newFixture(t)
	folder, _ := f.root.FolderPath("plain")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	job, outcome, err := f.commits.Start("plain", "chore: x")
	if err != nil {
		t.Fatal(err)
	}
	snap := wait(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != KindCommitFailed {
		t.Fatalf("job %+v", snap)
	}
	if commit, _, _ := outcome.Result(); commit != "" || f.remote.count() != 0 {
		t.Fatalf("commit %q pushes %v", commit, f.remote.pushes)
	}
}

func TestTheAPIOperationCommitsAnExistingWorkspace(t *testing.T) {
	f := newFixture(t)
	folder := f.workspace(t, "alpha")
	write(t, folder, "new.txt", "new\n")
	op := f.commits.Operation()
	for name, want := range map[string]string{"missing": "workspace-not-found", "..": "invalid-workspace-name"} {
		var re *api.RequestError
		if _, err := op(api.JobRequest{Operation: api.OperationCommit, Workspace: name}); !errors.As(err, &re) || re.Kind != want {
			t.Errorf("%s: %v, want %s", name, err, want)
		}
	}
	runner, err := op(api.JobRequest{Operation: api.OperationCommit, Workspace: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.jobs.Start("alpha", api.OperationCommit, runner)
	if err != nil {
		t.Fatal(err)
	}
	if snap := wait(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("job %+v", snap)
	}
	repo, _ := git.PlainOpen(folder)
	head, _ := repo.Head()
	c, _ := repo.CommitObject(head.Hash())
	if c.Message != APIMessage {
		t.Fatalf("message %q", c.Message)
	}
}

// hold starts a job on name that runs until the returned function is
// called.
func hold(t *testing.T, f *fixture, name string) func() {
	t.Helper()
	release := make(chan struct{})
	job, err := f.jobs.Start(name, api.OperationStart, func(context.Context, *jobs.Run) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() { close(release) })
		wait(t, job)
	}
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func TestASweepCommitsOnlyIdleWorkspacesWithChanges(t *testing.T) {
	f := newFixture(t)
	dirty := f.workspace(t, "dirty")
	f.workspace(t, "clean")
	ignored := f.workspace(t, "ignored")
	busy := f.workspace(t, "busy")
	write(t, dirty, "new.txt", "new\n")
	write(t, ignored, "only.local", "ignored\n")
	write(t, busy, "new.txt", "new\n")
	busyTip := tip(t, busy, "busy")
	release := hold(t, f, "busy")

	var l logs
	started := f.commits.Sweep(l.logf)
	if !slices.Equal(started, []string{"dirty"}) {
		t.Fatalf("started %v", started)
	}
	for _, j := range f.jobs.List() {
		if j.Operation == api.OperationCommit {
			wait(t, mustGet(t, f, j.JobID))
		}
	}
	release()

	// The busy workspace was skipped, not queued, and its check changed
	// nothing.
	for _, j := range f.jobs.List() {
		if j.Workspace == "busy" && j.Operation == api.OperationCommit {
			t.Fatalf("a commit was queued for the busy workspace: %+v", j)
		}
	}
	if tip(t, busy, "busy") != busyTip {
		t.Fatal("the busy workspace's branch moved")
	}
	repo, _ := git.PlainOpen(busy)
	w, _ := repo.Worktree()
	status, _ := w.Status()
	if s := status.File("new.txt"); s.Staging != git.Untracked {
		t.Fatalf("the check staged a file: %+v", s)
	}
	if len(l.lines) != 0 {
		t.Fatalf("logs %v", l.lines)
	}
}

func mustGet(t *testing.T, f *fixture, id string) *jobs.Job {
	t.Helper()
	job, ok := f.jobs.Get(id)
	if !ok {
		t.Fatalf("job %s missing", id)
	}
	return job
}

func TestASweepLogsAnUnreadableWorkspaceWithoutItsError(t *testing.T) {
	f := newFixture(t)
	folder, _ := f.root.FolderPath("plain")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	var l logs
	if started := f.commits.Sweep(l.logf); len(started) != 0 {
		t.Fatalf("started %v", started)
	}
	if len(l.lines) != 1 || l.lines[0] != "menagerie: automatic commit: workspace plain: its changes could not be read" {
		t.Fatalf("logs %q", l.lines)
	}
}

func TestScheduleSweepsOnEachTickOnly(t *testing.T) {
	f := newFixture(t)
	folder := f.workspace(t, "alpha")
	write(t, folder, "new.txt", "new\n")
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.commits.Schedule(ctx, ticks, (&logs{}).logf)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	if n := len(f.jobs.List()); n != 0 {
		t.Fatalf("%d jobs before the first tick", n)
	}
	ticks <- time.Now()
	ticks <- time.Now() // the second tick waits for the first sweep
	cancel()
	<-done
	list := f.jobs.List()
	if len(list) != 1 || list[0].Operation != api.OperationCommit {
		t.Fatalf("jobs %+v", list)
	}
	snap := wait(t, mustGet(t, f, list[0].JobID))
	if snap.State != jobs.Succeeded {
		t.Fatalf("job %+v", snap)
	}
	repo, _ := git.PlainOpen(folder)
	head, _ := repo.Head()
	c, _ := repo.CommitObject(head.Hash())
	if _, err := c.File("new.txt"); c.Message != AutomaticMessage || err != nil {
		t.Fatalf("commit %q: %v", c.Message, err)
	}
}

func TestTheCommitOperationAndItsKindsAreDocumented(t *testing.T) {
	data, err := os.ReadFile("../../../docs/specifications/assets/workspace-api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum     []string `yaml:"enum"`
					Examples []string `yaml:"examples"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if ops := doc.Components.Schemas["JobRequest"].Properties["operation"].Enum; !slices.Contains(ops, api.OperationCommit) {
		t.Errorf("operation %s undocumented: %v", api.OperationCommit, ops)
	}
	kinds := doc.Components.Schemas["JobError"].Properties["kind"].Examples
	for _, kind := range []string{KindCommitFailed, KindBackupFailed} {
		if !slices.Contains(kinds, kind) {
			t.Errorf("kind %s undocumented", kind)
		}
	}
}
