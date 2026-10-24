package config

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type schemaNode struct {
	Properties map[string]*schemaNode `json:"properties"`
	Items      *schemaNode            `json:"items"`
	Required   []string               `json:"required"`
	Default    any                    `json:"default"`
}

// schemaKeys lists every property path the schema defines.
func schemaKeys(prefix string, n *schemaNode, out map[string]bool) {
	for name, child := range n.Properties {
		out[prefix+name] = true
		if child.Items != nil {
			schemaKeys(prefix+name+"[].", child.Items, out)
			continue
		}
		schemaKeys(prefix+name+".", child, out)
	}
}

// structKeys lists every yaml key path the parser accepts.
func structKeys(prefix string, t reflect.Type, out map[string]bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		name := f.Tag.Get("yaml")
		out[prefix+name] = true
		ft := f.Type
		switch ft.Kind() {
		case reflect.Struct:
			structKeys(prefix+name+".", ft, out)
		case reflect.Slice:
			if ft.Elem().Kind() == reflect.Struct {
				structKeys(prefix+name+"[].", ft.Elem(), out)
			}
		}
	}
}

func sorted(m map[string]bool) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

func TestSchemaAgreesWithParser(t *testing.T) {
	data, err := os.ReadFile("../../../docs/specifications/assets/configuration/menagerie.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var root schemaNode
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	fromSchema, fromParser := map[string]bool{}, map[string]bool{}
	schemaKeys("", &root, fromSchema)
	structKeys("", reflect.TypeOf(Config{}), fromParser)
	if a, b := sorted(fromSchema), sorted(fromParser); a != b {
		t.Fatalf("schema keys:\n%s\n\nparser keys:\n%s", a, b)
	}
	if got := root.Properties["listen"].Default; got != DefaultListen {
		t.Errorf("listen default %v", got)
	}
	if got := root.Properties["template-service"].Properties["socket"].Default; got != DefaultTemplateSocket {
		t.Errorf("socket default %v", got)
	}
	if got := strings.Join(root.Required, ","); got != "workspace-root,workspace-repository,wyrwood-directory,oidc" {
		t.Errorf("required %s", got)
	}
	if got := root.Properties["templates"].Items.Properties["id"]; got == nil {
		t.Fatal("template id missing")
	}
}
