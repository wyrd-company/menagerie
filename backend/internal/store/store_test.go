package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesAPrivateDatabaseAtTheSchemaVersion(t *testing.T) {
	s := open(t)
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("database mode %v", mode)
	}
	folder, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if mode := folder.Mode().Perm(); mode != 0o700 {
		t.Errorf("folder mode %v", mode)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version %d, %v", version, err)
	}
	var keys int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("foreign keys %d, %v", keys, err)
	}
}

func TestDataOutlivesTheProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "menagerie.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.HoldName(ctx, "alpha", "container-alpha", "host-a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	held, err := s.Held(ctx, "alpha")
	if err != nil || !slices.Equal(held.Names, []Name{{Name: "container-alpha", Agent: "host-a", Current: true}}) {
		t.Fatalf("held %+v, %v", held, err)
	}
}

func TestANewerSchemaIsRefusedAndAnOlderOneUpgraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "menagerie.db")
	// An empty database is version 0, as a release before the schema had.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("an older schema: %v", err)
	}
	_ = s.Close()

	db, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(path); !errors.Is(err, ErrNewer) {
		t.Fatalf("a newer schema opened: %v", err)
	}
}

func TestANameOrKeyBelongsToOneWorkspace(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.HoldName(ctx, "alpha", "shared", "host-a", true); err != nil {
		t.Fatal(err)
	}
	err := s.HoldName(ctx, "beta", "shared", "host-a", true)
	var taken *TakenError
	if !errors.Is(err, ErrTaken) || !errors.As(err, &taken) || taken.Holder != "alpha" {
		t.Fatalf("a name another workspace holds: %v", err)
	}
	key := Key{Policy: "host-sample", Name: "alpha"}
	if err := s.HoldKey(ctx, "alpha", key); err != nil {
		t.Fatal(err)
	}
	if err := s.HoldKey(ctx, "alpha", key); err != nil {
		t.Fatalf("a repeat: %v", err)
	}
	if err := s.HoldKey(ctx, "beta", key); !errors.Is(err, ErrTaken) {
		t.Fatalf("a key another workspace holds: %v", err)
	}
	// The constraint holds without the check, too.
	err = s.Update(ctx, func(tx *Tx) error {
		_, err := tx.exec(`INSERT INTO container_names (name, workspace) VALUES ('shared', 'beta')`)
		return err
	})
	if err == nil {
		t.Fatal("the schema took a name twice")
	}
	if exists, _ := s.Exists(ctx, "beta"); exists {
		t.Fatal("a refused name gave the workspace data")
	}
}

func TestOneNameIsCurrent(t *testing.T) {
	s, ctx := open(t), context.Background()
	for _, name := range []string{"first", "second"} {
		if err := s.HoldName(ctx, "alpha", name, "host-a", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.HoldName(ctx, "alpha", "retired", "", false); err != nil {
		t.Fatal(err)
	}
	held, err := s.Held(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := []Name{{Name: "first", Agent: "host-a"}, {Name: "retired"}, {Name: "second", Agent: "host-a", Current: true}}
	if !slices.Equal(held.Names, want) || held.Current() != "second" {
		t.Fatalf("held %+v", held.Names)
	}
	if err := s.ForgetName(ctx, "alpha", "second"); err != nil {
		t.Fatal(err)
	}
	if held, _ := s.Held(ctx, "alpha"); held.Current() != "" {
		t.Fatalf("a forgotten name stays current: %+v", held)
	}
}

func TestAWorkspaceThatHoldsSomethingKeepsItsData(t *testing.T) {
	s, ctx := open(t), context.Background()
	key := Key{Policy: "host-sample", Name: "alpha"}
	if err := s.HoldKey(ctx, "alpha", key); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveWorkspace(ctx, "alpha"); err == nil {
		t.Fatal("the data of a workspace that holds a key was removed")
	}
	if err := s.ForgetKey(ctx, "alpha", key); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveWorkspace(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := s.Exists(ctx, "alpha"); exists {
		t.Fatal("the workspace stays")
	}
}

func TestTheRemovingStateIsTheWorkspaces(t *testing.T) {
	s, ctx := open(t), context.Background()
	if removing, err := s.Removing(ctx, "alpha"); err != nil || removing != "" {
		t.Fatalf("removing %q, %v", removing, err)
	}
	if err := s.SetRemoving(ctx, "alpha", "1:2"); err != nil {
		t.Fatal(err)
	}
	if removing, _ := s.Removing(ctx, "alpha"); removing != "1:2" {
		t.Fatalf("removing %q", removing)
	}
	if err := s.SetRemoving(ctx, "alpha", ""); err != nil {
		t.Fatal(err)
	}
	if removing, _ := s.Removing(ctx, "alpha"); removing != "" {
		t.Fatalf("removing %q", removing)
	}
}

func TestACreateIsKeptUntilItsFolderOrItsEnd(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginCreate(ctx, "alpha"); !errors.Is(err, ErrExists) {
		t.Fatalf("a second create: %v", err)
	}
	// Its folder does not exist while the interview waits, and the list keeps
	// it without listing it.
	noFolder, err := s.Discover(ctx, nil)
	if err != nil || len(noFolder) != 0 {
		t.Fatalf("no-folder %v, %v", noFolder, err)
	}
	if exists, _ := s.Exists(ctx, "alpha"); !exists {
		t.Fatal("the list removed a create's data")
	}
	if err := s.AbandonCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := s.Exists(ctx, "alpha"); exists {
		t.Fatal("an abandoned create keeps its data")
	}

	if err := s.BeginCreate(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	if err := s.Created(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	if err := s.AbandonCreate(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := s.Exists(ctx, "beta"); !exists {
		t.Fatal("a create that made its folder lost its data")
	}
}

func TestAStartEndsTheCreatesARestartEnded(t *testing.T) {
	s, ctx := open(t), context.Background()
	for _, name := range []string{"made", "unmade"} {
		if err := s.BeginCreate(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.EndCreates(ctx, func(name string) bool { return name == "made" })
	if err != nil || !slices.Equal(removed, []string{"unmade"}) {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if exists, _ := s.Exists(ctx, "unmade"); exists {
		t.Fatal("a create with no folder keeps its data")
	}
	// The mark is gone: with its folder gone too, the list removes it.
	if _, err := s.Discover(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if exists, _ := s.Exists(ctx, "made"); exists {
		t.Fatal("the mark stayed")
	}
}

func TestDiscoverJoinsTheFoldersToTheData(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.HoldName(ctx, "gone-bound", "container-gone", "host-a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.HoldKey(ctx, "gone-keyed", Key{Policy: "host-sample", Name: "gone-keyed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(ctx, []string{"gone-empty"}); err != nil {
		t.Fatal(err)
	}
	noFolder, err := s.Discover(ctx, []string{"alpha"})
	if err != nil || !slices.Equal(noFolder, []string{"gone-bound", "gone-keyed"}) {
		t.Fatalf("no-folder %v, %v", noFolder, err)
	}
	for name, want := range map[string]bool{"alpha": true, "gone-bound": true, "gone-keyed": true, "gone-empty": false} {
		if exists, _ := s.Exists(ctx, name); exists != want {
			t.Errorf("%s exists %t", name, exists)
		}
	}
}

func TestTheAgentIsRecordedOnTheNamesWithout(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.HoldName(ctx, "alpha", "imported", "", true); err != nil {
		t.Fatal(err)
	}
	if err := s.HoldName(ctx, "alpha", "bound", "host-b", false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RecordAgent(ctx, "host-a"); err != nil || n != 1 {
		t.Fatalf("recorded %d, %v", n, err)
	}
	held, _ := s.Held(ctx, "alpha")
	want := []Name{{Name: "bound", Agent: "host-b"}, {Name: "imported", Agent: "host-a", Current: true}}
	if !slices.Equal(held.Names, want) {
		t.Fatalf("held %+v", held.Names)
	}
}

func TestAnImportRunsOnceAndWholeOrNotAtAll(t *testing.T) {
	s, ctx := open(t), context.Background()
	failed := errors.New("the import failed")
	ran, err := s.Import(ctx, 1, func(tx *Tx) error {
		if err := tx.AddWorkspace("alpha"); err != nil {
			return err
		}
		return failed
	})
	if ran || !errors.Is(err, failed) {
		t.Fatalf("ran %t, %v", ran, err)
	}
	if exists, _ := s.Exists(ctx, "alpha"); exists {
		t.Fatal("a failed import kept a change")
	}
	calls := 0
	for start := range 2 {
		ran, err := s.Import(ctx, 1, func(tx *Tx) error { calls++; return tx.AddWorkspace("alpha") })
		if err != nil || ran != (start == 0) {
			t.Fatalf("start %d ran %t, %v", start, ran, err)
		}
	}
	if calls != 1 {
		t.Fatalf("the import ran %d times", calls)
	}
}
