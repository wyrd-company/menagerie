// ---
// relationships:
//   implements: menagerie
//   references: [workspace]
// ---

package devcontainer

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// localEnvironment is one start or rebuild's view of Menagerie's own
// environment. Values holds only the allowed variables: it is the
// ${localEnv:...} source and Compose interpolation's environment. Process
// is the initializeCommand environment: the allowed variables and nothing
// else, so the command finds its programs on the runner image's own PATH.
type localEnvironment struct {
	values  map[string]string
	process []string
}

func newLocalEnvironment(environ, allow []string) localEnvironment {
	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		allowed[name] = true
	}
	env := localEnvironment{values: map[string]string{}, process: []string{}}
	for _, pair := range environ {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if allowed[name] {
			env.values[name] = value
			env.process = append(env.process, pair)
		}
	}
	return env
}

// variablePattern and the "name:args" split follow devcontainer-builder's
// substitute package, so a reference is found exactly when it substitutes.
var variablePattern = regexp.MustCompile(`\$\{(.*?)\}`)

// refusedLocalEnv lists, sorted and once each, the variables that the
// definition raw reads through ${localEnv:NAME} or ${env:NAME} and that
// are not allowed. Each resolves to its default value, or empty, as an
// unset variable does. It scans the decoded string values, so a JSON
// escape cannot hide a reference; object keys are not substituted and are
// not scanned. Raw that does not decode is scanned as it is.
func refusedLocalEnv(raw []byte, allow []string) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return refusedIn([]string{string(raw)}, allow)
	}
	return refusedIn(jsonStrings(doc, nil), allow)
}

// jsonStrings appends every string value in v.
func jsonStrings(v any, out []string) []string {
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, item := range v {
			out = jsonStrings(item, out)
		}
	case map[string]any:
		for _, item := range v {
			out = jsonStrings(item, out)
		}
	}
	return out
}

func refusedIn(texts []string, allow []string) []string {
	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		allowed[name] = true
	}
	seen := map[string]bool{}
	var out []string
	var matches [][]string
	for _, text := range texts {
		matches = append(matches, variablePattern.FindAllStringSubmatch(text, -1)...)
	}
	for _, m := range matches {
		parts := strings.Split(m[1], ":")
		if len(parts) < 2 || (parts[0] != "localEnv" && parts[0] != "env") {
			continue
		}
		name := parts[1]
		if name == "" || allowed[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
