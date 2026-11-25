package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func backupRepository(t *testing.T, s *Store, name, url string) BackupRepository {
	t.Helper()
	r, err := s.RegisterBackupRepository(context.Background(), BackupRepository{Name: name, URL: url})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A registration takes the id of its name, with -2 for a second of that
// name; a URL that is registered is refused with the registration it has,
// and a dry registration stores nothing.
func TestBackupRepositoriesTakeIDsFromTheirNamesAndOneRegistrationPerURL(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	first := backupRepository(t, s, "Example backups", "ssh://git@git.example.invalid/example-org/one.git")
	second := backupRepository(t, s, "Example backups", "ssh://git@git.example.invalid/example-org/two.git")
	if first.ID != "example-backups" || second.ID != "example-backups-2" {
		t.Fatalf("ids %q %q", first.ID, second.ID)
	}
	if none := backupRepository(t, s, "!!", "ssh://git@git.example.invalid/example-org/three.git"); none.ID != "backups" {
		t.Fatalf("id %q", none.ID)
	}
	_, err := s.RegisterBackupRepository(ctx, BackupRepository{Name: "Again", URL: first.URL})
	var exists *BackupRepositoryExistsError
	if !errors.As(err, &exists) || exists.ID != first.ID || !errors.Is(err, ErrBackupRepositoryExists) {
		t.Fatalf("%v", err)
	}
	tried, err := s.TryRegisterBackupRepository(ctx, BackupRepository{Name: "Dry", URL: "https://git.example.invalid/example-org/dry.git"})
	if err != nil || tried.ID != "dry" {
		t.Fatalf("%+v %v", tried, err)
	}
	if _, err := s.BackupRepository(ctx, "dry"); !errors.Is(err, ErrNoBackupRepository) {
		t.Fatalf("a dry registration was stored: %v", err)
	}
	list, err := s.BackupRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range list {
		ids = append(ids, r.ID)
	}
	if want := []string{"backups", "example-backups", "example-backups-2"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("%v", ids)
	}
}

// A create records its backup repository from its start, which keeps the
// repository registered while the create runs and after it; one no
// workspace backs up to can be removed.
func TestAWorkspaceKeepsItsBackupRepositoryRegistered(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	used := backupRepository(t, s, "Used", "ssh://git@git.example.invalid/example-org/used.git")
	idle := backupRepository(t, s, "Idle", "ssh://git@git.example.invalid/example-org/idle.git")
	if err := s.BeginCreateFrom(ctx, "alpha", "", used.ID); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.WorkspaceBackupRepository(ctx, "alpha")
	if err != nil || !ok || got.ID != used.ID || !reflect.DeepEqual(got.Workspaces, []string{"alpha"}) {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	err = s.RemoveBackupRepository(ctx, used.ID)
	var inUse *BackupRepositoryInUseError
	if !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.Workspaces, []string{"alpha"}) {
		t.Fatalf("%v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM backup_repositories WHERE id = ?`, used.ID); err == nil {
		t.Fatal("the database removed a backup repository a workspace backs up to")
	}
	if err := s.RemoveBackupRepository(ctx, idle.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveBackupRepository(ctx, idle.ID); !errors.Is(err, ErrNoBackupRepository) {
		t.Fatalf("%v", err)
	}
}

// A workspace with no recorded repository has none; setting one needs a
// registered repository; adopting gives a repository only to workspaces
// that have none.
func TestSettingAndAdoptingBackupRepositories(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.WorkspaceBackupRepository(ctx, "alpha"); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, ok, err := s.WorkspaceBackupRepository(ctx, "absent"); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	if err := s.SetWorkspaceBackupRepository(ctx, "alpha", "unregistered"); !errors.Is(err, ErrNoBackupRepository) {
		t.Fatalf("%v", err)
	}
	one := backupRepository(t, s, "One", "ssh://git@git.example.invalid/example-org/one.git")
	two := backupRepository(t, s, "Two", "ssh://git@git.example.invalid/example-org/two.git")
	if err := s.SetWorkspaceBackupRepository(ctx, "beta", two.ID); err != nil {
		t.Fatal(err)
	}
	var adopted int64
	if err := s.Update(ctx, func(tx *Tx) (err error) { adopted, err = tx.AdoptBackupRepository(one.ID); return err }); err != nil {
		t.Fatal(err)
	}
	if adopted != 1 {
		t.Fatalf("adopted %d", adopted)
	}
	all, err := s.WorkspaceBackupRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all["alpha"].ID != one.ID || all["beta"].ID != two.ID || len(all) != 2 {
		t.Fatalf("%+v", all)
	}
}

// A removed repository's id is never given to another registration, so a
// request that names it cannot reach the repository registered after; its
// URL can be registered again.
func TestARemovedBackupRepositorysIDIsNeverReused(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	first := backupRepository(t, s, "Other", "ssh://git@git.example.invalid/example-org/first.git")
	if err := s.RemoveBackupRepository(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackupRepository(ctx, first.ID); !errors.Is(err, ErrNoBackupRepository) {
		t.Fatalf("%v", err)
	}
	again := backupRepository(t, s, "Other", "ssh://git@git.example.invalid/example-org/again.git")
	if again.ID == first.ID {
		t.Fatalf("the removed id %q was reused", first.ID)
	}
	if err := s.SetWorkspaceBackupRepository(ctx, "alpha", first.ID); !errors.Is(err, ErrNoBackupRepository) {
		t.Fatalf("%v", err)
	}
	same := backupRepository(t, s, "First again", first.URL)
	if same.ID == first.ID {
		t.Fatalf("%+v", same)
	}
	list, _ := s.BackupRepositories(ctx)
	if len(list) != 2 {
		t.Fatalf("%+v", list)
	}
}

// A create that names a repository removed since it was read is refused in
// the transaction that would record it.
func TestACreateRefusesARemovedBackupRepository(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	r := backupRepository(t, s, "Other", "ssh://git@git.example.invalid/example-org/other.git")
	if err := s.RemoveBackupRepository(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginCreateFrom(ctx, "alpha", "", r.ID); !errors.Is(err, ErrNoBackupRepository) {
		t.Fatalf("%v", err)
	}
	if exists, _ := s.Exists(ctx, "alpha"); exists {
		t.Fatal("a refused create left data")
	}
}
