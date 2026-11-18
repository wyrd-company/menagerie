package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
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

// readOnly reads every address as a template of one source at its ref.
type readOnly struct{ calls int }

func (r *readOnly) ReadAddress(_ context.Context, address string) (*templates.Address, error) {
	r.calls++
	base, ref, _ := strings.Cut(strings.TrimPrefix(address, "https://"), "@")
	return &templates.Address{Source: "https://" + base, Reference: &ref, Base: "https://" + base}, nil
}
func (r *readOnly) ListVersions(context.Context, string) (*templates.Versions, error) {
	return nil, errors.New("not used")
}
func (r *readOnly) Check(context.Context, string, string) (*templates.Checked, error) {
	return nil, errors.New("not used")
}

// The first start takes the templates key over; every start that finds it
// warns that it is no longer read, and a later start reads no address.
func TestEachStartWarnsOfTheTemplatesKeyThatTheFirstImported(t *testing.T) {
	cfg, err := config.Parse([]byte(`
workspace-root: /srv/workspaces
workspace-repository: git@git.example.invalid:example-org/backups.git
wyrwood-directory: /run/wyrwood
oidc: {issuer: 'https://issuer.example.invalid', client-id: sample, redirect-url: 'https://menagerie.example.invalid/auth/callback'}
templates:
  - {id: kit-v1, name: Kit, address: 'https://git.example.invalid/example-org/kit@1.0.0'}
  - {id: kit-v2, name: Kit, address: 'https://git.example.invalid/example-org/kit@1.1.0'}
`))
	if err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := testData(t)
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	service := &readOnly{}
	for range 2 {
		if err := importTemplates(context.Background(), cfg, root, data, service); err != nil {
			t.Fatal(err)
		}
	}
	if service.calls != 2 {
		t.Fatalf("addresses read %d times", service.calls)
	}
	if warned := strings.Count(logged.String(), "templates key is no longer read"); warned != 2 {
		t.Fatalf("warned %d times:\n%s", warned, logged.String())
	}
	tpl, err := data.Template(context.Background(), "kit-v2")
	if err != nil || tpl.CurrentVersion != "1.1.0" {
		t.Fatalf("%+v %v", tpl, err)
	}
}
