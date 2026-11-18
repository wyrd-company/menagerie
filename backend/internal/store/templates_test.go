package store

import (
	"context"
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

// The list is by name without regard to case, then by name and id.
func TestTemplatesListByNameWithoutRegardToCase(t *testing.T) {
	s := open(t)
	registered(t, s, "Zeta", "alpha", "Beta", "beta")
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
