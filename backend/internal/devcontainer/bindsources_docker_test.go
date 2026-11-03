//go:build docker

package devcontainer_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
)

// checkRuntime is a runtime whose bind source checks carry the test's run
// label, so that the harness removes what a failed test leaves.
func checkRuntime(h *harness) (*devcontainer.Runtime, *throwaway.Docker) {
	checks := throwaway.NewDocker(h.client, throwaway.DockerOptions{Root: h.root, User: throwaway.ServiceUser(), Labels: map[string]string{"menagerie.test.run": h.runID}})
	return devcontainer.New(h.client, h.options(devcontainer.Options{BindSources: checks})), checks
}

// TestTheEngineAnswersForBindSources runs the check against a real Engine:
// the text it matches and the source kinds it accepts are the Engine's.
func TestTheEngineAnswersForBindSources(t *testing.T) {
	h := newHarness(t)
	_, checks := checkRuntime(h)
	present := t.TempDir()
	file := filepath.Join(present, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for source, want := range map[string]bool{present: true, file: true, filepath.Join(present, "absent"): false} {
		found, err := checks.BindSourceExists(h.ctx, "job-1", baseImage, source, nil)
		if err != nil || found != want {
			t.Errorf("%s: found %t, want %t, %v", source, found, want, err)
		}
	}
	list, err := h.client.ContainerList(h.ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "menagerie.test.run="+h.runID), filters.Arg("label", throwaway.LabelKind+"="+throwaway.KindBindSourceCheck))})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("left %d containers", len(list))
	}
}

// TestAStartCreatesItsMissingBindSources is a clone of a backup: nothing
// holds the folders its definition mounts, and it starts.
func TestAStartCreatesItsMissingBindSources(t *testing.T) {
	h := newHarness(t)
	rt, _ := checkRuntime(h)
	definition, _ := json.Marshal(map[string]any{
		"image": baseImage,
		"mounts": []any{
			"type=bind,source=${localWorkspaceFolder}/repos,target=/mnt/string",
			map[string]any{"type": "bind", "source": "${localWorkspaceFolder}/nested/tasks", "target": "/mnt/object"},
		},
	})
	folder := filepath.Join(h.root, "sample")
	if err := os.MkdirAll(filepath.Join(folder, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, ".devcontainer", "devcontainer.json"), definition, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	status, err := rt.Status(h.ctx, folder)
	if err != nil || status.State != devcontainer.StateRunning {
		t.Fatalf("status %+v %v", status, err)
	}
	info, err := h.client.ContainerInspect(h.ctx, status.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range info.Mounts {
		if m.Type == mount.TypeBind {
			got[m.Destination] = m.Source
		}
	}
	for target, source := range map[string]string{"/mnt/string": "repos", "/mnt/object": "nested/tasks"} {
		if got[target] != filepath.Join(folder, source) {
			t.Errorf("mount %s from %q, mounts %v", target, got[target], got)
		}
		host, err := os.Stat(filepath.Join(folder, source))
		if err != nil || !host.IsDir() {
			t.Errorf("%s: %v", source, err)
		}
	}
}

// TestTheCheckReportsAllThatTheHostLacks needs the Engine's own answers
// for the sources and the network, in one message.
func TestTheCheckReportsAllThatTheHostLacks(t *testing.T) {
	h := newHarness(t)
	rt, _ := checkRuntime(h)
	present := t.TempDir()
	absentOne, absentTwo := filepath.Join(present, "absent-one"), filepath.Join(present, "absent-two")
	consumer := filepath.Join(present, "consumers", "sample-box")
	net := "menagerie-test-" + strings.ToLower(strings.ReplaceAll(h.runID, "/", "-"))
	body := map[string]any{
		"image":   baseImage,
		"runArgs": []string{"--name", "sample-box", "--network", net},
		"mounts": []any{
			"type=bind,source=" + absentOne + ",target=/a",
			"type=bind,source=" + present + ",target=/b",
			"type=bind,source=" + absentTwo + ",target=/c",
			"type=bind,source=" + consumer + ",target=/d",
		},
		"customizations": map[string]any{"menagerie": map[string]any{}},
	}
	folder := filepath.Join(h.root, "sample")
	if err := os.MkdirAll(filepath.Join(folder, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	definition, _ := json.Marshal(body)
	if err := os.WriteFile(filepath.Join(folder, ".devcontainer", "devcontainer.json"), definition, 0o644); err != nil {
		t.Fatal(err)
	}
	err := rt.CheckHostPrerequisites(context.Background(), folder, &logProgress{t: t})
	var missing *devcontainer.HostPrerequisitesError
	if !errors.As(err, &missing) {
		t.Fatalf("error %v", err)
	}
	if want := []string{absentOne, absentTwo}; !reflect.DeepEqual(missing.BindSources, want) {
		t.Errorf("bind sources %q, want %q", missing.BindSources, want)
	}
	if !reflect.DeepEqual(missing.Networks, []string{net}) {
		t.Errorf("networks %q", missing.Networks)
	}
	created, err := h.client.NetworkCreate(h.ctx, net, network.CreateOptions{Labels: map[string]string{"menagerie.test.run": h.runID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.client.NetworkRemove(context.Background(), created.ID) })
	for _, source := range []string{absentOne, absentTwo} {
		if err := os.Mkdir(source, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.CheckHostPrerequisites(context.Background(), folder, &logProgress{t: t}); err != nil {
		t.Fatalf("with all it needs: %v", err)
	}
}
