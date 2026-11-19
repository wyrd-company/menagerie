package registry

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// The first start of the schema with applied versions takes each
// workspace's from its applied record, once: a ref that is a version is the
// version, any other ref leaves it unknown with the record's commit, which
// the template's found versions complete when a tag is at it; a workspace
// whose version Menagerie knows already, or that has no readable record,
// keeps what it has; a later start, or a record edited since, imports
// nothing.
func TestTheFirstStartImportsEachWorkspacesAppliedVersionOnce(t *testing.T) {
	r, _, rootDir := world(t)
	ctx := context.Background()
	records := map[string]string{
		"alpha": `{"applications":[{"target":".","template":"` + kit + `@1.0.0","source":"` + kit + `","commit":"c100"}]}`,
		"beta":  `{"applications":[{"target":".","template":"` + kit + `@v1.1.0","source":"` + kit + `","commit":null}]}`,
		"gamma": `{"applications":[{"target":".","template":"` + kit + `@main","source":"` + kit + `","commit":"c120"}]}`,
		"delta": `{"applications":[{"target":".","template":"` + kit + `@main","source":"` + kit + `","commit":"c999"}]}`,
		"other": `{"applications":[{"target":".","template":"gh:example-org/other@2.0.0","source":"gh:example-org/other","commit":"o200"}]}`,
		"plain": `not Toha's JSON`,
		"known": `{"applications":[{"target":".","template":"` + kit + `@1.0.0","source":"` + kit + `","commit":"c100"}]}`,
	}
	var names []string
	for name, record := range records {
		names = append(names, name)
		if err := os.MkdirAll(filepath.Join(rootDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootDir, name, RecordName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	names = append(names, "empty")
	if _, err := r.Data.Discover(ctx, names); err != nil {
		t.Fatal(err)
	}
	// An imported template, which the import of the templates key joined to
	// its workspaces and no check has fetched yet.
	if _, err := r.Data.RegisterTemplate(ctx, store.Template{ID: "kit", Name: "Kit", Address: kit, Source: kit,
		CurrentVersion: "1.2.0", CurrentTag: "1.2.0"}, map[string]store.AppliedVersion{"alpha": {}, "beta": {}, "gamma": {}, "delta": {}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Data.SetAppliedVersion(ctx, "known", store.AppliedVersion{Version: "1.2.0", Commit: "c120"}); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	if err := ImportApplied(ctx, r.Data, r.Root, logf); err != nil {
		t.Fatal(err)
	}
	want := map[string]store.AppliedVersion{
		"alpha": {Version: "1.0.0", Commit: "c100"},
		"beta":  {Version: "1.1.0"},
		"gamma": {Commit: "c120"},
		"delta": {Commit: "c999"},
		"other": {Version: "2.0.0", Commit: "o200"},
		"plain": {},
		"empty": {},
		"known": {Version: "1.2.0", Commit: "c120"},
	}
	if got, err := r.Data.AppliedVersions(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "applied versions") {
		t.Fatalf("%v", logged)
	}
	// The template's first check completes the unknown version whose
	// commit a tag is at.
	if _, err := r.CheckVersions(ctx, "kit"); err != nil {
		t.Fatal(err)
	}
	want["gamma"] = store.AppliedVersion{Version: "1.2.0", Commit: "c120"}
	if got, _ := r.Data.AppliedVersions(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	// Once imported, a record edited later, and a later start, import
	// nothing.
	if err := os.WriteFile(filepath.Join(rootDir, "plain", RecordName), []byte(records["alpha"]), 0o644); err != nil {
		t.Fatal(err)
	}
	logged = nil
	if err := ImportApplied(ctx, r.Data, r.Root, logf); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Data.AppliedVersions(ctx); !reflect.DeepEqual(got, want) || len(logged) != 0 {
		t.Fatalf("%+v %v", got, logged)
	}
	if done, _ := r.Data.Imported(ctx, AppliedImportID); !done {
		t.Fatal("no mark")
	}
}

// When the found versions are there at the import, as on a host whose
// templates a check has listed, the import completes an unknown version at
// once.
func TestTheAppliedImportCompletesAnUnknownVersionFromFoundVersions(t *testing.T) {
	r, _, rootDir := world(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(rootDir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"applications":[{"target":".","template":"` + kit + `@main","source":"` + kit + `","commit":"c110"}]}`
	if err := os.WriteFile(filepath.Join(rootDir, "alpha", RecordName), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Data.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	registered(t, r, "")
	// The registration's join already recorded what the record gives.
	if err := r.Data.SetAppliedVersion(ctx, "alpha", store.AppliedVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := ImportApplied(ctx, r.Data, r.Root, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Data.AppliedVersion(ctx, "alpha"); got != (store.AppliedVersion{Version: "1.1.0", Commit: "c110"}) {
		t.Fatalf("%+v", got)
	}
}
