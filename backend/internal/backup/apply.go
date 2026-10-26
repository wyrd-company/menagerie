// ---
// relationships:
//   implements: menagerie
//   references: [workspace, backup-force-push-overwrites-branch]
// ---

package backup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// ApplyOutcome is what an apply did to the workspace branch.
type ApplyOutcome string

// Apply outcomes.
const (
	// AlreadyApplied: the fetched commit is the workspace branch's commit
	// or one of its ancestors. Nothing changed.
	AlreadyApplied ApplyOutcome = "already-applied"
	// FastForwarded: the workspace branch moved to the fetched commit.
	FastForwarded ApplyOutcome = "fast-forward"
	// Merged: the workspace branch gained a merge commit of its commit and
	// the fetched commit.
	Merged ApplyOutcome = "merged"
)

// Applied is the result of an apply.
type Applied struct {
	Outcome ApplyOutcome
	// Before is the workspace branch's commit before the apply.
	Before plumbing.Hash
	// Hash is the workspace branch's commit after the apply.
	Hash plumbing.Hash
	// Paths are the paths written to or removed from the folder, sorted.
	Paths []string
}

// NotApplicableError refuses an apply before anything in the folder, the
// index or the workspace branch changes.
type NotApplicableError struct {
	// Reason says why, in one sentence.
	Reason string
	// Paths are the paths that refuse the apply, sorted. It may be empty.
	Paths []string
}

func (e *NotApplicableError) Error() string {
	if len(e.Paths) == 0 {
		return e.Reason
	}
	return e.Reason + ": " + strings.Join(e.Paths, ", ")
}

// ApplyError is an apply that failed after it began to change the folder.
// Files written before the failure remain; the workspace branch has not
// moved.
type ApplyError struct{ Err error }

func (e *ApplyError) Error() string { return e.Err.Error() }
func (e *ApplyError) Unwrap() error { return e.Err }

// entry is one file of a tree: its mode and blob. A path that is not in a
// tree has the zero entry.
type entry struct {
	mode filemode.FileMode
	hash plumbing.Hash
}

func (e entry) present() bool { return e.mode != filemode.Empty }

// Plan is an apply worked out from the repository alone. Making it wrote
// at most the objects of a merge commit, which no ref names; the folder,
// the index and every ref are as they were.
type Plan struct {
	Applied
	workspace    string
	head, result map[string]entry
}

// PlanApply works out how the commit at FetchedRef comes into the
// workspace branch. The workspace branch's commit is H and the fetched
// commit T:
//
//   - T is H or an ancestor of H: AlreadyApplied, nothing changes.
//   - H is an ancestor of T: the branch fast-forwards to T.
//   - Otherwise H and T merge file by file over their single merge base
//     B. A path whose mode and blob are equal in H and T keeps them; one
//     unchanged in H since B takes T's; one unchanged in T keeps H's;
//     any other path conflicts. The branch gains a merge commit, parents
//     H and T, by Menagerie.
//
// No merge base, several merge bases, a conflict, a file where the result
// has a folder of the same name, a path the change writes or removes that
// is not a valid tree path or is a submodule, and fetched history that
// lacks a commit or tree refuse with *NotApplicableError.
func PlanApply(folder, workspace, branch string, log io.Writer) (*Plan, error) {
	repo, ref, err := open(folder, workspace)
	if err != nil {
		return nil, err
	}
	if err := onBranch(repo, ref); err != nil {
		return nil, err
	}
	headRef, err := repo.Reference(ref, false)
	if err != nil {
		return nil, err
	}
	fetchedRef, err := repo.Reference(FetchedRef, false)
	if err != nil {
		return nil, fmt.Errorf("read the fetched commit: %w", err)
	}
	head, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, err
	}
	theirs, err := repo.CommitObject(fetchedRef.Hash())
	if err != nil {
		return nil, err
	}
	out := Applied{Before: head.Hash, Hash: head.Hash}
	// IsAncestor holds for the commit itself.
	contained, err := theirs.IsAncestor(head)
	if err != nil {
		return nil, err
	}
	if contained {
		out.Outcome = AlreadyApplied
		return &Plan{Applied: out, workspace: workspace}, nil
	}

	headFiles, err := commitFiles(repo.Storer, head)
	if err != nil {
		return nil, err
	}
	theirFiles, err := commitFiles(repo.Storer, theirs)
	if err != nil {
		return nil, missingHistory(err)
	}
	forward, err := head.IsAncestor(theirs)
	if err != nil {
		return nil, missingHistory(err)
	}
	var result map[string]entry
	if forward {
		out.Outcome, out.Hash, result = FastForwarded, theirs.Hash, theirFiles
		fmt.Fprintf(log, "%s fast-forwards the workspace branch from %s to %s\n", branch, head.Hash, theirs.Hash)
	} else {
		bases, err := head.MergeBase(theirs)
		if err != nil {
			return nil, err
		}
		switch len(bases) {
		case 0:
			return nil, &NotApplicableError{Reason: fmt.Sprintf("%s and the workspace branch have no commit in common", branch)}
		case 1:
		default:
			return nil, &NotApplicableError{Reason: fmt.Sprintf("%s and the workspace branch have %d merge bases", branch, len(bases))}
		}
		baseFiles, err := commitFiles(repo.Storer, bases[0])
		if err != nil {
			return nil, err
		}
		var conflicts []string
		result, conflicts = mergeFiles(baseFiles, headFiles, theirFiles)
		if len(conflicts) > 0 {
			return nil, &NotApplicableError{Reason: "both the workspace branch and " + branch + " changed these paths", Paths: conflicts}
		}
		out.Outcome = Merged
		fmt.Fprintf(log, "merging %s at %s into the workspace branch at %s over their merge base %s\n", branch, theirs.Hash, head.Hash, bases[0].Hash)
	}
	// Paths are checked before the clash check walks their folders.
	out.Paths = changedPaths(headFiles, result)
	if err := checkChange(out.Paths, headFiles, result); err != nil {
		return nil, err
	}
	if clashes := fileFolderClashes(result); len(clashes) > 0 {
		return nil, &NotApplicableError{Reason: "the result has a file where it also has a folder of the same name", Paths: clashes}
	}
	if out.Outcome == Merged {
		out.Hash, err = mergeCommit(repo.Storer, result, head.Hash, theirs.Hash, fmt.Sprintf("Apply %s to workspace %s", branch, workspace))
		if err != nil {
			return nil, err
		}
	}

	return &Plan{Applied: out, workspace: workspace, head: headFiles, result: result}, nil
}

// Write brings the plan into the workspace folder and branch. It refuses
// with *NotApplicableError, before anything changes, when the workspace
// branch is no longer at H, and as checkFolder and moveAside do. It
// writes only the paths that differ between H and the result, through
// workspaceFolder, then the index records the result and the branch moves
// last. A failure while writing is *ApplyError: the branch has not moved,
// the H files not yet replaced have their names back, those still at a
// temporary name are named, and recovery is manual.
func (p *Plan) Write(folder string, log io.Writer) (Applied, error) {
	repo, ref, err := open(folder, p.workspace)
	if err != nil {
		return Applied{}, err
	}
	if err := onBranch(repo, ref); err != nil {
		return Applied{}, err
	}
	headRef, err := repo.Reference(ref, false)
	if err != nil {
		return Applied{}, err
	}
	if headRef.Hash() != p.Before {
		return Applied{}, &NotApplicableError{Reason: fmt.Sprintf("the workspace branch moved to %s after the apply was planned", headRef.Hash())}
	}
	out, headFiles, result := p.Applied, p.head, p.result
	root, err := os.OpenRoot(folder)
	if err != nil {
		return Applied{}, fmt.Errorf("open workspace folder: %w", err)
	}
	defer root.Close()
	if err := checkFolder(root, out.Paths, headFiles, result); err != nil {
		return Applied{}, err
	}
	beforeWrite()
	w, err := openWorkspaceFolder(folder)
	if err != nil {
		return Applied{}, err
	}
	defer w.Close()
	moved, err := moveAside(w, out.Paths, headFiles)
	if err != nil {
		return Applied{}, err
	}
	written, err := writeFolder(w, repo.Storer, moved, out.Paths, result, log)
	if err != nil {
		return Applied{}, &ApplyError{Err: err}
	}
	beforeIndex()
	if err := recordIndex(repo, out.Paths, result, written); err != nil {
		return Applied{}, &ApplyError{Err: err}
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(ref, out.Hash), headRef); err != nil {
		return Applied{}, &ApplyError{Err: fmt.Errorf("move the workspace branch: %w", err)}
	}
	return out, nil
}

// beforeWrite runs between the folder check and the first change to the
// folder, in tests.
var beforeWrite = func() {}

// afterMoveAside runs between moving H files aside and checking them, in
// tests.
var afterMoveAside = func() {}

// beforeIndex runs between the last write to the folder and the index
// update, in tests.
var beforeIndex = func() {}

// missingHistory refuses an apply whose fetched history lacks a commit or
// tree, as in a shallow repository.
func missingHistory(err error) error {
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return &NotApplicableError{Reason: "the workspace repository lacks part of the history the apply needs"}
	}
	return err
}

// commitFiles returns the files of a commit's tree by path. Names are
// read as the tree holds them; checkChange refuses an invalid one before
// it is written. A tree that is missing is an error, never an empty
// folder.
func commitFiles(s storer.EncodedObjectStorer, c *object.Commit) (map[string]entry, error) {
	files := map[string]entry{}
	var walk func(prefix string, hash plumbing.Hash) error
	walk = func(prefix string, hash plumbing.Hash) error {
		tree, err := object.GetTree(s, hash)
		if err != nil {
			return err
		}
		for _, e := range tree.Entries {
			name := prefix + e.Name
			if e.Mode == filemode.Dir {
				if err := walk(name+"/", e.Hash); err != nil {
					return err
				}
				continue
			}
			files[name] = entry{e.Mode, e.Hash}
		}
		return nil
	}
	return files, walk("", c.TreeHash)
}

// mergeFiles merges the files of head and theirs over base, path by path.
// It returns the merged files and the paths both sides changed
// differently, sorted.
func mergeFiles(base, head, theirs map[string]entry) (map[string]entry, []string) {
	paths := map[string]bool{}
	for _, files := range []map[string]entry{base, head, theirs} {
		for p := range files {
			paths[p] = true
		}
	}
	result := map[string]entry{}
	var conflicts []string
	for p := range paths {
		b, h, t := base[p], head[p], theirs[p]
		var e entry
		switch {
		case h == t, t == b:
			e = h
		case h == b:
			e = t
		default:
			conflicts = append(conflicts, p)
			continue
		}
		if e.present() {
			result[p] = e
		}
	}
	sort.Strings(conflicts)
	return result, conflicts
}

// fileFolderClashes returns the files of files that are also a folder
// above another of its paths, sorted.
func fileFolderClashes(files map[string]entry) []string {
	var clashes []string
	seen := map[string]bool{}
	for p := range files {
		for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if _, ok := files[dir]; ok && !seen[dir] {
				seen[dir] = true
				clashes = append(clashes, dir)
			}
		}
	}
	sort.Strings(clashes)
	return clashes
}

// changedPaths returns the paths whose entry differs between before and
// after, sorted.
func changedPaths(before, after map[string]entry) []string {
	var paths []string
	for p, e := range before {
		if after[p] != e {
			paths = append(paths, p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// checkChange refuses a change that writes or removes a path that is not
// a valid tree path or that is a submodule on either side.
func checkChange(paths []string, head, result map[string]entry) error {
	var invalid, submodules []string
	for _, p := range paths {
		switch {
		case !validTreePath(p):
			invalid = append(invalid, p)
		case head[p].mode == filemode.Submodule || result[p].mode == filemode.Submodule:
			submodules = append(submodules, p)
		}
	}
	if len(invalid) > 0 {
		return &NotApplicableError{Reason: "the change has paths that are not valid in a workspace", Paths: invalid}
	}
	if len(submodules) > 0 {
		return &NotApplicableError{Reason: "the change has submodules", Paths: submodules}
	}
	return nil
}

// mergeCommit writes the tree of files and a merge commit of it with
// parents head and theirs, and returns the commit. No ref changes.
func mergeCommit(s storer.EncodedObjectStorer, files map[string]entry, head, theirs plumbing.Hash, message string) (plumbing.Hash, error) {
	tree, err := writeTree(s, files)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	sig := signature()
	c := &object.Commit{Author: *sig, Committer: *sig, Message: message, TreeHash: tree, ParentHashes: []plumbing.Hash{head, theirs}}
	obj := s.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.SetEncodedObject(obj)
}

// writeTree writes the trees of files, a flat map of slash paths, and
// returns the root tree.
func writeTree(s storer.EncodedObjectStorer, files map[string]entry) (plumbing.Hash, error) {
	type folder struct {
		files   map[string]entry
		folders map[string]*folder
	}
	newFolder := func() *folder { return &folder{files: map[string]entry{}, folders: map[string]*folder{}} }
	root := newFolder()
	for p, e := range files {
		f := root
		parts := strings.Split(p, "/")
		for _, part := range parts[:len(parts)-1] {
			next, ok := f.folders[part]
			if !ok {
				next = newFolder()
				f.folders[part] = next
			}
			f = next
		}
		f.files[parts[len(parts)-1]] = e
	}
	var write func(f *folder) (plumbing.Hash, error)
	write = func(f *folder) (plumbing.Hash, error) {
		t := &object.Tree{}
		for name, e := range f.files {
			t.Entries = append(t.Entries, object.TreeEntry{Name: name, Mode: e.mode, Hash: e.hash})
		}
		for name, sub := range f.folders {
			hash, err := write(sub)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			t.Entries = append(t.Entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
		}
		sort.Sort(object.TreeEntrySorter(t.Entries))
		obj := s.NewEncodedObject()
		if err := t.Encode(obj); err != nil {
			return plumbing.ZeroHash, err
		}
		return s.SetEncodedObject(obj)
	}
	return write(root)
}

// checkFolder refuses, with *NotApplicableError, a folder where the change
// could write over something H does not hold: a nested Git repository
// above a path, or an untracked or ignored file, folder or symbolic link
// where the result adds a path. A folder in the way is allowed only when
// it holds nothing but H files the change removes. Paths H has are
// checked by moveAside, once they are out of the devcontainer's way.
func checkFolder(root *os.Root, paths []string, head, result map[string]entry) error {
	changed := map[string]bool{}
	for _, p := range paths {
		changed[p] = true
	}
	removed := func(p string) bool { return changed[p] && head[p].present() && !result[p].present() }
	var nested, occupied []string
	for _, p := range paths {
		if dir, kind := above(root, p, removed); kind != "" {
			if kind == "nested" {
				nested = append(nested, dir)
			} else {
				occupied = append(occupied, dir)
			}
			continue
		}
		if head[p].present() {
			continue
		}
		info, err := root.Lstat(p)
		switch {
		// Below an H file the change removes, nothing exists yet.
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		case err == nil && info.IsDir():
			if kind := folderInTheWay(root, p, removed); kind == "nested" {
				nested = append(nested, p)
			} else if kind != "" {
				occupied = append(occupied, p)
			}
		default:
			occupied = append(occupied, p)
		}
	}
	switch {
	case len(nested) > 0:
		return &NotApplicableError{Reason: "the change writes inside nested Git repositories", Paths: unique(nested)}
	case len(occupied) > 0:
		return &NotApplicableError{Reason: "untracked or ignored files or folders are in the way of the change", Paths: unique(occupied)}
	}
	return nil
}

// above checks the folders above p in the workspace. kind is "nested"
// when one of them holds a nested Git repository and "occupied" when one
// is not a real folder and is not an H file the change removes; dir names
// it. An absent folder ends the check.
func above(root *os.Root, p string, removed func(string) bool) (dir, kind string) {
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		dir := strings.Join(parts[:i], "/")
		info, err := root.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", ""
		case err != nil:
			return dir, "occupied"
		case !info.IsDir():
			if removed(dir) {
				return "", ""
			}
			return dir, "occupied"
		}
		if _, err := root.Lstat(dir + "/.git"); !errors.Is(err, fs.ErrNotExist) {
			return dir, "nested"
		}
	}
	return "", ""
}

// folderInTheWay checks a real folder at a path the result writes. kind
// is "" when it holds only H files the change removes, "nested" when it
// holds a .git entry, and "occupied" otherwise.
func folderInTheWay(root *os.Root, p string, removed func(string) bool) (kind string) {
	err := fs.WalkDir(root.FS(), p, func(name string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			kind = "occupied"
		case d.Name() == ".git":
			kind = "nested"
		case d.IsDir():
			return nil
		case !removed(name):
			kind = "occupied"
		default:
			return nil
		}
		return fs.SkipAll
	})
	if err != nil {
		return "occupied"
	}
	return kind
}

func unique(paths []string) []string {
	sort.Strings(paths)
	out := paths[:0]
	for i, p := range paths {
		if i == 0 || p != paths[i-1] {
			out = append(out, p)
		}
	}
	return out
}

// changedReason refuses an H file that no longer matches H.
const changedReason = "these files changed in the workspace after its local changes were backed up"

// movedAside is an H file moved to a temporary name in its own folder.
type movedAside struct{ path, temp string }

// moveAside moves every H file the change replaces or removes to a
// temporary name in its folder, out of the devcontainer's way, then checks
// that each still has H's mode and blob. When one does not, or is gone, it
// moves them all back and refuses with *NotApplicableError, so nothing
// changed.
func moveAside(w *workspaceFolder, paths []string, head map[string]entry) ([]movedAside, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	prefix := ".menagerie-apply-" + hex.EncodeToString(random) + "-"
	var moved []movedAside
	var mismatched []string
	for i, p := range paths {
		if !head[p].present() {
			continue
		}
		temp := fmt.Sprintf("%s%d", prefix, i)
		err := w.rename(p, path.Base(p), temp)
		switch {
		case err == nil:
			moved = append(moved, movedAside{p, temp})
		// Gone, or below a folder that became a file or a link.
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP):
			mismatched = append(mismatched, p)
		default:
			return nil, moveBack(w, moved, fmt.Errorf("move %s aside: %w", p, err))
		}
	}
	afterMoveAside()
	for _, m := range moved {
		if same, err := w.matches(m.path, m.temp, head[m.path]); err != nil || !same {
			mismatched = append(mismatched, m.path)
		}
	}
	if len(mismatched) > 0 {
		return nil, moveBack(w, moved, &NotApplicableError{Reason: changedReason, Paths: unique(mismatched)})
	}
	return moved, nil
}

// moveBack gives the moved files their names back and returns cause. A
// file it cannot move back makes cause an *ApplyError naming it.
func moveBack(w *workspaceFolder, moved []movedAside, cause error) error {
	var stuck []string
	for _, m := range moved {
		if err := w.rename(m.path, m.temp, path.Base(m.path)); err != nil {
			stuck = append(stuck, fmt.Sprintf("%s (left at %s)", m.path, path.Join(path.Dir(m.path), m.temp)))
		}
	}
	if len(stuck) > 0 {
		return &ApplyError{Err: fmt.Errorf("%v; these files could not be moved back: %s", cause, strings.Join(stuck, ", "))}
	}
	return cause
}

// writeFolder writes the result's files of paths, each to a name nothing
// holds, then removes the moved-aside H files, and returns the stat data
// of each file it wrote, taken from the file it wrote. A folder in the
// way, which holds only moved-aside files and folders, is removed first;
// a folder left empty by a removal is removed, as Git does. On a failure
// the moved-aside files go back to their names where nothing was written
// there; the error and log name those that keep a temporary name.
func writeFolder(w *workspaceFolder, objects storer.EncodedObjectStorer, moved []movedAside, paths []string, result map[string]entry, log io.Writer) (map[string]fileStat, error) {
	temps := map[string]bool{}
	for _, m := range moved {
		temps[path.Join(path.Dir(m.path), m.temp)] = true
	}
	gone := map[string]bool{}
	drop := func(dir, name string) bool {
		key := path.Join(dir, name)
		gone[key] = temps[key]
		return temps[key]
	}
	written := map[string]fileStat{}
	err := writeFiles(w, objects, paths, result, drop, written, log)
	if err == nil {
		err = removeAside(w, moved, gone, result, log)
	}
	if err != nil {
		return nil, keepNames(w, moved, gone, err, log)
	}
	return written, nil
}

// removeAside removes the moved-aside files gone does not list, and the
// folders a removal empties.
func removeAside(w *workspaceFolder, moved []movedAside, gone map[string]bool, result map[string]entry, log io.Writer) error {
	for i := len(moved) - 1; i >= 0; i-- {
		m := moved[i]
		key := path.Join(path.Dir(m.path), m.temp)
		if !gone[key] {
			if err := w.unlink(m.path, m.temp); err != nil {
				return fmt.Errorf("remove %s: %w", m.path, err)
			}
			gone[key] = true
		}
		if result[m.path].present() {
			continue
		}
		fmt.Fprintf(log, "removed: %s\n", m.path)
		for dir := path.Dir(m.path); dir != "."; dir = path.Dir(dir) {
			if w.rmdir(dir) != nil {
				break
			}
		}
	}
	return nil
}

// writeFiles writes the result's files of paths and records their stat
// data in written.
func writeFiles(w *workspaceFolder, objects storer.EncodedObjectStorer, paths []string, result map[string]entry, drop func(dir, name string) bool, written map[string]fileStat, log io.Writer) error {
	for _, p := range paths {
		e := result[p]
		if !e.present() {
			continue
		}
		if dir := path.Dir(p); dir != "." {
			if err := w.mkdirAll(dir); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
		}
		if w.isFolder(p) {
			if err := w.removeFolder(p, drop); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
		}
		st, err := writeEntry(w, objects, p, e)
		if err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		written[p] = st
		fmt.Fprintf(log, "wrote: %s\n", p)
	}
	return nil
}

// keepNames moves the moved-aside files that are still there back to
// their names where nothing was written, and returns cause naming every
// one that keeps a temporary name.
func keepNames(w *workspaceFolder, moved []movedAside, gone map[string]bool, cause error, log io.Writer) error {
	var left []string
	for _, m := range moved {
		temp := path.Join(path.Dir(m.path), m.temp)
		if gone[temp] || w.rename(m.path, m.temp, path.Base(m.path)) == nil {
			continue
		}
		fmt.Fprintf(log, "left at a temporary name: %s is at %s\n", m.path, temp)
		left = append(left, fmt.Sprintf("%s at %s", m.path, temp))
	}
	if len(left) == 0 {
		return cause
	}
	return fmt.Errorf("%w; these files of the state before the apply keep a temporary name: %s", cause, strings.Join(left, ", "))
}

// writeEntry creates p, which must not exist, with e's blob.
func writeEntry(w *workspaceFolder, objects storer.EncodedObjectStorer, p string, e entry) (fileStat, error) {
	obj, err := objects.EncodedObject(plumbing.BlobObject, e.hash)
	if err != nil {
		return fileStat{}, err
	}
	r, err := obj.Reader()
	if err != nil {
		return fileStat{}, err
	}
	defer r.Close()
	return w.create(p, e.mode, r)
}

// recordIndex makes the index record result for paths: each path's entry
// is removed, and a path the result has is recorded with the stat data
// written holds for it. A path without stat data keeps none, so Git and
// go-git compare its content.
func recordIndex(repo *git.Repository, paths []string, result map[string]entry, written map[string]fileStat) error {
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	changed := map[string]bool{}
	for _, p := range paths {
		changed[p] = true
	}
	removeEntries(idx, func(name string) bool { return changed[name] })
	for _, p := range paths {
		e := result[p]
		if !e.present() {
			continue
		}
		ie := idx.Add(p)
		ie.Hash, ie.Mode = e.hash, e.mode
		if st, ok := written[p]; ok {
			ie.ModifiedAt, ie.Size = st.modified, st.size
			ie.Dev, ie.Inode, ie.UID, ie.GID = st.dev, st.inode, st.uid, st.gid
		}
	}
	return repo.Storer.SetIndex(idx)
}
