//go:build templateservice && docker

package workflows

// The acceptance scenario with the real devcontainer runtime on the Docker
// Engine named by DOCKER_HOST. The workspace root must have the same path
// for this process and the engine. The definition uses an image without
// features, so no BuildKit build runs.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
)

func TestCreateAndUpgradeRunTheDevcontainer(t *testing.T) {
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.Ping(ctx); err != nil {
		t.Fatalf("Docker Engine unreachable: %v", err)
	}
	dir, err := os.MkdirTemp("", "docker-config-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("DOCKER_CONFIG", dir)
	rt := devcontainer.New(client, devcontainer.Options{})
	a := newAccept(t, rt)
	// Remove only the containers labelled for this test's workspaces.
	t.Cleanup(func() {
		list, _ := client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "devcontainer.local_folder"))})
		for _, c := range list {
			if strings.HasPrefix(c.Labels["devcontainer.local_folder"], a.root+"/") {
				t.Logf("removing container %s for %s", c.ID[:12], c.Labels["devcontainer.local_folder"])
				_ = client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
			}
		}
	})

	var created string
	acceptCreateAndUpgrade(t, a, func(folder string) {
		status, err := rt.Status(ctx, folder)
		if err != nil || status.State != devcontainer.StateRunning {
			t.Fatalf("after create: %+v %v", status, err)
		}
		created = status.ContainerID
		marker, err := os.ReadFile(filepath.Join(folder, "markers/initialize.txt"))
		if err != nil || string(marker) != "initialized\n" {
			t.Fatalf("initializeCommand marker %q %v", marker, err)
		}
		t.Logf("created container %s for %s", created[:12], folder)
	})
	folder := filepath.Join(a.root, "alpha")
	status, err := rt.Status(ctx, folder)
	if err != nil || status.State != devcontainer.StateRunning {
		t.Fatalf("after upgrade: %+v %v", status, err)
	}
	if status.ContainerID == created {
		t.Fatal("the upgrade did not rebuild the container")
	}
	t.Logf("rebuilt container %s for %s", status.ContainerID[:12], folder)
}
