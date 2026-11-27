package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/backuprepos"
	"github.com/wyrd-company/menagerie/backend/internal/gitcreds"
	"github.com/wyrd-company/menagerie/backend/internal/githubapp"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

type backupBody struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	URL        string            `json:"url"`
	Credential *BackupCredential `json:"credential"`
	Workspaces []string          `json:"workspaces"`
}

// A backup repository is checked, registered, listed with the workspaces
// that back up to it, shown on their rows, and removed once none does.
func TestABackupRepositoryIsRegisteredListedAndRemovedThroughTheAPI(t *testing.T) {
	f := newFixture(t, nil)
	host := backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(host.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	f.server.opts.Backups = &backuprepos.Registry{Data: f.data, Credentials: &gitcreds.Source{Socket: host.AgentPath, KnownHosts: []string{host.KnownHosts}}}
	url := host.URL("backups.git")
	key := `{"sshKey":"` + backuptest.AuthorizedKey(host.UserKey) + `"}`

	resp, data := f.do(t, "POST", "/api/backup-repositories", `{"name":"Example backups","url":"`+url+`","credential":`+key+`,"check":true}`)
	if checked := decode[backupBody](t, data); resp.StatusCode != http.StatusOK || checked.ID != "example-backups" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "POST", "/api/backup-repositories", `{"name":"Example backups","url":"`+url+`","credential":`+key+`}`)
	saved := decode[backupBody](t, data)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/api/backup-repositories/example-backups" ||
		saved.Credential == nil || saved.Credential.SSHKey != backuptest.AuthorizedKey(host.UserKey) || saved.Credential.Fingerprint != ssh.FingerprintSHA256(host.UserKey) {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "POST", "/api/backup-repositories", `{"name":"Again","url":"`+url+`","credential":`+key+`}`)
	if body := decode[checkBody](t, data); resp.StatusCode != http.StatusConflict || body.Error.Kind != KindBackupRepositoryExists {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	for body, want := range map[string]string{
		`{"name":"Local","url":"/srv/backups.git"}`:                                           "url",
		`{"name":"Absent","url":"` + host.URL("absent.git") + `","credential":` + key + `}`:   "write",
		`{"name":"No key","url":"` + host.URL("other.git") + `"}`:                             "credential",
		`{"name":"App","url":"` + host.URL("other.git") + `","credential":{"githubApp":"a"}}`: "credential",
	} {
		resp, data = f.do(t, "POST", "/api/backup-repositories", body)
		if got := decode[checkBody](t, data); resp.StatusCode != http.StatusUnprocessableEntity || got.Error.Kind != KindBackupCheckFailed || got.Error.Check != want {
			t.Errorf("%s: %d %s", body, resp.StatusCode, data)
		}
	}
	for _, body := range []string{`{"name":"","url":"` + url + `"}`, `{"url":"` + url + `"}`, `{"name":"x","url":"` + url + `","extra":1}`,
		`{"name":"x","url":"` + url + `","credential":{}}`, `{"name":"x","url":"` + url + `","credential":{"githubApp":"a","sshKey":"k"}}`,
		`{"name":"x","url":"` + url + `","credential":{"sshKey":"k","fingerprint":"f"}}`} {
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

// A credential change is checked first, recorded only without check, and
// refused for an unknown repository; the credential list names the Apps and
// the agent's keys, or why the agent listed none.
func TestABackupRepositorysCredentialIsChangedThroughTheAPI(t *testing.T) {
	f := newFixture(t, nil)
	host := backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(host.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	source := &gitcreds.Source{Socket: host.AgentPath, KnownHosts: []string{host.KnownHosts}, Apps: githubapp.Apps{{Name: "sample-org", Server: "https://git.example.invalid"}}}
	f.server.opts.Backups = &backuprepos.Registry{Data: f.data, Credentials: source}
	f.server.opts.Credentials = source
	ctx := context.Background()
	if _, err := f.data.RegisterBackupRepository(ctx, store.BackupRepository{Name: "Earlier", URL: host.URL("backups.git")}); err != nil {
		t.Fatal(err)
	}
	_, data := f.do(t, "GET", "/api/backup-repositories", "")
	if !strings.Contains(string(data), `"credential":null`) {
		t.Fatalf("a registration with no credential: %s", data)
	}
	key := backuptest.AuthorizedKey(host.UserKey)
	resp, data := f.do(t, "PUT", "/api/backup-repositories/earlier/credential", `{"credential":{"sshKey":"`+key+` a-comment"},"check":true}`)
	if got := decode[backupBody](t, data); resp.StatusCode != http.StatusOK || got.Credential == nil || got.Credential.SSHKey != key {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if r, _ := f.data.BackupRepository(ctx, "earlier"); !r.Credential.IsZero() {
		t.Fatalf("a checked change was stored: %+v", r)
	}
	resp, data = f.do(t, "PUT", "/api/backup-repositories/earlier/credential", `{"credential":{"githubApp":"sample-org"}}`)
	if got := decode[checkBody](t, data); resp.StatusCode != http.StatusUnprocessableEntity || got.Error.Check != "credential" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	for _, body := range []string{`{}`, `{"credential":null}`, `{"credential":{}}`, `{"credential":{"sshKey":"k"},"extra":1}`} {
		if resp, data := f.do(t, "PUT", "/api/backup-repositories/earlier/credential", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, resp.StatusCode, data)
		}
	}
	resp, data = f.do(t, "PUT", "/api/backup-repositories/absent/credential", `{"credential":{"sshKey":"`+key+`"}}`)
	if got := decode[checkBody](t, data); resp.StatusCode != http.StatusNotFound || got.Error.Kind != KindUnknownBackupRepository {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	resp, data = f.do(t, "PUT", "/api/backup-repositories/earlier/credential", `{"credential":{"sshKey":"`+key+`"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if r, _ := f.data.BackupRepository(ctx, "earlier"); r.Credential.Key != key {
		t.Fatalf("not stored: %+v", r)
	}

	_, data = f.do(t, "GET", "/api/credentials", "")
	want := `{"githubApps":[{"name":"sample-org","server":"https://git.example.invalid"}],"sshKeys":[{"name":"` + backuptest.UserKeyName + `","fingerprint":"` + ssh.FingerprintSHA256(host.UserKey) + `","publicKey":"` + key + `"}],"sshAgent":null}`
	if strings.TrimSpace(string(data)) != want {
		t.Fatalf("%s\nwant %s", data, want)
	}
	source.Socket = host.ServeAgent(t, agent.NewKeyring())
	if _, data = f.do(t, "GET", "/api/credentials", ""); !strings.Contains(string(data), `"sshKeys":[],"sshAgent":null`) {
		t.Fatalf("an empty agent: %s", data)
	}
	source.Socket = filepath.Join(t.TempDir(), "absent.sock")
	if _, data = f.do(t, "GET", "/api/credentials", ""); !strings.Contains(string(data), `"sshKeys":[],"sshAgent":"the SSH agent could not be reached"`) {
		t.Fatalf("no agent: %s", data)
	}
	f.server.opts.Credentials = nil
	if _, data = f.do(t, "GET", "/api/credentials", ""); strings.TrimSpace(string(data)) != `{"githubApps":[],"sshKeys":[],"sshAgent":null}` {
		t.Fatalf("no source: %s", data)
	}
}
