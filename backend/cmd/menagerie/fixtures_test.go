//go:build pocketid

package main

// Fixtures the Pocket ID-backed suites share: required settings, test Git
// repositories and the generic two-version sample template.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is required", name)
	}
	return v
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Example User", "GIT_AUTHOR_EMAIL=user@example.invalid",
		"GIT_COMMITTER_NAME=Example User", "GIT_COMMITTER_EMAIL=user@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeTo(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// browserTemplate writes version 1 or 2 of the generic sample template.
// Version 2 adds one required question and changes one generated line.
// The hook and the devcontainer's initializeCommand leave markers.
func browserTemplate(t *testing.T, dir string, version int) {
	region, regionLine := "", ""
	if version == 2 {
		region = "  - {id: region, type: select, prompt: Region, description: Added by version 2 of the sample template., options: [north, south], required: true}\n"
		regionLine = "region: {{ region }}\n"
	}
	writeTo(t, dir, "template.yml", "name: sample-kit\ninterview:\n"+
		"  - {id: label, type: text, prompt: Label, description: Shown in result.txt., placeholder: Sample label, required: true, validate: {min: 3, max: 40}}\n"+
		"  - {id: size, type: select, prompt: Size, options: [small, large], default: small}\n"+
		"  - {id: verbose, type: confirm, prompt: Verbose logging?, default: false}\n"+
		"  - {id: tags, type: multiselect, prompt: Tags, options: [alpha, beta, gamma], default: [alpha]}\n"+
		region+
		"  - message: \"Sample kit for {{ label }}.\"\n"+
		"hooks:\n  - run: [sh, -c, 'echo ran > hook-marker.txt']\n")
	writeTo(t, dir, "template/result.txt", "label: {{ label }}\nsize: {{ size }}\n"+regionLine)
	writeTo(t, dir, "template/local.txt", fmt.Sprintf("first: template\nsecond: template\nthird: stable\nfourth: stable\nfifth: v%d\n", version))
	writeTo(t, dir, "template/.gitignore", "/markers/\n")
	writeTo(t, dir, "template/.devcontainer/devcontainer.json", `{
  "name": "sample",
  "image": "busybox:1.37",
  "initializeCommand": "mkdir -p markers && echo initialized > markers/initialize.txt"
}
`)
}
