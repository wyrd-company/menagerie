package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layout is a test copy of the image's folders under a temporary root, with
// a foreign folder beside them that no change may reach.
type layout struct {
	base, run, lib, data, foreign string
	list                          []folder
}

func newLayout(t *testing.T) *layout {
	t.Helper()
	base := t.TempDir()
	l := &layout{base: base, run: filepath.Join(base, "run"), lib: filepath.Join(base, "lib"), foreign: filepath.Join(base, "foreign")}
	l.data = filepath.Join(l.lib, "menagerie")
	for _, d := range []string{l.run, l.lib, l.foreign} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(l.foreign, "stale"), []byte("foreign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.foreign, 0o710); err != nil {
		t.Fatal(err)
	}
	l.list = []folder{
		{parent: l.run, name: "menagerie", mode: 0o700},
		{parent: l.lib, name: "menagerie", mode: 0o755, data: true},
		{parent: l.data, name: "toha", mode: 0o755, data: true, contents: true},
		{parent: l.data, name: "home", mode: 0o700, data: true, contents: true},
	}
	t.Cleanup(func() { afterCheck = func() {}; afterCreate = func(string) {} })
	return l
}

func (l *layout) prepare() error {
	return prepare(l.list, os.Getuid(), os.Getgid(), "/sample/workspaces")
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func (l *layout) foreignUnchanged(t *testing.T) {
	t.Helper()
	if m := mode(t, l.foreign).Perm(); m != 0o710 {
		t.Errorf("the foreign folder's mode changed to %o", m)
	}
	if m := mode(t, filepath.Join(l.foreign, "stale")).Perm(); m != 0o644 {
		t.Errorf("the foreign file's mode changed to %o", m)
	}
}

func TestPrepareCreatesTheFoldersWithTheirModes(t *testing.T) {
	l := newLayout(t)
	if err := l.prepare(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(l.run, "menagerie"): 0o700, l.data: 0o755,
		filepath.Join(l.data, "toha"): 0o755, filepath.Join(l.data, "home"): 0o700,
	} {
		if m := mode(t, path); !m.IsDir() || m.Perm() != want {
			t.Errorf("%s is %v, want a folder with mode %o", path, m, want)
		}
	}
	// A second start finds them and sets the same modes.
	if err := os.Chmod(filepath.Join(l.data, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.prepare(); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Join(l.data, "home")).Perm(); m != 0o700 {
		t.Errorf("home's mode is %o after a second start", m)
	}
}

func TestPrepareRefusesLinksAndFilesBeforeChangingAnything(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, l *layout)
		want  string
	}{
		{"a linked home", func(t *testing.T, l *layout) {
			if err := os.Symlink(l.foreign, filepath.Join(l.data, "home")); err != nil {
				t.Fatal(err)
			}
		}, "/home is a symbolic link"},
		{"a linked data folder", func(t *testing.T, l *layout) {
			if err := os.Remove(l.data); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(l.foreign, l.data); err != nil {
				t.Fatal(err)
			}
		}, "/menagerie is a symbolic link"},
		{"a Toha file", func(t *testing.T, l *layout) {
			if err := os.WriteFile(filepath.Join(l.data, "toha"), []byte("a file\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "/toha is not a folder"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLayout(t)
			if err := os.Mkdir(l.data, 0o750); err != nil {
				t.Fatal(err)
			}
			c.setup(t, l)
			err := l.prepare()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("prepare answered %v, want %q", err, c.want)
			}
			l.foreignUnchanged(t)
			if _, err := os.Lstat(filepath.Join(l.run, "menagerie")); !os.IsNotExist(err) {
				t.Errorf("the run folder was created before the refusal: %v", err)
			}
			if info, err := os.Lstat(l.data); err == nil && info.Mode().IsDir() && info.Mode().Perm() != 0o750 {
				t.Errorf("the data folder's mode changed to %o", info.Mode().Perm())
			}
		})
	}
}

// A checked home replaced by a link to the foreign folder before the
// changes: the change reaches the checked folder, never the link's target.
func TestPrepareChangesTheCheckedFolderNotItsReplacement(t *testing.T) {
	l := newLayout(t)
	home := filepath.Join(l.data, "home")
	for _, d := range []string{l.data, home} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	afterCheck = func() {
		if err := os.Rename(home, home+".checked"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(l.foreign, home); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.prepare(); err != nil {
		t.Fatal(err)
	}
	l.foreignUnchanged(t)
	if m := mode(t, home); m&os.ModeSymlink == 0 {
		t.Fatalf("the replacement did not happen: home is %v", m)
	}
	if m := mode(t, home+".checked").Perm(); m != 0o700 {
		t.Errorf("the checked home has mode %o, want 0700", m)
	}
}

// A missing Toha folder replaced by a link between its creation and its
// opening: prepare refuses, and the link's target is unchanged.
func TestPrepareRefusesACreatedFolderReplacedByALink(t *testing.T) {
	l := newLayout(t)
	if err := os.Mkdir(l.data, 0o755); err != nil {
		t.Fatal(err)
	}
	toha := filepath.Join(l.data, "toha")
	afterCreate = func(path string) {
		if path != toha {
			return
		}
		if err := os.Remove(toha); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(l.foreign, toha); err != nil {
			t.Fatal(err)
		}
	}
	err := l.prepare()
	if err == nil || !strings.Contains(err.Error(), "/toha is a symbolic link") {
		t.Fatalf("prepare answered %v", err)
	}
	l.foreignUnchanged(t)
}
