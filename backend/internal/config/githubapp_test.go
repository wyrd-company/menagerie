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
	want := &GitHubApp{Name: DefaultGitHubAppName, AppID: 123456, TransitKey: "menagerie-github-app", Installations: map[string]int64{"example-org": 7890, "Another-Org": 12}, Server: "https://github.com"}
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
		{"owner twice", app(base + "  installations:\n    V4lue-Marker: 1\n    V4LUE-MARKER: 2\n"), "github-app.installations names one account twice"},
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

// TestDecodingErrorsNameNoMapKeyThatIsData: an installation's owner is
// data, so a decoding error names the installations field and the type,
// never the owner; phase names are the document's own and stay named.
func TestDecodingErrorsNameNoMapKeyThatIsData(t *testing.T) {
	const owner = "sentinel-owner"
	app := valid + "github-app:\n  app-id: 1\n  transit-key: k\n  installations:\n"
	for _, doc := range []string{
		app + "    " + owner + ": invalid\n",
		app + "    " + owner + ": [1]\n",
		app + "    " + owner + ": {a: 1}\n",
		valid + "github-app: {app-id: 1, transit-key: k, installations: {" + owner + ": x}}\n",
	} {
		_, err := Parse([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), "github-app.installations must be a whole number") || strings.Contains(err.Error(), owner) {
			t.Errorf("%q: %v", doc, err)
		}
	}
	_, err := Parse([]byte(valid + "job-limits:\n  phases:\n    build: [1h]\n"))
	if err == nil || !strings.Contains(err.Error(), "job-limits.phases.build must be a duration") {
		t.Errorf("phase: %v", err)
	}
}

func TestParseGitHubApps(t *testing.T) {
	cfg, err := Parse([]byte(valid + "github-app:\n  app-id: 1\n  transit-key: k\n"))
	if err != nil || len(cfg.Apps()) != 1 || cfg.Apps()[0].Name != DefaultGitHubAppName {
		t.Fatalf("github-app without a name: %+v %v", cfg.Apps(), err)
	}
	cfg, err = Parse([]byte(valid + "github-app:\n  name: sample-org\n  app-id: 1\n  transit-key: k\n"))
	if err != nil || cfg.Apps()[0].Name != "sample-org" {
		t.Fatalf("github-app with a name: %+v %v", cfg.Apps(), err)
	}
	cfg, err = Parse([]byte(valid + "github-apps:\n  - {name: first-org, app-id: 1, transit-key: first-key}\n  - {name: second-org, app-id: 2, transit-key: second-key, server: https://git.example.test}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []GitHubApp{
		{Name: "first-org", AppID: 1, TransitKey: "first-key", Server: "https://github.com"},
		{Name: "second-org", AppID: 2, TransitKey: "second-key", Server: "https://git.example.test"},
	}
	if !reflect.DeepEqual(cfg.Apps(), want) {
		t.Fatalf("github-apps %+v", cfg.Apps())
	}
	if cfg, err := Parse([]byte(valid + "github-apps: []\n")); err != nil || len(cfg.Apps()) != 0 {
		t.Fatalf("empty github-apps: %+v %v", cfg.Apps(), err)
	}
}

// TestGitHubAppsErrorsNameNoValue checks the errors of github-apps: each
// names the App by its index, and none quotes what was given.
func TestGitHubAppsErrorsNameNoValue(t *testing.T) {
	const marker = "v4lue-marker"
	apps := func(body string) string { return valid + "github-apps:\n" + body }
	cases := []struct{ name, doc, want string }{
		{"both keys", valid + "github-app: {app-id: 1, transit-key: k}\ngithub-apps:\n  - {name: " + marker + ", app-id: 1, transit-key: k}\n", "github-app and github-apps cannot both be given"},
		{"no name", apps("  - {app-id: 1, transit-key: k}\n"), "github-apps[0].name is required"},
		{"name shape", apps("  - {name: a/" + marker + ", app-id: 1, transit-key: k}\n"), "github-apps[0].name must be one name"},
		{"name too long", apps("  - {name: " + strings.Repeat("a", 101) + ", app-id: 1, transit-key: k}\n"), "github-apps[0].name must be one name"},
		{"name twice", apps("  - {name: " + marker + ", app-id: 1, transit-key: k}\n  - {name: V4LUE-MARKER, app-id: 2, transit-key: k}\n"), "github-apps[1].name is the name of an earlier App"},
		{"second app id", apps("  - {name: a, app-id: 1, transit-key: k}\n  - {name: b, app-id: 0, transit-key: k}\n"), "github-apps[1].app-id must be a positive integer"},
		{"transit key", apps("  - {name: a, app-id: 1, transit-key: ../" + marker + "}\n"), "github-apps[0].transit-key must be one Transit key name"},
		{"server", apps("  - {name: a, app-id: 1, transit-key: k, server: http://" + marker + ".example.test}\n"), "github-apps[0].server must be an https URL"},
		{"installations", apps("  - {name: a, app-id: 1, transit-key: k, installations: {example-org: 0}}\n"), "github-apps[0].installations has an installation id"},
		{"github-app name", valid + "github-app: {name: a/" + marker + ", app-id: 1, transit-key: k}\n", "github-app.name must be one name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(strings.ToLower(err.Error()), marker) {
				t.Fatalf("got %v, want %q without the value", err, tc.want)
			}
		})
	}
}
