// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, backup-force-push-overwrites-branch]
// ---

package workflows

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// Job error kinds of the apply-branch operation, beside backup-failed and
// the container operations' kinds.
const (
	KindBranchNotFound = "branch-not-found"
	KindFetchFailed    = "fetch-failed"
	KindNotApplicable  = "not-applicable"
	KindApplyFailed    = "apply-failed"
)

// applyBranch validates an apply-branch request. It changes nothing.
func (w *Workflows) applyBranch(req api.JobRequest) (jobsRunner, error) {
	if err := workspace.ValidName(req.Workspace); err != nil {
		return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
	}
	if _, err := backup.Branch(req.Workspace); err != nil {
		return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
	}
	if req.Branch == "" {
		return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-request", Message: "branch is required"}
	}
	if _, err := backup.ApplyBranch(req.Branch); err != nil {
		return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-branch", Message: err.Error()}
	}
	folder, err := w.opts.Root.Folder(req.Workspace)
	if errors.Is(err, workspace.ErrNotFound) {
		return nil, &api.RequestError{Status: http.StatusNotFound, Kind: "workspace-not-found", Message: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	return (&applying{w: w, name: req.Workspace, folder: folder, branch: req.Branch}).run, nil
}

// applying is one apply-branch job.
//
// Its messages name the branch, the workspace and commits, never the
// workspace repository's address or a transport error, which may carry
// it.
type applying struct {
	w      *Workflows
	name   string
	folder string
	branch string
}

func (a *applying) run(ctx context.Context, run *jobs.Run) error {
	run.Phase("fetch")
	defer func() {
		if err := backup.RemoveFetched(a.folder, a.name); err != nil {
			fmt.Fprintf(run.Log(""), "could not delete %s; the next apply replaces it\n", backup.FetchedRef)
		}
	}()
	fetched, err := a.w.opts.Remote.Fetch(ctx, a.folder, a.name, a.branch, run.Log("fetch: "))
	if errors.Is(err, backup.ErrBranchNotFound) {
		return &jobs.Failure{Kind: KindBranchNotFound, Err: fmt.Errorf("the workspace repository has no branch %s; nothing was changed", a.branch)}
	}
	if err != nil {
		return &jobs.Failure{Kind: KindFetchFailed, Err: fmt.Errorf("fetching branch %s from the workspace repository failed; nothing was changed", a.branch)}
	}
	fmt.Fprintf(run.Log(""), "fetched branch %s at %s\n", a.branch, fetched)

	run.Phase("backup-local-changes")
	before, err := backup.CommitAll(a.folder, a.name, fmt.Sprintf("Back up local changes before applying %s", a.branch), run.Log("commit: "))
	if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("committing local changes failed: %w; nothing was applied", err)}
	}
	if before.Changed {
		fmt.Fprintf(run.Log(""), "committed local changes as %s\n", before.Hash)
	} else {
		fmt.Fprintf(run.Log(""), "no local changes; branch %s is at %s\n", a.name, before.Hash)
	}
	run.Phase("backup")
	if err := push(ctx, a.w.opts.Remote, a.folder, a.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("backing up the workspace before applying %s failed; nothing was applied", a.branch)}
	}

	run.Phase("apply")
	applied, err := backup.Apply(a.folder, a.name, a.branch, run.Log("apply: "))
	var refused *backup.NotApplicableError
	var failed *backup.ApplyError
	switch {
	case errors.As(err, &refused):
		for _, p := range refused.Paths {
			fmt.Fprintf(run.Log(""), "not applicable: %s\n", p)
		}
		return &jobs.Failure{Kind: KindNotApplicable, Err: fmt.Errorf("branch %s at %s cannot be applied: %s; nothing was applied; the workspace branch is at %s", a.branch, fetched, refusal(refused), before.Hash)}
	case errors.As(err, &failed):
		return &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("applying branch %s failed: %w; files written before the failure remain in %s; the state before the apply is backed up as %s; recovery is manual", a.branch, failed, a.folder, before.Hash)}
	case err != nil:
		return &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("applying branch %s failed: %w; nothing was applied", a.branch, err)}
	}
	if applied.Outcome == backup.AlreadyApplied {
		fmt.Fprintf(run.Log(""), "branch %s at %s is already applied; nothing changed and the devcontainer was not rebuilt\n", a.branch, fetched)
		return nil
	}
	fmt.Fprintf(run.Log(""), "applied branch %s (%s): the workspace branch moved from %s to %s\n", a.branch, applied.Outcome, applied.Before, applied.Hash)

	run.Phase("backup")
	if err := push(ctx, a.w.opts.Remote, a.folder, a.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("branch %s was applied as %s, but its backup failed; the devcontainer was not rebuilt", a.branch, applied.Hash)}
	}
	if err := a.w.opts.Runtime.Rebuild(ctx, a.folder, run); err != nil {
		return api.ContainerFailure(fmt.Errorf("branch %s was applied and backed up as %s, but the devcontainer did not rebuild: %w", a.branch, applied.Hash, err))
	}
	return nil
}

// refusal describes a refused apply for the job's error.
func refusal(e *backup.NotApplicableError) string {
	if len(e.Paths) == 0 {
		return e.Reason
	}
	return e.Reason + " [paths: " + strings.Join(e.Paths, ", ") + "]"
}
