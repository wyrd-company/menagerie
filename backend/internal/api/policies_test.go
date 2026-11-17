package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/wyrwood/operator"
)

type policyFixture struct {
	calls    int
	fail     error
	policies []operator.Policy
}

func (f *policyFixture) Policies(context.Context) (operator.Grant, []operator.Policy, error) {
	f.calls++
	return operator.Grant{Agents: []string{"host"}, Policies: []string{"*"}, Actions: []operator.Action{operator.ActionRead}}, f.policies, f.fail
}
func policyCall(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRecorder()
	s.ServeHTTP(r, httptest.NewRequest(method, path, strings.NewReader(body)))
	return r
}

func TestPolicyHTTPReadsTheGrantAfreshAndShowsLostPolicies(t *testing.T) {
	f := newFixture(t, nil)
	source := &policyFixture{policies: []operator.Policy{{Name: "one", ValueNames: []string{"identity"}}, {Name: "two"}}}
	f.server.opts.Policies = source
	response := policyCall(f.server, "PUT", "/api/policies/settings", `{"required":["one"],"groups":[{"name":"SSH","mode":"multi","policies":["two"]}],"revision":0}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = policyCall(f.server, "GET", "/api/workspaces", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"policies":["one"]`) || !strings.Contains(response.Body.String(), `"needsChoice":["SSH"]`) {
		t.Fatal(response.Body.String())
	}
	source.policies = []operator.Policy{{Name: "two"}}
	response = policyCall(f.server, "GET", "/api/policies", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"name":"one","valueNames":[],"inGrant":false`) {
		t.Fatal(response.Body.String())
	}
	// A previously assigned policy outside the grant can be retained or removed.
	response = policyCall(f.server, "PUT", "/api/policies/settings", `{"required":["one"],"groups":[],"revision":1}`)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if source.calls != 3 {
		t.Fatalf("grant read %d times, want 3", source.calls)
	}
	if len(f.manager.List()) != 0 {
		t.Fatal("settings started a job")
	}
}

func TestPolicyHTTPUnavailableAndRefusals(t *testing.T) {
	f := newFixture(t, nil)
	response := policyCall(f.server, "GET", "/api/policies", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"grant":null`) {
		t.Fatal(response.Body.String())
	}
	response = policyCall(f.server, "PUT", "/api/policies/settings", `{"required":[],"groups":[],"revision":0}`)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "provisioning-unavailable") {
		t.Fatal(response.Body.String())
	}
	source := &policyFixture{policies: []operator.Policy{{Name: "one"}}}
	f.server.opts.Policies = source
	for _, tc := range []struct {
		body, kind string
		status     int
	}{
		{`{"required":["missing","outside"],"groups":[],"revision":0}`, "policy-outside-grant", 422},
		{`{"required":[],"groups":[{"name":"SSH","mode":"other","policies":["one"]}],"revision":0}`, "invalid-settings", 422},
		{`{"required":[],"groups":[{"id":0,"name":"SSH","mode":"single","policies":["one"]}],"revision":0}`, "invalid-settings", 422},
		{`{"required":[],"groups":[],"revision":1}`, "settings-changed", 409},
		{`{"required":[],"groups":[]}`, "invalid-request", 400},
		{`{"required":[],"groups":[],"revision":0} {}`, "invalid-request", 400},
	} {
		response = policyCall(f.server, "PUT", "/api/policies/settings", tc.body)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.kind) {
			t.Fatalf("%s: %d %s", tc.body, response.Code, response.Body.String())
		}
		if tc.kind == "policy-outside-grant" && (!strings.Contains(response.Body.String(), "missing") || !strings.Contains(response.Body.String(), "outside")) {
			t.Fatal(response.Body.String())
		}
	}
	source.fail = errors.New("controller unavailable")
	for _, method := range []string{"GET", "PUT"} {
		path := "/api/policies"
		if method == "PUT" {
			path += "/settings"
		}
		response = policyCall(f.server, method, path, `{"required":[],"groups":[],"revision":0}`)
		if response.Code != 503 || !strings.Contains(response.Body.String(), "wyrwood-unavailable") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	if len(f.manager.List()) != 0 {
		t.Fatal("refused settings started a job")
	}
}

func TestWorkspacePoliciesIncludeEmptySelection(t *testing.T) {
	f := newFixture(t, nil)
	r := policyCall(f.server, "GET", "/api/workspaces", "")
	var body struct {
		Workspaces []struct {
			Policies    []string `json:"policies"`
			NeedsChoice []string `json:"needsChoice"`
		}
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, row := range body.Workspaces {
		if row.Policies == nil || row.NeedsChoice == nil {
			t.Fatal("missing empty lists")
		}
	}
}

func TestPolicyHTTPExposesOutsideSelectionEvenAfterSettingsRemoval(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if _, err := f.data.Discover(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.data.SavePolicySettings(ctx, store.PolicySettings{Required: []string{"lost"}}, []string{"lost"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.data.SavePolicySettings(ctx, store.PolicySettings{Revision: 1}, nil); err != nil {
		t.Fatal(err)
	}
	f.server.opts.Policies = &policyFixture{}
	r := policyCall(f.server, "GET", "/api/policies", "")
	if !strings.Contains(r.Body.String(), `"name":"lost","valueNames":[],"inGrant":false`) {
		t.Fatal(r.Body.String())
	}
}
