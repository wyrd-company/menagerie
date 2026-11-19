package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// fakeService plays the template service: each address's reading, its
// repository's versions and each version's check, or a refusal.
type fakeService struct {
	mu       sync.Mutex
	read     map[string]templates.Address
	versions map[string][]templates.Version
	checked  map[string]templates.Checked
	refuse   map[string]error
	calls    []string
}

func (f *fakeService) fail(route string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, route)
	return f.refuse[route]
}

func (f *fakeService) ReadAddress(_ context.Context, address string) (*templates.Address, error) {
	if err := f.fail("address " + address); err != nil {
		return nil, err
	}
	read, ok := f.read[address]
	if !ok {
		return nil, &templates.Error{Status: 400, Kind: "invalid-address", Message: "the address is not a remote Git address"}
	}
	return &read, nil
}

func (f *fakeService) ListVersions(_ context.Context, address string) (*templates.Versions, error) {
	if err := f.fail("versions " + address); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &templates.Versions{Source: f.read[address].Source, Versions: slices.Clone(f.versions[address])}, nil
}

func (f *fakeService) Check(_ context.Context, address, ref string) (*templates.Checked, error) {
	if err := f.fail("check " + address + "@" + ref); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	checked, ok := f.checked[address+"@"+ref]
	if !ok {
		return nil, &templates.Error{Status: 422, Kind: "template-resolve-failed", Message: "no such ref"}
	}
	return &checked, nil
}

const kit = "https://git.example.invalid/example-org/kit"

// testNow is the clock of the registry's version checks in tests.
var testNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// kitVersions are the kit repository's versions, as a check finds them.
var kitVersions = []store.Version{
	{Version: "1.0.0", Tag: "1.0.0", Commit: "c100000000000000000000000000000000000000"},
	{Version: "1.1.0", Tag: "v1.1.0", Commit: "c110000000000000000000000000000000000000"},
	{Version: "1.2.0", Tag: "1.2.0", Commit: "c120000000000000000000000000000000000000"},
}

func ref(s string) *string { return &s }

func world(t *testing.T) (*Registry, *fakeService, string) {
	t.Helper()
	dir := t.TempDir()
	data, err := store.Open(filepath.Join(dir, "state", "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	rootDir := filepath.Join(dir, "workspaces")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeService{
		read: map[string]templates.Address{
			kit:             {Source: kit, Base: kit},
			kit + "@v1.1.0": {Source: kit, Reference: ref("v1.1.0"), Base: kit},
			kit + "@main":   {Source: kit, Reference: ref("main"), Base: kit},
		},
		versions: map[string][]templates.Version{kit: {
			{Version: "1.0.0", Tag: "1.0.0", Commit: "c100000000000000000000000000000000000000"},
			{Version: "1.1.0", Tag: "v1.1.0", Commit: "c110000000000000000000000000000000000000"},
			{Version: "1.2.0", Tag: "1.2.0", Commit: "c120000000000000000000000000000000000000"},
		}},
		checked: map[string]templates.Checked{
			kit + "@1.0.0":  {Source: kit, Commit: "c100000000000000000000000000000000000000", Definition: true},
			kit + "@v1.1.0": {Source: kit, Commit: "c110000000000000000000000000000000000000", Definition: true},
			kit + "@1.2.0":  {Source: kit, Commit: "c120000000000000000000000000000000000000", Definition: true},
		},
		refuse: map[string]error{},
	}
	return &Registry{Data: data, Templates: f, Root: root, Now: func() time.Time { return testNow }}, f, rootDir
}

func checkOf(t *testing.T, err error) *CheckError {
	t.Helper()
	var check *CheckError
	if !errors.As(err, &check) {
		t.Fatalf("not a check refusal: %v", err)
	}
	if strings.Contains(check.Message, "example.invalid") {
		t.Fatalf("the refusal quotes the address: %s", check.Message)
	}
	return check
}

func TestRegistrationTakesTheNewestVersionOrTheAddressTag(t *testing.T) {
	r, _, _ := world(t)
	ctx := context.Background()
	newest, err := r.Register(ctx, "Sample kit", kit, "", false)
	if err != nil {
		t.Fatal(err)
	}
	want := store.Template{ID: "sample-kit", Name: "Sample kit", Address: kit, Source: kit,
		CurrentVersion: "1.2.0", CurrentTag: "1.2.0", CurrentCommit: "c120000000000000000000000000000000000000", Workspaces: []string{},
		Versions: kitVersions, CheckedAt: testNow}
	if !reflect.DeepEqual(newest, want) {
		t.Fatalf("%+v", newest)
	}
	// The same source is one registration.
	var exists *store.TemplateExistsError
	if _, err := r.Register(ctx, "Again", kit+"@v1.1.0", "", false); !errors.As(err, &exists) || exists.ID != "sample-kit" {
		t.Fatalf("%v", err)
	}
	if err := r.Data.RemoveTemplate(ctx, "sample-kit"); err != nil {
		t.Fatal(err)
	}
	tagged, err := r.Register(ctx, "Sample kit", kit+"@v1.1.0", "alpine:3.22", false)
	if err != nil {
		t.Fatal(err)
	}
	if tagged.CurrentVersion != "1.1.0" || tagged.CurrentTag != "v1.1.0" || tagged.CurrentCommit != "c110000000000000000000000000000000000000" || tagged.RunnerImage != "alpine:3.22" || tagged.Address != kit {
		t.Fatalf("%+v", tagged)
	}
}

func TestEachCheckRefusesInOrderAndNamesItself(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		address string
		setup   func(*fakeService)
		check   string
		message string
	}{
		{"address", "/srv/kit", nil, CheckAddress, "not a remote Git address"},
		{"listing refused", kit, func(f *fakeService) {
			f.refuse["versions "+kit] = &templates.Error{Kind: "template-resolve-failed", Message: "https://git.example.invalid refused"}
		}, CheckFetch, "refused or failed"},
		{"repository missing", kit, func(f *fakeService) {
			f.refuse["versions "+kit] = &templates.Error{Kind: "template-not-found", Message: "gone"}
		}, CheckFetch, "no such repository"},
		{"no version", kit, func(f *fakeService) { f.versions[kit] = []templates.Version{} }, CheckVersion, "has no version"},
		{"ref not a version", kit + "@main", nil, CheckVersion, "not a version's tag"},
		{"version not fetched", kit, func(f *fakeService) { delete(f.checked, kit+"@1.2.0") }, CheckFetch, "could not be fetched"},
		{"manifest", kit, func(f *fakeService) {
			f.refuse["check "+kit+"@1.2.0"] = &templates.Error{Kind: "template-invalid", Message: "template.yml"}
		}, CheckRead, "could not be read"},
		{"link", kit, func(f *fakeService) {
			f.refuse["check "+kit+"@1.2.0"] = &templates.Error{Kind: "unsafe-template-link", Message: "link"}
		}, CheckRead, "outside its repository"},
		{"definition", kit, func(f *fakeService) {
			f.checked[kit+"@1.2.0"] = templates.Checked{Source: kit, Commit: "c120000000000000000000000000000000000000"}
		}, CheckDefinition, ".devcontainer/devcontainer.json"},
	} {
		// A check alone refuses exactly as a registration does.
		for _, checkOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/check=%t", c.name, checkOnly), func(t *testing.T) {
				r, f, _ := world(t)
				if c.setup != nil {
					c.setup(f)
				}
				_, err := r.Register(ctx, "Sample kit", c.address, "", checkOnly)
				check := checkOf(t, err)
				if check.Check != c.check || !strings.Contains(check.Message, c.message) {
					t.Fatalf("%s: %q", check.Check, check.Message)
				}
				if templates, _ := r.Data.Templates(ctx); len(templates) != 0 {
					t.Fatalf("a refused template was recorded: %+v", templates)
				}
			})
		}
	}
}

// A check alone runs the registration's checks and answers the template a
// registration would record, its id and joined workspaces included, and
// stores nothing; it refuses a registered source as a registration does.
// A registration after a passing check runs every check again.
func TestACheckAloneStoresNothingAndARegistrationChecksAgain(t *testing.T) {
	r, f, rootDir := world(t)
	ctx := context.Background()
	folder := filepath.Join(rootDir, "alpha")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"applications":[{"target":".","template":"` + kit + `@1.0.0","source":"` + kit + `","commit":null}]}`
	if err := os.WriteFile(filepath.Join(folder, RecordName), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Data.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	checked, err := r.Register(ctx, "Sample kit", kit, "alpine:3.22", true)
	if err != nil {
		t.Fatal(err)
	}
	want := store.Template{ID: "sample-kit", Name: "Sample kit", Address: kit, Source: kit, RunnerImage: "alpine:3.22",
		CurrentVersion: "1.2.0", CurrentTag: "1.2.0", CurrentCommit: "c120000000000000000000000000000000000000", Workspaces: []string{"alpha"},
		Versions: kitVersions, CheckedAt: testNow}
	if !reflect.DeepEqual(checked, want) {
		t.Fatalf("%+v", checked)
	}
	if list, _ := r.Data.Templates(ctx); len(list) != 0 {
		t.Fatalf("a check stored %+v", list)
	}
	if names, _ := r.Data.Untemplated(ctx); !reflect.DeepEqual(names, []string{"alpha"}) {
		t.Fatalf("a check joined a workspace: %v", names)
	}
	// A registration trusts no earlier check: the definition is gone now.
	calls := len(f.calls)
	f.checked[kit+"@1.2.0"] = templates.Checked{Source: kit, Commit: "c120000000000000000000000000000000000000"}
	if check := checkOf(t, errOf(r.Register(ctx, "Sample kit", kit, "alpine:3.22", false))); check.Check != CheckDefinition {
		t.Fatalf("%+v", check)
	}
	for _, route := range []string{"address " + kit, "versions " + kit, "check " + kit + "@1.2.0"} {
		if !slices.Contains(f.calls[calls:], route) {
			t.Errorf("the registration did not ask %s again: %v", route, f.calls[calls:])
		}
	}
	f.checked[kit+"@1.2.0"] = templates.Checked{Source: kit, Commit: "c120000000000000000000000000000000000000", Definition: true}
	if _, err := r.Register(ctx, "Sample kit", kit, "alpine:3.22", false); err != nil {
		t.Fatal(err)
	}
	var exists *store.TemplateExistsError
	if _, err := r.Register(ctx, "Again", kit, "", true); !errors.As(err, &exists) || exists.ID != "sample-kit" {
		t.Fatalf("%v", err)
	}
	// A check names the id a registration would take now.
	other := "https://git.example.invalid/example-org/other"
	f.read[other] = templates.Address{Source: other, Base: other}
	f.versions[other] = f.versions[kit]
	f.checked[other+"@1.2.0"] = templates.Checked{Source: other, Commit: "c120000000000000000000000000000000000000", Definition: true}
	if second, err := r.Register(ctx, "Sample kit", other, "", true); err != nil || second.ID != "sample-kit-2" || len(second.Workspaces) != 0 {
		t.Fatalf("%+v %v", second, err)
	}
	if list, _ := r.Data.Templates(ctx); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
}

func TestAnUnreachableTemplateServiceIsNoCheck(t *testing.T) {
	r, f, _ := world(t)
	f.refuse["address "+kit] = &templates.UnavailableError{Err: errors.New("no socket")}
	var unavailable *templates.UnavailableError
	if _, err := r.Register(context.Background(), "Sample kit", kit, "", false); !errors.As(err, &unavailable) {
		t.Fatalf("%v", err)
	}
}

func TestNamesAndRunnerImagesAreChecked(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	for _, name := range []string{"", "  ", "two\nlines", "tab\there", strings.Repeat("n", 101), "line\u2028separator"} {
		var invalid *InvalidError
		if _, err := r.Register(ctx, name, kit, "", false); !errors.As(err, &invalid) {
			t.Errorf("%q: %v", name, err)
		}
	}
	var invalid *InvalidError
	if _, err := r.Register(ctx, "Sample kit", kit, "Not An Image!", false); !errors.As(err, &invalid) {
		t.Errorf("image: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("an invalid request reached the template service: %v", f.calls)
	}
	if _, err := r.Register(ctx, strings.Repeat("é", 100), kit, "", false); err != nil {
		t.Fatalf("100 characters: %v", err)
	}
}

func TestIDsComeFromTheNameAndStayPermanent(t *testing.T) {
	for name, id := range map[string]string{
		"Sample kit, version 1": "sample-kit-version-1",
		"  --Kit__2  ":          "kit-2",
		"Éclair":                "clair",
		"日本":                    "template",
	} {
		if got := store.TemplateID(name); got != id {
			t.Errorf("%q: %q", name, got)
		}
	}
	r, f, _ := world(t)
	ctx := context.Background()
	other := "https://git.example.invalid/example-org/other"
	f.read[other] = templates.Address{Source: other, Base: other}
	f.versions[other] = f.versions[kit]
	for _, v := range []string{"1.0.0", "v1.1.0", "1.2.0"} {
		f.checked[other+"@"+v] = f.checked[kit+"@"+v]
	}
	first, err := r.Register(ctx, "Kit", kit, "", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Register(ctx, "Kit", other, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "kit" || second.ID != "kit-2" {
		t.Fatalf("%s %s", first.ID, second.ID)
	}
	name := "A renamed kit"
	renamed, err := r.Edit(ctx, "kit", &name, nil)
	if err != nil || renamed.ID != "kit" || renamed.Name != name || renamed.Address != kit {
		t.Fatalf("%+v %v", renamed, err)
	}
	// Listed by name, then by id.
	list, err := r.Data.Templates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "kit" || list[1].ID != "kit-2" {
		t.Fatalf("%+v", list)
	}
	empty := ""
	cleared, err := r.Edit(ctx, "kit-2", nil, &empty)
	if err != nil || cleared.RunnerImage != "" || cleared.Name != "Kit" {
		t.Fatalf("%+v %v", cleared, err)
	}
}

func TestRegistrationJoinsTheWorkspacesOfItsSource(t *testing.T) {
	r, _, rootDir := world(t)
	ctx := context.Background()
	for name, source := range map[string]string{"alpha": kit, "beta": "gh:example-org/elsewhere"} {
		folder := filepath.Join(rootDir, name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		record := `{"applications":[{"target":".","template":"` + source + `@v1","source":"` + source + `","commit":null}]}`
		if err := os.WriteFile(filepath.Join(folder, RecordName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Data.Discover(ctx, []string{"alpha", "beta"}); err != nil {
		t.Fatal(err)
	}
	tpl, err := r.Register(ctx, "Kit", kit, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tpl.Workspaces, []string{"alpha"}) {
		t.Fatalf("%v", tpl.Workspaces)
	}
	var inUse *store.TemplateInUseError
	if err := r.Data.RemoveTemplate(ctx, "kit"); !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.Workspaces, []string{"alpha"}) {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(inUse.Error(), "alpha") {
		t.Fatalf("%s", inUse.Error())
	}
}

func TestATemplateMovesOnlyForwardToACheckedVersion(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "Kit", kit+"@v1.1.0", "", false); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.1.0", "1.0.0", "1.3.0", "v1.2.0", "main"} {
		if _, err := r.Upgrade(ctx, "kit", version, false); !errors.Is(err, ErrNotNewer) {
			t.Errorf("%s: %v", version, err)
		}
	}
	f.checked[kit+"@1.2.0"] = templates.Checked{Source: kit, Commit: "c120000000000000000000000000000000000000"}
	if check := checkOf(t, errOf(r.Upgrade(ctx, "kit", "1.2.0", false))); check.Check != CheckDefinition {
		t.Fatalf("%+v", check)
	}
	if tpl, _ := r.Data.Template(ctx, "kit"); tpl.CurrentVersion != "1.1.0" {
		t.Fatalf("a refused upgrade moved the template: %+v", tpl)
	}
	f.checked[kit+"@1.2.0"] = templates.Checked{Source: kit, Commit: "c120000000000000000000000000000000000000", Definition: true}
	upgraded, err := r.Upgrade(ctx, "kit", "1.2.0", false)
	if err != nil || upgraded.CurrentVersion != "1.2.0" || upgraded.CurrentTag != "1.2.0" || upgraded.CurrentCommit != "c120000000000000000000000000000000000000" {
		t.Fatalf("%+v %v", upgraded, err)
	}
	if _, err := r.Upgrade(ctx, "nothing", "1.2.0", false); !errors.Is(err, store.ErrNoTemplate) {
		t.Fatalf("%v", err)
	}
}

func errOf(_ store.Template, err error) error { return err }

// A check of a template's upgrade refuses as the upgrade does and answers
// the template at that version, storing nothing; the upgrade after it runs
// its checks again.
func TestACheckOfAnUpgradeStoresNothingAndTheUpgradeChecksAgain(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "Kit", kit+"@v1.1.0", "", false); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "1.1.0", "9.9.9"} {
		if _, err := r.Upgrade(ctx, "kit", version, true); !errors.Is(err, ErrNotNewer) {
			t.Fatalf("%s: %v", version, err)
		}
	}
	if _, err := r.Upgrade(ctx, "nothing", "1.2.0", true); !errors.Is(err, store.ErrNoTemplate) {
		t.Fatalf("%v", err)
	}
	checked, err := r.Upgrade(ctx, "kit", "1.2.0", true)
	if err != nil || checked.CurrentVersion != "1.2.0" || checked.CurrentTag != "1.2.0" || checked.CurrentCommit != "c120000000000000000000000000000000000000" {
		t.Fatalf("%+v %v", checked, err)
	}
	if tpl, _ := r.Data.Template(ctx, "kit"); tpl.CurrentVersion != "1.1.0" || tpl.CurrentCommit != "c110000000000000000000000000000000000000" {
		t.Fatalf("a check stored %+v", tpl)
	}
	calls := len(f.calls)
	f.checked[kit+"@1.2.0"] = templates.Checked{Source: kit, Commit: "c120000000000000000000000000000000000000"}
	for _, checkOnly := range []bool{true, false} {
		if check := checkOf(t, errOf(r.Upgrade(ctx, "kit", "1.2.0", checkOnly))); check.Check != CheckDefinition {
			t.Fatalf("check=%t: %+v", checkOnly, check)
		}
	}
	if n := len(slices.DeleteFunc(slices.Clone(f.calls[calls:]), func(c string) bool { return c != "check "+kit+"@1.2.0" })); n != 2 {
		t.Fatalf("the version was checked %d times: %v", n, f.calls[calls:])
	}
	if tpl, _ := r.Data.Template(ctx, "kit"); tpl.CurrentVersion != "1.1.0" {
		t.Fatalf("%+v", tpl)
	}
}

// A fault of the template service, a 5xx reply, is no failed check: the
// registration, its check alone and a template's upgrade answer it as an
// unreachable service, naming the status and kind, not the reply's message.
func TestATemplateServiceFaultIsNoFailedCheck(t *testing.T) {
	ctx := context.Background()
	for _, checkOnly := range []bool{false, true} {
		for _, route := range []string{"versions " + kit, "check " + kit + "@1.2.0"} {
			r, f, _ := world(t)
			f.refuse[route] = &templates.Error{Status: 500, Kind: "io-failed", Message: "reading https://sample-user:sample-password@git.example.invalid failed"}
			var unavailable *templates.UnavailableError
			_, err := r.Register(ctx, "Sample kit", kit, "", checkOnly)
			if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "500 io-failed") || strings.Contains(err.Error(), "sample-password") {
				t.Fatalf("%s check=%t: %v", route, checkOnly, err)
			}
		}
		r, f, _ := world(t)
		if _, err := r.Register(ctx, "Kit", kit+"@v1.1.0", "", false); err != nil {
			t.Fatal(err)
		}
		f.refuse["check "+kit+"@1.2.0"] = &templates.Error{Status: 500, Kind: "internal-error"}
		var unavailable *templates.UnavailableError
		if _, err := r.Upgrade(ctx, "kit", "1.2.0", checkOnly); !errors.As(err, &unavailable) {
			t.Fatalf("upgrade check=%t: %v", checkOnly, err)
		}
	}
}

// A registered source is refused before any fetch, so a repository that
// cannot be listed now still gives template-exists.
func TestARegisteredSourceIsRefusedBeforeAnyFetch(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "Sample kit", kit, "", false); err != nil {
		t.Fatal(err)
	}
	f.refuse["versions "+kit] = &templates.Error{Kind: "template-resolve-failed", Message: "refused"}
	calls := len(f.calls)
	var exists *store.TemplateExistsError
	if _, err := r.Register(ctx, "Again", kit, "", false); !errors.As(err, &exists) {
		t.Fatalf("%v", err)
	}
	if slices.Contains(f.calls[calls:], "versions "+kit) {
		t.Fatalf("a registered source was fetched: %v", f.calls[calls:])
	}
}

// A template upgrade refuses a version that is not newer, or that the
// repository does not list, before it checks or fetches it.
func TestAnUpgradeRefusesAnOlderVersionBeforeAnyCheck(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	if _, err := r.Register(ctx, "Kit", kit+"@v1.1.0", "", false); err != nil {
		t.Fatal(err)
	}
	for _, checkOnly := range []bool{false, true} {
		for _, version := range []string{"1.0.0", "1.1.0", "9.9.9"} {
			calls := len(f.calls)
			if _, err := r.Upgrade(ctx, "kit", version, checkOnly); !errors.Is(err, ErrNotNewer) {
				t.Fatalf("%s: %v", version, err)
			}
			for _, call := range f.calls[calls:] {
				if strings.HasPrefix(call, "check ") {
					t.Fatalf("%s: the upgrade checked a version it refuses: %v", version, f.calls[calls:])
				}
			}
		}
	}
}
