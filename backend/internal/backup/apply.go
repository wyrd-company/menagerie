// ---
// relationships:
//   implements: menagerie
//   references: [workspace, backup-force-push-overwrites-branch]
// ---

package backup

import (
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
	"github.com/go-git/go-git/v5/plumbing/format/index"
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

// Apply brings the commit at FetchedRef into the workspace branch and its
// folder. The workspace branch's commit is H and the fetched commit T:
//
//   - T is H or an ancestor of H: AlreadyApplied, nothing changes.
//   - H is an ancestor of T: the branch fast-forwards to T.
//   - Otherwise H and T merge file by file over their single merge base
//     B. A path whose mode and blob are equal in H and T keeps them; one
//     unchanged in H since B takes T's; one unchanged in T keeps H's;
//     any other path conflicts. The branch gains a merge commit, parents
//     H and T, by Menagerie.
//
// No merge base, several merge bases, a conflict, or a file where the
// result has a folder of the same name refuse with *NotApplicableError.
// So do, before anything is written, a path the change writes or removes
// that is not a valid tree path or is a submodule, and a folder that no
// longer matches H where the change writes: a file differing from H, or an
// untracked or ignored file, folder or nested Git repository in the way.
//
// Only the paths that differ between H and the result are written to the
// folder, without following symbolic links, then the index records the
// result and the branch moves last. A failure while writing is
// *ApplyError: the branch has not moved and recovery is manual.
func Apply(folder, workspace, branch string, log io.Writer) (Applied, error) {
	repo, ref, err := open(folder, workspace)
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
	fetchedRef, err := repo.Reference(FetchedRef, false)
	if err != nil {
		return Applied{}, fmt.Errorf("read the fetched commit: %w", err)
	}
	head, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return Applied{}, err
	}
	theirs, err := repo.CommitObject(fetchedRef.Hash())
	if err != nil {
		return Applied{}, err
	}
	out := Applied{Before: head.Hash, Hash: head.Hash}
	// IsAncestor holds for the commit itself.
	contained, err := theirs.IsAncestor(head)
	if err != nil {
		return Applied{}, err
	}
	if contained {
		out.Outcome = AlreadyApplied
		return out, nil
	}

	headFiles, err := commitFiles(head)
	if err != nil {
		return Applied{}, err
	}
	theirFiles, err := commitFiles(theirs)
	if err != nil {
		return Applied{}, err
	}
	forward, err := head.IsAncestor(theirs)
	if err != nil {
		return Applied{}, err
	}
	var result map[string]entry
	if forward {
		out.Outcome, out.Hash, result = FastForwarded, theirs.Hash, theirFiles
		fmt.Fprintf(log, "%s fast-forwards the workspace branch from %s to %s\n", branch, head.Hash, theirs.Hash)
	} else {
		bases, err := head.MergeBase(theirs)
		if err != nil {
			return Applied{}, err
		}
		switch len(bases) {
		case 0:
			return Applied{}, &NotApplicableError{Reason: fmt.Sprintf("%s and the workspace branch have no commit in common", branch)}
		case 1:
		default:
			return Applied{}, &NotApplicableError{Reason: fmt.Sprintf("%s and the workspace branch have %d merge bases", branch, len(bases))}
		}
		baseFiles, err := commitFiles(bases[0])
		if err != nil {
			return Applied{}, err
		}
		var conflicts []string
		result, conflicts = mergeFiles(baseFiles, headFiles, theirFiles)
		if len(conflicts) > 0 {
			return Applied{}, &NotApplicableError{Reason: "both the workspace branch and " + branch + " changed these paths", Paths: conflicts}
		}
		out.Outcome = Merged
		fmt.Fprintf(log, "merging %s at %s into the workspace branch at %s over their merge base %s\n", branch, theirs.Hash, head.Hash, bases[0].Hash)
	}
	if clashes := fileFolderClashes(result); len(clashes) > 0 {
		return Applied{}, &NotApplicableError{Reason: "the result has a file where it also has a folder of the same name", Paths: clashes}
	}
	out.Paths = changedPaths(headFiles, result)
	if err := checkChange(out.Paths, headFiles, result); err != nil {
		return Applied{}, err
	}
	if out.Outcome == Merged {
		out.Hash, err = mergeCommit(repo.Storer, result, head.Hash, theirs.Hash, fmt.Sprintf("Apply %s to workspace %s", branch, workspace))
		if err != nil {
			return Applied{}, err
		}
	}

	root, err := os.OpenRoot(folder)
	if err != nil {
		return Applied{}, fmt.Errorf("open workspace folder: %w", err)
	}
	defer root.Close()
	if err := checkFolder(root, out.Paths, headFiles, result); err != nil {
		return Applied{}, err
	}
	beforeWrite()
	if err := writeFolder(root, repo.Storer, out.Paths, headFiles, result, log); err != nil {
		return Applied{}, &ApplyError{Err: err}
	}
	if err := recordIndex(repo, root, out.Paths, result); err != nil {
		return Applied{}, &ApplyError{Err: err}
	}
	if err := repo.Storer.CheckAndSetReference(plumbing.NewHashReference(ref, out.Hash), headRef); err != nil {
		return Applied{}, &ApplyError{Err: fmt.Errorf("move the workspace branch: %w", err)}
	}
	return out, nil
}

// beforeWrite runs between the folder check and the first write, in
// tests.
var beforeWrite = func() {}

// commitFiles returns the files of a commit's tree by path. Names are
// read as the tree holds them; checkChange refuses an invalid one before
// it is written.
func commitFiles(c *object.Commit) (map[string]entry, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	files := map[string]entry{}
	walker := object.NewTreeWalker(tree, true, nil)
	walker.SkipPathValidation()
	defer walker.Close()
	for {
		name, e, err := walker.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if e.Mode != filemode.Dir {
			files[name] = entry{e.Mode, e.Hash}
		}
	}
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
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
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

// checkFolder refuses, with *NotApplicableError, a folder where writing
// paths could lose something H does not hold: a path whose file no longer
// matches its H entry, a nested Git repository above a path, or an
// untracked or ignored file, folder or symbolic link where the result
// writes. A folder in the way is allowed only when it holds nothing but H
// files the change removes.
func checkFolder(root *os.Root, paths []string, head, result map[string]entry) error {
	changed := map[string]bool{}
	for _, p := range paths {
		changed[p] = true
	}
	removed := func(p string) bool { return changed[p] && head[p].present() && !result[p].present() }
	var mismatched, nested, occupied []string
	for _, p := range paths {
		if dir, kind := above(root, p, removed); kind != "" {
			if kind == "nested" {
				nested = append(nested, dir)
			} else {
				occupied = append(occupied, dir)
			}
			continue
		}
		info, err := root.Lstat(p)
		h := head[p]
		switch {
		// Below an H file the change removes, nothing exists yet.
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			if h.present() {
				mismatched = append(mismatched, p)
			}
		case err != nil:
			mismatched = append(mismatched, p)
		case info.IsDir():
			if h.present() {
				mismatched = append(mismatched, p)
			} else if kind := folderInTheWay(root, p, removed); kind == "nested" {
				nested = append(nested, p)
			} else if kind != "" {
				occupied = append(occupied, p)
			}
		case !h.present():
			occupied = append(occupied, p)
		default:
			if !matches(root, p, info, h) {
				mismatched = append(mismatched, p)
			}
		}
	}
	switch {
	case len(nested) > 0:
		return &NotApplicableError{Reason: "the change writes inside nested Git repositories", Paths: unique(nested)}
	case len(occupied) > 0:
		return &NotApplicableError{Reason: "untracked or ignored files or folders are in the way of the change", Paths: unique(occupied)}
	case len(mismatched) > 0:
		return &NotApplicableError{Reason: "these files changed in the workspace after its local changes were backed up", Paths: mismatched}
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

// matches reports whether the file at p, a regular file or symbolic link
// whose information is info, has H's mode and blob.
func matches(root *os.Root, p string, info os.FileInfo, h entry) bool {
	mode, err := filemode.NewFromOSFileMode(info.Mode())
	if err != nil || mode != h.mode {
		return false
	}
	hash, err := blob(root, p, info, nil)
	return err == nil && hash == h.hash
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

// writeFolder removes the H files of paths the result changes or drops,
// deepest first, then writes the result's files of paths. A folder left
// empty by a removal is removed, as Git does.
func writeFolder(root *os.Root, objects storer.EncodedObjectStorer, paths []string, head, result map[string]entry, log io.Writer) error {
	for i := len(paths) - 1; i >= 0; i-- {
		p := paths[i]
		if !head[p].present() {
			continue
		}
		if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		if !result[p].present() {
			fmt.Fprintf(log, "removed: %s\n", p)
		}
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if root.Remove(dir) != nil {
				break
			}
		}
	}
	for _, p := range paths {
		e := result[p]
		if !e.present() {
			continue
		}
		if dir := path.Dir(p); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
		}
		// A folder in the way held only removed files and folders.
		if info, err := root.Lstat(p); err == nil && info.IsDir() {
			if err := removeEmptyFolders(root, p); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
		}
		if err := writeEntry(root, objects, p, e); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		fmt.Fprintf(log, "wrote: %s\n", p)
	}
	return nil
}

// removeEmptyFolders removes the folder p and the folders below it. It
// fails on anything else below p.
func removeEmptyFolders(root *os.Root, p string) error {
	entries, err := fs.ReadDir(root.FS(), p)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return fmt.Errorf("%s is not empty", p)
		}
		if err := removeEmptyFolders(root, p+"/"+e.Name()); err != nil {
			return err
		}
	}
	return root.Remove(p)
}

// writeEntry creates p, which must not exist, with e's blob: a symbolic
// link for a link entry, otherwise a regular file that is executable for
// an executable entry. The process umask applies, as it does for Git.
func writeEntry(root *os.Root, objects storer.EncodedObjectStorer, p string, e entry) error {
	obj, err := objects.EncodedObject(plumbing.BlobObject, e.hash)
	if err != nil {
		return err
	}
	r, err := obj.Reader()
	if err != nil {
		return err
	}
	defer r.Close()
	if e.mode == filemode.Symlink {
		target, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		return root.Symlink(string(target), p)
	}
	perm := os.FileMode(0o644)
	if e.mode == filemode.Executable {
		perm = 0o755
	}
	// O_EXCL also refuses a symbolic link put at p since the check.
	f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// recordIndex makes the index record result for paths: each path's entry
// is removed, and a path the result has is recorded with what was written.
func recordIndex(repo *git.Repository, root *os.Root, paths []string, result map[string]entry) error {
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
		if info, err := root.Lstat(p); err == nil {
			setStat(ie, info)
		}
	}
	return repo.Storer.SetIndex(idx)
}

// setStat records a file's information in its index entry.
func setStat(e *index.Entry, info os.FileInfo) {
	e.ModifiedAt = info.ModTime()
	e.Size = uint32(info.Size())
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.Dev, e.Inode, e.UID, e.GID = uint32(st.Dev), uint32(st.Ino), st.Uid, st.Gid
	}
}
