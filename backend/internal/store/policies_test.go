package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPolicySettingsPropagateAtomicallyAndPersist(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.Discover(ctx, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	if err := s.HoldName(ctx, "gone", "gone-container", "host", true); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginCreate(ctx, "creating"); err != nil {
		t.Fatal(err)
	}
	saved, err := s.SavePolicySettings(ctx, PolicySettings{Required: []string{"workspace", "key"}, Groups: []PolicyGroup{{Name: "SSH", Mode: "single", Policies: []string{"one", "two"}}}}, []string{"workspace", "key", "one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Groups[0].ID == 0 {
		t.Fatalf("saved %+v", saved)
	}
	_, selections, err := s.Policies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "gone", "creating"} {
		v := selections[name]
		if !reflect.DeepEqual(v.Policies, []string{"key", "workspace"}) || v.Revision != 1 {
			t.Fatalf("%s: %+v", name, v)
		}
	}
	if _, err := s.SavePolicySettings(ctx, PolicySettings{}, nil); err == nil {
		t.Fatal("stale save accepted")
	}
	saved.Required = []string{}
	saved, err = s.SavePolicySettings(ctx, saved, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, selections, err = s.Policies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if selections["alpha"].Revision != 1 || len(selections["alpha"].Policies) != 2 {
		t.Fatal(selections)
	}
	if _, err := s.Discover(ctx, []string{"alpha", "beta", "new"}); err != nil {
		t.Fatal(err)
	}
	_, selections, err = s.Policies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(selections["new"].Policies) != 0 {
		t.Fatal(selections["new"])
	}
	oldID := saved.Groups[0].ID
	saved.Groups = []PolicyGroup{}
	saved, err = s.SavePolicySettings(ctx, saved, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved.Groups = []PolicyGroup{{Name: "SSH again", Mode: "multi", Policies: []string{"one"}}}
	saved, err = s.SavePolicySettings(ctx, saved, []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Groups[0].ID <= oldID {
		t.Fatal("reused group ID")
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, selections, err := reopened.Policies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, saved) || selections["alpha"].Revision != 1 {
		t.Fatalf("%+v %+v", got, selections)
	}
}

func TestPolicySettingsRejectEachInvalidRuleWithoutChangingData(t *testing.T) {
	cases := []struct {
		name     string
		settings PolicySettings
		kind     string
	}{
		{"required and group", PolicySettings{Required: []string{"one"}, Groups: []PolicyGroup{{Name: "SSH", Mode: "single", Policies: []string{"one"}}}}, "invalid-settings"},
		{"two groups", PolicySettings{Groups: []PolicyGroup{{Name: "A", Mode: "single", Policies: []string{"one"}}, {Name: "B", Mode: "multi", Policies: []string{"one"}}}}, "invalid-settings"},
		{"empty group", PolicySettings{Groups: []PolicyGroup{{Name: "A", Mode: "single"}}}, "invalid-settings"},
		{"duplicate group name", PolicySettings{Groups: []PolicyGroup{{Name: "A", Mode: "single", Policies: []string{"one"}}, {Name: "A", Mode: "single", Policies: []string{"two"}}}}, "invalid-settings"},
		{"unknown mode", PolicySettings{Groups: []PolicyGroup{{Name: "A", Mode: "unknown", Policies: []string{"one"}}}}, "invalid-settings"},
		{"unknown id", PolicySettings{Groups: []PolicyGroup{{ID: 42, Name: "A", Mode: "single", Policies: []string{"one"}}}}, "invalid-settings"},
		{"multiline name", PolicySettings{Groups: []PolicyGroup{{Name: "A\nB", Mode: "single", Policies: []string{"one"}}}}, "invalid-settings"},
		{"outside grant", PolicySettings{Required: []string{"missing", "outside"}}, "policy-outside-grant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			ctx := context.Background()
			_, err := s.SavePolicySettings(ctx, tc.settings, []string{"one", "two"})
			var e *PolicyError
			if !errors.As(err, &e) || e.Kind != tc.kind {
				t.Fatalf("%v", err)
			}
			rule := map[string]string{"required and group": "both required policies and group", "two groups": "both group", "empty group": "no policy", "duplicate group name": "must be unique", "unknown mode": "unknown mode", "unknown id": "unknown or repeated id", "multiline name": "one line", "outside grant": "missing, outside"}[tc.name]
			if !strings.Contains(e.Message, rule) {
				t.Fatalf("refusal does not name rule %q: %s", rule, e.Message)
			}
			got, _, err := s.Policies(ctx)
			if err != nil || got.Revision != 0 || len(got.Names()) != 0 {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestPolicyNeedsChoice(t *testing.T) {
	s := PolicySettings{Groups: []PolicyGroup{{Name: "Single", Mode: "single", Policies: []string{"one", "two"}}, {Name: "Multi", Mode: "multi", Policies: []string{"three", "four"}}}}
	for _, tc := range []struct{ selection, want []string }{{nil, []string{"Single", "Multi"}}, {[]string{"one", "two", "three"}, []string{"Single"}}, {[]string{"one", "three", "four"}, []string{}}} {
		if got := s.NeedsChoice(tc.selection); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%v: %v", tc.selection, got)
		}
	}
}

func TestNewCreateReplacesAnUnclaimedSelection(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.Discover(ctx, []string{"reused"}); err != nil {
		t.Fatal(err)
	}
	settings, err := s.SavePolicySettings(ctx, PolicySettings{Required: []string{"old"}}, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	settings.Required = []string{}
	if _, err := s.SavePolicySettings(ctx, settings, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginCreate(ctx, "reused"); err != nil {
		t.Fatal(err)
	}
	_, selections, err := s.Policies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := selections["reused"]; len(got.Policies) != 0 || got.Revision != 0 {
		t.Fatalf("new create kept old selection: %+v", got)
	}
}

func TestPolicySchemaUpgradeKeepsProvisioningData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "menagerie.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workspaces (name) VALUES ('existing'); INSERT INTO container_names (name, workspace, agent, current) VALUES ('container','existing','host',1); PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	settings, selections, err := s.Policies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Revision != 0 || selections["existing"].Revision != 0 || len(selections["existing"].Policies) != 0 {
		t.Fatalf("%+v %+v", settings, selections)
	}
	held, err := s.Held(context.Background(), "existing")
	if err != nil || held.Current() != "container" || held.Names[0].Agent != "host" {
		t.Fatalf("%+v %v", held, err)
	}
	if _, err := s.SavePolicySettings(context.Background(), PolicySettings{Required: []string{"new"}}, []string{"new"}); err != nil {
		t.Fatal(err)
	}
	_, selections, err = s.Policies(context.Background())
	if err != nil || selections["existing"].Revision != 1 {
		t.Fatalf("%+v %v", selections, err)
	}
}

func TestPolicySettingsCannotPropagateALostGroupPolicyByMakingItRequired(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	saved, err := s.SavePolicySettings(ctx, PolicySettings{Groups: []PolicyGroup{{Name: "SSH", Mode: "single", Policies: []string{"lost"}}}}, []string{"lost"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.SavePolicySettings(ctx, PolicySettings{Required: []string{"lost"}, Revision: saved.Revision}, nil)
	var refusal *PolicyError
	if !errors.As(err, &refusal) || refusal.Kind != "policy-outside-grant" || !strings.Contains(refusal.Message, "lost") {
		t.Fatalf("%v", err)
	}
	_, selections, err := s.Policies(ctx)
	if err != nil || len(selections["alpha"].Policies) != 0 {
		t.Fatalf("%+v %v", selections, err)
	}
	// Retaining the group's member is allowed, so unrelated edits still work.
	saved.Groups[0].Name = "Renamed SSH"
	if _, err := s.SavePolicySettings(ctx, saved, nil); err != nil {
		t.Fatal(err)
	}
}
