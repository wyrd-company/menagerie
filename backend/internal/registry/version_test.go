package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// The template service's version rules (template-service/src/versions.rs),
// case for case.
func TestAVersionIsAReleaseTag(t *testing.T) {
	for tag, want := range map[string]*Version{
		"1.2.3":                    {1, 2, 3},
		"v1.2.3":                   {1, 2, 3},
		"0.0.0":                    {0, 0, 0},
		"10.20.30":                 {10, 20, 30},
		"1.2.3-rc.1":               nil,
		"1.2.3+build":              nil,
		"v1.2":                     nil,
		"1":                        nil,
		"v1":                       nil,
		"01.2.3":                   nil,
		"1.02.3":                   nil,
		"1.2.03":                   nil,
		"1.2.3.4":                  nil,
		"V1.2.3":                   nil,
		"vv1.2.3":                  nil,
		"1..3":                     nil,
		"":                         nil,
		"main":                     nil,
		"18446744073709551616.0.0": nil,
		"+1.2.3":                   nil,
	} {
		got, ok := ParseVersion(tag)
		if want == nil {
			if ok {
				t.Errorf("%q is a version %v", tag, got)
			}
			continue
		}
		if !ok || got != *want {
			t.Errorf("%q: %v %v, want %v", tag, got, ok, *want)
		}
	}
}

func TestVersionsOrderByTheirNumbers(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		newer bool
	}{
		{"1.10.0", "1.2.0", true},
		{"2.0.0", "1.99.99", true},
		{"1.2.0", "v1.2.0", false},
		{"1.2.0", "1.2.1", false},
		{"1.2.1", "1.2.0", true},
		{"main", "1.0.0", false},
		{"1.0.0", "main", false},
	} {
		if got := Newer(c.a, c.b); got != c.newer {
			t.Errorf("Newer(%q, %q) = %t", c.a, c.b, got)
		}
	}
}

func TestTheAppliedRecordGivesItsRootsSourceRefAndCommit(t *testing.T) {
	folder := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(folder, RecordName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"applications":[
		{"target":"sub","template":"gh:o/other@9.9.9","source":"gh:o/other","commit":null},
		{"target":".","template":"gh:o/kit@v1.2.0#templates/kit","source":"gh:o/kit#templates/kit","commit":"0123456789abcdef0123456789abcdef01234567"}]}`)
	got, err := ReadApplied(folder)
	want := Applied{Source: "gh:o/kit#templates/kit", Ref: "v1.2.0", Commit: "0123456789abcdef0123456789abcdef01234567"}
	if err != nil || got != want {
		t.Fatalf("%+v, %v", got, err)
	}
	write(`{"applications":[{"target":".","template":"ssh://git.example.invalid/o/kit@main","source":"ssh://git.example.invalid/o/kit","commit":null}]}`)
	if got, err := ReadApplied(folder); err != nil || got.Ref != "main" || got.Commit != "" {
		t.Fatalf("%+v, %v", got, err)
	}
	for _, bad := range []string{`not json`, `{"applications":[]}`, `{"applications":[{"target":"sub","source":"s"}]}`} {
		write(bad)
		if _, err := ReadApplied(folder); err == nil {
			t.Errorf("%s was read", bad)
		}
		if Source(folder) != "" {
			t.Errorf("%s has a source", bad)
		}
	}
	// A link in place of the record is not read.
	other := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(other, []byte(`{"applications":[{"target":".","template":"gh:o/kit@1.0.0","source":"gh:o/kit","commit":null}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(folder, RecordName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(folder, RecordName)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadApplied(folder); err == nil {
		t.Error("a linked record was read")
	}
}
