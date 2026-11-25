// ---
// relationships:
//   implements: [menagerie, choose-a-backup-repository-per-workspace]
//   references: [workspace-api, backup-force-push-overwrites-branch]
// ---

package workflows

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// Request and job error kinds of the move-backup operation.
const (
	// KindBackupRepositoryUnchanged refuses a move to the repository the
	// workspace already backs up to.
	KindBackupRepositoryUnchanged = "backup-repository-unchanged"
	// KindBackupMoveFailed fails a move that could not check, write or
	// verify the destination, or record it; the workspace keeps its
	// repository.
	KindBackupMoveFailed = "backup-move-failed"
	// KindBackupBranchConflict fails a move to a repository whose branch
	// of the workspace's name holds history the workspace does not have.
	KindBackupBranchConflict = "backup-branch-conflict"
)

// moveBackup names an existing workspace folder and a registered backup
// repository other than the one it backs up to.
func (w *Workflows) moveBackup(req api.JobRequest) (jobsRunner, error) {
	folder, err := api.ExistingFolder(w.opts.Root, req.Workspace)
	if err != nil {
		return nil, err
	}
	if req.BackupRepositoryID == "" {
		return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-request", Message: "backupRepositoryId is required: name the registered backup repository to move the workspace's backups to"}
	}
	ctx := context.Background()
	if _, err := w.createBackups(req.BackupRepositoryID); err != nil {
		return nil, err
	}
	current, ok, err := w.opts.Data.WorkspaceBackupRepository(ctx, req.Workspace)
	if err != nil {
		return nil, err
	}
	if ok && current.ID == req.BackupRepositoryID {
		return nil, &api.RequestError{Status: http.StatusConflict, Kind: KindBackupRepositoryUnchanged, Message: fmt.Sprintf("workspace %s already backs up to %s", req.Workspace, current.Name)}
	}
	return (&moving{w: w, name: req.Workspace, folder: folder, destination: req.BackupRepositoryID}).run, nil
}

// moving is one move-backup job: it proves the destination writable and
// free of another history, commits and pushes a current backup there,
// reads it back, and only then records the destination as the workspace's
// repository. Until that last step every backup still goes to the
// repository the workspace had, and the branch there is never changed.
type moving struct {
	w           *Workflows
	name        string
	folder      string
	destination string
}

func (m *moving) run(ctx context.Context, run *jobs.Run) error {
	run.Phase("verify")
	to, err := m.w.opts.Data.BackupRepository(ctx, m.destination)
	if errors.Is(err, store.ErrNoBackupRepository) {
		return &jobs.Failure{Kind: KindBackupMoveFailed, Err: fmt.Errorf("backup repository %s is no longer registered; nothing was changed", m.destination)}
	}
	if err != nil {
		return err
	}
	from, had, err := m.w.opts.Data.WorkspaceBackupRepository(ctx, m.name)
	if err != nil {
		return err
	}
	stays := "the workspace has no backup repository yet"
	if had {
		stays = fmt.Sprintf("the workspace still backs up to %s", from.Name)
		fmt.Fprintf(run.Log(""), "moving the backups of %s from %s (%s) to %s (%s)\n", m.name, from.Name, from.ID, to.Name, to.ID)
	} else {
		fmt.Fprintf(run.Log(""), "giving %s its first backup repository, %s (%s)\n", m.name, to.Name, to.ID)
	}
	fail := func(kind string, format string, args ...any) error {
		return &jobs.Failure{Kind: kind, Err: fmt.Errorf("%s; %s", fmt.Sprintf(format, args...), stays)}
	}

	pending, err := backup.CreatePending(m.folder, m.name)
	if err != nil {
		return fail(KindBackupMoveFailed, "reading the workspace's repository failed: %v", err)
	}
	if pending {
		return fail(KindBackupMoveFailed, "the workspace's create has not finished; start the workspace to finish it, then move its backups")
	}
	destination := m.w.opts.Remotes.Remote(to.URL)
	if err := destination.CheckWrite(ctx, run.Log("verify: ")); err != nil {
		return fail(KindBackupMoveFailed, "Menagerie cannot write to %s: %v", to.Name, err)
	}
	fmt.Fprintf(run.Log(""), "Menagerie can write to %s\n", to.Name)
	if failure := m.conflict(ctx, destination, to, fail); failure != nil {
		return failure
	}

	run.Phase("commit")
	commit, err := backup.CommitAll(m.folder, m.name, fmt.Sprintf("Back up workspace %s before moving its backups to %s", m.name, to.Name), run.Log("commit: "))
	if err != nil {
		return fail(KindBackupMoveFailed, "committing the workspace's changes failed: %v; nothing was pushed", err)
	}
	if commit.Changed {
		fmt.Fprintf(run.Log(""), "committed %s on branch %s\n", commit.Hash, m.name)
	} else {
		fmt.Fprintf(run.Log(""), "nothing to commit; branch %s is at %s\n", m.name, commit.Hash)
	}

	run.Phase("backup")
	// Without force: the destination takes the push only while its branch
	// is still absent or in the workspace's history, so a branch another
	// writer made after the conflict check is never overwritten.
	if _, err := destination.Extend(ctx, m.folder, m.name, run.Log("backup: ")); err != nil {
		return fail(KindBackupMoveFailed, "pushing branch %s to %s failed: %v; if %s's branch %s changed since the check, nothing of it was overwritten", m.name, to.Name, err, to.Name, m.name)
	}
	landed, found, err := destination.BranchCommit(ctx, m.name)
	switch {
	case err != nil:
		return fail(KindBackupMoveFailed, "reading branch %s back from %s failed: %v", m.name, to.Name, err)
	case !found:
		return fail(KindBackupMoveFailed, "%s has no branch %s after the push", to.Name, m.name)
	case landed != commit.Hash:
		return fail(KindBackupMoveFailed, "branch %s in %s is at %s after the push, not %s", m.name, to.Name, landed, commit.Hash)
	}
	fmt.Fprintf(run.Log(""), "branch %s in %s is at %s\n", m.name, to.Name, landed)

	run.Phase("switch")
	// The switch is the job's final step: a time limit that passed before it
	// times the job out and nothing is switched; once it begins, no limit
	// can turn a recorded switch into a timed-out outcome.
	err = run.Final(func() error { return m.w.opts.Data.SwitchWorkspaceBackupRepository(ctx, m.name, to.ID, to.URL) })
	switch {
	case errors.Is(err, jobs.ErrTimedOut):
		return fail(KindBackupMoveFailed, "the backup landed in %s, but the job passed its time limit before it switched the workspace's repository; move again", to.Name)
	case errors.Is(err, store.ErrBackupRepositoryChanged) || errors.Is(err, store.ErrNoBackupRepository):
		return fail(KindBackupMoveFailed, "backup repository %s was removed during the move; nothing was switched", to.ID)
	case err != nil:
		return fail(KindBackupMoveFailed, "the backup landed in %s, but recording it as the workspace's repository failed: %v; move again", to.Name, err)
	}
	if had {
		fmt.Fprintf(run.Log(""), "workspace %s now backs up to %s; its earlier backups stay on branch %s in %s\n", m.name, to.Name, m.name, from.Name)
	} else {
		fmt.Fprintf(run.Log(""), "workspace %s now backs up to %s\n", m.name, to.Name)
	}
	return nil
}

// conflict refuses a destination whose branch of the workspace's name holds
// a commit that is not in the workspace's history, such as another
// workspace's of that name. A branch at the workspace's own commit or an
// ancestor of it, which an earlier move or a copy left, is taken over.
func (m *moving) conflict(ctx context.Context, destination *backup.Remote, to store.BackupRepository, fail func(string, string, ...any) error) error {
	there, found, err := destination.BranchCommit(ctx, m.name)
	if err != nil {
		return fail(KindBackupMoveFailed, "reading the branches of %s failed: %v", to.Name, err)
	}
	if !found {
		return nil
	}
	ours, err := backup.InHistory(m.folder, m.name, there)
	if err != nil {
		return fail(KindBackupMoveFailed, "reading the workspace's history failed: %v", err)
	}
	if !ours {
		return fail(KindBackupBranchConflict, "%s already has a branch %s, at %s, which is not in the workspace's history; nothing was pushed: rename or remove that branch, or choose another repository", to.Name, m.name, there)
	}
	return nil
}
