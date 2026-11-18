// ---
// relationships:
//   implements: menagerie
//   references: workspace-api
// ---

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Selection reads a workspace's desired and applied selection atomically.
func (s *Store) Selection(ctx context.Context, name string) (out Selection, err error) {
	err = s.Update(ctx, func(t *Tx) error { out, err = t.Selection(name); return err })
	return
}

// ValidateChoices checks a create or edit against the current settings. An
// edit can retain an incomplete group, but cannot invalidate a valid choice.
func (settings PolicySettings) ValidateChoices(next, previous []string, create bool) error {
	if !create {
		for _, p := range settings.Required {
			if !slices.Contains(next, p) {
				return &PolicyError{"policy-required", fmt.Sprintf("Policy %q is required and cannot be removed.", p)}
			}
		}
	}
	for _, g := range settings.Groups {
		count := func(names []string) int {
			n := 0
			for _, p := range g.Policies {
				if slices.Contains(names, p) {
					n++
				}
			}
			return n
		}
		valid := func(n int) bool { return n > 0 && (g.Mode != "single" || n == 1) }
		if !valid(count(next)) && (create || valid(count(previous))) {
			return &PolicyError{"policy-choice-required", fmt.Sprintf("Choose %s policy in group %q.", map[bool]string{true: "exactly one", false: "at least one"}[g.Mode == "single"], g.Name)}
		}
	}
	return nil
}

// CheckAllowed refuses selected names missing from Wyrwood's current list.
func CheckAllowed(selected, allowed []string) error {
	outside := []string{}
	for _, p := range selected {
		if !slices.Contains(allowed, p) {
			outside = append(outside, p)
		}
	}
	slices.Sort(outside)
	if len(outside) > 0 {
		return &PolicyError{"policy-outside-grant", "Policies outside the operator key's grant or missing in Wyrwood: " + strings.Join(slices.Compact(outside), ", ")}
	}
	return nil
}

func (t *Tx) replacePolicies(name string, policies []string) error {
	if _, err := t.exec(`DELETE FROM workspace_policies WHERE workspace = ?`, name); err != nil {
		return err
	}
	for _, p := range policies {
		if _, err := t.exec(`INSERT INTO workspace_policies (workspace,policy) VALUES (?,?) ON CONFLICT DO NOTHING`, name, p); err != nil {
			return err
		}
	}
	return nil
}

// SaveSelection performs the revision, settings and grant checks in the same
// transaction as the edit. Empty selections are valid.
func (s *Store) SaveSelection(ctx context.Context, name string, policies []string, revision int64, allowed []string) (saved Selection, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		old, e := t.Selection(name)
		if e != nil {
			return e
		}
		if old.Revision != revision || !old.Imported {
			return &PolicyError{"selection-changed", "The workspace's policies changed. Reload them and try again."}
		}
		settings, e := t.PolicySettings()
		if e != nil {
			return e
		}
		if e = settings.ValidateChoices(policies, old.Policies, false); e != nil {
			return e
		}
		added := []string{}
		for _, p := range policies {
			if !slices.Contains(old.Policies, p) {
				added = append(added, p)
			}
		}
		if e = CheckAllowed(added, allowed); e != nil {
			return e
		}
		if e = t.replacePolicies(name, policies); e != nil {
			return e
		}
		if _, e = t.exec(`UPDATE workspaces SET selection_revision = selection_revision + 1, apply_failure = NULL WHERE name = ?`, name); e != nil {
			return e
		}
		saved, e = t.Selection(name)
		return e
	})
	return
}

// SetCreateSelection runs after BeginCreate and before any template work. The
// checks run again against the current settings, including their requirements.
func (s *Store) SetCreateSelection(ctx context.Context, name string, policies, allowed []string) error {
	return s.Update(ctx, func(t *Tx) error {
		settings, err := t.PolicySettings()
		if err != nil {
			return err
		}
		next := slices.Concat(policies, settings.Required)
		slices.Sort(next)
		next = slices.Compact(next)
		if err = settings.ValidateChoices(next, nil, true); err != nil {
			return err
		}
		if err = CheckAllowed(next, allowed); err != nil {
			return err
		}
		if err = t.replacePolicies(name, next); err != nil {
			return err
		}
		_, err = t.exec(`UPDATE workspaces SET selection_revision = max(selection_revision, 1), selection_imported = 1 WHERE name = ?`, name)
		return err
	})
}

// ImportSelection takes over the binding (or legacy declaration) once. The
// revision changes even for an empty import, invalidating a pre-import edit.
func (s *Store) ImportSelection(ctx context.Context, name string, policies []string, values map[string]string, key *Key) error {
	return s.Update(ctx, func(t *Tx) error {
		old, err := t.Selection(name)
		if err != nil || old.Imported {
			return err
		}
		settings, err := t.PolicySettings()
		if err != nil {
			return err
		}
		next := slices.Concat(policies, settings.Required)
		slices.Sort(next)
		next = slices.Compact(next)
		if err = t.replacePolicies(name, next); err != nil {
			return err
		}
		if values == nil {
			values = map[string]string{}
		}
		raw, err := json.Marshal(values)
		if err != nil {
			return err
		}
		var identity any
		if key != nil {
			b, e := json.Marshal(key)
			if e != nil {
				return e
			}
			identity = string(b)
		}
		_, err = t.exec(`UPDATE workspaces SET selection_revision = selection_revision + 1, selection_imported = 1, policy_values = ?, xolotl_identity = ? WHERE name = ?`, string(raw), identity, name)
		return err
	})
}

// Identity remembers new value names and a key exactly once. Existing values
// and an imported key survive selection and configuration changes.
func (s *Store) Identity(ctx context.Context, name string, valueNames []string, key *Key) (out Selection, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		old, e := t.Selection(name)
		if e != nil {
			return e
		}
		for _, n := range valueNames {
			if _, ok := old.Values[n]; !ok {
				old.Values[n] = name
			}
		}
		if old.Key == nil {
			old.Key = key
		}
		raw, e := json.Marshal(old.Values)
		if e != nil {
			return e
		}
		var identity any
		if old.Key != nil {
			b, e := json.Marshal(old.Key)
			if e != nil {
				return e
			}
			identity = string(b)
		}
		if _, e = t.exec(`UPDATE workspaces SET policy_values = ?, xolotl_identity = ? WHERE name = ?`, string(raw), identity, name); e != nil {
			return e
		}
		out, e = t.Selection(name)
		return e
	})
	return
}

// Applied records only the revision actually observed. A concurrent edit
// remains unapplied and keeps any failure from a newer attempt.
func (s *Store) Applied(ctx context.Context, name string, revision int64) error {
	return s.Update(ctx, func(t *Tx) error {
		_, err := t.exec(`UPDATE workspaces SET applied_revision = ?, apply_failure = CASE WHEN selection_revision = ? THEN NULL ELSE apply_failure END WHERE name = ?`, revision, revision, name)
		return err
	})
}

// ApplyFailed invalidates application of the failed revision and retains its
// failure only while that selection is current. Newer successful attempts stay.
func (s *Store) ApplyFailed(ctx context.Context, name string, revision int64, failure ApplyFailure) error {
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	return s.Update(ctx, func(t *Tx) error {
		_, err := t.exec(`UPDATE workspaces SET applied_revision = CASE WHEN applied_revision = ? THEN NULL ELSE applied_revision END, apply_failure = CASE WHEN selection_revision = ? THEN ? ELSE apply_failure END WHERE name = ?`, revision, revision, string(raw), name)
		return err
	})
}

// BindingID records the ID resolved for this workspace's container name.
func (s *Store) BindingID(ctx context.Context, workspace, name, id string) error {
	return s.Update(ctx, func(t *Tx) error {
		_, err := t.exec(`UPDATE container_names SET binding_id = ? WHERE workspace = ? AND name = ?`, nullable(id), workspace, name)
		return err
	})
}

// PrepareSelectionImport runs with import 5's global mark. A declared current
// name waits for a binding read; identities are taken over even during outages.
func (t *Tx) PrepareSelectionImport(name string, key *Key, pending bool) error {
	var identity any
	if key != nil {
		raw, err := json.Marshal(key)
		if err != nil {
			return err
		}
		identity = string(raw)
	}
	_, err := t.exec(`UPDATE workspaces SET selection_imported = ?, xolotl_identity = ?, selection_revision = selection_revision + 1 WHERE name = ?`, !pending, identity, name)
	return err
}
