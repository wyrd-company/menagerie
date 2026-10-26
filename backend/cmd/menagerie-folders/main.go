// ---
// relationships:
//   implements: menagerie
// ---

// Command menagerie-folders prepares the image's own folders for the
// services' user. The image's start-up runs it as root before either
// service starts:
//
//	menagerie-folders -uid UID -gid GID -root WORKSPACE_ROOT
//
// The folders are /run/menagerie, the template service's socket folder,
// and /var/lib/menagerie with its toha, home and candidates folders; the
// candidates folder is the template service's temporary folder, where Toha
// builds an upgrade's candidate. Each is opened
// without following a symbolic link and checked through that open
// descriptor; every change is made through the same descriptor, so a
// folder replaced after its check is never the one changed. Nothing is
// changed until every existing folder has passed its check, and nothing
// beneath them is ever changed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// folder is one of Menagerie's own folders.
type folder struct {
	parent, name string
	mode         uint32
	// data folders must belong to the services' user, or be root-owned and
	// empty, and only the user's entries may be inside them.
	data     bool
	contents bool
}

// folders are prepared in this order; a folder's parent comes first.
var folders = []folder{
	{parent: "/run", name: "menagerie", mode: 0o700},
	{parent: "/var/lib", name: "menagerie", mode: 0o755, data: true},
	{parent: "/var/lib/menagerie", name: "toha", mode: 0o755, data: true, contents: true},
	{parent: "/var/lib/menagerie", name: "home", mode: 0o700, data: true, contents: true},
	{parent: "/var/lib/menagerie", name: "candidates", mode: 0o700, data: true, contents: true},
}

// afterCheck runs between the checks and the changes; tests replace it.
var afterCheck = func() {}

// afterCreate runs between creating a missing folder and opening it; tests
// replace it.
var afterCreate = func(path string) {}

func main() {
	uid := flag.Int("uid", -1, "the services' user id")
	gid := flag.Int("gid", -1, "the services' group id")
	root := flag.String("root", "", "the workspace root, named in messages")
	flag.Parse()
	if *uid < 0 || *gid < 0 || *root == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: menagerie-folders -uid UID -gid GID -root WORKSPACE_ROOT")
		os.Exit(2)
	}
	if err := prepare(folders, *uid, *gid, *root); err != nil {
		fmt.Fprintln(os.Stderr, "menagerie:", err)
		os.Exit(1)
	}
}

// opened is a checked folder: its descriptor, or -1 when it did not exist.
type opened struct {
	folder
	fd int
}

func (f folder) path() string { return filepath.Join(f.parent, f.name) }

// prepare checks every folder, then creates the missing ones and gives each
// to uid:gid with its mode.
func prepare(list []folder, uid, gid int, root string) error {
	held := map[string]int{}
	defer func() {
		for _, fd := range held {
			_ = unix.Close(fd)
		}
	}()
	parentFD := func(f folder) (int, error) {
		if fd, ok := held[f.parent]; ok && fd >= 0 {
			return fd, nil
		}
		// The parents not prepared here belong to the image: /run and /var/lib.
		fd, err := unix.Open(f.parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, fmt.Errorf("%s: %w", f.parent, err)
		}
		held[f.parent+"\x00parent"] = fd
		return fd, nil
	}

	var checked []opened
	for _, f := range list {
		if fd, ok := held[f.parent]; ok && fd < 0 {
			// The parent does not exist yet, so neither does this folder.
			held[f.path()] = -1
			checked = append(checked, opened{f, -1})
			continue
		}
		pfd, err := parentFD(f)
		if err != nil {
			return err
		}
		fd, err := openFolder(pfd, f)
		if err != nil {
			return err
		}
		if fd >= 0 {
			if err := check(fd, f, uid, root); err != nil {
				_ = unix.Close(fd)
				return err
			}
		}
		held[f.path()] = fd
		checked = append(checked, opened{f, fd})
	}

	afterCheck()

	for _, o := range checked {
		fd := o.fd
		if fd < 0 {
			pfd, err := parentFD(o.folder)
			if err != nil {
				return err
			}
			if err := unix.Mkdirat(pfd, o.name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("%s: %w", o.path(), err)
			}
			afterCreate(o.path())
			if fd, err = openFolder(pfd, o.folder); err != nil {
				return err
			}
			if fd < 0 {
				return fmt.Errorf("%s disappeared while it was created", o.path())
			}
			held[o.path()] = fd
			if err := check(fd, o.folder, uid, root); err != nil {
				return err
			}
		}
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return fmt.Errorf("%s: %w", o.path(), err)
		}
		if err := unix.Fchmod(fd, o.mode); err != nil {
			return fmt.Errorf("%s: %w", o.path(), err)
		}
	}
	return nil
}

// openFolder opens name in the parent descriptor without following a
// symbolic link. It returns -1 when name does not exist.
func openFolder(pfd int, f folder) (int, error) {
	fd, err := unix.Openat(pfd, f.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		return fd, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return -1, nil
	}
	var st unix.Stat_t
	if serr := fstatat(pfd, f.name, &st); serr == nil {
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			return -1, fmt.Errorf("%s is a symbolic link; Menagerie's folders must be real folders", f.path())
		case unix.S_IFDIR:
		default:
			return -1, fmt.Errorf("%s is not a folder", f.path())
		}
	}
	return -1, fmt.Errorf("%s: %w", f.path(), err)
}

// check checks an open folder: a data folder belongs to uid, or is
// root-owned and empty (a new volume, or a folder just created here), and
// holds only uid's entries.
func check(fd int, f folder, uid int, root string) error {
	if !f.data {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s: %w", f.path(), err)
	}
	if int(st.Uid) != uid {
		empty, err := isEmpty(fd)
		if err != nil {
			return fmt.Errorf("%s: %w", f.path(), err)
		}
		switch {
		case st.Uid != 0:
			return fmt.Errorf("%s belongs to uid %d, but Menagerie runs as uid %d, the owner of %s. Remove the data volume or give it to uid %d.", f.path(), st.Uid, uid, root, uid)
		case !empty:
			return fmt.Errorf("%s belongs to uid 0 and is not empty, but Menagerie runs as uid %d, the owner of %s. Remove the data volume or give its contents to uid %d.", f.path(), uid, root, uid)
		}
	}
	if f.contents {
		return checkContents(fd, f.path(), uid, root)
	}
	return nil
}

// checkContents walks a folder through descriptors, never following a
// symbolic link, and refuses the first entry uid does not own.
func checkContents(fd int, path string, uid int, root string) error {
	names, err := readNames(fd)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, name := range names {
		var st unix.Stat_t
		if err := fstatat(fd, name, &st); err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(path, name), err)
		}
		entry := filepath.Join(path, name)
		if int(st.Uid) != uid {
			return fmt.Errorf("%s in /var/lib/menagerie belongs to uid %d, but Menagerie runs as uid %d, the owner of %s. Remove the data volume or give its contents to uid %d.", entry, st.Uid, uid, root, uid)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("%s: %w", entry, err)
			}
			err = checkContents(child, entry, uid, root)
			_ = unix.Close(child)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// fstatat reads name's own status in dirfd, never its link target's.
func fstatat(dirfd int, name string, st *unix.Stat_t) error {
	return unix.Fstatat(dirfd, name, st, unix.AT_SYMLINK_NOFOLLOW)
}

// readNames lists a folder's entries through a new descriptor for the same
// folder, which has its own position, so fd is left as it was.
func readNames(fd int) ([]string, error) {
	own, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(own), "")
	defer f.Close()
	return f.Readdirnames(-1)
}

func isEmpty(fd int) (bool, error) {
	names, err := readNames(fd)
	return len(names) == 0, err
}
