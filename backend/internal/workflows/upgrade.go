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
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// upgrading is one upgrade job.
type upgrading struct {
	w        *Workflows
	name     string
	folder   string
	template string
}

func (u *upgrading) run(ctx context.Context, run *jobs.Run) error {
	run.Phase("backup-local-changes")
	before, err := backup.CommitTracked(u.folder, u.name, fmt.Sprintf("Back up local changes before upgrading to %s", u.template))
	if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("committing local changes failed: %w; nothing was upgraded", err)}
	}
	if before.Changed {
		fmt.Fprintf(run.Log(""), "committed local changes to tracked files as %s\n", before.Hash)
	} else {
		fmt.Fprintf(run.Log(""), "no local changes to tracked files; branch %s is at %s\n", u.name, before.Hash)
	}
	run.Phase("backup")
	if err := push(ctx, u.w.opts.Remote, u.folder, u.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("backing up the workspace before the upgrade failed: %w; nothing was upgraded", err)}
	}

	t := u.w.opts.Templates
	sel := u.w.selector(u.name, u.folder, u.template)
	reply, err := u.w.interview(ctx, run, interviewSteps{
		start: func(ctx context.Context) (*templates.Reply, error) { return t.StartUpgrade(ctx, sel) },
		answer: func(ctx context.Context, session, submission json.RawMessage) (*templates.Reply, error) {
			return t.AnswerUpgrade(ctx, sel, session, submission)
		},
	})
	if err != nil {
		return err
	}

	run.Phase("apply")
	grant, err := u.w.grantHooks(ctx, run, u.folder, u.template)
	if err != nil {
		return fmt.Errorf("%w; nothing was upgraded", err)
	}
	applied, err := t.ApplyUpgrade(ctx, sel, reply.Session, grant.token())
	grant.revoke()
	if err != nil {
		return &jobs.Failure{Kind: grant.kind(KindUpgradeFailed), Err: fmt.Errorf("%s; files written before the failure remain in %s; the state before the upgrade is backed up as %s; recovery is manual", applyError(err), u.folder, before.Hash)}
	}
	if err := run.SetApply(applied.Raw); err != nil {
		return err
	}
	reportApplied(run.Log(""), applied)
	switch applied.Outcome {
	case templates.AlreadyCurrent:
		fmt.Fprintln(run.Log(""), "the workspace is already current; nothing was written and the devcontainer was not rebuilt")
		return nil
	case templates.NeedsTrust, templates.Planned:
		return &jobs.Failure{Kind: KindNotApplied, Err: fmt.Errorf("the upgrade reported %s and wrote nothing; the workspace is unchanged", applied.Outcome)}
	}

	run.Phase("commit")
	commit, err := backup.CommitUpgrade(u.folder, u.name, output(applied), fmt.Sprintf("Upgrade workspace %s to %s", u.name, u.template), run.Log("commit: "))
	if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("the upgrade was written, but committing it failed: %w; the devcontainer was not rebuilt", err)}
	}
	if commit.Changed {
		fmt.Fprintf(run.Log(""), "committed the upgrade as %s\n", commit.Hash)
	} else {
		fmt.Fprintln(run.Log(""), "the upgrade changed no tracked file")
	}

	run.Phase("backup")
	if err := push(ctx, u.w.opts.Remote, u.folder, u.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("the upgrade was written and committed as %s, but its backup failed: %w; the devcontainer was not rebuilt", commit.Hash, err)}
	}

	if applied.Outcome == templates.Conflicted {
		return &jobs.Failure{Kind: KindMergeConflicted, Err: fmt.Errorf("the upgrade is not complete: it wrote merge conflict markers into %s, which need manual resolution; the conflicted files are committed and backed up as %s, and the state before the upgrade as %s; the devcontainer was not rebuilt: resolve the conflicts, then rebuild", strings.Join(applied.Conflicts, ", "), commit.Hash, before.Hash)}
	}
	if err := u.w.opts.Runtime.Rebuild(ctx, u.folder, run); err != nil {
		return api.ContainerFailure(fmt.Errorf("the workspace was upgraded and backed up as %s, but its devcontainer did not rebuild: %w", commit.Hash, err))
	}
	return nil
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
