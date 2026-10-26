package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseGitHubApp(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || cfg.GitHubApp != nil {
		t.Fatalf("absent section: %+v %v", cfg.GitHubApp, err)
	}
	cfg, err = Parse([]byte(valid + "github-app:\n  app-id: 123456\n  transit-key: menagerie-github-app\n  installations:\n    example-org: 7890\n    Another-Org: 12\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := &GitHubApp{AppID: 123456, TransitKey: "menagerie-github-app", Installations: map[string]int64{"example-org": 7890, "Another-Org": 12}, Server: "https://github.com"}
	if !reflect.DeepEqual(cfg.GitHubApp, want) {
		t.Fatalf("github-app %+v", cfg.GitHubApp)
	}
	cfg, err = Parse([]byte(valid + "github-app:\n  app-id: 1\n  transit-key: k\n  server: https://git.example.test\n"))
	if err != nil || cfg.GitHubApp.Server != "https://git.example.test" || cfg.GitHubApp.Installations != nil {
		t.Fatalf("server: %+v %v", cfg.GitHubApp, err)
	}
}

// TestGitHubAppErrorsNameNoValue checks each error of the github-app
// block: it names the field and quotes nothing of what was given.
func TestGitHubAppErrorsNameNoValue(t *testing.T) {
	const marker = "v4lue-marker"
	app := func(body string) string { return valid + "github-app:\n" + body }
	const base = "  app-id: 1\n  transit-key: k\n"
	cases := []struct{ name, doc, want string }{
		{"empty block", app("  {}\n"), "github-app.app-id must be a positive integer"},
		{"no transit key", app("  app-id: 1\n"), "github-app.transit-key is required"},
		{"zero app id", app("  app-id: 0\n  transit-key: k\n"), "github-app.app-id must be a positive integer"},
		{"negative app id", app("  app-id: -4\n  transit-key: k\n"), "github-app.app-id must be a positive integer"},
		{"app id text", app("  app-id: " + marker + "\n  transit-key: k\n"), "github-app.app-id"},
		{"transit key path", app("  app-id: 1\n  transit-key: a/" + marker + "\n"), "github-app.transit-key must be one Transit key name"},
		{"transit key dots", app("  app-id: 1\n  transit-key: ../" + marker + "\n"), "github-app.transit-key must be one Transit key name"},
		{"http server", app(base + "  server: http://" + marker + ".example.test\n"), "github-app.server must be an https URL"},
		{"server path", app(base + "  server: https://" + marker + ".example.test/api\n"), "github-app.server must be an https URL"},
		{"server user", app(base + "  server: https://" + marker + "@example.test\n"), "github-app.server must be an https URL"},
		{"server query", app(base + "  server: https://example.test?" + marker + "\n"), "github-app.server must be an https URL"},
		{"empty server", app(base + "  server: \"\"\n"), ""},
		{"owner shape", app(base + "  installations:\n    " + marker + "_x: 1\n"), "github-app.installations has a key that is not a GitHub account name"},
		{"installation zero", app(base + "  installations:\n    example-org: 0\n"), "github-app.installations has an installation id that is not a positive integer"},
		{"installation text", app(base + "  installations:\n    example-org: " + marker + "\n"), "github-app.installations"},
		{"owner twice", app(base + "  installations:\n    " + marker + ": 1\n    V4LUE-MARKER: 2\n"), "github-app.installations names one account twice"},
		{"unknown field", app(base + "  private-key: " + marker + "\n"), "github-app has no field private-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if tc.want == "" {
				// An empty server takes the default.
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(strings.ToLower(err.Error()), marker) {
				t.Fatalf("got %v, want %q without the value", err, tc.want)
			}
		})
	}
}
