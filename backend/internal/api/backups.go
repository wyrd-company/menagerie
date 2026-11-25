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

	"github.com/wyrd-company/menagerie/backend/internal/backuprepos"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// BackupRegistry checks and registers backup repositories.
// *backuprepos.Registry satisfies it.
type BackupRegistry interface {
	// With check, it runs its checks alone and stores nothing.
	Register(ctx context.Context, name, url string, check bool) (store.BackupRepository, error)
}

// BackupRepository is a registered backup repository as the browser sees
// it.
type BackupRepository struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// Workspaces are the workspaces that back up to it.
	Workspaces []string `json:"workspaces"`
}

// backupRepositoryRef names a workspace's backup repository on its row.
type backupRepositoryRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func backupRepositoryOf(r store.BackupRepository) BackupRepository {
	out := BackupRepository{ID: r.ID, Name: r.Name, URL: backuprepos.PublicURL(r.URL), Workspaces: r.Workspaces}
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
		Name  *string `json:"name"`
		URL   *string `json:"url"`
		Check bool    `json:"check"`
	}
	if !decodeOne(w, r, &req) {
		return
	}
	if req.Name == nil || req.URL == nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "name and url are required"})
		return
	}
	registered, err := s.opts.Backups.Register(r.Context(), *req.Name, *req.URL, req.Check)
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
