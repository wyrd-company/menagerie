package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestSelectionEditsValidateAndPersistEmpty(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCreateSelection(ctx, "alpha", []string{"ssh"}, []string{"ssh"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Applied(ctx, "alpha", before.Revision); err != nil {
		t.Fatal(err)
	}
	empty, err := s.SaveSelection(ctx, "alpha", []string{}, before.Revision, nil)
	if err != nil || len(empty.Policies) != 0 || empty.Revision != before.Revision+1 || empty.AppliedRevision == nil || *empty.AppliedRevision != before.Revision {
		t.Fatalf("empty %+v, %v", empty, err)
	}
	_, err = s.SaveSelection(ctx, "alpha", []string{"other"}, empty.Revision, nil)
	var refusal *PolicyError
	if !errors.As(err, &refusal) || refusal.Kind != "policy-outside-grant" {
		t.Fatal(err)
	}
	_, err = s.SaveSelection(ctx, "alpha", nil, before.Revision, nil)
	if !errors.As(err, &refusal) || refusal.Kind != "selection-changed" {
		t.Fatal(err)
	}
	unchanged, err := s.Selection(ctx, "alpha")
	if err != nil || unchanged.Revision != empty.Revision {
		t.Fatalf("%+v %v", unchanged, err)
	}
}

func TestSelectionImportInvalidatesOldRevisionAndPreservesIdentity(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, func(tx *Tx) error {
		_, err := tx.exec(`UPDATE workspaces SET selection_imported = 0 WHERE name = 'alpha'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	imported := Key{"old-policy", "old-name"}
	if err := s.ImportSelection(ctx, "alpha", []string{"old"}, map[string]string{"account": "legacy"}, &imported); err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveSelection(ctx, "alpha", nil, before.Revision, nil)
	var refusal *PolicyError
	if !errors.As(err, &refusal) || refusal.Kind != "selection-changed" {
		t.Fatal(err)
	}
	identity, err := s.Identity(ctx, "alpha", []string{"account", "workspace"}, &Key{"new", "alpha"})
	if err != nil || !identity.Imported || identity.Key == nil || *identity.Key != imported || identity.Values["account"] != "legacy" || identity.Values["workspace"] != "alpha" {
		t.Fatalf("%+v %v", identity, err)
	}
	if err := s.ImportSelection(ctx, "alpha", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	after, err := s.Selection(ctx, "alpha")
	if err != nil || !slices.Equal(after.Policies, []string{"old"}) || after.Revision != identity.Revision {
		t.Fatalf("%+v %v", after, err)
	}
}

func TestSelectionGroupChoicesRequiredAndAtomic(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	settings, err := s.SavePolicySettings(ctx, PolicySettings{Required: []string{"workspace"}, Groups: []PolicyGroup{{Name: "SSH", Mode: "single", Policies: []string{"one", "two"}}}}, []string{"workspace", "one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, choices := range [][]string{nil, {"one", "two"}} {
		if err = s.SetCreateSelection(ctx, "alpha", choices, settings.Names()); err == nil {
			t.Fatalf("accepted choices %v", choices)
		}
	}
	if err = s.SetCreateSelection(ctx, "alpha", []string{"one"}, settings.Names()); err != nil {
		t.Fatal(err)
	}
	before, err := s.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, choices := range [][]string{{"one"}, {"workspace"}, {"workspace", "one", "two"}} {
		if _, err = s.SaveSelection(ctx, "alpha", choices, before.Revision, settings.Names()); err == nil {
			t.Fatalf("accepted choices %v", choices)
		}
	}
	after, err := s.Selection(ctx, "alpha")
	if err != nil || after.Revision != before.Revision || !slices.Equal(after.Policies, before.Policies) {
		t.Fatalf("%+v %v", after, err)
	}
	// A newly added group can remain incomplete while another policy changes.
	settings.Groups = append(settings.Groups, PolicyGroup{Name: "Network", Mode: "multi", Policies: []string{"network"}})
	if _, err = s.SavePolicySettings(ctx, settings, append(settings.Names(), "network")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveSelection(ctx, "alpha", []string{"workspace", "two"}, before.Revision, settings.Names()); err != nil {
		t.Fatal(err)
	}
}

func TestAnOldApplyDoesNotClaimANewerSelection(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.SaveSelection(ctx, "alpha", []string{"ssh"}, before.Revision, []string{"ssh"})
	if err != nil {
		t.Fatal(err)
	}
	failure := ApplyFailure{"binding-refused", "the controller answered: choose a policy"}
	if err = s.ApplyFailed(ctx, "alpha", next.Revision, failure); err != nil {
		t.Fatal(err)
	}
	if err = s.Applied(ctx, "alpha", before.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := s.Selection(ctx, "alpha")
	if err != nil || after.AppliedRevision == nil || *after.AppliedRevision != before.Revision || after.ApplyFailure == nil || *after.ApplyFailure != failure {
		t.Fatalf("%+v %v", after, err)
	}
}
