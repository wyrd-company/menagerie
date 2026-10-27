package throwaway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// inspector answers ContainerInspect from a table and counts calls. A
// non-nil hold blocks every call until it is closed.
type inspector struct {
	mu         sync.Mutex
	containers map[string]container.InspectResponse
	calls      []string
	hold       chan struct{}
}

func (i *inspector) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	if i.hold != nil {
		<-i.hold
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, id)
	c, ok := i.containers[id]
	if !ok {
		return container.InspectResponse{}, errors.New("no such container")
	}
	return c, nil
}

func ours(hostname, image string) container.InspectResponse {
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{Image: image}, Config: &container.Config{Hostname: hostname}}
}

func TestATemplatesImageThenTheDefaultThenOurOwn(t *testing.T) {
	docker := &inspector{containers: map[string]container.InspectResponse{"host-1": ours("host-1", "sha256:own")}}
	images := &Images{Templates: map[string]string{"sample": "runner:template"}, Docker: docker, Hostname: func() (string, error) { return "host-1", nil }, MountInfo: "/absent"}
	ctx := context.Background()
	for id, want := range map[string]string{"sample": "runner:template", "other": "sha256:own"} {
		if got, err := images.ForTemplate(ctx, id); err != nil || got != want {
			t.Errorf("template %s: %q %v", id, got, err)
		}
	}
	if got, err := images.ForWorkspace(ctx); err != nil || got != "sha256:own" {
		t.Errorf("workspace: %q %v", got, err)
	}
	if len(docker.calls) != 1 {
		t.Errorf("own image looked up %d times", len(docker.calls))
	}
	images.Default = "runner:default"
	for _, got := range []func() (string, error){
		func() (string, error) { return images.ForTemplate(ctx, "other") },
		func() (string, error) { return images.ForWorkspace(ctx) },
	} {
		if image, err := got(); err != nil || image != "runner:default" {
			t.Errorf("with a default: %q %v", image, err)
		}
	}
}

// TestAHostnameNeverOutranksOurMounts takes the container Menagerie's
// mounts name even when another container carries Menagerie's hostname.
func TestAHostnameNeverOutranksOurMounts(t *testing.T) {
	id := strings.Repeat("cd", 32)
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfo, []byte("1 2 0:1 /var/lib/docker/containers/"+id+"/resolv.conf /etc/resolv.conf rw - ext4 /dev/x rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docker := &inspector{containers: map[string]container.InspectResponse{
		"menagerie": ours("menagerie", "sha256:impostor"),
		id:          ours("menagerie", "sha256:own"),
	}}
	images := &Images{Docker: docker, Hostname: func() (string, error) { return "menagerie", nil }, MountInfo: mountinfo}
	if got, err := images.ForWorkspace(context.Background()); err != nil || got != "sha256:own" {
		t.Fatalf("got %q %v after %v", got, err, docker.calls)
	}
}

// TestAWaitForOurOwnImageEndsWithItsContext stops a caller waiting for a
// search that another caller started and that the Engine stalls.
func TestAWaitForOurOwnImageEndsWithItsContext(t *testing.T) {
	docker := &inspector{containers: map[string]container.InspectResponse{"host-1": ours("host-1", "sha256:own")}, hold: make(chan struct{})}
	images := &Images{Docker: docker, Hostname: func() (string, error) { return "host-1", nil }, MountInfo: "/absent"}
	first := make(chan error, 1)
	go func() {
		_, err := images.ForWorkspace(context.Background())
		first <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() {
		_, err := images.ForWorkspace(ctx)
		second <- err
	}()
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled caller still waits for the stalled search")
	}
	close(docker.hold)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if got, err := images.ForWorkspace(context.Background()); err != nil || got != "sha256:own" {
		t.Fatalf("after the search: %q %v", got, err)
	}
}

// TestOurOwnContainerIsFoundByItsMounts finds Menagerie's container by
// the container ids in its mounts, and never takes another container that
// its hostname names.
func TestOurOwnContainerIsFoundByItsMounts(t *testing.T) {
	id := strings.Repeat("ab", 32)
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfo, []byte("1 2 0:1 /var/lib/docker/containers/"+id+"/hostname /etc/hostname rw - ext4 /dev/x rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docker := &inspector{containers: map[string]container.InspectResponse{
		"menagerie": ours("other-host", "sha256:other"),
		id:          ours("menagerie", "sha256:own"),
	}}
	images := &Images{Docker: docker, Hostname: func() (string, error) { return "menagerie", nil }, MountInfo: mountinfo}
	if got, err := images.ForWorkspace(context.Background()); err != nil || got != "sha256:own" {
		t.Fatalf("got %q %v after %v", got, err, docker.calls)
	}
}

func TestNoOwnContainerIsAnImageErrorThatIsNotRemembered(t *testing.T) {
	// Without mounts that name a container, the hostname names one only
	// when that container carries it.
	docker := &inspector{containers: map[string]container.InspectResponse{"host-1": ours("other-host", "sha256:other")}}
	images := &Images{Docker: docker, Hostname: func() (string, error) { return "host-1", nil }, MountInfo: "/absent"}
	_, err := images.ForWorkspace(context.Background())
	var imageErr *ImageError
	if !errors.As(err, &imageErr) || !strings.Contains(err.Error(), "set runner-image") {
		t.Fatalf("got %v", err)
	}
	docker.containers["host-1"] = ours("host-1", "sha256:own")
	if got, err := images.ForWorkspace(context.Background()); err != nil || got != "sha256:own" {
		t.Fatalf("after the container appeared: %q %v", got, err)
	}
	if _, err := (&Images{}).ForWorkspace(context.Background()); !errors.As(err, &imageErr) {
		t.Fatalf("without Docker: %v", err)
	}
}
