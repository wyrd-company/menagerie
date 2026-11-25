package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func registered(t *testing.T, s *Store, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := s.RegisterTemplate(context.Background(), Template{Name: name, Address: "gh:example-org/" + name,
			Source: "gh:example-org/" + name, CurrentVersion: "1.0.0", CurrentTag: "1.0.0"}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// The list is by name without regard to case, then by name and id: beta
// takes the id beta and Beta, registered later, beta-2, yet Beta lists
// first.
func TestTemplatesListByNameWithoutRegardToCase(t *testing.T) {
	s := open(t)
	registered(t, s, "Zeta", "alpha", "beta", "Beta")
	list, err := s.Templates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tpl := range list {
		names = append(names, tpl.Name)
	}
	if want := []string{"alpha", "Beta", "beta", "Zeta"}; len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] || names[3] != want[3] {
		t.Fatalf("%v", names)
	}
}

// The foreign key keeps a template a workspace uses, beside the store's
// own check, whatever deletes it.
func TestTheForeignKeyKeepsATemplateAWorkspaceUses(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	registered(t, s, "Kit")
	if err := s.BeginCreateFrom(ctx, "alpha", "kit", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM templates WHERE id = 'kit'`); err == nil {
		t.Fatal("the database removed a template a workspace uses")
	}
	if _, err := s.Template(ctx, "kit"); err != nil {
		t.Fatal(err)
	}
}

// AdvanceTemplate refuses, inside its transaction, a version that is not
// newer than the current one, as a template upgraded meanwhile has.
func TestAdvanceTemplateMovesOnlyForwardInItsTransaction(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	registered(t, s, "Kit")
	newer := func(a, b string) bool { return a > b }
	for _, advance := range []func(context.Context, string, string, string, string, func(string, string) bool) (Template, error){s.AdvanceTemplate, s.TryAdvanceTemplate} {
		if _, err := advance(ctx, "kit", "1.0.0", "1.0.0", "c", newer); !errors.Is(err, ErrNotNewer) {
			t.Fatalf("%v", err)
		}
	}
	if tpl, err := s.AdvanceTemplate(ctx, "kit", "1.1.0", "1.1.0", "c", newer); err != nil || tpl.CurrentVersion != "1.1.0" {
		t.Fatalf("%+v %v", tpl, err)
	}
}

// A database of schema 4, which a release with registered templates and no
// version checks wrote, upgrades to the current schema with its templates
// and workspaces kept: no found version, no check, and each workspace's
// applied version unknown until the applied versions' import.
func TestSchemaFourUpgradesKeepingTemplatesAndWorkspaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "menagerie.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:4] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 4`,
		`INSERT INTO templates (id, name, address, source, current_version, current_tag, current_commit) VALUES ('kit', 'Kit', 'gh:example-org/kit', 'gh:example-org/kit', '1.0.0', '1.0.0', 'c100')`,
		`INSERT INTO workspaces (name, template) VALUES ('alpha', 'kit')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	tpl, err := s.Template(ctx, "kit")
	if err != nil || tpl.CurrentCommit != "c100" || len(tpl.Versions) != 0 || !tpl.CheckedAt.IsZero() || tpl.CheckFailure != "" ||
		!reflect.DeepEqual(tpl.Workspaces, []string{"alpha"}) {
		t.Fatalf("%+v %v", tpl, err)
	}
	if applied, err := s.AppliedVersion(ctx, "alpha"); err != nil || applied != (AppliedVersion{}) {
		t.Fatalf("%+v %v", applied, err)
	}
}

// A check's found versions keep their order, the newest last; a removed
// template takes them with it, and a check of a removed template is
// ErrNoTemplate.
func TestFoundVersionsKeepTheirOrderAndGoWithTheirTemplate(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	registered(t, s, "Kit")
	found := []Version{{"1.0.0", "1.0.0", "c100"}, {"1.10.0", "v1.10.0", "c110"}, {"2.0.0", "2.0.0", "c200"}}
	at := time.Date(2026, 11, 18, 9, 30, 0, 123, time.UTC)
	tpl, err := s.RecordCheck(ctx, "kit", found, at)
	if err != nil || !reflect.DeepEqual(tpl.Versions, found) || tpl.Newest() != "2.0.0" || !tpl.CheckedAt.Equal(at) {
		t.Fatalf("%+v %v", tpl, err)
	}
	if err := s.RemoveTemplate(ctx, "kit"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM template_versions`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d found versions outlived their template: %v", left, err)
	}
	if _, err := s.RecordCheck(ctx, "kit", found, at); !errors.Is(err, ErrNoTemplate) {
		t.Fatalf("%v", err)
	}
	if _, err := s.RecordCheckFailure(ctx, "kit", CheckServiceUnavailable, at); !errors.Is(err, ErrNoTemplate) {
		t.Fatalf("%v", err)
	}
}

// A failed check keeps the found versions and records its time and kind;
// the data refuses a kind that is not a check's.
func TestAFailedCheckKeepsTheFoundVersions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	registered(t, s, "Kit")
	found := []Version{{"1.0.0", "1.0.0", "c100"}}
	if _, err := s.RecordCheck(ctx, "kit", found, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	tpl, err := s.RecordCheckFailure(ctx, "kit", CheckTemplateNotFound, time.Unix(200, 0))
	if err != nil || !reflect.DeepEqual(tpl.Versions, found) || tpl.CheckFailure != CheckTemplateNotFound || !tpl.CheckedAt.Equal(time.Unix(200, 0)) {
		t.Fatalf("%+v %v", tpl, err)
	}
	if _, err := s.RecordCheckFailure(ctx, "kit", "internal-error", time.Unix(300, 0)); err == nil {
		t.Fatal("the data took a failure that is no check's")
	}
}

// The current version is moved while a check found its tag at another
// commit; AdvanceTemplate takes the current version again only then, with
// the new commit, and never an older one.
func TestAMovedCurrentVersionIsTakenAgainOnlyWhileMoved(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.RegisterTemplate(ctx, Template{Name: "Kit", Address: "gh:example-org/kit", Source: "gh:example-org/kit",
		CurrentVersion: "1.1.0", CurrentTag: "1.1.0", CurrentCommit: "c110"}, nil); err != nil {
		t.Fatal(err)
	}
	newer := func(a, b string) bool { return a > b }
	if _, err := s.AdvanceTemplate(ctx, "kit", "1.1.0", "1.1.0", "c11b", newer); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("taken again while not moved: %v", err)
	}
	tpl, err := s.RecordCheck(ctx, "kit", []Version{{"1.0.0", "1.0.0", "c100"}, {"1.1.0", "1.1.0", "c11b"}}, time.Unix(1, 0))
	if err != nil || !tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
	if _, err := s.AdvanceTemplate(ctx, "kit", "1.0.0", "1.0.0", "c100", newer); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("an older version while moved: %v", err)
	}
	tpl, err = s.AdvanceTemplate(ctx, "kit", "1.1.0", "1.1.0", "c11b", newer)
	if err != nil || tpl.CurrentCommit != "c11b" || tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
}

// An imported template, with no current commit, is never moved; its first
// check records the commit of its current version's found tag.
func TestAnImportedTemplateTakesItsCommitFromItsFirstCheck(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	registered(t, s, "Kit")
	tpl, err := s.RecordCheck(ctx, "kit", []Version{{"1.0.0", "v1.0.0", "c100"}}, time.Unix(1, 0))
	if err != nil || tpl.CurrentCommit != "c100" || tpl.CurrentTag != "v1.0.0" || tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
	// Later checks never replace a recorded commit: a moved tag shows.
	tpl, err = s.RecordCheck(ctx, "kit", []Version{{"1.0.0", "1.0.0", "c10b"}}, time.Unix(2, 0))
	if err != nil || tpl.CurrentCommit != "c100" || !tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
}

// A workspace's applied version is recorded with its commit; an unknown
// one takes the newest found version whose tag is at its commit at the
// template's next check.
func TestAnUnknownAppliedVersionIsCompletedFromItsCommit(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	registered(t, s, "Kit")
	if err := s.BeginCreateFrom(ctx, "alpha", "kit", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppliedVersion(ctx, "alpha", AppliedVersion{Commit: "c100"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppliedVersion(ctx, "alpha"); got != (AppliedVersion{Commit: "c100"}) {
		t.Fatalf("%+v", got)
	}
	if _, err := s.RecordCheck(ctx, "kit", []Version{{"1.0.0", "1.0.0", "c100"}, {"1.0.1", "1.0.1", "c100"}}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppliedVersion(ctx, "alpha"); got != (AppliedVersion{Version: "1.0.1", Commit: "c100"}) {
		t.Fatalf("%+v", got)
	}
	// A known version is never replaced by a check.
	if _, err := s.RecordCheck(ctx, "kit", []Version{{"2.0.0", "2.0.0", "c100"}}, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppliedVersion(ctx, "alpha"); got.Version != "1.0.1" {
		t.Fatalf("%+v", got)
	}
	// Recorded unknown again, it stays unknown until the next check.
	if err := s.SetAppliedVersion(ctx, "alpha", AppliedVersion{Commit: "c100"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppliedVersion(ctx, "alpha"); got != (AppliedVersion{Commit: "c100"}) {
		t.Fatalf("%+v", got)
	}
	if _, err := s.RecordCheck(ctx, "kit", []Version{{"2.0.0", "2.0.0", "c100"}}, time.Unix(3, 0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.AppliedVersion(ctx, "alpha"); got != (AppliedVersion{Version: "2.0.0", Commit: "c100"}) {
		t.Fatalf("%+v", got)
	}
}
