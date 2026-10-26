package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// contribute clones branch from the workspace repository, writes files,
// commits and pushes the commit to target, as a pull request's author
// would. It returns the commit.
func (w *world) contribute(t *testing.T, branch, target string, files map[string]string) plumbing.Hash {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: w.remote, ReferenceName: plumbing.NewBranchReferenceName(branch), SingleBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	for rel, body := range files {
		writeFile(t, dir, rel, body)
		if _, err := wt.Add(rel); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "Contributor", Email: "contributor@example.invalid", When: time.Now()}
	hash, err := wt.Commit("Change the workspace", &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	spec := config.RefSpec(fmt.Sprintf("+%s:%s", plumbing.NewBranchReferenceName(branch), plumbing.NewBranchReferenceName(target)))
	if err := repo.Push(&git.PushOptions{RefSpecs: []config.RefSpec{spec}}); err != nil {
		t.Fatal(err)
	}
	return hash
}

// applyBranch starts an apply-branch job and waits for its end.
func (w *world) applyBranch(t *testing.T, name, branch string) (*jobs.Job, jobs.Snapshot) {
	t.Helper()
	status, body := w.do(t, http.MethodPost, "/api/jobs", fmt.Sprintf(`{"operation":"apply-branch","workspace":%q,"branch":%q}`, name, branch))
	if status != http.StatusAccepted {
		t.Fatalf("start apply-branch: %d %s", status, body)
	}
	var snap jobs.Snapshot
	_ = json.Unmarshal([]byte(body), &snap)
	job, ok := w.manager.Get(snap.JobID)
	if !ok {
		t.Fatalf("job %s not found", snap.JobID)
	}
	return job, waitEnded(t, job)
}

// phases returns the job's phases in the order they started.
func phases(job *jobs.Job) []string {
	var out []string
	for _, d := range events(job, jobs.EventState) {
		var s struct{ Phase string }
		_ = json.Unmarshal([]byte(d), &s)
		if s.Phase != "" && (len(out) == 0 || out[len(out)-1] != s.Phase) {
			out = append(out, s.Phase)
		}
	}
	return out
}

func (w *world) fetchedRefGone(t *testing.T, name string) {
	t.Helper()
	repo, err := git.PlainOpen(filepath.Join(w.root, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Reference(backup.FetchedRef, false); err == nil {
		t.Fatal("the fetched ref outlived the job")
	}
}

func TestApplyBranchBringsAPushedCommitIntoTheWorkspaceAndTheNextBackupKeepsIt(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	pushed := w.contribute(t, "alpha", "alpha", map[string]string{".devcontainer/devcontainer.json": `{"image":"alpine"}` + "\n"})

	job, snap := w.applyBranch(t, "alpha", "alpha")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	// The workspace's own branch is pushed only after the apply.
	if want := []string{"fetch", "backup-local-changes", "apply", "backup", "rebuild"}; !reflect.DeepEqual(phases(job), want) {
		t.Fatalf("phases %v, want %v", phases(job), want)
	}
	if !strings.Contains(logText(job), "own branch alpha is not pushed before the apply") {
		t.Fatalf("log:\n%s", logText(job))
	}
	if body, _ := os.ReadFile(filepath.Join(folder, ".devcontainer", "devcontainer.json")); string(body) != `{"image":"alpine"}`+"\n" {
		t.Fatalf("the folder has %q", body)
	}
	if got := w.rt.seen(); !reflect.DeepEqual(got, []string{"up alpha", "rebuild alpha"}) {
		t.Fatalf("runtime %v", got)
	}
	if history := w.remoteHistory(t, "alpha"); history[0].Hash != pushed {
		t.Fatalf("the remote branch is at %s, want the pushed commit %s", history[0].Hash, pushed)
	}
	w.fetchedRefGone(t, "alpha")

	// The next backup extends the history that holds the pushed commit.
	writeFile(t, folder, "later.txt", "later\n")
	if c, err := backup.CommitAll(folder, "alpha", "later", io.Discard); err != nil || !c.Changed {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := w.flows.opts.Remote.Push(context.Background(), folder, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	history := w.remoteHistory(t, "alpha")
	if len(history) != 3 || history[1].Hash != pushed || fileAt(t, history[0], ".devcontainer/devcontainer.json") == "" {
		t.Fatalf("the next backup lost the applied commit: %v", history)
	}
}

func TestApplyBranchBacksUpEveryLocalChangeAndMerges(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	pushed := w.contribute(t, "alpha", "review/devcontainer", map[string]string{"reviewed.txt": "reviewed\n"})
	writeFile(t, folder, "notes.txt", "untracked operator notes\n")

	job, snap := w.applyBranch(t, "alpha", "review/devcontainer")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if want := []string{"fetch", "backup-local-changes", "backup", "apply", "backup", "rebuild"}; !reflect.DeepEqual(phases(job), want) {
		t.Fatalf("phases %v, want %v", phases(job), want)
	}
	history := w.remoteHistory(t, "alpha")
	merge, local := history[0], history[1]
	if local.Message != "Back up local changes before applying review/devcontainer" || fileAt(t, local, "notes.txt") == "" {
		t.Fatalf("local backup %q", local.Message)
	}
	if merge.Message != "Apply review/devcontainer to workspace alpha" || !reflect.DeepEqual(merge.ParentHashes, []plumbing.Hash{local.Hash, pushed}) {
		t.Fatalf("merge %q %v", merge.Message, merge.ParentHashes)
	}
	if fileAt(t, merge, "reviewed.txt") != "reviewed\n" || fileAt(t, merge, "notes.txt") == "" {
		t.Fatal("the merge lost a side")
	}
	if body, _ := os.ReadFile(filepath.Join(folder, "reviewed.txt")); string(body) != "reviewed\n" {
		t.Fatal("the folder lacks the reviewed file")
	}
	w.fetchedRefGone(t, "alpha")
}

func TestApplyingAnAppliedBranchChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	job, snap := w.applyBranch(t, "alpha", "alpha")
	if snap.State != jobs.Succeeded || !strings.Contains(logText(job), "is already applied") {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if want := []string{"fetch", "backup-local-changes", "apply"}; !reflect.DeepEqual(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	if got := w.rt.seen(); !reflect.DeepEqual(got, []string{"up alpha"}) {
		t.Fatalf("runtime %v", got)
	}
	w.fetchedRefGone(t, "alpha")
}

func TestARefusedApplyOfTheOwnBranchLeavesTheRemoteAsItWas(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	pushed := w.contribute(t, "alpha", "alpha", map[string]string{"local.txt": "first: reviewed\n"})
	writeFile(t, folder, "local.txt", "first: operator\n")

	job, snap := w.applyBranch(t, "alpha", "alpha")
	if snap.State != jobs.Failed || snap.Error.Kind != KindNotApplicable {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if !strings.Contains(snap.Error.Message, pushed.String()) {
		t.Fatalf("the refusal does not name the fetched commit: %s", snap.Error.Message)
	}
	if history := w.remoteHistory(t, "alpha"); history[0].Hash != pushed {
		t.Fatalf("the remote branch is at %s, want the pushed commit %s", history[0].Hash, pushed)
	}
	// The local change is committed, and waits for the next backup.
	repo, _ := git.PlainOpen(folder)
	head, _ := repo.Reference(plumbing.NewBranchReferenceName("alpha"), false)
	local, _ := repo.CommitObject(head.Hash())
	if local.Message != "Back up local changes before applying alpha" {
		t.Fatalf("local branch at %q", local.Message)
	}
	w.fetchedRefGone(t, "alpha")
}

func TestApplyBranchRequestsAreChecked(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	for body, want := range map[string]string{
		`{"operation":"apply-branch","workspace":"alpha"}`:                             "invalid-request",
		`{"operation":"apply-branch","workspace":"alpha","branch":"refs/heads/alpha"}`: "invalid-branch",
		`{"operation":"apply-branch","workspace":"alpha","branch":"a..b"}`:             "invalid-branch",
		`{"operation":"apply-branch","workspace":"absent","branch":"alpha"}`:           "workspace-not-found",
		`{"operation":"apply-branch","workspace":"Bad Name","branch":"alpha"}`:         "invalid-workspace-name",
	} {
		status, got := w.do(t, http.MethodPost, "/api/jobs", body)
		if status/100 != 4 || !strings.Contains(got, `"kind":"`+want+`"`) {
			t.Errorf("%s: %d %s, want %s", body, status, got, want)
		}
	}
	if len(w.manager.List()) != 1 {
		t.Fatal("a refused request started a job")
	}
}

func TestAMissingBranchChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	writeFile(t, folder, "notes.txt", "local\n")
	job, snap := w.applyBranch(t, "alpha", "absent")
	if snap.State != jobs.Failed || snap.Error.Kind != KindBranchNotFound {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if want := []string{"fetch"}; !reflect.DeepEqual(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	if len(w.remoteHistory(t, "alpha")) != 1 {
		t.Fatal("a missing branch committed or pushed")
	}
	pending, err := backup.Pending(folder, "alpha")
	if err != nil || !pending {
		t.Fatal("a missing branch committed the local change")
	}
	w.fetchedRefGone(t, "alpha")
}

func TestARefusedApplyListsThePathsAndKeepsTheLocalBackup(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	w.contribute(t, "alpha", "review/one", map[string]string{"local.txt": "first: reviewed\n"})
	writeFile(t, folder, "local.txt", "first: operator\n")

	job, snap := w.applyBranch(t, "alpha", "review/one")
	if snap.State != jobs.Failed || snap.Error.Kind != KindNotApplicable {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if !strings.Contains(logText(job), "not applicable: local.txt\n") || !strings.Contains(snap.Error.Message, "[paths: local.txt]") {
		t.Fatalf("%s\n%s", snap.Error.Message, logText(job))
	}
	if body, _ := os.ReadFile(filepath.Join(folder, "local.txt")); string(body) != "first: operator\n" {
		t.Fatal("a refused apply changed the folder")
	}
	if history := w.remoteHistory(t, "alpha"); len(history) != 2 || fileAt(t, history[0], "local.txt") != "first: operator\n" {
		t.Fatal("the local change was not backed up")
	}
	if got := w.rt.seen(); !reflect.DeepEqual(got, []string{"up alpha"}) {
		t.Fatalf("runtime %v", got)
	}
	w.fetchedRefGone(t, "alpha")
}

// countingAuth fails its failAt-th call, counted from 1, with an error
// that names the URL it was given.
type countingAuth struct {
	mu     sync.Mutex
	calls  int
	failAt int
}

func (a *countingAuth) auth(_ context.Context, url string) (transport.AuthMethod, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls == a.failAt {
		return nil, nil, fmt.Errorf("no credentials for %s", url)
	}
	return nil, func() {}, nil
}

func TestApplyBranchFailuresNeverNameTheWorkspaceRepository(t *testing.T) {
	const sentinel = "sentinel-workspace-repository"
	cases := map[string]struct {
		kind  string
		setup func(t *testing.T, w *world, auth *countingAuth) string
	}{
		"fetch credentials": {KindFetchFailed, func(t *testing.T, w *world, auth *countingAuth) string {
			auth.failAt = 1
			return "alpha"
		}},
		"unreachable repository": {KindFetchFailed, func(t *testing.T, w *world, auth *countingAuth) string {
			w.flows.opts.Remote.URL = filepath.Join(t.TempDir(), sentinel+"-absent.git")
			return "alpha"
		}},
		"missing branch": {KindBranchNotFound, func(t *testing.T, w *world, auth *countingAuth) string { return "absent" }},
		"backup before the apply": {KindBackupFailed, func(t *testing.T, w *world, auth *countingAuth) string {
			w.contribute(t, "alpha", "review/one", map[string]string{"new.txt": "new\n"})
			auth.failAt = 2
			return "review/one"
		}},
		"not applicable": {KindNotApplicable, func(t *testing.T, w *world, auth *countingAuth) string {
			w.contribute(t, "alpha", "review/one", map[string]string{"local.txt": "first: reviewed\n"})
			writeFile(t, filepath.Join(w.root, "alpha"), "local.txt", "first: operator\n")
			return "review/one"
		}},
		"write failure": {KindApplyFailed, func(t *testing.T, w *world, auth *countingAuth) string {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a read-only folder")
			}
			w.contribute(t, "alpha", "alpha", map[string]string{"locked/new.txt": "new\n"})
			locked := filepath.Join(w.root, "alpha", "locked")
			writeFile(t, locked, "keep.txt", "keep\n")
			if err := os.Chmod(locked, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			return "alpha"
		}},
		"backup after the apply": {KindBackupFailed, func(t *testing.T, w *world, auth *countingAuth) string {
			w.contribute(t, "alpha", "alpha", map[string]string{"new.txt": "new\n"})
			auth.failAt = 2
			return "alpha"
		}},
		"rebuild": {"operation-failed", func(t *testing.T, w *world, auth *countingAuth) string {
			w.contribute(t, "alpha", "alpha", map[string]string{"new.txt": "new\n"})
			w.rt.fail = errors.New("devcontainer failed")
			return "alpha"
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			// The workspace repository's address carries the sentinel.
			dir := filepath.Join(t.TempDir(), sentinel)
			if err := os.Rename(w.remote, dir); err != nil {
				t.Fatal(err)
			}
			w.remote = dir
			w.flows.opts.Remote.URL = dir
			w.createWorkspace(t, "alpha")
			auth := &countingAuth{}
			w.flows.opts.Remote.Auth = auth.auth
			branch := c.setup(t, w, auth)

			job, snap := w.applyBranch(t, "alpha", branch)
			if snap.State != jobs.Failed || snap.Error.Kind != c.kind {
				t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
			}
			// The write failure applies the workspace's own branch, which
			// is not pushed before the apply.
			for _, want := range []string{"files written before the failure remain", "committed but not backed up as"} {
				if c.kind == KindApplyFailed && !strings.Contains(snap.Error.Message, want) {
					t.Fatalf("an apply that failed while writing says %q", snap.Error.Message)
				}
			}
			all, _, _ := job.EventsAfter(0)
			for _, e := range all {
				if strings.Contains(string(e.Data), sentinel) {
					t.Fatalf("event %s names the workspace repository: %s", e.Type, e.Data)
				}
			}
			if strings.Contains(snap.Error.Message, sentinel) {
				t.Fatalf("the error names the workspace repository: %s", snap.Error.Message)
			}
			w.fetchedRefGone(t, "alpha")
		})
	}
}
