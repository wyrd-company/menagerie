package backup

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// BranchCommit finds the workspace's branch, and nothing in an empty
// repository or one without that branch.
func TestBranchCommitReadsTheAdvertisedBranch(t *testing.T) {
	dir := t.TempDir()
	bare(t, dir, "backups.git")
	remote := &Remote{URL: filepath.Join(dir, "backups.git")}
	ctx := context.Background()
	if _, found, err := remote.BranchCommit(ctx, "alpha"); err != nil || found {
		t.Fatalf("empty repository: %v %v", found, err)
	}
	folder := newWorkspace(t, "alpha")
	root, err := CommitRoot(folder, "alpha", generate(t, folder), "Create workspace alpha", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Push(ctx, folder, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	if got, found, err := remote.BranchCommit(ctx, "alpha"); err != nil || !found || got != root.Hash {
		t.Fatalf("%v %v %v", got, found, err)
	}
	if _, found, err := remote.BranchCommit(ctx, "beta"); err != nil || found {
		t.Fatalf("another branch: %v %v", found, err)
	}
	absent := &Remote{URL: filepath.Join(dir, "absent.git")}
	if _, _, err := absent.BranchCommit(ctx, "alpha"); err == nil {
		t.Fatal("a missing repository listed")
	}
}

// The branch's commit and its ancestors are in its history; a later
// commit, another workspace's and an unknown one are not.
func TestInHistoryFollowsTheWorkspaceBranch(t *testing.T) {
	folder := newWorkspace(t, "alpha")
	root, err := CommitRoot(folder, "alpha", generate(t, folder), "Create workspace alpha", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	write(t, folder, "later.txt", "later\n")
	later, err := CommitAll(folder, "alpha", "Later", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	other := newWorkspace(t, "beta")
	foreign, err := CommitRoot(other, "beta", generate(t, other), "Create workspace beta", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	for hash, want := range map[plumbing.Hash]bool{
		later.Hash:   true,
		root.Hash:    true,
		foreign.Hash: false,
		plumbing.NewHash("1111111111111111111111111111111111111111"): false,
	} {
		if got, err := InHistory(folder, "alpha", hash); err != nil || got != want {
			t.Errorf("%s: %v %v", hash, got, err)
		}
	}
}

// Extend moves a branch only forward: it creates a missing branch, moves
// one that is in the local history, and refuses one another writer moved
// to a history the workspace does not have, leaving it as it was.
func TestExtendNeverOverwritesAnotherHistory(t *testing.T) {
	dir := t.TempDir()
	bare(t, dir, "backups.git")
	remote := &Remote{URL: filepath.Join(dir, "backups.git")}
	ctx := context.Background()
	folder := newWorkspace(t, "alpha")
	root, err := CommitRoot(folder, "alpha", generate(t, folder), "Create workspace alpha", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if pushed, err := remote.Extend(ctx, folder, "alpha", nil); err != nil || !pushed {
		t.Fatalf("create: %v %v", pushed, err)
	}
	write(t, folder, "later.txt", "later\n")
	later, err := CommitAll(folder, "alpha", "Later", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if pushed, err := remote.Extend(ctx, folder, "alpha", nil); err != nil || !pushed {
		t.Fatalf("forward: %v %v", pushed, err)
	}
	if got, _, _ := remote.BranchCommit(ctx, "alpha"); got != later.Hash || got == root.Hash {
		t.Fatalf("at %s", got)
	}
	other := newWorkspace(t, "alpha")
	foreign, err := CommitRoot(other, "alpha", generate(t, other), "another alpha", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Push(ctx, other, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Extend(ctx, folder, "alpha", nil); err == nil {
		t.Fatal("Extend overwrote another history")
	}
	if got, _, _ := remote.BranchCommit(ctx, "alpha"); got != foreign.Hash {
		t.Fatalf("the other history's branch moved to %s", got)
	}
}
