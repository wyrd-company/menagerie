package store

import (
	"context"
	"errors"
	"testing"
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
	if err := s.BeginCreateFrom(ctx, "alpha", "kit"); err != nil {
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
