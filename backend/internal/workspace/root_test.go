package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"beta", "alpha", ".hidden-space"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "alpha"), filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNamesListsEveryDirectChildFolder(t *testing.T) {
	root, err := NewRoot(fixtureRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	names, err := root.Names()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != ".hidden-space,alpha,beta" {
		t.Fatalf("names %s", got)
	}
}

func TestFolderUsesTheConfiguredPathVerbatim(t *testing.T) {
	dir := fixtureRoot(t)
	root, err := NewRoot(dir + "/./")
	if err != nil {
		t.Fatal(err)
	}
	folder, err := root.Folder("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if folder != filepath.Clean(dir)+"/alpha" {
		t.Fatalf("folder %s", folder)
	}
}

func TestFolderRejects(t *testing.T) {
	root, err := NewRoot(fixtureRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]error{
		"":          ErrInvalidName,
		".":         ErrInvalidName,
		"..":        ErrInvalidName,
		"alpha/sub": ErrInvalidName,
		"../alpha":  ErrInvalidName,
		"a\x00b":    ErrInvalidName,
		"missing":   ErrNotFound,
		"notes.txt": ErrNotFound,
		"linked":    ErrNotFound,
	}
	for name, want := range cases {
		if _, err := root.Folder(name); !errors.Is(err, want) {
			t.Errorf("Folder(%q) = %v, want %v", name, err, want)
		}
	}
}

func TestNewRootRejects(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative/root", filepath.Join(dir, "missing"), file} {
		if _, err := NewRoot(path); err == nil {
			t.Errorf("NewRoot(%q) accepted", path)
		}
	}
}
