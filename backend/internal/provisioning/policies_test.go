package provisioning

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
	"github.com/wyrd-company/wyrwood/operator"
)

type policyHost struct {
	*host
	bindings                       map[string]operator.Binding
	hidden                         bool
	changes, binds, pending, stale int
	allowed                        []string
	changeError                    error
	vanishAfterChange              bool
	revokeBeforeMutation           bool
	policyReads                    int
}

func newPolicyHost(t *testing.T, selected []string) *policyHost {
	h := &policyHost{host: newHost(t), bindings: map[string]operator.Binding{}, allowed: []string{"workspace", "ssh", "key"}}
	ctx := context.Background()
	if _, err := h.data.SavePolicySettings(ctx, store.PolicySettings{Revision: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.data.BeginCreate(ctx, "sample"); err != nil {
		t.Fatal(err)
	}
	if err := h.data.SetCreateSelection(ctx, "sample", selected, h.allowed); err != nil {
		t.Fatal(err)
	}
	return h
}
func (h *policyHost) Policies(context.Context) (operator.Grant, []operator.Policy, error) {
	h.policyReads++
	allowed := h.allowed
	if h.revokeBeforeMutation && h.policyReads > 1 {
		allowed = []string{"workspace"}
	}
	policies := []operator.Policy{}
	for _, name := range allowed {
		p := operator.Policy{Name: name}
		if name == "key" {
			p.ValueNames = []string{"key_name"}
		}
		policies = append(policies, p)
	}
	return operator.Grant{Agents: []string{sampleAgent}, Policies: h.allowed, Actions: []operator.Action{operator.ActionRead, operator.ActionBind}}, policies, nil
}
func (h *policyHost) Read(_ context.Context, name string) (operator.Binding, bool, error) {
	if h.hidden {
		return operator.Binding{}, false, nil
	}
	for _, b := range h.bindings {
		if b.Key.Value == name {
			return b, true, nil
		}
	}
	return operator.Binding{}, false, nil
}
func (h *policyHost) Get(_ context.Context, id string) (operator.Binding, error) {
	b, ok := h.bindings[id]
	if !ok {
		return b, operator.ErrNotFound
	}
	if h.hidden {
		return operator.Binding{}, operator.ErrOutsideGrant
	}
	return b, nil
}
func (h *policyHost) Bind(_ context.Context, name string, s wyrwood.Selection) (operator.Binding, error) {
	h.binds++
	if h.hidden {
		return operator.Binding{}, operator.ErrOutsideGrant
	}
	if len(s.Policies) == 0 {
		return operator.Binding{}, &operator.Error{Code: operator.CodeInvalid, Message: "a binding needs at least one policy\nchoose a policy"}
	}
	b := operator.Binding{ID: fmt.Sprintf("binding-%d", h.binds), Agent: sampleAgent, Key: operator.Key{Kind: operator.KeyName, Value: name}, Policies: slices.Clone(s.Policies), Revision: 1, Applied: true, Settings: operator.Settings{AccessGroup: s.AccessGroup, Values: maps.Clone(s.Values)}}
	h.bindings[b.ID] = b
	if h.pending > 0 {
		h.pending--
		b.Applied = false
		h.bindings[b.ID] = b
		return operator.Binding{}, operator.ErrPending
	}
	return b, nil
}
func (h *policyHost) Change(_ context.Context, b operator.Binding, s wyrwood.Selection) (operator.Binding, error) {
	h.changes++
	if h.changeError != nil {
		return operator.Binding{}, h.changeError
	}
	if h.stale > 0 {
		h.stale--
		b.Revision++
		h.bindings[b.ID] = b
		return operator.Binding{}, operator.ErrRevisionDiffers
	}
	if len(s.Policies) == 0 {
		return operator.Binding{}, &operator.Error{Code: operator.CodeInvalid, Message: "a binding needs at least one policy\nchoose a policy"}
	}
	b.Policies = slices.Clone(s.Policies)
	b.Settings.Values = maps.Clone(s.Values)
	b.Settings.AccessGroup = s.AccessGroup
	b.Revision++
	b.Applied = true
	if h.pending > 0 {
		h.pending--
		b.Applied = false
		h.bindings[b.ID] = b
		return operator.Binding{}, operator.ErrPending
	}
	h.bindings[b.ID] = b
	if h.vanishAfterChange {
		delete(h.bindings, b.ID)
	}
	return b, nil
}
func (h *policyHost) steps() *Steps {
	return New(Options{Clients: Clients{Bindings: h, Agent: sampleAgent, Wait: time.Second}, Runtime: h.host, Data: h.data, Poll: time.Millisecond})
}
func (h *policyHost) existing(policies []string) {
	h.hold("sample", "container-sample", true)
	group := uint32(4321)
	h.bindings["existing"] = operator.Binding{ID: "existing", Agent: sampleAgent, Key: operator.Key{Kind: operator.KeyName, Value: "container-sample"}, Policies: policies, Revision: 4, Applied: true, Settings: operator.Settings{AccessGroup: &group, Values: map[string]string{}}}
	if err := h.data.BindingID(context.Background(), "sample", "container-sample", "existing"); err != nil {
		h.t.Fatal(err)
	}
}
func (h *policyHost) selection() store.Selection {
	current, err := h.data.Selection(context.Background(), "sample")
	if err != nil {
		h.t.Fatal(err)
	}
	return current
}

func TestPolicyApplyChangesRunningBindingWithoutReadingDefinition(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace", "ssh", "key"})
	h.existing([]string{"workspace"})
	h.definedErr = errors.New("invalid definition")
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	b := h.bindings["existing"]
	current := h.selection()
	if h.changes != 1 || h.binds != 0 || *b.Settings.AccessGroup != 4321 || !slices.Equal(b.Policies, []string{"key", "ssh", "workspace"}) || b.Settings.Values["key_name"] != "sample" || current.AppliedRevision == nil || *current.AppliedRevision != current.Revision || len(h.calls) != 0 {
		t.Fatalf("binding %+v selection %+v calls %v", b, current, h.calls)
	}
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil || h.changes != 1 {
		t.Fatalf("equal binding rewritten: %v changes %d", err, h.changes)
	}
}
func TestPolicyApplyFinishesPendingAndBoundsRetries(t *testing.T) {
	for _, pending := range []int{1, 4} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			h := newPolicyHost(t, []string{"workspace", "ssh"})
			h.existing([]string{"workspace"})
			h.pending = pending
			err := h.steps().ApplyPolicies(context.Background(), folder, h)
			if pending == 4 {
				wantKind(t, err, KindBindingPending)
				if h.selection().AppliedRevision != nil {
					t.Fatal("pending recorded applied")
				}
			} else if err != nil || h.changes != 2 {
				t.Fatalf("%v changes %d", err, h.changes)
			}
		})
	}
}
func TestPolicyApplyDecidesStaleChangeAgainOnce(t *testing.T) {
	for _, stale := range []int{1, 2} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			h := newPolicyHost(t, []string{"workspace", "ssh"})
			h.existing([]string{"workspace"})
			h.stale = stale
			err := h.steps().ApplyPolicies(context.Background(), folder, h)
			if stale == 2 {
				wantKind(t, err, KindBindingChanged)
			} else if err != nil {
				t.Fatal(err)
			}
			if h.changes != 2 {
				t.Fatalf("changes %d", h.changes)
			}
		})
	}
}
func TestPolicyApplyFindsReplacementByName(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace", "ssh"})
	h.existing([]string{"workspace"})
	b := h.bindings["existing"]
	delete(h.bindings, "existing")
	b.ID = "replacement"
	h.bindings[b.ID] = b
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	held := h.held("sample")
	if h.changes != 1 || held.Names[0].BindingID != "replacement" {
		t.Fatalf("changes %d held %+v", h.changes, held)
	}
}
func TestPolicyApplyRefusesHiddenAndOutsidePoliciesBeforeMutation(t *testing.T) {
	for _, hidden := range []bool{true, false} {
		t.Run(fmt.Sprint(hidden), func(t *testing.T) {
			h := newPolicyHost(t, []string{"workspace", "ssh"})
			h.existing([]string{"workspace"})
			kind := KindPolicyOutsideGrant
			if hidden {
				h.hidden = true
				kind = KindBindingOutsideGrant
			} else {
				h.allowed = []string{"workspace"}
			}
			wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), kind)
			if h.binds != 0 || h.changes != 0 || h.selection().AppliedRevision != nil {
				t.Fatal("changed or recorded applied")
			}
		})
	}
}
func TestEmptySelectionIsAppliedWithoutACurrentName(t *testing.T) {
	h := newPolicyHost(t, nil)
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if h.selection().AppliedRevision == nil || h.binds != 0 || h.changes != 0 {
		t.Fatal("empty selection not recorded without binding requests")
	}
}
func TestEmptySelectionRefusalKeepsBindingAndOneLineFailure(t *testing.T) {
	h := newPolicyHost(t, nil)
	h.existing([]string{"workspace"})
	wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), KindBindingRefused)
	current := h.selection()
	if h.changes != 1 || current.AppliedRevision != nil || current.ApplyFailure == nil || current.ApplyFailure.Kind != KindBindingRefused || strings.Contains(current.ApplyFailure.Message, "\n") || !strings.Contains(current.ApplyFailure.Message, "choose a policy") || !slices.Equal(h.bindings["existing"].Policies, []string{"workspace"}) {
		t.Fatalf("%+v", current)
	}
}
func TestMissingBindingRequiresAcceptedCurrentDeclaration(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace"})
	h.hold("sample", "old-name", true)
	wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), KindInvalid)
	if h.binds != 0 || h.changes != 0 || h.selection().AppliedRevision != nil {
		t.Fatal("changed missing binding with another declared name")
	}
}

func TestMissingBindingEmptySelectionUsesDeclarationThenWyrwoodRefusal(t *testing.T) {
	h := newPolicyHost(t, nil)
	h.hold("sample", "container-sample", true)
	wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), KindBindingRefused)
	if h.binds != 1 || h.selection().AppliedRevision != nil || h.selection().ApplyFailure == nil {
		t.Fatal("empty missing binding recorded applied")
	}
}
func TestHiddenReplacementRefusesBothEmptyAndNonemptySelections(t *testing.T) {
	for _, selection := range [][]string{nil, {"workspace"}} {
		t.Run(fmt.Sprint(selection), func(t *testing.T) {
			h := newPolicyHost(t, selection)
			h.existing([]string{"workspace"})
			delete(h.bindings, "existing")
			h.hidden = true
			wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), KindBindingOutsideGrant)
			if h.binds != 1 || h.changes != 0 || h.selection().AppliedRevision != nil {
				t.Fatal("hidden replacement changed or recorded applied")
			}
		})
	}
}
func TestPendingBindIsFinishedByChangeAndNextApply(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace"})
	h.hold("sample", "container-sample", true)
	h.pending = 4
	wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), KindBindingPending)
	if h.selection().AppliedRevision != nil {
		t.Fatal("pending bind recorded applied")
	}
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if h.selection().AppliedRevision == nil || h.binds != 1 {
		t.Fatal("next apply did not finish saved bind")
	}
}

func TestImportKeepsRunningBindingSelectionAndLegacyKey(t *testing.T) {
	h := newPolicyHost(t, nil)
	h.existing([]string{"workspace", "ssh"})
	b := h.bindings["existing"]
	b.Settings.Values = map[string]string{"key_name": "legacy-name"}
	h.bindings[b.ID] = b
	if _, err := h.data.Import(context.Background(), 99, func(tx *store.Tx) error {
		return tx.PrepareSelectionImport("sample", &Key{Policy: "legacy-policy", Name: "legacy-name"})
	}); err != nil {
		t.Fatal(err)
	}
	before := h.selection()
	if err := h.steps().EnsureImported(context.Background(), folder); err != nil {
		t.Fatal(err)
	}
	imported := h.selection()
	if !imported.Imported || !slices.Equal(imported.Policies, []string{"ssh", "workspace"}) || imported.Values["key_name"] != "legacy-name" || imported.Key == nil || imported.Key.String() != "legacy-policy/legacy-name" || imported.Revision <= before.Revision {
		t.Fatalf("%+v", imported)
	}
	next, err := h.data.SaveSelection(context.Background(), "sample", []string{"workspace", "ssh", "key"}, imported.Revision, h.allowed)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if h.bindings["existing"].Settings.Values["key_name"] != "legacy-name" || h.selection().AppliedRevision == nil || *h.selection().AppliedRevision != next.Revision || len(h.calls) != 0 {
		t.Fatal("imported running workspace was not changed without a rebuild")
	}
	h.state, h.container = "running", "same-container"
	h.definedErr = errors.New("definition does not parse")
	removed, err := h.data.SaveSelection(context.Background(), "sample", []string{"workspace", "key"}, next.Revision, h.allowed)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	binding := h.bindings["existing"]
	current := h.selection()
	if !slices.Equal(binding.Policies, []string{"key", "workspace"}) || binding.Settings.AccessGroup == nil || *binding.Settings.AccessGroup != 4321 || binding.Settings.Values["key_name"] != "legacy-name" || current.AppliedRevision == nil || *current.AppliedRevision != removed.Revision || h.state != "running" || h.container != "same-container" || len(h.calls) != 0 {
		t.Fatalf("removal lost running binding: %+v selection %+v calls %v", binding, current, h.calls)
	}
}

func TestProvisioningIgnoresTemplateIdentityAndNeedsConfiguredKeyPolicy(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = definition(`{"wyrwood":{"accessGroup":1000,"policies":null,"values":false},"xolotl":"ignored"}`)
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	selection, err := h.data.Selection(context.Background(), "sample")
	if err != nil {
		t.Fatal(err)
	}
	if selection.Key == nil || selection.Key.String() != "host-sample/sample" || selection.Values["key_name"] != "sample" {
		t.Fatalf("%+v", selection)
	}
	for _, name := range []string{"wyrwood.policies", "wyrwood.values", "xolotl"} {
		if !strings.Contains(h.logged.String(), "ignoring legacy customizations.menagerie."+name) {
			t.Fatalf("unreported member %s", name)
		}
	}
	other := newHost(t)
	steps := New(Options{Clients: Clients{Bindings: other, Agent: sampleAgent, Keys: other}, Runtime: other, Data: other.data})
	wantKind(t, steps.Start(context.Background(), folder, other), KindUnavailable)
	if len(other.calls) != 0 {
		t.Fatalf("key-policy failure performed operations %v", other.calls)
	}
}

func TestSuccessfulChangeWithMissingConfirmingReadResolvesAgain(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace", "ssh"})
	h.existing([]string{"workspace"})
	h.vanishAfterChange = true
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if h.changes != 1 || h.binds != 1 || h.selection().AppliedRevision == nil {
		t.Fatal("missing confirming read did not resolve and recreate")
	}
}
func TestMissingEmptyBindingWithoutCurrentDeclarationSendsNothing(t *testing.T) {
	h := newPolicyHost(t, nil)
	h.hold("sample", "old-name", true)
	wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), KindInvalid)
	if h.binds != 0 || h.selection().AppliedRevision != nil {
		t.Fatal("empty selection bypassed accepted declaration")
	}
}

func TestPolicyApplyRefreshesGrantBeforeMutation(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace", "ssh"})
	h.existing([]string{"workspace"})
	h.revokeBeforeMutation = true
	wantKind(t, h.steps().ApplyPolicies(context.Background(), folder, h), KindPolicyOutsideGrant)
	if h.changes != 0 || h.selection().AppliedRevision != nil {
		t.Fatal("changed after grant narrowed")
	}
}

func TestProvisioningRefusalInvalidatesPreviouslyAppliedSelection(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"create", "start", "rebuild"} {
		for _, scenario := range []string{"empty-missing", "empty-existing", "hidden"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				selected := []string{}
				if scenario == "hidden" {
					selected = []string{"workspace"}
				}
				h := newPolicyHost(t, selected)
				// With no current name, this revision legitimately counted as applied.
				if err := h.steps().ApplyPolicies(ctx, folder, h); err != nil {
					t.Fatal(err)
				}
				before := h.selection()
				if before.AppliedRevision == nil || *before.AppliedRevision != before.Revision {
					t.Fatal("no-name revision was not applied")
				}
				if scenario != "empty-missing" {
					h.existing([]string{"workspace"})
					// The next job resolves the name, as after a restart with no saved ID.
					if err := h.data.BindingID(ctx, "sample", "container-sample", ""); err != nil {
						t.Fatal(err)
					}
				}
				h.hidden = scenario == "hidden"
				bindings := maps.Clone(h.bindings)
				var err error
				switch operation {
				case "create":
					err = h.steps().Create(ctx, folder, h, func(context.Context) error { t.Fatal("refused create saved"); return nil })
				case "start":
					err = h.steps().Start(ctx, folder, h)
				case "rebuild":
					err = h.steps().Rebuild(ctx, folder, h)
				}
				kind := KindBindingRefused
				if h.hidden {
					kind = KindBindingOutsideGrant
				}
				wantKind(t, err, kind)
				after := h.selection()
				if after.Revision != before.Revision || !slices.Equal(after.Policies, selected) || after.AppliedRevision != nil || after.ApplyFailure == nil || after.ApplyFailure.Kind != kind {
					t.Fatalf("refusal kept an applied selection: %+v", after)
				}
				if strings.ContainsAny(after.ApplyFailure.Message, "\r\n") || !strings.Contains(h.logged.String(), after.ApplyFailure.Message+"\n") {
					t.Fatalf("failure not recorded on one console line: %+v; console %q", after.ApplyFailure, h.logged.String())
				}
				if !reflect.DeepEqual(h.bindings, bindings) {
					t.Fatal("refusal changed the binding")
				}
				if scenario == "empty-existing" {
					if h.changes != 1 || h.binds != 0 {
						t.Fatalf("change %d bind %d", h.changes, h.binds)
					}
				} else if h.binds != 1 || h.changes != 0 {
					t.Fatalf("change %d bind %d", h.changes, h.binds)
				}
				for _, call := range h.calls {
					if call == "build" || call == "create" || call == "up" {
						t.Fatalf("refusal brought up container: %v", h.calls)
					}
				}
			})
		}
	}
}

func TestInitialStaleIDDoesNotConsumeTheChangeRetry(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace", "ssh"})
	h.existing([]string{"workspace"})
	binding := h.bindings["existing"]
	delete(h.bindings, binding.ID)
	binding.ID = "replacement"
	h.bindings[binding.ID] = binding
	h.stale = 1
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if h.changes != 2 || h.binds != 0 || h.selection().AppliedRevision == nil {
		t.Fatalf("changes %d binds %d selection %+v", h.changes, h.binds, h.selection())
	}
}

func TestPolicyApplyKeepsAnAbsentAccessGroupWithoutReadingDefinition(t *testing.T) {
	h := newPolicyHost(t, []string{"workspace", "ssh"})
	h.existing([]string{"workspace"})
	binding := h.bindings["existing"]
	binding.Settings.AccessGroup = nil
	h.bindings[binding.ID] = binding
	h.definedErr = errors.New("definition cannot be parsed")
	if err := h.steps().ApplyPolicies(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	binding = h.bindings["existing"]
	if h.changes != 1 || binding.Settings.AccessGroup != nil || !slices.Equal(binding.Policies, []string{"ssh", "workspace"}) || h.selection().AppliedRevision == nil {
		t.Fatalf("binding %+v selection %+v", binding, h.selection())
	}
}

// interruptedPolicyWrite stops after Wyrwood saves a pending mutation but before
// applySelection receives its outcome. A panic also bypasses error-only cleanup.
type interruptedPolicyWrite struct {
	*policyHost
	beforeWrite func()
}

func (h interruptedPolicyWrite) Bind(ctx context.Context, name string, wanted wyrwood.Selection) (operator.Binding, error) {
	h.beforeWrite()
	_, _ = h.policyHost.Bind(ctx, name, wanted)
	panic("stopped after pending write")
}

func (h interruptedPolicyWrite) Change(ctx context.Context, binding operator.Binding, wanted wyrwood.Selection) (operator.Binding, error) {
	h.beforeWrite()
	_, _ = h.policyHost.Change(ctx, binding, wanted)
	panic("stopped after pending write")
}

func TestInterruptedPolicyWriteIsUnappliedBeforeMutation(t *testing.T) {
	ctx := context.Background()
	for _, operation := range []string{"bind", "change"} {
		t.Run(operation, func(t *testing.T) {
			h := newPolicyHost(t, []string{"workspace", "ssh"})
			// A nameless apply or an earlier successful job applied this revision.
			if err := h.steps().ApplyPolicies(ctx, folder, h); err != nil {
				t.Fatal(err)
			}
			revision := h.selection().Revision
			if operation == "change" {
				h.existing([]string{"workspace"})
			} else {
				h.hold("sample", "container-sample", true)
			}
			// Model a previously confirmed binding which Wyrwood later removed
			// or edited. HoldName itself now invalidates a new current name.
			if err := h.data.Applied(ctx, "sample", revision); err != nil {
				t.Fatal(err)
			}
			h.pending = 1
			var atWrite store.Selection
			steps := h.steps()
			steps.opts.Bindings = interruptedPolicyWrite{h, func() { atWrite = h.selection() }}
			func() {
				defer func() {
					if got := recover(); got != "stopped after pending write" {
						t.Fatalf("interruption: %v", got)
					}
				}()
				_ = steps.ApplyPolicies(ctx, folder, h)
				t.Fatal("write was not interrupted")
			}()
			after := h.selection()
			if atWrite.AppliedRevision != nil || after.AppliedRevision != nil || after.ApplyFailure != nil || after.Revision != revision {
				t.Fatalf("interrupted write still applied: at write %+v, after %+v", atWrite, after)
			}
			if len(h.bindings) != 1 {
				t.Fatalf("pending bindings: %+v", h.bindings)
			}
			for _, binding := range h.bindings {
				if binding.Applied {
					t.Fatal("interruption did not leave a pending binding")
				}
			}
			// The next apply can finish the saved mutation at the same revision.
			if err := h.steps().ApplyPolicies(ctx, folder, h); err != nil {
				t.Fatal(err)
			}
			repaired := h.selection()
			if repaired.AppliedRevision == nil || *repaired.AppliedRevision != revision {
				t.Fatalf("retry did not repair selection: %+v", repaired)
			}
		})
	}
}

func TestPlainJobsRecordNamelessSelectionImmediately(t *testing.T) {
	for _, operation := range []string{"create", "start", "rebuild"} {
		t.Run(operation, func(t *testing.T) {
			h := newPolicyHost(t, []string{"workspace"})
			plain := h.definitions[folder]
			plain.Block = nil
			plain.ContainerName = ""
			h.definitions[folder] = plain
			steps := h.steps()
			// A nameless workspace needs no controller, even when it has policies.
			steps.opts.Bindings = nil
			var err error
			switch operation {
			case "create":
				err = steps.Create(context.Background(), folder, h, func(context.Context) error { return nil })
			case "start":
				err = steps.Start(context.Background(), folder, h)
			case "rebuild":
				err = steps.Rebuild(context.Background(), folder, h)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := h.selection()
			if got.AppliedRevision == nil || *got.AppliedRevision != got.Revision || got.ApplyFailure != nil || h.held("sample").Current() != "" {
				t.Fatalf("successful plain job left policies pending: %+v", got)
			}
			if h.policyReads != 0 || h.binds != 0 || h.changes != 0 {
				t.Fatal("nameless job contacted Wyrwood")
			}
		})
	}
}

// interruptedNameChange stops on the first network call after HoldName, before
// applySelection can reach Applying or any binding write.
type interruptedNameChange struct{ *policyHost }

func (h interruptedNameChange) Policies(context.Context) (operator.Grant, []operator.Policy, error) {
	panic("stopped after holding current name")
}

func TestInterruptedCurrentNameChangeIsUnappliedBeforeNetwork(t *testing.T) {
	ctx := context.Background()
	for _, previous := range []string{"nameless", "bound"} {
		t.Run(previous, func(t *testing.T) {
			h := newPolicyHost(t, []string{"workspace"})
			if previous == "bound" {
				h.existing([]string{"workspace"})
			}
			if err := h.steps().ApplyPolicies(ctx, folder, h); err != nil {
				t.Fatal(err)
			}
			revision := h.selection().Revision
			if h.selection().AppliedRevision == nil {
				t.Fatal("setup not applied")
			}
			def := h.definitions[folder]
			def.ContainerName = "container-b"
			h.definitions[folder] = def
			steps := h.steps()
			steps.opts.Bindings = interruptedNameChange{h}
			func() {
				defer func() {
					if got := recover(); got != "stopped after holding current name" {
						t.Fatalf("interruption: %v", got)
					}
				}()
				_ = steps.Rebuild(ctx, folder, h)
				t.Fatal("name change was not interrupted")
			}()
			// Read through a fresh database handle, as the next process would.
			reopened, err := store.Open(h.data.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			after, err := reopened.Selection(ctx, "sample")
			if err != nil {
				t.Fatal(err)
			}
			held, err := reopened.Held(ctx, "sample")
			if err != nil {
				t.Fatal(err)
			}
			if held.Current() != "container-b" || after.AppliedRevision != nil || after.ApplyFailure != nil || after.Revision != revision || h.binds != 0 || h.changes != 0 {
				t.Fatalf("interruption left old observation: %+v held %+v writes %d/%d", after, held, h.binds, h.changes)
			}
			if err := h.steps().ApplyPolicies(ctx, folder, h); err != nil {
				t.Fatal(err)
			}
			repaired := h.selection()
			binding, found, err := h.Read(ctx, "container-b")
			if err != nil || !found || !binding.Applied || repaired.AppliedRevision == nil || *repaired.AppliedRevision != revision {
				t.Fatalf("retry: %+v binding %+v, %v", repaired, binding, err)
			}
		})
	}
}
