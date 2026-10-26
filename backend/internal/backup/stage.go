// ---
// relationships:
//   implements: menagerie
//   references: [workspace]
// ---

package backup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// maxExcludeFile bounds an exclude file the workspace names. A larger
// one is not used.
const maxExcludeFile = 1 << 20

// Notes written to the job console when one of the workspace's exclude
// files is not used. They name no path: the repository's configuration
// is written from inside the devcontainer.
const (
	noteExcludesFile = "the repository's core.excludesFile is not a file inside the workspace; it is not used"
	noteInfoExclude  = "the repository's .git/info/exclude is not a file inside the workspace; it is not used"
)

// ignoreRules gives w the workspace's own ignore rules, lowest priority
// first: the file the repository's own core.excludesFile names, then
// .git/info/exclude, then every .gitignore. Only the repository's
// .git/config is read, never Menagerie's global or system Git
// configuration. An exclude file is used only when it resolves, with its
// symbolic links followed, to a regular file inside the workspace folder;
// otherwise log gets a note and the file is skipped.
//
// go-git's status reads the .gitignore files itself and adds w.Excludes
// after them. Excludes ends with the .gitignore rules again, so they keep
// their priority over the exclude files.
func ignoreRules(repo *git.Repository, w *git.Worktree, folder string, log io.Writer) error {
	root, err := os.OpenRoot(folder)
	if err != nil {
		return fmt.Errorf("open workspace folder: %w", err)
	}
	defer root.Close()
	cfg, err := repo.Storer.Config()
	if err != nil {
		return fmt.Errorf("read repository configuration: %w", err)
	}
	var patterns []gitignore.Pattern
	if name := cfg.Raw.Section("core").Option("excludesFile"); name != "" {
		ps, ok := excludeFile(root, folder, name)
		if !ok {
			fmt.Fprintln(log, noteExcludesFile)
		}
		patterns = append(patterns, ps...)
	}
	ps, ok := excludeFile(root, folder, ".git/info/exclude")
	if !ok {
		fmt.Fprintln(log, noteInfoExclude)
	}
	patterns = append(patterns, ps...)
	gitignores, err := gitignore.ReadPatterns(w.Filesystem, nil)
	if err != nil {
		return fmt.Errorf("read ignore rules: %w", err)
	}
	w.Excludes = append(patterns, gitignores...)
	return nil
}

// excludeFile reads the patterns of the exclude file name, a path
// relative to the workspace folder or an absolute one. A missing file has
// no patterns, as in Git. ok is false when the file is not used: it is
// outside the workspace, a symbolic link out of it, not a regular file, or
// larger than maxExcludeFile.
func excludeFile(root *os.Root, folder, name string) (patterns []gitignore.Pattern, ok bool) {
	rel, inside := insideFolder(folder, name)
	if !inside {
		return nil, false
	}
	// os.Root follows symbolic links only while they stay inside the
	// folder. O_NONBLOCK keeps a named pipe from blocking the open.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxExcludeFile {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(f, maxExcludeFile+1))
	if err != nil || len(body) > maxExcludeFile {
		return nil, false
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(nil, maxExcludeFile+1)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "#") && strings.TrimSpace(line) != "" {
			patterns = append(patterns, gitignore.ParsePattern(line, nil))
		}
	}
	return patterns, true
}

// insideFolder returns name relative to folder when it lies inside it. A
// name that starts with ~ or %( is expanded by Git against a home or
// install folder, never the workspace, so it is outside.
func insideFolder(folder, name string) (string, bool) {
	if strings.HasPrefix(name, "~") || strings.HasPrefix(name, "%(") {
		return "", false
	}
	if !filepath.IsAbs(name) {
		rel := filepath.Clean(name)
		return rel, filepath.IsLocal(rel)
	}
	bases := []string{folder}
	if resolved, err := filepath.EvalSymlinks(folder); err == nil {
		bases = append(bases, resolved)
	}
	for _, base := range bases {
		if rel, err := filepath.Rel(base, name); err == nil && filepath.IsLocal(rel) {
			return rel, true
		}
	}
	return "", false
}

// stage records paths in idx as git add --all would. A path that is a
// regular file or a symbolic link is recorded with its content; any other
// path is removed from idx. An entry that a recorded path conflicts with
// is removed too: a file where a folder of that name is now, and the
// files below a folder that is now a file. With objects nil the blobs are
// only hashed, so nothing is written.
func stage(root *os.Root, idx *index.Index, paths []string, objects storer.EncodedObjectStorer) error {
	for _, p := range paths {
		if !validTreePath(p) {
			return fmt.Errorf("stage %s: not a valid path in a Git tree", p)
		}
		info, present := lstatBelowFolders(root, p)
		if !present {
			removeEntries(idx, func(name string) bool { return name == p })
			continue
		}
		hash, err := blob(root, p, info, objects)
		if err != nil {
			return fmt.Errorf("stage %s: %w", p, err)
		}
		mode, err := filemode.NewFromOSFileMode(info.Mode())
		if err != nil {
			return fmt.Errorf("stage %s: %w", p, err)
		}
		removeEntries(idx, func(name string) bool {
			return strings.HasPrefix(name, p+"/") || strings.HasPrefix(p, name+"/")
		})
		e, err := idx.Entry(p)
		if err != nil {
			e = idx.Add(p)
		}
		e.Hash = hash
		e.Mode = mode
		e.ModifiedAt = info.ModTime()
		e.Size = uint32(info.Size())
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			e.Dev, e.Inode, e.UID, e.GID = uint32(st.Dev), uint32(st.Ino), st.Uid, st.Gid
		}
	}
	return nil
}

// lstatBelowFolders returns p's own information when p is a regular file
// or a symbolic link and every folder above it is a real folder, not a
// symbolic link; Git records nothing beyond a symbolic link.
func lstatBelowFolders(root *os.Root, p string) (os.FileInfo, bool) {
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		info, err := root.Lstat(strings.Join(parts[:i], "/"))
		if err != nil || !info.IsDir() {
			return nil, false
		}
	}
	info, err := root.Lstat(p)
	if err != nil || !(info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, false
	}
	return info, true
}

// blob hashes p's content, a symbolic link's target or a regular file's
// bytes, and writes it to objects when objects is not nil.
func blob(root *os.Root, p string, info os.FileInfo, objects storer.EncodedObjectStorer) (plumbing.Hash, error) {
	var content io.Reader
	size := info.Size()
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(p)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		content, size = strings.NewReader(target), int64(len(target))
	} else {
		f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if !os.SameFile(info, opened) {
			return plumbing.ZeroHash, errors.New("changed while being staged")
		}
		content = f
	}
	if objects == nil {
		h := plumbing.NewHasher(plumbing.BlobObject, size)
		if _, err := io.CopyN(h, content, size); err != nil {
			return plumbing.ZeroHash, err
		}
		return h.Sum(), nil
	}
	obj := objects.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(size)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := io.CopyN(w, content, size); err != nil {
		w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return objects.SetEncodedObject(obj)
}

func removeEntries(idx *index.Index, match func(name string) bool) {
	kept := idx.Entries[:0]
	for _, e := range idx.Entries {
		if !match(e.Name) {
			kept = append(kept, e)
		}
	}
	idx.Entries = kept
}

// validTreePath refuses a path Git does not record in a tree: an empty,
// . or .. component, or a .git component in any letter case.
func validTreePath(p string) bool {
	if p == "" || path.IsAbs(p) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// matchesHead reports whether idx records exactly the files of the
// commit's tree: a commit of idx would change nothing.
func matchesHead(repo *git.Repository, head plumbing.Hash, idx *index.Index) (bool, error) {
	commit, err := repo.CommitObject(head)
	if err != nil {
		return false, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, err
	}
	type file struct {
		mode filemode.FileMode
		hash plumbing.Hash
	}
	files := map[string]file{}
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		if entry.Mode != filemode.Dir {
			files[name] = file{entry.Mode, entry.Hash}
		}
	}
	if len(files) != len(idx.Entries) {
		return false, nil
	}
	for _, e := range idx.Entries {
		if f, ok := files[e.Name]; !ok || f.mode != e.Mode || f.hash != e.Hash {
			return false, nil
		}
	}
	return true, nil
}
