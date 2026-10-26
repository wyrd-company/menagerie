// ---
// relationships:
//   implements: menagerie
//   references: [template-service-api, workspace-api, workspace-root]
// ---

package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// creation is one create job.
type creation struct {
	w        *Workflows
	name     string
	folder   string
	template string
}

func (c *creation) run(ctx context.Context, run *jobs.Run) error {
	t := c.w.opts.Templates
	sel := c.w.selector(c.name, c.folder, c.template)
	reply, err := c.w.interview(ctx, run, interviewSteps{
		start: func(ctx context.Context) (*templates.Reply, error) { return t.StartInterview(ctx, sel) },
		answer: func(ctx context.Context, session, submission json.RawMessage) (*templates.Reply, error) {
			return t.AnswerInterview(ctx, sel, session, submission)
		},
	})
	if err != nil {
		return err
	}

	run.Phase("initialize-repository")
	if err := os.Mkdir(c.folder, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return &jobs.Failure{Kind: KindWorkspaceExists, Err: fmt.Errorf("workspace %s was created while the interview ran; nothing was written", c.name)}
		}
		return fmt.Errorf("create workspace folder: %w", err)
	}
	if err := backup.Init(c.folder, c.name); err != nil {
		return fmt.Errorf("%w; %s", err, c.recovery("holds what the repository initialization left"))
	}

	run.Phase("generate")
	grant, err := c.w.grantHooks(ctx, run, c.folder, c.template)
	if err != nil {
		return fmt.Errorf("%w; %s", err, c.recovery("holds only the initialized repository"))
	}
	applied, err := t.Generate(ctx, sel, reply.Session, grant.token())
	grant.revoke()
	if err != nil {
		return &jobs.Failure{Kind: grant.kind(KindGenerationFailed), Err: fmt.Errorf("%s; %s", generationError(err), c.recovery("holds the files written before the failure and no applied record"))}
	}
	if err := run.SetApply(applied.Raw); err != nil {
		return err
	}
	reportApplied(run.Log(""), applied)
	if applied.Outcome != templates.Written {
		return &jobs.Failure{Kind: KindNotApplied, Err: fmt.Errorf("generation reported %s and wrote nothing; %s", applied.Outcome, c.recovery("holds only the initialized repository"))}
	}

	run.Phase("commit")
	commit, err := backup.CommitRoot(c.folder, c.name, output(applied), fmt.Sprintf("Create workspace %s from %s", c.name, c.template), run.Log("commit: "))
	if err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("the workspace was generated with its applied record, but committing it failed: %w; the workspace was not started; %s", err, c.recovery("holds the generated files"))}
	}
	fmt.Fprintf(run.Log(""), "committed %s as the root of branch %s\n", commit.Hash, c.name)

	run.Phase("backup")
	if err := push(ctx, c.w.opts.Remote, c.folder, c.name, run); err != nil {
		return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("the workspace was generated and committed as %s, but its backup failed: %w; the workspace was not started; start it with the start operation, and the next upgrade pushes its branch again", commit.Hash, err)}
	}

	if err := c.w.opts.Runtime.Start(ctx, c.folder, run); err != nil {
		return api.ContainerFailure(fmt.Errorf("the workspace was generated and backed up as %s, but its devcontainer did not start: %w", commit.Hash, err))
	}
	return nil
}

// recovery is the manual recovery guidance of a failed create whose folder
// exists. A second create of the same workspace is refused while the
// folder exists, so it does not name a retry.
func (c *creation) recovery(state string) string {
	return fmt.Sprintf("the workspace folder %s %s; recovery is manual: inspect the folder, then remove it before creating workspace %s again", c.folder, state, c.name)
}

// generationError describes a failed generation, naming the paths the
// template service refused.
func generationError(err error) string {
	var refusal *templates.Error
	if errors.As(err, &refusal) {
		msg := fmt.Sprintf("generation failed (%s): %s", refusal.Kind, refusal.Message)
		if len(refusal.Paths) > 0 {
			msg += " [paths: " + strings.Join(refusal.Paths, ", ") + "]"
		}
		return msg
	}
	return "generation failed: " + err.Error()
}

// output names an apply's written files and applied record for staging.
func output(applied *templates.Applied) backup.Output {
	out := backup.Output{Record: *applied.RecordPath}
	for _, f := range applied.Files {
		out.Reported = append(out.Reported, f.Path)
	}
	return out
}

// reportApplied logs an apply's files, hooks and messages.
func reportApplied(log io.Writer, applied *templates.Applied) {
	fmt.Fprintf(log, "template outcome: %s\n", applied.Outcome)
	for _, f := range applied.Files {
		fmt.Fprintf(log, "%s %s\n", f.Action, f.Path)
	}
	for _, h := range applied.Hooks {
		id, code := "(unnamed)", "none"
		if h.ID != nil {
			id = *h.ID
		}
		if h.ExitCode != nil {
			code = fmt.Sprint(*h.ExitCode)
		}
		fmt.Fprintf(log, "hook %s exit %s success %t\n", id, code, h.Success)
	}
	for _, m := range applied.Messages {
		fmt.Fprintf(log, "template: %s\n", m)
	}
}

// push backs up the workspace branch and logs whether it changed.
func push(ctx context.Context, remote *backup.Remote, folder, name string, run *jobs.Run) error {
	pushed, err := remote.Push(ctx, folder, name, run.Log("backup: "))
	if err != nil {
		return err
	}
	if pushed {
		fmt.Fprintf(run.Log(""), "pushed branch %s to the workspace repository\n", name)
	} else {
		fmt.Fprintf(run.Log(""), "branch %s in the workspace repository is already up to date\n", name)
	}
	return nil
}
