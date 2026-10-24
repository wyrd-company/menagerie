package backup

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

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
