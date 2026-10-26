// ---
// relationships:
//   implements: menagerie
//   references: [workspace, menagerie-has-host-root, job-without-timeout-blocks-workspace]
// ---

// Package throwaway runs template hooks and initializeCommand in throwaway
// containers through the Docker Engine API. Each command gets its own
// container: the workspace folder bind-mounted read-write at its own path
// and nothing else, no environment beyond what the caller gives and the
// image's own, the services' user, and the default bridge network. Every
// container carries Menagerie's labels and is removed when its command
// ends or its context is cancelled; leftovers are removed when the
// workspace service starts.
package throwaway

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	// Folder is the workspace folder, bind-mounted read-write at its own
	// path.
	Folder string
	// Dir is the working directory, inside Folder.
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
// or its context is cancelled.
var cleanupTimeout = 30 * time.Second

// ImageError reports that no runner image could be found or pulled.
type ImageError struct {
	// Image is the image reference, empty when none could be chosen.
	Image string
	Err   error
}

func (e *ImageError) Error() string {
	if e.Image == "" {
		return "runner image unavailable: " + e.Err.Error()
	}
	return fmt.Sprintf("runner image %s unavailable: %v", e.Image, e.Err)
}
func (e *ImageError) Unwrap() error { return e.Err }

// Run runs c in a new container and removes it.
func (d *Docker) Run(ctx context.Context, c Command) (code int, err error) {
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return -1, errors.New("the command is empty")
	}
	stdout, stderr := orDiscard(c.Stdout), orDiscard(c.Stderr)
	if err := d.ensureImage(ctx, c.Image, stderr); err != nil {
		return -1, &ImageError{Image: c.Image, Err: err}
	}
	labels := map[string]string{}
	for k, v := range d.opts.Labels {
		labels[k] = v
	}
	labels[LabelKind], labels[LabelRoot], labels[LabelJob] = c.Kind, d.opts.Root, c.Job
	env := c.Env
	if env == nil {
		env = []string{}
	}
	created, err := d.client.ContainerCreate(ctx, &container.Config{
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
		Mounts: []mount.Mount{{Type: mount.TypeBind, Source: c.Folder, Target: c.Folder}},
	}, nil, nil, "")
	if err != nil {
		return -1, fmt.Errorf("create the container: %w", err)
	}
	defer func() {
		if rerr := d.remove(ctx, created.ID); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	if c.Copy != "" {
		if err := d.copyFolder(ctx, created.ID, c.Copy); err != nil {
			return -1, err
		}
	}
	attached, err := d.client.ContainerAttach(ctx, created.ID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return -1, fmt.Errorf("attach to the container: %w", err)
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
		return -1, fmt.Errorf("start the container: %w", err)
	}
	select {
	case res := <-waited:
		// The output stream ends with the container: every process in it
		// has exited.
		<-copied
		if res.Error != nil {
			return -1, fmt.Errorf("wait for the container: %s", res.Error.Message)
		}
		return int(res.StatusCode), nil
	case err := <-waitErr:
		if ctx.Err() != nil {
			return -1, fmt.Errorf("cancelled: %w", context.Cause(ctx))
		}
		return -1, fmt.Errorf("wait for the container: %w", err)
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
		return fmt.Errorf("remove container %s: %w; Menagerie removes it when it next starts", shortID(id), err)
	}
	return nil
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
		return fmt.Errorf("inspect: %w", err)
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
		return fmt.Errorf("pull: %w", err)
	}
	defer rc.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(rc, io.Discard, 0, false, nil); err != nil {
		return fmt.Errorf("pull: %w", err)
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
		return fmt.Errorf("copy the template into the container: %w", err)
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
		return 0, fmt.Errorf("list throwaway containers: %w", err)
	}
	removed := 0
	var errs []error
	for _, c := range list {
		if err := d.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !dockerclient.IsErrNotFound(err) {
			errs = append(errs, fmt.Errorf("remove container %s: %w", shortID(c.ID), err))
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
