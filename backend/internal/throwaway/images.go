// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

package throwaway

import (
	"bufio"
	"context"
	"errors"
	"os"
	"regexp"
	"sync"

	"github.com/docker/docker/api/types/container"
)

// Inspector reads a container. *dockerclient.Client satisfies it.
type Inspector interface {
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
}

// Images chooses the runner image of a throwaway container: a template's
// own runner image, else the configured default, else the image of the
// container Menagerie itself runs in.
type Images struct {
	// Default is the configured runner-image; empty for Menagerie's own.
	Default string
	// Templates maps a template id to its runner-image.
	Templates map[string]string
	// Docker reads Menagerie's own container.
	Docker Inspector
	// Hostname and MountInfo locate Menagerie's own container; they
	// default to os.Hostname and /proc/self/mountinfo.
	Hostname  func() (string, error)
	MountInfo string

	mu  sync.Mutex
	own string
}

// ForTemplate is the runner image of a template's hooks.
func (i *Images) ForTemplate(ctx context.Context, id string) (string, error) {
	if image := i.Templates[id]; image != "" {
		return image, nil
	}
	return i.ForWorkspace(ctx)
}

// ForWorkspace is the runner image of initializeCommand, which belongs to
// no template.
func (i *Images) ForWorkspace(ctx context.Context) (string, error) {
	if i.Default != "" {
		return i.Default, nil
	}
	return i.ownImage(ctx)
}

// ownImage is the image id of Menagerie's own container, found once: by
// its hostname, Docker's default for a container, else by the container
// id in the paths Docker mounts /etc/hostname and /etc/resolv.conf from.
// A failed search is not remembered, so a later job searches again.
func (i *Images) ownImage(ctx context.Context) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.own != "" {
		return i.own, nil
	}
	if i.Docker == nil {
		return "", &ImageError{Err: errors.New("no runner-image is configured and Menagerie has no Docker client to find its own image")}
	}
	hostname := i.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	var candidates []string
	name, nameErr := hostname()
	if nameErr == nil && name != "" {
		candidates = append(candidates, name)
	}
	candidates = append(candidates, containerIDs(i.mountInfo())...)
	for _, id := range candidates {
		info, err := i.Docker.ContainerInspect(ctx, id)
		if err != nil || info.Config == nil || info.Image == "" {
			continue
		}
		// A container found by name is ours only when it carries our
		// hostname.
		if id == name && info.Config.Hostname != name {
			continue
		}
		i.own = info.Image
		return i.own, nil
	}
	return "", &ImageError{Err: errors.New("no runner-image is configured and Menagerie could not find its own container to use its image; set runner-image")}
}

func (i *Images) mountInfo() string {
	if i.MountInfo != "" {
		return i.MountInfo
	}
	return "/proc/self/mountinfo"
}

var containerPath = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// containerIDs lists, once each, the container ids in the mount sources
// of path.
func containerIDs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{}
	var ids []string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		for _, m := range containerPath.FindAllStringSubmatch(s.Text(), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				ids = append(ids, m[1])
			}
		}
	}
	return ids
}
