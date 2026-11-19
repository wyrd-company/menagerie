// ---
// relationships:
//   implements: menagerie
//   references: workspace
// ---

package registry

import (
	"context"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// AppliedImportID is the mark of the applied versions' import in Menagerie's
// data.
const AppliedImportID = 4

// ImportApplied takes over, once, the version last applied to each
// workspace Menagerie's data holds that has a registered template and
// whose applied version and commit it does not know yet, from the
// workspace's applied record, by the rule every join follows (JoinVersion):
// the found version whose tag is at the record's commit, else unknown with
// the commit when the record names the template's source and a full object
// id, which the template's next version check completes. A workspace with
// no template takes its version when it joins one. It records its mark in
// the same transaction, and a later start imports nothing.
func ImportApplied(ctx context.Context, data *store.Store, root *workspace.Root, logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	known := 0
	ran, err := data.Import(ctx, AppliedImportID, func(tx *store.Tx) error {
		names, err := tx.Workspaces()
		if err != nil {
			return err
		}
		for _, name := range names {
			current, err := tx.AppliedVersion(name)
			if err != nil {
				return err
			}
			if current != (store.AppliedVersion{}) {
				continue
			}
			id, err := tx.WorkspaceTemplate(name)
			if err != nil {
				return err
			}
			if id == "" {
				continue
			}
			tpl, err := tx.Template(id)
			if err != nil {
				return err
			}
			folder, err := root.Folder(name)
			if err != nil {
				continue
			}
			source, version := Join(folder)
			if source == "" {
				continue
			}
			if applied := version(tpl); applied != (store.AppliedVersion{}) {
				if err := tx.SetAppliedVersion(name, applied); err != nil {
					return err
				}
				known++
			}
		}
		return nil
	})
	if ran {
		logf("menagerie: imported the applied versions of %d workspaces from their applied records", known)
	}
	return err
}
