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
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
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
	// record is the applied version the planned merge's applied record
	// gives the workspace, when the merge changes the record.
	record *store.AppliedVersion
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
	plan, failure := a.plan(ctx, run, fetched, before.Hash)
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
		if plan, failure = a.plan(ctx, run, fetched, before.Hash); failure != nil {
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
	if a.record != nil {
		if err := a.w.opts.Data.SetAppliedVersion(context.WithoutCancel(ctx), a.name, *a.record); err != nil {
			fmt.Fprintf(run.Log(""), "recording version %s as the workspace's applied version failed: %v\n", a.record.Version, err)
		} else {
			fmt.Fprintf(run.Log(""), "the workspace's applied version is now %s, at commit %s\n", a.record.Version, a.record.Commit)
		}
	}

	run.Phase("backup")
	if err := push(ctx, a.w.opts.Remote, a.folder, a.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("branch %s was applied as %s, but its backup failed; the devcontainer was not rebuilt", a.branch, applied.Hash)}
	}
	if err := a.w.opts.Runtime.Rebuild(ctx, a.folder, run); err != nil {
		if started(err) {
			a.stopped = false
			return api.ContainerFailure(fmt.Errorf("branch %s was applied and backed up as %s: %w", a.branch, applied.Hash, err))
		}
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

// plan works out the apply against the workspace branch at head, and
// refuses a merge that would move the workspace's applied record backward
// (see forward).
func (a *applying) plan(ctx context.Context, run *jobs.Run, fetched, head plumbing.Hash) (*backup.Plan, error) {
	run.Phase("plan")
	plan, err := backup.PlanApply(a.folder, a.name, a.branch, run.Log("apply: "))
	var refused *backup.NotApplicableError
	switch {
	case errors.As(err, &refused):
		return nil, a.refusal(run, refused, fetched, head)
	case err != nil:
		return nil, &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("applying branch %s failed: %w; nothing was applied", a.branch, err)}
	}
	if a.record, err = a.forward(ctx, plan); err != nil {
		var refused *backup.NotApplicableError
		if errors.As(err, &refused) {
			return nil, a.refusal(run, refused, fetched, head)
		}
		return nil, err
	}
	return plan, nil
}

// forward reads, before anything is written, the applied record a planned
// merge would leave, when it changes the record: the workspace must have a
// registered template, and the record must be one Toha can read, name that
// template's source, and give a version, its ref's or the one whose tag is
// at its commit, no older than the workspace's applied version; while that
// version is unknown, the record's commit must descend from the applied
// commit, as an upgrade checks. Anything else refuses the apply, naming
// both versions or commits. It returns the applied version the record
// gives, or nil when the merge leaves the record as it is.
func (a *applying) forward(ctx context.Context, plan *backup.Plan) (*store.AppliedVersion, error) {
	change, err := plan.Change(a.folder, registry.RecordName, registry.MaxRecord)
	if err != nil {
		return nil, &jobs.Failure{Kind: KindApplyFailed, Err: fmt.Errorf("reading the applied record branch %s would write failed: %w; nothing was applied", a.branch, err)}
	}
	if !change.Changed {
		return nil, nil
	}
	refuse := func(format string, args ...any) error {
		return &backup.NotApplicableError{Reason: fmt.Sprintf(format, args...), Paths: []string{registry.RecordName}}
	}
	id, err := a.w.opts.Data.WorkspaceTemplate(ctx, a.name)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, refuse("the branch changes the applied record of workspace %s, which has no registered template to check it against", a.name)
	}
	tpl, err := a.w.opts.Data.Template(ctx, id)
	if err != nil {
		return nil, err
	}
	applied, err := a.w.opts.Data.AppliedVersion(ctx, a.name)
	if err != nil {
		return nil, err
	}
	was := "version " + applied.Version
	if applied.Version == "" {
		was = "an unknown version at commit " + orNone(applied.Commit)
	}
	if !change.After.Present {
		return nil, refuse("the branch removes the applied record, which names %s", was)
	}
	if !change.After.Regular {
		return nil, refuse("the branch leaves an applied record that is not a file, where the workspace has %s", was)
	}
	record, err := registry.ParseApplied(change.After.Data)
	if err != nil {
		return nil, refuse("the branch leaves an applied record Toha cannot read (%v), where the workspace has %s", err, was)
	}
	if record.Source != tpl.Source {
		return nil, refuse("the branch's applied record names another template than template %s, whose source the workspace has", tpl.ID)
	}
	version, tag := "", ""
	if v, ok := registry.ParseVersion(record.Ref); ok {
		version, tag = v.String(), record.Ref
	} else {
		for _, found := range tpl.Versions {
			if record.Commit != "" && found.Commit == record.Commit {
				version, tag = found.Version, found.Tag
			}
		}
	}
	if version == "" {
		return nil, refuse("the branch's applied record names ref %q at commit %s, which no version tag of template %s gives, where the workspace has %s", record.Ref, orNone(record.Commit), tpl.ID, was)
	}
	out := &store.AppliedVersion{Version: version, Commit: record.Commit}
	if applied.Version != "" {
		if registry.Newer(applied.Version, version) {
			return nil, refuse("the branch's applied record names version %s, older than the workspace's version %s", version, applied.Version)
		}
		return out, nil
	}
	// An unknown applied version moves only to a record whose commit
	// descends from the applied commit.
	if applied.Commit == "" || record.Commit == "" {
		return nil, refuse("the branch's applied record names version %s at commit %s, and the workspace's applied version and commit are not known, so it cannot be told to move forward", version, orNone(record.Commit))
	}
	if found, ok := tpl.Found(version); ok {
		tag = found.Tag
	}
	checked, err := a.w.opts.Templates.CheckSince(ctx, tpl.Address, tag, applied.Commit)
	var unavailable *templates.UnavailableError
	switch {
	case errors.As(err, &unavailable):
		return nil, &jobs.Failure{Kind: KindTemplateUnavailable, Err: fmt.Errorf("the template service could not say whether version %s descends from the applied commit %s: %w; nothing was applied", version, applied.Commit, err)}
	case err != nil:
		return nil, refuse("the template service refused to check version %s against the applied commit %s (%s)", version, applied.Commit, refusalKind(err))
	case checked.Commit != record.Commit:
		return nil, refuse("the branch's applied record names version %s at commit %s, but its tag is at commit %s", version, record.Commit, checked.Commit)
	case !*checked.Descends:
		return nil, refuse("the branch's applied record names version %s at commit %s, which does not descend from the workspace's applied commit %s", version, record.Commit, applied.Commit)
	}
	return out, nil
}

// orNone is a commit, or "none".
func orNone(commit string) string {
	if commit == "" {
		return "none"
	}
	return commit
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
	if err := a.w.opts.Runtime.Start(start, a.folder, run); started(err) {
		fmt.Fprintln(run.Log(""), err)
	} else if err != nil {
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
