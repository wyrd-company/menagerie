package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
)

// gitAdd stages paths through go-git, as someone working in the
// workspace would.
func gitAdd(t *testing.T, folder string, paths ...string) {
	t.Helper()
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if _, err := w.Add(p); err != nil {
			t.Fatal(err)
		}
	}
}

func treeOf(t *testing.T, folder string, c Commit) []string {
	t.Helper()
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	return tree(t, repo, c.Hash)
}

func TestAFileAndAFolderCanReplaceEachOther(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(t *testing.T, folder string)
		want   []string
	}{
		"file to folder": {
			change: func(t *testing.T, folder string) {
				if err := os.Remove(filepath.Join(folder, "result.txt")); err != nil {
					t.Fatal(err)
				}
				write(t, folder, "result.txt/child", "now a folder\n")
			},
			want: []string{".gitignore", ".toha.json", "hook-marker.txt", "result.txt/child", "sub/kept.txt"},
		},
		"folder to file": {
			change: func(t *testing.T, folder string) {
				if err := os.RemoveAll(filepath.Join(folder, "sub")); err != nil {
					t.Fatal(err)
				}
				write(t, folder, "sub", "now a file\n")
			},
			want: []string{".gitignore", ".toha.json", "hook-marker.txt", "result.txt", "sub"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			folder := committed(t, "alpha")
			tc.change(t, folder)
			if !pendingOf(t, folder, "alpha") {
				t.Fatal("change not pending")
			}
			c, err := CommitAll(folder, "alpha", "chore: record", &bytes.Buffer{})
			if err != nil || !c.Changed {
				t.Fatalf("commit %+v %v", c, err)
			}
			if got := treeOf(t, folder, c); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tree %v, want %v", got, tc.want)
			}
			if pendingOf(t, folder, "alpha") {
				t.Fatal("still pending after the commit")
			}
		})
	}
}

func TestAnUpgradeCanReplaceAFileWithAFolder(t *testing.T) {
	folder := committed(t, "alpha")
	if err := os.RemoveAll(filepath.Join(folder, "sub")); err != nil {
		t.Fatal(err)
	}
	write(t, folder, "sub", "now a file\n")
	if err := os.Remove(filepath.Join(folder, "result.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, folder, "result.txt/child", "now a folder\n")
	out := Output{Reported: []string{"sub", "result.txt/child"}, Record: ".toha.json"}
	c, err := CommitUpgrade(folder, "alpha", out, "Upgrade", &bytes.Buffer{})
	if err != nil || !c.Changed {
		t.Fatalf("commit %+v %v", c, err)
	}
	want := []string{".gitignore", ".toha.json", "hook-marker.txt", "result.txt/child", "sub"}
	if got := treeOf(t, folder, c); !reflect.DeepEqual(got, want) {
		t.Fatalf("tree %v, want %v", got, want)
	}
	if pendingOf(t, folder, "alpha") {
		t.Fatal("still pending after the upgrade commit")
	}
}

func setExcludesFile(t *testing.T, folder, value string) {
	t.Helper()
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Raw.Section("core").SetOption("excludesFile", value)
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestTheRepositorysOwnExcludeFilesAreHonoured(t *testing.T) {
	for name, tc := range map[string]func(t *testing.T, folder string){
		"info/exclude": func(t *testing.T, folder string) {
			write(t, folder, ".git/info/exclude", "# local\nsecret.txt\n")
		},
		"relative excludesFile": func(t *testing.T, folder string) {
			write(t, folder, "tools/excludes", "secret.txt\n")
			setExcludesFile(t, folder, "tools/excludes")
		},
		"absolute excludesFile": func(t *testing.T, folder string) {
			write(t, folder, ".git/excludes", "secret.txt\n")
			setExcludesFile(t, folder, filepath.Join(folder, ".git", "excludes"))
		},
		"excludesFile linked inside": func(t *testing.T, folder string) {
			write(t, folder, "tools/excludes", "secret.txt\n")
			if err := os.Symlink("tools/excludes", filepath.Join(folder, ".excludes")); err != nil {
				t.Fatal(err)
			}
			setExcludesFile(t, folder, ".excludes")
		},
	} {
		t.Run(name, func(t *testing.T) {
			folder := committed(t, "alpha")
			tc(t, folder)
			write(t, folder, "secret.txt", "excluded\n")
			var log bytes.Buffer
			c, err := CommitAll(folder, "alpha", "chore: record", &log)
			if err != nil {
				t.Fatal(err)
			}
			if files := strings.Join(treeOf(t, folder, c), " "); strings.Contains(files, "secret.txt") {
				t.Fatalf("excluded file committed: %s", files)
			}
			if strings.Contains(log.String(), "not used") {
				t.Fatalf("an exclude file inside the workspace was refused: %q", log.String())
			}
		})
	}
}

func TestAnExcludeFileOutsideTheWorkspaceIsNotRead(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-excludes")
	if err := os.WriteFile(outside, []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		set  func(t *testing.T, folder string)
		note string
	}{
		"absolute path": {func(t *testing.T, folder string) { setExcludesFile(t, folder, outside) }, noteExcludesFile},
		"relative escape": {func(t *testing.T, folder string) {
			rel, err := filepath.Rel(folder, outside)
			if err != nil {
				t.Fatal(err)
			}
			setExcludesFile(t, folder, rel)
		}, noteExcludesFile},
		"link out": {func(t *testing.T, folder string) {
			if err := os.Symlink(outside, filepath.Join(folder, ".excludes")); err != nil {
				t.Fatal(err)
			}
			setExcludesFile(t, folder, ".excludes")
		}, noteExcludesFile},
		"home": {func(t *testing.T, folder string) { setExcludesFile(t, folder, "~/.excludes") }, noteExcludesFile},
		"not a file": {func(t *testing.T, folder string) {
			if err := os.Mkdir(filepath.Join(folder, "tools"), 0o755); err != nil {
				t.Fatal(err)
			}
			setExcludesFile(t, folder, "tools")
		}, noteExcludesFile},
		"named pipe": {func(t *testing.T, folder string) {
			if err := syscall.Mkfifo(filepath.Join(folder, ".excludes"), 0o644); err != nil {
				t.Fatal(err)
			}
			setExcludesFile(t, folder, ".excludes")
		}, noteExcludesFile},
		"too large": {func(t *testing.T, folder string) {
			write(t, folder, ".excludes", "secret.txt\n"+strings.Repeat("#\n", maxExcludeFile))
			setExcludesFile(t, folder, ".excludes")
		}, noteExcludesFile},
		"info/exclude linked out": {func(t *testing.T, folder string) {
			info := filepath.Join(folder, ".git", "info")
			if err := os.MkdirAll(info, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(filepath.Join(info, "exclude"))
			if err := os.Symlink(outside, filepath.Join(info, "exclude")); err != nil {
				t.Fatal(err)
			}
		}, noteInfoExclude},
	} {
		t.Run(name, func(t *testing.T) {
			folder := committed(t, "alpha")
			tc.set(t, folder)
			write(t, folder, "secret.txt", "not excluded\n")
			var log bytes.Buffer
			var c Commit
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, err = CommitAll(folder, "alpha", "chore: record", &log)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("reading the exclude file blocked")
			}
			if err != nil || !c.Changed {
				t.Fatalf("commit %+v %v", c, err)
			}
			if files := strings.Join(treeOf(t, folder, c), " "); !strings.Contains(files, "secret.txt") {
				t.Fatalf("the outside file's rules were used: %s", files)
			}
			if !strings.Contains(log.String(), tc.note) {
				t.Fatalf("log %q lacks the note", log.String())
			}
			if strings.Contains(log.String(), outside) || strings.Contains(log.String(), "outside-excludes") {
				t.Fatalf("log names the path: %q", log.String())
			}
		})
	}
}

func TestMenageriesOwnGitConfigurationIsNeverRead(t *testing.T) {
	folder := committed(t, "alpha")
	home := t.TempDir()
	write(t, folder, "tools/excludes", "secret.txt\n")
	global := "[core]\n\texcludesFile = " + filepath.Join(folder, "tools", "excludes") + "\n"
	write(t, home, ".gitconfig", global)
	write(t, home, ".config/git/config", global)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	write(t, folder, "secret.txt", "not excluded by the workspace\n")
	c, err := CommitAll(folder, "alpha", "chore: record", &bytes.Buffer{})
	if err != nil || !c.Changed {
		t.Fatalf("commit %+v %v", c, err)
	}
	if files := strings.Join(treeOf(t, folder, c), " "); !strings.Contains(files, "secret.txt") {
		t.Fatalf("Menagerie's own configuration excluded a file: %s", files)
	}
}

// workspaceStates are changes after which CommitAll does or does not
// commit; Pending must say the same beforehand.
var workspaceStates = map[string]func(t *testing.T, folder string){
	"clean": func(t *testing.T, folder string) {},
	"edited": func(t *testing.T, folder string) {
		write(t, folder, "result.txt", "label: second\n")
	},
	"new file": func(t *testing.T, folder string) {
		write(t, folder, "new.txt", "new\n")
	},
	"deleted": func(t *testing.T, folder string) {
		if err := os.Remove(filepath.Join(folder, "result.txt")); err != nil {
			t.Fatal(err)
		}
	},
	"new file staged then deleted": func(t *testing.T, folder string) {
		write(t, folder, "new.txt", "new\n")
		gitAdd(t, folder, "new.txt")
		if err := os.Remove(filepath.Join(folder, "new.txt")); err != nil {
			t.Fatal(err)
		}
	},
	"edit staged then reverted": func(t *testing.T, folder string) {
		write(t, folder, "result.txt", "label: second\n")
		gitAdd(t, folder, "result.txt")
		write(t, folder, "result.txt", "label: first\n")
	},
	"edit staged": func(t *testing.T, folder string) {
		write(t, folder, "result.txt", "label: second\n")
		gitAdd(t, folder, "result.txt")
	},
	"made executable": func(t *testing.T, folder string) {
		if err := os.Chmod(filepath.Join(folder, "result.txt"), 0o755); err != nil {
			t.Fatal(err)
		}
	},
	"only ignored and nested": func(t *testing.T, folder string) {
		write(t, folder, "cache/more", "ignored\n")
		write(t, folder, "clone/inner.txt", "nested\n")
	},
	"symbolic link": func(t *testing.T, folder string) {
		if err := os.Symlink("result.txt", filepath.Join(folder, "link")); err != nil {
			t.Fatal(err)
		}
	},
	"file to folder": func(t *testing.T, folder string) {
		if err := os.Remove(filepath.Join(folder, "result.txt")); err != nil {
			t.Fatal(err)
		}
		write(t, folder, "result.txt/child", "x\n")
	},
	"rewritten unchanged": func(t *testing.T, folder string) {
		write(t, folder, "result.txt", "label: first\n")
	},
}

func TestPendingIsTrueExactlyWhenCommitAllCommits(t *testing.T) {
	want := map[string]bool{
		"clean": false, "new file staged then deleted": false, "edit staged then reverted": false,
		"only ignored and nested": false, "rewritten unchanged": false,
	}
	for name, change := range workspaceStates {
		t.Run(name, func(t *testing.T) {
			folder := committed(t, "alpha")
			change(t, folder)
			pending := pendingOf(t, folder, "alpha")
			c, err := CommitAll(folder, "alpha", "chore: record", &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if pending != c.Changed {
				t.Fatalf("Pending %v, CommitAll changed %v", pending, c.Changed)
			}
			if expected, listed := want[name]; listed && pending != expected {
				t.Fatalf("Pending %v, want %v", pending, expected)
			}
			if pendingOf(t, folder, "alpha") {
				t.Fatal("pending after CommitAll")
			}
		})
	}
}

func TestPendingWritesNothing(t *testing.T) {
	folder := committed(t, "alpha")
	write(t, folder, "new.txt", "new\n")
	index := filepath.Join(folder, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	objects := func() int {
		n := 0
		_ = filepath.WalkDir(filepath.Join(folder, ".git", "objects"), func(_ string, d os.DirEntry, _ error) error {
			if d != nil && !d.IsDir() {
				n++
			}
			return nil
		})
		return n
	}
	count := objects()
	if !pendingOf(t, folder, "alpha") {
		t.Fatal("not pending")
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || objects() != count {
		t.Fatal("Pending wrote the index or an object")
	}
}
