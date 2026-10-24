// ---
// relationships:
//   implements: menagerie
//   references: [workspace, menagerie-has-host-root]
// ---

// Package devcontainer starts, stops and rebuilds a workspace's
// devcontainer through the devcontainer-builder library, and runs the
// workspace's initializeCommand inside Menagerie before each start or
// rebuild. Single-container and Compose definitions are both supported;
// the mode branch is explicit in each operation.
package devcontainer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	dockerclient "github.com/docker/docker/client"
	"github.com/wyrd-company/devcontainer-builder/pkg/builder"
	"github.com/wyrd-company/devcontainer-builder/pkg/features"
	hostdocker "github.com/wyrd-company/devcontainer-builder/pkg/host/docker"
	"github.com/wyrd-company/devcontainer-builder/pkg/host/dockercompose"
	"github.com/wyrd-company/devcontainer-builder/pkg/spec"
)

const (
	labelLocalFolder = hostdocker.LabelLocalFolder
	labelConfigFile  = hostdocker.LabelConfigFile
)

// Container states reported by Status.
const (
	StateRunning     = "running"
	StateStopped     = "stopped"
	StateNoContainer = "no-container"
)

// Status is a workspace's container state.
type Status struct {
	State       string
	ContainerID string
}

// RegistryAuth supplies Engine API registry credentials for image pulls.
// *registryauth.Store satisfies it.
type RegistryAuth interface {
	EngineAuth(ref string) (string, error)
}

// Progress receives an operation's phases and output.
type Progress interface {
	// Phase records the step the operation is in.
	Phase(name string)
	// Log returns a writer for output; prefix starts each line.
	Log(prefix string) io.Writer
}

// Options configure a Runtime.
type Options struct {
	// RegistryAuth is used for image pulls by Up. Nil pulls anonymously.
	RegistryAuth RegistryAuth
	// Environ returns the process environment used for ${localEnv:...}
	// substitution and for initializeCommand. Defaults to os.Environ.
	Environ func() []string
}

// Runtime operates workspace devcontainers on one Docker Engine.
type Runtime struct {
	client *dockerclient.Client
	opts   Options
}

// New returns a Runtime using client. The caller owns the client.
func New(client *dockerclient.Client, opts Options) *Runtime {
	if opts.Environ == nil {
		opts.Environ = os.Environ
	}
	return &Runtime{client: client, opts: opts}
}

// Status reports the state of the newest container whose
// devcontainer.local_folder label equals folder. For Compose this is the
// primary service, the only service carrying that label.
func (r *Runtime) Status(ctx context.Context, folder string) (Status, error) {
	info, err := hostdocker.New(r.client, hostdocker.Options{}).Inspect(ctx, folder)
	if errors.Is(err, hostdocker.ErrContainerNotFound) {
		return Status{State: StateNoContainer}, nil
	}
	if err != nil {
		return Status{}, err
	}
	if info.Running {
		return Status{State: StateRunning, ContainerID: info.ID}, nil
	}
	return Status{State: StateStopped, ContainerID: info.ID}, nil
}

// Start runs initializeCommand and brings the devcontainer up, creating
// it when none exists. It follows devcontainer-builder's up sequencing:
// build the current definition when it needs a build, then Up, which
// reuses and starts an existing container and runs lifecycle phases
// according to their markers.
func (r *Runtime) Start(ctx context.Context, folder string, p Progress) error {
	return r.bringUp(ctx, folder, p, false)
}

// Rebuild runs initializeCommand, builds the current definition, removes
// the workspace's container and brings a new one up. Only the container
// labelled for the workspace is removed (in Compose, the primary
// service): named volumes, networks and other services are kept, and
// Compose Up recreates other services whose configuration changed.
func (r *Runtime) Rebuild(ctx context.Context, folder string, p Progress) error {
	return r.bringUp(ctx, folder, p, true)
}

func (r *Runtime) bringUp(ctx context.Context, folder string, p Progress, replace bool) error {
	p.Phase("read-definition")
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		return err
	}
	environ := r.opts.Environ()
	env := envMap(environ)
	if err := r.initialize(ctx, dc, folder, environ, env, p); err != nil {
		return err
	}
	switch dc.Mode() {
	case spec.ModeSingle:
		return r.upSingle(ctx, dc, folder, env, p, replace)
	case spec.ModeCompose:
		return r.upCompose(ctx, dc, folder, env, p, replace)
	default:
		return fmt.Errorf("unsupported devcontainer mode %s", dc.Mode())
	}
}

// InitializeError reports a failed initializeCommand.
type InitializeError struct{ Err error }

func (e *InitializeError) Error() string { return "initializeCommand failed: " + e.Err.Error() }
func (e *InitializeError) Unwrap() error { return e.Err }

func (r *Runtime) initialize(ctx context.Context, dc *spec.DevContainer, folder string, environ []string, env map[string]string, p Progress) error {
	cmd, err := InitializeCommand(dc)
	if err != nil {
		return err
	}
	if cmd.IsZero() {
		return nil
	}
	p.Phase("initialize-command")
	cmd = SubstituteCommand(SubstitutionContext(dc, folder, env), cmd)
	runner := CommandRunner{Dir: folder, Env: environ, Output: func(prefix string) io.Writer { return p.Log("initializeCommand: " + prefix) }}
	if err := runner.Run(ctx, cmd); err != nil {
		return &InitializeError{Err: err}
	}
	return nil
}

func (r *Runtime) upSingle(ctx context.Context, dc *spec.DevContainer, folder string, env map[string]string, p Progress, replace bool) error {
	image := dc.Image
	var built *builder.Result
	if needsBuild(dc) {
		p.Phase("build")
		image = ImageName(folder)
		opts, err := r.builderOptions(dc, folder, []string{image}, p)
		if err != nil {
			return err
		}
		if built, err = builder.New().Build(ctx, opts); err != nil {
			return fmt.Errorf("build: %w", err)
		}
	}
	if replace {
		if err := r.removeWorkspaceContainers(ctx, folder, p); err != nil {
			return err
		}
	}
	p.Phase("up")
	req := hostdocker.UpRequest{
		Spec:                dc,
		SpecPath:            dc.Path,
		LocalWorkspace:      folder,
		Image:               image,
		ImageBuilt:          built != nil,
		LocalEnv:            env,
		RemoteUserUIDUpdate: folderOwner(folder),
	}
	if built != nil {
		req.Features = built.Features
	}
	res, err := r.dockerHost(p).Up(ctx, req)
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}
	fmt.Fprintf(p.Log(""), "container %s up (created=%t started=%t)\n", shortID(res.ContainerID), res.CreatedThisCall, res.StartedThisCall)
	return nil
}

func (r *Runtime) upCompose(ctx context.Context, dc *spec.DevContainer, folder string, env map[string]string, p Progress, replace bool) error {
	p.Phase("build")
	opts, err := r.builderOptions(dc, folder, nil, p)
	if err != nil {
		return err
	}
	primary, err := dockercompose.BuildPrimaryImage(ctx, dockercompose.BuildPrimaryInputs{Spec: dc, Workspace: folder, LocalEnv: env, BuilderOptions: opts})
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if replace {
		if err := r.removeWorkspaceContainers(ctx, folder, p); err != nil {
			return err
		}
	}
	p.Phase("up")
	req := dockercompose.UpRequest{
		Spec:           dc,
		SpecPath:       dc.Path,
		LocalWorkspace: folder,
		PrimaryImage:   primary.Image,
		ImageBuilt:     primary.BuildResult != nil,
		LocalEnv:       env,
	}
	if primary.BuildResult != nil {
		req.Features = primary.BuildResult.Features
	}
	res, err := r.composeHost(p).Up(ctx, req)
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}
	fmt.Fprintf(p.Log(""), "compose project %s up (primary %s, created %v, recreated %v)\n", res.ProjectName, shortID(res.PrimaryContainerID), res.CreatedThisCall, res.RecreatedThisCall)
	return nil
}

// Stop stops the workspace's container. The mode comes from the existing
// container's labels, not the definition, so a changed or unreadable
// definition cannot select the wrong stop. A Compose primary stops its
// whole project. A workspace without a container is already stopped.
func (r *Runtime) Stop(ctx context.Context, folder string, p Progress) error {
	p.Phase("stop")
	host := r.dockerHost(p)
	info, err := host.Inspect(ctx, folder)
	if errors.Is(err, hostdocker.ErrContainerNotFound) {
		fmt.Fprintln(p.Log(""), "workspace has no container")
		return nil
	}
	if err != nil {
		return err
	}
	if project := info.Labels[dockercompose.LabelProject]; project != "" {
		fmt.Fprintf(p.Log(""), "stopping compose project %s\n", project)
		return r.composeHost(p).Stop(ctx, folder)
	}
	fmt.Fprintf(p.Log(""), "stopping container %s\n", shortID(info.ID))
	return host.StopByID(ctx, info.ID)
}

// removeWorkspaceContainers removes every container labelled for the
// workspace, so Up cannot reuse an older duplicate. Anonymous volumes go
// with the container, as devcontainer-builder's Remove does; named
// volumes and networks are kept.
func (r *Runtime) removeWorkspaceContainers(ctx context.Context, folder string, p Progress) error {
	p.Phase("remove-container")
	host := r.dockerHost(p)
	for {
		info, err := host.Inspect(ctx, folder)
		if errors.Is(err, hostdocker.ErrContainerNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(p.Log(""), "removing container %s\n", shortID(info.ID))
		if err := host.RemoveByID(ctx, info.ID); err != nil {
			return fmt.Errorf("remove container %s: %w", shortID(info.ID), err)
		}
	}
}

func (r *Runtime) builderOptions(dc *spec.DevContainer, folder string, tags []string, p Progress) (builder.Options, error) {
	opts := builder.Options{
		Spec:      dc,
		SpecPath:  dc.Path,
		Workspace: folder,
		Docker:    r.client,
		Tags:      tags,
		Load:      true,
		Logger:    logger(p),
		Stdout:    p.Log(""),
		Stderr:    p.Log(""),
	}
	if dc.Features.Len() > 0 {
		lock, err := features.ReadLockfile(dc.Path, false, false)
		if err != nil {
			return opts, err
		}
		opts.FeatureLockfile = lock
	}
	return opts, nil
}

func (r *Runtime) dockerHost(p Progress) *hostdocker.Host {
	out := p.Log("")
	return hostdocker.New(r.client, hostdocker.Options{Logger: logger(p), Stdout: out, Stderr: out, RegistryAuth: r.opts.RegistryAuth})
}

func (r *Runtime) composeHost(p Progress) *dockercompose.Host {
	out := p.Log("")
	return dockercompose.New(r.client, dockercompose.Options{Logger: logger(p), Stdout: out, Stderr: out, RegistryAuth: r.opts.RegistryAuth})
}

// needsBuild reports whether the definition needs a derived image, as
// devcontainer-builder's up decides without build options.
func needsBuild(dc *spec.DevContainer) bool {
	return dc.Build != nil || dc.Dockerfile != "" || dc.Features.Len() > 0
}

// ImageName is the image tag of a single-container workspace build. It is
// devcontainer-builder's default tag for the folder, so a workspace built
// by Menagerie and by devcontainer-builder on the host shares one image.
func ImageName(folder string) string {
	sum := sha256.Sum256([]byte(folder))
	return fmt.Sprintf("dcb-%x:latest", sum[:8])
}

// folderOwner remaps the remote user to the workspace folder's owner, the
// host user that devcontainer-builder on the host would pass.
func folderOwner(folder string) *hostdocker.RemoteUserUIDUpdate {
	info, err := os.Stat(folder)
	if err != nil {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &hostdocker.RemoteUserUIDUpdate{HostUID: int(st.Uid), HostGID: int(st.Gid)}
}

func logger(p Progress) func(string, ...any) {
	out := p.Log("")
	return func(format string, args ...any) {
		fmt.Fprintf(out, strings.TrimSuffix(format, "\n")+"\n", args...)
	}
}

func envMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, pair := range environ {
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[k] = v
		}
	}
	return out
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
