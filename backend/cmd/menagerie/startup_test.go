package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

const (
	startSource = "https://git.example.invalid/example-org/kit"
	startCommit = "a100000000000000000000000000000000000001"
)

// listed lists the kit at version 1.0.0, its tag at startCommit, and
// checks every ref at that commit.
type listed struct{ readOnly }

func (*listed) ListVersions(context.Context, string) (*templates.Versions, error) {
	return &templates.Versions{Source: startSource, Versions: []templates.Version{{Version: "1.0.0", Tag: "1.0.0", Commit: startCommit}}}, nil
}
func (*listed) Check(context.Context, string, string) (*templates.Checked, error) {
	return &templates.Checked{Source: startSource, Commit: startCommit, Definition: true}, nil
}

// startState is the data an earlier release left: the kit registered, its
// templates key imported, and the workspace alpha joined to it with no
// applied version, whose folder holds an applied record of the kit at
// startCommit.
func startState(t *testing.T) (*workspace.Root, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"applications":[{"target":".","template":"` + startSource + `@1.0.0","source":"` + startSource + `","commit":"` + startCommit + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "alpha", registry.RecordName), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, ctx := testData(t), context.Background()
	if _, err := data.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := data.Import(ctx, registry.ImportID, func(*store.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := data.RegisterTemplate(ctx, store.Template{ID: "kit", Name: "Kit", Address: startSource, Source: startSource,
		CurrentVersion: "1.0.0", CurrentTag: "1.0.0", CurrentCommit: startCommit},
		map[string]store.AppliedVersion{"alpha": {}}); err != nil {
		t.Fatal(err)
	}
	return root, data
}

// The first start of this release takes each workspace's applied version
// over from its applied record, after the templates key.
func TestAFirstStartImportsTheAppliedVersions(t *testing.T) {
	root, data := startState(t)
	ctx := context.Background()
	if err := importTemplateData(ctx, &config.Config{}, root, data, &readOnly{}); err != nil {
		t.Fatal(err)
	}
	applied, err := data.AppliedVersion(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if applied.Commit != startCommit {
		t.Fatalf("a first start did not import alpha's applied record: alpha's applied version is %+v, want commit %s", applied, startCommit)
	}
}

// A start checks every registered template for new versions at once, with
// no request, and records the check.
func TestAStartChecksTheTemplatesWithNoRequest(t *testing.T) {
	root, data := startState(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	versions := &registry.Registry{Data: data, Templates: &listed{}, Root: root}
	stop := checkVersions(ctx, &config.Config{}, versions)
	defer stop()
	const within = 10 * time.Second
	for deadline := time.Now().Add(within); ; time.Sleep(20 * time.Millisecond) {
		tpl, err := data.Template(ctx, "kit")
		if err != nil {
			t.Fatal(err)
		}
		if !tpl.CheckedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a start recorded no version check of the kit within %s, with no request", within)
		}
	}
}
