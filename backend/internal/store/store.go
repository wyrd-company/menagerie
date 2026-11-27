// ---
// relationships:
//   implements: [menagerie, keep-menagerie-data-in-sqlite]
//   references: menagerie-data-loss
// ---

// Package store keeps Menagerie's own data in one SQLite database that only
// the workspace service opens: the registered templates and backup
// repositories, the workspaces with their templates and backup
// repositories, each container name Menagerie bound and each Xolotl key it
// issued, the policy settings and selections, each template's found
// versions and last version check, each workspace's applied version, and the
// marks of the first-start imports. Every change is one transaction and
// foreign keys are enforced, so a constraint, not a scan of files, keeps a
// container name or a key with one workspace at a time, one registration per
// template source, and a template that a workspace uses in place. The
// schema's version is the database's user_version: an open upgrades an older
// schema in one transaction and refuses a newer one. The database holds no
// secret.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultPath is the database in Menagerie's data volume, in a folder private
// to the services' user, which no devcontainer and no throwaway container
// reaches.
const DefaultPath = "/var/lib/menagerie/state/menagerie.db"

// migrations bring the schema from the version of their index to the next. A
// later task appends its own; none is ever changed once released.
var migrations = []string{
	// 1: the workspaces, with the being-created mark and the folder a delete
	// began to remove; the container names and Xolotl keys provisioned for
	// them; and the marks of the first-start imports.
	`CREATE TABLE workspaces (
		name TEXT PRIMARY KEY CHECK (name != ''),
		creating INTEGER NOT NULL DEFAULT 0 CHECK (creating IN (0, 1)),
		removing TEXT CHECK (removing != '')
	);
	CREATE TABLE container_names (
		name TEXT PRIMARY KEY CHECK (name != ''),
		workspace TEXT NOT NULL REFERENCES workspaces(name),
		agent TEXT CHECK (agent != ''),
		current INTEGER NOT NULL DEFAULT 0 CHECK (current IN (0, 1))
	);
	CREATE INDEX container_names_workspace ON container_names(workspace);
	CREATE UNIQUE INDEX container_names_current ON container_names(workspace) WHERE current = 1;
	CREATE TABLE xolotl_keys (
		policy TEXT NOT NULL CHECK (policy != ''),
		name TEXT NOT NULL CHECK (name != ''),
		workspace TEXT NOT NULL REFERENCES workspaces(name),
		PRIMARY KEY (policy, name)
	);
	CREATE INDEX xolotl_keys_workspace ON xolotl_keys(workspace);
	CREATE TABLE imports (
		id INTEGER PRIMARY KEY,
		done_at TEXT NOT NULL
	);`,
	// 2: policy settings and workspace selections, with separate revisions.
	`CREATE TABLE policy_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		document TEXT NOT NULL,
		next_group_id INTEGER NOT NULL DEFAULT 1
	);
	INSERT INTO policy_settings (id, document) VALUES (1, '{"required":[],"groups":[],"revision":0}');
	ALTER TABLE workspaces ADD COLUMN selection_revision INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE workspace_policies (
		workspace TEXT NOT NULL REFERENCES workspaces(name) ON DELETE CASCADE,
		policy TEXT NOT NULL,
		PRIMARY KEY (workspace, policy)
	);
	CREATE TRIGGER workspace_required_policies AFTER INSERT ON workspaces BEGIN
		INSERT INTO workspace_policies (workspace, policy)
		SELECT NEW.name, value FROM policy_settings, json_each(document, '$.required');
		UPDATE workspaces SET selection_revision = 1 WHERE name = NEW.name
		AND EXISTS (SELECT 1 FROM workspace_policies WHERE workspace = NEW.name);
	END;`,
	// 3: policy application, immutable provisioning identity and binding IDs.
	`ALTER TABLE container_names ADD COLUMN binding_id TEXT;
	ALTER TABLE workspaces ADD COLUMN applied_revision INTEGER;
	ALTER TABLE workspaces ADD COLUMN apply_failure TEXT;
	ALTER TABLE workspaces ADD COLUMN selection_imported INTEGER NOT NULL DEFAULT 1 CHECK (selection_imported IN (0, 1));
	UPDATE workspaces SET selection_imported = 0;
	ALTER TABLE workspaces ADD COLUMN policy_values TEXT NOT NULL DEFAULT '{}';
	ALTER TABLE workspaces ADD COLUMN xolotl_identity TEXT;
	ALTER TABLE workspaces ADD COLUMN selection_import_fallback TEXT NOT NULL DEFAULT '{}';`,
	// 4: the registered templates, each with its current version, the tag
	// that gives it and its commit, null for an imported template until a
	// check records it; and each workspace's template, which a template
	// that a workspace uses cannot be removed from under.
	`CREATE TABLE templates (
		id TEXT PRIMARY KEY CHECK (id != ''),
		name TEXT NOT NULL CHECK (name != ''),
		address TEXT NOT NULL CHECK (address != ''),
		source TEXT NOT NULL UNIQUE CHECK (source != ''),
		runner_image TEXT CHECK (runner_image != ''),
		current_version TEXT NOT NULL CHECK (current_version != ''),
		current_tag TEXT NOT NULL CHECK (current_tag != ''),
		current_commit TEXT CHECK (current_commit != '')
	);
	ALTER TABLE workspaces ADD COLUMN template TEXT REFERENCES templates(id);
	CREATE INDEX workspaces_template ON workspaces(template);`,
	// 5: each template's found versions, in ascending order, and the time
	// and failure of its last version check; and each workspace's applied
	// version, null while unknown, and its applied commit.
	`CREATE TABLE template_versions (
		template TEXT NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
		position INTEGER NOT NULL,
		version TEXT NOT NULL CHECK (version != ''),
		tag TEXT NOT NULL CHECK (tag != ''),
		commit_id TEXT NOT NULL CHECK (commit_id != ''),
		PRIMARY KEY (template, version),
		UNIQUE (template, position)
	);
	ALTER TABLE templates ADD COLUMN checked_at TEXT CHECK (checked_at != '');
	ALTER TABLE templates ADD COLUMN check_failure TEXT CHECK (check_failure IN
		('template-not-found', 'template-resolve-failed', 'invalid-address', 'template-service-unavailable'));
	ALTER TABLE workspaces ADD COLUMN applied_version TEXT CHECK (applied_version != '');
	ALTER TABLE workspaces ADD COLUMN applied_commit TEXT CHECK (applied_commit != '');`,
	// 6: the registered backup repositories, and the one each workspace
	// backs up to, which cannot be removed from under it. A removed
	// repository keeps its row, marked removed, so its id is never given to
	// another; one URL has one registration that is not removed.
	`CREATE TABLE backup_repositories (
		id TEXT PRIMARY KEY CHECK (id != ''),
		name TEXT NOT NULL CHECK (name != ''),
		url TEXT NOT NULL CHECK (url != ''),
		removed_at TEXT CHECK (removed_at != '')
	);
	CREATE UNIQUE INDEX backup_repositories_url ON backup_repositories(url) WHERE removed_at IS NULL;
	ALTER TABLE workspaces ADD COLUMN backup_repository TEXT REFERENCES backup_repositories(id);
	CREATE INDEX workspaces_backup_repository ON workspaces(backup_repository);`,
	// 7: each backup repository's credential: the name of a GitHub App, or
	// an SSH key's public key, or neither for a registration an earlier
	// release made, whose credential its URL chooses.
	`ALTER TABLE backup_repositories ADD COLUMN credential_app TEXT CHECK (credential_app != '');
	ALTER TABLE backup_repositories ADD COLUMN credential_key TEXT CHECK (credential_key IS NULL OR (credential_key != '' AND credential_app IS NULL));`,
	// 8: each template's credential, as a backup repository's: the name of
	// a GitHub App, or an SSH key's public key, or neither for a template
	// an earlier release registered or imported, whose credential its URL
	// chooses; and how many times it changed, so a version check that
	// started before a change records nothing.
	`ALTER TABLE templates ADD COLUMN credential_app TEXT CHECK (credential_app != '');
	ALTER TABLE templates ADD COLUMN credential_key TEXT CHECK (credential_key IS NULL OR (credential_key != '' AND credential_app IS NULL));
	ALTER TABLE templates ADD COLUMN credential_changes INTEGER NOT NULL DEFAULT 0;`,
}

// SchemaVersion is the version of the schema this Menagerie reads and writes.
var SchemaVersion = len(migrations)

// ErrNewer means the database was written by a newer Menagerie.
var ErrNewer = errors.New("Menagerie's data is newer than this Menagerie")

// ErrTaken means a container name or a Xolotl key belongs to another
// workspace. A *TakenError carries it.
var ErrTaken = errors.New("taken by another workspace")

// ErrAgentDiffers means a container name is recorded on another Wyrwood agent.
// An *AgentError carries it.
var ErrAgentDiffers = errors.New("recorded on another Wyrwood agent")

// AgentError says that a container name's binding is recorded on another
// agent than the one a change names. The recorded binding has to be confirmed
// gone, and the name forgotten, before the name can be recorded on another
// agent.
type AgentError struct {
	Name string
	// Recorded is the agent the name's binding is recorded on.
	Recorded string
	// Agent is the agent the change named, "" for none.
	Agent string
}

func (e *AgentError) Error() string {
	return fmt.Sprintf("the binding of container name %s is recorded on Wyrwood agent %s, not %q", e.Name, e.Recorded, e.Agent)
}

// Is makes errors.Is match ErrAgentDiffers.
func (e *AgentError) Is(target error) bool { return target == ErrAgentDiffers }

// ErrExists means the data holds a workspace of that name.
var ErrExists = errors.New("the workspace exists in Menagerie's data")

// TakenError says which workspace holds a container name or a key.
type TakenError struct {
	// What is "container name <name>" or "Xolotl key <policy>/<name>".
	What string
	// Holder is the workspace that holds it.
	Holder string
}

func (e *TakenError) Error() string {
	return fmt.Sprintf("%s belongs to workspace %s in Menagerie's data", e.What, e.Holder)
}

// Is makes errors.Is match ErrTaken.
func (e *TakenError) Is(target error) bool { return target == ErrTaken }

// Store is Menagerie's data.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens the database at path, creating it and its folder when missing,
// and upgrades an older schema before it returns. It refuses a database whose
// schema is newer than SchemaVersion.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	// One connection: every change is serialized, and foreign keys, which
	// SQLite sets per connection, hold for all of them.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("Menagerie's data %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Menagerie's data %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("Menagerie's data %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path is the database's file.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("Menagerie's data %s: read the schema version: %w", s.path, err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w: %s has schema version %d, and this Menagerie reads up to %d; run the newer Menagerie, or restore the data volume from a backup this release wrote", ErrNewer, s.path, version, SchemaVersion)
	}
	if version == SchemaVersion {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("Menagerie's data %s: %w", s.path, err)
	}
	defer func() { _ = tx.Rollback() }()
	for v := version; v < SchemaVersion; v++ {
		if _, err := tx.Exec(migrations[v]); err != nil {
			return fmt.Errorf("Menagerie's data %s: upgrade the schema to version %d: %w", s.path, v+1, err)
		}
	}
	// PRAGMA takes no parameter; the version is this package's own number.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return fmt.Errorf("Menagerie's data %s: %w", s.path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("Menagerie's data %s: upgrade the schema: %w", s.path, err)
	}
	return nil
}

// Tx is one transaction on the data.
type Tx struct {
	ctx context.Context
	tx  *sql.Tx
}

// Update runs fn in one transaction, which commits when fn returns nil and
// changes nothing otherwise. fn must not call the Store: the transaction holds
// its only connection.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("Menagerie's data: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&Tx{ctx: ctx, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("Menagerie's data: %w", err)
	}
	return nil
}

// Dry runs fn in a transaction it always rolls back: what fn writes, fn
// alone sees, and nothing is stored.
func (s *Store) Dry(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("Menagerie's data: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(&Tx{ctx: ctx, tx: tx})
}

func (t *Tx) exec(query string, args ...any) (sql.Result, error) {
	result, err := t.tx.ExecContext(t.ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	return result, nil
}

// Import runs fn and records the import's mark in one transaction, unless the
// mark is already there. It says whether fn ran.
func (s *Store) Import(ctx context.Context, id int, fn func(*Tx) error) (bool, error) {
	ran := false
	err := s.Update(ctx, func(tx *Tx) error {
		var done int
		if err := tx.tx.QueryRowContext(ctx, `SELECT count(*) FROM imports WHERE id = ?`, id).Scan(&done); err != nil {
			return fmt.Errorf("Menagerie's data: %w", err)
		}
		if done > 0 {
			return nil
		}
		if err := fn(tx); err != nil {
			return err
		}
		ran = true
		_, err := tx.exec(`INSERT INTO imports (id, done_at) VALUES (?, ?)`, id, time.Now().UTC().Format(time.RFC3339))
		return err
	})
	return ran && err == nil, err
}

// Imported says that the import's mark is in the data.
func (s *Store) Imported(ctx context.Context, id int) (done bool, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		var n int
		if err := t.tx.QueryRowContext(ctx, `SELECT count(*) FROM imports WHERE id = ?`, id).Scan(&n); err != nil {
			return fmt.Errorf("Menagerie's data: %w", err)
		}
		done = n > 0
		return nil
	})
	return done, err
}

// nullable stores "" as NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
