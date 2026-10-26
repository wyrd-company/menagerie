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

	mu     sync.Mutex
	own    string
	search *ownSearch
}

// ownSearch is one search for Menagerie's own image, which every caller
// waiting for it shares.
type ownSearch struct {
	done  chan struct{}
	image string
	err   error
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

// ownImage is the image id of Menagerie's own container, found once. A
// failed search is not remembered, so a later job searches again. One
// search runs at a time, bounded by cleanupTimeout and shared by every
// caller; each caller stops waiting when its ctx ends.
func (i *Images) ownImage(ctx context.Context) (string, error) {
	i.mu.Lock()
	if i.own != "" {
		defer i.mu.Unlock()
		return i.own, nil
	}
	s := i.search
	if s == nil {
		s = &ownSearch{done: make(chan struct{})}
		i.search = s
		go i.find(s)
	}
	i.mu.Unlock()
	select {
	case <-s.done:
		return s.image, s.err
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

// find runs one search and publishes its outcome.
func (i *Images) find(s *ownSearch) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	s.image, s.err = i.findOwn(ctx)
	i.mu.Lock()
	if s.err == nil {
		i.own = s.image
	}
	i.search = nil
	i.mu.Unlock()
	close(s.done)
}

// findOwn finds Menagerie's own container by the container ids in the
// paths Docker mounts /etc/hostname and /etc/resolv.conf from. Only when
// those name no container does it fall back to the hostname, Docker's
// default for a container, and then only to a container that carries
// that hostname: any container can take a matching name or hostname,
// but only Menagerie's own is mounted into it.
func (i *Images) findOwn(ctx context.Context) (string, error) {
	if i.Docker == nil {
		return "", &ImageError{Err: errors.New("no runner-image is configured and Menagerie has no Docker client to find its own image")}
	}
	candidates := containerIDs(i.mountInfo())
	byName := len(candidates) == 0
	if byName {
		hostname := i.Hostname
		if hostname == nil {
			hostname = os.Hostname
		}
		if name, err := hostname(); err == nil && name != "" {
			candidates = []string{name}
		}
	}
	for _, id := range candidates {
		info, err := i.Docker.ContainerInspect(ctx, id)
		if err != nil || info.ContainerJSONBase == nil || info.Config == nil || info.Image == "" {
			continue
		}
		if byName && info.Config.Hostname != id {
			continue
		}
		return info.Image, nil
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
