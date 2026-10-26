package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

type schemaNode struct {
	Properties    map[string]*schemaNode `json:"properties"`
	Items         *schemaNode            `json:"items"`
	Required      []string               `json:"required"`
	Default       any                    `json:"default"`
	Pattern       string                 `json:"pattern"`
	Enum          []string               `json:"enum"`
	PropertyNames *schemaNode            `json:"propertyNames"`
	Defs          map[string]*schemaNode `json:"$defs"`
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
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
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

func loadSchema(t *testing.T) *schemaNode {
	t.Helper()
	data, err := os.ReadFile("../../../docs/specifications/assets/configuration/menagerie.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var root schemaNode
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	return &root
}

func TestSchemaAgreesWithParser(t *testing.T) {
	root := *loadSchema(t)
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
	oidc := root.Properties["oidc"]
	if got := strings.Join(oidc.Required, ","); got != "issuer,client-id,redirect-url" {
		t.Errorf("oidc required %s", got)
	}
	if got, want := fmt.Sprint(oidc.Properties["scopes"].Default), fmt.Sprint(auth.DefaultScopes); got != want {
		t.Errorf("scopes default %s, auth requests %s", got, want)
	}
	if got := strings.Join(root.Required, ","); got != "workspace-root,workspace-repository,wyrwood-directory,oidc" {
		t.Errorf("required %s", got)
	}
	if got := root.Properties["templates"].Items.Properties["id"]; got == nil {
		t.Fatal("template id missing")
	}
}

// TestLimitKeysAgreeWithJobs checks that the schema's phase keys are the
// phases the jobs package names, that the specification lists the same
// phases, that the schema's operation keys are the API's operations, and
// that the schema's duration pattern is the parser's.
func TestLimitKeysAgreeWithJobs(t *testing.T) {
	root := loadSchema(t)
	want := strings.Join(slices.Sorted(slices.Values(jobs.Phases)), ",")
	phases := root.Properties["job-limits"].Properties["phases"].PropertyNames.Enum
	if got := strings.Join(slices.Sorted(slices.Values(phases)), ","); got != want {
		t.Errorf("schema phases %s, jobs.Phases %s", got, want)
	}
	var spec struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(repoFile(t, "docs/specifications/configuration.yml")), &spec); err != nil {
		t.Fatal(err)
	}
	_, list, ok := strings.Cut(spec.Description, "The phase names are ")
	list, _, _ = strings.Cut(list, ".")
	var listed []string
	for _, m := range regexp.MustCompile("`([a-z-]+)`").FindAllStringSubmatch(list, -1) {
		listed = append(listed, m[1])
	}
	if got := strings.Join(slices.Sorted(slices.Values(listed)), ","); !ok || got != want {
		t.Errorf("specification phases %s, jobs.Phases %s", got, want)
	}

	var api struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal([]byte(repoFile(t, "docs/specifications/assets/workspace-api/openapi.yaml")), &api); err != nil {
		t.Fatal(err)
	}
	operations := slices.Sorted(maps.Keys(OperationLimits{}.ByName()))
	documented := slices.Sorted(slices.Values(api.Components.Schemas["JobRequest"].Properties["operation"].Enum))
	inSchema := slices.Sorted(maps.Keys(root.Properties["job-limits"].Properties["operations"].Properties))
	if a, b, c := strings.Join(operations, ","), strings.Join(documented, ","), strings.Join(inSchema, ","); a != b || a != c {
		t.Errorf("limit operations %s, API operations %s, schema operations %s", a, b, c)
	}

	if got := root.Defs["duration"].Pattern; got != durationPattern.String() {
		t.Errorf("schema duration pattern %s, parser %s", got, durationPattern)
	}
}
