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
	f.released("1.3.0", "1.3.0", "c130")
	later := testNow.Add(time.Hour)
	r.Now = func() time.Time { return later }
	tpl, err := r.CheckVersions(ctx, "sample-kit")
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(kitVersions), store.Version{Version: "1.3.0", Tag: "1.3.0", Commit: "c130"})
	if !reflect.DeepEqual(tpl.Versions, want) || tpl.Newest() != "1.3.0" || tpl.CurrentVersion != "1.2.0" ||
		!tpl.CheckedAt.Equal(later) || tpl.CheckFailure != "" || tpl.CurrentMoved() {
		t.Fatalf("%+v", tpl)
	}
	// What the check recorded is in the data.
	if stored, err := r.Data.Template(ctx, "sample-kit"); err != nil || !reflect.DeepEqual(stored, tpl) {
		t.Fatalf("%+v %v", stored, err)
	}
	upgraded, err := r.Upgrade(ctx, "sample-kit", "1.3.0", false)
	if err != nil || upgraded.CurrentVersion != "1.3.0" || upgraded.CurrentTag != "1.3.0" || upgraded.CurrentCommit != "c130" {
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
	f.released("1.3.0", "1.3.0", "c130")
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
	tpl, err := r.CheckVersions(context.Background(), "sample-kit")
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
	if _, err := r.Upgrade(ctx, "sample-kit", "1.2.0", false); !errors.Is(err, ErrNotNewer) {
		t.Fatalf("the current version was taken again while its tag had not moved: %v", err)
	}
	f.moved("1.2.0", "c12b")
	tpl, err := r.CheckVersions(ctx, "sample-kit")
	if err != nil || !tpl.CurrentMoved() || tpl.CurrentCommit != "c120" {
		t.Fatalf("%+v %v", tpl, err)
	}
	if checked, err := r.Upgrade(ctx, "sample-kit", "1.2.0", true); err != nil || checked.CurrentCommit != "c12b" || checked.CurrentMoved() {
		t.Fatalf("check: %+v %v", checked, err)
	}
	if stored, _ := r.Data.Template(ctx, "sample-kit"); stored.CurrentCommit != "c120" || !stored.CurrentMoved() {
		t.Fatalf("a check alone recorded the commit: %+v", stored)
	}
	before := f.called("check " + kit + "@1.2.0")
	tpl, err = r.Upgrade(ctx, "sample-kit", "1.2.0", false)
	if err != nil || tpl.CurrentVersion != "1.2.0" || tpl.CurrentCommit != "c12b" || tpl.CurrentMoved() {
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
	f.moved("1.2.0", "c12b")
	if _, err := r.CheckVersions(ctx, "sample-kit"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.1.0", "1.0.0"} {
		if _, err := r.Upgrade(ctx, "sample-kit", version, false); !errors.Is(err, ErrNotNewer) {
			t.Fatalf("%s: %v", version, err)
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
	if err != nil || tpl.CurrentCommit != "c110" || tpl.CurrentTag != "v1.1.0" || tpl.CurrentMoved() {
		t.Fatalf("%+v %v", tpl, err)
	}
}

// A workspace whose applied record's ref is no version takes the version
// whose tag is at its applied commit, from the template's checks.
func TestACheckGivesAnUnknownAppliedVersionTheVersionAtItsCommit(t *testing.T) {
	r, f, rootDir := world(t)
	ctx := context.Background()
	for name, record := range map[string]string{
		"alpha": `{"applications":[{"target":".","template":"` + kit + `@main","source":"` + kit + `","commit":"c110"}]}`,
		"beta":  `{"applications":[{"target":".","template":"` + kit + `@main","source":"` + kit + `","commit":"c130"}]}`,
		"gamma": `{"applications":[{"target":".","template":"` + kit + `@v1.0.0","source":"` + kit + `","commit":"c999"}]}`,
	} {
		if err := os.MkdirAll(filepath.Join(rootDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootDir, name, RecordName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Data.Discover(ctx, []string{"alpha", "beta", "gamma"}); err != nil {
		t.Fatal(err)
	}
	registered(t, r, "")
	want := map[string]store.AppliedVersion{
		// The registration's listing already holds c110's version.
		"alpha": {Version: "1.1.0", Commit: "c110"},
		"beta":  {Commit: "c130"},
		// A ref that is a version is the applied version.
		"gamma": {Version: "1.0.0", Commit: "c999"},
	}
	if got, err := r.Data.AppliedVersions(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	f.released("1.3.0", "1.3.0", "c130")
	if _, err := r.CheckVersions(ctx, "sample-kit"); err != nil {
		t.Fatal(err)
	}
	want["beta"] = store.AppliedVersion{Version: "1.3.0", Commit: "c130"}
	if got, err := r.Data.AppliedVersions(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
}

// heldService holds each listing until released, counting them.
type heldService struct {
	TemplateService
	listings atomic.Int32
	release  chan struct{}
}

func (h *heldService) ListVersions(ctx context.Context, address string) (*templates.Versions, error) {
	h.listings.Add(1)
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return h.TemplateService.ListVersions(ctx, address)
}

// Two checks of one template at once share one run, and a caller that
// leaves does not end it for the other.
func TestChecksOfOneTemplateAtOnceShareOneRun(t *testing.T) {
	r, f, _ := world(t)
	registered(t, r, "")
	f.released("1.3.0", "1.3.0", "c130")
	held := &heldService{TemplateService: r.Templates, release: make(chan struct{})}
	r.Templates = held
	leaving, leave := context.WithCancel(context.Background())
	results := make(chan error, 2)
	go func() { _, err := r.CheckVersions(leaving, "sample-kit"); results <- err }()
	for held.listings.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	var second store.Template
	go func() {
		var err error
		second, err = r.CheckVersions(context.Background(), "sample-kit")
		results <- err
	}()
	leave()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatalf("the leaving caller: %v", err)
	}
	close(held.release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if n := held.listings.Load(); n != 1 || second.Newest() != "1.3.0" {
		t.Fatalf("%d listings, %+v", n, second)
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
	f.released("1.3.0", "1.3.0", "c130")
	// The check at start finds it.
	waitFor(t, "the check at start to find 1.3.0", func() bool { return newest() == "1.3.0" })
	f.released("1.4.0", "1.4.0", "c140")
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
