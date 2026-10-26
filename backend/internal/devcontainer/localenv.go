// ---
// relationships:
//   implements: menagerie
//   references: [workspace]
// ---

package devcontainer

import (
	"regexp"
	"sort"
	"strings"
)

// localEnvironment is one start or rebuild's view of Menagerie's own
// environment. Values holds only the allowed variables: it is the
// ${localEnv:...} source and Compose interpolation's environment. Process
// is the initializeCommand environment: the allowed variables and PATH,
// so the command can find its programs. PATH is not readable through
// ${localEnv:PATH} unless it is allowed.
type localEnvironment struct {
	values  map[string]string
	process []string
}

func newLocalEnvironment(environ, allow []string) localEnvironment {
	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		allowed[name] = true
	}
	env := localEnvironment{values: map[string]string{}}
	for _, pair := range environ {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if allowed[name] {
			env.values[name] = value
			env.process = append(env.process, pair)
		} else if name == "PATH" {
			env.process = append(env.process, pair)
		}
	}
	return env
}

// variablePattern and the "name:args" split follow devcontainer-builder's
// substitute package, so a reference is found exactly when it substitutes.
var variablePattern = regexp.MustCompile(`\$\{(.*?)\}`)

// refusedLocalEnv lists, sorted and once each, the variables that raw
// reads through ${localEnv:NAME} or ${env:NAME} and that are not
// allowed. Each resolves to its default value, or empty, as an unset
// variable does.
func refusedLocalEnv(raw []byte, allow []string) []string {
	allowed := make(map[string]bool, len(allow))
	for _, name := range allow {
		allowed[name] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range variablePattern.FindAllSubmatch(raw, -1) {
		parts := strings.Split(string(m[1]), ":")
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
