package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// undeclared is a host whose definitions declare nothing and whose containers
// hold no name.
type undeclared struct{}

func (undeclared) Definition(string) (devcontainer.Definition, error) {
	return devcontainer.Definition{}, nil
}
func (undeclared) ContainerHolder(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (undeclared) ContainerNames(context.Context, string) ([]string, error) { return nil, nil }

// A start ends the creates the restart ended, imports the retirement records
// of the previous release on the configured agent, and removes them.
func TestAStartPreparesMenageriesData(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"made", "built"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, ctx := testData(t), context.Background()
	for _, name := range []string{"made", "built", "unmade"} {
		if err := data.BeginCreate(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	records := provisioning.Records{Folder: t.TempDir()}
	if err := os.WriteFile(filepath.Join(records.Folder, "made.json"), []byte(`{"names":["container-old"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Provisioning: &config.Provisioning{Wyrwood: &config.Wyrwood{Agent: "sample-host"}}}
	if err := prepareData(ctx, cfg, root, data, undeclared{}, records); err != nil {
		t.Fatal(err)
	}
	if exists, _ := data.Exists(ctx, "unmade"); exists {
		t.Error("a create with no folder keeps its data")
	}
	held, err := data.Held(ctx, "made")
	if err != nil || !slices.Equal(held.Names, []store.Name{{Name: "container-old", Agent: "sample-host"}}) {
		t.Fatalf("made holds %+v %v", held, err)
	}
	if left, _ := records.Workspaces(); len(left) != 0 {
		t.Errorf("records left %v", left)
	}
	// The mark of the create that made its folder is gone: an abandon, which
	// removes only a workspace being created, keeps it.
	if err := data.AbandonCreate(ctx, "built"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := data.Exists(ctx, "built"); !exists {
		t.Error("the mark of a create that made its folder stayed")
	}
}
