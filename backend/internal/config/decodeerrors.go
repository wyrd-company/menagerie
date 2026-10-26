// ---
// relationships:
//   implements: configuration
// ---

package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// fieldError is a decoding error of this package's own. It names no value.
type fieldError string

func (e fieldError) Error() string { return string(e) }

var (
	unmarshalEntry = regexp.MustCompile(`(?s)^line ([0-9]+): cannot unmarshal .* into (\S+)$`)
	fieldEntry     = regexp.MustCompile(`(?s)^line ([0-9]+): field (.*) (not found|already set) in type (\S+)$`)
	duplicateEntry = regexp.MustCompile(`(?s)^line ([0-9]+): mapping key .* already defined at line ([0-9]+)$`)
	lineEntry      = regexp.MustCompile(`^line ([0-9]+): `)
	tagEntry       = regexp.MustCompile(`(?s)^yaml: cannot decode .* as a (!![a-z]+)$`)
	// syntaxProblem is the shape of the parser's fixed problem texts. A
	// message that also carries a value, such as an unknown tag's, has
	// characters outside it.
	syntaxProblem = regexp.MustCompile(`^yaml: (?:line ([0-9]+): )?([A-Za-z ,'\]}<>:%-]+)$`)
	keyPattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// decodeError turns an error of the YAML decoder into a diagnostic of a
// line, a field and what the field expects. The decoder's own text quotes
// values, and an anchor can share the webhook URL's node with any field,
// so none of that text is kept.
func decodeError(data []byte, err error) error {
	var own fieldError
	if errors.As(err, &own) {
		return own
	}
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		if m := syntaxProblem.FindStringSubmatch(err.Error()); m != nil {
			return errors.New(atLine(m[1]) + m[2])
		}
		if m := tagEntry.FindStringSubmatch(err.Error()); m != nil {
			return fmt.Errorf("a value tagged %s is not one", m[1])
		}
		if m := lineEntry.FindStringSubmatch(strings.TrimPrefix(err.Error(), "yaml: ")); m != nil {
			return errors.New(atLine(m[1]) + "the document is not valid YAML")
		}
		return errors.New("the document is not valid YAML")
	}
	fields := indexFields(data)
	errs := make([]error, 0, len(te.Errors))
	for _, entry := range te.Errors {
		errs = append(errs, fields.describe(entry))
	}
	return errors.Join(errs...)
}

func atLine(line string) string {
	if line == "" {
		return ""
	}
	return "line " + line + ": "
}

// keyName is a mapping key that is safe to name in a diagnostic: the shape
// of the document's own keys, which no URL has.
func keyName(n *yaml.Node) (string, bool) {
	n = resolved(n)
	return n.Value, n.Kind == yaml.ScalarNode && keyPattern.MatchString(n.Value)
}

// fieldIndex finds the fields at a line of the document. Each map is keyed
// by line and Go type: values holds the fields whose value starts there,
// keys the mappings with a key there.
type fieldIndex struct {
	values, keys map[string][]string
	budget       int
}

func indexFields(data []byte) *fieldIndex {
	x := &fieldIndex{values: map[string][]string{}, keys: map[string][]string{}, budget: 10000}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) == nil && doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		x.walk(doc.Content[0], reflect.TypeFor[Config](), "", 0)
	}
	return x
}

func (x *fieldIndex) add(index map[string][]string, line int, t reflect.Type, path string) {
	key := fmt.Sprintf("%d %s", line, t)
	if !slices.Contains(index[key], path) {
		index[key] = append(index[key], path)
	}
}

func (x *fieldIndex) walk(n *yaml.Node, t reflect.Type, path string, depth int) {
	if depth > 32 || x.budget <= 0 {
		return
	}
	x.budget--
	n = resolved(n)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	x.add(x.values, n.Line, t, path)
	switch {
	case n.Kind == yaml.SequenceNode && t.Kind() == reflect.Slice:
		for i, item := range n.Content {
			x.walk(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i), depth+1)
		}
	case n.Kind == yaml.MappingNode && (t.Kind() == reflect.Struct || t.Kind() == reflect.Map):
		x.walkMapping(n, t, path, depth)
	}
}

func (x *fieldIndex) walkMapping(n *yaml.Node, t reflect.Type, path string, depth int) {
	if depth > 32 || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		x.add(x.keys, k.Line, t, path)
		if k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge" {
			if v = resolved(v); v.Kind == yaml.SequenceNode {
				for _, m := range v.Content {
					x.walkMapping(resolved(m), t, path, depth+1)
				}
			} else {
				x.walkMapping(v, t, path, depth+1)
			}
			continue
		}
		name, ok := keyName(k)
		if !ok {
			continue
		}
		var field reflect.Type
		if t.Kind() == reflect.Map {
			field = t.Elem()
		} else if field, ok = structField(t, name); !ok {
			continue
		}
		child := name
		if path != "" {
			child = path + "." + name
		}
		x.walk(v, field, child, depth+1)
	}
}

func structField(t reflect.Type, name string) (reflect.Type, bool) {
	for f := range t.Fields() {
		if tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); tag == name {
			return f.Type, true
		}
	}
	return nil, false
}

// at names the fields in index at line with type typ, or "the document".
func (x *fieldIndex) at(index map[string][]string, line, typ string) (string, bool) {
	paths := index[line+" "+strings.TrimLeft(typ, "*")]
	if len(paths) == 0 {
		return "", false
	}
	names := slices.Clone(paths)
	for i, p := range names {
		if p == "" {
			names[i] = "the document"
		}
	}
	slices.Sort(names)
	return strings.Join(names, ", "), true
}

func (x *fieldIndex) describe(entry string) error {
	if m := unmarshalEntry.FindStringSubmatch(entry); m != nil {
		if where, ok := x.at(x.values, m[1], m[2]); ok {
			return fmt.Errorf("line %s: %s must be %s", m[1], where, expected(m[2]))
		}
		return fmt.Errorf("line %s: expected %s", m[1], expected(m[2]))
	}
	if m := fieldEntry.FindStringSubmatch(entry); m != nil {
		where, ok := x.at(x.keys, m[1], m[4])
		if !ok {
			where = "a mapping"
		}
		known := keyPattern.MatchString(m[2])
		switch {
		case m[3] == "already set" && known:
			return fmt.Errorf("line %s: %s gives field %s twice", m[1], where, m[2])
		case m[3] == "already set":
			return fmt.Errorf("line %s: %s gives a field twice", m[1], where)
		case known:
			return fmt.Errorf("line %s: %s has no field %s", m[1], where, m[2])
		}
		return fmt.Errorf("line %s: %s has a field it does not know", m[1], where)
	}
	if m := duplicateEntry.FindStringSubmatch(entry); m != nil {
		return fmt.Errorf("line %s: a key is given again, first at line %s", m[1], m[2])
	}
	if m := lineEntry.FindStringSubmatch(entry); m != nil {
		return fmt.Errorf("line %s: the value does not fit the configuration", m[1])
	}
	return errors.New("a value does not fit the configuration")
}

// expected describes what a value of the Go type typ is in the document.
func expected(typ string) string {
	typ = strings.TrimLeft(typ, "*")
	switch {
	case typ == "config.Duration":
		return "a duration such as 45m"
	case typ == "string":
		return "a string"
	case typ == "bool":
		return "true or false"
	case strings.HasPrefix(typ, "[]"):
		return "a list"
	case strings.HasPrefix(typ, "map["), strings.HasPrefix(typ, "config."):
		return "a mapping"
	case strings.HasPrefix(typ, "int"), strings.HasPrefix(typ, "uint"):
		return "a whole number"
	}
	return "a value of another kind"
}
