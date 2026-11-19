package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// registered registers the kit at its newest version, 1.2.0, or, with
// ref, at that tag.
func registered(t *testing.T, r *Registry, ref string) store.Template {
	t.Helper()
	address := kit
	if ref != "" {
		address += "@" + ref
	}
	tpl, err := r.Register(context.Background(), "Sample kit", address, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}

// released adds a version to the kit repository, as a new tag does.
func (f *fakeService) released(version, tag, commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions[kit] = append(f.versions[kit], templates.Version{Version: version, Tag: tag, Commit: commit})
	f.checked[kit+"@"+tag] = templates.Checked{Source: kit, Commit: commit, Definition: true}
}

// moved puts the kit's tag at another commit, as a force-pushed tag does.
func (f *fakeService) moved(tag, commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range f.versions[kit] {
		if v.Tag == tag {
			f.versions[kit][i].Commit = commit
		}
	}
	f.checked[kit+"@"+tag] = templates.Checked{Source: kit, Commit: commit, Definition: true}
}

func (f *fakeService) refusing(route string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse[route] = err
}

func (f *fakeService) called(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == route {
			n++
		}
	}
	return n
}

// A check replaces the found versions with what it finds and records its
// time with no failure; a newer version found is the newest, and only a
// found version newer than the current one is taken.
func TestACheckFindsANewVersionThatTheTemplateCanTake(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	registered(t, r, "")
	f.released("1.3.0", "1.3.0", "c130000000000000000000000000000000000000")
	later := testNow.Add(time.Hour)
	r.Now = func() time.Time { return later }
	tpl, err := r.CheckVersions(ctx, "sample-kit")
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(kitVersions), store.Version{Version: "1.3.0", Tag: "1.3.0", Commit: "c130000000000000000000000000000000000000"})
	if !reflect.DeepEqual(tpl.Versions, want) || tpl.Newest() != "1.3.0" || tpl.CurrentVersion != "1.2.0" ||
		!tpl.CheckedAt.Equal(later) || tpl.CheckFailure != "" || tpl.CurrentMoved() {
		t.Fatalf("%+v", tpl)
	}
	// What the check recorded is in the data.
	if stored, err := r.Data.Template(ctx, "sample-kit"); err != nil || !reflect.DeepEqual(stored, tpl) {
		t.Fatalf("%+v %v", stored, err)
	}
	upgraded, err := r.Upgrade(ctx, "sample-kit", "1.3.0", false)
	if err != nil || upgraded.CurrentVersion != "1.3.0" || upgraded.CurrentTag != "1.3.0" || upgraded.CurrentCommit != "c130000000000000000000000000000000000000" {
		t.Fatalf("%+v %v", upgraded, err)
	}
}

// A version the template's checks have not found is refused before any
// check, even when the repository has it: the upgrade takes found versions
// only.
func TestAnUpgradeTakesOnlyAFoundVersion(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	registered(t, r, "v1.1.0")
	f.released("1.3.0", "1.3.0", "c130000000000000000000000000000000000000")
	for _, version := range []string{"1.3.0", "1.1.0", "1.0.0", "0.9.0", "v1.2.0", ""} {
		if _, err := r.Upgrade(ctx, "sample-kit", version, false); !errors.Is(err, ErrNotNewer) {
			t.Fatalf("%q: %v", version, err)
		}
	}
	if n := f.called("check " + kit + "@1.3.0"); n != 0 {
		t.Fatalf("an unfound version was checked %d times", n)
	}
	if tpl, err := r.Upgrade(ctx, "sample-kit", "1.2.0", false); err != nil || tpl.CurrentVersion != "1.2.0" {
		t.Fatalf("%+v %v", tpl, err)
	}
}

// A failed check records its time and reason, the refusal kinds of a
// listing as they are and every other failure as
// template-service-unavailable, and keeps the versions found before; a
// later check that succeeds clears the failure.
func TestAFailedCheckKeepsTheFoundVersionsAndRecordsWhy(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	registered(t, r, "")
	route := "versions " + kit
	for i, tc := range []struct {
		err  error
		want string
	}{
		{&templates.UnavailableError{Err: errors.New("dial unix: no such file")}, store.CheckServiceUnavailable},
		{&templates.Error{Status: 500, Kind: "internal-error", Message: "https://user@git.example.invalid/x"}, store.CheckServiceUnavailable},
		{&templates.Error{Status: 503, Kind: "template-not-found"}, store.CheckServiceUnavailable},
		{&templates.Error{Status: 422, Kind: "template-not-found"}, store.CheckTemplateNotFound},
		{&templates.Error{Status: 422, Kind: "template-resolve-failed"}, store.CheckTemplateResolve},
		{&templates.Error{Status: 400, Kind: "invalid-address"}, store.CheckInvalidAddress},
		{&templates.Error{Status: 400, Kind: "malformed-request"}, store.CheckServiceUnavailable},
		{context.DeadlineExceeded, store.CheckServiceUnavailable},
	} {
		f.refusing(route, tc.err)
		at := testNow.Add(time.Duration(i+1) * time.Minute)
		r.Now = func() time.Time { return at }
		tpl, err := r.CheckVersions(ctx, "sample-kit")
		if err != nil {
			t.Fatalf("%v: %v", tc.err, err)
		}
		if tpl.CheckFailure != tc.want || !tpl.CheckedAt.Equal(at) || !reflect.DeepEqual(tpl.Versions, kitVersions) {
			t.Fatalf("%v: %+v", tc.err, tpl)
		}
	}
	f.refusing(route, nil)
	if tpl, err := r.CheckVersions(ctx, "sample-kit"); err != nil || tpl.CheckFailure != "" {
		t.Fatalf("%+v %v", tpl, err)
	}
	if _, err := r.CheckVersions(ctx, "absent"); !errors.Is(err, store.ErrNoTemplate) {
		t.Fatalf("%v", err)
	}
}

// A template service that takes the listing and never answers fails the
// check within its wait, recorded as template-service-unavailable.
func TestACheckThatGetsNoAnswerFailsWithinItsWait(t *testing.T) {
	r, _, _ := world(t)
	registered(t, r, "")
	r.Templates = stalled{r.Templates}
	r.CheckWait = 50 * time.Millisecond
	// The test's own deadline: a check its wait does not end fails here,
	// by name, not at the test binary's limit.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tpl, err := r.CheckVersions(ctx, "sample-kit")
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the check did not end within its wait of 50ms: it was still running after 5s")
	}
	if err != nil || tpl.CheckFailure != store.CheckServiceUnavailable {
		t.Fatalf("%+v %v", tpl, err)
	}
}

type stalled struct{ TemplateService }

func (stalled) ListVersions(ctx context.Context, _ string) (*templates.Versions, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A check that finds the current version's tag at another commit shows the
// current version as moved; the template's upgrade to that version again
// runs its checks and records the new commit, and only while it has moved.
func TestAMovedTagIsTakenAgainOnlyByUpgradingToIt(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	registered(t, r, "")
	checks := f.called("check " + kit + "@1.2.0")
	if _, err := r.Upgrade(ctx, "sample-kit", "1.2.0", false); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("the current version was taken again while its tag had not moved: %v", err)
	}
	if f.called("check "+kit+"@1.2.0") != checks {
		t.Fatal("the current version was checked again while its tag had not moved")
	}
	f.moved("1.2.0", "c12b000000000000000000000000000000000000")
	tpl, err := r.CheckVersions(ctx, "sample-kit")
	if err != nil || !tpl.CurrentMoved() || tpl.CurrentCommit != "c120000000000000000000000000000000000000" {
		t.Fatalf("%+v %v", tpl, err)
	}
	if checked, err := r.Upgrade(ctx, "sample-kit", "1.2.0", true); err != nil || checked.CurrentCommit != "c12b000000000000000000000000000000000000" || checked.CurrentMoved() {
		t.Fatalf("check: %+v %v", checked, err)
	}
	if stored, _ := r.Data.Template(ctx, "sample-kit"); stored.CurrentCommit != "c120000000000000000000000000000000000000" || !stored.CurrentMoved() {
		t.Fatalf("a check alone recorded the commit: %+v", stored)
	}
	before := f.called("check " + kit + "@1.2.0")
	tpl, err = r.Upgrade(ctx, "sample-kit", "1.2.0", false)
	if err != nil || tpl.CurrentVersion != "1.2.0" || tpl.CurrentCommit != "c12b000000000000000000000000000000000000" || tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
	if f.called("check "+kit+"@1.2.0") != before+1 {
		t.Fatal("the version was not checked again before its commit was recorded")
	}
	if _, err := r.Upgrade(ctx, "sample-kit", "1.2.0", false); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("the current version was taken again once its tag was recorded: %v", err)
	}
}

// An older version found at the current version's moved tag is no way back:
// only a newer found version, or the current one, is taken.
func TestAMovedTagTakesNoOlderVersion(t *testing.T) {
	r, f, _ := world(t)
	ctx := context.Background()
	registered(t, r, "")
	f.moved("1.2.0", "c12b000000000000000000000000000000000000")
	if _, err := r.CheckVersions(ctx, "sample-kit"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.1.0", "1.0.0"} {
		checks := f.called("check " + kit + "@" + map[string]string{"1.1.0": "v1.1.0", "1.0.0": "1.0.0"}[version])
		if _, err := r.Upgrade(ctx, "sample-kit", version, false); !errors.Is(err, ErrNotNewer) {
			t.Fatalf("%s: %v", version, err)
		}
		if f.called("check "+kit+"@"+map[string]string{"1.1.0": "v1.1.0", "1.0.0": "1.0.0"}[version]) != checks {
			t.Fatalf("%s: an older version was checked while the current tag had moved", version)
		}
	}
}

// An imported template's current commit is unknown until its first check,
// which takes it, and the tag, from the found version that is its current
// version.
func TestTheFirstCheckOfAnImportedTemplateRecordsItsCommit(t *testing.T) {
	r, _, _ := world(t)
	ctx := context.Background()
	if _, err := r.Data.RegisterTemplate(ctx, store.Template{ID: "kit", Name: "Kit", Address: kit, Source: kit,
		CurrentVersion: "1.1.0", CurrentTag: "1.1.0"}, nil); err != nil {
		t.Fatal(err)
	}
	tpl, err := r.CheckVersions(ctx, "kit")
	if err != nil || tpl.CurrentCommit != "c110000000000000000000000000000000000000" || tpl.CurrentTag != "v1.1.0" || tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
}

// A registration joins each workspace by the one rule: the version whose
// tag is at the record's commit, the ref's when that is a version; any
// other record leaves the version unknown with its commit, as a ref naming
// a version no tag gives at that commit does, until a check finds a tag at
// that commit.
func TestARegistrationAndACheckGiveTheVersionAtTheRecordsCommit(t *testing.T) {
	r, f, rootDir := world(t)
	ctx := context.Background()
	folders(t, r, rootDir, map[string]string{
		"alpha": toha(kit, "main", at110),
		"beta":  toha(kit, "main", "c130000000000000000000000000000000000000"),
		"gamma": toha(kit, "v1.0.0", untaged),
		// kit@8.0.0 at 1.0.0's commit.
		"eight": toha(kit, "8.0.0", at100),
		// A found tag at another commit.
		"moved": toha(kit, "1.2.0", at110),
		"other": toha("gh:example-org/other", "1.0.0", at100),
	})
	registered(t, r, "")
	want := map[string]store.AppliedVersion{
		"alpha": {Version: "1.1.0", Commit: at110},
		"beta":  {Commit: "c130000000000000000000000000000000000000"},
		"gamma": {Commit: untaged},
		"eight": {Commit: at100},
		"moved": {Commit: at110},
		"other": {},
	}
	if got, err := r.Data.AppliedVersions(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	f.released("1.3.0", "1.3.0", "c130000000000000000000000000000000000000")
	if _, err := r.CheckVersions(ctx, "sample-kit"); err != nil {
		t.Fatal(err)
	}
	want["beta"] = store.AppliedVersion{Version: "1.3.0", Commit: "c130000000000000000000000000000000000000"}
	want["eight"] = store.AppliedVersion{Version: "1.0.0", Commit: at100}
	want["moved"] = store.AppliedVersion{Version: "1.1.0", Commit: at110}
	if got, err := r.Data.AppliedVersions(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
}

// One rule gives the version of a record: of the template's source, at a
// full commit id, the found version whose tag is at that commit, the ref's
// when the ref is a version, else the newest of the tags there.
func TestOneRuleGivesTheVersionOfARecord(t *testing.T) {
	a, b, c := "a000000000000000000000000000000000000000", "b000000000000000000000000000000000000000", "c000000000000000000000000000000000000000"
	sha256 := strings.Repeat("ab", 32)
	tpl := store.Template{ID: "kit", Source: kit, Versions: []store.Version{
		{Version: "1.0.0", Tag: "1.0.0", Commit: a}, {Version: "1.0.1", Tag: "1.0.1", Commit: a},
		{Version: "1.1.0", Tag: "v1.1.0", Commit: b}, {Version: "2.0.0", Tag: "2.0.0", Commit: sha256},
	}}
	for _, tc := range []struct {
		name, source, ref, commit string
		version                   string
		// join is what a join takes when the rule gives no version.
		join  store.AppliedVersion
		names []string
	}{
		{name: "the ref's version", ref: "1.0.0", commit: a, version: "1.0.0"},
		{name: "the other tag at that commit", ref: "1.0.1", commit: a, version: "1.0.1"},
		{name: "a branch takes the newest tag there", ref: "main", commit: a, version: "1.0.1"},
		{name: "a v-prefixed tag", ref: "v1.1.0", commit: b, version: "1.1.0"},
		{name: "a plain ref of a v-prefixed tag", ref: "1.1.0", commit: b, version: "1.1.0"},
		{name: "a SHA-256 commit", ref: "2.0.0", commit: sha256, version: "2.0.0"},
		{name: "a version no tag gives at that commit", ref: "8.0.0", commit: a,
			join: store.AppliedVersion{Commit: a}, names: []string{`"8.0.0"`, "version 8.0.0", "gives 1.0.1"}},
		{name: "a found tag at another commit", ref: "1.1.0", commit: a,
			join: store.AppliedVersion{Commit: a}, names: []string{`"1.1.0"`, a}},
		{name: "a branch at an untagged commit", ref: "main", commit: c,
			join: store.AppliedVersion{Commit: c}, names: []string{"no version tag", c, `"main"`}},
		{name: "a version at an untagged commit", ref: "v9.9.9", commit: c,
			join: store.AppliedVersion{Commit: c}, names: []string{"no version tag", c}},
		{name: "another source", source: "gh:example-org/other", ref: "1.0.0", commit: a,
			names: []string{"another template"}},
		{name: "no commit", ref: "1.0.0", commit: "", names: []string{`""`, "full Git object id"}},
		{name: "a short commit", ref: "1.0.0", commit: a[:12], names: []string{"full Git object id"}},
		{name: "an upper-case commit", ref: "1.0.0", commit: strings.ToUpper(a), names: []string{"full Git object id"}},
		{name: "a commit holding a line", ref: "1.0.0", commit: "not-a-commit\nfake console line", names: []string{`"not-a-commit\nfake console line"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := kit
			if tc.source != "" {
				source = tc.source
			}
			rec := Applied{Source: source, Ref: tc.ref, Commit: tc.commit}
			found, err := RecordVersion(rec, tpl)
			joined := JoinVersion(rec, tpl)
			if tc.version != "" {
				if err != nil || found.Version != tc.version || found.Commit != tc.commit {
					t.Fatalf("the rule gave %+v %v, want version %s at the record's commit", found, err, tc.version)
				}
				if joined != (store.AppliedVersion{Version: tc.version, Commit: tc.commit}) {
					t.Fatalf("a join took %+v, want version %s at the record's commit", joined, tc.version)
				}
				return
			}
			var refused *VersionError
			if !errors.As(err, &refused) {
				t.Fatalf("the rule gave %+v %v, want no version", found, err)
			}
			for _, named := range tc.names {
				if !strings.Contains(err.Error(), named) {
					t.Errorf("the refusal does not name %s: %v", named, err)
				}
			}
			// The ref and commit are quoted: no value writes a line.
			if strings.ContainsRune(err.Error(), '\n') {
				t.Errorf("the refusal holds a line break: %q", err.Error())
			}
			if joined != tc.join {
				t.Fatalf("join %+v, want %+v", joined, tc.join)
			}
		})
	}
}

// heldService holds each listing until released, counting them.
type heldService struct {
	TemplateService
	listings atomic.Int32
	started  chan struct{}
	release  chan struct{}
}

func (h *heldService) ListVersions(ctx context.Context, address string) (*templates.Versions, error) {
	h.listings.Add(1)
	h.started <- struct{}{}
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return h.TemplateService.ListVersions(ctx, address)
}

// Two checks of one template at once share one run, and a caller that
// leaves does not end it for the other: the first caller's listing is
// held until the second has joined its run and the first has left.
func TestChecksOfOneTemplateAtOnceShareOneRun(t *testing.T) {
	r, f, _ := world(t)
	registered(t, r, "")
	f.released("1.3.0", "1.3.0", "c130000000000000000000000000000000000000")
	held := &heldService{TemplateService: r.Templates, started: make(chan struct{}, 2), release: make(chan struct{})}
	r.Templates = held
	var release sync.Once
	free := func() { release.Do(func() { close(held.release) }) }
	t.Cleanup(free)
	joined := make(chan struct{}, 1)
	r.shared = func(string) { joined <- struct{}{} }
	const within = 10 * time.Second
	await := func(c <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case <-time.After(within):
			t.Fatalf("%s within %s", what, within)
		}
	}
	leaving, leave := context.WithCancel(context.Background())
	results := make(chan error, 2)
	go func() { _, err := r.CheckVersions(leaving, "sample-kit"); results <- err }()
	await(held.started, "the first check started no listing")
	var second store.Template
	go func() {
		var err error
		second, err = r.CheckVersions(context.Background(), "sample-kit")
		results <- err
	}()
	await(joined, "the second check did not join the first one's run")
	leave()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatalf("the leaving caller got %v, want context.Canceled", err)
	}
	free()
	if err := <-results; err != nil {
		t.Fatalf("the staying caller: %v", err)
	}
	if n := held.listings.Load(); n != 1 || second.Newest() != "1.3.0" || second.CheckFailure != "" {
		t.Fatalf("the staying caller got newest %q, failure %q, from %d listings; want 1.3.0 found by the one listing the leaving caller started", second.Newest(), second.CheckFailure, n)
	}
}

// The schedule checks every template when it starts and again at each
// tick, so a version released while Menagerie runs is found with no
// restart; a failed check is logged by the template's id and kind.
func TestTheScheduleChecksEveryTemplateAtStartAndEachTick(t *testing.T) {
	r, f, _ := world(t)
	ctx, cancel := context.WithCancel(context.Background())
	registered(t, r, "")
	var logged atomic.Value
	logged.Store("")
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Schedule(ctx, ticks, func(format string, args ...any) {
			logged.Store(logged.Load().(string) + fmt.Sprintf(format, args...) + "\n")
		})
	}()
	newest := func() string {
		tpl, err := r.Data.Template(context.Background(), "sample-kit")
		if err != nil {
			t.Fatal(err)
		}
		return tpl.Newest()
	}
	f.released("1.3.0", "1.3.0", "c130000000000000000000000000000000000000")
	// The check at start finds it.
	waitFor(t, "the check at start to find 1.3.0", func() bool { return newest() == "1.3.0" })
	f.released("1.4.0", "1.4.0", "c140000000000000000000000000000000000000")
	ticks <- testNow
	waitFor(t, "the check at the tick to find 1.4.0", func() bool { return newest() == "1.4.0" })
	f.refusing("versions "+kit, &templates.Error{Status: 422, Kind: "template-not-found", Message: "https://git.example.invalid/secret"})
	ticks <- testNow
	waitFor(t, "the failed check to be logged", func() bool { return logged.Load().(string) != "" })
	cancel()
	<-done
	text := logged.Load().(string)
	if !strings.Contains(text, "sample-kit") || !strings.Contains(text, "template-not-found") || strings.Contains(text, "example.invalid") {
		t.Fatalf("%s", text)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// staleCheck points the fetch of a tag at commit while the listing keeps
// the commit it had, as a tag moved after the versions were listed.
func (f *fakeService) staleCheck(tag, commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked[kit+"@"+tag] = templates.Checked{Source: kit, Commit: commit, Definition: true}
}

// A template takes a version only at the commit its listing found: a tag
// moved between the listing and the fetch refuses the registration, the
// upgrade and Update version under version, a check alone or not, storing
// nothing; after a check lists the new commit, the upgrade takes it.
func TestATemplateTakesAVersionOnlyAtTheCommitItsListingFound(t *testing.T) {
	ctx := context.Background()
	moved := "c12b000000000000000000000000000000000000"
	again := "c12c000000000000000000000000000000000000"
	refusedVersion := func(t *testing.T, err error, from, to string) {
		t.Helper()
		check := checkOf(t, err)
		if check.Check != CheckVersion || !strings.Contains(check.Message, "moved") || !strings.Contains(check.Message, from) || !strings.Contains(check.Message, to) {
			t.Fatalf("%+v", check)
		}
	}
	t.Run("registration", func(t *testing.T) {
		r, f, _ := world(t)
		f.staleCheck("1.2.0", moved)
		for _, checkOnly := range []bool{true, false} {
			_, err := r.Register(ctx, "Sample kit", kit, "", checkOnly)
			refusedVersion(t, err, at120, moved)
		}
		if list, _ := r.Data.Templates(ctx); len(list) != 0 {
			t.Fatalf("stored %+v", list)
		}
	})
	t.Run("upgrade", func(t *testing.T) {
		r, f, _ := world(t)
		registered(t, r, "v1.1.0")
		f.staleCheck("1.2.0", moved)
		for _, checkOnly := range []bool{true, false} {
			_, err := r.Upgrade(ctx, "sample-kit", "1.2.0", checkOnly)
			refusedVersion(t, err, at120, moved)
		}
		if tpl, _ := r.Data.Template(ctx, "sample-kit"); tpl.CurrentVersion != "1.1.0" {
			t.Fatalf("stored %+v", tpl)
		}
		f.moved("1.2.0", moved)
		if _, err := r.CheckVersions(ctx, "sample-kit"); err != nil {
			t.Fatal(err)
		}
		if tpl, err := r.Upgrade(ctx, "sample-kit", "1.2.0", false); err != nil || tpl.CurrentCommit != moved || tpl.CurrentMoved() {
			t.Fatalf("%+v %v", tpl, err)
		}
	})
	t.Run("update version", func(t *testing.T) {
		r, f, _ := world(t)
		registered(t, r, "")
		f.moved("1.2.0", moved)
		if tpl, err := r.CheckVersions(ctx, "sample-kit"); err != nil || !tpl.CurrentMoved() {
			t.Fatalf("%+v %v", tpl, err)
		}
		f.staleCheck("1.2.0", again)
		for _, checkOnly := range []bool{true, false} {
			_, err := r.Upgrade(ctx, "sample-kit", "1.2.0", checkOnly)
			refusedVersion(t, err, moved, again)
		}
		if tpl, _ := r.Data.Template(ctx, "sample-kit"); tpl.CurrentCommit != at120 || !tpl.CurrentMoved() {
			t.Fatalf("stored %+v", tpl)
		}
		f.moved("1.2.0", again)
		if _, err := r.CheckVersions(ctx, "sample-kit"); err != nil {
			t.Fatal(err)
		}
		if tpl, err := r.Upgrade(ctx, "sample-kit", "1.2.0", false); err != nil || tpl.CurrentCommit != again || tpl.CurrentMoved() {
			t.Fatalf("%+v %v", tpl, err)
		}
	})
}

// A template service whose listing names a version with no commit is no
// answer: the check records template-service-unavailable, with its time,
// and keeps the versions found before.
func TestAMalformedListingIsRecordedAsUnavailable(t *testing.T) {
	r, _, _ := world(t)
	registered(t, r, "")
	socket := filepath.Join(t.TempDir(), "template.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"source":"`+kit+`","versions":[{"version":"1.3.0","tag":"1.3.0","commit":""}]}`)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	r.Templates = templates.New(socket)
	later := testNow.Add(time.Hour)
	r.Now = func() time.Time { return later }
	tpl, err := r.CheckVersions(context.Background(), "sample-kit")
	if err != nil {
		t.Fatalf("the check failed instead of recording the failure: %v", err)
	}
	if tpl.CheckFailure != store.CheckServiceUnavailable || !tpl.CheckedAt.Equal(later) || !reflect.DeepEqual(tpl.Versions, kitVersions) {
		t.Fatalf("%+v", tpl)
	}
}
