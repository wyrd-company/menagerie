// ---
// relationships:
//   implements: [menagerie, choose-a-backup-repository-per-workspace]
//   references: [workspace-api, configuration]
// ---

// Package backuprepos registers the backup repositories a workspace can
// back up to, and resolves each workspace's backups to the repository its
// data names.
package backuprepos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// DefaultCheckWait bounds a registration's write check.
const DefaultCheckWait = time.Minute

// The checks a registration runs, as a CheckError names them.
const (
	CheckURL   = "url"
	CheckWrite = "write"
)

// InvalidError refuses a request value, such as a name.
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return e.Message }

// CheckError refuses a repository that failed a registration check. Check
// is CheckURL or CheckWrite; the message names what failed and no
// credential.
type CheckError struct {
	Check   string
	Message string
}

func (e *CheckError) Error() string { return e.Message }

// NoRepositoryError says that a workspace has no backup repository, so it
// cannot be backed up.
type NoRepositoryError struct{ Workspace string }

func (e *NoRepositoryError) Error() string {
	return fmt.Sprintf("workspace %s has no backup repository; register one and move the workspace's backups to it", e.Workspace)
}

// Registry is the registered backup repositories, reached with Auth.
type Registry struct {
	Data *store.Store
	// Auth gives each transfer its credentials, chosen by URL.
	Auth backup.Auth
	// CheckWait bounds a registration's write check; zero is
	// DefaultCheckWait.
	CheckWait time.Duration
}

// Remote is the repository at url, reached with the registry's
// credentials.
func (r *Registry) Remote(url string) *backup.Remote {
	return &backup.Remote{URL: url, Auth: r.Auth}
}

// ValidName checks a display name: one line of 1 to 100 characters.
func ValidName(name string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(name) || strings.ContainsAny(name, "\r\n") {
		return &InvalidError{Message: "name must be one line of 1 to 100 characters"}
	}
	return nil
}

// ValidURL checks a URL a manager registers: an ssh, scp-like or https Git
// URL. A local path or a file URL is refused, so a registration never
// writes into Menagerie's own filesystem. No refusal quotes the URL, which
// can hold a password.
func ValidURL(url string) error {
	if strings.TrimSpace(url) != url || url == "" {
		return &CheckError{Check: CheckURL, Message: "the URL must be an ssh or https Git URL with no surrounding space"}
	}
	endpoint, err := transport.NewEndpoint(url)
	if err != nil {
		return &CheckError{Check: CheckURL, Message: "the URL is not a valid Git URL"}
	}
	if endpoint.Password != "" {
		return &CheckError{Check: CheckURL, Message: "the URL must not hold a password: Menagerie's credentials come from the SSH agent or the GitHub App"}
	}
	switch endpoint.Protocol {
	case "ssh", "https":
		return nil
	}
	return &CheckError{Check: CheckURL, Message: "the URL must be an ssh, scp-like or https Git URL; a local path, a file URL and plain http are refused"}
}

// Register checks a repository and records it under the id its name gives.
// The checks are its name, its URL, that no registration has the URL, and
// that Menagerie can write to it (backup.Remote.CheckWrite). With check,
// it runs the checks alone, answers what it would record, and stores
// nothing.
func (r *Registry) Register(ctx context.Context, name, url string, check bool) (store.BackupRepository, error) {
	name = strings.TrimSpace(name)
	if err := ValidName(name); err != nil {
		return store.BackupRepository{}, err
	}
	if err := ValidURL(url); err != nil {
		return store.BackupRepository{}, err
	}
	candidate := store.BackupRepository{Name: name, URL: url}
	tried, err := r.Data.TryRegisterBackupRepository(ctx, candidate)
	if err != nil {
		return store.BackupRepository{}, err
	}
	wait := r.CheckWait
	if wait == 0 {
		wait = DefaultCheckWait
	}
	checkCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := r.Remote(url).CheckWrite(checkCtx, io.Discard); err != nil {
		if checkCtx.Err() != nil && ctx.Err() == nil {
			return store.BackupRepository{}, &CheckError{Check: CheckWrite, Message: fmt.Sprintf("the repository did not answer within %s", wait)}
		}
		return store.BackupRepository{}, &CheckError{Check: CheckWrite, Message: "Menagerie could not write to the repository: " + strings.TrimPrefix(err.Error(), "write check: ")}
	}
	if check {
		return tried, nil
	}
	return r.Data.RegisterBackupRepository(ctx, candidate)
}

// For is the repository a workspace backs up to, or a *NoRepositoryError.
func (r *Registry) For(ctx context.Context, workspace string) (*backup.Remote, error) {
	repo, ok, err := r.Data.WorkspaceBackupRepository(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &NoRepositoryError{Workspace: workspace}
	}
	return r.Remote(repo.URL), nil
}

// Push backs the workspace branch up to the workspace's repository; see
// backup.Remote.Push.
func (r *Registry) Push(ctx context.Context, folder, workspace string, progress io.Writer) (bool, error) {
	remote, err := r.For(ctx, workspace)
	if err != nil {
		return false, err
	}
	return remote.Push(ctx, folder, workspace, progress)
}

// ImportID is the mark of the backup repository import in Menagerie's
// data.
const ImportID = 6

// Import takes over the configuration's workspace-repository key once, at
// the first start whose data has no backup repository import: url, when
// it is not "", is registered under a name its path gives, unless a
// registration has that URL already, and every workspace with no backup
// repository then backs up to it. No check runs: the key was the
// repository every backup went to. With url "" it records the mark and
// registers nothing.
func Import(ctx context.Context, data *store.Store, url string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var registered store.BackupRepository
	var adopted int64
	ran, err := data.Import(ctx, ImportID, func(tx *store.Tx) error {
		if url == "" {
			return nil
		}
		var err error
		registered, err = tx.RegisterBackupRepository(store.BackupRepository{Name: ImportName(url), URL: url})
		var exists *store.BackupRepositoryExistsError
		if errors.As(err, &exists) {
			registered = store.BackupRepository{ID: exists.ID, Name: exists.Name}
		} else if err != nil {
			return err
		}
		adopted, err = tx.AdoptBackupRepository(registered.ID)
		return err
	})
	if err == nil && ran && url != "" {
		logf("menagerie: the configuration's workspace-repository is registered as backup repository %s (%s); %d workspaces back up to it", registered.Name, registered.ID, adopted)
	}
	return err
}

// ImportName is the display name of the imported repository: the path of
// its URL, without a leading slash and a .git suffix, such as
// example-org/workspace-backups, or "Workspace backups" when it has none.
func ImportName(url string) string {
	endpoint, err := transport.NewEndpoint(url)
	if err != nil {
		return "Workspace backups"
	}
	path := strings.TrimSuffix(strings.Trim(endpoint.Path, "/"), ".git")
	if ValidName(path) != nil {
		return "Workspace backups"
	}
	return path
}
