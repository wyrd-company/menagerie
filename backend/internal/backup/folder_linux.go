// ---
// relationships:
//   implements: menagerie
//   references: [workspace]
// ---

package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"golang.org/x/sys/unix"
)

// workspaceFolder changes the workspace folder through descriptors: every
// folder is opened beneath the workspace with no symbolic link in any
// component, and each change is made relative to its parent folder, so a
// folder swapped for a link is refused, never followed.
type workspaceFolder struct{ fd int }

func openWorkspaceFolder(folder string) (*workspaceFolder, error) {
	fd, err := unix.Open(folder, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open workspace folder: %w", err)
	}
	return &workspaceFolder{fd: fd}, nil
}

func (w *workspaceFolder) Close() error { return unix.Close(w.fd) }

// beneath is how every folder below the workspace is resolved.
var beneath = unix.OpenHow{
	Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
	Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
}

// dir opens the folder rel, "." for the workspace folder itself.
func (w *workspaceFolder) dir(rel string) (int, error) {
	how := beneath
	return unix.Openat2(w.fd, rel, &how)
}

// parent opens the folder that holds p and returns p's name in it.
func (w *workspaceFolder) parent(p string) (int, string, error) {
	fd, err := w.dir(path.Dir(p))
	return fd, path.Base(p), err
}

// mkdirAll creates the folder rel and the folders above it, one component
// at a time.
func (w *workspaceFolder) mkdirAll(rel string) error {
	fd, err := w.dir(".")
	if err != nil {
		return err
	}
	for _, part := range strings.Split(rel, "/") {
		if err := unix.Mkdirat(fd, part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(fd)
			return err
		}
		how := beneath
		next, err := unix.Openat2(fd, part, &how)
		unix.Close(fd)
		if err != nil {
			return err
		}
		fd = next
	}
	return unix.Close(fd)
}

// rename renames from to to in the folder that holds p, never over an
// existing entry.
func (w *workspaceFolder) rename(p, from, to string) error {
	fd, _, err := w.parent(p)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE)
}

// unlink removes the file or link name from the folder that holds p.
func (w *workspaceFolder) unlink(p, name string) error {
	fd, _, err := w.parent(p)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Unlinkat(fd, name, 0)
}

// rmdir removes the empty folder rel.
func (w *workspaceFolder) rmdir(rel string) error {
	fd, name, err := w.parent(rel)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
}

// removeFolder removes the folder rel, the folders below it and the files
// in them that drop names, and fails on anything else below rel.
func (w *workspaceFolder) removeFolder(rel string, drop func(dir, name string) bool) error {
	fd, err := w.dir(rel)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), rel)
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch {
		case e.IsDir():
			if err := w.removeFolder(rel+"/"+e.Name(), drop); err != nil {
				return err
			}
		case drop(rel, e.Name()):
			if err := w.unlink(rel+"/"+e.Name(), e.Name()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not empty", rel)
		}
	}
	return w.rmdir(rel)
}

// fileStat is the stat data the index records for a file.
type fileStat struct {
	modified             time.Time
	size                 uint32
	dev, inode, uid, gid uint32
}

func newFileStat(st *unix.Stat_t) fileStat {
	return fileStat{
		modified: time.Unix(st.Mtim.Sec, st.Mtim.Nsec),
		size:     uint32(st.Size),
		dev:      uint32(st.Dev), inode: uint32(st.Ino), uid: st.Uid, gid: st.Gid,
	}
}

// create writes p, which must not exist, with content: a symbolic link
// for a link entry, otherwise a regular file that is executable for an
// executable entry. The process umask applies, as it does for Git. It
// returns the stat data of what it wrote: of a file, from its descriptor
// before it closes; of a link, read right after the link is made.
func (w *workspaceFolder) create(p string, mode filemode.FileMode, content io.Reader) (fileStat, error) {
	fd, name, err := w.parent(p)
	if err != nil {
		return fileStat{}, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if mode == filemode.Symlink {
		target, err := io.ReadAll(content)
		if err != nil {
			return fileStat{}, err
		}
		if err := unix.Symlinkat(string(target), fd, name); err != nil {
			return fileStat{}, err
		}
		err = unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
		return newFileStat(&st), err
	}
	perm := uint32(0o644)
	if mode == filemode.Executable {
		perm = 0o755
	}
	// O_EXCL also refuses a symbolic link put at p since the check.
	file, err := unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, perm)
	if err != nil {
		return fileStat{}, err
	}
	f := os.NewFile(uintptr(file), p)
	_, err = io.Copy(f, content)
	if err == nil {
		err = unix.Fstat(file, &st)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return newFileStat(&st), err
}

// matches reports whether name, in the folder that holds p, has h's mode
// and blob, without following a symbolic link. Mode, size and content
// come from one open file, so they describe the same file.
func (w *workspaceFolder) matches(p, name string, h entry) (bool, error) {
	fd, _, err := w.parent(p)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var mode filemode.FileMode
	var content io.Reader
	var size int64
	file, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	// O_NOFOLLOW refuses a symbolic link with ELOOP.
	case errors.Is(err, unix.ELOOP):
		target, err := readlinkat(fd, name)
		if err != nil {
			return false, err
		}
		mode, content, size = filemode.Symlink, strings.NewReader(target), int64(len(target))
	case err != nil:
		return false, err
	default:
		f := os.NewFile(uintptr(file), p)
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return false, err
		}
		// A folder reads as filemode.Dir, never an H entry's mode.
		if mode, err = filemode.NewFromOSFileMode(info.Mode()); err != nil {
			return false, err
		}
		content, size = f, info.Size()
	}
	if mode != h.mode {
		return false, nil
	}
	hasher := plumbing.NewHasher(plumbing.BlobObject, size)
	if _, err := io.CopyN(hasher, content, size); err != nil {
		return false, err
	}
	return hasher.Sum() == h.hash, nil
}

func readlinkat(fd int, name string) (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(fd, name, buf)
		if err != nil {
			return "", err
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// isFolder reports whether p is a real folder.
func (w *workspaceFolder) isFolder(p string) bool {
	fd, name, err := w.parent(p)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	return unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR
}
