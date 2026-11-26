// ---
// relationships:
//   implements: menagerie
//   references: [template-service-api, workspace-api]
// ---

package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/gitcreds"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
)

// upgrading is one upgrade job: it applies the current version of the
// workspace's registered template, which it pins when it starts.
type upgrading struct {
	w        *Workflows
	name     string
	folder   string
	template string
	// reanswer asks every question again, with the recorded answers as
	// the defaults.
	reanswer bool
}

func (u *upgrading) run(ctx context.Context, run *jobs.Run) error {
	tpl, err := u.w.opts.Data.Template(ctx, u.template)
	if err != nil {
		return &jobs.Failure{Kind: KindTemplateRefused, Err: fmt.Errorf("the workspace's template %s could not be read: %w; nothing was upgraded", u.template, err)}
	}
	// A tag that moved after the request was checked is the template
	// service's template-stale-revision: the job names the recorded commit.
	from, approved, failure := u.forward(ctx, run, tpl)
	if failure != nil {
		return failure
	}
	if tpl.CurrentCommit == "" && approved != "" {
		// No commit is recorded for the current version yet: the job pins
		// the one the descent check approved, so the template service
		// refuses a tag moved since.
		tpl.CurrentCommit = approved
	}
	fmt.Fprintf(run.Log(""), "upgrading from %s to template %s, version %s\n", from, tpl.ID, tpl.CurrentVersion)
	target := tpl.ID + " " + tpl.CurrentVersion
	run.Phase("backup-local-changes")
	before, err := backup.CommitTracked(u.folder, u.name, fmt.Sprintf("Back up local changes before upgrading to %s", target))
	if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("committing local changes failed: %w; nothing was upgraded", err)}
	}
	if before.Changed {
		fmt.Fprintf(run.Log(""), "committed local changes to tracked files as %s\n", before.Hash)
	} else {
		fmt.Fprintf(run.Log(""), "no local changes to tracked files; branch %s is at %s\n", u.name, before.Hash)
	}
	run.Phase("backup")
	if err := push(ctx, u.w.opts.Remotes, u.folder, u.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("backing up the workspace before the upgrade failed: %w; nothing was upgraded", err)}
	}

	t := u.w.opts.Templates
	sel := u.w.selector(u.name, u.folder, tpl)
	if u.reanswer {
		fmt.Fprintln(run.Log(""), "asking every question again, with the recorded answers as the defaults")
	}
	reply, err := u.w.interview(ctx, run, interviewSteps{
		start: func(ctx context.Context) (*templates.Reply, error) { return t.StartUpgrade(ctx, sel, u.reanswer) },
		answer: func(ctx context.Context, session, submission json.RawMessage) (*templates.Reply, error) {
			return t.AnswerUpgrade(ctx, sel, session, submission)
		},
	})
	if err != nil {
		return err
	}

	run.Phase("apply")
	grant, err := u.w.grantHooks(ctx, run, u.folder, u.template, throwaway.TargetCandidate)
	if err != nil {
		return fmt.Errorf("%w; nothing was upgraded", err)
	}
	result, err := t.ApplyUpgrade(ctx, sel, reply.Session, grant.token())
	grant.revoke()
	if err != nil {
		return &jobs.Failure{Kind: grant.kind(KindUpgradeFailed), Err: fmt.Errorf("%s; files written before the failure remain in %s; the state before the upgrade is backed up as %s; recovery is manual", applyError(err), u.folder, before.Hash)}
	}
	if err := run.SetApply(result.Raw); err != nil {
		return err
	}
	reportApplied(run.Log(""), result)
	switch result.Outcome {
	case templates.Written, templates.Conflicted, templates.AlreadyCurrent:
		// The applied record now names the pinned version, as the data does.
		u.w.recordApplied(ctx, run, u.name, u.folder, tpl)
	}
	switch result.Outcome {
	case templates.AlreadyCurrent:
		fmt.Fprintln(run.Log(""), "the workspace is already current; nothing was written and the devcontainer was not rebuilt")
		return nil
	case templates.NeedsTrust, templates.Planned:
		return &jobs.Failure{Kind: KindNotApplied, Err: fmt.Errorf("the upgrade reported %s and wrote nothing; the workspace is unchanged", result.Outcome)}
	}

	run.Phase("commit")
	commit, err := backup.CommitUpgrade(u.folder, u.name, output(result), fmt.Sprintf("Upgrade workspace %s to %s", u.name, target), run.Log("commit: "))
	if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("the upgrade was written, but committing it failed: %w; the devcontainer was not rebuilt", err)}
	}
	if commit.Changed {
		fmt.Fprintf(run.Log(""), "committed the upgrade as %s\n", commit.Hash)
	} else {
		fmt.Fprintln(run.Log(""), "the upgrade changed no tracked file")
	}

	run.Phase("backup")
	if err := push(ctx, u.w.opts.Remotes, u.folder, u.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("the upgrade was written and committed as %s, but its backup failed: %w; the devcontainer was not rebuilt", commit.Hash, err)}
	}

	if result.Outcome == templates.Conflicted {
		return &jobs.Failure{Kind: KindMergeConflicted, Err: fmt.Errorf("the upgrade is not complete: it wrote merge conflict markers into %s, which need manual resolution; the conflicted files are committed and backed up as %s, and the state before the upgrade as %s; the devcontainer was not rebuilt: resolve the conflicts, then rebuild", strings.Join(result.Conflicts, ", "), commit.Hash, before.Hash)}
	}
	if err := u.w.opts.Runtime.Rebuild(ctx, u.folder, run); err != nil {
		if started(err) {
			return api.ContainerFailure(fmt.Errorf("the workspace was upgraded and backed up as %s: %w", commit.Hash, err))
		}
		return api.ContainerFailure(fmt.Errorf("the workspace was upgraded and backed up as %s, but its devcontainer did not rebuild: %w", commit.Hash, err))
	}
	return nil
}

// forward checks that the upgrade moves the workspace forward, before
// anything is backed up or asked: a known applied version must be older
// than the template's current version; an unknown one, whose applied
// commit has no version tag, goes on only when the template service
// answers that the current version's commit descends from that commit. It
// says what the workspace is upgraded from, and the commit a descent check
// approved, if one ran.
func (u *upgrading) forward(ctx context.Context, run *jobs.Run, tpl store.Template) (string, string, error) {
	applied, err := u.w.opts.Data.AppliedVersion(ctx, u.name)
	if err != nil {
		return "", "", err
	}
	switch {
	case applied.Version != "":
		if !registry.Newer(tpl.CurrentVersion, applied.Version) {
			return "", "", &jobs.Failure{Kind: KindTemplateRefused, Err: fmt.Errorf("the workspace already has version %s, and template %s's current version is %s; nothing was upgraded", applied.Version, tpl.ID, tpl.CurrentVersion)}
		}
		return "version " + applied.Version, "", nil
	case applied.Commit == "":
		return "", "", &jobs.Failure{Kind: KindTemplateRefused, Err: fmt.Errorf("neither the version nor the commit last applied to the workspace is known, so the upgrade cannot tell that version %s moves it forward; nothing was upgraded", tpl.CurrentVersion)}
	}
	fmt.Fprintf(run.Log(""), "the workspace's applied version is unknown; asking whether version %s's commit descends from its applied commit %q\n", tpl.CurrentVersion, applied.Commit)
	checked, err := u.w.opts.Templates.CheckSince(ctx, tpl.Address, tpl.CurrentTag, applied.Commit, templates.CredentialOf(tpl.Credential.App, tpl.Credential.Key))
	var unavailable *templates.UnavailableError
	switch {
	case errors.As(err, &unavailable):
		return "", "", &jobs.Failure{Kind: KindTemplateUnavailable, Err: fmt.Errorf("the template service could not say whether version %s descends from the applied commit %q: %w; nothing was upgraded", tpl.CurrentVersion, applied.Commit, err)}
	case err != nil:
		return "", "", &jobs.Failure{Kind: KindTemplateRefused, Err: fmt.Errorf("the template service refused to check version %s against the applied commit %q%s (%s); nothing was upgraded", tpl.CurrentVersion, applied.Commit, gitcreds.Phrase(tpl.Credential), refusalKind(err))}
	case tpl.CurrentCommit != "" && checked.Commit != tpl.CurrentCommit:
		return "", "", &jobs.Failure{Kind: KindTemplateRefused, Err: fmt.Errorf("version %s's tag is at commit %q, not the recorded %q; nothing was upgraded", tpl.CurrentVersion, checked.Commit, tpl.CurrentCommit)}
	case !*checked.Descends:
		return "", "", &jobs.Failure{Kind: KindTemplateRefused, Err: fmt.Errorf("version %s's commit %q does not descend from the workspace's applied commit %q, so the upgrade would not move it forward; nothing was upgraded", tpl.CurrentVersion, checked.Commit, applied.Commit)}
	}
	return "commit " + applied.Commit, checked.Commit, nil
}

// refusalKind names a template service refusal by its kind alone.
func refusalKind(err error) string {
	var refused *templates.Error
	if errors.As(err, &refused) {
		return refused.Kind
	}
	return "no reply"
}

// recordApplied records tpl's current version, which the job pinned, as
// the version last applied to the workspace, with its commit, or the one
// the applied record names when the template's is not known yet.
func (w *Workflows) recordApplied(ctx context.Context, run *jobs.Run, name, folder string, tpl store.Template) {
	commit := tpl.CurrentCommit
	if commit == "" {
		if applied, err := registry.ReadApplied(folder); err == nil && registry.IsCommit(applied.Commit) {
			commit = applied.Commit
		}
	}
	if err := w.opts.Data.SetAppliedVersion(context.WithoutCancel(ctx), name, store.AppliedVersion{Version: tpl.CurrentVersion, Commit: commit}); err != nil {
		fmt.Fprintf(run.Log(""), "recording version %s as the workspace's applied version failed: %v\n", tpl.CurrentVersion, err)
	}
}

// applyError describes a failed upgrade apply.
func applyError(err error) string {
	var refusal *templates.Error
	if errors.As(err, &refusal) {
		msg := fmt.Sprintf("the upgrade failed (%s): %s", refusal.Kind, refusal.Message)
		if len(refusal.Paths) > 0 {
			msg += " [paths: " + strings.Join(refusal.Paths, ", ") + "]"
		}
		return msg
	}
	return "the upgrade failed: " + err.Error()
}
