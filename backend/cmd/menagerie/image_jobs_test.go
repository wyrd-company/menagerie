//go:build pocketid && image

package main

// Helpers the built-image suites share: container jobs through the
// workspace API, and a workspace's devcontainers, found by their label.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// labelFilter selects the devcontainers of a workspace folder.
func labelFilter(folder string) filters.Args {
	return filters.NewArgs(filters.Arg("label", "devcontainer.local_folder="+folder))
}

// removeDevcontainers removes a workspace's devcontainers, found by label.
func removeDevcontainers(p *pocketID, folder string) {
	list, _ := p.docker.ContainerList(context.Background(), container.ListOptions{All: true, Filters: labelFilter(folder)})
	for _, l := range list {
		_ = p.docker.ContainerRemove(context.Background(), l.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}
}

// devcontainerImage returns the image of the devcontainer for a folder.
func devcontainerImage(t *testing.T, p *pocketID, folder string) string {
	t.Helper()
	list, err := p.docker.ContainerList(context.Background(), container.ListOptions{All: true, Filters: labelFilter(folder)})
	if err != nil || len(list) != 1 {
		t.Fatalf("devcontainers for %s: %d, %v", folder, len(list), err)
	}
	return list[0].Image
}

// runJob starts a container operation and waits for it to end.
func runJob(t *testing.T, c *http.Client, site, operation, workspace string) jobs.Snapshot {
	t.Helper()
	header := http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}}
	s := fetch(t, c, http.MethodPost, site+"/api/jobs", strings.NewReader(fmt.Sprintf(`{"operation":%q,"workspace":%q}`, operation, workspace)), header)
	if s.status != http.StatusAccepted {
		t.Fatalf("%s answered %d %s", operation, s.status, s.body)
	}
	var snap jobs.Snapshot
	if err := json.Unmarshal([]byte(s.body), &snap); err != nil {
		t.Fatal(err)
	}
	return waitJob(t, c, site, snap.JobID)
}

// waitJob waits for a job to end and returns its snapshot.
func waitJob(t *testing.T, c *http.Client, site, id string) jobs.Snapshot {
	t.Helper()
	var snap jobs.Snapshot
	for i := 0; ; i++ {
		s := fetch(t, c, http.MethodGet, site+"/api/jobs/"+id, nil, nil)
		if err := json.Unmarshal([]byte(s.body), &snap); err != nil {
			t.Fatalf("job answered %d %s", s.status, s.body)
		}
		if snap.FinishedAt != nil {
			return snap
		}
		if i == jobAttempts {
			t.Fatalf("job %s did not end: %s", id, s.body)
		}
		time.Sleep(imageReadyInterval)
	}
}
