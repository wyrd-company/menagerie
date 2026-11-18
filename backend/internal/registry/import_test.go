package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

func TestTheTemplatesKeyIsImportedOnceAsOneTemplatePerSource(t *testing.T) {
	r, f, rootDir := world(t)
	ctx := context.Background()
	other := "gh:example-org/other"
	password := "https://sample-user:sample-password@git.example.invalid/example-org/kit"
	f.read[kit+"@1.0.0"] = templates.Address{Source: kit, Reference: ref("1.0.0"), Base: kit}
	f.read["https://sample-user@git.example.invalid/example-org/kit@1.1.0"] = templates.Address{Source: kit, Reference: ref("1.1.0"), Base: kit}
	f.read[other+"@v1"] = templates.Address{Source: other, Reference: ref("v1"), Base: other}
	f.read[other+"@main"] = templates.Address{Source: other, Reference: ref("main"), Base: other}
	for name, source := range map[string]string{"alpha": kit, "beta": other} {
		folder := filepath.Join(rootDir, name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		record := `{"applications":[{"target":".","template":"` + source + `@1.0.0","source":"` + source + `","commit":null}]}`
		if err := os.WriteFile(filepath.Join(folder, RecordName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Data.Discover(ctx, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	var logged []string
	opts := ImportOptions{
		Data: r.Data, Templates: f, Root: r.Root,
		Entries: []Entry{
			{ID: "kit-v1", Name: "Kit, version 1", Address: kit + "@1.0.0"},
			{ID: "kit-v2", Name: "Kit, version 2", Address: "https://sample-user@git.example.invalid/example-org/kit@1.1.0", RunnerImage: "alpine:3.22"},
			{ID: "kit-main", Name: "Kit, main", Address: kit + "@main"},
			{ID: "secret", Name: "Secret", Address: password},
			{ID: "folder", Name: "Folder", Address: "/srv/kit"},
			{ID: "other-v1", Name: "Other", Address: other + "@v1"},
			{ID: "other-main", Name: "Other", Address: other + "@main"},
		},
		Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}
	if err := Import(ctx, opts); err != nil {
		t.Fatal(err)
	}
	list, err := r.Data.Templates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Template{{ID: "kit-v2", Name: "Kit, version 2", Address: kit, Source: kit, RunnerImage: "alpine:3.22",
		CurrentVersion: "1.1.0", CurrentTag: "1.1.0", Workspaces: []string{"alpha"}}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("%+v", list)
	}
	text := strings.Join(logged, "\n")
	for _, named := range []string{"secret", "folder", "other-v1, other-main", "invalid-address", "kit-v2"} {
		if !strings.Contains(text, named) {
			t.Errorf("the log does not name %s:\n%s", named, text)
		}
	}
	for _, quoted := range []string{"sample-password", "example.invalid", "/srv/kit"} {
		if strings.Contains(text, quoted) {
			t.Errorf("the log quotes an address:\n%s", text)
		}
	}
	// The import ran once: a later start, or a changed key, imports nothing.
	calls := len(f.calls)
	opts.Entries = append(opts.Entries, Entry{ID: "late", Name: "Late", Address: other + "@v1"})
	if err := Import(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != calls {
		t.Fatalf("a second start read addresses: %v", f.calls[calls:])
	}
	if list, _ := r.Data.Templates(ctx); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
}

func TestAStartThatCannotReachTheTemplateServiceFailsAndImportsLater(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	f.read[kit+"@1.0.0"] = templates.Address{Source: kit, Reference: ref("1.0.0"), Base: kit}
	f.refuse["address "+kit+"@1.0.0"] = &templates.UnavailableError{Err: errors.New("no socket")}
	opts := ImportOptions{Data: r.Data, Templates: f, Root: r.Root, Wait: time.Millisecond,
		Entries: []Entry{{ID: "kit", Name: "Kit", Address: kit + "@1.0.0"}}}
	var unavailable *templates.UnavailableError
	if err := Import(ctx, opts); !errors.As(err, &unavailable) {
		t.Fatalf("%v", err)
	}
	if done, _ := r.Data.Imported(ctx, ImportID); done {
		t.Fatal("a failed import was marked done")
	}
	delete(f.refuse, "address "+kit+"@1.0.0")
	if err := Import(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if tpl, err := r.Data.Template(ctx, "kit"); err != nil || tpl.CurrentVersion != "1.0.0" || tpl.CurrentCommit != "" {
		t.Fatalf("%+v %v", tpl, err)
	}
}

func TestAHostWithNoTemplatesKeyMarksTheImportWithoutTheTemplateService(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	if err := Import(ctx, ImportOptions{Data: r.Data, Templates: f, Root: r.Root}); err != nil {
		t.Fatal(err)
	}
	if done, _ := r.Data.Imported(ctx, ImportID); !done || len(f.calls) != 0 {
		t.Fatalf("done %t, calls %v", done, f.calls)
	}
}
