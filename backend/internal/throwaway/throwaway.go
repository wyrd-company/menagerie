// ---
// relationships:
//   implements: menagerie
//   references: [workspace, menagerie-has-host-root, job-without-timeout-blocks-workspace]
// ---

// Package throwaway runs template hooks and initializeCommand in throwaway
// containers through the Docker Engine API. Each command gets its own
// container: the workspace folder bind-mounted read-write at its own path,
// or a candidate folder copied in and back, and nothing else, no
// environment beyond what the caller gives and the image's own, the
// services' user, and the default bridge network. Every
// container carries Menagerie's labels and is removed when its command
// ends or its context is cancelled; leftovers are removed when the
// workspace service starts. No process in a container gains privileges
// through setuid or file capabilities.
package throwaway

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
)

// Labels every throwaway container carries.
const (
	// LabelKind names what the container runs: KindHook or
	// KindInitializeCommand.
	LabelKind = "menagerie.throwaway"
	// LabelRoot is the workspace root of the Menagerie that created it, so
	// two Menageries on one Docker Engine never remove each other's.
	LabelRoot = "menagerie.workspace-root"
	// LabelJob is the id of the job the command belongs to.
	LabelJob = "menagerie.job"
	// LabelID is a random id of one container, so a container whose
	// creation reply was lost can still be found and removed.
	LabelID = "menagerie.throwaway.id"
)

// Kinds of throwaway containers.
const (
	KindHook              = "hook"
	KindInitializeCommand = "initialize-command"
)

// Command is one command to run in a throwaway container.
type Command struct {
	// Kind is KindHook or KindInitializeCommand.
	Kind string
	// Job is the id of the job the command belongs to.
	Job string
	// Image is the image reference the container runs.
	Image string
	// Folder, when set, is the workspace folder, bind-mounted read-write
	// at its own path.
	Folder string
	// Target, when set, is a folder that is not a host path, such as an
	// upgrade's candidate: it is copied into the container at its own path
	// and, when the command exits, copied back over itself, so it holds
	// exactly what the command left. It is never mounted. A command has
	// Folder or Target, not both.
	Target string
	// Dir is the working directory, inside Folder or Target.
	Dir string
	// Argv is run as given: Argv[0] is the program, found on the image's
	// PATH when it has no slash.
	Argv []string
	// Env is the whole environment Menagerie gives the command, NAME=VALUE.
	// The image's own environment, its PATH included, still applies.
	Env []string
	// Copy, when set, is a folder copied into the container at its own
	// path before the command starts.
	Copy string
	// Stdout and Stderr receive the command's output. Nil discards it.
	Stdout, Stderr io.Writer
	// Log receives Menagerie's own notes on the run, such as an image
	// pull, and never the command's output. Nil discards them.
	Log io.Writer
}

// Containers runs commands in throwaway containers. *Docker satisfies it.
type Containers interface {
	// Run runs c and returns its exit code. A cancelled ctx removes the
	// container, and Run returns only after that removal was attempted.
	Run(ctx context.Context, c Command) (int, error)
}

// RegistryAuth returns the Engine API X-Registry-Auth value for an image
// reference, or "" for an anonymous pull.
type RegistryAuth interface {
	EngineAuth(ref string) (string, error)
}

// DockerOptions configure a Docker.
type DockerOptions struct {
	// Root is the workspace root, the value of LabelRoot.
	Root string
	// User is the containers' user, uid:gid.
	User string
	// Labels are added to every container, for tests.
	Labels map[string]string
	// Credentials returns the registry credentials of one pull. Nil pulls
	// anonymously.
	Credentials func(ctx context.Context) RegistryAuth
}

// Docker runs throwaway containers on one Docker Engine.
type Docker struct {
	client *dockerclient.Client
	opts   DockerOptions
}

// NewDocker returns a Docker using client. The caller owns the client.
func NewDocker(client *dockerclient.Client, opts DockerOptions) *Docker {
	return &Docker{client: client, opts: opts}
}

// cleanupTimeout bounds the removal of a container after its command ends
// or its context is cancelled, and the creation of a container, which a
// cancellation does not interrupt.
var cleanupTimeout = 30 * time.Second

// ImageError reports that no runner image could be found or pulled.
type ImageError struct {
	// Image is the image reference, empty when none could be chosen.
	Image string
	Err   error
}

// Error names the image and why it is unavailable. Err is a *RunError, a
// registry credential error or a reason Menagerie chose, never the
// Engine's own message.
func (e *ImageError) Error() string {
	if e.Image == "" {
		return "runner image unavailable: " + e.Err.Error()
	}
	return fmt.Sprintf("runner image %s unavailable: %v", e.Image, e.Err)
}
func (e *ImageError) Unwrap() error { return e.Err }

// Reason is a reason Menagerie chose that no runner image is available.
// It carries no value from a template, a command or the Engine.
type Reason string

func (r Reason) Error() string { return string(r) }

// Stage is one step of a throwaway container's life.
type Stage string

// Stages of a throwaway container.
const (
	StageInspect Stage = "inspect"
	StagePull    Stage = "pull"
	StageCreate  Stage = "create"
	StageCopy    Stage = "copy"
	StageAttach  Stage = "attach"
	StageStart   Stage = "start"
	StageWait    Stage = "wait"
	StageRemove  Stage = "remove"
	StageList    Stage = "list"
	// StageCopyBack copies Target back out of the container.
	StageCopyBack Stage = "copy-back"
)

var stageText = map[Stage]string{
	StageInspect:  "the runner image could not be inspected",
	StagePull:     "the runner image could not be pulled",
	StageCreate:   "the throwaway container could not be created",
	StageCopy:     "the template could not be copied into the throwaway container",
	StageAttach:   "the throwaway container's output could not be attached",
	StageStart:    "the throwaway container could not start",
	StageWait:     "the throwaway container's exit could not be read",
	StageRemove:   "the throwaway container could not be removed; Menagerie removes it when it next starts",
	StageList:     "the throwaway containers could not be listed",
	StageCopyBack: "the command's changes could not be copied back from the throwaway container",
}

// RunError is a failed stage of a throwaway container. Its text is fixed
// per stage and the Engine error's class: the Engine's own message can
// name the command, its working directory or its output, so it never
// leaves this package.
type RunError struct {
	Stage Stage
	// Class is the Engine error's class, such as not-found or
	// invalid-argument.
	Class string
	// Container is the short id of the container, when one exists.
	Container string
}

func (e *RunError) Error() string {
	if e.Container != "" {
		return fmt.Sprintf("%s (container %s, %s)", stageText[e.Stage], e.Container, e.Class)
	}
	return fmt.Sprintf("%s (%s)", stageText[e.Stage], e.Class)
}

// failed is the RunError of stage for err, which it reads only for its
// class.
func failed(stage Stage, err error, id string) *RunError {
	return &RunError{Stage: stage, Class: class(err), Container: shortID(id)}
}

// class names the Engine error class of err.
func class(err error) string {
	switch {
	case errors.Is(err, context.Canceled) || cerrdefs.IsCanceled(err):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || cerrdefs.IsDeadlineExceeded(err):
		return "deadline-exceeded"
	case cerrdefs.IsNotFound(err):
		return "not-found"
	case cerrdefs.IsInvalidArgument(err):
		return "invalid-argument"
	case cerrdefs.IsConflict(err):
		return "conflict"
	case cerrdefs.IsUnauthorized(err):
		return "unauthorized"
	case cerrdefs.IsPermissionDenied(err):
		return "permission-denied"
	case cerrdefs.IsUnavailable(err):
		return "unavailable"
	case cerrdefs.IsNotImplemented(err):
		return "not-implemented"
	case cerrdefs.IsInternal(err):
		return "internal"
	case dockerclient.IsErrConnectionFailed(err):
		return "connection-failed"
	}
	return "unknown"
}

// Run runs c in a new container and removes it. Every error it returns is
// a *RunError, an *ImageError, or a fixed sentence.
func (d *Docker) Run(ctx context.Context, c Command) (code int, err error) {
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return -1, errors.New("the command is empty")
	}
	if (c.Folder == "") == (c.Target == "") {
		return -1, errors.New("the command needs exactly one of a mounted folder and a copied target")
	}
	stdout, stderr := orDiscard(c.Stdout), orDiscard(c.Stderr)
	if err := d.ensureImage(ctx, c.Image, orDiscard(c.Log)); err != nil {
		return -1, &ImageError{Image: c.Image, Err: err}
	}
	id, err := randomID()
	if err != nil {
		return -1, errors.New("no random container id could be drawn")
	}
	labels := map[string]string{}
	for k, v := range d.opts.Labels {
		labels[k] = v
	}
	labels[LabelKind], labels[LabelRoot], labels[LabelJob], labels[LabelID] = c.Kind, d.opts.Root, c.Job, id
	env := c.Env
	if env == nil {
		env = []string{}
	}
	// A cancellation that arrives while the Engine creates the container
	// would lose its id, so the creation runs to its reply, bounded by
	// cleanupTimeout, and a cancelled ctx removes the container after.
	createCtx, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	var mounts []mount.Mount
	if c.Folder != "" {
		mounts = []mount.Mount{{Type: mount.TypeBind, Source: c.Folder, Target: c.Folder}}
	}
	created, err := d.client.ContainerCreate(createCtx, &container.Config{
		Image: c.Image,
		// The program is the entrypoint, so the image's own entrypoint and
		// command never wrap or extend it.
		Entrypoint:   c.Argv[:1],
		Cmd:          c.Argv[1:],
		Env:          env,
		WorkingDir:   c.Dir,
		User:         d.opts.User,
		Labels:       labels,
		AttachStdout: true,
		AttachStderr: true,
	}, &container.HostConfig{
		Mounts:      mounts,
		SecurityOpt: []string{"no-new-privileges"},
	}, nil, nil, "")
	cancelCreate()
	if err != nil {
		// Without a reply the Engine may still have created it.
		return -1, errors.Join(failed(StageCreate, err, ""), d.removeLabelled(ctx, id))
	}
	defer func() {
		if rerr := d.remove(ctx, created.ID); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	for _, folder := range []string{c.Copy, c.Target} {
		if folder == "" {
			continue
		}
		if err := d.copyFolder(ctx, created.ID, folder); err != nil {
			return -1, err
		}
	}
	attached, err := d.client.ContainerAttach(ctx, created.ID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return -1, failed(StageAttach, err, created.ID)
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = stdcopy.StdCopy(stdout, stderr, attached.Reader)
	}()
	// No output is written after Run returns.
	defer func() {
		attached.Close()
		<-copied
	}()
	waited, waitErr := d.client.ContainerWait(ctx, created.ID, container.WaitConditionNextExit)
	if err := d.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return -1, failed(StageStart, err, created.ID)
	}
	select {
	case res := <-waited:
		// The output stream ends with the container: every process in it
		// has exited.
		<-copied
		if res.Error != nil {
			return -1, &RunError{Stage: StageWait, Class: "unknown", Container: shortID(created.ID)}
		}
		// A cancelled command's target is left as it was copied in.
		if ctx.Err() != nil {
			return -1, fmt.Errorf("cancelled: %w", context.Cause(ctx))
		}
		if c.Target != "" {
			if err := d.copyBack(ctx, created.ID, c.Target, orDiscard(c.Log)); err != nil {
				return -1, err
			}
		}
		return int(res.StatusCode), nil
	case err := <-waitErr:
		if ctx.Err() != nil {
			return -1, fmt.Errorf("cancelled: %w", context.Cause(ctx))
		}
		return -1, failed(StageWait, err, created.ID)
	case <-ctx.Done():
		return -1, fmt.Errorf("cancelled: %w", context.Cause(ctx))
	}
}

// remove force-removes a container and its anonymous volumes, killing a
// command that still runs. It outlives a cancelled ctx by at most
// cleanupTimeout.
func (d *Docker) remove(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := d.client.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !dockerclient.IsErrNotFound(err) {
		return failed(StageRemove, err, id)
	}
	return nil
}

// removeLabelled force-removes every container carrying id as LabelID. It
// outlives a cancelled ctx by at most cleanupTimeout.
func (d *Docker) removeLabelled(ctx context.Context, id string) error {
	listCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	list, err := d.client.ContainerList(listCtx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", LabelID+"="+id))})
	if err != nil {
		return failed(StageList, err, "")
	}
	var errs []error
	for _, c := range list {
		errs = append(errs, d.remove(ctx, c.ID))
	}
	return errors.Join(errs...)
}

// ensureImage pulls ref when the Engine does not have it, as `docker run`
// does, with the configured registry credentials.
func (d *Docker) ensureImage(ctx context.Context, ref string, log io.Writer) error {
	if ref == "" {
		return errors.New("no image reference")
	}
	if _, err := d.client.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !dockerclient.IsErrNotFound(err) {
		return failed(StageInspect, err, "")
	}
	var encoded string
	if d.opts.Credentials != nil {
		if auth := d.opts.Credentials(ctx); auth != nil {
			var err error
			if encoded, err = auth.EngineAuth(ref); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(log, "pulling runner image %s\n", ref)
	rc, err := d.client.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: encoded})
	if err != nil {
		return failed(StagePull, err, "")
	}
	defer rc.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(rc, io.Discard, 0, false, nil); err != nil {
		return failed(StagePull, err, "")
	}
	return nil
}

// copyFolder copies folder into the container at its own path. Entries
// keep their modes and belong to the containers' user; symbolic links are
// copied as links, never followed. Missing parent folders are created by
// the Engine.
func (d *Docker) copyFolder(ctx context.Context, id, folder string) error {
	uid, gid := userIDs(d.opts.User)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, folder, uid, gid)) }()
	defer pr.Close()
	if err := d.client.CopyToContainer(ctx, id, "/", pr, container.CopyToContainerOptions{CopyUIDGID: true}); err != nil {
		return failed(StageCopy, err, id)
	}
	return nil
}

// copyBack makes target on disk hold exactly the container's copy of it:
// added, changed and removed entries, modes, and symbolic and hard links as
// links. A note on an old copy left behind goes to log.
func (d *Docker) copyBack(ctx context.Context, id, target string, log io.Writer) error {
	rc, _, err := d.client.CopyFromContainer(ctx, id, target)
	if err != nil {
		return failed(StageCopyBack, err, id)
	}
	defer rc.Close()
	if err := ReplaceFolder(target, rc, log); err != nil {
		cls := "io"
		if errors.Is(err, ErrUnsafeEntry) {
			cls = "unsafe-entry"
		}
		return &RunError{Stage: StageCopyBack, Class: cls, Container: shortID(id)}
	}
	return nil
}

// writeTar writes folder and everything beneath it, named by their paths
// without the leading slash.
func writeTar(w io.Writer, folder string, uid, gid int) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
			// Sockets, devices and pipes are not template content.
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = strings.TrimPrefix(path, "/")
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = uid, gid, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// userIDs reads uid:gid; a missing or unreadable part is 0.
func userIDs(user string) (int, int) {
	u, g, _ := strings.Cut(user, ":")
	uid, _ := strconv.Atoi(u)
	gid, _ := strconv.Atoi(g)
	return uid, gid
}

// RemoveLeftovers removes every throwaway container of this workspace
// root, which a stopped Menagerie can leave behind. It returns how many it
// removed.
func (d *Docker) RemoveLeftovers(ctx context.Context) (int, error) {
	list, err := d.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", LabelKind),
		filters.Arg("label", LabelRoot+"="+d.opts.Root),
	)})
	if err != nil {
		return 0, failed(StageList, err, "")
	}
	removed := 0
	var errs []error
	for _, c := range list {
		if err := d.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !dockerclient.IsErrNotFound(err) {
			errs = append(errs, failed(StageRemove, err, c.ID))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// ServiceUser is the user the workspace service runs as, uid:gid: the
// owner of the workspace root.
func ServiceUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("container id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
