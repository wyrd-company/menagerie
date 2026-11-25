package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/backuprepos"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

type backupBody struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Workspaces []string `json:"workspaces"`
}

// A backup repository is checked, registered, listed with the workspaces
// that back up to it, shown on their rows, and removed once none does.
func TestABackupRepositoryIsRegisteredListedAndRemovedThroughTheAPI(t *testing.T) {
	f := newFixture(t, nil)
	host := backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(host.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	f.server.opts.Backups = &backuprepos.Registry{Data: f.data, Auth: backup.AgentAuth(host.AgentPath, host.KnownHosts)}
	url := host.URL("backups.git")

	resp, data := f.do(t, "POST", "/api/backup-repositories", `{"name":"Example backups","url":"`+url+`","check":true}`)
	if checked := decode[backupBody](t, data); resp.StatusCode != http.StatusOK || checked.ID != "example-backups" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "POST", "/api/backup-repositories", `{"name":"Example backups","url":"`+url+`"}`)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/api/backup-repositories/example-backups" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "POST", "/api/backup-repositories", `{"name":"Again","url":"`+url+`"}`)
	if body := decode[checkBody](t, data); resp.StatusCode != http.StatusConflict || body.Error.Kind != KindBackupRepositoryExists {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	for body, want := range map[string]string{
		`{"name":"Local","url":"/srv/backups.git"}`:                "url",
		`{"name":"Absent","url":"` + host.URL("absent.git") + `"}`: "write",
	} {
		resp, data = f.do(t, "POST", "/api/backup-repositories", body)
		if got := decode[checkBody](t, data); resp.StatusCode != http.StatusUnprocessableEntity || got.Error.Kind != KindBackupCheckFailed || got.Error.Check != want {
			t.Errorf("%s: %d %s", body, resp.StatusCode, data)
		}
	}
	for _, body := range []string{`{"name":"","url":"` + url + `"}`, `{"url":"` + url + `"}`, `{"name":"x","url":"` + url + `","extra":1}`} {
		if resp, data = f.do(t, "POST", "/api/backup-repositories", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, resp.StatusCode, data)
		}
	}

	if err := f.data.SetWorkspaceBackupRepository(context.Background(), "alpha", "example-backups"); err != nil {
		t.Fatal(err)
	}
	_, data = f.do(t, "GET", "/api/backup-repositories", "")
	var list struct{ BackupRepositories []backupBody }
	if err := json.Unmarshal(data, &list); err != nil || len(list.BackupRepositories) != 1 || strings.Join(list.BackupRepositories[0].Workspaces, ",") != "alpha" {
		t.Fatalf("%s %v", data, err)
	}
	_, data = f.do(t, "GET", "/api/workspaces", "")
	if !strings.Contains(string(data), `"backupRepository":{"id":"example-backups","name":"Example backups","url":"`+url+`"},`) {
		t.Fatalf("%s", data)
	}
	resp, data = f.do(t, "DELETE", "/api/backup-repositories/example-backups", "")
	if body := decode[checkBody](t, data); resp.StatusCode != http.StatusConflict || body.Error.Kind != KindBackupRepositoryInUse || !strings.Contains(body.Error.Message, "alpha") {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if err := f.data.RemoveWorkspace(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if resp, _ = f.do(t, "DELETE", "/api/backup-repositories/example-backups", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%d", resp.StatusCode)
	}
	resp, data = f.do(t, "DELETE", "/api/backup-repositories/example-backups", "")
	if body := decode[checkBody](t, data); resp.StatusCode != http.StatusNotFound || body.Error.Kind != KindUnknownBackupRepository {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
}

// A repository the import took over with a password in its URL is listed,
// and shown on its workspaces' rows, without it.
func TestRepliesNameNoBackupCredential(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	url := "https://sentinel@git.example.invalid/example-org/backups.git?token=sentinel"
	r, err := f.data.RegisterBackupRepository(ctx, store.BackupRepository{Name: "Imported", URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.data.SetWorkspaceBackupRepository(ctx, "alpha", r.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/backup-repositories", "/api/workspaces"} {
		_, data := f.do(t, "GET", path, "")
		if strings.Contains(string(data), "sentinel") || !strings.Contains(string(data), `"url":"https://git.example.invalid/example-org/backups.git"`) {
			t.Errorf("%s: %s", path, data)
		}
	}
}
