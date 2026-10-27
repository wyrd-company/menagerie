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
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
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
	// running is whether the devcontainer ran before the apply stopped
	// it; stopped is whether the apply has left it stopped.
	running, stopped bool
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
	var shallow *backup.NotApplicableError
	if errors.As(err, &shallow) {
		return &jobs.Failure{Kind: KindNotApplicable, Err: fmt.Errorf("branch %s cannot be applied: %s; nothing was changed", a.branch, shallow.Reason)}
	}
	if err != nil {
		return &jobs.Failure{Kind: KindFetchFailed, Err: fmt.Errorf("fetching branch %s from the workspace repository failed; nothing was changed", a.branch)}
	}
	fmt.Fprintf(run.Log(""), "fetched branch %s at %s\n", a.branch, fetched)

	before, failure := a.backUp(ctx, run, false)
	if failure != nil {
		return failure
	}
	plan, failure := a.plan(run, fetched, before.Hash)
	if failure != nil {
		return failure
	}
	if plan.Outcome == backup.AlreadyApplied {
		fmt.Fprintf(run.Log(""), "branch %s at %s is already applied; nothing changed and the devcontainer was not rebuilt\n", a.branch, fetched)
		// The workspace's own branch then contains the remote's commit, so
		// the push only moves the remote forward.
		if a.branch == a.name {
			run.Phase("backup")
			if err := push(ctx, a.w.opts.Remote, a.folder, a.name, run); err != nil {
				return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("branch %s is already applied, but the backup of the workspace failed", a.branch)}
			}
		}
		return nil
	}

	// Nothing in the devcontainer writes to the folder while the apply
	// does. What it wrote before it stopped is committed, and the merge is
	// planned again on top of it. A refusal returns the devcontainer to
	// its state before the stop.
	status, err := a.w.opts.Runtime.Status(ctx, a.folder)
	if err != nil {
		return api.ContainerFailure(fmt.Errorf("reading the devcontainer's state failed: %w; nothing was applied", err))
	}
	a.running = status.State == devcontainer.StateRunning
	run.Phase("stop")
	a.stopped = true
	// Written as a console line, so it stays when a time limit replaces
	// the job's error.
	defer func() {
		if a.stopped {
			fmt.Fprintln(run.Log(""), StoppedNotice)
		}
	}()
	if err := a.w.opts.Runtime.Stop(ctx, a.folder, run); err != nil {
		return api.ContainerFailure(fmt.Errorf("the devcontainer did not stop: %w; nothing was applied", err))
	}
	again, failure := a.backUp(ctx, run, true)
	if failure != nil {
		return a.resume(ctx, run, failure)
	}
	if again.Changed {
		before = again
		if plan, failure = a.plan(run, fetched, before.Hash); failure != nil {
			return a.resume(ctx, run, failure)
		}
	}

	kept := "backed up"
	if a.branch == a.name {
		kept = "committed but not backed up"
	}
	run.Phase("apply")
	if failure := a.unmounted(ctx, run); failure != nil {
		return a.resume(ctx, run, failure)
	}
	applied, err := plan.Write(a.folder, run.Log("apply: "))
	var refused *backup.NotApplicableError
	var failed *backup.ApplyError
	switch {
	case errors.As(err, &refused):
		return a.resume(ctx, run, a.refusal(run, refused, fetched, before.Hash))
	case errors.As(err, &failed):
		return &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("applying branch %s failed: %w; files written before the failure remain in %s; the state before the apply is %s as %s; the devcontainer is stopped; recovery is manual", a.branch, failed, a.folder, kept, before.Hash)}
	case err != nil:
		return a.resume(ctx, run, &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("applying branch %s failed: %w; nothing was applied", a.branch, err)})
	}
	fmt.Fprintf(run.Log(""), "applied branch %s (%s): the workspace branch moved from %s to %s\n", a.branch, applied.Outcome, applied.Before, applied.Hash)

	run.Phase("backup")
	if err := push(ctx, a.w.opts.Remote, a.folder, a.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("branch %s was applied as %s, but its backup failed; the devcontainer was not rebuilt", a.branch, applied.Hash)}
	}
	if err := a.w.opts.Runtime.Rebuild(ctx, a.folder, run); err != nil {
		return api.ContainerFailure(fmt.Errorf("branch %s was applied and backed up as %s, but the devcontainer did not rebuild: %w", a.branch, applied.Hash, err))
	}
	a.stopped = false
	return nil
}

// StoppedNotice is the console line of every apply-branch exit that
// leaves the devcontainer stopped.
const StoppedNotice = "the devcontainer is stopped; start or rebuild it"

// MountedReason refuses an apply while another container can write to
// the workspace folder.
const MountedReason = "a running container mounts the workspace folder"

// unmounted refuses the apply while a running container other than
// Menagerie's own has a bind mount of the workspace folder, of a path
// inside it, or of a folder that contains it. The console names each by
// its short id.
func (a *applying) unmounted(ctx context.Context, run *jobs.Run) error {
	ids, err := a.w.opts.Runtime.Mounting(ctx, a.folder)
	if err != nil {
		return &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("listing the running containers failed: %w; nothing was applied", err)}
	}
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		fmt.Fprintf(run.Log(""), "running container %s mounts the workspace folder\n", id)
	}
	return &jobs.Failure{Kind: KindNotApplicable, Err: fmt.Errorf("branch %s cannot be applied: %s; nothing was applied", a.branch, MountedReason)}
}

// backUp commits every local change and, unless the branch to apply is
// the workspace's own, backs the workspace branch up: always, or with
// onlyChanged only when the commit changed something. The workspace's own
// branch is not pushed before the apply: the push would replace the
// commits about to be applied on the remote.
func (a *applying) backUp(ctx context.Context, run *jobs.Run, onlyChanged bool) (backup.Commit, error) {
	run.Phase("backup-local-changes")
	c, err := backup.CommitAll(a.folder, a.name, fmt.Sprintf("Back up local changes before applying %s", a.branch), run.Log("commit: "))
	if err != nil {
		return c, &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("committing local changes failed: %w; nothing was applied", err)}
	}
	if c.Changed {
		fmt.Fprintf(run.Log(""), "committed local changes as %s\n", c.Hash)
	} else {
		fmt.Fprintf(run.Log(""), "no local changes; branch %s is at %s\n", a.name, c.Hash)
	}
	switch {
	case a.branch == a.name:
		fmt.Fprintf(run.Log(""), "the workspace's own branch %s is not pushed before the apply, so that its commits stay on the workspace repository\n", a.name)
	case onlyChanged && !c.Changed:
	default:
		run.Phase("backup")
		if err := push(ctx, a.w.opts.Remote, a.folder, a.name, run); err != nil {
			return c, &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("backing up the workspace before applying %s failed; nothing was applied", a.branch)}
		}
	}
	return c, nil
}

// plan works out the apply against the workspace branch at head.
func (a *applying) plan(run *jobs.Run, fetched, head plumbing.Hash) (*backup.Plan, error) {
	run.Phase("plan")
	plan, err := backup.PlanApply(a.folder, a.name, a.branch, run.Log("apply: "))
	var refused *backup.NotApplicableError
	switch {
	case errors.As(err, &refused):
		return nil, a.refusal(run, refused, fetched, head)
	case err != nil:
		return nil, &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("applying branch %s failed: %w; nothing was applied", a.branch, err)}
	}
	return plan, nil
}

// refusal lists a refused apply's paths in the console and is its job
// failure.
func (a *applying) refusal(run *jobs.Run, refused *backup.NotApplicableError, fetched, head plumbing.Hash) error {
	for _, p := range refused.Paths {
		fmt.Fprintf(run.Log(""), "not applicable: %s\n", p)
	}
	return &jobs.Failure{Kind: KindNotApplicable, Err: fmt.Errorf("branch %s at %s cannot be applied: %s; nothing was applied; the workspace branch is at %s", a.branch, fetched, refusal(refused), head)}
}

// resume starts the devcontainer again after a failure that left the
// folder as it was when the devcontainer stopped, and returns failure.
// When the start fails, the start's failure is the job's error and
// failure's message stays in the console.
func (a *applying) resume(ctx context.Context, run *jobs.Run, failure error) error {
	if !a.running {
		fmt.Fprintln(run.Log(""), "nothing was applied; the devcontainer was not running before the apply, so it stays stopped")
		return failure
	}
	fmt.Fprintln(run.Log(""), "nothing was applied; starting the devcontainer again")
	// The start runs even after the job was cancelled, within its own
	// bound.
	limit := a.w.opts.RestartLimit
	if limit <= 0 {
		limit = DefaultRestartLimit
	}
	start, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
	defer cancel()
	if err := a.w.opts.Runtime.Start(start, a.folder, run); err != nil {
		fmt.Fprintf(run.Log(""), "starting the devcontainer again failed: %v\n", err)
		fmt.Fprintln(run.Log(""), failure)
		return api.ContainerFailure(fmt.Errorf("nothing was applied, and the devcontainer did not start again: %w", err))
	}
	a.stopped = false
	return failure
}

// DefaultRestartLimit bounds the start after a refusal when the start
// operation has no time limit.
const DefaultRestartLimit = 10 * time.Minute

// refusal describes a refused apply for the job's error.
func refusal(e *backup.NotApplicableError) string {
	if len(e.Paths) == 0 {
		return e.Reason
	}
	return e.Reason + " [paths: " + strings.Join(e.Paths, ", ") + "]"
}
