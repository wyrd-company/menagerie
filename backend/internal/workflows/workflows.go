// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, template-service-api, workspace, job-without-timeout-blocks-workspace]
// ---

// Package workflows runs the create and upgrade operations. Each is a job
// that holds its workspace's job slot from the request to its end,
// including while its template interview waits for answers. Create
// interviews, initializes the workspace repository, generates, checks what
// the definition needs from the host, commits the independent root of the
// workspace branch, backs it up and starts the devcontainer. Upgrade backs up local changes to tracked files, interviews,
// applies the merge, commits, backs up and rebuilds. Apply-branch fetches a
// branch of the workspace repository, backs up every local change, brings
// the branch into the workspace, backs up and rebuilds. Delete backs up and
// removes the workspace from the host.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// Templates is the template service. *templates.Client satisfies it.
type Templates interface {
	StartInterview(ctx context.Context, s templates.Selector) (*templates.Reply, error)
	AnswerInterview(ctx context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error)
	Generate(ctx context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error)
	StartUpgrade(ctx context.Context, s templates.Selector, reanswer bool) (*templates.Reply, error)
	AnswerUpgrade(ctx context.Context, s templates.Selector, session, submission json.RawMessage) (*templates.Reply, error)
	ApplyUpgrade(ctx context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error)
	// CheckSince checks a version and says whether its commit is since or
	// descends from it.
	CheckSince(ctx context.Context, address, ref, since string) (*templates.Checked, error)
}

// Runtime starts, stops and rebuilds devcontainers with their provisioning,
// reads their state and lists the running containers that mount a folder.
// *provisioning.Steps satisfies it.
type Runtime interface {
	Status(ctx context.Context, folder string) (devcontainer.Status, error)
	Start(ctx context.Context, folder string, p devcontainer.Progress) error
	Stop(ctx context.Context, folder string, p devcontainer.Progress) error
	Rebuild(ctx context.Context, folder string, p devcontainer.Progress) error
	Mounting(ctx context.Context, folder string) ([]string, error)
	// Create starts a workspace with no commit: it checks the host and runs
	// save, which commits and backs the workspace up, before the start.
	Create(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error
	// Delete removes a workspace from the host, and runs save, which commits
	// and backs the workspace up, before it removes anything.
	Delete(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error
}

// Remotes resolves a workspace's backup repository.
// *backuprepos.Registry satisfies it.
type Remotes interface {
	For(ctx context.Context, workspace string) (*backup.Remote, error)
	// Remote is the registered repository repo, reached with its
	// credential.
	Remote(repo store.BackupRepository) *backup.Remote
}

// Options compose the workflows.
type Options struct {
	Root *workspace.Root
	// Data is Menagerie's data, which holds the registered templates and a
	// workspace from the start of its create.
	Data      *store.Store
	Policies  api.PolicySource
	Templates Templates
	// Remotes gives each workspace the backup repository its backups go to
	// and Apply branch fetches from.
	Remotes Remotes
	Runtime Runtime
	// Hooks runs the template service's hooks in throwaway containers.
	// Nil refuses every hook.
	Hooks *throwaway.HookRunner
	// RunnerImage returns the runner image of a registered template's hooks.
	RunnerImage func(ctx context.Context, templateID string) (string, error)
	// RestartLimit bounds the start of a devcontainer an apply refused
	// after stopping it; zero is DefaultRestartLimit.
	RestartLimit time.Duration
}

// Workflows owns the create, upgrade, apply-branch and delete operations, and the
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

// Operations returns the start, create, upgrade, apply-branch, delete and
// move-backup operations.
func (w *Workflows) Operations() api.Operations {
	return api.Operations{
		api.OperationStart:       w.start,
		api.OperationCreate:      w.create,
		api.OperationUpgrade:     w.upgrade,
		api.OperationApplyBranch: w.applyBranch,
		api.OperationDelete:      w.delete,
		api.OperationMoveBackup:  w.moveBackup,
	}
}

// Request kinds of the create and upgrade operations.
const (
	KindTemplateNotRegistered = "template-not-registered"
	KindUpgradeNotNeeded      = "upgrade-not-needed"
	KindTemplateVersionMoved  = "template-version-moved"
)

// moved refuses a create or an upgrade of a template whose current
// version's tag has moved.
func moved(tpl store.Template) error {
	return &api.RequestError{Status: http.StatusConflict, Kind: KindTemplateVersionMoved, Message: registry.MovedMessage(tpl)}
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

// request checks the workspace name both operations share. It changes
// nothing.
func (w *Workflows) request(req api.JobRequest) (string, error) {
	if err := workspace.ValidName(req.Workspace); err != nil {
		return "", &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
	}
	if _, err := backup.Branch(req.Workspace); err != nil {
		return "", &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
	}
	return w.opts.Root.FolderPath(req.Workspace)
}

// create names a registered template, whose current version its job pins.
// It refuses a workspace whose folder exists, one Menagerie's data holds,
// being created or listed with no folder, and a template whose current
// version's tag has moved.
func (w *Workflows) create(req api.JobRequest) (jobsRunner, error) {
	folder, err := w.request(req)
	if err != nil {
		return nil, err
	}
	if req.TemplateID == "" {
		return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-request", Message: "templateId is required"}
	}
	tpl, err := w.opts.Data.Template(context.Background(), req.TemplateID)
	if errors.Is(err, store.ErrNoTemplate) {
		return nil, &api.RequestError{Status: http.StatusNotFound, Kind: api.KindUnknownTemplate, Message: fmt.Sprintf("no registered template has the id %q", req.TemplateID)}
	} else if err != nil {
		return nil, err
	}
	if tpl.CurrentMoved() {
		return nil, moved(tpl)
	}
	backups, err := w.createBackups(req.BackupRepositoryID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(folder); !errors.Is(err, os.ErrNotExist) {
		return nil, &api.RequestError{Status: http.StatusConflict, Kind: KindWorkspaceExists, Message: fmt.Sprintf("workspace %s already exists", req.Workspace)}
	}
	claimed, err := w.opts.Data.Claimed(context.Background(), req.Workspace)
	if err != nil {
		return nil, err
	}
	if claimed {
		return nil, &api.RequestError{Status: http.StatusConflict, Kind: KindWorkspaceExists, Message: fmt.Sprintf("workspace %s already exists in Menagerie's data: it is being created, or its folder is gone and it still holds a binding or a key, which its delete removes", req.Workspace)}
	}
	return (&creation{w: w, name: req.Workspace, folder: folder, template: req.TemplateID, backups: backups, policies: req.Policies}).run, nil
}

// KindUnknownBackupRepository refuses a request that names no registered
// backup repository.
const KindUnknownBackupRepository = api.KindUnknownBackupRepository

// createBackups is the backup repository a create names, or, when it names
// none, the only registered one.
func (w *Workflows) createBackups(id string) (string, error) {
	ctx := context.Background()
	if id == "" {
		registered, err := w.opts.Data.BackupRepositories(ctx)
		if err != nil {
			return "", err
		}
		if len(registered) != 1 {
			return "", &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-request", Message: "backupRepositoryId is required: name one of the registered backup repositories"}
		}
		return registered[0].ID, nil
	}
	if _, err := w.opts.Data.BackupRepository(ctx, id); errors.Is(err, store.ErrNoBackupRepository) {
		return "", &api.RequestError{Status: http.StatusNotFound, Kind: KindUnknownBackupRepository, Message: fmt.Sprintf("no registered backup repository has the id %q", id)}
	} else if err != nil {
		return "", err
	}
	return id, nil
}

// upgrade applies the current version of the workspace's registered
// template. It takes no templateId, and it is refused when the workspace has
// no registered template, when its applied version is that version or a
// newer one, so no upgrade applies an older version, and while that
// version's tag has moved.
func (w *Workflows) upgrade(req api.JobRequest) (jobsRunner, error) {
	if _, err := w.request(req); err != nil {
		return nil, err
	}
	if req.TemplateID != "" {
		return nil, &api.RequestError{Status: http.StatusBadRequest, Kind: "invalid-request", Message: "an upgrade takes no templateId: it applies the current version of the workspace's template"}
	}
	folder, err := w.opts.Root.Folder(req.Workspace)
	if errors.Is(err, workspace.ErrNotFound) {
		return nil, &api.RequestError{Status: http.StatusNotFound, Kind: "workspace-not-found", Message: err.Error()}
	}
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	id, err := w.opts.Data.WorkspaceTemplate(ctx, req.Workspace)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, &api.RequestError{Status: http.StatusConflict, Kind: KindTemplateNotRegistered, Message: fmt.Sprintf("workspace %s has no registered template; register the template its applied record names, which joins it", req.Workspace)}
	}
	tpl, err := w.opts.Data.Template(ctx, id)
	if err != nil {
		return nil, err
	}
	applied, err := w.opts.Data.AppliedVersion(ctx, req.Workspace)
	if err != nil {
		return nil, err
	}
	if applied.Version != "" && !registry.Newer(tpl.CurrentVersion, applied.Version) {
		return nil, &api.RequestError{Status: http.StatusConflict, Kind: KindUpgradeNotNeeded, Message: fmt.Sprintf("workspace %s already has version %s of template %s, whose current version is %s", req.Workspace, applied.Version, tpl.ID, tpl.CurrentVersion)}
	}
	if tpl.CurrentMoved() {
		return nil, moved(tpl)
	}
	return (&upgrading{w: w, name: req.Workspace, folder: folder, template: id, reanswer: req.Reanswer}).run, nil
}

// start starts a workspace, and finishes a create that failed after
// generation.
func (w *Workflows) start(req api.JobRequest) (jobsRunner, error) {
	folder, err := api.ExistingFolder(w.opts.Root, req.Workspace)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, run *jobs.Run) error {
		pending, err := backup.CreatePending(folder, req.Workspace)
		if err != nil {
			return &jobs.Failure{Kind: KindBackupFailed, Err: fmt.Errorf("reading the workspace repository failed: %w; the devcontainer was not started", err)}
		}
		if !pending {
			return api.ContainerFailure(w.opts.Runtime.Start(ctx, folder, run))
		}
		fmt.Fprintf(run.Log(""), "the workspace's create did not finish; finishing it\n")
		saving := &saving{w: w, name: req.Workspace, folder: folder,
			output:  backup.Output{Record: backup.RecordName},
			message: fmt.Sprintf("Create workspace %s", req.Workspace),
		}
		return saving.finish(ctx, run, "nothing was committed or started; start the workspace again when this is fixed")
	}, nil
}

// selector names the workspace and the template version a job pinned: its
// registered template's address with the current version's tag and commit.
func (w *Workflows) selector(name, folder string, tpl store.Template) templates.Selector {
	return templates.Selector{Workspace: name, Target: folder, Address: tpl.Address, Ref: tpl.CurrentTag, Commit: tpl.CurrentCommit}
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
