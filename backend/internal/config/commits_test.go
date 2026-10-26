package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseAutoCommitAndWorkspaceSocket(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || cfg.AutoCommit != nil || cfg.WorkspaceSocket != nil {
		t.Fatalf("absent sections: %+v %+v %v", cfg.AutoCommit, cfg.WorkspaceSocket, err)
	}
	cfg, err = Parse([]byte(valid + "auto-commit:\n  every: 1h30m\nworkspace-socket:\n  folder: /srv/menagerie-sockets\njob-limits:\n  operations:\n    commit: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoCommit.Every.Value() != 90*time.Minute {
		t.Errorf("every %v", cfg.AutoCommit.Every)
	}
	if s := cfg.WorkspaceSocket; s.Folder != "/srv/menagerie-sockets" || s.MinIntervalValue() != DefaultMinInterval {
		t.Errorf("socket %+v %v", s, s.MinIntervalValue())
	}
	if cfg.JobLimits.Operations.Commit.Value() != 10*time.Minute || cfg.JobLimits.Operations.ByName()["commit"] != "10m" {
		t.Errorf("commit limit %+v", cfg.JobLimits.Operations)
	}
	for doc, want := range map[string]time.Duration{"0": 0, "5m": 5 * time.Minute, "30s": 30 * time.Second} {
		cfg, err := Parse([]byte(valid + "workspace-socket:\n  folder: /srv/menagerie-sockets\n  min-interval: " + doc + "\n"))
		if err != nil || cfg.WorkspaceSocket.MinIntervalValue() != want {
			t.Errorf("min-interval %s: %v", doc, err)
		}
	}
}

// TestCommitConfigurationErrorsNameNoValue checks each error of the new
// fields: it names the field and quotes nothing of what was given.
func TestCommitConfigurationErrorsNameNoValue(t *testing.T) {
	const marker = "v4lue-marker"
	socket := func(body string) string { return valid + "workspace-socket:\n" + body }
	cases := []struct{ name, doc, want string }{
		{"every missing", valid + "auto-commit: {}\n", "auto-commit.every is required"},
		{"every empty", valid + "auto-commit:\n  every:\n", "auto-commit.every must be a duration"},
		{"every units", valid + "auto-commit:\n  every: 1" + marker + "\n", "auto-commit.every must be whole hours"},
		{"every zero", valid + "auto-commit:\n  every: 0s\n", "auto-commit.every must be a positive duration"},
		{"every list", valid + "auto-commit:\n  every: [" + marker + "]\n", "auto-commit.every must be a duration such as 45m"},
		{"unknown field", valid + "auto-commit:\n  every: 1h\n  message: x\n", "auto-commit has no field message"},
		{"folder missing", socket("  min-interval: 1m\n"), "workspace-socket.folder is required"},
		{"folder relative", socket("  folder: " + marker + "\n"), "workspace-socket.folder must be an absolute clean folder"},
		{"folder unclean", socket("  folder: /srv/" + marker + "/\n"), "workspace-socket.folder must be an absolute clean folder"},
		{"folder root", socket("  folder: /\n"), "workspace-socket.folder must be an absolute clean folder other than /"},
		{"folder comma", socket("  folder: /srv/a," + marker + "\n"), "workspace-socket.folder must not hold a comma"},
		{"folder in root", socket("  folder: /srv/workspaces/" + marker + "\n"), "workspace-socket.folder must be neither inside workspace-root nor contain it"},
		{"folder holds root", socket("  folder: /srv\n"), "workspace-socket.folder must be neither inside workspace-root nor contain it"},
		{"folder in wyrwood", socket("  folder: /run/wyrwood/containers/example/" + marker + "\n"), "neither inside wyrwood-directory"},
		{"folder candidates", socket("  folder: /var/lib/menagerie\n"), "neither inside template-service.candidate-folder"},
		{"interval units", socket("  folder: /srv/s\n  min-interval: 1" + marker + "\n"), "workspace-socket.min-interval must be whole hours, minutes and seconds, such as 2h, 45m or 1h30m, or 0 for none"},
		{"interval zero unit", socket("  folder: /srv/s\n  min-interval: 0s\n"), "workspace-socket.min-interval must be a positive duration such as 45m, or 0 for none"},
		{"interval empty", socket("  folder: /srv/s\n  min-interval:\n"), "workspace-socket.min-interval must be a duration"},
		{"interval mapping", socket("  folder: /srv/s\n  min-interval: {a: " + marker + "}\n"), "workspace-socket.min-interval must be a duration such as 45m"},
		{"unknown anchor", socket("  folder: *" + marker + "\n"), "an alias names no anchor defined before it"},
		{"unknown anchor in auto-commit", valid + "auto-commit:\n  every: *" + marker + "\n", "an alias names no anchor"},
		{"commit limit", valid + "job-limits:\n  operations:\n    commit: 1" + marker + "\n", "job-limits.operations.commit must be whole hours"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), marker) {
				t.Fatalf("got %v, want %q without the value", err, tc.want)
			}
		})
	}
}

// TestAnchorProblemsNameNoAnchor checks the decoder's texts that quote an
// anchor's name, including one no document of the configuration reaches.
func TestAnchorProblemsNameNoAnchor(t *testing.T) {
	const marker = "v4lue-marker"
	for _, text := range []string{
		"yaml: unknown anchor '" + marker + "' referenced",
		"yaml: anchor '" + marker + "' value contains itself",
		"yaml: line 3: unknown anchor '" + marker + "' referenced",
	} {
		err := decodeError(nil, errors.New(text))
		if !strings.Contains(err.Error(), "an alias names no anchor defined before it, or an anchor holds its own alias") || strings.Contains(err.Error(), marker) {
			t.Errorf("%q became %v", text, err)
		}
	}
}
