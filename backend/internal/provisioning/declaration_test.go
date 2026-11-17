package provisioning

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
)

const sampleBlock = `{"wyrwood":{"accessGroup":1000,"policies":["workspace","client-key"],"values":{"key_name":"sample"}},"xolotl":{"keyPolicy":"host-sample","keyName":"sample"}}`

func definition(block string) devcontainer.Definition {
	return devcontainer.Definition{Block: json.RawMessage(block), ContainerName: "container-sample"}
}

func TestParseReadsAValidBlock(t *testing.T) {
	d, err := Parse(definition(sampleBlock))
	if err != nil {
		t.Fatal(err)
	}
	want := &Declaration{
		ContainerName: "container-sample",
		Wyrwood:       WyrwoodBlock{AccessGroup: 1000},
		Ignored:       []string{"xolotl", "wyrwood.policies", "wyrwood.values"},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("%+v", d)
	}
}

func TestParseReadsABlockWithoutTheOptionalParts(t *testing.T) {
	d, err := Parse(definition(`{"wyrwood":{"accessGroup":0,"policies":["workspace"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Xolotl != nil || d.Wyrwood.AccessGroup != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestParseOfNoBlockIsNoDeclaration(t *testing.T) {
	d, err := Parse(devcontainer.Definition{ContainerName: "container-sample"})
	if d != nil || err != nil {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestParseRefusesEachInvalidField(t *testing.T) {
	wrap := func(wyrwood string) string { return `{"wyrwood":` + wyrwood + `}` }
	for name, tc := range map[string]struct{ block, want string }{
		"not an object":      {`[]`, "customizations.menagerie must be an object"},
		"null":               {`null`, "customizations.menagerie must be an object"},
		"unknown key":        {`{"wyrwood":{"accessGroup":1,"policies":["a"]},"other":1}`, `unknown key "other"`},
		"a key's other case": {`{"Wyrwood":{"accessGroup":1,"policies":["a"]}}`, `unknown key "Wyrwood"`},
		"no wyrwood":         {`{}`, "wyrwood is required"},
		"unknown in wyrwood": {wrap(`{"accessGroup":1,"policies":["a"],"other":1}`), `unknown key "other"`},
		"case in wyrwood":    {wrap(`{"accessgroup":1,"policies":["a"]}`), `unknown key "accessgroup"`},
		"no group":           {wrap(`{"policies":["a"]}`), "accessGroup is required"},
		"null group":         {wrap(`{"accessGroup":null,"policies":["a"]}`), "accessGroup must be"},
		"negative group":     {wrap(`{"accessGroup":-1,"policies":["a"]}`), "accessGroup must be"},
		"fractional group":   {wrap(`{"accessGroup":1.5,"policies":["a"]}`), "accessGroup must be"},
		"text group":         {wrap(`{"accessGroup":"1","policies":["a"]}`), "accessGroup must be"},
		"group too large":    {wrap(`{"accessGroup":4294967295,"policies":["a"]}`), "accessGroup must be"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(definition(tc.block))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseRefusesAComposeDefinitionAndNoName(t *testing.T) {
	def := definition(sampleBlock)
	def.Compose = true
	if _, err := Parse(def); err == nil || !strings.Contains(err.Error(), "Compose") {
		t.Errorf("compose: %v", err)
	}
	def = definition(sampleBlock)
	def.ContainerName = ""
	if _, err := Parse(def); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Errorf("no name: %v", err)
	}
}

// TestSchemaAgreesWithTheParser checks that the schema's keys are the keys the
// parser accepts and that its patterns are the parser's.
func TestSchemaAgreesWithTheParser(t *testing.T) {
	data, err := os.ReadFile("../../../docs/specifications/assets/provisioning-declaration/menagerie-customization.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	type node struct {
		Properties map[string]*node `json:"properties"`
		Required   []string         `json:"required"`
		Items      *node            `json:"items"`
		Pattern    string           `json:"pattern"`
		MaxLength  int              `json:"maxLength"`
		Maximum    uint64           `json:"maximum"`
		Names      *node            `json:"propertyNames"`
		Additional json.RawMessage  `json:"additionalProperties"`
	}
	var schema node
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	keys := func(n *node) []string { return slices.Sorted(maps.Keys(n.Properties)) }
	wyrwood := schema.Properties["wyrwood"]
	if got := keys(&schema); !slices.Equal(got, []string{"wyrwood"}) {
		t.Errorf("keys %v", got)
	}
	if got := keys(wyrwood); !slices.Equal(got, []string{"accessGroup"}) {
		t.Errorf("wyrwood keys %v", got)
	}
	if !slices.Equal(schema.Required, []string{"wyrwood"}) || !slices.Equal(wyrwood.Required, []string{"accessGroup"}) {
		t.Fatal("required fields drifted")
	}
	if wyrwood.Properties["accessGroup"].Maximum != maxAccessGroup {
		t.Fatal("access group range drifted")
	}
	// The schema lists rendered fields. The parser additionally accepts exactly
	// the three migration fields, with any value, and reports every one.
	d, err := Parse(definition(`{"wyrwood":{"accessGroup":0,"policies":false,"values":123},"xolotl":null}`))
	if err != nil || !slices.Equal(d.Ignored, []string{"xolotl", "wyrwood.policies", "wyrwood.values"}) {
		t.Fatalf("%+v %v", d, err)
	}
	for _, raw := range []string{`{"wyrwood":{"accessGroup":0},"other":null}`, `{"wyrwood":{"accessGroup":0,"other":null}}`} {
		if _, err := Parse(definition(raw)); err == nil {
			t.Fatal("unknown key accepted")
		}
	}
}
