package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// file is a file a remote commit writes. A nil *file removes the path.
type file struct {
	body string
	mode filemode.FileMode
}

func regular(body string) *file    { return &file{body, filemode.Regular} }
func executable(body string) *file { return &file{body, filemode.Executable} }
func symlink(target string) *file  { return &file{target, filemode.Symlink} }

// applyFixture is the workspace alpha, committed and backed up to a bare
// workspace repository that the test also writes commits to, as a pull
// request's author would push them.
type applyFixture struct {
	folder string
	url    string
	bare   *git.Repository
	remote *Remote
	log    bytes.Buffer
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	f := &applyFixture{folder: committed(t, "alpha")}
	dir := t.TempDir()
	f.bare = bare(t, dir, "backups.git")
	f.url = filepath.Join(dir, "backups.git")
	f.remote = &Remote{URL: f.url}
	f.push(t)
	return f
}

func (f *applyFixture) push(t *testing.T) {
	t.Helper()
	if _, err := f.remote.Push(context.Background(), f.folder, "alpha", nil); err != nil {
		t.Fatal(err)
	}
}

// head returns the workspace branch's commit.
func (f *applyFixture) head(t *testing.T) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(f.folder)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("alpha"), false)
	if err != nil {
		t.Fatal(err)
	}
	return ref.Hash()
}

// local commits the workspace's changes as backup-local-changes does.
func (f *applyFixture) local(t *testing.T) plumbing.Hash {
	t.Helper()
	c, err := CommitAll(f.folder, "alpha", "local", &bytes.Buffer{})
	if err != nil || !c.Changed {
		t.Fatalf("local commit: %v %v", c.Changed, err)
	}
	return c.Hash
}

// remoteCommit writes a commit to the bare repository whose tree is the
// first parent's with edits, and points branch at it.
func (f *applyFixture) remoteCommit(t *testing.T, branch string, edits map[string]*file, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	files := map[string]entry{}
	if len(parents) > 0 {
		c, err := f.bare.CommitObject(parents[0])
		if err != nil {
			t.Fatal(err)
		}
		if files, err = commitFiles(f.bare.Storer, c); err != nil {
			t.Fatal(err)
		}
	}
	s := f.bare.Storer
	for p, e := range edits {
		if e == nil {
			delete(files, p)
			continue
		}
		obj := s.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, _ := obj.Writer()
		_, _ = w.Write([]byte(e.body))
		_ = w.Close()
		hash, err := s.SetEncodedObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = entry{e.mode, hash}
	}
	tree, err := writeTree(s, files)
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "Contributor", Email: "contributor@example.invalid"}
	c := &object.Commit{Author: sig, Committer: sig, Message: "change", TreeHash: tree, ParentHashes: parents}
	obj := s.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		t.Fatal(err)
	}
	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), hash)); err != nil {
		t.Fatal(err)
	}
	return hash
}

// apply fetches branch and applies it.
func (f *applyFixture) apply(t *testing.T, branch string) (Applied, error) {
	t.Helper()
	if _, err := f.remote.Fetch(context.Background(), f.folder, "alpha", branch, &f.log); err != nil {
		t.Fatalf("fetch %s: %v", branch, err)
	}
	return applyNow(f.folder, "alpha", branch, &f.log)
}

// state is everything an apply may change: the branch, the index and
// every file of the folder outside .git, with its mode.
func (f *applyFixture) state(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(f.head(t).String() + "\n")
	idx, err := os.ReadFile(filepath.Join(f.folder, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	b.Write(idx)
	_ = filepath.WalkDir(f.folder, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(f.folder, p)
		if rel == ".git" {
			return filepath.SkipDir
		}
		info, _ := os.Lstat(p)
		b.WriteString(rel + " " + info.Mode().String() + "\n")
		if info.Mode().IsRegular() {
			body, _ := os.ReadFile(p)
			b.Write(body)
		}
		return nil
	})
	return b.String()
}

func (f *applyFixture) read(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(f.folder, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// clean fails unless the index records exactly the branch's commit and
// the folder matches it outside ignored and nested paths.
func (f *applyFixture) clean(t *testing.T) {
	t.Helper()
	repo, err := git.PlainOpen(f.folder)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		t.Fatal(err)
	}
	if same, err := matchesHead(repo, f.head(t), idx); err != nil || !same {
		t.Fatalf("the index differs from the branch after the apply: %v", err)
	}
	if pendingOf(t, f.folder, "alpha") {
		t.Fatal("the folder differs from the branch after the apply")
	}
}

// refused fails unless err is a *NotApplicableError naming paths and
// nothing changed.
func refused(t *testing.T, err error, before, after string, paths ...string) *NotApplicableError {
	t.Helper()
	var na *NotApplicableError
	if !errors.As(err, &na) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if len(paths) > 0 && !reflect.DeepEqual(na.Paths, paths) {
		t.Fatalf("refused %v, want %v (%s)", na.Paths, paths, na.Reason)
	}
	if before != after {
		t.Fatal("a refused apply changed the workspace")
	}
	return na
}

func TestApplyBranchNamesAreBranchShortNames(t *testing.T) {
	for _, ok := range []string{"alpha", "feature/one", "review/alpha/devcontainer", "a+b"} {
		if _, err := ApplyBranch(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "HEAD", "refs/heads/alpha", "-x", "a..b", "a b", "a:b", "x/", "/x", "a//b", "a~1", "a^", "a*", "a.lock", ".hidden"} {
		if _, err := ApplyBranch(bad); !errors.Is(err, ErrInvalidApplyBranch) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFetchKeepsOnlyTheNamedBranchAtThePrivateRef(t *testing.T) {
	f := newApplyFixture(t)
	tip := f.remoteCommit(t, "feature/one", map[string]*file{"new.txt": regular("new\n")}, f.head(t))
	other := f.remoteCommit(t, "other", map[string]*file{"other.txt": regular("other\n")}, f.head(t))
	// A tag on the fetched commit is not fetched either.
	if err := f.bare.Storer.SetReference(plumbing.NewHashReference("refs/tags/v1", tip)); err != nil {
		t.Fatal(err)
	}
	configBefore, _ := os.ReadFile(filepath.Join(f.folder, ".git", "config"))

	got, err := f.remote.Fetch(context.Background(), f.folder, "alpha", "feature/one", nil)
	if err != nil || got != tip {
		t.Fatalf("fetched %s, %v; want %s", got, err, tip)
	}
	repo, _ := git.PlainOpen(f.folder)
	refs, _ := repo.References()
	var names []string
	_ = refs.ForEach(func(r *plumbing.Reference) error { names = append(names, r.Name().String()); return nil })
	if want := []string{"HEAD", "refs/heads/alpha", "refs/menagerie/apply"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("refs %v, want %v", names, want)
	}
	if _, err := repo.CommitObject(other); err == nil {
		t.Fatal("another branch's commit was fetched")
	}
	configAfter, _ := os.ReadFile(filepath.Join(f.folder, ".git", "config"))
	if !bytes.Equal(configBefore, configAfter) {
		t.Fatal("the fetch wrote the repository configuration")
	}

	// Fetching it again finds the ref up to date; a later fetch replaces
	// it; RemoveFetched deletes it.
	if got, err := f.remote.Fetch(context.Background(), f.folder, "alpha", "feature/one", nil); err != nil || got != tip {
		t.Fatalf("second fetch %s %v", got, err)
	}
	if got, err := f.remote.Fetch(context.Background(), f.folder, "alpha", "alpha", nil); err != nil || got != f.head(t) {
		t.Fatalf("refetch %s %v", got, err)
	}
	if err := RemoveFetched(f.folder, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Reference(FetchedRef, false); err == nil {
		t.Fatal("the fetched ref is still there")
	}
	if err := RemoveFetched(f.folder, "alpha"); err != nil {
		t.Fatalf("removing a missing ref: %v", err)
	}
}

func TestFetchingAMissingBranchIsBranchNotFound(t *testing.T) {
	f := newApplyFixture(t)
	if _, err := f.remote.Fetch(context.Background(), f.folder, "alpha", "absent", nil); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("got %v", err)
	}
	empty := &Remote{URL: filepath.Join(t.TempDir(), "empty.git")}
	if _, err := git.PlainInit(empty.URL, true); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Fetch(context.Background(), f.folder, "alpha", "alpha", nil); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("empty repository: %v", err)
	}
	missing := &Remote{URL: filepath.Join(t.TempDir(), "absent.git")}
	if _, err := missing.Fetch(context.Background(), f.folder, "alpha", "alpha", nil); err == nil || errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("an unreachable repository: %v", err)
	}
	if _, err := f.remote.Fetch(context.Background(), f.folder, "alpha", "refs/heads/alpha", nil); !errors.Is(err, ErrInvalidApplyBranch) {
		t.Fatalf("a full ref name: %v", err)
	}
}

func TestACommitAlreadyOnTheBranchIsAlreadyApplied(t *testing.T) {
	f := newApplyFixture(t)
	root := f.head(t)
	before := f.state(t)
	got, err := f.apply(t, "alpha")
	if err != nil || got.Outcome != AlreadyApplied || got.Hash != root {
		t.Fatalf("%+v %v", got, err)
	}
	// An ancestor of the branch.
	write(t, f.folder, "result.txt", "label: second\n")
	next := f.local(t)
	before = f.state(t)
	got, err = f.apply(t, "alpha")
	if err != nil || got.Outcome != AlreadyApplied || got.Hash != next || got.Before != next {
		t.Fatalf("%+v %v", got, err)
	}
	if f.state(t) != before {
		t.Fatal("an already applied branch changed the workspace")
	}
}

func TestAFastForwardWritesOnlyTheChangedPaths(t *testing.T) {
	f := newApplyFixture(t)
	if err := os.Chmod(filepath.Join(f.folder, "hook-marker.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	tip := f.remoteCommit(t, "alpha", map[string]*file{
		"result.txt":           regular("label: reviewed\n"),
		".devcontainer/run.sh": executable("#!/bin/sh\necho run\n"),
		"docs/link":            symlink("../result.txt"),
		"sub/kept.txt":         nil,
		".toha.json":           regular(`{"applications":["reviewed"]}` + "\n"),
	}, f.head(t))
	before := f.head(t)

	got, err := f.apply(t, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != FastForwarded || got.Hash != tip || got.Before != before || f.head(t) != tip {
		t.Fatalf("%+v", got)
	}
	if want := []string{".devcontainer/run.sh", ".toha.json", "docs/link", "result.txt", "sub/kept.txt"}; !reflect.DeepEqual(got.Paths, want) {
		t.Fatalf("paths %v, want %v", got.Paths, want)
	}
	if f.read(t, "result.txt") != "label: reviewed\n" {
		t.Fatal("result.txt not written")
	}
	info, err := os.Stat(filepath.Join(f.folder, ".devcontainer", "run.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh: %v %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(f.folder, "docs", "link")); err != nil || target != "../result.txt" {
		t.Fatalf("link: %q %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(f.folder, "sub")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the emptied folder sub remains")
	}
	// An unchanged path is not rewritten: its local mode survives.
	if info, _ := os.Stat(filepath.Join(f.folder, "hook-marker.txt")); info.Mode().Perm() != 0o600 {
		t.Fatal("an unchanged path was rewritten")
	}
	// Ignored files and the nested repository are untouched.
	if f.read(t, "cache/blob") != "ignored hook output\n" || f.read(t, "clone/README") != "nested repository file\n" {
		t.Fatal("ignored or nested files changed")
	}
	// The index holds the stat data of what the apply wrote.
	repo, _ := git.PlainOpen(f.folder)
	idx, _ := repo.Storer.Index()
	for _, p := range []string{"result.txt", "docs/link"} {
		e, err := idx.Entry(p)
		info, lerr := os.Lstat(filepath.Join(f.folder, p))
		if err != nil || lerr != nil || !e.ModifiedAt.Equal(info.ModTime()) || int64(e.Size) != info.Size() || e.Inode == 0 {
			t.Fatalf("%s: index %+v, file %v", p, e, info)
		}
	}
	f.clean(t)
}

func TestAFileAndAFolderReplaceEachOtherOnApply(t *testing.T) {
	f := newApplyFixture(t)
	tip := f.remoteCommit(t, "swap", map[string]*file{
		"result.txt":       nil,
		"result.txt/inner": regular("inner\n"),
		"sub/kept.txt":     nil,
		"sub":              regular("now a file\n"),
	}, f.head(t))
	got, err := f.apply(t, "swap")
	if err != nil || got.Hash != tip {
		t.Fatalf("%+v %v", got, err)
	}
	if f.read(t, "result.txt/inner") != "inner\n" || f.read(t, "sub") != "now a file\n" {
		t.Fatal("file and folder not swapped")
	}
	f.clean(t)
}

func TestAFolderOfRemovedFilesAndEmptyFoldersGivesWayToAFile(t *testing.T) {
	f := newApplyFixture(t)
	if err := os.MkdirAll(filepath.Join(f.folder, "sub", "empty", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	tip := f.remoteCommit(t, "swap", map[string]*file{"sub/kept.txt": nil, "sub": regular("now a file\n")}, f.head(t))
	got, err := f.apply(t, "swap")
	if err != nil || got.Hash != tip {
		t.Fatalf("%+v %v", got, err)
	}
	if f.read(t, "sub") != "now a file\n" {
		t.Fatal("sub not written")
	}
	f.clean(t)
}

func TestDivergedBranchesMergeFileByFile(t *testing.T) {
	f := newApplyFixture(t)
	base := f.head(t)
	theirs := f.remoteCommit(t, "review/one", map[string]*file{
		"result.txt":           regular("label: reviewed\n"),
		"same.txt":             regular("both\n"),
		".devcontainer/a.json": regular("{}\n"),
		"hook-marker.txt":      nil,
	}, base)
	write(t, f.folder, "local.txt", "local work\n")
	write(t, f.folder, "same.txt", "both\n")
	write(t, f.folder, ".toha.json", `{"applications":["local"]}`+"\n")
	head := f.local(t)

	got, err := f.apply(t, "review/one")
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != Merged || got.Before != head || f.head(t) != got.Hash {
		t.Fatalf("%+v", got)
	}
	if want := []string{".devcontainer/a.json", "hook-marker.txt", "result.txt"}; !reflect.DeepEqual(got.Paths, want) {
		t.Fatalf("paths %v, want %v", got.Paths, want)
	}
	repo, _ := git.PlainOpen(f.folder)
	merge, err := repo.CommitObject(got.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merge.ParentHashes, []plumbing.Hash{head, theirs}) || merge.Message != "Apply review/one to workspace alpha" || merge.Author.Name != "Menagerie" || merge.Committer.Name != "Menagerie" {
		t.Fatalf("merge commit %+v", merge)
	}
	files := strings.Join(tree(t, repo, got.Hash), " ")
	for _, want := range []string{"local.txt", "same.txt", ".devcontainer/a.json", "result.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("%s missing from the merge: %s", want, files)
		}
	}
	if strings.Contains(files, "hook-marker.txt") {
		t.Error("a path theirs removed survived")
	}
	if f.read(t, "result.txt") != "label: reviewed\n" || f.read(t, "local.txt") != "local work\n" || f.read(t, ".toha.json") != `{"applications":["local"]}`+"\n" {
		t.Fatal("the merged folder is wrong")
	}
	f.clean(t)
}

func TestConflictingChangesAreNotApplicable(t *testing.T) {
	f := newApplyFixture(t)
	base := f.head(t)
	f.remoteCommit(t, "review/one", map[string]*file{
		"result.txt":   regular("label: reviewed\n"),
		"sub/kept.txt": nil,
		"other.txt":    regular("fine\n"),
	}, base)
	write(t, f.folder, "result.txt", "label: local\n")
	write(t, f.folder, "sub/kept.txt", "edited\n")
	f.local(t)
	before := f.state(t)
	_, err := f.apply(t, "review/one")
	refused(t, err, before, f.state(t), "result.txt", "sub/kept.txt")
}

func TestAModeChangeOnBothSidesConflicts(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"result.txt": executable("label: first\n")}, f.head(t))
	if err := os.Chmod(filepath.Join(f.folder, "result.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, f.folder, "result.txt", "label: local\n")
	f.local(t)
	before := f.state(t)
	_, err := f.apply(t, "review/one")
	refused(t, err, before, f.state(t), "result.txt")
}

func TestBranchesWithoutOneMergeBaseAreNotApplicable(t *testing.T) {
	t.Run("no base", func(t *testing.T) {
		f := newApplyFixture(t)
		f.remoteCommit(t, "unrelated", map[string]*file{"x.txt": regular("x\n")})
		write(t, f.folder, "local.txt", "local\n")
		f.local(t)
		before := f.state(t)
		_, err := f.apply(t, "unrelated")
		if na := refused(t, err, before, f.state(t)); !strings.Contains(na.Reason, "no commit in common") {
			t.Fatal(na.Reason)
		}
	})
	t.Run("several bases", func(t *testing.T) {
		f := newApplyFixture(t)
		x := f.head(t)
		write(t, f.folder, "a.txt", "a\n")
		a1 := f.local(t)
		f.push(t)
		b1 := f.remoteCommit(t, "b", map[string]*file{"b.txt": regular("b\n")}, x)
		// Criss-cross: each side merges the other.
		f.remoteCommit(t, "alpha", map[string]*file{"b.txt": regular("b\n")}, a1, b1)
		f.remoteCommit(t, "criss", map[string]*file{"a.txt": regular("a\n"), "c.txt": regular("c\n")}, b1, a1)
		if got, err := f.apply(t, "alpha"); err != nil || got.Outcome != FastForwarded {
			t.Fatalf("%+v %v", got, err)
		}
		before := f.state(t)
		_, err := f.apply(t, "criss")
		if na := refused(t, err, before, f.state(t)); !strings.Contains(na.Reason, "2 merge bases") {
			t.Fatal(na.Reason)
		}
	})
}

func TestAFileWhereTheMergeHasAFolderConflicts(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"notes/today.txt": regular("today\n")}, f.head(t))
	write(t, f.folder, "notes", "a file\n")
	f.local(t)
	before := f.state(t)
	_, err := f.apply(t, "review/one")
	if na := refused(t, err, before, f.state(t), "notes"); !strings.Contains(na.Reason, "folder of the same name") {
		t.Fatal(na.Reason)
	}
}

func TestUntrackedFilesInTheWayAreNotApplicable(t *testing.T) {
	cases := map[string]struct {
		edits map[string]*file
		setup func(t *testing.T, folder string)
		want  []string
	}{
		"an ignored file":                     {edits: map[string]*file{"cache/blob": regular("tracked now\n")}, want: []string{"cache/blob"}},
		"an ignored file at a top-level path": {edits: map[string]*file{"settings.local": regular("tracked now\n")}, want: []string{"settings.local"}},
		"an ignored folder":                   {edits: map[string]*file{"cache": regular("a file\n")}, want: []string{"cache"}},
		"an ignored file above":               {edits: map[string]*file{"build.local/out": regular("x\n")}, setup: func(t *testing.T, folder string) { write(t, folder, "build.local", "ignored\n") }, want: []string{"build.local"}},
		"an untracked symbolic link above": {
			edits: map[string]*file{"linked/file": regular("x\n")},
			setup: func(t *testing.T, folder string) {
				write(t, folder, "elsewhere.local/keep", "x\n")
				write(t, folder, ".git/info/exclude", "linked\n")
				if err := os.Symlink("elsewhere.local", filepath.Join(folder, "linked")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"linked"},
		},
		"a folder holding an ignored file": {
			edits: map[string]*file{"sub": regular("now a file\n"), "sub/kept.txt": nil},
			setup: func(t *testing.T, folder string) { write(t, folder, "sub/x.local", "ignored\n") },
			want:  []string{"sub"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newApplyFixture(t)
			if c.setup != nil {
				c.setup(t, f.folder)
				if pendingOf(t, f.folder, "alpha") {
					t.Fatal("the setup left changes CommitAll would commit")
				}
			}
			f.remoteCommit(t, "review/one", c.edits, f.head(t))
			before := f.state(t)
			_, err := f.apply(t, "review/one")
			if na := refused(t, err, before, f.state(t), c.want...); !strings.Contains(na.Reason, "in the way") {
				t.Fatal(na.Reason)
			}
		})
	}
}

func TestPathsInsideANestedRepositoryAreNotApplicable(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"clone/README": regular("replaced\n"), "fine.txt": regular("fine\n")}, f.head(t))
	before := f.state(t)
	_, err := f.apply(t, "review/one")
	if na := refused(t, err, before, f.state(t), "clone"); !strings.Contains(na.Reason, "nested") {
		t.Fatal(na.Reason)
	}

	// A folder the result writes a file over that holds a repository.
	g := newApplyFixture(t)
	g.remoteCommit(t, "review/two", map[string]*file{"clone": regular("a file\n")}, g.head(t))
	before = g.state(t)
	_, err = g.apply(t, "review/two")
	if na := refused(t, err, before, g.state(t), "clone"); !strings.Contains(na.Reason, "nested") {
		t.Fatal(na.Reason)
	}
}

func TestSubmodulesAndInvalidPathsAreNotApplicable(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"vendor/lib": {body: "", mode: filemode.Submodule}}, f.head(t))
	before := f.state(t)
	_, err := f.apply(t, "review/one")
	if na := refused(t, err, before, f.state(t), "vendor/lib"); !strings.Contains(na.Reason, "submodule") {
		t.Fatal(na.Reason)
	}

	// result.txt/./y also clashes with the file result.txt: the path is
	// refused as invalid before the clash check reads its folders.
	// go-git's own server refuses to send some of these trees, so the
	// commit is written straight into the workspace repository.
	for _, bad := range []string{"sub/.git/config", "x/.GIT/hooks/pre-commit", "git~1/config", "x/GIT~1/y", "tab\there.txt", "new\nline.txt", "x\\..\\y", "result.txt/./y"} {
		t.Run(bad, func(t *testing.T) {
			g := newApplyFixture(t)
			g.fetchedWith(t, map[string]*file{bad: regular("x\n")})
			before := g.state(t)
			_, err := applyNow(g.folder, "alpha", "review/two", &g.log)
			if na := refused(t, err, before, g.state(t), bad); !strings.Contains(na.Reason, "not valid") {
				t.Fatal(na.Reason)
			}
		})
	}
}

func TestAFileChangedAfterTheLocalBackupIsNotApplicable(t *testing.T) {
	cases := map[string]func(t *testing.T, folder string){
		"edited":  func(t *testing.T, folder string) { write(t, folder, "result.txt", "label: edited later\n") },
		"deleted": func(t *testing.T, folder string) { _ = os.Remove(filepath.Join(folder, "result.txt")) },
		"made a folder": func(t *testing.T, folder string) {
			_ = os.Remove(filepath.Join(folder, "result.txt"))
			write(t, folder, "result.txt/x", "x\n")
		},
		"made executable": func(t *testing.T, folder string) { _ = os.Chmod(filepath.Join(folder, "result.txt"), 0o755) },
		"made a link": func(t *testing.T, folder string) {
			_ = os.Remove(filepath.Join(folder, "result.txt"))
			_ = os.Symlink("hook-marker.txt", filepath.Join(folder, "result.txt"))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newApplyFixture(t)
			f.remoteCommit(t, "review/one", map[string]*file{"result.txt": regular("label: reviewed\n")}, f.head(t))
			change(t, f.folder)
			before := f.state(t)
			_, err := f.apply(t, "review/one")
			if na := refused(t, err, before, f.state(t), "result.txt"); !strings.Contains(na.Reason, "changed in the workspace") {
				t.Fatal(na.Reason)
			}
		})
	}
}

func TestAWriteFailureLeavesTheBranchWhereItWas(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"a.txt": regular("a\n"), "z/new.txt": regular("z\n")}, f.head(t))
	head := f.head(t)
	beforeWrite = func() { write(t, f.folder, "z", "in the way\n") }
	t.Cleanup(func() { beforeWrite = func() {} })
	_, err := f.apply(t, "review/one")
	var failed *ApplyError
	if !errors.As(err, &failed) {
		t.Fatalf("got %v", err)
	}
	if f.head(t) != head {
		t.Fatal("the branch moved after a failed write")
	}
}

func TestALinkPutWhereTheApplyWritesIsNotFollowed(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"a.txt": regular("written\n")}, f.head(t))
	beforeWrite = func() {
		if err := os.Symlink("hook-marker.txt", filepath.Join(f.folder, "a.txt")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeWrite = func() {} })
	_, err := f.apply(t, "review/one")
	var failed *ApplyError
	if !errors.As(err, &failed) {
		t.Fatalf("got %v", err)
	}
	if f.read(t, "hook-marker.txt") != "ran\n" {
		t.Fatal("the apply wrote through a link")
	}
}

func TestTheBranchMovesOnlyFromTheCommitTheApplyRead(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"a.txt": regular("a\n")}, f.head(t))
	var moved plumbing.Hash
	beforeWrite = func() {
		write(t, f.folder, "elsewhere.txt", "committed meanwhile\n")
		moved = f.local(t)
	}
	t.Cleanup(func() { beforeWrite = func() {} })
	_, err := f.apply(t, "review/one")
	var failed *ApplyError
	if !errors.As(err, &failed) || f.head(t) != moved {
		t.Fatalf("got %v, branch at %s, want %s", err, f.head(t), moved)
	}
}

// tempFiles returns the moved-aside files left in the folder.
func tempFiles(t *testing.T, folder string) []string {
	t.Helper()
	var left []string
	_ = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if d != nil && strings.HasPrefix(d.Name(), ".menagerie-apply-") {
			left = append(left, p)
		}
		return nil
	})
	return left
}

func TestAnEditAfterTheFolderCheckIsKeptAndRefuses(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{
		"result.txt":   regular("label: reviewed\n"),
		"sub/kept.txt": nil,
	}, f.head(t))
	head := f.head(t)
	beforeWrite = func() { write(t, f.folder, "sub/kept.txt", "edited during the apply\n") }
	t.Cleanup(func() { beforeWrite = func() {} })
	_, err := f.apply(t, "review/one")
	var na *NotApplicableError
	if !errors.As(err, &na) || !reflect.DeepEqual(na.Paths, []string{"sub/kept.txt"}) || na.Reason != changedReason {
		t.Fatalf("got %v", err)
	}
	// Every moved file is back under its name, the edit included.
	if f.read(t, "sub/kept.txt") != "edited during the apply\n" || f.read(t, "result.txt") != "label: first\n" {
		t.Fatal("the folder lost a file")
	}
	if left := tempFiles(t, f.folder); len(left) != 0 || f.head(t) != head {
		t.Fatalf("left %v, branch %s", left, f.head(t))
	}
}

func TestAFolderSwappedForALinkIsNeverFollowed(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"sub/kept.txt": regular("changed\n")}, f.head(t))
	beforeWrite = func() {
		write(t, f.folder, "decoy.local/kept.txt", "kept\n")
		if err := os.Rename(filepath.Join(f.folder, "sub"), filepath.Join(f.folder, "sub.local")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("decoy.local", filepath.Join(f.folder, "sub")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeWrite = func() {} })
	_, err := f.apply(t, "review/one")
	var na *NotApplicableError
	if !errors.As(err, &na) || !reflect.DeepEqual(na.Paths, []string{"sub/kept.txt"}) {
		t.Fatalf("got %v", err)
	}
	if f.read(t, "decoy.local/kept.txt") != "kept\n" || len(tempFiles(t, f.folder)) != 0 {
		t.Fatal("the apply changed the folder behind the link")
	}
}

func TestAFolderMadeALinkBeforeAWriteIsNeverFollowed(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{"newdir/sub/x.txt": regular("x\n")}, f.head(t))
	head := f.head(t)
	beforeWrite = func() {
		if err := os.Mkdir(filepath.Join(f.folder, "decoy.local"), 0o755); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("decoy.local", filepath.Join(f.folder, "newdir")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeWrite = func() {} })
	_, err := f.apply(t, "review/one")
	var failed *ApplyError
	if !errors.As(err, &failed) || f.head(t) != head {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.folder, "decoy.local", "sub")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the apply created a folder through the link")
	}
}

func TestAShallowRepositoryIsNotApplicable(t *testing.T) {
	f := newApplyFixture(t)
	write(t, f.folder, ".git/shallow", f.head(t).String()+"\n")
	_, err := f.remote.Fetch(context.Background(), f.folder, "alpha", "alpha", nil)
	var na *NotApplicableError
	if !errors.As(err, &na) || !strings.Contains(na.Reason, "shallow") {
		t.Fatalf("got %v", err)
	}
}

// fetched points the fetched ref at a commit written to the workspace
// repository by hand, with tree and parents.
func (f *applyFixture) fetched(t *testing.T, tree plumbing.Hash, parents ...plumbing.Hash) {
	t.Helper()
	repo, err := git.PlainOpen(f.folder)
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "Contributor", Email: "contributor@example.invalid"}
	c := &object.Commit{Author: sig, Committer: sig, Message: "partial", TreeHash: tree, ParentHashes: parents}
	obj := repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		t.Fatal(err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(FetchedRef, hash)); err != nil {
		t.Fatal(err)
	}
}

// fetchedWith points the fetched ref at a child of the workspace branch,
// written to the workspace repository by hand, whose tree is the branch's
// with edits.
func (f *applyFixture) fetchedWith(t *testing.T, edits map[string]*file) {
	t.Helper()
	repo, err := git.PlainOpen(f.folder)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.CommitObject(f.head(t))
	if err != nil {
		t.Fatal(err)
	}
	files, err := commitFiles(repo.Storer, head)
	if err != nil {
		t.Fatal(err)
	}
	for p, e := range edits {
		obj := repo.Storer.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, _ := obj.Writer()
		_, _ = w.Write([]byte(e.body))
		_ = w.Close()
		hash, err := repo.Storer.SetEncodedObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = entry{e.mode, hash}
	}
	tree, err := writeTree(repo.Storer, files)
	if err != nil {
		t.Fatal(err)
	}
	f.fetched(t, tree, head.Hash)
}

func TestMissingHistoryIsNotApplicable(t *testing.T) {
	missing := plumbing.NewHash("0123456789abcdef0123456789abcdef01234567")
	t.Run("a missing parent", func(t *testing.T) {
		f := newApplyFixture(t)
		head, _ := git.PlainOpen(f.folder)
		c, _ := head.CommitObject(f.head(t))
		f.fetched(t, c.TreeHash, missing)
		before := f.state(t)
		_, err := applyNow(f.folder, "alpha", "partial", &f.log)
		if na := refused(t, err, before, f.state(t)); !strings.Contains(na.Reason, "lacks part of the history") {
			t.Fatal(na.Reason)
		}
	})
	t.Run("a missing folder", func(t *testing.T) {
		f := newApplyFixture(t)
		repo, _ := git.PlainOpen(f.folder)
		c, _ := repo.CommitObject(f.head(t))
		tree, _ := c.Tree()
		// The head's tree, with sub's tree object missing.
		entries := append([]object.TreeEntry(nil), tree.Entries...)
		for i := range entries {
			if entries[i].Name == "sub" {
				entries[i].Hash = missing
			}
		}
		obj := repo.Storer.NewEncodedObject()
		if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
			t.Fatal(err)
		}
		partial, _ := repo.Storer.SetEncodedObject(obj)
		f.fetched(t, partial, f.head(t))
		before := f.state(t)
		_, err := applyNow(f.folder, "alpha", "partial", &f.log)
		if na := refused(t, err, before, f.state(t)); !strings.Contains(na.Reason, "lacks part of the history") {
			t.Fatal(na.Reason)
		}
	})
}

func TestAnAbsoluteTreePathIsNotApplicable(t *testing.T) {
	f := newApplyFixture(t)
	repo, _ := git.PlainOpen(f.folder)
	c, _ := repo.CommitObject(f.head(t))
	tree, _ := c.Tree()
	// An entry named /absolute at the root reads as an absolute path.
	entries := append([]object.TreeEntry(nil), tree.Entries...)
	entries = append(entries, object.TreeEntry{Name: "/absolute", Mode: filemode.Regular, Hash: tree.Entries[0].Hash})
	sort.Sort(object.TreeEntrySorter(entries))
	obj := repo.Storer.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		t.Fatal(err)
	}
	absolute, _ := repo.Storer.SetEncodedObject(obj)
	f.fetched(t, absolute, f.head(t))
	before := f.state(t)
	done := make(chan error, 1)
	go func() {
		_, err := applyNow(f.folder, "alpha", "review/one", &f.log)
		done <- err
	}()
	select {
	case err := <-done:
		if na := refused(t, err, before, f.state(t), "/absolute"); !strings.Contains(na.Reason, "not valid") {
			t.Fatal(na.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the apply did not end")
	}
}

func TestTheClashCheckEndsOnAnAbsolutePath(t *testing.T) {
	done := make(chan []string, 1)
	go func() {
		done <- fileFolderClashes(map[string]entry{"/absolute": {filemode.Regular, plumbing.ZeroHash}})
	}()
	select {
	case clashes := <-done:
		if len(clashes) != 0 {
			t.Fatalf("clashes %v", clashes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the clash check did not end")
	}
}

func TestAMoveBackNeverReplacesAFileCreatedMeanwhile(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "review/one", map[string]*file{
		"result.txt":   regular("label: reviewed\n"),
		"sub/kept.txt": regular("changed\n"),
	}, f.head(t))
	beforeWrite = func() { write(t, f.folder, "sub/kept.txt", "edited during the apply\n") }
	afterMoveAside = func() { write(t, f.folder, "result.txt", "created meanwhile\n") }
	t.Cleanup(func() { beforeWrite, afterMoveAside = func() {}, func() {} })
	_, err := f.apply(t, "review/one")
	var failed *ApplyError
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "could not be moved back: result.txt (left at .menagerie-apply-") {
		t.Fatalf("got %v", err)
	}
	if f.read(t, "result.txt") != "created meanwhile\n" || f.read(t, "sub/kept.txt") != "edited during the apply\n" {
		t.Fatal("a move back replaced a file")
	}
}

func TestALinkOfTheBranchIsReplacedAndALinkRetargetedRefuses(t *testing.T) {
	f := newApplyFixture(t)
	first := f.remoteCommit(t, "alpha", map[string]*file{"docs/link": symlink("../result.txt")}, f.head(t))
	if _, err := f.apply(t, "alpha"); err != nil {
		t.Fatal(err)
	}
	f.push(t)
	second := f.remoteCommit(t, "alpha", map[string]*file{"docs/link": symlink("../hook-marker.txt")}, first)
	if got, err := f.apply(t, "alpha"); err != nil || got.Hash != second {
		t.Fatalf("%+v %v", got, err)
	}
	if target, _ := os.Readlink(filepath.Join(f.folder, "docs", "link")); target != "../hook-marker.txt" {
		t.Fatalf("link to %q", target)
	}
	f.clean(t)

	f.remoteCommit(t, "alpha", map[string]*file{"docs/link": symlink("../.gitignore")}, second)
	beforeWrite = func() {
		_ = os.Remove(filepath.Join(f.folder, "docs", "link"))
		_ = os.Symlink("../result.txt", filepath.Join(f.folder, "docs", "link"))
	}
	t.Cleanup(func() { beforeWrite = func() {} })
	_, err := f.apply(t, "alpha")
	var na *NotApplicableError
	if !errors.As(err, &na) || !reflect.DeepEqual(na.Paths, []string{"docs/link"}) {
		t.Fatalf("got %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(f.folder, "docs", "link")); target != "../result.txt" {
		t.Fatalf("link to %q", target)
	}
}

// applyNow plans the apply of the fetched commit and writes it.
func applyNow(folder, workspace, branch string, log io.Writer) (Applied, error) {
	plan, err := PlanApply(folder, workspace, branch, log)
	if err != nil {
		return Applied{}, err
	}
	return plan.Write(folder, log)
}

func TestTheIndexKeepsTheStatOfWhatTheApplyWrote(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "alpha", map[string]*file{"result.txt": regular("label: reviewed\n")}, f.head(t))
	// A writer after the write and before the index update: same size,
	// and a time before the index is written, so go-git trusts matching
	// stat data and compares content only when the stat data differs.
	beforeIndex = func() {
		p := filepath.Join(f.folder, "result.txt")
		write(t, f.folder, "result.txt", "label: REVIEWED\n")
		earlier := time.Now().Add(-time.Hour)
		if err := os.Chtimes(p, earlier, earlier); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeIndex = func() {} })
	if _, err := f.apply(t, "alpha"); err != nil {
		t.Fatal(err)
	}
	if !pendingOf(t, f.folder, "alpha") {
		t.Fatal("the edit after the write is not pending")
	}
	c, err := CommitAll(f.folder, "alpha", "after", &bytes.Buffer{})
	if err != nil || !c.Changed {
		t.Fatalf("the edit after the write was not committed: %v %v", c.Changed, err)
	}
}

func TestAWriteFailureGivesBackTheNamesItCan(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "alpha", map[string]*file{
		".toha.json": regular(`{"applications":["reviewed"]}` + "\n"),
		"b.txt":      regular("b\n"),
		"result.txt": regular("label: reviewed\n"),
	}, f.head(t))
	head := f.head(t)
	// .toha.json is written first; b.txt is then taken.
	afterMoveAside = func() { write(t, f.folder, "b.txt", "in the way\n") }
	t.Cleanup(func() { afterMoveAside = func() {} })
	_, err := f.apply(t, "alpha")
	var failed *ApplyError
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "keep a temporary name: .toha.json at .menagerie-apply-") || strings.Contains(err.Error(), "result.txt at") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(f.log.String(), "left at a temporary name: .toha.json is at .menagerie-apply-") {
		t.Fatalf("log:\n%s", f.log.String())
	}
	if f.read(t, "result.txt") != "label: first\n" || len(tempFiles(t, f.folder)) != 1 || f.head(t) != head {
		t.Fatalf("result.txt %q, left %v", f.read(t, "result.txt"), tempFiles(t, f.folder))
	}
}

func TestAPlanIsNotWrittenOnceTheBranchMoved(t *testing.T) {
	f := newApplyFixture(t)
	f.remoteCommit(t, "alpha", map[string]*file{"result.txt": regular("label: reviewed\n")}, f.head(t))
	if _, err := f.remote.Fetch(context.Background(), f.folder, "alpha", "alpha", nil); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanApply(f.folder, "alpha", "alpha", &f.log)
	if err != nil {
		t.Fatal(err)
	}
	write(t, f.folder, "notes.txt", "committed after the plan\n")
	f.local(t)
	before := f.state(t)
	_, err = plan.Write(f.folder, &f.log)
	if na := refused(t, err, before, f.state(t)); !strings.Contains(na.Reason, "moved") {
		t.Fatal(na.Reason)
	}
}

func TestAMovedAsideFileThatCannotBeRemovedIsNamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only folder")
	}
	f := newApplyFixture(t)
	f.remoteCommit(t, "alpha", map[string]*file{"sub/kept.txt": nil, "sub": regular("now a file\n")}, f.head(t))
	sub := filepath.Join(f.folder, "sub")
	// The folder in the way, holding the moved-aside kept.txt, becomes
	// read-only, so the moved-aside file cannot be removed.
	afterMoveAside = func() {
		if err := os.Chmod(sub, 0o555); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterMoveAside = func() {}; _ = os.Chmod(sub, 0o755) })
	_, err := f.apply(t, "alpha")
	var failed *ApplyError
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "keep a temporary name: sub/kept.txt at sub/.menagerie-apply-") {
		t.Fatalf("got %v", err)
	}
}
