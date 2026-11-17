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
	"unicode"
	"unicode/utf8"
)

// PolicyGroup requires exactly one (single) or at least one (multi) choice.
// An ID is assigned once and never reused, including after a group is removed.
type PolicyGroup struct {
	ID       int64    `json:"id,omitempty"`
	Name     string   `json:"name"`
	Mode     string   `json:"mode"`
	Policies []string `json:"policies"`
}

// PolicySettings is a revisioned classification of policies.
type PolicySettings struct {
	Required []string      `json:"required"`
	Groups   []PolicyGroup `json:"groups"`
	Revision int64         `json:"revision"`
}

// PolicyError is a settings refusal that names the failed rule or policies.
type PolicyError struct{ Kind, Message string }

func (e *PolicyError) Error() string { return e.Message }

// Names returns every policy named by these settings, in name order.
func (s PolicySettings) Names() []string {
	out := append([]string{}, s.Required...)
	for _, g := range s.Groups {
		out = append(out, g.Policies...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// NeedsChoice returns group names for which the selection has no valid choice.
func (s PolicySettings) NeedsChoice(selected []string) []string {
	out := []string{}
	for _, g := range s.Groups {
		count := 0
		for _, p := range g.Policies {
			if slices.Contains(selected, p) {
				count++
			}
		}
		if count == 0 || (g.Mode == "single" && count != 1) {
			out = append(out, g.Name)
		}
	}
	return out
}

// Policies reads settings and all selections in one transaction.
func (s *Store) Policies(ctx context.Context) (settings PolicySettings, selections map[string]Selection, err error) {
	selections = map[string]Selection{}
	err = s.Update(ctx, func(t *Tx) error {
		var e error
		settings, e = t.PolicySettings()
		if e != nil {
			return e
		}
		names, e := t.names(`SELECT name FROM workspaces ORDER BY name`)
		if e != nil {
			return e
		}
		for _, name := range names {
			selection, e := t.Selection(name)
			if e != nil {
				return e
			}
			selections[name] = selection
		}
		return nil
	})
	return
}

// PolicySettings reads the settings in this transaction.
func (t *Tx) PolicySettings() (PolicySettings, error) {
	var raw string
	if err := t.tx.QueryRowContext(t.ctx, `SELECT document FROM policy_settings WHERE id = 1`).Scan(&raw); err != nil {
		return PolicySettings{}, err
	}
	var out PolicySettings
	err := json.Unmarshal([]byte(raw), &out)
	return out, err
}

// Selection is a workspace's stored policy names and revision. Task 24 uses
// this revision to track which selection the binding has applied.
type Selection struct {
	Policies []string
	Revision int64
}

// Selection reads a workspace's selection in this transaction.
func (t *Tx) Selection(name string) (Selection, error) {
	out := Selection{Policies: []string{}}
	if err := t.tx.QueryRowContext(t.ctx, `SELECT selection_revision FROM workspaces WHERE name = ?`, name).Scan(&out.Revision); err != nil {
		return out, err
	}
	names, err := t.names(`SELECT policy FROM workspace_policies WHERE workspace = ? ORDER BY policy`, name)
	if names != nil {
		out.Policies = names
	}
	return out, err
}

// SavePolicySettings checks and stores a settings change atomically. Only
// new required or group memberships need to be inside the current grant.
// Required policies propagate to every selection, including no-folder and
// being-created workspaces; removing a requirement retains each selection.
func (s *Store) SavePolicySettings(ctx context.Context, next PolicySettings, allowed []string) (saved PolicySettings, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		old, e := t.PolicySettings()
		if e != nil {
			return e
		}
		if old.Revision != next.Revision {
			return &PolicyError{"settings-changed", "Policy settings changed. Reload them and try again."}
		}
		invalid := func(message string) error { return &PolicyError{"invalid-settings", message} }
		var nextID int64
		if e := t.tx.QueryRowContext(t.ctx, `SELECT next_group_id FROM policy_settings WHERE id = 1`).Scan(&nextID); e != nil {
			return e
		}
		ids := map[int64]bool{}
		names := map[string]bool{}
		assigned := map[string]string{}
		assign := func(p, location string) error {
			if strings.TrimSpace(p) == "" {
				return invalid("A policy name must not be empty.")
			}
			if previous, ok := assigned[p]; ok {
				return invalid(fmt.Sprintf("Policy %q occurs in both %s and %s; a policy is required, in one group, or neither.", p, previous, location))
			}
			assigned[p] = location
			return nil
		}
		if next.Required == nil {
			next.Required = []string{}
		}
		if next.Groups == nil {
			next.Groups = []PolicyGroup{}
		}
		for _, p := range next.Required {
			if e := assign(p, "required policies"); e != nil {
				return e
			}
		}
		for i := range next.Groups {
			g := &next.Groups[i]
			if g.ID != 0 {
				known := slices.ContainsFunc(old.Groups, func(old PolicyGroup) bool { return old.ID == g.ID })
				if !known || ids[g.ID] {
					return invalid(fmt.Sprintf("Group %q has unknown or repeated id %d.", g.Name, g.ID))
				}
				ids[g.ID] = true
			}
			if strings.TrimSpace(g.Name) == "" || utf8.RuneCountInString(g.Name) > 100 || strings.ContainsFunc(g.Name, unicode.IsControl) {
				return invalid("A group name must be one line of 1 to 100 characters.")
			}
			if names[g.Name] {
				return invalid(fmt.Sprintf("Two groups have the name %q; group names must be unique.", g.Name))
			}
			names[g.Name] = true
			if g.Mode != "single" && g.Mode != "multi" {
				return invalid(fmt.Sprintf("Group %q has unknown mode %q; choose single or multi.", g.Name, g.Mode))
			}
			if len(g.Policies) == 0 {
				return invalid(fmt.Sprintf("Group %q has no policy; each group needs at least one.", g.Name))
			}
			for _, p := range g.Policies {
				if e := assign(p, fmt.Sprintf("group %q", g.Name)); e != nil {
					return e
				}
			}
			if g.ID == 0 {
				g.ID = nextID
				nextID++
			}
		}
		added := []string{}
		for _, p := range next.Required {
			if !slices.Contains(old.Required, p) {
				added = append(added, p)
			}
		}
		for _, g := range next.Groups {
			var previous []string
			for _, before := range old.Groups {
				if before.ID == g.ID {
					previous = before.Policies
					break
				}
			}
			for _, p := range g.Policies {
				if !slices.Contains(previous, p) {
					added = append(added, p)
				}
			}
		}
		outside := []string{}
		for _, p := range added {
			if !slices.Contains(allowed, p) {
				outside = append(outside, p)
			}
		}
		slices.Sort(outside)
		outside = slices.Compact(outside)
		if len(outside) > 0 {
			return &PolicyError{"policy-outside-grant", "Policies outside the operator key's grant or missing in Wyrwood: " + strings.Join(outside, ", ")}
		}
		changed := map[string]bool{}
		for _, p := range next.Required {
			names, e := t.names(`SELECT name FROM workspaces WHERE NOT EXISTS (SELECT 1 FROM workspace_policies WHERE workspace = workspaces.name AND policy = ?)`, p)
			if e != nil {
				return e
			}
			for _, name := range names {
				changed[name] = true
			}
			// A revision changes only when the selection changes, once per settings
			// save even when several required policies are added.
			if _, e := t.exec(`INSERT INTO workspace_policies (workspace, policy) SELECT name, ? FROM workspaces WHERE NOT EXISTS (SELECT 1 FROM workspace_policies WHERE workspace = workspaces.name AND policy = ?)`, p, p); e != nil {
				return e
			}
		}
		for name := range changed {
			if _, e := t.exec(`UPDATE workspaces SET selection_revision = selection_revision + 1 WHERE name = ?`, name); e != nil {
				return e
			}
		}
		next.Revision++
		raw, e := json.Marshal(next)
		if e != nil {
			return e
		}
		_, e = t.exec(`UPDATE policy_settings SET document = ?, next_group_id = ? WHERE id = 1`, string(raw), nextID)
		saved = next
		return e
	})
	return saved, err
}
