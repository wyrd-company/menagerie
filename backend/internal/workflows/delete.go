// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, workspace-delete-loses-ignored-files]
// ---

package workflows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
)

// delete validates a delete request. It changes nothing.
func (w *Workflows) delete(req api.JobRequest) (jobsRunner, error) {
	folder, err := api.ExistingFolder(w.opts.Root, req.Workspace)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, run *jobs.Run) error {
		err := w.opts.Runtime.Delete(ctx, folder, run, func(ctx context.Context) error {
			return w.saveBeforeDelete(ctx, run, folder, req.Workspace)
		})
		if err != nil && !nothingRemoved(err) {
			fmt.Fprintln(run.Log(""), "the delete did not finish, and what it removed stays removed; delete the workspace again to continue")
		}
		return api.ContainerFailure(err)
	}, nil
}

// nothingRemoved says that a delete failed in a way that removed nothing, which
// its message says.
func nothingRemoved(err error) bool {
	var failure *jobs.Failure
	if !errors.As(err, &failure) {
		return false
	}
	return failure.Kind == provisioning.KindUnavailable || failure.Kind == provisioning.KindDeleteRefused || failure.Kind == KindBackupFailed
}

// saveBeforeDelete commits and backs up what the backup can hold. A folder with
// no applied record has no backup.
func (w *Workflows) saveBeforeDelete(ctx context.Context, run *jobs.Run, folder, name string) error {
	if _, err := os.Lstat(filepath.Join(folder, backup.RecordName)); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(run.Log(""), "the folder has no applied record, so it has no backup")
		return nil
	} else if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("reading the applied record failed: %w; nothing was removed", err)}
	}
	run.Phase("commit")
	commit, err := backup.CommitAll(folder, name, fmt.Sprintf("Back up workspace %s before its delete", name), run.Log("commit: "))
	if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("committing the workspace's changes failed: %w; nothing was removed", err)}
	}
	if commit.Changed {
		fmt.Fprintf(run.Log(""), "committed %s on branch %s\n", commit.Hash, name)
	} else {
		fmt.Fprintf(run.Log(""), "nothing to commit; branch %s is at %s\n", name, commit.Hash)
	}
	run.Phase("backup")
	if err := push(ctx, w.opts.Remote, folder, name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("backing up the workspace failed; branch %s is at %s; nothing was removed", name, commit.Hash)}
	}
	return nil
}
