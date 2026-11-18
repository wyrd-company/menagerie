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

func TestFailedApplyClearsOnlyItsOwnAppliedRevision(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Applied(ctx, "alpha", before.Revision); err != nil {
		t.Fatal(err)
	}
	failure := ApplyFailure{"binding-refused", "choose a policy"}
	if err = s.ApplyFailed(ctx, "alpha", before.Revision, failure); err != nil {
		t.Fatal(err)
	}
	after, err := s.Selection(ctx, "alpha")
	if err != nil || after.AppliedRevision != nil || after.ApplyFailure == nil || *after.ApplyFailure != failure {
		t.Fatalf("%+v %v", after, err)
	}
	next, err := s.SaveSelection(ctx, "alpha", []string{"ssh"}, before.Revision, []string{"ssh"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Applied(ctx, "alpha", next.Revision); err != nil {
		t.Fatal(err)
	}
	// A late old failure cannot invalidate a newer successful attempt.
	if err = s.ApplyFailed(ctx, "alpha", before.Revision, failure); err != nil {
		t.Fatal(err)
	}
	after, err = s.Selection(ctx, "alpha")
	if err != nil || after.AppliedRevision == nil || *after.AppliedRevision != next.Revision || after.ApplyFailure != nil {
		t.Fatalf("%+v %v", after, err)
	}
}

func TestApplyingClearsOnlyItsOwnAppliedRevision(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Applied(ctx, "alpha", before.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.Applying(ctx, "alpha", before.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := s.Selection(ctx, "alpha")
	if err != nil || after.AppliedRevision != nil || after.ApplyFailure != nil {
		t.Fatalf("%+v %v", after, err)
	}
	next, err := s.SaveSelection(ctx, "alpha", []string{"ssh"}, before.Revision, []string{"ssh"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Applied(ctx, "alpha", next.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.Applying(ctx, "alpha", before.Revision); err != nil {
		t.Fatal(err)
	}
	after, err = s.Selection(ctx, "alpha")
	if err != nil || after.AppliedRevision == nil || *after.AppliedRevision != next.Revision || after.ApplyFailure != nil {
		t.Fatalf("%+v %v", after, err)
	}
}

func TestDiscoveryAppliesOnlyImportedNamelessSelections(t *testing.T) {
	s, ctx := open(t), context.Background()
	for _, name := range []string{"pending", "bound", "retired"} {
		if err := s.HoldName(ctx, name, "container-"+name, "host", name != "retired"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Import(ctx, 99, func(tx *Tx) error { return tx.PrepareSelectionImport("pending", nil, nil, nil) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(ctx, []string{"discovered", "pending", "bound", "retired"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"discovered", "pending", "bound", "retired"} {
		got, err := s.Selection(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		wantApplied := name == "discovered"
		if wantApplied != (got.AppliedRevision != nil && *got.AppliedRevision == got.Revision) {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

func TestHoldNameInvalidatesOnlyChangedCurrentBinding(t *testing.T) {
	s, ctx := open(t), context.Background()
	if err := s.HoldName(ctx, "alpha", "container-a", "host", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Applied(ctx, "alpha", 0); err != nil {
		t.Fatal(err)
	}
	for _, current := range []bool{false, true} {
		name := "container-retired"
		if current {
			name = "container-a"
		}
		if err := s.HoldName(ctx, "alpha", name, "host", current); err != nil {
			t.Fatal(err)
		}
		got, err := s.Selection(ctx, "alpha")
		if err != nil || got.AppliedRevision == nil {
			t.Fatalf("unchanged current invalidated: %+v %v", got, err)
		}
	}
	if err := s.HoldName(ctx, "alpha", "container-b", "host", true); err != nil {
		t.Fatal(err)
	}
	got, err := s.Selection(ctx, "alpha")
	if err != nil || got.AppliedRevision != nil {
		t.Fatalf("new name kept old observation: %+v %v", got, err)
	}
}

func TestDiscoveryPreservesAnExistingNamelessApplyFailure(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, err := s.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	initial, err := s.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.SaveSelection(ctx, "alpha", []string{"ssh"}, initial.Revision, []string{"ssh"})
	if err != nil {
		t.Fatal(err)
	}
	failure := ApplyFailure{Kind: "policy-outside-grant", Message: "ssh is outside the operator key's grant"}
	if err := s.ApplyFailed(ctx, "alpha", next.Revision, failure); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Selection(ctx, "alpha")
	if err != nil || after.AppliedRevision == nil || *after.AppliedRevision == after.Revision || after.ApplyFailure == nil || *after.ApplyFailure != failure {
		t.Fatalf("discovery covered a failed apply: %+v %v", after, err)
	}
}
