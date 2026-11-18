package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{Version: "1.0.0", Tag: "1.0.0", Commit: "c1"}, {Version: "1.1.0", Tag: "1.1.0", Commit: "c11"},
	}}, nil
}

func (s *templateService) Check(_ context.Context, _, ref string) (*templates.Checked, error) {
	return &templates.Checked{Source: kitAddress, Commit: "c" + ref, Definition: s.definition[ref]}, nil
}

func templateFixture(t *testing.T) (*fixture, *templateService) {
	t.Helper()
	f := newFixture(t, nil)
	service := &templateService{definition: map[string]bool{"1.0.0": true, "1.1.0": true}}
	f.server.opts.Registry = &registry.Registry{Data: f.data, Templates: service, Root: f.server.opts.Root}
	f.server.opts.AppliedSource = registry.Source
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
	if tpl, _ := f.data.Template(context.Background(), "kit"); tpl.CurrentCommit != "c1.1.0" || tpl.CurrentTag != "1.1.0" {
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
	record := `{"applications":[{"target":".","template":"` + kitAddress + `@1.0.0","source":"` + kitAddress + `","commit":null}]}`
	if err := os.WriteFile(filepath.Join(folder, registry.RecordName), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	_, data := f.do(t, "GET", "/api/workspaces", "")
	if !strings.Contains(string(data), `"template":{"id":"kit","name":"Kit"},"name":"delta"`) || !strings.Contains(string(data), `"template":null,"name":"alpha"`) {
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
