package devcontainer

import (
	"slices"
	"testing"

	"github.com/wyrd-company/devcontainer-builder/pkg/substitute"
)

func TestLocalEnvironmentKeepsOnlyAllowedVariables(t *testing.T) {
	environ := []string{"PATH=/usr/bin:/bin", "ALLOWED_VALUE=a=b", "SECRET_VALUE=s", "EMPTY_ALLOWED=", "malformed"}
	env := newLocalEnvironment(environ, []string{"ALLOWED_VALUE", "EMPTY_ALLOWED", "UNSET_ALLOWED"})
	if len(env.values) != 2 || env.values["ALLOWED_VALUE"] != "a=b" || env.values["EMPTY_ALLOWED"] != "" {
		t.Fatalf("values %v", env.values)
	}
	if _, ok := env.values["PATH"]; ok {
		t.Fatal("PATH is readable through ${localEnv:PATH} without being allowed")
	}
	if want := []string{"PATH=/usr/bin:/bin", "ALLOWED_VALUE=a=b", "EMPTY_ALLOWED="}; !slices.Equal(env.process, want) {
		t.Fatalf("process %q want %q", env.process, want)
	}
	if env := newLocalEnvironment(environ, nil); len(env.values) != 0 || !slices.Equal(env.process, []string{"PATH=/usr/bin:/bin"}) {
		t.Fatalf("nil allowlist: %v %q", env.values, env.process)
	}
	if env := newLocalEnvironment(environ, []string{"PATH"}); env.values["PATH"] != "/usr/bin:/bin" || len(env.process) != 1 {
		t.Fatalf("allowed PATH: %v %q", env.values, env.process)
	}
}

func TestRefusedLocalEnvNamesEachRefusedReference(t *testing.T) {
	raw := []byte(`{
  // ${localEnv:IN_COMMENT} is still read by substitution.
  "containerEnv": {
    "A": "${localEnv:ALLOWED_VALUE}",
    "B": "${localEnv:SECRET_VALUE}",
    "C": "${env:OTHER_SECRET:fallback}",
    "D": "${localEnv:SECRET_VALUE:x}",
    "E": "${containerEnv:NOT_LOCAL}",
    "F": "${localEnv}",
    "G": "${localWorkspaceFolder}"
  }
}`)
	got := refusedLocalEnv(raw, []string{"ALLOWED_VALUE"})
	if want := []string{"IN_COMMENT", "OTHER_SECRET", "SECRET_VALUE"}; !slices.Equal(got, want) {
		t.Fatalf("refused %q want %q", got, want)
	}
	if got := refusedLocalEnv(raw, []string{"ALLOWED_VALUE", "IN_COMMENT", "OTHER_SECRET", "SECRET_VALUE"}); len(got) != 0 {
		t.Fatalf("all allowed, refused %q", got)
	}
}

// TestRefusedLocalEnvAgreesWithSubstitution checks the reference scan
// against devcontainer-builder's substitution: a string changes under the
// filtered environment exactly when it reads a refused variable.
func TestRefusedLocalEnvAgreesWithSubstitution(t *testing.T) {
	environ := []string{"PATH=/bin", "ALLOWED_VALUE=a", "SECRET_VALUE=s", "OTHER_SECRET=o"}
	allow := []string{"ALLOWED_VALUE"}
	full := substitute.Context{LocalEnv: map[string]string{"PATH": "/bin", "ALLOWED_VALUE": "a", "SECRET_VALUE": "s", "OTHER_SECRET": "o"}}
	filtered := substitute.Context{LocalEnv: newLocalEnvironment(environ, allow).values}
	corpus := []string{
		"${localEnv:ALLOWED_VALUE}",
		"${localEnv:SECRET_VALUE}",
		"${env:SECRET_VALUE}",
		"${localEnv:SECRET_VALUE:default}",
		"prefix ${localEnv:OTHER_SECRET} suffix",
		"${localEnv:PATH}",
		"${localEnv:UNSET_VALUE}",
		"${containerEnv:SECRET_VALUE}",
		"${localEnv:ALLOWED_VALUE}${localEnv:SECRET_VALUE}",
		"$${localEnv:SECRET_VALUE}",
		"${localEnv}",
	}
	for _, s := range corpus {
		changed := substitute.Substitute(full, s) != substitute.Substitute(filtered, s)
		reads := false
		for _, name := range refusedLocalEnv([]byte(s), allow) {
			if _, set := full.LocalEnv[name]; set {
				reads = true
			}
		}
		if changed != reads {
			t.Errorf("%q: substitution changed %t, scan reports a set refused variable %t", s, changed, reads)
		}
	}
}
