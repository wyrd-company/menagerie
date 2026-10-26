// ---
// relationships:
//   implements: menagerie
//   references: [workspace, menagerie-has-host-root]
// ---

// Package devcontainer starts, stops and rebuilds a workspace's
// devcontainer through the devcontainer-builder library, and runs the
// workspace's initializeCommand in throwaway containers before each start
// or rebuild. Single-container and Compose definitions are both supported;
// the mode branch is explicit in each operation.
package devcontainer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/mount"
	dockerclient "github.com/docker/docker/client"
	"github.com/wyrd-company/devcontainer-builder/pkg/builder"
	"github.com/wyrd-company/devcontainer-builder/pkg/buildkit"
	"github.com/wyrd-company/devcontainer-builder/pkg/features"
	hostdocker "github.com/wyrd-company/devcontainer-builder/pkg/host/docker"
	"github.com/wyrd-company/devcontainer-builder/pkg/host/dockercompose"
	"github.com/wyrd-company/devcontainer-builder/pkg/registryauth"
	"github.com/wyrd-company/devcontainer-builder/pkg/spec"

	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
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

// The workspace socket in a workspace's container: the socket folder's
// folder of the workspace is mounted read-only at WorkspaceSocketTarget,
// and WorkspaceSocketEnv names the socket in it.
const (
	WorkspaceSocketTarget = "/run/menagerie"
	WorkspaceSocketName   = "workspace.sock"
	WorkspaceSocketEnv    = "MENAGERIE_WORKSPACE_SOCKET"
	WorkspaceSocketPath   = WorkspaceSocketTarget + "/" + WorkspaceSocketName
)

// Status is a workspace's container state.
type Status struct {
	State       string
	ContainerID string
}

// RegistryAuth answers devcontainer-builder's registry credential requests:
// BuildKit sessions, base image and feature reads, and Engine pulls.
type RegistryAuth = buildkit.RegistryAuth

// Progress receives an operation's phases and output.
type Progress interface {
	// Phase records the step the operation is in.
	Phase(name string)
	// Log returns a writer for output; prefix starts each line.
	Log(prefix string) io.Writer
}

// Options configure a Runtime.
type Options struct {
	// Credentials returns the registry credentials of one start or rebuild.
	// Nil is anonymous for every registry. Every devcontainer-builder call
	// receives a credential source, so none reads the Docker client
	// configuration or runs a credential helper.
	Credentials func(ctx context.Context) RegistryAuth
	// Environ returns Menagerie's own environment. Defaults to os.Environ.
	Environ func() []string
	// LocalEnv names the variables of Environ that a definition may read
	// through ${localEnv:NAME} and that Compose interpolation and
	// initializeCommand receive. Nil allows none. Any other reference
	// resolves to its default value, or empty, and the job log notes it.
	LocalEnv []string
	// Containers runs initializeCommand in throwaway containers. Nil fails
	// a definition that has one.
	Containers throwaway.Containers
	// RunnerImage returns the image initializeCommand runs in.
	RunnerImage func(ctx context.Context) (string, error)
	// WorkspaceSocket returns the host folder that holds the workspace's
	// socket, creating the socket when it is missing. The folder is
	// mounted read-only at WorkspaceSocketTarget on each rebuild and on
	// each start that creates the container or reuses one that has the
	// mount; a start that reuses a container without it leaves the
	// container as it is. Nil mounts nothing.
	WorkspaceSocket func(folder string) (string, error)
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
	local := newLocalEnvironment(r.opts.Environ(), r.opts.LocalEnv)
	for _, name := range refusedLocalEnv(dc.Raw, r.opts.LocalEnv) {
		fmt.Fprintf(p.Log(""), "localEnv %s is not in local-env; it resolves to its default value\n", name)
	}
	env := local.values
	socket, err := r.workspaceSocket(dc, folder, p)
	if err != nil {
		return err
	}
	if err := r.initialize(ctx, dc, folder, local.process, env, p); err != nil {
		return err
	}
	// The lockfile snapshot lives for the whole operation; the builder only
	// prepares the next generation, which is published after Up succeeds.
	lock, err := featureLockfile(dc)
	if err != nil {
		return err
	}
	held := false
	if socket != "" && !replace {
		if socket, err = r.keepContainer(ctx, folder, socket, p); err != nil {
			return err
		}
		held = socket == ""
	}
	if socket != "" {
		nameSocket(dc)
	}
	op := &operation{dc: dc, folder: folder, env: env, p: p, lock: lock, auth: r.credentials(ctx), socket: socket, socketHeld: held}
	switch dc.Mode() {
	case spec.ModeSingle:
		err = r.upSingle(ctx, op, replace)
	case spec.ModeCompose:
		err = r.upCompose(ctx, op, replace)
	default:
		err = fmt.Errorf("unsupported devcontainer mode %s", dc.Mode())
	}
	if err != nil {
		return err
	}
	if err := lock.Publish(ctx); err != nil {
		return fmt.Errorf("publish feature lockfile: %w", err)
	}
	return nil
}

// operation carries one start or rebuild's inputs.
type operation struct {
	dc     *spec.DevContainer
	folder string
	env    map[string]string
	p      Progress
	lock   *features.LockfileState
	auth   RegistryAuth
	// socket is the host folder mounted at WorkspaceSocketTarget; empty
	// mounts none.
	socket string
	// socketHeld is set when a start leaves the socket out to keep a
	// container created without it.
	socketHeld bool
}

// workspaceSocket ensures the workspace's socket and returns the folder to
// mount, or "" for none. Compose reads a mount as comma-separated fields
// with surrounding spaces trimmed, so a folder it would misread is not
// mounted.
func (r *Runtime) workspaceSocket(dc *spec.DevContainer, folder string, p Progress) (string, error) {
	if r.opts.WorkspaceSocket == nil {
		return "", nil
	}
	source, err := r.opts.WorkspaceSocket(folder)
	if err != nil {
		return "", err
	}
	if dc.Mode() == spec.ModeCompose && (strings.Contains(source, ",") || strings.TrimSpace(source) != source) {
		fmt.Fprintln(p.Log(""), "the workspace socket is not mounted: Compose cannot mount a folder whose name holds a comma or ends in a space")
		return "", nil
	}
	return source, nil
}

// Notes for the job console about a start that leaves out the workspace
// socket: noteSocketAtRebuild before Up, when the container exists without
// it; noteCreatedWithoutSocket after Up, when Up created or recreated the
// container anyway, for a change of the definition.
const (
	noteSocketAtRebuild      = "the workspace socket attaches at the next rebuild: this container was created without it, and adding it would recreate the container"
	noteCreatedWithoutSocket = "this start created the container again, without the workspace socket; the socket attaches at the next rebuild"
)

// keepContainer returns the socket folder to mount on a start, or "" when
// the workspace's container exists without the socket mount. Adding a
// mount or a variable changes the configuration devcontainer-builder
// compares, so Compose would recreate the primary; the start keeps the
// container, and the socket attaches at the next rebuild. A container
// that has the mount keeps getting it, so its configuration is unchanged.
//
// devcontainer-builder exposes no way to learn before Up whether Up will
// recreate the container, so a start whose definition changed in another
// way still recreates it, without the socket; noteCreatedWithoutSocket
// tells the console so.
func (r *Runtime) keepContainer(ctx context.Context, folder, socket string, p Progress) (string, error) {
	found, err := hostdocker.New(r.client, hostdocker.Options{}).Inspect(ctx, folder)
	if errors.Is(err, hostdocker.ErrContainerNotFound) {
		return socket, nil
	}
	if err != nil {
		return "", err
	}
	info, err := r.client.ContainerInspect(ctx, found.ID)
	if err != nil {
		return "", fmt.Errorf("inspect container %s: %w", shortID(found.ID), err)
	}
	for _, m := range info.Mounts {
		if m.Destination == WorkspaceSocketTarget {
			return socket, nil
		}
	}
	fmt.Fprintln(p.Log(""), noteSocketAtRebuild)
	return "", nil
}

// nameSocket names the socket in the container's environment unless the
// definition sets that name itself. Only the definition in memory
// changes, not the file.
func nameSocket(dc *spec.DevContainer) {
	if _, set := dc.ContainerEnv[WorkspaceSocketEnv]; set {
		return
	}
	if dc.ContainerEnv == nil {
		dc.ContainerEnv = map[string]any{}
	}
	dc.ContainerEnv[WorkspaceSocketEnv] = WorkspaceSocketPath
}

// featureLockfile reads the definition's feature lockfile snapshot, as
// devcontainer-builder does without lockfile flags: an existing file,
// including an empty initialization marker, is updated; none is created.
func featureLockfile(dc *spec.DevContainer) (*features.LockfileState, error) {
	if dc.Features.Len() == 0 {
		return nil, nil
	}
	return features.ReadLockfile(dc.Path, false, false)
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
	if r.opts.Containers == nil || r.opts.RunnerImage == nil {
		return &InitializeError{Err: errors.New("no container runner is configured")}
	}
	image, err := r.opts.RunnerImage(ctx)
	if err != nil {
		return &InitializeError{Err: err}
	}
	fmt.Fprintf(p.Log(""), "running initializeCommand in throwaway containers of %s\n", image)
	runner := CommandRunner{
		Containers: r.opts.Containers, Image: image, Folder: folder, Env: environ, Job: jobID(p),
		Output: func(prefix string) io.Writer { return p.Log("initializeCommand: " + prefix) },
	}
	if err := runner.Run(ctx, cmd); err != nil {
		return &InitializeError{Err: err}
	}
	return nil
}

// jobID is the id of the job p reports to, when it has one.
func jobID(p Progress) string {
	if j, ok := p.(interface{ JobID() string }); ok {
		return j.JobID()
	}
	return ""
}

func (r *Runtime) upSingle(ctx context.Context, op *operation, replace bool) error {
	dc, folder, p := op.dc, op.folder, op.p
	image := dc.Image
	var built *builder.Result
	if needsBuild(dc) {
		p.Phase("build")
		image = ImageName(folder)
		var err error
		if built, err = builder.New().Build(ctx, r.builderOptions(op, []string{image})); err != nil {
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
		LocalEnv:            op.env,
		RemoteUserUIDUpdate: folderOwner(folder),
	}
	if built != nil {
		req.Features = built.Features
	}
	if op.socket != "" {
		req.Mounts = append(req.Mounts, mount.Mount{Type: mount.TypeBind, Source: op.socket, Target: WorkspaceSocketTarget, ReadOnly: true})
	}
	res, err := r.dockerHost(p, op.auth).Up(ctx, req)
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}
	fmt.Fprintf(p.Log(""), "container %s up (created=%t started=%t)\n", shortID(res.ContainerID), res.CreatedThisCall, res.StartedThisCall)
	op.noteCreated(res.CreatedThisCall)
	return nil
}

func (r *Runtime) upCompose(ctx context.Context, op *operation, replace bool) error {
	dc, folder, p := op.dc, op.folder, op.p
	p.Phase("build")
	primary, err := dockercompose.BuildPrimaryImage(ctx, dockercompose.BuildPrimaryInputs{Spec: dc, Workspace: folder, LocalEnv: op.env, BuilderOptions: r.builderOptions(op, nil)})
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
		LocalEnv:       op.env,
	}
	if primary.BuildResult != nil {
		req.Features = primary.BuildResult.Features
	}
	if op.socket != "" {
		req.Mounts = append(req.Mounts, "type=bind,source="+op.socket+",target="+WorkspaceSocketTarget+",readonly")
	}
	res, err := r.composeHost(p, op.auth).Up(ctx, req)
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}
	fmt.Fprintf(p.Log(""), "compose project %s up (primary %s, created %v, recreated %v)\n", res.ProjectName, shortID(res.PrimaryContainerID), res.CreatedThisCall, res.RecreatedThisCall)
	op.noteCreated(slices.Contains(res.CreatedThisCall, dc.Service) || slices.Contains(res.RecreatedThisCall, dc.Service))
	return nil
}

// noteCreated tells the console when Up created or recreated the
// container on a start that left the socket out.
func (op *operation) noteCreated(created bool) {
	if created && op.socketHeld {
		fmt.Fprintln(op.p.Log(""), noteCreatedWithoutSocket)
	}
}

// Stop stops the workspace's container. The mode comes from the existing
// container's labels, not the definition, so a changed or unreadable
// definition cannot select the wrong stop. A Compose primary stops its
// whole project. A workspace without a container is already stopped.
func (r *Runtime) Stop(ctx context.Context, folder string, p Progress) error {
	p.Phase("stop")
	host := r.dockerHost(p, anonymous)
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
		return r.composeHost(p, anonymous).Stop(ctx, folder)
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
	host := r.dockerHost(p, anonymous)
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

// anonymous is the credential source of operations that pull and build
// nothing. It is still a source, so no call falls back to the Docker
// client configuration.
var anonymous RegistryAuth = registryauth.Static{}

// credentials is the credential source of one start or rebuild.
func (r *Runtime) credentials(ctx context.Context) RegistryAuth {
	if r.opts.Credentials == nil {
		return anonymous
	}
	if auth := r.opts.Credentials(ctx); auth != nil {
		return auth
	}
	return anonymous
}

func (r *Runtime) builderOptions(op *operation, tags []string) builder.Options {
	return builder.Options{
		Spec:            op.dc,
		SpecPath:        op.dc.Path,
		Workspace:       op.folder,
		Docker:          r.client,
		Tags:            tags,
		Load:            true,
		FeatureLockfile: op.lock,
		RegistryAuth:    op.auth,
		Logger:          logger(op.p),
		Stdout:          op.p.Log(""),
		Stderr:          op.p.Log(""),
	}
}

// dockerHost and composeHost wrap the shared client with one operation's
// output and credential source.
func (r *Runtime) dockerHost(p Progress, auth RegistryAuth) *hostdocker.Host {
	out := p.Log("")
	return hostdocker.New(r.client, hostdocker.Options{Logger: logger(p), Stdout: out, Stderr: out, RegistryAuth: auth})
}

func (r *Runtime) composeHost(p Progress, auth RegistryAuth) *dockercompose.Host {
	out := p.Log("")
	return dockercompose.New(r.client, dockercompose.Options{Logger: logger(p), Stdout: out, Stderr: out, RegistryAuth: auth})
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

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
