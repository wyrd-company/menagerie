//go:build templateservice && docker

package workflows

// The acceptance scenario with the real devcontainer runtime and throwaway
// containers on the Docker Engine named by DOCKER_HOST. The workspace root
// must have the same path for this process and the engine. The definition
// uses an image without features, so no BuildKit build runs. Hooks run in
// alpine/git, which has the shell and Git the sample template's hook uses;
// initializeCommand runs in alpine.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
)

const (
	hookImage       = "alpine/git:2.54.0"
	initializeImage = "alpine:3.22"
)

// testContainers runs throwaway containers that carry the test's run
// label, and removes every one of them when the test ends.
func testContainers(t *testing.T, client *dockerclient.Client) *throwaway.Docker {
	t.Helper()
	run := filepath.Base(t.TempDir())
	t.Cleanup(func() {
		list, _ := client.ContainerList(context.Background(), container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "menagerie.test.run="+run))})
		for _, c := range list {
			_ = client.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
	})
	return throwaway.NewDocker(client, throwaway.DockerOptions{Root: t.TempDir(), User: throwaway.ServiceUser(), Labels: map[string]string{"menagerie.test.run": run}})
}

func pull(t *testing.T, client *dockerclient.Client, refs ...string) {
	t.Helper()
	for _, ref := range refs {
		if _, err := client.ImageInspect(context.Background(), ref); err == nil {
			continue
		}
		rc, err := client.ImagePull(context.Background(), ref, image.PullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		rc.Close()
	}
}

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
	containers := testContainers(t, client)
	rt := devcontainer.New(client, devcontainer.Options{Containers: containers, RunnerImage: func(context.Context) (string, error) { return initializeImage, nil }})
	pull(t, client, initializeImage, hookImage)
	a := newAcceptWith(t, rt, acceptOptions{containers: containers, image: hookImage})
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

// TestAHungHookEndsWithItsTimedOutJob creates a workspace from a template
// whose hook writes to the folder forever, under a 5 second generate
// limit: the job ends timed-out, no hook container remains, and nothing
// writes to the folder after the job ended.
func TestAHungHookEndsWithItsTimedOutJob(t *testing.T) {
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	containers := testContainers(t, client)
	pull(t, client, hookImage)
	a := newAcceptWith(t, &fakeRuntime{}, acceptOptions{
		containers: containers, image: hookImage,
		limits: jobs.Limits{Phases: map[string]time.Duration{"generate": 5 * time.Second}},
	})
	job, _ := a.drive(t, "create", "alpha", "hangs", map[string]any{"label": "first"})
	snap := waitEnded(t, job)
	if snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != jobs.KindTimedOut {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	left, err := client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", throwaway.LabelJob+"="+snap.JobID))})
	if err != nil || len(left) != 0 {
		t.Fatalf("%d hook containers remain after the job ended: %v", len(left), err)
	}
	ticks := filepath.Join(a.root, "alpha", "ticks.txt")
	before, err := os.ReadFile(ticks)
	if err != nil || len(before) == 0 {
		t.Fatalf("the hook never ran: %q %v", before, err)
	}
	time.Sleep(2 * time.Second)
	if after, _ := os.ReadFile(ticks); string(after) != string(before) {
		t.Fatal("the folder changed after the job ended")
	}
}

// TestACapturedValueStaysOutOfAFailedJobInAThrowawayContainer runs the
// leak templates' hooks in throwaway containers: each job fails with its
// template service kind, and neither its error nor its events carry the
// captured value. The linked working directory exists for this process
// but not in the container.
func TestACapturedValueStaysOutOfAFailedJobInAThrowawayContainer(t *testing.T) {
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	pull(t, client, hookImage)
	a := newAcceptWith(t, &fakeRuntime{}, acceptOptions{containers: testContainers(t, client), image: hookImage})
	for name, kind := range leakKinds {
		t.Run(name, func(t *testing.T) {
			job, _ := a.drive(t, "create", name, name, map[string]any{"label": "x"})
			evidence(t, job)
			noCapturedValue(t, job, kind)
		})
	}
	t.Run("leaks", func(t *testing.T) {
		job, _ := a.drive(t, "create", "leaky", "leaks", map[string]any{"label": "x"})
		evidence(t, job)
		noCapturedValue(t, job, "(hook-failed)")
	})
}
