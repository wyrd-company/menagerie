package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newWorkspace returns an initialized workspace folder named name.
func newWorkspace(t *testing.T, name string) string {
	t.Helper()
	folder := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Init(folder, name); err != nil {
		t.Fatal(err)
	}
	return folder
}

// generate writes what a template generation leaves: generated files, the
// applied record, a hook's output, an ignored file and a nested repository.
func generate(t *testing.T, folder string) Output {
	t.Helper()
	write(t, folder, ".gitignore", "/cache/\n*.local\n")
	write(t, folder, "result.txt", "label: first\n")
	write(t, folder, "sub/kept.txt", "kept\n")
	write(t, folder, "settings.local", "generated but ignored\n")
	write(t, folder, ".toha.json", `{"applications":[]}`+"\n")
	write(t, folder, "hook-marker.txt", "ran\n")
	write(t, folder, "cache/blob", "ignored hook output\n")
	write(t, folder, "clone/README", "nested repository file\n")
	if _, err := git.PlainInit(filepath.Join(folder, "clone"), false); err != nil {
		t.Fatal(err)
	}
	return Output{Reported: []string{".gitignore", "result.txt", "sub/kept.txt", "settings.local"}, Record: ".toha.json"}
}

// bare creates an empty bare repository under the fixture.
func bare(t *testing.T, dir, name string) *git.Repository {
	t.Helper()
	repo, err := git.PlainInit(filepath.Join(dir, name), true)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func tree(t *testing.T, repo *git.Repository, hash plumbing.Hash) []string {
	t.Helper()
	c, err := repo.CommitObject(hash)
	if err != nil {
		t.Fatal(err)
	}
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	_ = files.ForEach(func(f *object.File) error { names = append(names, f.Name); return nil })
	sort.Strings(names)
	return names
}

// history returns the first-parent chain from the tip of branch in repo.
func history(t *testing.T, repo *git.Repository, branch string) []*object.Commit {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), false)
	if err != nil {
		t.Fatalf("branch %s: %v", branch, err)
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
		if c.NumParents() > 1 {
			t.Fatalf("commit %s has %d parents", c.Hash, c.NumParents())
		}
		hash = c.ParentHashes[0]
	}
}

func TestBranchAcceptsOnlyLegalSingleComponentNames(t *testing.T) {
	for _, ok := range []string{"alpha", "team-one", "a.b", "x_1", "Upper"} {
		if _, err := Branch(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".hidden", "a..b", "x.lock", "with space", "a/b", "back\\slash", "tilde~", "colon:", "q?", "star*", "br[", "at@{", "-dash"} {
		if _, err := Branch(bad); !errors.Is(err, ErrInvalidBranch) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInitCreatesAnUnbornWorkspaceBranchInAnEmptyFolder(t *testing.T) {
	folder := newWorkspace(t, "alpha")
	entries, _ := os.ReadDir(folder)
	if len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("folder holds %v", entries)
	}
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Storer.Reference(plumbing.HEAD)
	if head.Target() != "refs/heads/alpha" {
		t.Fatalf("HEAD %s", head)
	}
	if _, err := repo.Head(); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("branch is born: %v", err)
	}
	if err := Init(folder, "alpha"); err == nil {
		t.Fatal("Init accepted a non-empty folder")
	}
	other := filepath.Join(t.TempDir(), "x")
	_ = os.Mkdir(other, 0o755)
	if err := Init(other, "bad..name"); !errors.Is(err, ErrInvalidBranch) {
		t.Fatalf("illegal branch accepted: %v", err)
	}
}

func TestBackupsOverSSHKeepAnIndependentRootAndHistory(t *testing.T) {
	f := backuptest.New(t)
	remoteRepo := bare(t, f.Dir, "backups.git")
	remote := &Remote{URL: f.URL("backups.git"), Auth: AgentAuth(f.AgentPath, f.KnownHosts)}
	ctx := context.Background()

	folder := newWorkspace(t, "alpha")
	out := generate(t, folder)
	var log bytes.Buffer
	root, err := CommitRoot(folder, "alpha", out, "Create workspace alpha", &log)
	if err != nil {
		t.Fatal(err)
	}
	wantStaged := []string{".gitignore", ".toha.json", "hook-marker.txt", "result.txt", "sub/kept.txt"}
	if !reflect.DeepEqual(root.Staged, wantStaged) || !root.Changed || len(root.Preserved) != 0 {
		t.Fatalf("root %+v", root)
	}
	for _, line := range []string{"staged hook output: hook-marker.txt", "not staged, ignored: settings.local"} {
		if !strings.Contains(log.String(), line) {
			t.Errorf("log lacks %q:\n%s", line, log.String())
		}
	}
	if strings.Contains(log.String(), "clone/") {
		t.Errorf("log mentions the nested repository:\n%s", log.String())
	}
	var progress bytes.Buffer
	pushed, err := remote.Push(ctx, folder, "alpha", &progress)
	if err != nil || !pushed {
		t.Fatalf("push: %v %v", pushed, err)
	}
	if got := f.Commands(); len(got) != 1 || got[0] != "git-receive-pack '"+filepath.Join(f.Dir, "backups.git")+"'" {
		t.Fatalf("remote commands %q", got)
	}
	commits := history(t, remoteRepo, "alpha")
	if len(commits) != 1 || commits[0].Hash != root.Hash {
		t.Fatalf("remote history %v", commits)
	}
	if got := tree(t, remoteRepo, root.Hash); !reflect.DeepEqual(got, wantStaged) {
		t.Fatalf("remote tree %v", got)
	}
	if commits[0].Author.Name != "Menagerie" {
		t.Fatalf("author %v", commits[0].Author)
	}

	// Pre-upgrade backup: a tracked edit and deletion; an untracked file stays out.
	write(t, folder, "result.txt", "label: operator\n")
	if err := os.Remove(filepath.Join(folder, "sub/kept.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, folder, "notes.txt", "operator notes\n")
	tracked, err := CommitTracked(folder, "alpha", "Back up local changes before upgrade")
	if err != nil || !tracked.Changed {
		t.Fatalf("tracked %+v %v", tracked, err)
	}
	if pushed, err := remote.Push(ctx, folder, "alpha", nil); err != nil || !pushed {
		t.Fatalf("push: %v %v", pushed, err)
	}
	if got := tree(t, remoteRepo, tracked.Hash); !reflect.DeepEqual(got, []string{".gitignore", ".toha.json", "hook-marker.txt", "result.txt"}) {
		t.Fatalf("tracked tree %v", got)
	}

	// Upgrade output: an added file, a deletion and the record. The operator's
	// earlier untracked file is not part of it and is removed first, as
	// Toha's clean-workspace check requires.
	_ = os.Remove(filepath.Join(folder, "notes.txt"))
	write(t, folder, "added.txt", "new in v2\n")
	write(t, folder, ".toha.json", `{"applications":[{"v":2}]}`+"\n")
	if err := os.Remove(filepath.Join(folder, "hook-marker.txt")); err != nil {
		t.Fatal(err)
	}
	upgraded, err := CommitUpgrade(folder, "alpha", Output{Reported: []string{"added.txt", "hook-marker.txt", "never-tracked.txt"}, Record: ".toha.json"}, "Upgrade workspace alpha", &log)
	if err != nil || !upgraded.Changed {
		t.Fatalf("upgrade %+v %v", upgraded, err)
	}
	if pushed, err := remote.Push(ctx, folder, "alpha", nil); err != nil || !pushed {
		t.Fatalf("push: %v %v", pushed, err)
	}
	commits = history(t, remoteRepo, "alpha")
	if len(commits) != 3 || commits[0].Hash != upgraded.Hash || commits[1].Hash != tracked.Hash || commits[2].Hash != root.Hash {
		t.Fatalf("remote history %v", commits)
	}
	if got := tree(t, remoteRepo, upgraded.Hash); !reflect.DeepEqual(got, []string{".gitignore", ".toha.json", "added.txt", "result.txt"}) {
		t.Fatalf("upgraded tree %v", got)
	}

	// Nothing changed: no commit, and the push reports the branch current.
	again, err := CommitTracked(folder, "alpha", "Back up local changes before upgrade")
	if err != nil || again.Changed || again.Hash != upgraded.Hash {
		t.Fatalf("empty commit %+v %v", again, err)
	}
	if pushed, err := remote.Push(ctx, folder, "alpha", nil); err != nil || pushed {
		t.Fatalf("up-to-date push: %v %v", pushed, err)
	}
}

func TestPushReplacesTheRemoteBranchAndFetchesNothing(t *testing.T) {
	f := backuptest.New(t)
	remoteRepo := bare(t, f.Dir, "backups.git")
	ctx := context.Background()

	// Another history already on the remote branch, plus an unrelated branch.
	elsewhere := newWorkspace(t, "alpha")
	write(t, elsewhere, "elsewhere.txt", "pushed from elsewhere\n")
	foreign, err := CommitRoot(elsewhere, "alpha", Output{Record: "elsewhere.txt"}, "elsewhere", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	local := &Remote{URL: filepath.Join(f.Dir, "backups.git")}
	if _, err := local.Push(ctx, elsewhere, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	other := newWorkspace(t, "beta")
	write(t, other, "beta.txt", "beta\n")
	betaRoot, _ := CommitRoot(other, "beta", Output{Record: "beta.txt"}, "beta", &bytes.Buffer{})
	if _, err := local.Push(ctx, other, "beta", nil); err != nil {
		t.Fatal(err)
	}

	folder := newWorkspace(t, "alpha")
	out := generate(t, folder)
	root, err := CommitRoot(folder, "alpha", out, "Create workspace alpha", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	remote := &Remote{URL: f.URL("backups.git"), Auth: AgentAuth(f.AgentPath, f.KnownHosts)}
	if pushed, err := remote.Push(ctx, folder, "alpha", nil); err != nil || !pushed {
		t.Fatalf("push: %v %v", pushed, err)
	}
	commits := history(t, remoteRepo, "alpha")
	if len(commits) != 1 || commits[0].Hash != root.Hash {
		t.Fatalf("remote branch was not replaced by the independent root: %v", commits)
	}
	if beta := history(t, remoteRepo, "beta"); beta[0].Hash != betaRoot.Hash {
		t.Fatal("another workspace's branch changed")
	}
	repo, _ := git.PlainOpen(folder)
	for _, hash := range []plumbing.Hash{foreign.Hash, betaRoot.Hash} {
		if _, err := repo.CommitObject(hash); err == nil {
			t.Fatalf("push fetched remote commit %s", hash)
		}
	}
	refs, _ := repo.References()
	_ = refs.ForEach(func(r *plumbing.Reference) error {
		if r.Name().IsRemote() {
			t.Errorf("push left remote-tracking ref %s", r.Name())
		}
		return nil
	})
	cfg, _ := repo.Config()
	if len(cfg.Remotes) != 0 {
		t.Fatalf("push wrote remotes %v", cfg.Remotes)
	}
}

func TestPushVerifiesTheHostKeyAndUsesOnlyTheAgent(t *testing.T) {
	f := backuptest.New(t)
	remoteRepo := bare(t, f.Dir, "backups.git")
	folder := newWorkspace(t, "alpha")
	if _, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	other := backuptest.New(t)
	wrongHosts := filepath.Join(t.TempDir(), "known_hosts")
	f.WriteKnownHosts(t, wrongHosts, other.HostKey)
	emptyAgent := f.ServeAgent(t, agent.NewKeyring())
	cases := []struct {
		name string
		auth Auth
		want string
	}{
		{"unknown host key", AgentAuth(f.AgentPath, wrongHosts), "key mismatch"},
		{"agent without the key", AgentAuth(emptyAgent, f.KnownHosts), "unable to authenticate"},
		{"no agent", AgentAuth(filepath.Join(t.TempDir(), "absent.sock"), f.KnownHosts), "ssh agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Remote{URL: f.URL("backups.git"), Auth: tc.auth}).Push(ctx, folder, "alpha", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if _, err := remoteRepo.Reference("refs/heads/alpha", false); err == nil {
				t.Fatal("refused push wrote the remote branch")
			}
		})
	}
}

func TestAgentAuthUsesTheDefaultKnownHostsWhenNoneAreGiven(t *testing.T) {
	f := backuptest.New(t)
	t.Setenv("SSH_KNOWN_HOSTS", f.KnownHosts)
	bare(t, f.Dir, "backups.git")
	folder := newWorkspace(t, "alpha")
	if _, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Remote{URL: f.URL("backups.git"), Auth: AgentAuth(f.AgentPath)}).Push(context.Background(), folder, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	other := backuptest.New(t)
	t.Setenv("SSH_KNOWN_HOSTS", filepath.Join(t.TempDir(), "absent"))
	_, err := (&Remote{URL: other.URL("x.git"), Auth: AgentAuth(other.AgentPath)}).Push(context.Background(), folder, "alpha", nil)
	if err == nil {
		t.Fatal("push trusted a host with no known key")
	}
}

func hookCommit(t *testing.T, folder, file string) plumbing.Hash {
	t.Helper()
	repo, _ := git.PlainOpen(folder)
	w, _ := repo.Worktree()
	write(t, folder, file, "from a hook\n")
	if _, err := w.Add(file); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "hook", Email: "hook@example.invalid", When: time.Now()}
	h, err := w.Commit("hook commit", &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestATemplatesOwnGitHistoryIsKeptBesideTheIndependentRoot(t *testing.T) {
	t.Run("commit on the workspace branch", func(t *testing.T) {
		folder := newWorkspace(t, "alpha")
		hook := hookCommit(t, folder, "hooked.txt")
		var log bytes.Buffer
		root, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &log)
		if err != nil {
			t.Fatal(err)
		}
		repo, _ := git.PlainOpen(folder)
		c, _ := repo.CommitObject(root.Hash)
		if c.NumParents() != 0 {
			t.Fatal("root has a parent")
		}
		kept, err := repo.Reference(PreservedBranch, false)
		if err != nil || kept.Hash() != hook || !reflect.DeepEqual(root.Preserved, []plumbing.ReferenceName{PreservedBranch}) {
			t.Fatalf("kept %v %v %v", kept, err, root.Preserved)
		}
		if got := tree(t, repo, root.Hash); !strings.Contains(strings.Join(got, ","), "hooked.txt") {
			t.Fatalf("root tree lost the hook's tracked file: %v", got)
		}
		if !strings.Contains(log.String(), "kept the template's Git history at refs/menagerie/template-history") {
			t.Fatalf("log %s", log.String())
		}
	})
	t.Run("detached HEAD", func(t *testing.T) {
		folder := newWorkspace(t, "alpha")
		hook := hookCommit(t, folder, "hooked.txt")
		repo, _ := git.PlainOpen(folder)
		_ = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, hook))
		_ = repo.Storer.RemoveReference("refs/heads/alpha")
		root, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if kept, err := repo.Reference(PreservedHead, false); err != nil || kept.Hash() != hook {
			t.Fatalf("kept %v %v", kept, err)
		}
		head, _ := repo.Storer.Reference(plumbing.HEAD)
		if head.Target() != "refs/heads/alpha" {
			t.Fatalf("HEAD %v", head)
		}
		if c, _ := repo.CommitObject(root.Hash); c.NumParents() != 0 {
			t.Fatal("root has a parent")
		}
	})
	t.Run("another branch", func(t *testing.T) {
		folder := newWorkspace(t, "alpha")
		repo, _ := git.PlainOpen(folder)
		_ = repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main"))
		hook := hookCommit(t, folder, "hooked.txt")
		root, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		if main, err := repo.Reference("refs/heads/main", false); err != nil || main.Hash() != hook {
			t.Fatalf("main %v %v", main, err)
		}
		if len(root.Preserved) != 0 {
			t.Fatalf("preserved %v", root.Preserved)
		}
		if c, _ := repo.CommitObject(root.Hash); c.NumParents() != 0 {
			t.Fatal("root has a parent")
		}
	})
}

func TestLaterBackupsNeedTheWorkspaceBranch(t *testing.T) {
	folder := newWorkspace(t, "alpha")
	if _, err := CommitTracked(folder, "alpha", "x"); err == nil || !strings.Contains(err.Error(), "has no commit") {
		t.Fatalf("unborn branch: %v", err)
	}
	if _, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	repo, _ := git.PlainOpen(folder)
	_ = repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main"))
	if _, err := CommitTracked(folder, "alpha", "x"); err == nil || !strings.Contains(err.Error(), "not on the workspace branch") {
		t.Fatalf("other branch: %v", err)
	}
	if _, err := CommitUpgrade(folder, "alpha", Output{Record: ".toha.json"}, "x", &bytes.Buffer{}); err == nil {
		t.Fatal("upgrade commit off the workspace branch")
	}
	if _, err := CommitTracked(t.TempDir(), "alpha", "x"); err == nil {
		t.Fatal("folder without a repository accepted")
	}
}

func TestOutputPathsMustStayInTheWorkspace(t *testing.T) {
	folder := newWorkspace(t, "alpha")
	write(t, folder, ".toha.json", "{}")
	for _, out := range []Output{
		{Record: "../escape"},
		{Record: ".toha.json", Reported: []string{"/abs"}},
		{Record: ".git/config"},
		{Record: ""},
	} {
		if _, err := CommitRoot(folder, "alpha", out, "x", &bytes.Buffer{}); err == nil {
			t.Errorf("%+v accepted", out)
		}
	}
}
