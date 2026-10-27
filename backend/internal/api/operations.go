// ---
// relationships:
//   implements: workspace-api
// ---

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// JobRequest is the body of POST /api/jobs.
type JobRequest struct {
	Operation  string `json:"operation"`
	Workspace  string `json:"workspace"`
	TemplateID string `json:"templateId,omitempty"`
	// Branch names the branch of the workspace repository that
	// apply-branch applies.
	Branch string `json:"branch,omitempty"`
}

// RequestError rejects a job request before a job exists.
type RequestError struct {
	Status  int
	Kind    string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

// Operation validates a job request and returns the runner for its job.
// It runs before the workspace's job slot is acquired and must not change
// any state.
type Operation func(req JobRequest) (jobs.Runner, error)

// Operations maps an operation name to its Operation.
type Operations map[string]Operation

// Container operations.
const (
	OperationStart   = "start"
	OperationStop    = "stop"
	OperationRebuild = "rebuild"
)

// Interview operations: they run a template interview and wait for its
// answers in the waiting-for-answers state.
const (
	OperationCreate  = "create"
	OperationUpgrade = "upgrade"
)

// OperationCommit commits a workspace's changes and backs its branch up.
const OperationCommit = "commit"

// OperationApplyBranch brings a branch of the workspace repository into a
// workspace and rebuilds its devcontainer.
const OperationApplyBranch = "apply-branch"

// ContainerRuntime runs container operations on a workspace folder.
// *devcontainer.Runtime satisfies it.
type ContainerRuntime interface {
	Start(ctx context.Context, folder string, p devcontainer.Progress) error
	Stop(ctx context.Context, folder string, p devcontainer.Progress) error
	Rebuild(ctx context.Context, folder string, p devcontainer.Progress) error
}

// ContainerOperations returns start, stop and rebuild for workspaces under
// root. Each requires an existing workspace folder.
func ContainerOperations(root *workspace.Root, rt ContainerRuntime) Operations {
	op := func(do func(context.Context, string, devcontainer.Progress) error) Operation {
		return func(req JobRequest) (jobs.Runner, error) {
			folder, err := root.Folder(req.Workspace)
			switch {
			case errors.Is(err, workspace.ErrInvalidName):
				return nil, &RequestError{Status: http.StatusBadRequest, Kind: "invalid-workspace-name", Message: err.Error()}
			case errors.Is(err, workspace.ErrNotFound):
				return nil, &RequestError{Status: http.StatusNotFound, Kind: "workspace-not-found", Message: err.Error()}
			case err != nil:
				return nil, err
			}
			return func(ctx context.Context, run *jobs.Run) error {
				return ContainerFailure(do(ctx, folder, run))
			}, nil
		}
	}
	return Operations{
		OperationStart:   op(rt.Start),
		OperationStop:    op(rt.Stop),
		OperationRebuild: op(rt.Rebuild),
	}
}

// Job error kinds of the container operations.
const (
	KindInitializeCommandFailed        = "initialize-command-failed"
	KindRegistryCredentialsUnavailable = "registry-credentials-unavailable"
	KindRunnerImageUnavailable         = "runner-image-unavailable"
	KindIdentityLabelRefused           = "identity-label-refused"
	KindProxyDenied                    = "proxy-denied"
)

// ContainerFailure names the job error kind of a container operation
// failure: registry-credentials-unavailable, runner-image-unavailable,
// initialize-command-failed, identity-label-refused, proxy-denied, or the
// default operation-failed.
func ContainerFailure(err error) error {
	var credErr *registrycreds.Error
	if errors.As(err, &credErr) {
		return &jobs.Failure{Kind: KindRegistryCredentialsUnavailable, Err: err}
	}
	var imageErr *throwaway.ImageError
	if errors.As(err, &imageErr) {
		return &jobs.Failure{Kind: KindRunnerImageUnavailable, Err: err}
	}
	var initErr *devcontainer.InitializeError
	if errors.As(err, &initErr) {
		return &jobs.Failure{Kind: KindInitializeCommandFailed, Err: err}
	}
	var labelErr *devcontainer.IdentityLabelError
	if errors.As(err, &labelErr) {
		return &jobs.Failure{Kind: KindIdentityLabelRefused, Err: err}
	}
	if dockerproxy.Denial(err) != nil {
		return &jobs.Failure{Kind: KindProxyDenied, Err: err}
	}
	return err
}
