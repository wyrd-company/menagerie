package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// generated is a workspace folder whose generation wrote its applied record.
func generated(t *testing.T, name string) string {
	t.Helper()
	folder := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Init(folder, name); err != nil {
		t.Fatal(err)
	}
	if err := MarkCreate(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, RecordName), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return folder
}

func TestCreatePendingIsAGeneratedWorkspaceWithNoCommit(t *testing.T) {
	folder := generated(t, "sample")
	if pending, err := CreatePending(folder, "sample"); err != nil || !pending {
		t.Fatalf("after generation: %t %v", pending, err)
	}
	if _, err := CommitRoot(folder, "sample", Output{Record: RecordName}, "first", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// Its create stays unfinished until its first backup lands.
	if pending, err := CreatePending(folder, "sample"); err != nil || !pending {
		t.Fatalf("after its commit: %t %v", pending, err)
	}
	remote := bareRemote(t)
	if _, err := remote.Push(context.Background(), folder, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if pending, err := CreatePending(folder, "sample"); err != nil || pending {
		t.Fatalf("after its backup: %t %v", pending, err)
	}
}

// A push from any job keeps the earlier branch, and a later one fetches nothing.
func TestTheFirstPushOfACreateArchivesAndLaterOnesDoNot(t *testing.T) {
	remote := bareRemote(t)
	ctx := context.Background()
	first, firstHash := pushable(t, remote, "sample", "first")
	if _, err := remote.Push(ctx, first, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	second, _ := pushable(t, remote, "sample", "second")
	var console bytes.Buffer
	if _, err := remote.Push(ctx, second, "sample", &console); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(console.String(), "kept the earlier backup of branch sample as archive/sample-") {
		t.Errorf("console %s", console.String())
	}
	bare, err := git.PlainOpen(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	found := func() (n int) {
		refs, err := bare.Branches()
		if err != nil {
			t.Fatal(err)
		}
		_ = refs.ForEach(func(r *plumbing.Reference) error {
			if strings.HasPrefix(r.Name().Short(), ArchivePrefix) && r.Hash() == firstHash {
				n++
			}
			return nil
		})
		return n
	}
	if found() != 1 {
		t.Fatal("no archive")
	}
	// The branch can move on the remote; only a create's first push copies it.
	if err := os.WriteFile(filepath.Join(second, "file"), []byte("third"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitAll(second, "sample", "third", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Push(ctx, second, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	refs, err := bare.Branches()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	_ = refs.ForEach(func(*plumbing.Reference) error { count++; return nil })
	if count != 2 {
		t.Errorf("%d branches", count)
	}
}

func TestAPushThatFailsKeepsTheCreateUnfinished(t *testing.T) {
	folder := generated(t, "sample")
	if _, err := CommitRoot(folder, "sample", Output{Record: RecordName}, "first", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	remote := &Remote{URL: filepath.Join(t.TempDir(), "absent.git")}
	if _, err := remote.Push(context.Background(), folder, "sample", &bytes.Buffer{}); err == nil {
		t.Fatal("the push passed")
	}
	if pending, err := CreatePending(folder, "sample"); err != nil || !pending {
		t.Fatalf("%t %v", pending, err)
	}
}

func TestARetryBacksUpTheBranchAsItIs(t *testing.T) {
	folder := generated(t, "sample")
	if made, err := CreateCommit(folder, "sample"); err != nil || !made.IsZero() {
		t.Fatalf("no root: %s %v", made, err)
	}
	// A recorded root with no branch commits again.
	gone := plumbing.NewHash("0123456789012345678901234567890123456789")
	if err := RecordCreateCommit(folder, gone); err != nil {
		t.Fatal(err)
	}
	if made, err := CreateCommit(folder, "sample"); err != nil || !made.IsZero() {
		t.Fatalf("no branch: %s %v", made, err)
	}
	c, err := CommitRoot(folder, "sample", Output{Record: RecordName}, "first", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordCreateCommit(folder, c.Hash); err != nil {
		t.Fatal(err)
	}
	if err := MarkCreate(folder); err != nil {
		t.Fatal(err)
	}
	if made, err := CreateCommit(folder, "sample"); err != nil || made != c.Hash {
		t.Fatalf("root: %s %v", made, err)
	}
	// A job moved the branch: the retry reports what it pushes.
	if err := os.WriteFile(filepath.Join(folder, "later"), []byte("later"), 0o644); err != nil {
		t.Fatal(err)
	}
	later, err := CommitAll(folder, "sample", "later", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if made, err := CreateCommit(folder, "sample"); err != nil || made != later.Hash {
		t.Fatalf("moved: %s %v, want %s", made, err, later.Hash)
	}
}

// An apply-branch job holds FetchedRef across its first push.
func TestArchiveLeavesAnApplysFetchedRefAlone(t *testing.T) {
	remote := bareRemote(t)
	ctx := context.Background()
	first, _ := pushable(t, remote, "sample", "first")
	if _, err := remote.Push(ctx, first, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	second, _ := pushable(t, remote, "sample", "second")
	held, err := remote.Fetch(ctx, second, "sample", "sample", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Push(ctx, second, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(second)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(FetchedRef, false)
	if err != nil || ref.Hash() != held {
		t.Fatalf("fetched ref %v %v, want %s", ref, err, held)
	}
	if _, err := repo.Reference(archiveRef, false); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Errorf("the archive's ref stays: %v", err)
	}
}

func TestCreatePendingLeavesOtherFoldersAlone(t *testing.T) {
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, RecordName), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pending, err := CreatePending(plain, "sample"); err != nil || pending {
		t.Errorf("no repository: %t %v", pending, err)
	}
	folder := filepath.Join(t.TempDir(), "sample")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Init(folder, "sample"); err != nil {
		t.Fatal(err)
	}
	if pending, err := CreatePending(folder, "sample"); err != nil || pending {
		t.Errorf("no applied record: %t %v", pending, err)
	}
	other := generated(t, "sample")
	repo, err := git.PlainOpen(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/another")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(markerPath(other)); err != nil {
		t.Fatal(err)
	}
	if pending, err := CreatePending(other, "sample"); err != nil || pending {
		t.Errorf("HEAD on another branch: %t %v", pending, err)
	}
}

func TestCreatePendingFinishesADetachedHead(t *testing.T) {
	folder := generated(t, "sample")
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, plumbing.NewHash("0123456789012345678901234567890123456789"))); err != nil {
		t.Fatal(err)
	}
	if pending, err := CreatePending(folder, "sample"); err != nil || !pending {
		t.Errorf("%t %v", pending, err)
	}
}

func bareRemote(t *testing.T) *Remote {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backups.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	return &Remote{URL: dir}
}

// pushable commits a new workspace of the name, as a create does before its push.
func pushable(t *testing.T, remote *Remote, name, content string) (string, plumbing.Hash) {
	t.Helper()
	folder := generated(t, name)
	if err := os.WriteFile(filepath.Join(folder, "file"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := CommitRoot(folder, name, Output{Record: RecordName}, content, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return folder, c.Hash
}

func TestArchiveKeepsADeletedWorkspacesBackup(t *testing.T) {
	remote := bareRemote(t)
	ctx := context.Background()
	first, firstHash := pushable(t, remote, "sample", "first")
	if _, err := remote.Push(ctx, first, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	second, secondHash := pushable(t, remote, "sample", "second")
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("elsewhere", 3600))
	archived, err := remote.Archive(ctx, second, "sample", now, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if archived != "archive/sample-20300102T020405Z" {
		t.Fatalf("archived %q", archived)
	}
	if _, err := remote.Push(ctx, second, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	bare, err := git.PlainOpen(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := bare.Reference(plumbing.NewBranchReferenceName(archived), false)
	if err != nil || ref.Hash() != firstHash {
		t.Fatalf("archive %v %v, want %s", ref, err, firstHash)
	}
	ref, err = bare.Reference("refs/heads/sample", false)
	if err != nil || ref.Hash() != secondHash {
		t.Fatalf("branch %v %v, want %s", ref, err, secondHash)
	}
	repo, err := git.PlainOpen(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Reference(archiveRef, false); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Errorf("the archive's ref stays: %v", err)
	}
}

func TestArchiveWithNothingToKeepWritesNothing(t *testing.T) {
	remote := bareRemote(t)
	ctx := context.Background()
	folder, _ := pushable(t, remote, "sample", "first")
	if archived, err := remote.Archive(ctx, folder, "sample", time.Now(), &bytes.Buffer{}); err != nil || archived != "" {
		t.Fatalf("no branch: %q %v", archived, err)
	}
	if _, err := remote.Push(ctx, folder, "sample", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if archived, err := remote.Archive(ctx, folder, "sample", time.Now(), &bytes.Buffer{}); err != nil || archived != "" {
		t.Fatalf("its own commit: %q %v", archived, err)
	}
	bare, err := git.PlainOpen(remote.URL)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := bare.Branches()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	_ = refs.ForEach(func(*plumbing.Reference) error { count++; return nil })
	if count != 1 {
		t.Errorf("%d branches", count)
	}
}
