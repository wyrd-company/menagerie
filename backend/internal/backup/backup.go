// ---
// relationships:
//   implements: menagerie
//   references: [workspace, backup-force-push-overwrites-branch]
// ---

// Package backup keeps each workspace's Git repository and backs it up to
// the workspace repository with go-git. The local workspace is the source of
// truth: the backup branch, named exactly after the workspace, is
// force-pushed and never fetched or pulled. Its first commit has no parent
// and later backups extend its history.
package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Refs that keep Git history a template established before the workspace
// branch's independent root was created.
const (
	// PreservedBranch keeps commits a template made on the workspace branch.
	PreservedBranch plumbing.ReferenceName = "refs/menagerie/template-history"
	// PreservedHead keeps the commit a template left HEAD detached at.
	PreservedHead plumbing.ReferenceName = "refs/menagerie/template-head"
)

// signature names Menagerie as author and committer of backup commits.
func signature() *object.Signature {
	return &object.Signature{Name: "Menagerie", Email: "menagerie@menagerie.invalid", When: time.Now()}
}

// ErrInvalidBranch means a workspace name is not a legal Git branch name.
var ErrInvalidBranch = errors.New("workspace name is not a legal Git branch name")

// Branch returns the workspace branch, refs/heads/<workspace>.
func Branch(workspace string) (plumbing.ReferenceName, error) {
	name := plumbing.NewBranchReferenceName(workspace)
	if workspace == "" || strings.ContainsAny(workspace, "/\\") || name.Validate() != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidBranch, workspace)
	}
	return name, nil
}

// Init initializes an unborn Git repository in folder whose HEAD names the
// workspace branch. folder must exist and be empty, so a template's
// generation records its applied record into this repository.
func Init(folder, workspace string) error {
	branch, err := Branch(workspace)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return fmt.Errorf("initialize repository: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("initialize repository: folder %s is not empty", folder)
	}
	_, err = git.PlainInitWithOptions(folder, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: branch},
	})
	if err != nil {
		return fmt.Errorf("initialize repository: %w", err)
	}
	return nil
}

// Commit is the result of a backup commit.
type Commit struct {
	// Hash is the workspace branch's commit after the call.
	Hash plumbing.Hash
	// Changed is false when there was nothing to commit.
	Changed bool
	// Staged lists the paths staged for the commit, sorted.
	Staged []string
	// Preserved lists refs created to keep a template's own Git history.
	Preserved []plumbing.ReferenceName
}

// Output names the files a template generation or upgrade apply wrote.
type Output struct {
	// Reported are the paths the template service reported, relative to the
	// workspace folder.
	Reported []string
	// Record is the applied record's path, relative to the workspace folder.
	Record string
}

// CommitRoot commits a new workspace's generated files as the independent
// root of its workspace branch: a commit with no parent. When the
// template's hooks already made Git history, that history is kept under
// PreservedBranch or PreservedHead, never deleted.
func CommitRoot(folder, workspace string, out Output, message string, log io.Writer) (Commit, error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return Commit{}, err
	}
	preserved, err := independentRoot(repo, branch)
	if err != nil {
		return Commit{}, err
	}
	for _, ref := range preserved {
		fmt.Fprintf(log, "kept the template's Git history at %s\n", ref)
	}
	c, err := commitOutput(repo, folder, out, message, log)
	if err != nil {
		return Commit{}, err
	}
	c.Preserved = preserved
	if !c.Changed {
		return Commit{}, errors.New("commit generated files: nothing to commit")
	}
	commit, err := repo.CommitObject(c.Hash)
	if err != nil {
		return Commit{}, err
	}
	if commit.NumParents() != 0 {
		return Commit{}, fmt.Errorf("commit generated files: %s has a parent", c.Hash)
	}
	return c, nil
}

// CommitUpgrade commits an upgrade's written files on the workspace
// branch, extending its history.
func CommitUpgrade(folder, workspace string, out Output, message string, log io.Writer) (Commit, error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return Commit{}, err
	}
	if err := onBranch(repo, branch); err != nil {
		return Commit{}, err
	}
	return commitOutput(repo, folder, out, message, log)
}

// CommitTracked commits changes to tracked files, including deletions, on
// the workspace branch. Untracked files are left alone.
func CommitTracked(folder, workspace, message string) (Commit, error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return Commit{}, err
	}
	if err := onBranch(repo, branch); err != nil {
		return Commit{}, err
	}
	w, err := repo.Worktree()
	if err != nil {
		return Commit{}, err
	}
	return commit(repo, w, folder, message, true, nil)
}

// CommitAll commits every change of the workspace on its branch, as
// git add --all followed by a commit would: untracked, modified and
// deleted files, and whatever is already staged. Untracked paths the
// workspace's ignore rules exclude stay untracked, and anything inside a
// nested Git repository stays unstaged. Nothing to commit is Changed
// false.
func CommitAll(folder, workspace, message string, log io.Writer) (Commit, error) {
	c, err := changes(folder, workspace, log)
	if err != nil {
		return Commit{}, err
	}
	for _, p := range c.paths {
		fmt.Fprintf(log, "staged: %s\n", p)
	}
	return commit(c.repo, c.w, folder, message, false, c.paths)
}

// Pending reports whether CommitAll would commit anything: whether the
// index, with every change CommitAll stages applied in memory, differs
// from the branch's commit. It writes nothing.
func Pending(folder, workspace string) (bool, error) {
	c, err := changes(folder, workspace, io.Discard)
	if err != nil {
		return false, err
	}
	idx, err := c.repo.Storer.Index()
	if err != nil {
		return false, err
	}
	root, err := os.OpenRoot(folder)
	if err != nil {
		return false, fmt.Errorf("open workspace folder: %w", err)
	}
	defer root.Close()
	if err := stage(root, idx, c.paths, nil); err != nil {
		return false, err
	}
	head, err := c.repo.Reference(c.branch, false)
	if err != nil {
		return false, err
	}
	same, err := matchesHead(c.repo, head.Hash(), idx)
	return !same, err
}

// workspaceChanges are the changes CommitAll stages.
type workspaceChanges struct {
	repo   *git.Repository
	w      *git.Worktree
	branch plumbing.ReferenceName
	// paths are the untracked, modified and deleted paths outside a nested
	// repository, sorted. The status leaves out untracked paths the
	// workspace's ignore rules exclude.
	paths []string
}

func changes(folder, workspace string, log io.Writer) (workspaceChanges, error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return workspaceChanges{}, err
	}
	if err := onBranch(repo, branch); err != nil {
		return workspaceChanges{}, err
	}
	w, err := repo.Worktree()
	if err != nil {
		return workspaceChanges{}, err
	}
	if err := ignoreRules(repo, w, folder, log); err != nil {
		return workspaceChanges{}, err
	}
	status, err := w.Status()
	if err != nil {
		return workspaceChanges{}, fmt.Errorf("read workspace status: %w", err)
	}
	nested := nestedRepositories{folder: folder, known: map[string]bool{}}
	c := workspaceChanges{repo: repo, w: w, branch: branch}
	for p, s := range status {
		changed := s.Worktree == git.Untracked || s.Worktree == git.Modified || s.Worktree == git.Deleted
		if changed && !nested.contains(p) {
			c.paths = append(c.paths, p)
		}
	}
	sort.Strings(c.paths)
	return c, nil
}

func open(folder, workspace string) (*git.Repository, plumbing.ReferenceName, error) {
	branch, err := Branch(workspace)
	if err != nil {
		return nil, "", err
	}
	repo, err := git.PlainOpen(folder)
	if err != nil {
		return nil, "", fmt.Errorf("open workspace repository %s: %w", folder, err)
	}
	return repo, branch, nil
}

// onBranch checks that HEAD names the workspace branch and that the branch
// has a commit.
func onBranch(repo *git.Repository, branch plumbing.ReferenceName) error {
	head, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	if head.Type() != plumbing.SymbolicReference || head.Target() != branch {
		return fmt.Errorf("HEAD is not on the workspace branch %s", branch.Short())
	}
	if _, err := repo.Reference(branch, false); err != nil {
		return fmt.Errorf("workspace branch %s has no commit: %w", branch.Short(), err)
	}
	return nil
}

// independentRoot makes HEAD name the workspace branch with no commit, so
// the next commit has no parent. Commits a template made on that branch or
// left HEAD detached at are kept under preservation refs; other branches
// stay as they are.
func independentRoot(repo *git.Repository, branch plumbing.ReferenceName) ([]plumbing.ReferenceName, error) {
	head, err := repo.Storer.Reference(plumbing.HEAD)
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	// Every ref to keep is named and checked before any ref changes, so a
	// collision leaves the repository as it was.
	keep := map[plumbing.ReferenceName]plumbing.Hash{}
	if head.Type() == plumbing.HashReference {
		keep[PreservedHead] = head.Hash()
	}
	existing, err := repo.Reference(branch, false)
	onBranch := err == nil
	if onBranch {
		keep[PreservedBranch] = existing.Hash()
	}
	var preserved []plumbing.ReferenceName
	for _, name := range []plumbing.ReferenceName{PreservedHead, PreservedBranch} {
		if _, ok := keep[name]; !ok {
			continue
		}
		if _, err := repo.Reference(name, false); err == nil {
			return nil, fmt.Errorf("cannot keep the template's Git history: %s already exists", name)
		}
		preserved = append(preserved, name)
	}
	for _, name := range preserved {
		if err := repo.Storer.SetReference(plumbing.NewHashReference(name, keep[name])); err != nil {
			return nil, err
		}
	}
	if onBranch {
		if err := repo.Storer.RemoveReference(branch); err != nil {
			return nil, err
		}
	}
	if head.Type() != plumbing.SymbolicReference || head.Target() != branch {
		if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
			return nil, err
		}
	}
	return preserved, nil
}

// commitOutput stages a generation's or apply's output and commits it.
//
// It stages the paths the template service reported, the applied record,
// and every path that the workspace status, read once after the template
// service answered, shows as untracked, modified or deleted: files the
// template's hooks wrote are among them. Paths the workspace's ignore rules
// exclude, and anything inside a nested Git repository, stay unstaged; the
// applied record is staged even when the ignore rules exclude it.
//
// The status does not tell who wrote a path. What bounds it is the template
// service's refusal: it generates only into a folder that holds nothing but
// the new repository, and it applies an upgrade only into a workspace with
// no change and no untracked file outside the ignore rules, so an operator
// file that exists when generation or the apply starts fails the job
// instead. A file written after the template service's check and before
// this status read is staged with the output.
func commitOutput(repo *git.Repository, folder string, out Output, message string, log io.Writer) (Commit, error) {
	w, err := repo.Worktree()
	if err != nil {
		return Commit{}, err
	}
	if err := ignoreRules(repo, w, folder, log); err != nil {
		return Commit{}, err
	}
	status, err := w.Status()
	if err != nil {
		return Commit{}, fmt.Errorf("read workspace status: %w", err)
	}
	ignored := gitignore.NewMatcher(w.Excludes)
	nested := nestedRepositories{folder: folder, known: map[string]bool{}}

	record, err := relative(out.Record)
	if err != nil {
		return Commit{}, fmt.Errorf("applied record: %w", err)
	}
	stage := map[string]bool{record: true}
	for _, p := range out.Reported {
		rel, err := relative(p)
		if err != nil {
			return Commit{}, fmt.Errorf("reported path: %w", err)
		}
		switch {
		case nested.contains(rel):
			fmt.Fprintf(log, "not staged, inside a nested repository: %s\n", rel)
		case ignored.Match(strings.Split(rel, "/"), false):
			fmt.Fprintf(log, "not staged, ignored: %s\n", rel)
		default:
			stage[rel] = true
		}
	}
	for p, s := range status {
		changed := s.Worktree == git.Untracked || s.Worktree == git.Modified || s.Worktree == git.Deleted
		if !changed || stage[p] || nested.contains(p) {
			continue
		}
		stage[p] = true
		fmt.Fprintf(log, "staged hook output: %s\n", p)
	}
	staged := make([]string, 0, len(stage))
	for p := range stage {
		staged = append(staged, p)
	}
	sort.Strings(staged)
	return commit(repo, w, folder, message, false, staged)
}

// commit stages paths (or, with all, every tracked change) and commits.
// Paths are staged as stage does, so an entry a staged path conflicts
// with leaves the index. Nothing to commit is reported as Changed false,
// not an error.
func commit(repo *git.Repository, w *git.Worktree, folder, message string, all bool, paths []string) (Commit, error) {
	if len(paths) > 0 {
		idx, err := repo.Storer.Index()
		if err != nil {
			return Commit{}, err
		}
		root, err := os.OpenRoot(folder)
		if err != nil {
			return Commit{}, fmt.Errorf("open workspace folder: %w", err)
		}
		err = stage(root, idx, paths, repo.Storer)
		root.Close()
		if err != nil {
			return Commit{}, err
		}
		if err := repo.Storer.SetIndex(idx); err != nil {
			return Commit{}, fmt.Errorf("write index: %w", err)
		}
	}
	sig := signature()
	hash, err := w.Commit(message, &git.CommitOptions{All: all, Author: sig, Committer: sig})
	if errors.Is(err, git.ErrEmptyCommit) {
		head, herr := repo.Head()
		if herr != nil {
			return Commit{Staged: paths}, nil
		}
		return Commit{Hash: head.Hash(), Staged: paths}, nil
	}
	if err != nil {
		return Commit{}, fmt.Errorf("commit: %w", err)
	}
	return Commit{Hash: hash, Changed: true, Staged: paths}, nil
}

// relative cleans a workspace-relative slash path and refuses one that
// leaves the workspace.
func relative(p string) (string, error) {
	clean := path.Clean(filepath.ToSlash(p))
	if p == "" || clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q is not a path inside the workspace", p)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return "", fmt.Errorf("%q is inside the repository's Git folder", p)
	}
	return clean, nil
}

// nestedRepositories reports whether a path lies inside a Git repository
// nested in the workspace: a folder below the workspace root that holds a
// .git entry.
type nestedRepositories struct {
	folder string
	known  map[string]bool
}

func (n nestedRepositories) contains(rel string) bool {
	dir := path.Dir(rel)
	for dir != "." && dir != "/" {
		nested, ok := n.known[dir]
		if !ok {
			_, err := os.Lstat(filepath.Join(n.folder, filepath.FromSlash(dir), ".git"))
			nested = err == nil
			n.known[dir] = nested
		}
		if nested {
			return true
		}
		dir = path.Dir(dir)
	}
	return false
}
