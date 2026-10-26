// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, workspace, workspace-fills-its-backup-branch]
// ---

// Package commits runs the commit operation: a job that commits every
// change of a workspace on its branch, as git add --all and a commit would,
// and backs the branch up. Scheduled commits start it on each idle
// workspace that has changes; a workspace's socket starts it for its own
// workspace.
package commits

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// Job error kinds of the commit operation.
const (
	KindCommitFailed = "commit-failed"
	KindBackupFailed = "backup-failed"
)

// Commit messages of the commits Menagerie starts itself.
const (
	// AutomaticMessage is the message of a scheduled commit.
	AutomaticMessage = "chore(menagerie): automatic commit"
	// APIMessage is the message of a commit started through the workspace
	// API.
	APIMessage = "chore(menagerie): commit workspace changes"
)

// Remote backs up a workspace branch. *backup.Remote satisfies it.
type Remote interface {
	Push(ctx context.Context, folder, workspace string, progress io.Writer) (bool, error)
}

// Options compose the commit operation.
type Options struct {
	Root   *workspace.Root
	Remote Remote
	Jobs   *jobs.Manager
}

// Commits starts commit jobs.
type Commits struct{ opts Options }

// New returns the commit operation.
func New(opts Options) *Commits { return &Commits{opts: opts} }

// Outcome is what a commit job did. It is complete once the job has ended.
type Outcome struct {
	mu      sync.Mutex
	commit  string
	changed bool
	pushed  bool
}

// Result returns the workspace branch's commit after the commit phase, or
// "" when that phase failed; whether the phase made a commit; and whether
// the backup changed the workspace repository's branch.
func (o *Outcome) Result() (commit string, changed, pushed bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.commit, o.changed, o.pushed
}

// Operation is the commit operation of POST /api/jobs. Its commit has the
// message APIMessage.
func (c *Commits) Operation() api.Operation {
	return func(req api.JobRequest) (jobs.Runner, error) {
		folder, err := c.opts.Root.Folder(req.Workspace)
		switch {
		case errors.Is(err, workspace.ErrInvalidName):
			return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
		case errors.Is(err, workspace.ErrNotFound):
			return nil, &api.RequestError{Status: http.StatusNotFound, Kind: "workspace-not-found", Message: err.Error()}
		case err != nil:
			return nil, err
		}
		return c.runner(folder, req.Workspace, APIMessage, &Outcome{}), nil
	}
}

// Start starts a commit job on the workspace with message. It returns a
// *jobs.BusyError when the workspace has an unfinished job, and an error
// matching workspace.ErrNotFound when it has no folder.
func (c *Commits) Start(name, message string) (*jobs.Job, *Outcome, error) {
	folder, err := c.opts.Root.Folder(name)
	if err != nil {
		return nil, nil, err
	}
	outcome := &Outcome{}
	job, err := c.opts.Jobs.Start(name, api.OperationCommit, c.runner(folder, name, message, outcome))
	if err != nil {
		return nil, nil, err
	}
	return job, outcome, nil
}

// runner commits the workspace's changes, then force-pushes its branch.
// The push runs even when nothing changed, so a backup that failed before
// is retried.
func (c *Commits) runner(folder, name, message string, outcome *Outcome) jobs.Runner {
	return func(ctx context.Context, run *jobs.Run) error {
		// A job started as Menagerie shuts down, or cancelled before it
		// ran, writes nothing.
		if ctx.Err() != nil {
			return &jobs.Failure{Kind: KindCommitFailed, Err: errors.New("the job was cancelled before its commit phase; nothing was committed or backed up")}
		}
		run.Phase("commit")
		commit, err := backup.CommitAll(folder, name, message, run.Log("commit: "))
		if err != nil {
			return &jobs.Failure{Kind: KindCommitFailed, Err: fmt.Errorf("committing the workspace's changes failed: %w; nothing was backed up", err)}
		}
		outcome.mu.Lock()
		outcome.commit, outcome.changed = commit.Hash.String(), commit.Changed
		outcome.mu.Unlock()
		if commit.Changed {
			fmt.Fprintf(run.Log(""), "committed %s on branch %s\n", commit.Hash, name)
		} else {
			fmt.Fprintf(run.Log(""), "nothing to commit; branch %s is at %s\n", name, commit.Hash)
		}

		run.Phase("backup")
		pushed, err := c.opts.Remote.Push(ctx, folder, name, run.Log("backup: "))
		if err != nil {
			return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("branch %s is at %s, but its backup failed: %w; the next commit job pushes it again", name, commit.Hash, err)}
		}
		outcome.mu.Lock()
		outcome.pushed = pushed
		outcome.mu.Unlock()
		if pushed {
			fmt.Fprintf(run.Log(""), "pushed branch %s to the workspace repository\n", name)
		} else {
			fmt.Fprintf(run.Log(""), "branch %s in the workspace repository is already up to date\n", name)
		}
		return nil
	}
}
