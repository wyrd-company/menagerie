package throwaway

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// entry is one archive entry.
type entry struct {
	name, body, link string
	mode             int64
	dir              bool
	kind             byte
}

func archive(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: tar.TypeReg, Size: int64(len(e.body)), Linkname: e.link}
		switch {
		case e.kind != 0:
			hdr.Typeflag, hdr.Size = e.kind, 0
		case e.dir:
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeTree creates folder with files, by relative path.
func writeTree(t *testing.T, folder string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(folder, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
}

// readTree is folder's tree by relative path: a file's content, a
// folder's "/", a link's "-> " and its target.
func readTree(t *testing.T, folder string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == folder {
			return err
		}
		rel, _ := filepath.Rel(folder, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			tree[rel] = "-> " + target
			return err
		case d.IsDir():
			tree[rel] = "/"
		default:
			body, err := os.ReadFile(path)
			tree[rel] = string(body)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

// TestACopyBackMakesTheFolderMatchTheArchive replaces a folder: changed,
// added and removed entries, modes, links kept as links and hard links as
// copies.
func TestACopyBackMakesTheFolderMatchTheArchive(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "target")
	writeTree(t, folder, map[string]string{"changed.txt": "v1\n", "removed.txt": "x", "old/inner.txt": "x"})
	err := ReplaceFolder(folder, bytes.NewReader(archive(t, []entry{
		{name: "target/", mode: 0o750, dir: true},
		{name: "target/changed.txt", mode: 0o755, body: "v2\n"},
		{name: "target/sub/", mode: 0o555, dir: true},
		{name: "target/sub/added.txt", mode: 0o4644, body: "added\n"},
		{name: "target/link", kind: tar.TypeSymlink, link: "../outside"},
		{name: "target/twin.txt", kind: tar.TypeLink, link: "target/changed.txt"},
	})))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"changed.txt": "v2\n", "sub": "/", "sub/added.txt": "added\n", "link": "-> ../outside", "twin.txt": "v2\n"}
	if got := readTree(t, folder); !reflect.DeepEqual(got, want) {
		t.Fatalf("tree %v, want %v", got, want)
	}
	for rel, want := range map[string]fs.FileMode{".": fs.ModeDir | 0o750, "changed.txt": 0o755, "sub": fs.ModeDir | 0o555, "sub/added.txt": 0o644, "twin.txt": 0o755} {
		if got := mode(t, filepath.Join(folder, rel)); got != want {
			t.Errorf("%s mode %v, want %v", rel, got, want)
		}
	}
	if err := os.Chmod(filepath.Join(folder, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(folder)); len(entries) != 1 {
		t.Fatalf("the copy left %d entries beside the folder", len(entries))
	}
}

// TestUnsafeArchiveEntriesAreRefused refuses each unsafe entry and leaves
// the folder as it was.
func TestUnsafeArchiveEntriesAreRefused(t *testing.T) {
	root := entry{name: "target/", mode: 0o755, dir: true}
	for name, bad := range map[string]entry{
		"absolute":            {name: "/etc/sample.txt", body: "x"},
		"parent":              {name: "target/../escaped.txt", body: "x"},
		"dot":                 {name: "target/./x.txt", body: "x"},
		"empty component":     {name: "target//x.txt", body: "x"},
		"another folder":      {name: "other/x.txt", body: "x"},
		"fifo":                {name: "target/pipe", kind: tar.TypeFifo},
		"device":              {name: "target/dev", kind: tar.TypeChar},
		"hard link outside":   {name: "target/twin", kind: tar.TypeLink, link: "../x"},
		"hard link to folder": {name: "target/twin", kind: tar.TypeLink, link: "target"},
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			folder := filepath.Join(base, "target")
			writeTree(t, folder, map[string]string{"kept.txt": "kept\n"})
			err := ReplaceFolder(folder, bytes.NewReader(archive(t, []entry{root, bad})))
			if !errors.Is(err, ErrUnsafeEntry) {
				t.Fatalf("error %v, want ErrUnsafeEntry", err)
			}
			if got := readTree(t, folder); !reflect.DeepEqual(got, map[string]string{"kept.txt": "kept\n"}) {
				t.Fatalf("a refused copy changed the folder: %v", got)
			}
			if entries, _ := os.ReadDir(base); len(entries) != 1 {
				t.Fatalf("the refused copy left %d entries", len(entries))
			}
		})
	}
	folder := filepath.Join(t.TempDir(), "target")
	writeTree(t, folder, nil)
	if err := ReplaceFolder(folder, bytes.NewReader(archive(t, []entry{{name: "target", body: "x"}}))); !errors.Is(err, ErrUnsafeEntry) {
		t.Fatalf("a file as the folder: %v", err)
	}
}

// TestACopyBackNeverWritesThroughALink writes an entry beneath a link the
// archive made: the write fails, and nothing outside the folder changes.
func TestACopyBackNeverWritesThroughALink(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, entries := range map[string][]entry{
		"a file beneath a linked folder": {{name: "target/up", kind: tar.TypeSymlink, link: outside}, {name: "target/up/planted.txt", body: "x"}},
		"a folder beneath it":            {{name: "target/up", kind: tar.TypeSymlink, link: outside}, {name: "target/up/planted/", dir: true, mode: 0o755}},
		"a file over a linked file":      {{name: "target/f", kind: tar.TypeSymlink, link: filepath.Join(outside, "file.txt")}, {name: "target/f", body: "x"}},
		"a hard link through a link":     {{name: "target/up", kind: tar.TypeSymlink, link: outside}, {name: "target/twin", kind: tar.TypeLink, link: "target/up/file.txt"}},
	} {
		t.Run(name, func(t *testing.T) {
			folder := filepath.Join(t.TempDir(), "target")
			writeTree(t, folder, map[string]string{"kept.txt": "kept\n"})
			all := append([]entry{{name: "target/", mode: 0o755, dir: true}}, entries...)
			if err := ReplaceFolder(folder, bytes.NewReader(archive(t, all))); err == nil {
				t.Fatal("the copy wrote through a link")
			}
			if got := readTree(t, outside); !reflect.DeepEqual(got, map[string]string{"file.txt": "outside\n"}) {
				t.Fatalf("outside the folder: %v", got)
			}
			if got := readTree(t, folder); !reflect.DeepEqual(got, map[string]string{"kept.txt": "kept\n"}) {
				t.Fatalf("a failed copy changed the folder: %v", got)
			}
		})
	}
}
