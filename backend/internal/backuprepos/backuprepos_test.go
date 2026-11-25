package backuprepos

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func host(t *testing.T) (*backuptest.Host, *Registry) {
	t.Helper()
	f := backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(f.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	return f, &Registry{Data: open(t), Auth: backup.AgentAuth(f.AgentPath, f.KnownHosts)}
}

// A registration checks its name, its URL and that Menagerie can write to
// the repository; a checked registration stores nothing and a saved one
// is listed.
func TestRegisterChecksTheRepositoryBeforeItRecordsIt(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	url := f.URL("backups.git")
	tried, err := r.Register(ctx, "  Example backups ", url, true)
	if err != nil || tried.ID != "example-backups" || tried.Name != "Example backups" {
		t.Fatalf("%+v %v", tried, err)
	}
	if list, _ := r.Data.BackupRepositories(ctx); len(list) != 0 {
		t.Fatalf("a checked registration was stored: %+v", list)
	}
	saved, err := r.Register(ctx, "Example backups", url, false)
	if err != nil || saved.ID != "example-backups" || saved.URL != url {
		t.Fatalf("%+v %v", saved, err)
	}
	if _, err := r.Register(ctx, "Again", url, true); !errors.Is(err, store.ErrBackupRepositoryExists) {
		t.Fatalf("%v", err)
	}
}

func TestRegisterRefusals(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, url, check string
	}{
		{"", f.URL("backups.git"), ""},
		{strings.Repeat("n", 101), f.URL("backups.git"), ""},
		{"Two\nlines", f.URL("backups.git"), ""},
		{"Local", filepath.Join(f.Dir, "backups.git"), CheckURL},
		{"File", "file://" + filepath.Join(f.Dir, "backups.git"), CheckURL},
		{"Plain", "http://git.example.invalid/example-org/backups.git", CheckURL},
		{"Spaced", " " + f.URL("backups.git"), CheckURL},
		{"Absent", f.URL("absent.git"), CheckWrite},
	} {
		_, err := r.Register(ctx, c.name, c.url, true)
		var check *CheckError
		var invalid *InvalidError
		switch {
		case c.check == "" && !errors.As(err, &invalid):
			t.Errorf("%q: %v", c.name, err)
		case c.check != "" && (!errors.As(err, &check) || check.Check != c.check):
			t.Errorf("%q: %v", c.name, err)
		}
	}
}

// A workspace backs up to the repository its data names, and one with
// none fails before any transfer.
func TestPushGoesToTheWorkspacesOwnRepository(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	repo, err := r.Register(ctx, "Example backups", f.URL("backups.git"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Data.SetWorkspaceBackupRepository(ctx, "alpha", repo.ID); err != nil {
		t.Fatal(err)
	}
	remote, err := r.For(ctx, "alpha")
	if err != nil || remote.URL != repo.URL {
		t.Fatalf("%+v %v", remote, err)
	}
	_, err = r.Push(ctx, t.TempDir(), "beta", nil)
	var none *NoRepositoryError
	if !errors.As(err, &none) || none.Workspace != "beta" {
		t.Fatalf("%v", err)
	}
}

// The import registers the configured repository once and gives it to
// every workspace that has none; a later start imports nothing, and an
// absent key registers nothing.
func TestImportTakesOverTheConfiguredRepositoryOnce(t *testing.T) {
	ctx := context.Background()
	data := open(t)
	if err := data.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := data.Created(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	url := "ssh://git@git.example.invalid/example-org/workspace-backups.git"
	if err := Import(ctx, data, url, logf); err != nil {
		t.Fatal(err)
	}
	list, err := data.BackupRepositories(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	if got := list[0]; got.Name != "example-org/workspace-backups" || got.URL != url || !reflect.DeepEqual(got.Workspaces, []string{"alpha"}) {
		t.Fatalf("%+v", got)
	}
	if len(logged) != 1 {
		t.Fatalf("%q", logged)
	}
	if err := data.BeginCreate(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	if err := Import(ctx, data, "ssh://git@git.example.invalid/example-org/other.git", logf); err != nil {
		t.Fatal(err)
	}
	if list, _ := data.BackupRepositories(ctx); len(list) != 1 {
		t.Fatalf("a second start imported again: %+v", list)
	}
	if _, ok, _ := data.WorkspaceBackupRepository(ctx, "beta"); ok {
		t.Fatal("a later workspace was given the imported repository")
	}

	fresh := open(t)
	if err := Import(ctx, fresh, "", logf); err != nil {
		t.Fatal(err)
	}
	if done, _ := fresh.Imported(ctx, ImportID); !done {
		t.Fatal("an import with no key recorded no mark")
	}
	if list, _ := fresh.BackupRepositories(ctx); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

func TestImportName(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:example-org/backups.git":             "example-org/backups",
		"https://github.com/example-org/backups":             "example-org/backups",
		"ssh://git@git.example.invalid:2222/srv/backups.git": "srv/backups",
		"ssh://git@git.example.invalid/":                     "Workspace backups",
	} {
		if got := ImportName(url); got != want {
			t.Errorf("%s: %q", url, got)
		}
	}
}
