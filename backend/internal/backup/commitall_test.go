package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
)

// committed returns a workspace whose branch has a root commit of what
// generate writes.
func committed(t *testing.T, name string) string {
	t.Helper()
	folder := newWorkspace(t, name)
	if _, err := CommitRoot(folder, name, generate(t, folder), "root", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	return folder
}

func pendingOf(t *testing.T, folder, name string) bool {
	t.Helper()
	dirty, err := Pending(folder, name)
	if err != nil {
		t.Fatal(err)
	}
	return dirty
}

func TestCommitAllRecordsEveryChangeOutsideIgnoredAndNestedPaths(t *testing.T) {
	folder := committed(t, "alpha")
	if pendingOf(t, folder, "alpha") {
		t.Fatal("a freshly committed workspace has pending changes")
	}
	write(t, folder, "result.txt", "label: second\n")
	write(t, folder, "scripts/new.sh", "echo new\n")
	write(t, folder, "cache/more", "ignored\n")
	write(t, folder, "extra.local", "ignored\n")
	write(t, folder, "clone/inner.txt", "inside the nested repository\n")
	if err := os.Remove(filepath.Join(folder, "sub", "kept.txt")); err != nil {
		t.Fatal(err)
	}
	if !pendingOf(t, folder, "alpha") {
		t.Fatal("changes not pending")
	}

	var log bytes.Buffer
	c, err := CommitAll(folder, "alpha", "chore: record", &log)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Changed {
		t.Fatal("nothing committed")
	}
	if want := []string{"result.txt", "scripts/new.sh", "sub/kept.txt"}; !reflect.DeepEqual(c.Staged, want) {
		t.Fatalf("staged %v, want %v", c.Staged, want)
	}
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Join(tree(t, repo, c.Hash), " ")
	for _, want := range []string{"result.txt", "scripts/new.sh", "hook-marker.txt"} {
		if !strings.Contains(files, want) {
			t.Errorf("%s missing from %s", want, files)
		}
	}
	for _, absent := range []string{"sub/kept.txt", "cache/", "extra.local", "clone/"} {
		if strings.Contains(files, absent) {
			t.Errorf("%s committed: %s", absent, files)
		}
	}
	chain := history(t, repo, "alpha")
	if len(chain) != 2 || chain[0].Message != "chore: record" || chain[0].Author.Name != "Menagerie" {
		t.Fatalf("history %d, tip %q by %q", len(chain), chain[0].Message, chain[0].Author.Name)
	}
	if !strings.Contains(log.String(), "staged: scripts/new.sh") {
		t.Errorf("log %q", log.String())
	}
	if pendingOf(t, folder, "alpha") {
		t.Fatal("changes still pending after the commit")
	}

	again, err := CommitAll(folder, "alpha", "chore: again", &log)
	if err != nil || again.Changed || again.Hash != c.Hash {
		t.Fatalf("second commit %+v %v", again, err)
	}
}

func TestCommitAllRecordsAnIndexStagedInsideTheWorkspace(t *testing.T) {
	folder := committed(t, "alpha")
	write(t, folder, "staged.txt", "staged by someone\n")
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("staged.txt"); err != nil {
		t.Fatal(err)
	}
	if !pendingOf(t, folder, "alpha") {
		t.Fatal("a staged change is not pending")
	}
	c, err := CommitAll(folder, "alpha", "chore: record", &bytes.Buffer{})
	if err != nil || !c.Changed {
		t.Fatalf("commit %+v %v", c, err)
	}
	if files := strings.Join(tree(t, repo, c.Hash), " "); !strings.Contains(files, "staged.txt") {
		t.Fatalf("staged file missing: %s", files)
	}
}

func TestCommitAllNeedsTheWorkspaceBranch(t *testing.T) {
	folder := newWorkspace(t, "alpha")
	if _, err := CommitAll(folder, "alpha", "x", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "has no commit") {
		t.Fatalf("unborn branch: %v", err)
	}
	if _, err := Pending(folder, "alpha"); err == nil {
		t.Fatal("pending on an unborn branch")
	}
	if _, err := Pending(t.TempDir(), "alpha"); err == nil {
		t.Fatal("pending without a repository")
	}
}

func TestPathsGoGitCannotReadBackStayUnstaged(t *testing.T) {
	folder := committed(t, "alpha")
	for _, name := range []string{"tab\there.txt", "new\nline.txt", "x/GIT~1/y"} {
		write(t, folder, name, "unreadable\n")
	}
	if pendingOf(t, folder, "alpha") {
		t.Fatal("only unreadable paths are pending")
	}
	write(t, folder, "ok.txt", "fine\n")
	if !pendingOf(t, folder, "alpha") {
		t.Fatal("ok.txt is not pending")
	}
	var log bytes.Buffer
	c, err := CommitAll(folder, "alpha", "chore: record", &log)
	if err != nil || !c.Changed || !reflect.DeepEqual(c.Staged, []string{"ok.txt"}) {
		t.Fatalf("%+v %v", c, err)
	}
	if !strings.Contains(log.String(), `not staged, not a valid path in a Git tree: "tab\there.txt"`) {
		t.Fatalf("log:\n%s", log.String())
	}
	// The repository still reads back: the next check and commit agree.
	if pendingOf(t, folder, "alpha") {
		t.Fatal("pending after the commit")
	}
	if c, err := CommitAll(folder, "alpha", "chore: again", &log); err != nil || c.Changed {
		t.Fatalf("%+v %v", c, err)
	}
}
