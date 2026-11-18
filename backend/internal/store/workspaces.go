// ---
// relationships:
//   implements: menagerie
//   references: workspace
// ---

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// Name is a container name Menagerie bound for a workspace.
type Name struct {
	Name string
	// Agent is the Wyrwood agent it was bound on, or "" for a name imported
	// with no provisioning.wyrwood, which no job binds or unbinds until a start
	// with that section records the agent.
	Agent string
	// Current says that it is the workspace's current container name.
	Current bool
	// BindingID is the last resolved Wyrwood ID; a missing ID is resolved by name.
	BindingID string
}

// Key is a Xolotl client key Menagerie issued for a workspace.
type Key struct {
	Policy string
	Name   string
}

func (k Key) String() string { return k.Policy + "/" + k.Name }

// Held is what a workspace holds: the container names bound and the keys
// issued for it, each in name order.
type Held struct {
	Names []Name
	Keys  []Key
}

// Empty says that the workspace holds no name and no key.
func (h Held) Empty() bool { return len(h.Names) == 0 && len(h.Keys) == 0 }

// Current is the workspace's current container name, or "".
func (h Held) Current() string {
	for _, n := range h.Names {
		if n.Current {
			return n.Name
		}
	}
	return ""
}

// AddWorkspace gives a workspace its data, unless it has some.
func (t *Tx) AddWorkspace(name string) error {
	_, err := t.exec(`INSERT INTO workspaces (name) VALUES (?) ON CONFLICT (name) DO NOTHING`, name)
	return err
}

// Exists says that the data holds a workspace of that name.
func (t *Tx) Exists(name string) (bool, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM workspaces WHERE name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("Menagerie's data: %w", err)
	}
	return n > 0, nil
}

// Held reads what a workspace holds.
func (t *Tx) Held(workspace string) (Held, error) {
	var out Held
	rows, err := t.tx.QueryContext(t.ctx, `SELECT name, coalesce(agent, ''), current, coalesce(binding_id, '') FROM container_names WHERE workspace = ? ORDER BY name`, workspace)
	if err != nil {
		return Held{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	for rows.Next() {
		var n Name
		if err := rows.Scan(&n.Name, &n.Agent, &n.Current, &n.BindingID); err != nil {
			_ = rows.Close()
			return Held{}, fmt.Errorf("Menagerie's data: %w", err)
		}
		out.Names = append(out.Names, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Held{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	rows, err = t.tx.QueryContext(t.ctx, `SELECT policy, name FROM xolotl_keys WHERE workspace = ? ORDER BY policy, name`, workspace)
	if err != nil {
		return Held{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.Policy, &k.Name); err != nil {
			_ = rows.Close()
			return Held{}, fmt.Errorf("Menagerie's data: %w", err)
		}
		out.Keys = append(out.Keys, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Held{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	return out, nil
}

// NameHolder is the workspace that holds a container name, or "".
func (t *Tx) NameHolder(name string) (string, error) {
	return t.holder(`SELECT workspace FROM container_names WHERE name = ?`, name)
}

// KeyHolder is the workspace that holds a Xolotl key, or "".
func (t *Tx) KeyHolder(key Key) (string, error) {
	return t.holder(`SELECT workspace FROM xolotl_keys WHERE policy = ? AND name = ?`, key.Policy, key.Name)
}

func (t *Tx) holder(query string, args ...any) (string, error) {
	var holder string
	err := t.tx.QueryRowContext(t.ctx, query, args...).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("Menagerie's data: %w", err)
	}
	return holder, nil
}

// HoldName records a container name as the workspace's, on agent ("" for
// none), and as its current one when current is true, which ends any other
// name's being current. A name another workspace holds is a *TakenError. A name
// already recorded on another agent is an *AgentError: the binding on that
// agent stays recorded until Wyrwood confirms it gone, so only a name with no
// agent, imported without provisioning.wyrwood, takes one here. The workspace
// gets its data when it has none.
func (t *Tx) HoldName(workspace, name, agent string, current bool) error {
	holder, err := t.NameHolder(name)
	if err != nil {
		return err
	}
	if holder != "" && holder != workspace {
		return &TakenError{What: "container name " + name, Holder: holder}
	}
	recorded, err := t.agentOf(name)
	if err != nil {
		return err
	}
	if recorded != "" && recorded != agent {
		return &AgentError{Name: name, Recorded: recorded, Agent: agent}
	}
	if err := t.AddWorkspace(workspace); err != nil {
		return err
	}
	if current {
		// An applied observation belongs to the old current name and agent.
		// Invalidate atomically before publishing a different binding identity.
		if _, err := t.exec(`UPDATE workspaces SET applied_revision = NULL WHERE name = ?
   AND NOT EXISTS (SELECT 1 FROM container_names WHERE workspace = ? AND name = ? AND coalesce(agent, '') = ? AND current = 1)`, workspace, workspace, name, agent); err != nil {
			return err
		}
		if _, err := t.exec(`UPDATE container_names SET current = 0 WHERE workspace = ? AND name != ?`, workspace, name); err != nil {
			return err
		}
	}
	_, err = t.exec(`INSERT INTO container_names (name, workspace, agent, current) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET agent = excluded.agent, current = excluded.current`,
		name, workspace, nullable(agent), current)
	return err
}

// agentOf is the agent a container name is recorded on, or "" for none or no
// such name.
func (t *Tx) agentOf(name string) (string, error) {
	return t.holder(`SELECT coalesce(agent, '') FROM container_names WHERE name = ?`, name)
}

// ForgetName removes a container name of the workspace, once Wyrwood confirmed
// its binding gone.
func (t *Tx) ForgetName(workspace, name string) error {
	_, err := t.exec(`DELETE FROM container_names WHERE workspace = ? AND name = ?`, workspace, name)
	return err
}

// HoldKey records a Xolotl key as the workspace's. A key another workspace
// holds is a *TakenError. The workspace gets its data when it has none.
func (t *Tx) HoldKey(workspace string, key Key) error {
	holder, err := t.KeyHolder(key)
	if err != nil {
		return err
	}
	if holder != "" && holder != workspace {
		return &TakenError{What: "Xolotl key " + key.String(), Holder: holder}
	}
	if err := t.AddWorkspace(workspace); err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO xolotl_keys (policy, name, workspace) VALUES (?, ?, ?) ON CONFLICT (policy, name) DO NOTHING`,
		key.Policy, key.Name, workspace)
	return err
}

// ForgetKey removes a Xolotl key of the workspace, once Xolotl confirmed it
// gone.
func (t *Tx) ForgetKey(workspace string, key Key) error {
	_, err := t.exec(`DELETE FROM xolotl_keys WHERE workspace = ? AND policy = ? AND name = ?`, workspace, key.Policy, key.Name)
	return err
}

// SetRemoving records the folder, by its device and inode, that a delete of
// the workspace began to remove; "" ends the state.
func (t *Tx) SetRemoving(workspace, folder string) error {
	if folder != "" {
		if err := t.AddWorkspace(workspace); err != nil {
			return err
		}
	}
	_, err := t.exec(`UPDATE workspaces SET removing = ? WHERE name = ?`, nullable(folder), workspace)
	return err
}

// Removing is the folder a delete of the workspace began to remove, or "".
func (t *Tx) Removing(workspace string) (string, error) {
	var removing sql.NullString
	err := t.tx.QueryRowContext(t.ctx, `SELECT removing FROM workspaces WHERE name = ?`, workspace).Scan(&removing)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("Menagerie's data: %w", err)
	}
	return removing.String, nil
}

// RemoveWorkspace removes a workspace's data. It refuses while the workspace
// holds a container name or a key, because the foreign keys do.
func (t *Tx) RemoveWorkspace(name string) error {
	_, err := t.exec(`DELETE FROM workspaces WHERE name = ?`, name)
	return err
}

// The Store's methods below are each one transaction.

// Exists says that the data holds a workspace of that name, one being created
// included.
func (s *Store) Exists(ctx context.Context, name string) (exists bool, err error) {
	err = s.Update(ctx, func(t *Tx) error { exists, err = t.Exists(name); return err })
	return exists, err
}

// Held reads what a workspace holds.
func (s *Store) Held(ctx context.Context, workspace string) (held Held, err error) {
	err = s.Update(ctx, func(t *Tx) error { held, err = t.Held(workspace); return err })
	return held, err
}

// HoldName records a container name as the workspace's; see Tx.HoldName.
func (s *Store) HoldName(ctx context.Context, workspace, name, agent string, current bool) error {
	return s.Update(ctx, func(t *Tx) error { return t.HoldName(workspace, name, agent, current) })
}

// ForgetName removes a container name of the workspace.
func (s *Store) ForgetName(ctx context.Context, workspace, name string) error {
	return s.Update(ctx, func(t *Tx) error { return t.ForgetName(workspace, name) })
}

// HoldKey records a Xolotl key as the workspace's; see Tx.HoldKey.
func (s *Store) HoldKey(ctx context.Context, workspace string, key Key) error {
	return s.Update(ctx, func(t *Tx) error { return t.HoldKey(workspace, key) })
}

// ForgetKey removes a Xolotl key of the workspace.
func (s *Store) ForgetKey(ctx context.Context, workspace string, key Key) error {
	return s.Update(ctx, func(t *Tx) error { return t.ForgetKey(workspace, key) })
}

// NameHolder is the workspace that holds a container name, or "".
func (s *Store) NameHolder(ctx context.Context, name string) (holder string, err error) {
	err = s.Update(ctx, func(t *Tx) error { holder, err = t.NameHolder(name); return err })
	return holder, err
}

// KeyHolder is the workspace that holds a Xolotl key, or "".
func (s *Store) KeyHolder(ctx context.Context, key Key) (holder string, err error) {
	err = s.Update(ctx, func(t *Tx) error { holder, err = t.KeyHolder(key); return err })
	return holder, err
}

// SetRemoving records the folder a delete began to remove; see Tx.SetRemoving.
func (s *Store) SetRemoving(ctx context.Context, workspace, folder string) error {
	return s.Update(ctx, func(t *Tx) error { return t.SetRemoving(workspace, folder) })
}

// Removing is the folder a delete of the workspace began to remove, or "".
func (s *Store) Removing(ctx context.Context, workspace string) (folder string, err error) {
	err = s.Update(ctx, func(t *Tx) error { folder, err = t.Removing(workspace); return err })
	return folder, err
}

// RemoveWorkspace removes a workspace's data; see Tx.RemoveWorkspace.
func (s *Store) RemoveWorkspace(ctx context.Context, name string) error {
	return s.Update(ctx, func(t *Tx) error { return t.RemoveWorkspace(name) })
}

// Claimed says that the data holds a workspace of that name that a create
// must not take: one being created, or one that holds a container name or a
// Xolotl key. The data of a workspace whose folder is gone and that holds
// neither is what discovery removes, so it claims nothing.
func (t *Tx) Claimed(name string) (bool, error) {
	var n int
	err := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM workspaces WHERE name = ? AND (creating = 1
		OR EXISTS (SELECT 1 FROM container_names WHERE workspace = workspaces.name)
		OR EXISTS (SELECT 1 FROM xolotl_keys WHERE workspace = workspaces.name))`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("Menagerie's data: %w", err)
	}
	return n > 0, nil
}

// Claimed says that the data holds a workspace of that name that a create
// must not take; see Tx.Claimed.
func (s *Store) Claimed(ctx context.Context, name string) (claimed bool, err error) {
	err = s.Update(ctx, func(t *Tx) error { claimed, err = t.Claimed(name); return err })
	return claimed, err
}

// BeginCreate gives a workspace its data marked as being created, so that
// discovery keeps it while its create has no folder. A workspace the data
// claims, being created or holding a name or a key, is ErrExists; the caller
// has found no folder, so any other data of that name is replaced.
func (s *Store) BeginCreate(ctx context.Context, name string) error {
	return s.BeginCreateFrom(ctx, name, "")
}

// BeginCreateFrom begins a create, as BeginCreate does, of a workspace of
// the registered template id, so that the template cannot be removed while
// the create runs; "" names none.
func (s *Store) BeginCreateFrom(ctx context.Context, name, template string) error {
	return s.Update(ctx, func(t *Tx) error {
		claimed, err := t.Claimed(name)
		if err != nil {
			return err
		}
		if claimed {
			return fmt.Errorf("%w: %s", ErrExists, name)
		}
		// Replace any unclaimed data, including its policy selection. A new
		// create starts with the current required policies through the trigger.
		if _, err := t.exec(`DELETE FROM workspaces WHERE name = ?`, name); err != nil {
			return err
		}
		_, err = t.exec(`INSERT INTO workspaces (name, creating, template) VALUES (?, 1, ?)`, name, nullable(template))
		return err
	})
}

// Unknown lists the names of folders that have no workspace in the data yet,
// in the order given: the folders a discovery lists for the first time.
func (s *Store) Unknown(ctx context.Context, folders []string) (unknown []string, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		unknown = nil
		for _, name := range folders {
			exists, err := t.Exists(name)
			if err != nil {
				return err
			}
			if !exists {
				unknown = append(unknown, name)
			}
		}
		return nil
	})
	return unknown, err
}

// Created ends a workspace's being-created mark, once its create made the
// folder.
func (s *Store) Created(ctx context.Context, name string) error {
	return s.Update(ctx, func(t *Tx) error {
		_, err := t.exec(`UPDATE workspaces SET creating = 0 WHERE name = ?`, name)
		return err
	})
}

// AbandonCreate removes the data of a workspace whose create ended before it
// made the folder. A workspace that is not being created, or that holds a name
// or a key, keeps its data.
func (s *Store) AbandonCreate(ctx context.Context, name string) error {
	return s.Update(ctx, func(t *Tx) error { return t.abandon(name) })
}

func (t *Tx) abandon(name string) error {
	_, err := t.exec(`DELETE FROM workspaces WHERE name = ? AND creating = 1
		AND NOT EXISTS (SELECT 1 FROM container_names WHERE workspace = workspaces.name)
		AND NOT EXISTS (SELECT 1 FROM xolotl_keys WHERE workspace = workspaces.name)`, name)
	return err
}

// EndCreates runs at start-up, when no job runs: each workspace marked as being
// created whose folder does not exist loses its data, and the others lose the
// mark. It returns the workspaces whose data it removed.
func (s *Store) EndCreates(ctx context.Context, folderExists func(name string) bool) (removed []string, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		names, err := t.names(`SELECT name FROM workspaces WHERE creating = 1 ORDER BY name`)
		if err != nil {
			return err
		}
		for _, name := range names {
			if folderExists(name) {
				if _, err := t.exec(`UPDATE workspaces SET creating = 0 WHERE name = ?`, name); err != nil {
					return err
				}
				continue
			}
			if err := t.abandon(name); err != nil {
				return err
			}
			removed = append(removed, name)
		}
		return nil
	})
	return removed, err
}

// Discover joins the workspace folders to the data, as the workspace list
// does: a folder with no data gets it, a workspace whose folder is gone that
// holds neither a name nor a key and that no create is making loses it, and
// the workspaces whose folder is gone and that hold a name or a key are
// returned, in name order.
func (s *Store) Discover(ctx context.Context, folders []string) (noFolder []string, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		for _, name := range folders {
			exists, err := t.Exists(name)
			if err != nil {
				return err
			}
			// Listing must not retry or cover an existing selection's failed apply.
			// A newly discovered folder has no current name or prior observation.
			if exists {
				continue
			}
			if err := t.AddWorkspace(name); err != nil {
				return err
			}
			if err := t.appliedWithoutName(name); err != nil {
				return err
			}
		}
		known, err := t.names(`SELECT name FROM workspaces ORDER BY name`)
		if err != nil {
			return err
		}
		for _, name := range known {
			if slices.Contains(folders, name) {
				continue
			}
			held, err := t.Held(name)
			if err != nil {
				return err
			}
			if !held.Empty() {
				noFolder = append(noFolder, name)
				continue
			}
			if _, err := t.exec(`DELETE FROM workspaces WHERE name = ? AND creating = 0`, name); err != nil {
				return err
			}
		}
		return nil
	})
	return noFolder, err
}

// RecordAgent records agent on each container name that has none, as the
// first start with provisioning.wyrwood does for names imported without it.
// It returns how many it recorded.
func (s *Store) RecordAgent(ctx context.Context, agent string) (recorded int64, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		result, err := t.exec(`UPDATE container_names SET agent = ? WHERE agent IS NULL`, agent)
		if err != nil {
			return err
		}
		recorded, err = result.RowsAffected()
		return err
	})
	return recorded, err
}

func (t *Tx) names(query string, args ...any) ([]string, error) {
	rows, err := t.tx.QueryContext(t.ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("Menagerie's data: %w", err)
		}
		out = append(out, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	return out, nil
}
