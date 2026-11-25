package api

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

const kitAddress = "https://git.example.invalid/example-org/kit"

// templateService plays the template service for one repository at
// 1.0.0 and 1.1.0; without definition, a version's files have no
// devcontainer definition.
type templateService struct {
	down       bool
	definition map[string]bool
}

func (s *templateService) ReadAddress(_ context.Context, address string) (*templates.Address, error) {
	if s.down {
		return nil, &templates.UnavailableError{Err: errors.New("no socket")}
	}
	base, ref, _ := strings.Cut(strings.TrimPrefix(address, "https://"), "@")
	base = "https://" + base
	if base != kitAddress {
		return nil, &templates.Error{Status: 400, Kind: "invalid-address", Message: "the address is not a remote Git address"}
	}
	read := &templates.Address{Source: kitAddress, Base: kitAddress}
	if ref != "" {
		read.Reference = &ref
	}
	return read, nil
}

func (s *templateService) ListVersions(context.Context, string) (*templates.Versions, error) {
	return &templates.Versions{Source: kitAddress, Versions: []templates.Version{
		{Version: "1.0.0", Tag: "1.0.0", Commit: commitOf("1.0.0")}, {Version: "1.1.0", Tag: "1.1.0", Commit: commitOf("1.1.0")},
	}}, nil
}

func (s *templateService) Check(_ context.Context, _, ref string) (*templates.Checked, error) {
	return &templates.Checked{Source: kitAddress, Commit: commitOf(ref), Definition: s.definition[ref]}, nil
}

// commitOf is the fixture's commit of a ref: a full object id.
func commitOf(ref string) string { return fmt.Sprintf("%x", sha1.Sum([]byte(ref))) }

func templateFixture(t *testing.T) (*fixture, *templateService) {
	t.Helper()
	f := newFixture(t, nil)
	service := &templateService{definition: map[string]bool{"1.0.0": true, "1.1.0": true}}
	f.server.opts.Registry = &registry.Registry{Data: f.data, Templates: service, Root: f.server.opts.Root}
	f.server.opts.AppliedRecord = registry.Join
	return f, service
}

type templateBody struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Address        string   `json:"address"`
	CurrentVersion string   `json:"currentVersion"`
	RunnerImage    *string  `json:"runnerImage"`
	Workspaces     []string `json:"workspaces"`
}

type checkBody struct {
	Error struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
		Check   string `json:"check"`
	} `json:"error"`
}

func TestATemplateIsRegisteredListedEditedAndRemovedThroughTheAPI(t *testing.T) {
	f, _ := templateFixture(t)
	resp, data := f.do(t, "POST", "/api/templates", `{"name":"Sample kit","address":"`+kitAddress+`@1.0.0","runnerImage":"alpine:3.22"}`)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/api/templates/sample-kit" {
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("Location"), data)
	}
	created := decode[templateBody](t, data)
	if created.ID != "sample-kit" || created.Address != kitAddress || created.CurrentVersion != "1.0.0" || *created.RunnerImage != "alpine:3.22" || len(created.Workspaces) != 0 {
		t.Fatalf("%s", data)
	}
	resp, data = f.do(t, "POST", "/api/templates", `{"name":"Another kit","address":"`+kitAddress+`"}`)
	if body := decode[checkBody](t, data); resp.StatusCode != http.StatusConflict || body.Error.Kind != "template-exists" || !strings.Contains(body.Error.Message, "sample-kit") {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "PATCH", "/api/templates/sample-kit", `{"name":"A kit","runnerImage":null}`)
	if edited := decode[templateBody](t, data); resp.StatusCode != 200 || edited.Name != "A kit" || edited.RunnerImage != nil || edited.Address != kitAddress {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if _, err := f.data.RegisterTemplate(context.Background(), store.Template{ID: "zulu", Name: "A kit", Address: "gh:o/z", Source: "gh:o/z", CurrentVersion: "1.0.0", CurrentTag: "1.0.0"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.data.RegisterTemplate(context.Background(), store.Template{ID: "alpha-kit", Name: "Zeta kit", Address: "gh:o/a", Source: "gh:o/a", CurrentVersion: "1.0.0", CurrentTag: "1.0.0"}, nil); err != nil {
		t.Fatal(err)
	}
	_, data = f.do(t, "GET", "/api/templates", "")
	var list struct{ Templates []templateBody }
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tpl := range list.Templates {
		ids = append(ids, tpl.ID)
	}
	if strings.Join(ids, ",") != "sample-kit,zulu,alpha-kit" {
		t.Fatalf("not by name, then by id: %s", data)
	}
	resp, _ = f.do(t, "DELETE", "/api/templates/zulu", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %d", resp.StatusCode)
	}
	for _, path := range []string{"/api/templates/zulu"} {
		resp, data = f.do(t, "DELETE", path, "")
		if body := decode[checkBody](t, data); resp.StatusCode != 404 || body.Error.Kind != "unknown-template" {
			t.Fatalf("%d %s", resp.StatusCode, data)
		}
		resp, data = f.do(t, "PATCH", path, `{"name":"x"}`)
		if body := decode[checkBody](t, data); resp.StatusCode != 404 || body.Error.Kind != "unknown-template" {
			t.Fatalf("%d %s", resp.StatusCode, data)
		}
		resp, data = f.do(t, "POST", path+"/upgrades", `{"version":"1.1.0"}`)
		if body := decode[checkBody](t, data); resp.StatusCode != 404 || body.Error.Kind != "unknown-template" {
			t.Fatalf("%d %s", resp.StatusCode, data)
		}
	}
}

func TestARegistrationRefusalNamesItsCheckAndNeverTheAddress(t *testing.T) {
	f, service := templateFixture(t)
	password := "https://sample-user:sample-password@git.example.invalid/example-org/kit"
	for _, c := range []struct {
		body, kind, check string
		status            int
	}{
		{`{"name":"Kit","address":"/srv/kit"}`, "template-check-failed", "address", 422},
		{`{"name":"Kit","address":"` + password + `"}`, "template-check-failed", "address", 422},
		{`{"name":"Kit","address":"` + kitAddress + `@main"}`, "template-check-failed", "version", 422},
		{`{"name":"","address":"` + kitAddress + `"}`, "invalid-request", "", 400},
		{`{"name":"Kit","address":"` + kitAddress + `","runnerImage":"Not An Image!"}`, "invalid-request", "", 400},
		{`{"name":"Kit"}`, "invalid-request", "", 400},
		{`{"name":"Kit","address":"` + kitAddress + `","extra":1}`, "invalid-request", "", 400},
		{`{"name":"Kit","address":5}`, "invalid-request", "", 400},
		{`["` + password + `"]`, "invalid-request", "", 400},
		{`{"name":"Kit","address":"` + kitAddress + `"} {}`, "invalid-request", "", 400},
	} {
		resp, data := f.do(t, "POST", "/api/templates", c.body)
		body := decode[checkBody](t, data)
		if resp.StatusCode != c.status || body.Error.Kind != c.kind || body.Error.Check != c.check {
			t.Errorf("%s: %d %s", c.body, resp.StatusCode, data)
		}
		if strings.Contains(string(data), "sample-password") || strings.Contains(string(data), "example.invalid") {
			t.Errorf("%s: the refusal quotes the address: %s", c.body, data)
		}
	}
	service.definition["1.1.0"] = false
	resp, data := f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`"}`)
	if body := decode[checkBody](t, data); resp.StatusCode != 422 || body.Error.Check != "definition" || !strings.Contains(body.Error.Message, ".devcontainer/devcontainer.json") {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	service.down = true
	resp, data = f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`"}`)
	if body := decode[checkBody](t, data); resp.StatusCode != 503 || body.Error.Kind != "template-service-unavailable" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if templates, _ := f.data.Templates(context.Background()); len(templates) != 0 {
		t.Fatalf("a refused template was recorded: %+v", templates)
	}
}

func TestATemplateUpgradesOnlyToANewerVersionItsChecksPass(t *testing.T) {
	f, service := templateFixture(t)
	if resp, data := f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`@1.0.0"}`); resp.StatusCode != 201 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	for _, version := range []string{"1.0.0", "0.9.0", "2.0.0", "main"} {
		resp, data := f.do(t, "POST", "/api/templates/kit/upgrades", `{"version":"`+version+`"}`)
		if body := decode[checkBody](t, data); resp.StatusCode != 409 || body.Error.Kind != "version-not-newer" {
			t.Errorf("%s: %d %s", version, resp.StatusCode, data)
		}
	}
	service.definition["1.1.0"] = false
	resp, data := f.do(t, "POST", "/api/templates/kit/upgrades", `{"version":"1.1.0"}`)
	if body := decode[checkBody](t, data); resp.StatusCode != 422 || body.Error.Kind != "template-check-failed" || body.Error.Check != "definition" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	service.definition["1.1.0"] = true
	resp, data = f.do(t, "POST", "/api/templates/kit/upgrades", `{"version":"1.1.0"}`)
	if upgraded := decode[templateBody](t, data); resp.StatusCode != 200 || upgraded.CurrentVersion != "1.1.0" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if tpl, _ := f.data.Template(context.Background(), "kit"); tpl.CurrentCommit != commitOf("1.1.0") || tpl.CurrentTag != "1.1.0" {
		t.Fatalf("%+v", tpl)
	}
	resp, data = f.do(t, "POST", "/api/templates/kit/upgrades", `{}`)
	if body := decode[checkBody](t, data); resp.StatusCode != 400 || body.Error.Kind != "invalid-request" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
}

func TestAWorkspaceListedForTheFirstTimeJoinsItsTemplateWhichItsUseKeeps(t *testing.T) {
	f, _ := templateFixture(t)
	if resp, data := f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`"}`); resp.StatusCode != 201 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	folder := filepath.Join(f.root, "delta")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"applications":[{"target":".","template":"` + kitAddress + `@1.0.0","source":"` + kitAddress + `","commit":"` + commitOf("1.0.0") + `"}]}`
	if err := os.WriteFile(filepath.Join(folder, registry.RecordName), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	_, data := f.do(t, "GET", "/api/workspaces", "")
	if !strings.Contains(string(data), `"template":{"id":"kit","name":"Kit"},"backupRepository":null,"appliedVersion":"1.0.0","appliedCommit":"`+commitOf("1.0.0")+`","needsUpgrade":true,"name":"delta"`) || !strings.Contains(string(data), `"template":null,"backupRepository":null,"appliedVersion":null,"appliedCommit":null,"needsUpgrade":false,"name":"alpha"`) {
		t.Fatalf("%s", data)
	}
	resp, data := f.do(t, "DELETE", "/api/templates/kit", "")
	if body := decode[checkBody](t, data); resp.StatusCode != 409 || body.Error.Kind != "template-in-use" || !strings.Contains(body.Error.Message, "delta") {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	_, data = f.do(t, "GET", "/api/templates", "")
	if !strings.Contains(string(data), `"workspaces":["delta"]`) {
		t.Fatalf("%s", data)
	}
}

// With check, registration and a template's upgrade run their checks and
// refuse as they do, answer 200 with the template they would store, with
// no Location, and store nothing; check must be a boolean.
func TestACheckRegistersAndUpgradesNothing(t *testing.T) {
	f, service := templateFixture(t)
	ctx := context.Background()
	resp, data := f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`@1.0.0","check":true}`)
	if checked := decode[templateBody](t, data); resp.StatusCode != 200 || checked.ID != "kit" || checked.CurrentVersion != "1.0.0" || checked.Address != kitAddress {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if location := resp.Header.Get("Location"); location != "" {
		t.Fatalf("a check answered Location %s", location)
	}
	if list, _ := f.data.Templates(ctx); len(list) != 0 {
		t.Fatalf("a check stored %+v", list)
	}
	service.definition["1.0.0"] = false
	resp, data = f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`@1.0.0","check":true}`)
	if body := decode[checkBody](t, data); resp.StatusCode != 422 || body.Error.Check != "definition" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	service.definition["1.0.0"] = true
	if resp, data := f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`@1.0.0","check":false}`); resp.StatusCode != 201 || resp.Header.Get("Location") != "/api/templates/kit" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "POST", "/api/templates", `{"name":"Again","address":"`+kitAddress+`","check":true}`)
	if body := decode[checkBody](t, data); resp.StatusCode != 409 || body.Error.Kind != "template-exists" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "POST", "/api/templates/kit/upgrades", `{"version":"1.1.0","check":true}`)
	if checked := decode[templateBody](t, data); resp.StatusCode != 200 || checked.CurrentVersion != "1.1.0" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if tpl, _ := f.data.Template(ctx, "kit"); tpl.CurrentVersion != "1.0.0" {
		t.Fatalf("a check stored %+v", tpl)
	}
	resp, data = f.do(t, "POST", "/api/templates/kit/upgrades", `{"version":"1.0.0","check":true}`)
	if body := decode[checkBody](t, data); resp.StatusCode != 409 || body.Error.Kind != "version-not-newer" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	for _, body := range []string{
		`{"name":"Kit","address":"` + kitAddress + `","check":"yes"}`,
		`{"name":"Kit","address":"` + kitAddress + `","check":null,"other":1}`,
	} {
		resp, data := f.do(t, "POST", "/api/templates", body)
		if decoded := decode[checkBody](t, data); resp.StatusCode != 400 || decoded.Error.Kind != "invalid-request" {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, data)
		}
	}
	if resp, data := f.do(t, "POST", "/api/templates/kit/upgrades", `{"version":"1.1.0","check":1}`); resp.StatusCode != 400 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
}

type versionsBody struct {
	templateBody
	NewestVersion *string `json:"newestVersion"`
	Versions      []struct {
		Version, Tag, Commit string
	} `json:"versions"`
	CurrentMoved bool `json:"currentMoved"`
	LastCheck    *struct {
		At      time.Time `json:"at"`
		Failure *string   `json:"failure"`
	} `json:"lastCheck"`
}

// A check on request answers the template with its check's record: the
// versions found, the newest, and the time with no failure; while the
// template service is down it answers 200 too, with the failure recorded
// and the found versions kept. An unknown template is 404.
func TestACheckOnRequestAnswersTheTemplateWhateverTheOutcome(t *testing.T) {
	f, _ := templateFixture(t)
	if resp, data := f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`@1.0.0"}`); resp.StatusCode != 201 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data := f.do(t, "POST", "/api/templates/kit/version-checks", "")
	checked := decode[versionsBody](t, data)
	if resp.StatusCode != 200 || checked.NewestVersion == nil || *checked.NewestVersion != "1.1.0" || len(checked.Versions) != 2 ||
		checked.Versions[1].Tag != "1.1.0" || checked.CurrentVersion != "1.0.0" || checked.CurrentMoved ||
		checked.LastCheck == nil || checked.LastCheck.Failure != nil || time.Since(checked.LastCheck.At) > time.Minute {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	// The template service is down: no socket answers.
	f.server.opts.Registry = &registry.Registry{Data: f.data, Templates: templates.New(filepath.Join(t.TempDir(), "absent.sock")), Root: f.server.opts.Root}
	resp, data = f.do(t, "POST", "/api/templates/kit/version-checks", "")
	down := decode[versionsBody](t, data)
	if resp.StatusCode != 200 || down.LastCheck == nil || down.LastCheck.Failure == nil || *down.LastCheck.Failure != "template-service-unavailable" ||
		len(down.Versions) != 2 || *down.NewestVersion != "1.1.0" || down.LastCheck.At.Before(checked.LastCheck.At) {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if strings.Contains(string(data), "absent.sock") {
		t.Fatalf("the answer names the socket: %s", data)
	}
	_, data = f.do(t, "GET", "/api/templates", "")
	if !strings.Contains(string(data), `"failure":"template-service-unavailable"`) {
		t.Fatalf("the list does not show the failure: %s", data)
	}
	resp, data = f.do(t, "POST", "/api/templates/absent/version-checks", "")
	if body := decode[checkBody](t, data); resp.StatusCode != 404 || body.Error.Kind != KindUnknownTemplate {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
}

// Each workspace shows its applied version and commit, and that it needs an
// upgrade while its template's current version is newer; an unknown
// version never needs one. A folder listed for the first time takes the
// version whose tag is at its record's commit, whatever the record's ref
// says, else an unknown version with the commit.
func TestTheWorkspaceListShowsAppliedVersionsAndUpgrades(t *testing.T) {
	f, _ := templateFixture(t)
	ctx := context.Background()
	for name, at := range map[string][2]string{
		"delta":   {"1.0.0", commitOf("1.0.0")},
		"echo":    {"main", commitOf("echo")},
		"foxtrot": {"8.0.0", commitOf("1.0.0")},
	} {
		folder := filepath.Join(f.root, name)
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		record := `{"applications":[{"target":".","template":"` + kitAddress + `@` + at[0] + `","source":"` + kitAddress + `","commit":"` + at[1] + `"}]}`
		if err := os.WriteFile(filepath.Join(folder, registry.RecordName), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if resp, data := f.do(t, "POST", "/api/templates", `{"name":"Kit","address":"`+kitAddress+`@1.0.0"}`); resp.StatusCode != 201 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	type row struct {
		Name           string  `json:"name"`
		AppliedVersion *string `json:"appliedVersion"`
		AppliedCommit  *string `json:"appliedCommit"`
		NeedsUpgrade   bool    `json:"needsUpgrade"`
	}
	list := func() map[string]row {
		_, data := f.do(t, "GET", "/api/workspaces", "")
		var body struct{ Workspaces []row }
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]row{}
		for _, r := range body.Workspaces {
			out[r.Name] = r
		}
		return out
	}
	rows := list()
	if r := rows["delta"]; r.AppliedVersion == nil || *r.AppliedVersion != "1.0.0" || *r.AppliedCommit != commitOf("1.0.0") || r.NeedsUpgrade {
		t.Fatalf("%+v", r)
	}
	for _, name := range []string{"echo", "foxtrot"} {
		want := map[string]string{"echo": commitOf("echo"), "foxtrot": commitOf("1.0.0")}[name]
		if r := rows[name]; r.AppliedVersion != nil || r.AppliedCommit == nil || *r.AppliedCommit != want || r.NeedsUpgrade {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	if r := rows["alpha"]; r.AppliedVersion != nil || r.AppliedCommit != nil || r.NeedsUpgrade {
		t.Fatalf("%+v", r)
	}
	if resp, data := f.do(t, "POST", "/api/templates/kit/upgrades", `{"version":"1.1.0"}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	rows = list()
	if !rows["delta"].NeedsUpgrade || rows["echo"].NeedsUpgrade {
		t.Fatalf("%+v", rows)
	}
	if err := f.data.SetAppliedVersion(ctx, "delta", store.AppliedVersion{Version: "1.1.0", Commit: commitOf("1.1.0")}); err != nil {
		t.Fatal(err)
	}
	if r := list()["delta"]; r.NeedsUpgrade || *r.AppliedVersion != "1.1.0" {
		t.Fatalf("%+v", r)
	}
}
