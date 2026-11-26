// ---
// relationships:
//   implements: [workspace-api, choose-a-backup-repository-per-workspace]
//   references: menagerie
// ---

package api

import (
	"context"
	"errors"
	"net/http"

	"golang.org/x/crypto/ssh"

	"github.com/wyrd-company/menagerie/backend/internal/backuprepos"
	"github.com/wyrd-company/menagerie/backend/internal/gitcreds"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// BackupRegistry checks and registers backup repositories, and changes
// their credentials. *backuprepos.Registry satisfies it.
type BackupRegistry interface {
	// With check, it runs its checks alone and stores nothing.
	Register(ctx context.Context, name, url string, credential store.Credential, check bool) (store.BackupRepository, error)
	// With check, it runs its checks alone and stores nothing.
	SetCredential(ctx context.Context, id string, credential store.Credential, check bool) (store.BackupRepository, error)
}

// CredentialSource lists the credentials a backup repository can name.
// *gitcreds.Source satisfies it.
type CredentialSource interface {
	AppChoices() []gitcreds.App
	Keys(ctx context.Context) ([]gitcreds.Key, error)
}

// BackupRepository is a registered backup repository as the browser sees
// it.
type BackupRepository struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// Credential is null for a registration from an earlier release, whose
	// URL chooses its credential.
	Credential *BackupCredential `json:"credential"`
	// Workspaces are the workspaces that back up to it.
	Workspaces []string `json:"workspaces"`
}

// BackupCredential is a backup repository's credential: a GitHub App by
// name, or an SSH key by its public key, with the key's fingerprint in a
// reply. A request gives one of GitHubApp and SSHKey.
type BackupCredential struct {
	GitHubApp   string `json:"githubApp,omitempty"`
	SSHKey      string `json:"sshKey,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

func credentialOf(c store.Credential) *BackupCredential {
	switch {
	case c.App != "":
		return &BackupCredential{GitHubApp: c.App}
	case c.Key != "":
		out := &BackupCredential{SSHKey: c.Key}
		if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.Key)); err == nil {
			out.Fingerprint = ssh.FingerprintSHA256(key)
		}
		return out
	}
	return nil
}

// requestedCredential is the credential a request names, or a refusal.
func requestedCredential(c *BackupCredential) (store.Credential, error) {
	if c == nil {
		return store.Credential{}, nil
	}
	if c.Fingerprint != "" {
		return store.Credential{}, &backuprepos.InvalidError{Message: "a credential names githubApp or sshKey; fingerprint is the reply's"}
	}
	if (c.GitHubApp == "") == (c.SSHKey == "") {
		return store.Credential{}, &backuprepos.InvalidError{Message: "a credential names exactly one of githubApp and sshKey"}
	}
	return store.Credential{App: c.GitHubApp, Key: c.SSHKey}, nil
}

// backupRepositoryRef names a workspace's backup repository on its row.
type backupRepositoryRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func backupRepositoryOf(r store.BackupRepository) BackupRepository {
	out := BackupRepository{ID: r.ID, Name: r.Name, URL: backuprepos.PublicURL(r.URL), Credential: credentialOf(r.Credential), Workspaces: r.Workspaces}
	if out.Workspaces == nil {
		out.Workspaces = []string{}
	}
	return out
}

func (s *Server) listBackupRepositories(w http.ResponseWriter, r *http.Request) {
	registered, err := s.opts.Data.BackupRepositories(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	out := make([]BackupRepository, 0, len(registered))
	for _, b := range registered {
		out = append(out, backupRepositoryOf(b))
	}
	writeJSON(w, http.StatusOK, map[string][]BackupRepository{"backupRepositories": out})
}

func (s *Server) registerBackupRepository(w http.ResponseWriter, r *http.Request) {
	if s.opts.Backups == nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: "backup repositories cannot be registered: the workspace service has no backup registry"})
		return
	}
	var req struct {
		Name       *string           `json:"name"`
		URL        *string           `json:"url"`
		Credential *BackupCredential `json:"credential"`
		Check      bool              `json:"check"`
	}
	if !decodeOne(w, r, &req) {
		return
	}
	if req.Name == nil || req.URL == nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "name and url are required"})
		return
	}
	credential, err := requestedCredential(req.Credential)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	registered, err := s.opts.Backups.Register(r.Context(), *req.Name, *req.URL, credential, req.Check)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	if req.Check {
		// The checks passed; nothing was stored.
		writeJSON(w, http.StatusOK, backupRepositoryOf(registered))
		return
	}
	w.Header().Set("Location", "/api/backup-repositories/"+registered.ID)
	writeJSON(w, http.StatusCreated, backupRepositoryOf(registered))
}

// setBackupCredential changes a backup repository's credential once its
// checks pass; with check, it runs them alone.
func (s *Server) setBackupCredential(w http.ResponseWriter, r *http.Request) {
	if s.opts.Backups == nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: "backup repositories cannot be changed: the workspace service has no backup registry"})
		return
	}
	var req struct {
		Credential *BackupCredential `json:"credential"`
		Check      bool              `json:"check"`
	}
	if !decodeOne(w, r, &req) {
		return
	}
	if req.Credential == nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "credential is required"})
		return
	}
	credential, err := requestedCredential(req.Credential)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	changed, err := s.opts.Backups.SetCredential(r.Context(), r.PathValue("id"), credential, req.Check)
	if err != nil {
		writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, backupRepositoryOf(changed))
}

// listCredentials answers the credentials a backup repository can name:
// the configured GitHub Apps, and the keys the SSH agent offers, or why it
// listed none.
func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	type app struct {
		Name   string `json:"name"`
		Server string `json:"server"`
	}
	type key struct {
		Name        string `json:"name"`
		Fingerprint string `json:"fingerprint"`
		PublicKey   string `json:"publicKey"`
	}
	out := struct {
		GitHubApps []app   `json:"githubApps"`
		SSHKeys    []key   `json:"sshKeys"`
		SSHAgent   *string `json:"sshAgent"`
	}{GitHubApps: []app{}, SSHKeys: []key{}}
	if c := s.opts.Credentials; c != nil {
		for _, a := range c.AppChoices() {
			out.GitHubApps = append(out.GitHubApps, app{Name: a.Name, Server: a.Server})
		}
		keys, err := c.Keys(r.Context())
		if err != nil {
			reason := err.Error()
			out.SSHAgent = &reason
		}
		for _, k := range keys {
			out.SSHKeys = append(out.SSHKeys, key{Name: k.Name, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) removeBackupRepository(w http.ResponseWriter, r *http.Request) {
	if err := s.opts.Data.RemoveBackupRepository(r.Context(), r.PathValue("id")); err != nil {
		writeBackupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The refusal kinds of the backup repository routes.
const (
	KindBackupCheckFailed       = "backup-repository-check-failed"
	KindBackupRepositoryExists  = "backup-repository-exists"
	KindBackupRepositoryInUse   = "backup-repository-in-use"
	KindUnknownBackupRepository = "unknown-backup-repository"
)

func writeBackupError(w http.ResponseWriter, err error) {
	var (
		check   *backuprepos.CheckError
		invalid *backuprepos.InvalidError
		exists  *store.BackupRepositoryExistsError
		inUse   *store.BackupRepositoryInUseError
	)
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: invalid.Message})
	case errors.As(err, &check):
		writeError(w, http.StatusUnprocessableEntity, apiError{Kind: KindBackupCheckFailed, Message: check.Message, Check: check.Check})
	case errors.As(err, &exists):
		writeError(w, http.StatusConflict, apiError{Kind: KindBackupRepositoryExists, Message: exists.Error()})
	case errors.As(err, &inUse):
		writeError(w, http.StatusConflict, apiError{Kind: KindBackupRepositoryInUse, Message: inUse.Error() + "; move those workspaces' backups to another repository first"})
	case errors.Is(err, store.ErrNoBackupRepository):
		writeError(w, http.StatusNotFound, apiError{Kind: KindUnknownBackupRepository, Message: "no registered backup repository has this id"})
	default:
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
	}
}
