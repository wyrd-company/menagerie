package devcontainer

import (
	"context"
	"slices"
	"testing"

	"github.com/wyrd-company/devcontainer-builder/pkg/spec"
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
	// PATH is the runner image's own, never Menagerie's.
	if want := []string{"ALLOWED_VALUE=a=b", "EMPTY_ALLOWED="}; !slices.Equal(env.process, want) {
		t.Fatalf("process %q want %q", env.process, want)
	}
	if env := newLocalEnvironment(environ, nil); len(env.values) != 0 || env.process == nil || len(env.process) != 0 {
		t.Fatalf("nil allowlist: %v %q", env.values, env.process)
	}
	if env := newLocalEnvironment(environ, []string{"PATH"}); env.values["PATH"] != "/usr/bin:/bin" || len(env.process) != 1 {
		t.Fatalf("allowed PATH: %v %q", env.values, env.process)
	}
}

func TestRefusedLocalEnvNamesEachRefusedReference(t *testing.T) {
	raw := []byte(`{
  "containerEnv": {
    "A": "${localEnv:ALLOWED_VALUE}",
    "B": "${localEnv:SECRET_VALUE}",
    "C": "${env:OTHER_SECRET:fallback}",
    "D": "${localEnv:SECRET_VALUE:x}",
    "E": "${containerEnv:NOT_LOCAL}",
    "F": "${localEnv}",
    "G": "${localWorkspaceFolder}",
    "${localEnv:IN_KEY}": "keys are not substituted"
  },
  "runArgs": ["--label", "x=${localEnv:IN_ARRAY}"]
}`)
	got := refusedLocalEnv(raw, []string{"ALLOWED_VALUE"})
	if want := []string{"IN_ARRAY", "OTHER_SECRET", "SECRET_VALUE"}; !slices.Equal(got, want) {
		t.Fatalf("refused %q want %q", got, want)
	}
	if got := refusedLocalEnv(raw, []string{"ALLOWED_VALUE", "IN_ARRAY", "OTHER_SECRET", "SECRET_VALUE"}); len(got) != 0 {
		t.Fatalf("all allowed, refused %q", got)
	}
	if got := refusedLocalEnv([]byte(`not json ${localEnv:SECRET_VALUE}`), nil); !slices.Equal(got, []string{"SECRET_VALUE"}) {
		t.Fatalf("undecodable input, refused %q", got)
	}
}

// TestRefusedLocalEnvReadsTheLoadedDefinition scans dc.Raw from the
// definition-loading path: comments are gone and JSON escapes decode to
// the reference that substitution sees.
func TestRefusedLocalEnvReadsTheLoadedDefinition(t *testing.T) {
	dc, _ := writeDefinition(t, `{
  // ${localEnv:IN_COMMENT} is never substituted.
  "image": "alpine:3.22",
  "containerEnv": {
    "ESCAPED_DOLLAR": "\u0024{localEnv:ESCAPED_SECRET}",
    "ESCAPED_NAME": "${localEnv:ALLOWED\u005fVALUE}",
    "ESCAPED_REFUSED": "${localEnv:OTHER\u005fSECRET}"
  }
}`)
	got := refusedLocalEnv(dc.Raw, []string{"ALLOWED_VALUE"})
	if want := []string{"ESCAPED_SECRET", "OTHER_SECRET"}; !slices.Equal(got, want) {
		t.Fatalf("refused %q want %q", got, want)
	}
}

// TestInitializeEnvironmentInheritsNothing runs a command with the
// environment of an empty allowlist: the container gets an empty
// environment, never a nil one.
func TestInitializeEnvironmentInheritsNothing(t *testing.T) {
	env := newLocalEnvironment([]string{"MENAGERIE_TEST_SECRET=secret-value", "PATH=/menagerie/bin"}, nil)
	if env.process == nil || len(env.process) != 0 {
		t.Fatalf("process %#v", env.process)
	}
	runner, l := newRunner(t.TempDir(), env.process, nil)
	if err := runner.Run(context.Background(), spec.Command{Args: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if c := l.commands[0]; c.Env == nil || len(c.Env) != 0 {
		t.Fatalf("command environment %#v", c.Env)
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
