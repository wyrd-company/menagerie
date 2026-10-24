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
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// JobRequest is the body of POST /api/jobs.
type JobRequest struct {
	Operation  string `json:"operation"`
	Workspace  string `json:"workspace"`
	TemplateID string `json:"templateId,omitempty"`
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
				return failureKind(do(ctx, folder, run))
			}, nil
		}
	}
	return Operations{
		OperationStart:   op(rt.Start),
		OperationStop:    op(rt.Stop),
		OperationRebuild: op(rt.Rebuild),
	}
}

// failureKind names the job error kind of a container operation failure.
func failureKind(err error) error {
	var initErr *devcontainer.InitializeError
	if errors.As(err, &initErr) {
		return &jobs.Failure{Kind: "initialize-command-failed", Err: err}
	}
	var helperErr *devcontainer.CredentialHelperError
	if errors.As(err, &helperErr) {
		return &jobs.Failure{Kind: "credential-helper-unsupported", Err: err}
	}
	return err
}
