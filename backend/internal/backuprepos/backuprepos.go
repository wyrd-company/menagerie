// ---
// relationships:
//   implements: [menagerie, choose-a-backup-repository-per-workspace, choose-a-credential-per-backup-repository]
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
	neturl "net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"golang.org/x/crypto/ssh"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/gitcreds"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// DefaultCheckWait bounds a registration's write check.
const DefaultCheckWait = time.Minute

// The checks a registration runs, as a CheckError names them.
const (
	CheckURL        = "url"
	CheckCredential = "credential"
	CheckWrite      = "write"
)

// InvalidError refuses a request value, such as a name.
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return e.Message }

// CheckError refuses a repository that failed a registration check. Check
// is CheckURL, CheckCredential or CheckWrite; the message names what
// failed and no secret.
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

// Credentials gives a credential its transfers' authentication, and
// checks that a credential fits a URL. *gitcreds.Source satisfies it.
type Credentials interface {
	Auth(c store.Credential) backup.Auth
	// Fit refuses, with a *gitcreds.FitError, a credential a new
	// registration of url cannot have.
	Fit(ctx context.Context, url string, c store.Credential) error
}

// Registry is the registered backup repositories, each reached with its
// own credential.
type Registry struct {
	Data *store.Store
	// Credentials gives each repository's transfers the authentication of
	// its credential.
	Credentials Credentials
	// CheckWait bounds a registration's write check; zero is
	// DefaultCheckWait.
	CheckWait time.Duration
}

// Remote is the registered repository repo, reached with its credential.
func (r *Registry) Remote(repo store.BackupRepository) *backup.Remote {
	return &backup.Remote{URL: repo.URL, Auth: r.Credentials.Auth(repo.Credential)}
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

// Register checks a repository and records it under the id its name gives,
// with its credential. The checks are its name, its URL, that no
// registration has the URL, that the credential fits the URL
// (Credentials.Fit), and that Menagerie can write to it with that
// credential (backup.Remote.CheckWrite). With check, it runs the checks
// alone, answers what it would record, and stores nothing.
func (r *Registry) Register(ctx context.Context, name, url string, credential store.Credential, check bool) (store.BackupRepository, error) {
	name = strings.TrimSpace(name)
	if err := ValidName(name); err != nil {
		return store.BackupRepository{}, err
	}
	if err := ValidURL(url); err != nil {
		return store.BackupRepository{}, err
	}
	credential, err := normalCredential(credential)
	if err != nil {
		return store.BackupRepository{}, err
	}
	candidate := store.BackupRepository{Name: name, URL: url, Credential: credential}
	tried, err := r.Data.TryRegisterBackupRepository(ctx, candidate)
	if err != nil {
		return store.BackupRepository{}, err
	}
	if err := r.check(ctx, candidate); err != nil {
		return store.BackupRepository{}, err
	}
	if check {
		return tried, nil
	}
	return r.Data.RegisterBackupRepository(ctx, candidate)
}

// SetCredential checks the registered repository id with credential, as
// Register checks a new one, and then records it as the repository's
// credential, only while the repository is still registered at the URL it
// checked. With check, it runs the checks alone, answers what it would
// record, and stores nothing.
func (r *Registry) SetCredential(ctx context.Context, id string, credential store.Credential, check bool) (store.BackupRepository, error) {
	repo, err := r.Data.BackupRepository(ctx, id)
	if err != nil {
		return store.BackupRepository{}, err
	}
	if repo.Credential, err = normalCredential(credential); err != nil {
		return store.BackupRepository{}, err
	}
	if err := r.check(ctx, repo); err != nil {
		return store.BackupRepository{}, err
	}
	if check {
		return repo, nil
	}
	return r.Data.SetBackupRepositoryCredential(ctx, id, repo.URL, repo.Credential)
}

// normalCredential is c with its key in the form a credential records, or
// a *CheckError for one that names an App and a key, or a key that is not
// a public key.
func normalCredential(c store.Credential) (store.Credential, error) {
	if c.App != "" && c.Key != "" {
		return c, &CheckError{Check: CheckCredential, Message: "a credential is a GitHub App or an SSH key, not both"}
	}
	if c.Key != "" {
		key, ok := gitcreds.NormalKey(c.Key)
		if !ok {
			return c, &CheckError{Check: CheckCredential, Message: "the SSH key is not a public key in authorized-keys form"}
		}
		c.Key = key
	}
	return c, nil
}

// check runs a registration's credential and write checks on repo.
func (r *Registry) check(ctx context.Context, repo store.BackupRepository) error {
	if err := r.Credentials.Fit(ctx, repo.URL, repo.Credential); err != nil {
		var fit *gitcreds.FitError
		if errors.As(err, &fit) {
			return &CheckError{Check: CheckCredential, Message: fit.Message}
		}
		return err
	}
	wait := r.CheckWait
	if wait == 0 {
		wait = DefaultCheckWait
	}
	checkCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := r.Remote(repo).CheckWrite(checkCtx, io.Discard); err != nil {
		if checkCtx.Err() != nil && ctx.Err() == nil {
			return &CheckError{Check: CheckWrite, Message: fmt.Sprintf("the repository did not answer within %s%s", wait, credentialPhrase(repo.Credential))}
		}
		return &CheckError{Check: CheckWrite, Message: "Menagerie could not write to the repository" + credentialPhrase(repo.Credential) + ": " + strings.TrimPrefix(err.Error(), "write check: ")}
	}
	return nil
}

// credentialPhrase names c in a write check's refusal, so a manager knows
// which credential the Git host refused: " with GitHub App {name}", or
// " with SSH key {fingerprint}". The zero credential, chosen by URL, is "".
// Neither names a secret.
func credentialPhrase(c store.Credential) string {
	switch {
	case c.App != "":
		return " with GitHub App " + c.App
	case c.Key != "":
		if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.Key)); err == nil {
			return " with SSH key " + ssh.FingerprintSHA256(key)
		}
	}
	return ""
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
	return r.Remote(repo), nil
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
// repository then backs up to it. The caller lists the workspace folders
// first, so a folder with no data has its workspace by then. No check
// runs: the key was the repository every backup went to. With url "" it
// records the mark and registers nothing.
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
// its URL, without a leading slash, a query, a fragment and a .git suffix,
// such as example-org/workspace-backups, or "Workspace backups" when it
// has none. It holds no credential the URL holds.
func ImportName(url string) string {
	endpoint, err := transport.NewEndpoint(url)
	if err != nil {
		return "Workspace backups"
	}
	path, _, _ := strings.Cut(endpoint.Path, "?")
	path, _, _ = strings.Cut(path, "#")
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if ValidName(path) != nil {
		return "Workspace backups"
	}
	return path
}

// PublicURL is url as a reply or a page shows it: an URL with a scheme
// loses its password, query and fragment, and an http or https URL its
// user name too, since a token can stand in either, as an earlier
// release's workspace-repository could carry them. A registration refuses
// a URL with a password; the import keeps one, because backups used it.
func PublicURL(url string) string {
	u, err := neturl.Parse(url)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if at := strings.LastIndex(url, "@"); at >= 0 && strings.Contains(url[:at], ":") && !strings.Contains(url, "://") {
			// An scp-like address with a password, user:password@host:path.
			user, _, _ := strings.Cut(url[:at], ":")
			return user + url[at:]
		}
		return url
	}
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		u.User = nil
	case u.User != nil:
		u.User = neturl.User(u.User.Username())
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String()
}
