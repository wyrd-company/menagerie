package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/wyrwood/operator"
)

type selectionApplication struct {
	data      *store.Store
	importing bool
	importErr error
	applied   chan string
}

func (a *selectionApplication) EnsureImported(ctx context.Context, folder string) error {
	if a.importErr != nil {
		return a.importErr
	}
	if a.importing {
		return a.data.ImportSelection(ctx, "alpha", []string{"one"}, nil, nil)
	}
	return nil
}
func (a *selectionApplication) ApplyPolicies(_ context.Context, folder string, _ devcontainer.Progress) error {
	if a.applied != nil {
		a.applied <- folder
	}
	return nil
}

func TestSelectionHTTPImportsBeforeRevisionAndStoresNothingOnOutage(t *testing.T) {
	f := newFixture(t, nil)
	_ = policyCall(f.server, "GET", "/api/workspaces", "")
	f.server.opts.Policies = &policyFixture{policies: []operator.Policy{{Name: "one"}, {Name: "two"}}}
	if _, err := f.data.Import(context.Background(), 99, func(tx *store.Tx) error { return tx.PrepareSelectionImport("alpha", nil, nil, nil) }); err != nil {
		t.Fatal(err)
	}
	before, err := f.data.Selection(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	app := &selectionApplication{data: f.data, importing: true, importErr: errors.New("controller unavailable")}
	f.server.opts.PolicyApplication = app
	body := fmt.Sprintf(`{"policies":["two"],"revision":%d}`, before.Revision)
	response := policyCall(f.server, "PUT", "/api/workspaces/alpha/policies", body)
	if response.Code != 503 || !strings.Contains(response.Body.String(), "wyrwood-unavailable") {
		t.Fatal(response.Body.String())
	}
	current, err := f.data.Selection(context.Background(), "alpha")
	if err != nil || current.Revision != before.Revision || current.Imported {
		t.Fatalf("%+v %v", current, err)
	}
	app.importErr = nil
	response = policyCall(f.server, "PUT", "/api/workspaces/alpha/policies", body)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "selection-changed") {
		t.Fatal(response.Body.String())
	}
	current, err = f.data.Selection(context.Background(), "alpha")
	if err != nil || len(current.Policies) != 1 || current.Policies[0] != "one" {
		t.Fatalf("%+v %v", current, err)
	}
	if len(f.manager.List()) != 0 {
		t.Fatal("refused edit started a job")
	}
}

func TestSelectionHTTPQueuesAfterBusyJobAndAcceptsLastRemoval(t *testing.T) {
	f := newFixture(t, nil)
	_ = policyCall(f.server, "GET", "/api/workspaces", "")
	f.server.opts.Policies = &policyFixture{policies: []operator.Policy{{Name: "one"}}}
	app := &selectionApplication{data: f.data, applied: make(chan string, 1)}
	f.server.opts.PolicyApplication = app
	before, err := f.data.Selection(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	added, err := f.data.SaveSelection(context.Background(), "alpha", []string{"one"}, before.Revision, []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	defer func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	}()
	if _, err = f.manager.Start("alpha", "start", func(context.Context, *jobs.Run) error { <-hold; return nil }); err != nil {
		t.Fatal(err)
	}
	response := policyCall(f.server, "PUT", "/api/workspaces/alpha/policies", fmt.Sprintf(`{"policies":[],"revision":%d}`, added.Revision))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"jobId":null`) {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	close(hold)
	f.manager.Wait()
	select {
	case <-app.applied:
	default:
		t.Fatal("policy edit was not applied after the active job")
	}
	current, err := f.data.Selection(context.Background(), "alpha")
	if err != nil || len(current.Policies) != 0 || current.Revision != added.Revision+1 {
		t.Fatalf("%+v %v", current, err)
	}
}

func TestCreatePolicyRefusalsHappenBeforeAJob(t *testing.T) {
	f := newFixture(t, nil)
	f.server.opts.Operations[OperationCreate] = func(JobRequest) (jobs.Runner, error) {
		return func(context.Context, *jobs.Run) error { return nil }, nil
	}
	source := &policyFixture{policies: []operator.Policy{{Name: "one"}, {Name: "two"}}}
	f.server.opts.Policies = source
	_, err := f.data.SavePolicySettings(context.Background(), store.PolicySettings{Groups: []store.PolicyGroup{{Name: "SSH", Mode: "single", Policies: []string{"one", "two"}}}}, []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		policies, kind string
		status         int
	}{{`[]`, "policy-choice-required", 422}, {`["one","two"]`, "policy-choice-required", 422}, {`["outside"]`, "policy-outside-grant", 422}, {`["one","one"]`, "invalid-request", 400}} {
		response := policyCall(f.server, "POST", "/api/jobs", `{"operation":"create","workspace":"new","policies":`+tc.policies+`}`)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.kind) {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
	source.fail = errors.New("unavailable")
	if response := policyCall(f.server, "POST", "/api/jobs", `{"operation":"create","workspace":"new","policies":["one"]}`); response.Code != 503 || !strings.Contains(response.Body.String(), "wyrwood-unavailable") {
		t.Fatal(response.Body.String())
	}
	source.fail = nil
	f.server.opts.Provisioned = true
	if response := policyCall(f.server, "POST", "/api/jobs", `{"operation":"create","workspace":"Uppercase","policies":["one"]}`); response.Code != 400 || !strings.Contains(response.Body.String(), "invalid-workspace-name") {
		t.Fatal(response.Body.String())
	}
	f.server.opts.Policies = nil
	if response := policyCall(f.server, "POST", "/api/jobs", `{"operation":"create","workspace":"new","policies":["one"]}`); response.Code != 409 || !strings.Contains(response.Body.String(), "provisioning-unavailable") {
		t.Fatal(response.Body.String())
	}
	if len(f.manager.List()) != 0 {
		t.Fatal("refused create started a job")
	}
}
func TestCreateWithNoPoliciesIsAccepted(t *testing.T) {
	f := newFixture(t, nil)
	f.server.opts.Policies = &policyFixture{policies: []operator.Policy{{Name: "one"}}}
	f.server.opts.Operations[OperationCreate] = func(JobRequest) (jobs.Runner, error) {
		return func(context.Context, *jobs.Run) error { return nil }, nil
	}
	if response := policyCall(f.server, "POST", "/api/jobs", `{"operation":"create","workspace":"new","policies":[]}`); response.Code != 202 {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	f.manager.Wait()
}

func TestSelectionHTTPDoesNotRequireListingTheFolderFirst(t *testing.T) {
	f := newFixture(t, nil)
	f.server.opts.Policies = &policyFixture{policies: []operator.Policy{{Name: "one"}}}
	f.server.opts.PolicyApplication = &selectionApplication{data: f.data}
	response := policyCall(f.server, "PUT", "/api/workspaces/alpha/policies", `{"policies":["one"],"revision":0}`)
	if response.Code != 200 {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	f.manager.Wait()
	current, err := f.data.Selection(context.Background(), "alpha")
	if err != nil || len(current.Policies) != 1 || current.Policies[0] != "one" {
		t.Fatalf("%+v %v", current, err)
	}
}

func TestStartupRetriesARevisionWhoseLaterBindingAttemptFailed(t *testing.T) {
	f := newFixture(t, nil)
	_ = policyCall(f.server, "GET", "/api/workspaces", "")
	ctx := context.Background()
	before, err := f.data.Selection(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.data.Applied(ctx, "alpha", before.Revision); err != nil {
		t.Fatal(err)
	}
	app := &selectionApplication{data: f.data}
	f.server.opts.PolicyApplication = app
	// The already-applied revision has no work at startup.
	if err = f.server.ApplyPending(ctx); err != nil {
		t.Fatal(err)
	}
	f.manager.Wait()
	for _, job := range f.manager.List() {
		if job.Workspace == "alpha" {
			t.Fatal("applied revision was retried")
		}
	}
	if err = f.data.ApplyFailed(ctx, "alpha", before.Revision, store.ApplyFailure{Kind: "binding-refused", Message: "choose a policy"}); err != nil {
		t.Fatal(err)
	}
	if err = f.server.ApplyPending(ctx); err != nil {
		t.Fatal(err)
	}
	f.manager.Wait()
	for _, job := range f.manager.List() {
		if job.Workspace == "alpha" && job.Operation == OperationApplyPolicies {
			return
		}
	}
	t.Fatal("startup skipped the failed revision")
}

func TestSettingsApplySkipsWorkspacesWithNoFolder(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	_ = policyCall(f.server, "GET", "/api/workspaces", "")
	if err := f.data.HoldName(ctx, "gone", "container-gone", "host", true); err != nil {
		t.Fatal(err)
	}
	f.server.opts.Policies = &policyFixture{policies: []operator.Policy{{Name: "one"}}}
	f.server.opts.PolicyApplication = &selectionApplication{data: f.data}
	response := policyCall(f.server, "PUT", "/api/policies/settings", `{"required":["one"],"groups":[],"revision":0}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	f.manager.Wait()
	jobs := f.manager.List()
	if len(jobs) != 3 {
		t.Fatalf("want three folder workspaces only, got %+v", jobs)
	}
	for _, job := range jobs {
		if job.Workspace == "gone" || job.State != "succeeded" || job.Operation != OperationApplyPolicies {
			t.Fatalf("unnecessary or failed apply: %+v", job)
		}
	}
	gone, err := f.data.Selection(ctx, "gone")
	if err != nil || len(gone.Policies) != 1 || gone.Policies[0] != "one" || gone.AppliedRevision != nil {
		t.Fatalf("no-folder selection: %+v %v", gone, err)
	}
}
