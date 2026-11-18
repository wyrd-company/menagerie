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
	RunnerImage    string
	CurrentVersion string
	CurrentTag     string
	// CurrentCommit is "" for an imported template no check has fetched.
	CurrentCommit string
	// Workspaces are the workspaces that use it, in name order.
	Workspaces []string
}

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

const templateColumns = `id, name, address, source, coalesce(runner_image, ''), current_version, current_tag, coalesce(current_commit, '')`

func scanTemplate(row interface{ Scan(...any) error }) (Template, error) {
	var t Template
	err := row.Scan(&t.ID, &t.Name, &t.Address, &t.Source, &t.RunnerImage, &t.CurrentVersion, &t.CurrentTag, &t.CurrentCommit)
	return t, err
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
		if out[i].Workspaces, err = t.templateWorkspaces(out[i].ID); err != nil {
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
	tpl.Workspaces, err = t.templateWorkspaces(id)
	return tpl, err
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
	if len(parts) == 0 {
		return "template"
	}
	return strings.Join(parts, "-")
}

// RegisterTemplate records tpl, whose ID is empty for a registration, which
// then takes TemplateID of its name with -2, -3 and on added while that id
// is taken, or an imported template's own id. A template whose source is
// registered is a *TemplateExistsError. It joins the template to each of
// joins that has no template, and returns the template as recorded.
func (t *Tx) RegisterTemplate(tpl Template, joins []string) (Template, error) {
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
	if _, err := t.exec(`INSERT INTO templates (id, name, address, source, runner_image, current_version, current_tag, current_commit)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tpl.ID, tpl.Name, tpl.Address, tpl.Source, nullable(tpl.RunnerImage), tpl.CurrentVersion, tpl.CurrentTag, nullable(tpl.CurrentCommit)); err != nil {
		return Template{}, err
	}
	for _, name := range joins {
		if _, err := t.exec(`UPDATE workspaces SET template = ? WHERE name = ? AND template IS NULL`, tpl.ID, name); err != nil {
			return Template{}, err
		}
	}
	return t.Template(tpl.ID)
}

// RegisterTemplate records a template; see Tx.RegisterTemplate.
func (s *Store) RegisterTemplate(ctx context.Context, tpl Template, joins []string) (recorded Template, err error) {
	err = s.Update(ctx, func(t *Tx) error { recorded, err = t.RegisterTemplate(tpl, joins); return err })
	return recorded, err
}

// TryRegisterTemplate answers what RegisterTemplate would record, its id
// and joined workspaces included, and its refusals, and stores nothing.
func (s *Store) TryRegisterTemplate(ctx context.Context, tpl Template, joins []string) (recorded Template, err error) {
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

// AdvanceTemplate makes version, with its tag and the commit its checks
// fetched, the template's current version. newer says whether one version
// is newer than another; a version that is not newer than the current one,
// read in this transaction, is ErrNotNewer, so the current version moves
// only forward.
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
	if !newer(version, current.CurrentVersion) {
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
// registered template whose source sourceOf gives it, and returns how many
// it joined. sourceOf answers "" for a workspace it cannot read.
func (s *Store) JoinTemplates(ctx context.Context, names []string, sourceOf func(name string) string) (joined int, err error) {
	sources := map[string]string{}
	for _, name := range names {
		if source := sourceOf(name); source != "" {
			sources[name] = source
		}
	}
	err = s.Update(ctx, func(t *Tx) error {
		joined = 0
		for name, source := range sources {
			result, err := t.exec(`UPDATE workspaces SET template = (SELECT id FROM templates WHERE source = ?)
				WHERE name = ? AND template IS NULL AND EXISTS (SELECT 1 FROM templates WHERE source = ?)`, source, name, source)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("Menagerie's data: %w", err)
			}
			joined += int(n)
		}
		return nil
	})
	return joined, err
}
