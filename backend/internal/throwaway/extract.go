// ---
// relationships:
//   implements: menagerie
// ---

package throwaway

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrUnsafeEntry refuses an archive entry that names an absolute path, a
// ".." component, a path outside the copied folder, or a kind of file a
// folder copy does not hold.
var ErrUnsafeEntry = errors.New("unsafe archive entry")

// ReplaceFolder makes folder hold exactly the tree in archive, the Engine's
// copy of a folder with folder's base name: added, changed and removed
// entries, permission bits (setuid, setgid and sticky bits dropped), and
// symbolic links and hard links as links. It extracts into a fresh
// sibling folder, opening every path component without following a
// symbolic link, and then swaps that folder in, so a refused or failed
// archive leaves folder as it was. Entries belong to this process's user.
// The old tree is removed after the swap; a tree that cannot be removed
// completely adds a note to log and does not fail the replacement. No
// error or note names an entry.
func ReplaceFolder(folder string, archive io.Reader, log io.Writer) (err error) {
	fresh, err := os.MkdirTemp(filepath.Dir(folder), ".menagerie-copy-")
	if err != nil {
		return err
	}
	// After the swap, fresh holds the old tree.
	defer func() {
		if removeTree(fresh) != nil {
			fmt.Fprintln(log, "a hook's previous candidate copy could not be removed completely")
		}
	}()
	if err := extract(fresh, filepath.Base(folder), archive); err != nil {
		return err
	}
	return swap(fresh, folder)
}

// removeTree removes folder and everything beneath it without following a
// symbolic link. Each folder gets its owner's read, write and search
// permission before its entries are removed, so a read-only folder is
// removed too. It removes what it can and returns every failure.
func removeTree(folder string) error {
	parent, err := unix.Open(filepath.Dir(folder), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return removeAt(parent, filepath.Base(folder))
}

// removeAt removes the entry name in the folder dir and, for a folder,
// everything beneath it.
func removeAt(dir int, name string) error {
	err := unix.Unlinkat(dir, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		return err
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	var errs []error
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		errs = append(errs, err)
	} else if err := unix.Fchmod(fd, st.Mode&0o7777|0o700); err != nil {
		errs = append(errs, err)
	}
	f := os.NewFile(uintptr(fd), name)
	names, err := f.Readdirnames(-1)
	if err != nil {
		errs = append(errs, err)
	}
	for _, n := range names {
		if err := removeAt(fd, n); err != nil {
			errs = append(errs, err)
		}
	}
	f.Close()
	if err := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// swap exchanges the folders fresh and folder. Where the filesystem cannot
// exchange them in one step, folder moves aside to fresh's name first.
func swap(fresh, folder string) error {
	err := unix.Renameat2(unix.AT_FDCWD, fresh, unix.AT_FDCWD, folder, unix.RENAME_EXCHANGE)
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) {
		return err
	}
	aside := fresh + ".old"
	if err := os.Rename(folder, aside); err != nil {
		return err
	}
	if err := os.Rename(fresh, folder); err != nil {
		_ = os.Rename(aside, folder)
		return err
	}
	return os.Rename(aside, fresh)
}

// extract writes the entries of archive whose names lie beneath base into
// root, an empty folder. base itself is root.
func extract(root, base string, archive io.Reader) error {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	type folderMode struct {
		rel  string
		mode uint32
	}
	// Folder modes are set last, deepest first, so a read-only folder is
	// still written into.
	var modes []folderMode
	// files holds the regular files written so far, which a hard link may
	// name.
	files := map[string]bool{}
	tr := tar.NewReader(archive)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		rel, ok := entryPath(hdr.Name, base)
		if !ok {
			return ErrUnsafeEntry
		}
		perm := uint32(hdr.Mode) & 0o777
		if rel == "" {
			if hdr.Typeflag != tar.TypeDir {
				return ErrUnsafeEntry
			}
			modes = append(modes, folderMode{"", perm})
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = inParent(rootFD, rel, func(dir int, name string) error {
				return unix.Mkdirat(dir, name, 0o700)
			})
			modes = append(modes, folderMode{rel, perm})
		case tar.TypeReg:
			err = inParent(rootFD, rel, func(dir int, name string) error {
				return writeFile(dir, name, perm, tr)
			})
			files[rel] = true
		case tar.TypeSymlink:
			err = inParent(rootFD, rel, func(dir int, name string) error {
				return unix.Symlinkat(hdr.Linkname, dir, name)
			})
		case tar.TypeLink:
			// A hard link stays a link to a regular file already written.
			source, ok := entryPath(hdr.Linkname, base)
			if !ok || !files[source] {
				return ErrUnsafeEntry
			}
			err = linkEntry(rootFD, source, rel)
			files[rel] = true
		default:
			return ErrUnsafeEntry
		}
		if err != nil {
			return err
		}
	}
	for i := len(modes) - 1; i >= 0; i-- {
		if err := chmodFolder(rootFD, modes[i].rel, modes[i].mode); err != nil {
			return err
		}
	}
	return nil
}

// entryPath is name relative to its first component, which must be base:
// "" for base itself. A name that is absolute, has an empty, "." or ".."
// component, or lies outside base is refused.
func entryPath(name, base string) (string, bool) {
	name = strings.TrimSuffix(name, "/")
	if name == "" || path.IsAbs(name) {
		return "", false
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	if parts[0] != base {
		return "", false
	}
	return strings.Join(parts[1:], "/"), true
}

// inParent opens rel's parent folder beneath rootFD, one component at a
// time and never through a symbolic link, and calls fn with it and rel's
// last component.
func inParent(rootFD int, rel string, fn func(dir int, name string) error) error {
	parts := strings.Split(rel, "/")
	dir, err := unix.Dup(rootFD)
	if err != nil {
		return err
	}
	defer func() { unix.Close(dir) }()
	for _, p := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		unix.Close(dir)
		dir = next
	}
	return fn(dir, parts[len(parts)-1])
}

// writeFile creates name in dir, which must not exist, with r's content.
func writeFile(dir int, name string, perm uint32, r io.Reader) error {
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	if err := unix.Fchmod(fd, perm); err != nil {
		return err
	}
	return f.Close()
}

// linkEntry makes rel a hard link to source, both beneath rootFD.
func linkEntry(rootFD int, source, rel string) error {
	return inParent(rootFD, source, func(srcDir int, srcName string) error {
		return inParent(rootFD, rel, func(dir int, name string) error {
			// Without AT_SYMLINK_FOLLOW, a link is never followed.
			return unix.Linkat(srcDir, srcName, dir, name, 0)
		})
	})
}

// chmodFolder sets the mode of the folder rel beneath rootFD.
func chmodFolder(rootFD int, rel string, mode uint32) error {
	if rel == "" {
		return unix.Fchmod(rootFD, mode)
	}
	return inParent(rootFD, rel, func(dir int, name string) error {
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		return unix.Fchmod(fd, mode)
	})
}
