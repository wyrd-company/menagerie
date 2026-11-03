//go:build docker

// What a delete removes, on a real Docker Engine.
package devcontainer_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/docker/docker/api/types/mount"
	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
)

// A file only root can remove, a repository, and a link out of the folder: the
// removal takes them all and leaves the link's target.
func TestRemoveFolderRemovesWhatTheServicesUserCannot(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("removed-space", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `"}`,
		".git/HEAD":                       "ref: refs/heads/removed-space\n",
		"kept/file":                       "content\n",
	})
	outside := filepath.Join(h.root, "outside-file")
	if err := os.WriteFile(outside, []byte("stays\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(folder, "link")); err != nil {
		t.Fatal(err)
	}
	h.mustRun(h.rt.Start, folder)
	status := h.status(folder)
	var inside string
	for _, m := range h.inspect(status.ContainerID).Mounts {
		if m.Source == folder {
			inside = m.Destination
		}
	}
	if inside == "" {
		t.Fatal("the container does not mount the folder")
	}
	h.exec(status.ContainerID, "sh", "-c", "mkdir -p "+inside+"/kept/root && touch "+inside+"/kept/root/file && chown -R 0:0 "+inside+"/kept/root && chmod 700 "+inside+"/kept/root")

	var removeSocket []string
	rt := devcontainer.New(h.client, h.options(devcontainer.Options{RemoveWorkspaceSocket: func(f string) error {
		removeSocket = append(removeSocket, f)
		return nil
	}}))
	if err := rt.RemoveContainers(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ContainerInspect(h.ctx, status.ContainerID); !dockerclient.IsErrNotFound(err) {
		t.Fatalf("container stays: %v", err)
	}
	if err := rt.RemoveFolder(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(folder); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("folder stays: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "stays\n" {
		t.Errorf("link's target: %q %v", data, err)
	}
	if !slices.Equal(removeSocket, []string{folder}) {
		t.Errorf("socket removals %v", removeSocket)
	}
	// What is already gone is done.
	if err := rt.RemoveFolder(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	if err := rt.RemoveContainers(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveContainersTakesEveryServiceAndKeepsNamedVolumes(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("compose-removed", map[string]string{
		".devcontainer/compose.yml":       composeFile,
		".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data"}`,
	})
	h.mustRun(h.rt.Start, folder)
	primary := h.status(folder).ContainerID
	project := h.inspect(primary).Config.Labels["com.docker.compose.project"]
	sidecar := h.service(project, "sidecar")
	var named string
	for _, m := range h.inspect(primary).Mounts {
		if m.Type == mount.TypeVolume {
			named = m.Name
		}
	}
	if own, err := h.rt.MountingOutside(h.ctx, folder, &logProgress{t: t}); err != nil || len(own) != 0 {
		t.Fatalf("own containers count as others: %v %v", own, err)
	}
	if err := h.rt.Stop(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	if err := h.rt.RemoveContainers(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{primary, sidecar} {
		if _, err := h.client.ContainerInspect(h.ctx, id); !dockerclient.IsErrNotFound(err) {
			t.Errorf("container %s stays: %v", id, err)
		}
	}
	if _, err := h.client.VolumeInspect(h.ctx, named); err != nil {
		t.Errorf("named volume %q: %v", named, err)
	}
}

func TestMountingOutsideFindsAContainerOfAnotherWorkspace(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("mounted-space", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `"}`,
	})
	other := h.workspace("mounting-space", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "mounts": ["type=bind,source=` + folder + `,target=/elsewhere"]}`,
	})
	h.mustRun(h.rt.Start, folder)
	if got, err := h.rt.MountingOutside(h.ctx, folder, &logProgress{t: t}); err != nil || len(got) != 0 {
		t.Fatalf("own container: %v %v", got, err)
	}
	h.mustRun(h.rt.Start, other)
	got, err := h.rt.MountingOutside(h.ctx, folder, &logProgress{t: t})
	if err != nil {
		t.Fatal(err)
	}
	id := h.status(other).ContainerID
	if !slices.Equal(got, []string{id[:12]}) {
		t.Errorf("mounting %v, want %s", got, id[:12])
	}
}
