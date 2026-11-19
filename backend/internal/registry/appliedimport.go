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
// workspace Menagerie's data holds whose applied version and commit it does
// not know yet, from the workspace's applied record: its ref when that is a
// version, else unknown, with the record's commit, which a version check of
// the workspace's template completes. Its found versions complete it at
// once when one's tag is at that commit. It records its mark in the same
// transaction, and a later start imports nothing.
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
			folder, err := root.Folder(name)
			if err != nil {
				continue
			}
			if _, applied := Record(folder); applied != (store.AppliedVersion{}) {
				if err := tx.SetAppliedVersion(name, applied); err != nil {
					return err
				}
				known++
			}
		}
		return tx.CompleteApplied()
	})
	if ran {
		logf("menagerie: imported the applied versions of %d workspaces from their applied records", known)
	}
	return err
}
