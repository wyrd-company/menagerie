package workspacesocket

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type socketDoc struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func members(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(m))
}

// TestTheSocketAPIAgreesWithItsDocument checks the route, the result's
// members and the refusal kinds against the OpenAPI document.
func TestTheSocketAPIAgreesWithItsDocument(t *testing.T) {
	data, err := os.ReadFile("../../../docs/specifications/assets/workspace-socket/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc socketDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if paths := slices.Sorted(maps.Keys(doc.Paths)); len(paths) != 1 || paths[0] != CommitsPath || len(doc.Paths[CommitsPath]) != 1 || doc.Paths[CommitsPath]["post"] == nil {
		t.Fatalf("paths %v", doc.Paths)
	}
	schemas := doc.Components.Schemas
	if a, b := strings.Join(slices.Sorted(maps.Keys(schemas["CommitResult"].Properties)), ","), strings.Join(members(t, Result{}), ","); a != b {
		t.Errorf("result documented %s encoded %s", a, b)
	}
	refusal := schemas["RefusalEnvelope"].Properties["error"].Properties
	if a, b := strings.Join(slices.Sorted(maps.Keys(refusal)), ","), strings.Join(members(t, Error{Kind: "k", ActiveJobID: "j", RetryAfterSeconds: 1}), ","); a != b {
		t.Errorf("refusal documented %s encoded %s", a, b)
	}
	kinds := []string{KindInvalidRequest, KindInvalidMessage, KindRateLimited, KindWorkspaceBusy, KindWorkspaceMissing, KindNotFound, KindMethodNotAllowed, KindInternal}
	if a, b := strings.Join(slices.Sorted(slices.Values(refusal["kind"].Enum)), ","), strings.Join(slices.Sorted(slices.Values(kinds)), ","); a != b {
		t.Errorf("kinds documented %s served %s", a, b)
	}
}
