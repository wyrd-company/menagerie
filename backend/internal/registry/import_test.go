package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		CurrentVersion: "1.1.0", CurrentTag: "1.1.0", Workspaces: []string{"alpha"}, Versions: []store.Version{}}}
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

// replying is a template service on a unix socket that answers every
// request with status and body.
func replying(t *testing.T, status int, body string) *templates.Client {
	t.Helper()
	// A unix socket's path must stay short.
	dir, err := os.MkdirTemp("", "tsvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return templates.New(socket)
}

// A template service fault, or any error reply but its refusal of the
// address, is no refusal: the start fails, naming the entry by its id and
// not by the reply's message, the import records nothing and no mark, and
// the next start imports the entry.
func TestATemplateServiceFaultFailsTheStartAndImportsLater(t *testing.T) {
	for _, reply := range []struct {
		status int
		kind   string
	}{{500, "internal-error"}, {500, "io-failed"}, {400, "malformed-request"}} {
		t.Run(reply.kind, func(t *testing.T) {
			r, f, _ := world(t)
			ctx := context.Background()
			var logged []string
			// The service answers at once; the wait bounds each read, so it
			// must outlast a slow run's request.
			opts := ImportOptions{Data: r.Data, Root: r.Root, Wait: 10 * time.Second,
				Templates: replying(t, reply.status, `{"error":{"kind":"`+reply.kind+`","message":"reading https://sample-user:sample-password@git.example.invalid/kit failed"}}`),
				Entries:   []Entry{{ID: "kit", Name: "Kit", Address: kit + "@1.0.0"}},
				Logf:      func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
			}
			err := Import(ctx, opts)
			if err == nil || !strings.Contains(err.Error(), "entry kit") || !strings.Contains(err.Error(), reply.kind) {
				t.Fatalf("the import did not fail naming the entry and the kind: %v", err)
			}
			if strings.Contains(err.Error(), "sample-password") || strings.Contains(err.Error(), "example.invalid") {
				t.Fatalf("the error quotes the reply's message: %v", err)
			}
			if done, _ := r.Data.Imported(ctx, ImportID); done {
				t.Fatal("a failed import was marked done")
			}
			if list, _ := r.Data.Templates(ctx); len(list) != 0 {
				t.Fatalf("a failed import stored %+v", list)
			}
			if text := strings.Join(logged, "\n"); strings.Contains(text, "not imported") {
				t.Fatalf("the fault was logged as a skipped entry:\n%s", text)
			}
			f.read[kit+"@1.0.0"] = templates.Address{Source: kit, Reference: ref("1.0.0"), Base: kit}
			opts.Templates = f
			if err := Import(ctx, opts); err != nil {
				t.Fatal(err)
			}
			if tpl, err := r.Data.Template(ctx, "kit"); err != nil || tpl.CurrentVersion != "1.0.0" {
				t.Fatalf("the next start did not import the entry: %+v %v", tpl, err)
			}
		})
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

// hanging is a template service on a unix socket that takes each request
// and never answers.
func hanging(t *testing.T) *templates.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "tsvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { close(released); _ = server.Close() })
	return templates.New(socket)
}

// A template service that takes the request and never answers fails the
// start within the wait, recording no mark.
func TestATemplateServiceThatNeverAnswersFailsTheStartWithinTheWait(t *testing.T) {
	r, _, _ := world(t)
	ctx := context.Background()
	opts := ImportOptions{Data: r.Data, Root: r.Root, Wait: 300 * time.Millisecond, Templates: hanging(t),
		Entries: []Entry{{ID: "kit", Name: "Kit", Address: kit + "@1.0.0"}}}
	done := make(chan error, 1)
	go func() { done <- Import(ctx, opts) }()
	select {
	case err := <-done:
		var unavailable *templates.UnavailableError
		if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "within the import's wait") {
			t.Fatalf("%v", err)
		}
	// Test-only bound: far above the wait, so a passing run never waits it.
	case <-time.After(10 * time.Second):
		t.Fatal("the import still waits for a template service that never answers")
	}
	if done, _ := r.Data.Imported(ctx, ImportID); done {
		t.Fatal("a failed import was marked done")
	}
}

// comingUp is the template service of world, unreachable for its first
// calls, as one that starts during the wait.
type comingUp struct {
	*fakeService
	down int
}

func (c *comingUp) ReadAddress(ctx context.Context, address string) (*templates.Address, error) {
	if c.down > 0 {
		c.down--
		return nil, &templates.UnavailableError{Err: errors.New("starting")}
	}
	return c.fakeService.ReadAddress(ctx, address)
}

// A template service that comes up during the wait is asked again, and the
// import imports.
func TestATemplateServiceThatComesUpDuringTheWaitIsAskedAgain(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	f.read[kit+"@1.0.0"] = templates.Address{Source: kit, Reference: ref("1.0.0"), Base: kit}
	service := &comingUp{fakeService: f, down: 1}
	if err := Import(ctx, ImportOptions{Data: r.Data, Root: r.Root, Wait: 10 * time.Second, Templates: service,
		Entries: []Entry{{ID: "kit", Name: "Kit", Address: kit + "@1.0.0"}}}); err != nil {
		t.Fatal(err)
	}
	if tpl, err := r.Data.Template(ctx, "kit"); err != nil || tpl.CurrentVersion != "1.0.0" {
		t.Fatalf("%+v %v", tpl, err)
	}
	if service.down != 0 {
		t.Fatal("the import never asked the template service")
	}
}

// When one source has entries at v1.2.0 and 1.2.0, the plain tag is the
// version's, whichever entry comes first.
func TestAnImportPrefersThePlainTagOfAVersion(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	f.read[kit+"@v1.2.0"] = templates.Address{Source: kit, Reference: ref("v1.2.0"), Base: kit}
	f.read[kit+"@1.2.0"] = templates.Address{Source: kit, Reference: ref("1.2.0"), Base: kit}
	if err := Import(ctx, ImportOptions{Data: r.Data, Templates: f, Root: r.Root, Entries: []Entry{
		{ID: "prefixed", Name: "Prefixed", Address: kit + "@v1.2.0"},
		{ID: "plain", Name: "Plain", Address: kit + "@1.2.0"},
	}}); err != nil {
		t.Fatal(err)
	}
	list, err := r.Data.Templates(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "plain" || list[0].CurrentTag != "1.2.0" {
		t.Fatalf("%+v %v", list, err)
	}
}

// Every ordering of entries at 0.0.0, v0.0.0, 1.0.0 and v1.0.0, with one
// that names no version, imports the newest version at its plain tag, and
// none fails: 0.0.0 is a version like any other, not a zero to compare
// with before any entry was taken.
func TestAnImportTakesTheNewestVersionAtItsPlainTagInAnyOrder(t *testing.T) {
	refs := []string{"0.0.0", "v0.0.0", "1.0.0", "v1.0.0", "main"}
	var orderings func(prefix, rest []string)
	var all [][]string
	orderings = func(prefix, rest []string) {
		if len(rest) == 0 {
			all = append(all, prefix)
			return
		}
		for i := range rest {
			next := append(slices.Clone(rest[:i]), rest[i+1:]...)
			orderings(append(slices.Clone(prefix), rest[i]), next)
		}
	}
	// Every subset that holds a version, in every order.
	for mask := 1; mask < 1<<len(refs); mask++ {
		var chosen []string
		for i, ref := range refs {
			if mask&(1<<i) != 0 {
				chosen = append(chosen, ref)
			}
		}
		orderings(nil, chosen)
	}
	ctx := context.Background()
	for _, order := range all {
		r, f, _ := world(t)
		var entries []Entry
		for _, tag := range order {
			address := kit + "@" + tag
			f.read[address] = templates.Address{Source: kit, Reference: ref(tag), Base: kit}
			entries = append(entries, Entry{ID: "kit-" + strings.ReplaceAll(tag, ".", "-"), Name: "Kit", Address: address})
		}
		if err := Import(ctx, ImportOptions{Data: r.Data, Templates: f, Root: r.Root, Entries: entries}); err != nil {
			t.Fatalf("%v: %v", order, err)
		}
		list, err := r.Data.Templates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		for _, tag := range []string{"1.0.0", "v1.0.0", "0.0.0", "v0.0.0"} {
			if slices.Contains(order, tag) {
				want = tag
				break
			}
		}
		if want == "" {
			if len(list) != 0 {
				t.Fatalf("%v imported %+v", order, list)
			}
			continue
		}
		if len(list) != 1 || list[0].CurrentTag != want {
			t.Fatalf("%v: imported %+v, want tag %s", order, list, want)
		}
	}
}
