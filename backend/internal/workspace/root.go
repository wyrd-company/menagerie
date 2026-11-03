// ---
// relationships:
//   implements: menagerie
//   references: [workspace, workspace-root]
// ---

// Package workspace discovers workspaces: every folder directly under the
// workspace root is a workspace, unless its name starts with a dot. Local
// folders are authoritative; this package never consults Docker.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotFound means no workspace folder has the requested name.
var ErrNotFound = errors.New("workspace not found")

// ErrInvalidName means a name cannot be a workspace's folder under the root.
var ErrInvalidName = errors.New("invalid workspace name")

// Root is the workspace root.
type Root struct {
	path string
}

// NewRoot opens the workspace root at an absolute path. The path is used
// as given (cleaned, not symlink-resolved), because container labels carry
// the same path the host daemon sees.
func NewRoot(path string) (*Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("workspace root %q is not absolute", path)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a folder", path)
	}
	return &Root{path: path}, nil
}

// Path returns the workspace root path.
func (r *Root) Path() string { return r.path }

// Names returns the name of every workspace, sorted: the folders directly
// under the root whose names ValidName accepts. Symbolic links and files are
// not workspaces.
func (r *Root) Names() ([]string, error) {
	entries, err := os.ReadDir(r.path)
	if err != nil {
		return nil, fmt.Errorf("read workspace root: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// FolderPath returns the absolute folder path for a workspace name without
// checking that it exists.
func (r *Root) FolderPath(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return r.path + string(filepath.Separator) + name, nil
}

// Folder returns the absolute path of an existing workspace folder.
func (r *Root) Folder(name string) (string, error) {
	folder, err := r.FolderPath(name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(folder)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", name, err)
	}
	return folder, nil
}

// ValidName reports whether name can be a workspace's folder directly under
// the root. A name that starts with a dot cannot: such a folder belongs to the
// host, as `.shared` does.
func ValidName(name string) error {
	switch {
	case name == "", strings.HasPrefix(name, "."):
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return nil
}
