//go:build templateservice

package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// templateVersions reads a template as the API lists it.
func (a *accept) templateVersions(t *testing.T, id string) (newest string, moved bool, failure *string) {
	t.Helper()
	status, body := a.send(t, http.MethodGet, "/api/templates", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	var list struct {
		Templates []struct {
			ID            string
			NewestVersion *string
			CurrentMoved  bool
			LastCheck     *struct{ Failure *string }
		}
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	for _, tpl := range list.Templates {
		if tpl.ID == id {
			if tpl.NewestVersion != nil {
				newest = *tpl.NewestVersion
			}
			if tpl.LastCheck != nil {
				failure = tpl.LastCheck.Failure
			}
			return newest, tpl.CurrentMoved, failure
		}
	}
	t.Fatalf("no template %s: %s", id, body)
	return "", false, nil
}

// release commits a change to the repository and tags it, as a template's
// author does, and returns the commit.
func (a *accept) release(t *testing.T, repo, tag string, version int) string {
	t.Helper()
	sampleKit(t, repo, version)
	writeFile(t, repo, "template/release.txt", tag+"\n")
	gitCLI(t, repo, "add", "-A")
	gitCLI(t, repo, "commit", "-q", "-m", tag)
	gitCLI(t, repo, "tag", "-f", tag)
	return gitCLI(t, repo, "rev-parse", "HEAD")
}

// Menagerie finds a new version tag of a registered template in a local Git
// repository, served over HTTPS, by itself on its schedule and on request,
// with no restart. A tag moved to another commit shows the current version
// as moved and refuses creates until the template is upgraded to that
// version again, which records the new commit that a create then pins.
func TestVersionsAreFoundOnTheScheduleAndOnRequestWithNoRestart(t *testing.T) {
	a := newAccept(t, &fakeRuntime{})
	kit := a.kitRepository(t, "found", "1.0.0", false)
	repo := filepath.Join(a.templates, "found")
	if status, body := a.post(t, "/api/templates", `{"name":"Found","address":"`+kit+`"}`); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	root, err := workspace.NewRoot(a.root)
	if err != nil {
		t.Fatal(err)
	}
	reg := &registry.Registry{Data: a.data, Templates: templates.New(a.socket), Root: root}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	logged := &lockedText{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.Schedule(ctx, ticks, func(format string, args ...any) { fmt.Fprintf(logged, format+"\n", args...) })
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, "the check at start", func() bool {
		tpl, err := a.data.Template(context.Background(), "found")
		return err == nil && tpl.CheckedAt.After(time.Now().Add(-time.Minute)) && tpl.Newest() == "1.0.0"
	})

	// A tag pushed while Menagerie runs is found at the next tick.
	v110 := a.release(t, repo, "1.1.0", 1)
	ticks <- time.Now()
	waitUntil(t, "the scheduled check", func() bool {
		newest, _, _ := a.templateVersions(t, "found")
		return newest == "1.1.0"
	})
	if tpl, _ := a.data.Template(context.Background(), "found"); tpl.Versions[1] != (store.Version{Version: "1.1.0", Tag: "1.1.0", Commit: v110}) {
		t.Fatalf("%+v", tpl.Versions)
	}

	// And one pushed after it, on request.
	v120 := a.release(t, repo, "v1.2.0", 1)
	status, body := a.post(t, "/api/templates/found/version-checks", "")
	if status != http.StatusOK || !strings.Contains(body, `"newestVersion":"1.2.0"`) || !strings.Contains(body, `{"version":"1.2.0","tag":"v1.2.0","commit":"`+v120+`"}`) {
		t.Fatalf("check: %d %s", status, body)
	}
	if status, body := a.post(t, "/api/templates/found/upgrades", `{"version":"1.2.0"}`); status != http.StatusOK || !strings.Contains(body, `"currentVersion":"1.2.0"`) {
		t.Fatalf("template upgrade: %d %s", status, body)
	}

	// The current version's tag moves to another commit.
	moved := a.release(t, repo, "v1.2.0", 2)
	status, body = a.post(t, "/api/templates/found/version-checks", "")
	if status != http.StatusOK || !strings.Contains(body, `"currentMoved":true`) {
		t.Fatalf("check after the move: %d %s", status, body)
	}
	if tpl, _ := a.data.Template(context.Background(), "found"); tpl.CurrentCommit != v120 {
		t.Fatalf("a check replaced the recorded commit: %+v", tpl)
	}
	if status, body := a.post(t, "/api/jobs", `{"operation":"create","workspace":"beta","templateId":"found"}`); status != http.StatusConflict || !strings.Contains(body, "template-version-moved") {
		t.Fatalf("create while moved: %d %s", status, body)
	}
	if status, body := a.post(t, "/api/templates/found/upgrades", `{"version":"1.1.0"}`); status != http.StatusConflict || !strings.Contains(body, "version-not-newer") {
		t.Fatalf("an older version while moved: %d %s", status, body)
	}
	status, body = a.post(t, "/api/templates/found/upgrades", `{"version":"1.2.0"}`)
	if status != http.StatusOK || !strings.Contains(body, `"currentMoved":false`) {
		t.Fatalf("taking the moved tag again: %d %s", status, body)
	}
	if tpl, _ := a.data.Template(context.Background(), "found"); tpl.CurrentCommit != moved || tpl.CurrentTag != "v1.2.0" {
		t.Fatalf("the new commit was not recorded: %+v", tpl)
	}
	job, _ := a.drive(t, "create", "beta", "found", map[string]any{"label": "first", "extra": "second"})
	if snap := job.Snapshot(); snap.State != jobs.Succeeded {
		evidence(t, job)
		t.Fatalf("create %+v %+v", snap, snap.Error)
	}
	if applied, _ := a.data.AppliedVersion(context.Background(), "beta"); applied != (store.AppliedVersion{Version: "1.2.0", Commit: moved}) {
		t.Fatalf("applied %+v", applied)
	}
	if strings.Contains(logged.String(), "example") || strings.Contains(logged.String(), a.origin) {
		t.Fatalf("the schedule logged an address:\n%s", logged.String())
	}
}

// An upgrade of a workspace whose applied version is unknown asks the real
// template service whether the current version's commit descends from the
// applied commit: a commit on the version's history upgrades, and one off
// it fails as template-refused, naming both, with nothing asked.
func TestAnUnknownAppliedVersionUpgradesOnlyByDescentThroughTheTemplateService(t *testing.T) {
	a := newAccept(t, &fakeRuntime{})
	kit := a.kitRepository(t, "lineage", "1.0.0", false)
	repo := filepath.Join(a.templates, "lineage")
	if status, body := a.post(t, "/api/templates", `{"name":"Lineage","address":"`+kit+`"}`); status != http.StatusCreated {
		t.Fatalf("register: %d %s", status, body)
	}
	first := gitCLI(t, repo, "rev-parse", "1.0.0")
	gitCLI(t, repo, "checkout", "-q", "-b", "side")
	side := a.release(t, repo, "side-marker", 1)
	gitCLI(t, repo, "checkout", "-q", "main")
	second := a.release(t, repo, "2.0.0", 2)
	for _, name := range []string{"ahead", "aside"} {
		job, _ := a.drive(t, "create", name, "lineage", map[string]any{"label": name})
		if snap := job.Snapshot(); snap.State != jobs.Succeeded {
			evidence(t, job)
			t.Fatalf("create %+v %+v", snap, snap.Error)
		}
	}
	if status, body := a.post(t, "/api/templates/lineage/version-checks", ""); status != http.StatusOK || !strings.Contains(body, `"newestVersion":"2.0.0"`) {
		t.Fatalf("check: %d %s", status, body)
	}
	if status, body := a.post(t, "/api/templates/lineage/upgrades", `{"version":"2.0.0"}`); status != http.StatusOK {
		t.Fatalf("template upgrade: %d %s", status, body)
	}
	ctx := context.Background()
	for name, commit := range map[string]string{"ahead": first, "aside": side} {
		if err := a.data.SetAppliedVersion(ctx, name, store.AppliedVersion{Commit: commit}); err != nil {
			t.Fatal(err)
		}
	}
	job, _ := a.drive(t, "upgrade", "aside", "", map[string]any{"extra": "added"})
	snap := job.Snapshot()
	if snap.State != jobs.Failed || snap.Error.Kind != KindTemplateRefused || !strings.Contains(snap.Error.Message, side) || !strings.Contains(snap.Error.Message, second) {
		evidence(t, job)
		t.Fatalf("upgrade off the history %+v %+v", snap, snap.Error)
	}
	if strings.Contains(strings.Join(events(job, "interview"), ""), "extra") {
		t.Fatal("the refused upgrade asked a question")
	}
	job, asked := a.drive(t, "upgrade", "ahead", "", map[string]any{"extra": "added"})
	if snap := job.Snapshot(); snap.State != jobs.Succeeded {
		evidence(t, job)
		t.Fatalf("upgrade on the history %+v %+v", snap, snap.Error)
	}
	if len(asked) != 1 || asked[0][0] != "extra" {
		t.Fatalf("asked %v", asked)
	}
	if applied, _ := a.data.AppliedVersion(ctx, "ahead"); applied != (store.AppliedVersion{Version: "2.0.0", Commit: second}) {
		t.Fatalf("applied %+v", applied)
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
