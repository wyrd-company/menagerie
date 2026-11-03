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
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/container"
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
	// BindSources asks the Docker Engine whether a bind source exists. Nil
	// fails the check of a definition that names one outside its folder.
	BindSources throwaway.BindSources
	// RunnerImage returns the image initializeCommand runs in, and the
	// image of a bind source check.
	RunnerImage func(ctx context.Context) (string, error)
	// Own lists Menagerie's own container ids, which Mounting leaves out:
	// Menagerie mounts the workspace root.
	Own []string
	// WorkspaceSocket returns the host folder that holds the workspace's
	// socket, creating the socket when it is missing. The folder is
	// mounted read-only at WorkspaceSocketTarget on each rebuild and on
	// each start that creates the container or reuses one that has the
	// mount; a start that reuses a container without it leaves the
	// container as it is. Nil mounts nothing.
	WorkspaceSocket func(folder string) (string, error)
	// RemoveWorkspaceSocket removes the workspace's socket and its folder.
	// What is already gone is done. Nil removes nothing.
	RemoveWorkspaceSocket func(folder string) error
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

// Docker returns the client the Runtime uses.
func (r *Runtime) Docker() *dockerclient.Client { return r.client }

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

// Start creates the missing bind source folders inside the workspace
// folder, runs initializeCommand and brings the devcontainer up, creating
// it when none exists. It follows devcontainer-builder's up sequencing:
// build the current definition when it needs a build, then Up, which
// reuses and starts an existing container and runs lifecycle phases
// according to their markers.
func (r *Runtime) Start(ctx context.Context, folder string, p Progress) error {
	return r.bringUp(ctx, folder, p, false)
}

// Rebuild creates the missing bind source folders inside the workspace
// folder, runs initializeCommand, builds the current definition, removes
// the workspace's container and brings a new one up. Only the container
// labelled for the workspace is removed (in Compose, the primary
// service): named volumes, networks and other services are kept, and
// Compose Up recreates other services whose configuration changed.
func (r *Runtime) Rebuild(ctx context.Context, folder string, p Progress) error {
	return r.bringUp(ctx, folder, p, true)
}

func (r *Runtime) bringUp(ctx context.Context, folder string, p Progress, replace bool) error {
	b, err := r.begin(folder, p, replace)
	if err != nil {
		return err
	}
	if err := b.Build(ctx); err != nil {
		return err
	}
	return b.Up(ctx)
}

// BringUp is one start or rebuild as separate steps, so that a job can act
// between them: before Build, and on the created container before Up. A job
// calls Build once and then Up, with any number of Create calls between
// them. Start and Rebuild run the same steps with nothing between them and
// no Create.
type BringUp struct {
	r       *Runtime
	dc      *spec.DevContainer
	folder  string
	env     map[string]string
	process []string
	p       Progress
	lock    *features.LockfileState
	auth    RegistryAuth
	// socket is the host folder mounted at WorkspaceSocketTarget; empty
	// mounts none.
	socket string
	// socketHeld is set when a start leaves the socket out to keep a
	// container created without it.
	socketHeld bool
	// replace removes the workspace's containers once, before the first
	// Create or Up.
	replace bool
	built   bool
	// image is a single-container definition's image, and a Compose
	// definition's primary image.
	image string
	// imageBuilt says Build built image; features are what it installed.
	imageBuilt bool
	features   []features.Resolved
}

// BeginStart reads the definition for a start.
func (r *Runtime) BeginStart(folder string, p Progress) (*BringUp, error) {
	return r.begin(folder, p, false)
}

// BeginRebuild reads the definition for a rebuild.
func (r *Runtime) BeginRebuild(folder string, p Progress) (*BringUp, error) {
	return r.begin(folder, p, true)
}

func (r *Runtime) begin(folder string, p Progress, replace bool) (*BringUp, error) {
	p.Phase("read-definition")
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		return nil, err
	}
	local := newLocalEnvironment(r.opts.Environ(), r.opts.LocalEnv)
	for _, name := range refusedLocalEnv(dc.Raw, r.opts.LocalEnv) {
		fmt.Fprintf(p.Log(""), "localEnv %s is not in local-env; it resolves to its default value\n", name)
	}
	return &BringUp{r: r, dc: dc, folder: folder, env: local.values, process: local.process, p: p, replace: replace}, nil
}

// Build creates the missing bind source folders inside the workspace
// folder, runs initializeCommand and builds the current definition when it
// needs a build.
func (b *BringUp) Build(ctx context.Context) error {
	r := b.r
	if err := createBindSources(ctx, b.dc, b.folder, b.env, b.p); err != nil {
		return err
	}
	socket, err := r.workspaceSocket(b.dc, b.folder, b.p)
	if err != nil {
		return err
	}
	if err := r.initialize(ctx, b.dc, b.folder, b.process, b.env, b.p); err != nil {
		return err
	}
	// The lockfile snapshot lives for the whole operation; the builder only
	// prepares the next generation, which is published after Up succeeds.
	if b.lock, err = featureLockfile(b.dc); err != nil {
		return err
	}
	if socket != "" && !b.replace {
		if socket, err = r.keepContainer(ctx, b.folder, socket, b.p); err != nil {
			return err
		}
		b.socketHeld = socket == ""
	}
	if socket != "" {
		nameSocket(b.dc)
	}
	b.socket = socket
	b.auth = r.credentials(ctx)
	switch b.dc.Mode() {
	case spec.ModeSingle:
		err = b.buildSingle(ctx)
	case spec.ModeCompose:
		err = b.buildCompose(ctx)
	default:
		err = fmt.Errorf("unsupported devcontainer mode %s", b.dc.Mode())
	}
	if err != nil {
		return err
	}
	b.built = true
	return nil
}

// Create creates the workspace's container without starting it and returns
// the container's id, the primary service's for Compose. It runs no
// lifecycle command. A rebuild removes the old container first. A later
// Create or a start keeps the container or replaces it under the rule Up
// applies, and Create never starts or stops a container that it keeps. A
// bind that devcontainer-builder cannot create without a start, one with an
// SELinux `z` or `Z` option or with `bind-create-src`, fails here and not in
// Up.
func (b *BringUp) Create(ctx context.Context) (string, error) {
	if err := b.removeForRebuild(ctx); err != nil {
		return "", err
	}
	b.p.Phase("create-container")
	id, err := b.host(ctx, true)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return id, nil
}

// Up starts the workspace's container, creating it when none exists, runs
// its lifecycle commands according to their markers and publishes the
// feature lockfile.
func (b *BringUp) Up(ctx context.Context) error {
	if err := b.removeForRebuild(ctx); err != nil {
		return err
	}
	b.p.Phase("up")
	if _, err := b.host(ctx, false); err != nil {
		return fmt.Errorf("up: %w", err)
	}
	if err := b.lock.Publish(ctx); err != nil {
		return fmt.Errorf("publish feature lockfile: %w", err)
	}
	return nil
}

// removeForRebuild removes the old containers of a rebuild, once.
func (b *BringUp) removeForRebuild(ctx context.Context) error {
	if !b.built {
		return errors.New("the definition was not built")
	}
	if !b.replace {
		return nil
	}
	if err := b.r.removeWorkspaceContainers(ctx, b.folder, b.p); err != nil {
		return err
	}
	b.replace = false
	return nil
}

// host calls devcontainer-builder's Up for the definition's mode and
// returns the container's id.
func (b *BringUp) host(ctx context.Context, noStart bool) (string, error) {
	if b.dc.Mode() == spec.ModeCompose {
		return b.upCompose(ctx, noStart)
	}
	return b.upSingle(ctx, noStart)
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

func (b *BringUp) buildSingle(ctx context.Context) error {
	b.image = b.dc.Image
	if !needsBuild(b.dc) {
		return nil
	}
	b.p.Phase("build")
	b.image = ImageName(b.folder)
	built, err := builder.New().Build(ctx, b.builderOptions([]string{b.image}))
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	b.imageBuilt, b.features = true, built.Features
	return nil
}

func (b *BringUp) buildCompose(ctx context.Context) error {
	b.p.Phase("build")
	primary, err := dockercompose.BuildPrimaryImage(ctx, dockercompose.BuildPrimaryInputs{Spec: b.dc, Workspace: b.folder, LocalEnv: b.env, BuilderOptions: b.builderOptions(nil)})
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	b.image = primary.Image
	if primary.BuildResult != nil {
		b.imageBuilt, b.features = true, primary.BuildResult.Features
	}
	return nil
}

func (b *BringUp) upSingle(ctx context.Context, noStart bool) (string, error) {
	req := hostdocker.UpRequest{
		Spec:                b.dc,
		SpecPath:            b.dc.Path,
		LocalWorkspace:      b.folder,
		Image:               b.image,
		ImageBuilt:          b.imageBuilt,
		Features:            b.features,
		LocalEnv:            b.env,
		RemoteUserUIDUpdate: folderOwner(b.folder),
		NoStart:             noStart,
	}
	if b.socket != "" {
		req.Mounts = append(req.Mounts, mount.Mount{Type: mount.TypeBind, Source: b.socket, Target: WorkspaceSocketTarget, ReadOnly: true})
	}
	res, err := b.r.dockerHost(b.p, b.auth).Up(ctx, req)
	// Up can create the container and then fail, in a lifecycle command
	// for example; its result still says so.
	if res != nil {
		b.noteCreated(res.CreatedThisCall)
	}
	if err != nil {
		return "", err
	}
	fmt.Fprintf(b.p.Log(""), "container %s %s (created=%t started=%t)\n", shortID(res.ContainerID), doneWord(noStart), res.CreatedThisCall, res.StartedThisCall)
	return res.ContainerID, nil
}

func (b *BringUp) upCompose(ctx context.Context, noStart bool) (string, error) {
	req := dockercompose.UpRequest{
		Spec:           b.dc,
		SpecPath:       b.dc.Path,
		LocalWorkspace: b.folder,
		PrimaryImage:   b.image,
		ImageBuilt:     b.imageBuilt,
		Features:       b.features,
		LocalEnv:       b.env,
		NoStart:        noStart,
	}
	if b.socket != "" {
		req.Mounts = append(req.Mounts, "type=bind,source="+b.socket+",target="+WorkspaceSocketTarget+",readonly")
	}
	res, err := b.r.composeHost(b.p, b.auth).Up(ctx, req)
	// Up can recreate the primary and then fail, in a lifecycle command
	// for example; its result still says so.
	if res != nil {
		b.noteCreated(slices.Contains(res.CreatedThisCall, b.dc.Service) || slices.Contains(res.RecreatedThisCall, b.dc.Service))
	}
	if err != nil {
		return "", err
	}
	fmt.Fprintf(b.p.Log(""), "compose project %s %s (primary %s, created %v, recreated %v)\n", res.ProjectName, doneWord(noStart), shortID(res.PrimaryContainerID), res.CreatedThisCall, res.RecreatedThisCall)
	return res.PrimaryContainerID, nil
}

// doneWord is the console's word for what a call to Up did.
func doneWord(noStart bool) string {
	if noStart {
		return "created, not started"
	}
	return "up"
}

// noteCreated tells the console when Up created or recreated the
// container on a start that left the socket out.
func (b *BringUp) noteCreated(created bool) {
	if created && b.socketHeld {
		fmt.Fprintln(b.p.Log(""), noteCreatedWithoutSocket)
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
		// A Compose project keeps its other services running without its
		// primary; its project label still finds them.
		err := r.composeHost(p, anonymous).Stop(ctx, folder)
		if errors.Is(err, dockercompose.ErrProjectNotFound) {
			fmt.Fprintln(p.Log(""), "workspace has no container")
			return nil
		}
		if err == nil {
			fmt.Fprintln(p.Log(""), "stopped the services left of the workspace's compose project")
		}
		return err
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

// RemoveContainers removes the workspace's containers, and for Compose every
// container of its project. Named volumes and networks stay.
func (r *Runtime) RemoveContainers(ctx context.Context, folder string, p Progress) error {
	p.Phase("remove-container")
	project, err := r.composeHost(p, anonymous).Inspect(ctx, folder)
	switch {
	case errors.Is(err, dockercompose.ErrProjectNotFound):
	case err != nil:
		return err
	default:
		for _, c := range project.Containers {
			fmt.Fprintf(p.Log(""), "removing container %s\n", shortID(c.ID))
			err := r.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
			if err != nil && !dockerclient.IsErrNotFound(err) {
				return fmt.Errorf("remove container %s: %w", shortID(c.ID), err)
			}
		}
	}
	return r.removeWorkspaceContainers(ctx, folder, p)
}

// RemovingMark is the name a delete gives a workspace's .git folder before it
// removes the rest, so that from then on no job can commit what is left of the
// folder, and a retried delete knows that its backup has run.
const RemovingMark = ".menagerie-removing"

// removalScript removes everything in the working directory, which is a
// workspace folder mounted at its own path. Renaming is one step, so a removal
// that stops leaves either the whole repository or none under its name.
const removalScript = `set -e
if [ -e .git ] || [ -L .git ]; then mv .git ` + RemovingMark + `; fi
find . -mindepth 1 -delete
`

// RemoveFolder removes the workspace's socket folder and then its folder. A
// throwaway container that runs as root, with only that folder mounted, removes
// what the services' user cannot. The service's user owns the workspace root,
// and removes the empty folder.
func (r *Runtime) RemoveFolder(ctx context.Context, folder string, p Progress) error {
	p.Phase("remove-folder")
	if r.opts.RemoveWorkspaceSocket != nil {
		if err := r.opts.RemoveWorkspaceSocket(folder); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(folder); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if r.opts.Containers == nil || r.opts.RunnerImage == nil {
		return errors.New("removing the workspace folder needs throwaway containers, and none is configured")
	}
	image, err := r.opts.RunnerImage(ctx)
	if err != nil {
		return err
	}
	out := p.Log("remove-folder: ")
	code, err := r.opts.Containers.Run(ctx, throwaway.Command{
		Kind:   throwaway.KindFolderRemoval,
		Job:    jobID(p),
		Image:  image,
		Folder: folder,
		Dir:    folder,
		User:   "0:0",
		Argv:   []string{"sh", "-c", removalScript},
		Stdout: out,
		Stderr: out,
		Log:    p.Log(""),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the container that removes the workspace folder exited with code %d", code)
	}
	return os.Remove(folder)
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

func (b *BringUp) builderOptions(tags []string) builder.Options {
	return builder.Options{
		Spec:            b.dc,
		SpecPath:        b.dc.Path,
		Workspace:       b.folder,
		Docker:          b.r.client,
		Tags:            tags,
		Load:            true,
		FeatureLockfile: b.lock,
		RegistryAuth:    b.auth,
		Logger:          logger(b.p),
		Stdout:          b.p.Log(""),
		Stderr:          b.p.Log(""),
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

// Mounting returns the short ids of the running containers, other than
// Menagerie's own, with a bind mount whose source is folder, is inside it,
// or contains it.
func (r *Runtime) Mounting(ctx context.Context, folder string) ([]string, error) {
	list, err := r.client.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}
	return mounting(list, folder, r.opts.Own), nil
}

// MountingOutside is Mounting without the workspace's own containers: those
// labelled for its folder and those of its Compose project.
func (r *Runtime) MountingOutside(ctx context.Context, folder string, p Progress) ([]string, error) {
	list, err := r.client.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}
	var name string
	project, err := r.composeHost(p, anonymous).Inspect(ctx, folder)
	switch {
	case errors.Is(err, dockercompose.ErrProjectNotFound):
	case err != nil:
		return nil, err
	default:
		name = project.Name
	}
	others := make([]container.Summary, 0, len(list))
	for _, c := range list {
		if c.Labels[labelLocalFolder] == folder || (name != "" && c.Labels[dockercompose.LabelProject] == name) {
			continue
		}
		others = append(others, c)
	}
	return mounting(others, folder, r.opts.Own), nil
}

// mounting returns the short ids of the containers in list, other than
// own, with a bind mount whose source is folder, is inside it, or
// contains it.
func mounting(list []container.Summary, folder string, own []string) []string {
	skip := map[string]bool{}
	for _, id := range own {
		skip[id] = true
	}
	var ids []string
	for _, c := range list {
		if skip[c.ID] {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == mount.TypeBind && (within(m.Source, folder) || within(folder, m.Source)) {
				ids = append(ids, shortID(c.ID))
				break
			}
		}
	}
	return ids
}

// within reports whether p is folder or inside it.
func within(p, folder string) bool {
	rel, err := filepath.Rel(folder, p)
	return err == nil && filepath.IsLocal(rel)
}
