package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// Full object ids of the kit's tags and of commits no tag is at.
const (
	at100   = "c100000000000000000000000000000000000000"
	at110   = "c110000000000000000000000000000000000000"
	at120   = "c120000000000000000000000000000000000000"
	untaged = "c999000000000000000000000000000000000000"
)

// toha is an applied record of source at ref and commit; commit "null"
// writes a record with no commit.
func toha(source, ref, commit string) string {
	quoted, _ := json.Marshal(commit)
	c := string(quoted)
	if commit == "null" {
		c = "null"
	}
	return `{"applications":[{"target":".","template":"` + source + `@` + ref + `","source":"` + source + `","commit":` + c + `}]}`
}

// folders writes each workspace folder with its applied record ("" for
// none) and gives it data.
func folders(t *testing.T, r *Registry, rootDir string, records map[string]string) {
	t.Helper()
	var names []string
	for name, record := range records {
		names = append(names, name)
		if err := os.MkdirAll(filepath.Join(rootDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if record == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(rootDir, name, RecordName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Data.Discover(context.Background(), names); err != nil {
		t.Fatal(err)
	}
}

// The first start of the schema with applied versions takes, once, the
// version of each workspace with a template from its applied record by the
// one rule: the found version whose tag is at the record's commit. An
// imported template has no found version yet, so its workspaces are
// unknown with their record's commit, which the template's first check
// completes; a record of another source, or with no well-formed commit,
// gives nothing; a workspace with no template, one whose version
// Menagerie knows already, or one with no readable record keeps what it
// has; a later start, or a record edited since, imports nothing.
func TestTheFirstStartImportsEachWorkspacesAppliedVersionOnce(t *testing.T) {
	r, _, rootDir := world(t)
	ctx := context.Background()
	other := "gh:example-org/other"
	folders(t, r, rootDir, map[string]string{
		"alpha": toha(kit, "1.0.0", at100),
		"beta":  toha(kit, "v1.1.0", "null"),
		"gamma": toha(kit, "main", at120),
		"delta": toha(kit, "main", untaged),
		// Its template is kit, and its record now names another source.
		"drift": toha(other, "5.0.0", "e500000000000000000000000000000000000000"),
		// A ref that names a version at another version's commit.
		"eight": toha(kit, "8.0.0", at100),
		"forge": toha(kit, "1.0.0", "not-a-commit\nfake console line"),
		"short": toha(kit, "1.0.0", at100[:12]),
		"other": toha(other, "2.0.0", "e200000000000000000000000000000000000000"),
		"plain": "not Toha's JSON",
		"known": toha(kit, "1.0.0", at100),
		"empty": "",
	})
	// An imported template, which the import of the templates key joined to
	// its workspaces and no check has fetched yet.
	joined := map[string]store.AppliedVersion{}
	for _, name := range []string{"alpha", "beta", "gamma", "delta", "drift", "eight", "forge", "short", "known"} {
		joined[name] = store.AppliedVersion{}
	}
	if _, err := r.Data.RegisterTemplate(ctx, store.Template{ID: "kit", Name: "Kit", Address: kit, Source: kit,
		CurrentVersion: "1.2.0", CurrentTag: "1.2.0"}, joined); err != nil {
		t.Fatal(err)
	}
	if err := r.Data.SetAppliedVersion(ctx, "known", store.AppliedVersion{Version: "1.2.0", Commit: at120}); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	if err := ImportApplied(ctx, r.Data, r.Root, logf); err != nil {
		t.Fatal(err)
	}
	want := map[string]store.AppliedVersion{
		"alpha": {Commit: at100},
		"beta":  {},
		"gamma": {Commit: at120},
		"delta": {Commit: untaged},
		"drift": {},
		"eight": {Commit: at100},
		"forge": {},
		"short": {},
		"other": {},
		"plain": {},
		"empty": {},
		"known": {Version: "1.2.0", Commit: at120},
	}
	if got, err := r.Data.AppliedVersions(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "applied versions") {
		t.Fatalf("%v", logged)
	}
	// The template's first check completes each unknown version whose
	// commit a tag is at, the version-shaped ref of eight included.
	if _, err := r.CheckVersions(ctx, "kit"); err != nil {
		t.Fatal(err)
	}
	want["alpha"] = store.AppliedVersion{Version: "1.0.0", Commit: at100}
	want["gamma"] = store.AppliedVersion{Version: "1.2.0", Commit: at120}
	want["eight"] = store.AppliedVersion{Version: "1.0.0", Commit: at100}
	if got, _ := r.Data.AppliedVersions(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	// Once imported, a record edited later, and a later start, import
	// nothing.
	if err := os.WriteFile(filepath.Join(rootDir, "beta", RecordName), []byte(toha(kit, "1.1.0", at110)), 0o644); err != nil {
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
// templates a check has listed, the import takes the version whose tag is
// at the record's commit at once, and only that one.
func TestTheAppliedImportTakesTheFoundVersionAtTheRecordsCommit(t *testing.T) {
	r, _, rootDir := world(t)
	ctx := context.Background()
	folders(t, r, rootDir, map[string]string{
		"alpha": toha(kit, "main", at110),
		"beta":  toha(kit, "8.0.0", at100),
		"gamma": toha(kit, "1.2.0", untaged),
	})
	registered(t, r, "")
	// The registration's join already recorded what each record gives.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := r.Data.SetAppliedVersion(ctx, name, store.AppliedVersion{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ImportApplied(ctx, r.Data, r.Root, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]store.AppliedVersion{
		"alpha": {Version: "1.1.0", Commit: at110},
		"beta":  {Commit: at100},
		"gamma": {Commit: untaged},
	}
	if got, _ := r.Data.AppliedVersions(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}
