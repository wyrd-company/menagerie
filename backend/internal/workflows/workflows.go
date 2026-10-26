// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, template-service-api, workspace, job-without-timeout-blocks-workspace]
// ---

// Package workflows runs the create and upgrade operations. Each is a job
// that holds its workspace's job slot from the request to its end,
// including while its template interview waits for answers. Create
// interviews, initializes the workspace repository, generates, commits the
// independent root of the workspace branch, backs it up and starts the
// devcontainer. Upgrade backs up local changes to tracked files, interviews,
// applies the merge, commits, backs up and rebuilds. Apply-branch fetches a
// branch of the workspace repository, backs up every local change, brings
// the branch into the workspace, backs up and rebuilds.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// Templates is the template service. *templates.Client satisfies it.
type Templates interface {
	StartInterview(ctx context.Context, s templates.Selector) (*templates.Reply, error)
	AnswerInterview(ctx context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error)
	Generate(ctx context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error)
	StartUpgrade(ctx context.Context, s templates.Selector) (*templates.Reply, error)
	AnswerUpgrade(ctx context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error)
	ApplyUpgrade(ctx context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error)
}

// Runtime starts, stops and rebuilds devcontainers. *devcontainer.Runtime
// satisfies it.
type Runtime interface {
	Start(ctx context.Context, folder string, p devcontainer.Progress) error
	Stop(ctx context.Context, folder string, p devcontainer.Progress) error
	Rebuild(ctx context.Context, folder string, p devcontainer.Progress) error
}

// Options compose the workflows.
type Options struct {
	Root      *workspace.Root
	Templates Templates
	// Approved holds the approved template ids.
	Approved map[string]bool
	// Remote is the workspace repository backups are pushed to.
	Remote  *backup.Remote
	Runtime Runtime
	// Hooks runs the template service's hooks in throwaway containers.
	// Nil refuses every hook.
	Hooks *throwaway.HookRunner
	// RunnerImage returns the runner image of a template's hooks.
	RunnerImage func(ctx context.Context, templateID string) (string, error)
}

// Workflows owns the create, upgrade and apply-branch operations, and the
// interviews of the first two.
type Workflows struct {
	opts Options

	mu         sync.Mutex
	interviews map[string]*interview
}

// New returns the workflows.
func New(opts Options) *Workflows {
	return &Workflows{opts: opts, interviews: map[string]*interview{}}
}

// Operations returns the create, upgrade and apply-branch operations.
func (w *Workflows) Operations() api.Operations {
	return api.Operations{
		api.OperationCreate:      w.create,
		api.OperationUpgrade:     w.upgrade,
		api.OperationApplyBranch: w.applyBranch,
	}
}

// Job error kinds of the create and upgrade operations, beside the
// container operations' kinds.
const (
	KindWorkspaceExists     = "workspace-exists"
	KindTemplateRefused     = "template-refused"
	KindTemplateUnavailable = "template-service-unavailable"
	KindGenerationFailed    = "generation-failed"
	KindUpgradeFailed       = "upgrade-failed"
	KindNotApplied          = "not-applied"
	KindMergeConflicted     = "merge-conflicted"
	KindBackupFailed        = "backup-failed"
)

// request checks the members both operations share. It changes nothing.
func (w *Workflows) request(req api.JobRequest) (string, error) {
	if err := workspace.ValidName(req.Workspace); err != nil {
		return "", &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
	}
	if _, err := backup.Branch(req.Workspace); err != nil {
		return "", &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
	}
	if req.TemplateID == "" {
		return "", &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-request", Message: "templateId is required"}
	}
	if !w.opts.Approved[req.TemplateID] {
		return "", &api.RequestError{Status: http.StatusNotFound, Kind: "unknown-template", Message: fmt.Sprintf("template %q is not approved", req.TemplateID)}
	}
	return w.opts.Root.FolderPath(req.Workspace)
}

func (w *Workflows) create(req api.JobRequest) (jobsRunner, error) {
	folder, err := w.request(req)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(folder); !errors.Is(err, os.ErrNotExist) {
		return nil, &api.RequestError{Status: http.StatusConflict, Kind: KindWorkspaceExists, Message: fmt.Sprintf("workspace %s already exists", req.Workspace)}
	}
	return (&creation{w: w, name: req.Workspace, folder: folder, template: req.TemplateID}).run, nil
}

func (w *Workflows) upgrade(req api.JobRequest) (jobsRunner, error) {
	if _, err := w.request(req); err != nil {
		return nil, err
	}
	folder, err := w.opts.Root.Folder(req.Workspace)
	if errors.Is(err, workspace.ErrNotFound) {
		return nil, &api.RequestError{Status: http.StatusNotFound, Kind: "workspace-not-found", Message: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	return (&upgrading{w: w, name: req.Workspace, folder: folder, template: req.TemplateID}).run, nil
}

func (w *Workflows) selector(name, folder, template string) templates.Selector {
	return templates.Selector{Workspace: name, Target: folder, TemplateID: template}
}

// hooks is the hook grant of one generate or apply request. Its zero
// value grants nothing, so the template service refuses every hook.
type hooks struct{ grant *throwaway.HookGrant }

// grantHooks lets the template service run the template's hooks against
// targets of kind, for the job's folder, in its runner image and console,
// until revoke. The hooks stop when ctx, the job's, is cancelled.
func (w *Workflows) grantHooks(ctx context.Context, run *jobs.Run, folder, template string, kind throwaway.TargetKind) (hooks, error) {
	if w.opts.Hooks == nil {
		return hooks{}, nil
	}
	image := w.opts.RunnerImage
	grant, err := w.opts.Hooks.Grant(ctx, throwaway.HookJob{
		ID:     run.JobID(),
		Folder: folder,
		Target: kind,
		Image: func(ctx context.Context) (string, error) {
			if image == nil {
				return "", &throwaway.ImageError{Err: throwaway.Reason("no runner image is configured")}
			}
			return image(ctx, template)
		},
		Log: run.Log,
	})
	return hooks{grant}, err
}

func (h hooks) token() string {
	if h.grant == nil {
		return ""
	}
	return h.grant.Token()
}

// revoke refuses the token and waits for the hooks still running, so no
// hook writes to the folder after the job's template step.
func (h hooks) revoke() {
	if h.grant != nil {
		h.grant.Revoke()
	}
}

// kind is the job error kind of a failed generate or apply: the runner
// image's or its registry credentials' when a hook found no image.
func (h hooks) kind(fallback string) string {
	if h.grant == nil {
		return fallback
	}
	err := h.grant.ImageFailure()
	var credErr *registrycreds.Error
	switch {
	case errors.As(err, &credErr):
		return api.KindRegistryCredentialsUnavailable
	case err != nil:
		return api.KindRunnerImageUnavailable
	}
	return fallback
}
