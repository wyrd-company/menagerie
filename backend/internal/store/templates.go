// ---
// relationships:
//   implements: menagerie
//   references: workspace-api
// ---

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Template is a registered template: its permanent id, its display name,
// its address without a ref and the source Toha names its repository and
// folder by, its runner image, and its current version with the tag that
// gives it and the commit its checks fetched.
type Template struct {
	ID      string
	Name    string
	Address string
	Source  string
	// RunnerImage is the image of its hooks' throwaway containers, or "".
	RunnerImage string
	// Credential is the one credential its Git connections use, or the
	// zero Credential for a template an earlier release registered or
	// imported, whose URL chooses it.
	Credential     Credential
	CurrentVersion string
	CurrentTag     string
	// CurrentCommit is "" for an imported template no check has fetched.
	CurrentCommit string
	// Workspaces are the workspaces that use it, in name order.
	Workspaces []string
	// Versions are the versions its last successful version check found,
	// or its registration listed, in ascending order.
	Versions []Version
	// CheckedAt is when its last version check ended; zero for none.
	CheckedAt time.Time
	// CheckFailure is why its last version check failed: the template
	// service's refusal kind or template-service-unavailable; "" when it
	// succeeded.
	CheckFailure string
}

// Version is a version of a template's repository: its numbers as
// MAJOR.MINOR.PATCH, the tag that gives it and that tag's commit.
type Version struct{ Version, Tag, Commit string }

// The failures a version check records.
const (
	CheckTemplateNotFound   = "template-not-found"
	CheckTemplateResolve    = "template-resolve-failed"
	CheckInvalidAddress     = "invalid-address"
	CheckServiceUnavailable = "template-service-unavailable"
)

// Newest is the newest found version, or "" when none was found.
func (t Template) Newest() string {
	if len(t.Versions) == 0 {
		return ""
	}
	return t.Versions[len(t.Versions)-1].Version
}

// Found is the found version v, if a check found it.
func (t Template) Found(v string) (Version, bool) {
	for _, found := range t.Versions {
		if found.Version == v {
			return found, true
		}
	}
	return Version{}, false
}

// CurrentMoved says that the last version check found the current
// version's tag at another commit than the recorded one, so that jobs
// refuse the current version until a manager upgrades the template to it
// again.
func (t Template) CurrentMoved() bool {
	found, ok := t.Found(t.CurrentVersion)
	return ok && t.CurrentCommit != "" && found.Commit != t.CurrentCommit
}

// AppliedVersion is what Menagerie knows of the template version last
// applied to a workspace: the version, "" while unknown, and the commit, "" when no
// record names one.
type AppliedVersion struct{ Version, Commit string }

// ErrTemplateExists means a template of that source is registered. A
// *TemplateExistsError carries it.
var ErrTemplateExists = errors.New("a template of this source is registered")

// TemplateExistsError names the registration a template's source has.
type TemplateExistsError struct{ ID, Name string }

func (e *TemplateExistsError) Error() string {
	return fmt.Sprintf("this template is registered already, as %s (%s)", e.Name, e.ID)
}

// Is makes errors.Is match ErrTemplateExists.
func (e *TemplateExistsError) Is(target error) bool { return target == ErrTemplateExists }

// ErrTemplateInUse means workspaces use a template. A *TemplateInUseError
// carries it.
var ErrTemplateInUse = errors.New("workspaces use this template")

// TemplateInUseError names the workspaces that use a template.
type TemplateInUseError struct{ Workspaces []string }

func (e *TemplateInUseError) Error() string {
	return "the template is used by workspace " + strings.Join(e.Workspaces, ", ")
}

// Is makes errors.Is match ErrTemplateInUse.
func (e *TemplateInUseError) Is(target error) bool { return target == ErrTemplateInUse }

// ErrNoTemplate means no template has that id.
var ErrNoTemplate = errors.New("no registered template has this id")

// ErrNotNewer means a version is not newer than the template's current one.
var ErrNotNewer = errors.New("the version is not newer than the current version")

const templateColumns = `id, name, address, source, coalesce(runner_image, ''), current_version, current_tag, coalesce(current_commit, ''),
	coalesce(checked_at, ''), coalesce(check_failure, ''), coalesce(credential_app, ''), coalesce(credential_key, '')`

func scanTemplate(row interface{ Scan(...any) error }) (Template, error) {
	var t Template
	var checked string
	err := row.Scan(&t.ID, &t.Name, &t.Address, &t.Source, &t.RunnerImage, &t.CurrentVersion, &t.CurrentTag, &t.CurrentCommit,
		&checked, &t.CheckFailure, &t.Credential.App, &t.Credential.Key)
	if err == nil && checked != "" {
		if t.CheckedAt, err = time.Parse(time.RFC3339Nano, checked); err != nil {
			err = fmt.Errorf("the last version check's time: %w", err)
		}
	}
	return t, err
}

// filled reads what a template's rows do not hold: its workspaces and its
// found versions.
func (t *Tx) filled(tpl Template) (Template, error) {
	var err error
	if tpl.Workspaces, err = t.templateWorkspaces(tpl.ID); err != nil {
		return Template{}, err
	}
	if tpl.Versions, err = t.templateVersions(tpl.ID); err != nil {
		return Template{}, err
	}
	return tpl, nil
}

func (t *Tx) templateVersions(id string) ([]Version, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT version, tag, commit_id FROM template_versions WHERE template = ? ORDER BY position`, id)
	if err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	out := []Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.Version, &v.Tag, &v.Commit); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("Menagerie's data: %w", err)
		}
		out = append(out, v)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	return out, nil
}

// Templates lists the registered templates by name without regard to case,
// then by name and id, each with the workspaces that use it.
func (t *Tx) Templates() ([]Template, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT `+templateColumns+` FROM templates ORDER BY name COLLATE NOCASE, name, id`)
	if err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	var out []Template
	for rows.Next() {
		tpl, err := scanTemplate(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("Menagerie's data: %w", err)
		}
		out = append(out, tpl)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("Menagerie's data: %w", err)
	}
	for i := range out {
		if out[i], err = t.filled(out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Template reads one registered template, or ErrNoTemplate.
func (t *Tx) Template(id string) (Template, error) {
	tpl, err := scanTemplate(t.tx.QueryRowContext(t.ctx, `SELECT `+templateColumns+` FROM templates WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Template{}, fmt.Errorf("%w: %s", ErrNoTemplate, id)
	}
	if err != nil {
		return Template{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	return t.filled(tpl)
}

func (t *Tx) templateWorkspaces(id string) ([]string, error) {
	names, err := t.names(`SELECT name FROM workspaces WHERE template = ? ORDER BY name`, id)
	if names == nil {
		names = []string{}
	}
	return names, err
}

// TemplateID makes a template's permanent id from its name: the lower-case
// letters and digits of the name, separated by single dashes, or
// "template" when the name has none.
func TemplateID(name string) string {
	if id := slug(name); id != "" {
		return id
	}
	return "template"
}

// slug is the lower-case letters and digits of name, separated by single
// dashes, or "" when it has none.
func slug(name string) string {
	var parts []string
	var part strings.Builder
	flush := func() {
		if part.Len() > 0 {
			parts = append(parts, part.String())
			part.Reset()
		}
	}
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			part.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(parts, "-")
}

// RegisterTemplate records tpl, whose ID is empty for a registration, which
// then takes TemplateID of its name with -2, -3 and on added while that id
// is taken, or an imported template's own id. A template whose source is
// registered is a *TemplateExistsError. Its Versions, with its CheckedAt,
// are the versions its registration listed, recorded as its last version
// check; none for an imported template. It joins the template to each
// workspace of joins that has no template, with the applied version and
// commit joins gives it, and returns the template as recorded.
func (t *Tx) RegisterTemplate(tpl Template, joins map[string]AppliedVersion) (Template, error) {
	var existing, existingName string
	err := t.tx.QueryRowContext(t.ctx, `SELECT id, name FROM templates WHERE source = ?`, tpl.Source).Scan(&existing, &existingName)
	switch {
	case err == nil:
		return Template{}, &TemplateExistsError{ID: existing, Name: existingName}
	case !errors.Is(err, sql.ErrNoRows):
		return Template{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	if tpl.ID == "" {
		base := TemplateID(tpl.Name)
		tpl.ID = base
		for n := 2; ; n++ {
			var taken int
			if err := t.tx.QueryRowContext(t.ctx, `SELECT count(*) FROM templates WHERE id = ?`, tpl.ID).Scan(&taken); err != nil {
				return Template{}, fmt.Errorf("Menagerie's data: %w", err)
			}
			if taken == 0 {
				break
			}
			tpl.ID = fmt.Sprintf("%s-%d", base, n)
		}
	}
	if _, err := t.exec(`INSERT INTO templates (id, name, address, source, runner_image, current_version, current_tag, current_commit, credential_app, credential_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tpl.ID, tpl.Name, tpl.Address, tpl.Source, nullable(tpl.RunnerImage), tpl.CurrentVersion, tpl.CurrentTag, nullable(tpl.CurrentCommit),
		nullable(tpl.Credential.App), nullable(tpl.Credential.Key)); err != nil {
		return Template{}, err
	}
	if !tpl.CheckedAt.IsZero() {
		if err := t.replaceVersions(tpl.ID, tpl.Versions, tpl.CheckedAt); err != nil {
			return Template{}, err
		}
	}
	for name, applied := range joins {
		result, err := t.exec(`UPDATE workspaces SET template = ? WHERE name = ? AND template IS NULL`, tpl.ID, name)
		if err != nil {
			return Template{}, err
		}
		if n, err := result.RowsAffected(); err != nil {
			return Template{}, fmt.Errorf("Menagerie's data: %w", err)
		} else if n > 0 {
			if err := t.SetAppliedVersion(name, applied); err != nil {
				return Template{}, err
			}
		}
	}
	return t.Template(tpl.ID)
}

// replaceVersions records versions, in ascending order, as the template's
// found versions and at as the end of its last version check, which
// succeeded.
func (t *Tx) replaceVersions(id string, versions []Version, at time.Time) error {
	if _, err := t.exec(`DELETE FROM template_versions WHERE template = ?`, id); err != nil {
		return err
	}
	for i, v := range versions {
		if _, err := t.exec(`INSERT INTO template_versions (template, position, version, tag, commit_id) VALUES (?, ?, ?, ?, ?)`,
			id, i, v.Version, v.Tag, v.Commit); err != nil {
			return err
		}
	}
	_, err := t.exec(`UPDATE templates SET checked_at = ?, check_failure = NULL WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), id)
	return err
}

// completeApplied gives each workspace of the template whose applied
// version is unknown the newest found version whose tag is at its applied
// commit, when one is.
func (t *Tx) completeApplied(id string) error {
	_, err := t.exec(`UPDATE workspaces SET applied_version = (
			SELECT version FROM template_versions
			WHERE template = workspaces.template AND commit_id = workspaces.applied_commit
			ORDER BY position DESC LIMIT 1)
		WHERE template = ? AND applied_version IS NULL AND applied_commit IS NOT NULL
		AND EXISTS (SELECT 1 FROM template_versions WHERE template = workspaces.template AND commit_id = workspaces.applied_commit)`, id)
	return err
}

// Workspaces lists the workspaces Menagerie's data holds, in name order.
func (t *Tx) Workspaces() ([]string, error) {
	return t.names(`SELECT name FROM workspaces ORDER BY name`)
}

// SetAppliedVersion records the version and commit last applied to a workspace,
// "" for unknown; the workspace gets its data when it has none.
func (t *Tx) SetAppliedVersion(workspace string, applied AppliedVersion) error {
	if err := t.AddWorkspace(workspace); err != nil {
		return err
	}
	_, err := t.exec(`UPDATE workspaces SET applied_version = ?, applied_commit = ? WHERE name = ?`,
		nullable(applied.Version), nullable(applied.Commit), workspace)
	return err
}

// AppliedVersion reads what Menagerie knows of the version last applied to a
// workspace.
func (t *Tx) AppliedVersion(workspace string) (AppliedVersion, error) {
	var version, commit sql.NullString
	err := t.tx.QueryRowContext(t.ctx, `SELECT applied_version, applied_commit FROM workspaces WHERE name = ?`, workspace).Scan(&version, &commit)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AppliedVersion{}, fmt.Errorf("Menagerie's data: %w", err)
	}
	return AppliedVersion{Version: version.String, Commit: commit.String}, nil
}

// SetAppliedVersion records the version and commit last applied to a
// workspace; see Tx.SetAppliedVersion. A version "" leaves it unknown, and
// the next version check of its template completes it from the commit.
func (s *Store) SetAppliedVersion(ctx context.Context, workspace string, applied AppliedVersion) error {
	return s.Update(ctx, func(t *Tx) error { return t.SetAppliedVersion(workspace, applied) })
}

// AppliedVersion reads what Menagerie knows of the version last applied to a
// workspace.
func (s *Store) AppliedVersion(ctx context.Context, workspace string) (applied AppliedVersion, err error) {
	err = s.Update(ctx, func(t *Tx) error { applied, err = t.AppliedVersion(workspace); return err })
	return applied, err
}

// AppliedVersions maps every workspace in the data to what Menagerie knows of the
// version last applied to it.
func (s *Store) AppliedVersions(ctx context.Context) (out map[string]AppliedVersion, err error) {
	out = map[string]AppliedVersion{}
	err = s.Update(ctx, func(t *Tx) error {
		rows, err := t.tx.QueryContext(t.ctx, `SELECT name, coalesce(applied_version, ''), coalesce(applied_commit, '') FROM workspaces`)
		if err != nil {
			return fmt.Errorf("Menagerie's data: %w", err)
		}
		for rows.Next() {
			var name string
			var a AppliedVersion
			if err := rows.Scan(&name, &a.Version, &a.Commit); err != nil {
				_ = rows.Close()
				return fmt.Errorf("Menagerie's data: %w", err)
			}
			out[name] = a
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return out, err
}

// RecordCheck records a template's successful version check with the
// credential used, which ended at at and found versions, in ascending
// order: they replace its found versions. An imported template whose
// current commit is unknown takes the commit, and the tag, of the found
// version that is its current version. Each of its workspaces whose
// applied version is unknown takes the newest version whose tag is at its
// applied commit. A check whose credential is no longer the template's,
// one a credential change superseded while it ran, records nothing, and
// the answer is the template as recorded.
func (s *Store) RecordCheck(ctx context.Context, id string, used Credential, versions []Version, at time.Time) (tpl Template, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		current, err := t.Template(id)
		if err != nil || current.Credential != used {
			tpl = current
			return err
		}
		tpl, err = t.recordCheck(current, versions, at)
		return err
	})
	return tpl, err
}

// recordCheck records versions, found at at, as current's version check;
// see RecordCheck.
func (t *Tx) recordCheck(current Template, versions []Version, at time.Time) (Template, error) {
	id := current.ID
	if err := t.replaceVersions(id, versions, at); err != nil {
		return Template{}, err
	}
	if current.CurrentCommit == "" {
		for _, v := range versions {
			if v.Version == current.CurrentVersion {
				if _, err := t.exec(`UPDATE templates SET current_tag = ?, current_commit = ? WHERE id = ?`, v.Tag, v.Commit, id); err != nil {
					return Template{}, err
				}
			}
		}
	}
	if err := t.completeApplied(id); err != nil {
		return Template{}, err
	}
	return t.Template(id)
}

// RecordCheckFailure records a template's failed version check with the
// credential used, which ended at at for failure, one of the Check kinds;
// its found versions stay. As with RecordCheck, a check a credential
// change superseded records nothing.
func (s *Store) RecordCheckFailure(ctx context.Context, id string, used Credential, failure string, at time.Time) (tpl Template, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		current, err := t.Template(id)
		if err != nil || current.Credential != used {
			tpl = current
			return err
		}
		if _, err := t.exec(`UPDATE templates SET checked_at = ?, check_failure = ? WHERE id = ?`, at.UTC().Format(time.RFC3339Nano), failure, id); err != nil {
			return err
		}
		tpl, err = t.Template(id)
		return err
	})
	return tpl, err
}

// RegisterTemplate records a template; see Tx.RegisterTemplate.
func (s *Store) RegisterTemplate(ctx context.Context, tpl Template, joins map[string]AppliedVersion) (recorded Template, err error) {
	err = s.Update(ctx, func(t *Tx) error { recorded, err = t.RegisterTemplate(tpl, joins); return err })
	return recorded, err
}

// TryRegisterTemplate answers what RegisterTemplate would record, its id
// and joined workspaces included, and its refusals, and stores nothing.
func (s *Store) TryRegisterTemplate(ctx context.Context, tpl Template, joins map[string]AppliedVersion) (recorded Template, err error) {
	err = s.Dry(ctx, func(t *Tx) error { recorded, err = t.RegisterTemplate(tpl, joins); return err })
	return recorded, err
}

// Templates lists the registered templates; see Tx.Templates.
func (s *Store) Templates(ctx context.Context) (templates []Template, err error) {
	err = s.Update(ctx, func(t *Tx) error { templates, err = t.Templates(); return err })
	return templates, err
}

// Template reads one registered template, or ErrNoTemplate.
func (s *Store) Template(ctx context.Context, id string) (tpl Template, err error) {
	err = s.Update(ctx, func(t *Tx) error { tpl, err = t.Template(id); return err })
	return tpl, err
}

// EditTemplate changes a template's name, its runner image ("" for none),
// or both; a nil value is left as it is.
func (s *Store) EditTemplate(ctx context.Context, id string, name, runnerImage *string) (tpl Template, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		if _, err := t.Template(id); err != nil {
			return err
		}
		if name != nil {
			if _, err := t.exec(`UPDATE templates SET name = ? WHERE id = ?`, *name, id); err != nil {
				return err
			}
		}
		if runnerImage != nil {
			if _, err := t.exec(`UPDATE templates SET runner_image = ? WHERE id = ?`, nullable(*runnerImage), id); err != nil {
				return err
			}
		}
		tpl, err = t.Template(id)
		return err
	})
	return tpl, err
}

// SetTemplateCredential records c as the credential of the registered
// template id, or is ErrNoTemplate, with versions, which a listing with c
// found at at, as its version check, replacing a check with the earlier
// credential. It returns the template as recorded.
func (s *Store) SetTemplateCredential(ctx context.Context, id string, c Credential, versions []Version, at time.Time) (tpl Template, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		current, err := t.Template(id)
		if err != nil {
			return err
		}
		if _, err := t.exec(`UPDATE templates SET credential_app = ?, credential_key = ? WHERE id = ?`, nullable(c.App), nullable(c.Key), id); err != nil {
			return err
		}
		current.Credential = c
		tpl, err = t.recordCheck(current, versions, at)
		return err
	})
	return tpl, err
}

// AdvanceTemplate makes version, with its tag and the commit its checks
// fetched, the template's current version. newer says whether one version
// is newer than another; a version that is not newer than the current one,
// read in this transaction, is ErrNotNewer, so the current version moves
// only forward. The current version itself is taken again, with the new
// commit, only while its tag has moved.
func (s *Store) AdvanceTemplate(ctx context.Context, id, version, tag, commit string, newer func(a, b string) bool) (tpl Template, err error) {
	err = s.Update(ctx, func(t *Tx) error { tpl, err = t.advanceTemplate(id, version, tag, commit, newer); return err })
	return tpl, err
}

// TryAdvanceTemplate answers what AdvanceTemplate would record, and its
// refusals, and stores nothing.
func (s *Store) TryAdvanceTemplate(ctx context.Context, id, version, tag, commit string, newer func(a, b string) bool) (tpl Template, err error) {
	err = s.Dry(ctx, func(t *Tx) error { tpl, err = t.advanceTemplate(id, version, tag, commit, newer); return err })
	return tpl, err
}

func (t *Tx) advanceTemplate(id, version, tag, commit string, newer func(a, b string) bool) (Template, error) {
	current, err := t.Template(id)
	if err != nil {
		return Template{}, err
	}
	again := version == current.CurrentVersion && current.CurrentMoved()
	if !again && !newer(version, current.CurrentVersion) {
		return Template{}, ErrNotNewer
	}
	if _, err := t.exec(`UPDATE templates SET current_version = ?, current_tag = ?, current_commit = ? WHERE id = ?`, version, tag, commit, id); err != nil {
		return Template{}, err
	}
	return t.Template(id)
}

// RemoveTemplate removes a template no workspace uses; one that workspaces
// use is a *TemplateInUseError naming them, and the foreign key refuses it
// too.
func (s *Store) RemoveTemplate(ctx context.Context, id string) error {
	return s.Update(ctx, func(t *Tx) error {
		tpl, err := t.Template(id)
		if err != nil {
			return err
		}
		if len(tpl.Workspaces) > 0 {
			return &TemplateInUseError{Workspaces: tpl.Workspaces}
		}
		_, err = t.exec(`DELETE FROM templates WHERE id = ?`, id)
		return err
	})
}

// SetWorkspaceTemplate records a workspace's template, giving the workspace
// its data when it has none.
func (t *Tx) SetWorkspaceTemplate(workspace, id string) error {
	if err := t.AddWorkspace(workspace); err != nil {
		return err
	}
	_, err := t.exec(`UPDATE workspaces SET template = ? WHERE name = ?`, id, workspace)
	return err
}

// WorkspaceTemplate is a workspace's template id, or "".
func (t *Tx) WorkspaceTemplate(workspace string) (string, error) {
	return t.holder(`SELECT coalesce(template, '') FROM workspaces WHERE name = ?`, workspace)
}

// WorkspaceTemplate is a workspace's template id, or "".
func (s *Store) WorkspaceTemplate(ctx context.Context, workspace string) (id string, err error) {
	err = s.Update(ctx, func(t *Tx) error { id, err = t.WorkspaceTemplate(workspace); return err })
	return id, err
}

// WorkspaceTemplates maps each workspace that has a template to it.
func (s *Store) WorkspaceTemplates(ctx context.Context) (out map[string]Template, err error) {
	out = map[string]Template{}
	err = s.Update(ctx, func(t *Tx) error {
		templates, err := t.Templates()
		if err != nil {
			return err
		}
		for _, tpl := range templates {
			for _, name := range tpl.Workspaces {
				out[name] = tpl
			}
		}
		return nil
	})
	return out, err
}

// Untemplated lists the workspaces that have no template, in name order.
func (s *Store) Untemplated(ctx context.Context) (names []string, err error) {
	err = s.Update(ctx, func(t *Tx) error {
		names, err = t.names(`SELECT name FROM workspaces WHERE template IS NULL ORDER BY name`)
		return err
	})
	return names, err
}

// JoinTemplates joins each workspace that has no template to the
// registered template whose source read gives it, with the applied version
// read's version gives against that template, read in the same
// transaction, and returns how many it joined. read answers "" for a
// workspace it cannot read.
func (s *Store) JoinTemplates(ctx context.Context, names []string, read func(name string) (source string, version func(Template) AppliedVersion)) (joined int, err error) {
	type record struct {
		source  string
		version func(Template) AppliedVersion
	}
	records := map[string]record{}
	for _, name := range names {
		if source, version := read(name); source != "" {
			records[name] = record{source, version}
		}
	}
	err = s.Update(ctx, func(t *Tx) error {
		joined = 0
		for name, r := range records {
			result, err := t.exec(`UPDATE workspaces SET template = (SELECT id FROM templates WHERE source = ?)
				WHERE name = ? AND template IS NULL AND EXISTS (SELECT 1 FROM templates WHERE source = ?)`, r.source, name, r.source)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("Menagerie's data: %w", err)
			}
			if n == 0 {
				continue
			}
			joined++
			id, err := t.WorkspaceTemplate(name)
			if err != nil {
				return err
			}
			tpl, err := t.Template(id)
			if err != nil {
				return err
			}
			if err := t.SetAppliedVersion(name, r.version(tpl)); err != nil {
				return err
			}
		}
		return nil
	})
	return joined, err
}
