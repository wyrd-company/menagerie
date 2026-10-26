package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/alerts"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// alertSink is a webhook that hands each alert body to the test.
func alertSink(t *testing.T) (string, <-chan alerts.Payload) {
	t.Helper()
	got := make(chan alerts.Payload, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var p alerts.Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		got <- p
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/hook", got
}

// postJob starts a job through the API and returns its snapshot.
func postJob(t *testing.T, base, body string) jobs.Snapshot {
	t.Helper()
	resp, err := http.Post(base+"/api/jobs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("job request answered %d %s", resp.StatusCode, data)
	}
	var snap jobs.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

// awaitEnd reads GET /api/jobs/{id} until the job has ended.
func awaitEnd(t *testing.T, base, id string) jobs.Snapshot {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		resp, err := http.Get(base + "/api/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var snap jobs.Snapshot
		err = json.NewDecoder(resp.Body).Decode(&snap)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if snap.State.Terminal() {
			return snap
		}
	}
	t.Fatalf("job %s did not end", id)
	return jobs.Snapshot{}
}

// TestAJobPastItsLimitTimesOutThroughTheAPI runs a start whose
// initializeCommand hangs, with a 100 ms initialize-command limit, through
// the composed server: the job fails as timed-out, the webhook hears of
// it, and the workspace takes the next job.
func TestAJobPastItsLimitTimesOutThroughTheAPI(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "alpha")
	if err := os.MkdirAll(filepath.Join(folder, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	definition := `{"image": "alpine", "initializeCommand": ["sleep", "30"]}`
	if err := os.WriteFile(filepath.Join(folder, ".devcontainer", "devcontainer.json"), []byte(definition), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	hook, received := alertSink(t)
	webhook, err := alerts.New(alerts.Options{URL: hook, Origin: "https://menagerie.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(webhook.Close)
	ctx, cancel := context.WithCancel(context.Background())
	manager := jobs.NewManager(ctx, jobs.Options{
		Limits: jobs.Limits{Phases: map[string]time.Duration{"initialize-command": 100 * time.Millisecond}},
		Alerts: webhook,
	})
	t.Cleanup(manager.Wait)
	t.Cleanup(cancel)
	cfg := &config.Config{WorkspaceRoot: dir, TemplateService: config.TemplateService{Socket: filepath.Join(dir, "absent.sock")}}
	server := httptest.NewServer(newServer(cfg, root, devcontainer.New(nil, devcontainer.Options{}), manager))
	t.Cleanup(server.Close)

	for range 2 {
		started := time.Now()
		snap := awaitEnd(t, server.URL, postJob(t, server.URL, `{"operation":"start","workspace":"alpha"}`).JobID)
		want := "the initialize-command phase passed its 100ms limit"
		if snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != jobs.KindTimedOut || snap.Error.Message != want {
			t.Fatalf("snapshot %+v error %+v", snap, snap.Error)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("the cancelled command held the job for %v", elapsed)
		}
		select {
		case p := <-received:
			if p.Event != alerts.EventJobFailed || p.JobID != snap.JobID || p.Workspace != "alpha" || p.Operation != "start" ||
				p.Phase != "initialize-command" || p.Error == nil || p.Error.Kind != jobs.KindTimedOut ||
				p.Console != "https://menagerie.example.test/?job="+snap.JobID {
				t.Fatalf("alert %+v", p)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no alert")
		}
	}
}

// TestJobOptionsCarryTheConfiguration checks that job-limits and alerts
// reach the jobs manager, and that console links use the browser origin.
func TestJobOptionsCarryTheConfiguration(t *testing.T) {
	opts, webhook, err := jobOptions(&config.Config{OIDC: config.OIDC{RedirectURL: "https://menagerie.example.test/auth/callback"}})
	if err != nil || webhook != nil || opts.Alerts != nil || opts.Limits.Default != 0 || opts.LongRunning != 0 {
		t.Fatalf("absent sections: %+v %v %v", opts, webhook, err)
	}
	hook, received := alertSink(t)
	cfg := &config.Config{
		OIDC: config.OIDC{RedirectURL: "https://menagerie.example.test/auth/callback"},
		JobLimits: &config.JobLimits{
			Default:    "2h",
			Operations: config.OperationLimits{Rebuild: "1h", Upgrade: "30m"},
			Phases:     map[string]config.Duration{"build": "45m"},
		},
		Alerts: &config.Alerts{Webhook: config.WebhookURL(hook), LongRunning: "45m"},
	}
	opts, webhook, err = jobOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(webhook.Close)
	l := opts.Limits
	if l.Default != 2*time.Hour || len(l.Operations) != 2 || l.Operations["rebuild"] != time.Hour || l.Operations["upgrade"] != 30*time.Minute ||
		len(l.Phases) != 1 || l.Phases["build"] != 45*time.Minute || opts.LongRunning != 45*time.Minute {
		t.Fatalf("options %+v", opts)
	}
	if opts.Alerts != jobs.Alerts(webhook) {
		t.Fatal("alerts are not the webhook")
	}
	webhook.JobLongRunning(jobs.Snapshot{JobID: "j1", Workspace: "beta", Operation: "upgrade", State: jobs.Running})
	if p := <-received; p.Console != "https://menagerie.example.test/workspaces/beta/upgrade?job=j1" {
		t.Fatalf("console %s", p.Console)
	}
}
