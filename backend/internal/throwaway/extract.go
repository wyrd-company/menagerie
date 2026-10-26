// ---
// relationships:
//   implements: menagerie
// ---

package throwaway

import (
	"archive/tar"
	"errors"
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
// symbolic links as links. It extracts into a fresh sibling folder,
// opening every path component without following a symbolic link, and
// then swaps that folder in, so a refused or failed archive leaves folder
// as it was. Entries belong to this process's user. No error names an
// entry.
func ReplaceFolder(folder string, archive io.Reader) error {
	fresh, err := os.MkdirTemp(filepath.Dir(folder), ".menagerie-copy-")
	if err != nil {
		return err
	}
	// After the swap, fresh holds the old tree.
	defer os.RemoveAll(fresh)
	if err := extract(fresh, filepath.Base(folder), archive); err != nil {
		return err
	}
	return swap(fresh, folder)
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
		case tar.TypeSymlink:
			err = inParent(rootFD, rel, func(dir int, name string) error {
				return unix.Symlinkat(hdr.Linkname, dir, name)
			})
		case tar.TypeLink:
			// A hard link becomes its own copy of an entry already written.
			source, ok := entryPath(hdr.Linkname, base)
			if !ok || source == "" {
				return ErrUnsafeEntry
			}
			err = copyEntry(rootFD, source, rel)
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

// copyEntry writes a copy of the regular file source as rel.
func copyEntry(rootFD int, source, rel string) error {
	var src *os.File
	var perm uint32
	err := inParent(rootFD, source, func(dir int, name string) error {
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			unix.Close(fd)
			return ErrUnsafeEntry
		}
		src, perm = os.NewFile(uintptr(fd), name), st.Mode&0o777
		return nil
	})
	if err != nil {
		return err
	}
	defer src.Close()
	return inParent(rootFD, rel, func(dir int, name string) error {
		return writeFile(dir, name, perm, src)
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
