package throwaway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// inspector answers ContainerInspect from a table and counts calls.
type inspector struct {
	containers map[string]container.InspectResponse
	calls      []string
}

func (i *inspector) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
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

// TestOurOwnContainerIsFoundByHostnameOrMounts finds Menagerie's container
// by a hostname it carries, else by the container ids in its mounts, and
// never takes another container that a hostname names.
func TestOurOwnContainerIsFoundByHostnameOrMounts(t *testing.T) {
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
	docker := &inspector{containers: map[string]container.InspectResponse{}}
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
