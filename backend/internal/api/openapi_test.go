package api

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

type openAPIDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Enum       []string `yaml:"enum"`
			Properties map[string]struct {
				Enum     []string `yaml:"enum"`
				Examples []string `yaml:"examples"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func loadOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()
	data, err := os.ReadFile("../../../docs/specifications/assets/workspace-api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func joinSorted(items []string) string {
	sort.Strings(items)
	return strings.Join(items, "\n")
}

func TestOpenAPIPathsAgreeWithRoutes(t *testing.T) {
	doc := loadOpenAPI(t)
	var documented []string
	for path, item := range doc.Paths {
		for method := range item {
			if method == "parameters" {
				continue
			}
			documented = append(documented, strings.ToUpper(method)+" "+path)
		}
	}
	var served []string
	for _, r := range New(Options{}).Routes() {
		served = append(served, r.Method+" "+r.Pattern)
	}
	if a, b := joinSorted(documented), joinSorted(served); a != b {
		t.Fatalf("documented:\n%s\n\nserved:\n%s", a, b)
	}
}

func TestOpenAPIEnumsAgreeWithCode(t *testing.T) {
	doc := loadOpenAPI(t)
	schemas := doc.Components.Schemas
	var ops []string
	for name := range ContainerOperations(nil, &fakeRuntime{}) {
		ops = append(ops, name)
	}
	// The workflows package registers the interview operations under these
	// names; its own test checks that it serves exactly them.
	ops = append(ops, OperationCreate, OperationUpgrade)
	if a, b := joinSorted(schemas["JobRequest"].Properties["operation"].Enum), joinSorted(ops); a != b {
		t.Errorf("operations documented %q served %q", a, b)
	}
	states := []string{string(jobs.Running), string(jobs.WaitingForAnswers), string(jobs.Succeeded), string(jobs.Failed), string(jobs.Ended)}
	if a, b := joinSorted(schemas["JobState"].Enum), joinSorted(states); a != b {
		t.Errorf("job states documented %q code %q", a, b)
	}
	if a, b := joinSorted(schemas["JobLimit"].Properties["scope"].Enum), joinSorted([]string{jobs.ScopeJob, jobs.ScopePhase}); a != b {
		t.Errorf("limit scopes documented %q code %q", a, b)
	}
	if !slices.Contains(schemas["JobError"].Properties["kind"].Examples, jobs.KindTimedOut) {
		t.Errorf("error kind %s undocumented", jobs.KindTimedOut)
	}
	containers := []string{devcontainer.StateRunning, devcontainer.StateStopped, devcontainer.StateNoContainer}
	if a, b := joinSorted(schemas["Workspace"].Properties["state"].Enum), joinSorted(containers); a != b {
		t.Errorf("workspace states documented %q code %q", a, b)
	}
}

// TestOpenAPIJobAgreesWithTheSnapshot checks that the documented job has
// exactly the members a snapshot encodes.
func TestOpenAPIJobAgreesWithTheSnapshot(t *testing.T) {
	doc := loadOpenAPI(t)
	var documented, encoded []string
	for name := range doc.Components.Schemas["Job"].Properties {
		documented = append(documented, name)
	}
	raw, err := json.Marshal(jobs.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]any
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	for name := range members {
		encoded = append(encoded, name)
	}
	if a, b := joinSorted(documented), joinSorted(encoded); a != b {
		t.Errorf("job documented:\n%s\n\nencoded:\n%s", a, b)
	}
}
