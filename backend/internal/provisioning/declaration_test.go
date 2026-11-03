package provisioning

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"regexp"
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
		Wyrwood:       WyrwoodBlock{AccessGroup: 1000, Policies: []string{"workspace", "client-key"}, Values: map[string]string{"key_name": "sample"}},
		Xolotl:        &Key{Policy: "host-sample", Name: "sample"},
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
	if d.Xolotl != nil || d.Wyrwood.Values != nil || d.Wyrwood.AccessGroup != 0 {
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
		"no policies":        {wrap(`{"accessGroup":1}`), "policies is required"},
		"empty policies":     {wrap(`{"accessGroup":1,"policies":[]}`), "at least one"},
		"null policies":      {wrap(`{"accessGroup":1,"policies":null}`), "at least one"},
		"bad policy":         {wrap(`{"accessGroup":1,"policies":["A"]}`), "not a policy name"},
		"long policy":        {wrap(`{"accessGroup":1,"policies":["` + strings.Repeat("a", 65) + `"]}`), "not a policy name"},
		"same policy twice":  {wrap(`{"accessGroup":1,"policies":["a","a"]}`), "twice"},
		"values not object":  {wrap(`{"accessGroup":1,"policies":["a"],"values":[]}`), "values must be an object"},
		"values null":        {wrap(`{"accessGroup":1,"policies":["a"],"values":null}`), "values must be an object"},
		"bad value name":     {wrap(`{"accessGroup":1,"policies":["a"],"values":{"A":"b"}}`), "not a value name"},
		"value not text":     {wrap(`{"accessGroup":1,"policies":["a"],"values":{"a":1}}`), "only text"},
		"wildcard value":     {wrap(`{"accessGroup":1,"policies":["a"],"values":{"a":"*"}}`), "values.a must"},
		"path value":         {wrap(`{"accessGroup":1,"policies":["a"],"values":{"a":"b/c"}}`), "values.a must"},
		"long value":         {wrap(`{"accessGroup":1,"policies":["a"],"values":{"a":"` + strings.Repeat("b", 64) + `"}}`), "values.a must"},
		"xolotl not object":  {`{"wyrwood":{"accessGroup":1,"policies":["a"]},"xolotl":"a"}`, "xolotl must be an object"},
		"unknown in xolotl":  {`{"wyrwood":{"accessGroup":1,"policies":["a"]},"xolotl":{"keyPolicy":"a","keyName":"b","c":1}}`, `unknown key "c"`},
		"no key policy":      {`{"wyrwood":{"accessGroup":1,"policies":["a"]},"xolotl":{"keyName":"b"}}`, "keyPolicy is required"},
		"no key name":        {`{"wyrwood":{"accessGroup":1,"policies":["a"]},"xolotl":{"keyPolicy":"a"}}`, "keyName is required"},
		"bad key policy":     {`{"wyrwood":{"accessGroup":1,"policies":["a"]},"xolotl":{"keyPolicy":"A","keyName":"b"}}`, "keyPolicy must be"},
		"bad key name":       {`{"wyrwood":{"accessGroup":1,"policies":["a"]},"xolotl":{"keyPolicy":"a","keyName":"b/c"}}`, "keyName must be"},
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
	wyrwood, xolotl := schema.Properties["wyrwood"], schema.Properties["xolotl"]
	if got := keys(&schema); !slices.Equal(got, []string{"wyrwood", "xolotl"}) {
		t.Errorf("keys %v", got)
	}
	if got := keys(wyrwood); !slices.Equal(got, []string{"accessGroup", "policies", "values"}) {
		t.Errorf("wyrwood keys %v", got)
	}
	if got := keys(xolotl); !slices.Equal(got, []string{"keyName", "keyPolicy"}) {
		t.Errorf("xolotl keys %v", got)
	}
	if wyrwood.Properties["accessGroup"].Maximum != maxAccessGroup {
		t.Errorf("maximum %d", wyrwood.Properties["accessGroup"].Maximum)
	}
	var value node
	if err := json.Unmarshal(wyrwood.Properties["values"].Additional, &value); err != nil {
		t.Fatal(err)
	}
	for field, tc := range map[string]struct {
		schema string
		code   *regexp.Regexp
	}{
		"policies":   {wyrwood.Properties["policies"].Items.Pattern, policyPattern},
		"value name": {wyrwood.Properties["values"].Names.Pattern, valueNamePattern},
		"value":      {value.Pattern, valuePattern},
		"keyPolicy":  {xolotl.Properties["keyPolicy"].Pattern, xolotlNamePattern},
		"keyName":    {xolotl.Properties["keyName"].Pattern, xolotlNamePattern},
	} {
		if tc.schema != tc.code.String() {
			t.Errorf("%s: schema %q, parser %q", field, tc.schema, tc.code)
		}
	}
}
