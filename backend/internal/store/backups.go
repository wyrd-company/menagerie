// ---
// relationships:
//   implements: [menagerie, choose-a-backup-repository-per-workspace]
//   references: workspace-api
// ---

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// BackupRepository is a registered backup repository: its permanent id, its
// display name and its Git URL, which holds one backup branch per workspace
// that backs up to it.
type BackupRepository struct {
	ID   string
	Name string
	URL  string
	// Workspaces are the workspaces that back up to it, in name order.
	Workspaces []string
}

// ErrNoBackupRepository means no backup repository has that id.
var ErrNoBackupRepository = errors.New("no registered backup repository has this id")

// ErrBackupRepositoryExists means a backup repository of that URL is
// registered. A *BackupRepositoryExistsError carries it.
var ErrBackupRepositoryExists = errors.New("a backup repository of this URL is registered")

// BackupRepositoryExistsError names the registration a URL has.
type BackupRepositoryExistsError struct{ ID, Name string }

func (e *BackupRepositoryExistsError) Error() string {
	return fmt.Sprintf("this repository is registered already, as %s (%s)", e.Name, e.ID)
}

// Is makes errors.Is match ErrBackupRepositoryExists.
func (e *BackupRepositoryExistsError) Is(target error) bool {
	return target == ErrBackupRepositoryExists
}

// ErrBackupRepositoryInUse means workspaces back up to a repository. A
// *BackupRepositoryInUseError carries it.
var ErrBackupRepositoryInUse = errors.New("workspaces back up to this repository")

// BackupRepositoryInUseError names the workspaces that back up to a
// repository.
type BackupRepositoryInUseError struct{ Workspaces []string }

func (e *BackupRepositoryInUseError) Error() string {
	return "workspace " + strings.Join(e.Workspaces, ", ") + " backs up to this repository"
}

// Is makes errors.Is match ErrBackupRepositoryInUse.
func (e *BackupRepositoryInUseError) Is(target error) bool {
	return target == ErrBackupRepositoryInUse
}

// BackupRepositoryID makes a backup repository's permanent id from its name,
// as TemplateID does, or "backups" when the name has no letter or digit.
func BackupRepositoryID(name string) string {
	if id := slug(name); id != "" {
		return id
	}
	return "backups"
}

func (t *Tx) backupRepository(id string) (BackupRepository, error) {
	var r BackupRepository
	err := t.tx.QueryRowContext(t.ctx, `SELECT id, name, url FROM backup_repositories WHERE id = ?`, id).Scan(&r.ID, &r.Name, &r.URL)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupRepository{}, fmt.Errorf("%w: %s", ErrNoBackupRepository, id)
	}
	if err != nil {
		return BackupRepository{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	if r.Workspaces, err = t.names(`SELECT name FROM workspaces WHERE backup_repository = ? ORDER BY name`, id); err != nil {
		return BackupRepository{}, err
	}
	if r.Workspaces == nil {
		r.Workspaces = []string{}
	}
	return r, nil
}

// RegisterBackupRepository records r, whose ID is empty, under
// BackupRepositoryID of its name with -2, -3 and on added while that id is
// taken. A URL that is registered is a *BackupRepositoryExistsError.
func (t *Tx) RegisterBackupRepository(r BackupRepository) (BackupRepository, error) {
	var existing, existingName string
	err := t.tx.QueryRowContext(t.ctx, `SELECT id, name FROM backup_repositories WHERE url = ?`, r.URL).Scan(&existing, &existingName)
	switch {
	case err == nil:
		return BackupRepository{}, &BackupRepositoryExistsError{ID: existing, Name: existingName}
	case !errors.Is(err, sql.ErrNoRows):
		return BackupRepository{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	base := BackupRepositoryID(r.Name)
	r.ID = base
	for n := 2; ; n++ {
		var taken int
		if err := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM backup_repositories WHERE id = ?`, r.ID).Scan(&taken); err != nil {
			return BackupRepository{}, fmt.Errorf("Menagerie's data: %w", err)
		}
		if taken == 0 {
			break
		}
		r.ID = fmt.Sprintf("%s-%d", base, n)
	}
	if _, err := t.exec(`INSERT INTO backup_repositories (id, name, url) VALUES (?, ?, ?)`, r.ID, r.Name, r.URL); err != nil {
		return BackupRepository{}, err
	}
	return t.backupRepository(r.ID)
}

// RegisterBackupRepository records a backup repository; see
// Tx.RegisterBackupRepository.
func (s *Store) RegisterBackupRepository(ctx context.Context, r BackupRepository) (recorded BackupRepository, err error) {
	err = s.Update(ctx, func(t *Tx) error { recorded, err = t.RegisterBackupRepository(r); return err })
	return recorded, err
}

// TryRegisterBackupRepository answers what RegisterBackupRepository would
// record, its id included, and its refusals, and stores nothing.
func (s *Store) TryRegisterBackupRepository(ctx context.Context, r BackupRepository) (recorded BackupRepository, err error) {
	err = s.Dry(ctx, func(t *Tx) error { recorded, err = t.RegisterBackupRepository(r); return err })
	return recorded, err
}

// BackupRepositories lists the registered backup repositories by name
// without regard to case, then by name and id, each with the workspaces
// that back up to it.
func (s *Store) BackupRepositories(ctx context.Context) (out []BackupRepository, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		ids, err := t.names(`SELECT id FROM backup_repositories ORDER BY name COLLATE NOCASE, name, id`)
		if err != nil {
			return err
		}
		out = make([]BackupRepository, 0, len(ids))
		for _, id := range ids {
			r, err := t.backupRepository(id)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// BackupRepository reads one registered backup repository, or
// ErrNoBackupRepository.
func (s *Store) BackupRepository(ctx context.Context, id string) (r BackupRepository, err error) {
	err = s.Update(ctx, func(t *Tx) error { r, err = t.backupRepository(id); return err })
	return r, err
}

// RemoveBackupRepository removes a backup repository no workspace backs up
// to; one that workspaces back up to is a *BackupRepositoryInUseError
// naming them, and the foreign key refuses it too. The repository itself,
// and the branches it holds, are left as they are.
func (s *Store) RemoveBackupRepository(ctx context.Context, id string) error {
	return s.Update(ctx, func(t *Tx) error {
		r, err := t.backupRepository(id)
		if err != nil {
			return err
		}
		if len(r.Workspaces) > 0 {
			return &BackupRepositoryInUseError{Workspaces: r.Workspaces}
		}
		_, err = t.exec(`DELETE FROM backup_repositories WHERE id = ?`, id)
		return err
	})
}

// WorkspaceBackupRepository is the backup repository a workspace backs up
// to, and false when the data holds none for it.
func (s *Store) WorkspaceBackupRepository(ctx context.Context, workspace string) (r BackupRepository, ok bool, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		id, err := t.holder(`SELECT coalesce(backup_repository, '') FROM workspaces WHERE name = ?`, workspace)
		if err != nil || id == "" {
			return err
		}
		r, err = t.backupRepository(id)
		ok = err == nil
		return err
	})
	return r, ok, err
}

// WorkspaceBackupRepositories maps each workspace that has a backup
// repository to it.
func (s *Store) WorkspaceBackupRepositories(ctx context.Context) (map[string]BackupRepository, error) {
	repositories, err := s.BackupRepositories(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]BackupRepository{}
	for _, r := range repositories {
		for _, name := range r.Workspaces {
			out[name] = r
		}
	}
	return out, nil
}

// SetWorkspaceBackupRepository makes the registered backup repository id
// the one a workspace backs up to, giving the workspace its data when it has
// none.
func (s *Store) SetWorkspaceBackupRepository(ctx context.Context, workspace, id string) error {
	return s.Update(ctx, func(t *Tx) error {
		if _, err := t.backupRepository(id); err != nil {
			return err
		}
		if err := t.AddWorkspace(workspace); err != nil {
			return err
		}
		_, err := t.exec(`UPDATE workspaces SET backup_repository = ? WHERE name = ?`, id, workspace)
		return err
	})
}

// AdoptBackupRepository gives every workspace that has no backup repository
// the registered backup repository id, and returns how many it gave it.
func (t *Tx) AdoptBackupRepository(id string) (int64, error) {
	result, err := t.exec(`UPDATE workspaces SET backup_repository = ? WHERE backup_repository IS NULL`, id)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("Menagerie's data: %w", err)
	}
	return n, nil
}
