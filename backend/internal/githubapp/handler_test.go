package githubapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func post(t *testing.T, h http.Handler, method, body string) (int, map[string]string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, CredentialPath, strings.NewReader(body)))
	var doc map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("answer %q is not a JSON object of strings", rec.Body.String())
	}
	return rec.Code, doc, rec.Header()
}

func TestTheCredentialRouteAnswersAReadTokenForOneRepository(t *testing.T) {
	g := newGitHub(t, newClock())
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	for _, url := range []string{g.server.URL + "/example-org/sample-repo", g.server.URL + "/example-org/sample-repo.git"} {
		status, doc, header := post(t, Handler(m), http.MethodPost, `{"url":"`+url+`"}`)
		if status != http.StatusOK || doc["username"] != "x-access-token" || !strings.HasPrefix(doc["password"], "sample-token-") || len(doc) != 2 {
			t.Fatalf("%s: %d %v", url, status, doc)
		}
		if header.Get("Cache-Control") != "no-store" || header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers %v", header)
		}
	}
	for _, r := range g.seen() {
		want := map[string]any{"repositories": []any{"sample-repo"}, "permissions": map[string]any{"contents": "read"}}
		if !reflect.DeepEqual(r.body, want) {
			t.Fatalf("token request %v", r.body)
		}
	}
	// The second URL names the same repository: one token.
	if g.count(http.MethodPost) != 1 {
		t.Fatalf("token requests %d", g.count(http.MethodPost))
	}
}

func TestTheCredentialRouteRefusals(t *testing.T) {
	g := newGitHub(t, newClock())
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	origin := g.server.URL
	cases := []struct {
		name    string
		minter  *Minter
		method  string
		body    string
		status  int
		reason  string
		noValue string
	}{
		{"no github-app", nil, http.MethodPost, `{"url":"https://github.com/example-org/sample-repo"}`, http.StatusNotFound, "no GitHub App is configured", ""},
		{"another host", m, http.MethodPost, `{"url":"https://elsewhere.example.invalid/example-org/sample-repo"}`, http.StatusNotFound, "the URL is not an https repository URL on the GitHub App's server", "elsewhere"},
		{"http", m, http.MethodPost, `{"url":"` + strings.Replace(origin, "https:", "http:", 1) + `/example-org/sample-repo"}`, http.StatusNotFound, "the URL is not an https repository URL on the GitHub App's server", "sample-repo"},
		{"user information", m, http.MethodPost, `{"url":"` + strings.Replace(origin, "https://", "https://someone@", 1) + `/example-org/sample-repo"}`, http.StatusNotFound, "the URL is not an https repository URL on the GitHub App's server", "someone"},
		{"deeper path", m, http.MethodPost, `{"url":"` + origin + `/example-org/sample-repo/info/refs"}`, http.StatusNotFound, "the URL is not an https repository URL on the GitHub App's server", "info"},
		{"GET", m, http.MethodGet, ``, http.StatusMethodNotAllowed, "the route accepts POST only", ""},
		{"unknown field", m, http.MethodPost, `{"url":"` + origin + `/example-org/sample-repo","permission":"write"}`, http.StatusBadRequest, "the request is not a credential request", "write"},
		{"trailing value", m, http.MethodPost, `{"url":"` + origin + `/example-org/sample-repo"} {}`, http.StatusBadRequest, "the request is not a credential request", ""},
		{"no url", m, http.MethodPost, `{}`, http.StatusBadRequest, "the request is not a credential request", ""},
		{"not an object", m, http.MethodPost, `"` + origin + `"`, http.StatusBadRequest, "the request is not a credential request", ""},
		{"too large", m, http.MethodPost, `{"url":"` + origin + `/` + strings.Repeat("a", maxCredentialRequest) + `/r"}`, http.StatusBadRequest, "the request is not a credential request", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, doc, _ := post(t, Handler(c.minter), c.method, c.body)
			if status != c.status || doc["reason"] != c.reason || len(doc) != 1 {
				t.Fatalf("%d %v", status, doc)
			}
			if c.noValue != "" && strings.Contains(doc["reason"], c.noValue) {
				t.Fatalf("the reason names the value: %v", doc)
			}
		})
	}
	if len(g.seen()) != 0 {
		t.Fatalf("a refusal reached GitHub: %+v", g.seen())
	}
}

func TestTheCredentialRouteAnswers502WithAFixedReason(t *testing.T) {
	g := newGitHub(t, newClock())
	g.answer = func(w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, sentinelBody, http.StatusUnprocessableEntity)
		return true
	}
	m, _ := g.minter(t, map[string]int64{"example-org": sampleInstallation})
	status, doc, _ := post(t, Handler(m), http.MethodPost, `{"url":"`+g.server.URL+`/example-org/sample-repo"}`)
	if status != http.StatusBadGateway || doc["reason"] != "GitHub refused the token: the repository is not in the installation, or the permission is not granted" || len(doc) != 1 {
		t.Fatalf("%d %v", status, doc)
	}
}
