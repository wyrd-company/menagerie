// ---
// relationships:
//   implements: [menagerie, admit-templates-through-the-workspace-service]
//   references: [workspace-api, template-service-api]
// ---

// Package registry keeps the templates workspaces come from: a manager
// registers a template from its address, and registration runs the minimum
// checks in order, refusing the template at the first that fails; a manager
// upgrades a template to a newer version of its repository, renames it,
// changes its runner image and removes one no workspace uses. Registration
// is the approval the template service relies on: this package names an
// address to the template service only for the request that registers it,
// and afterwards only a registered template's address with the tag and
// commit of its current version.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// Definition is the file a template's content folder must hold.
const Definition = ".devcontainer/devcontainer.json"

// The registration checks, in the order they run.
const (
	CheckAddress    = "address"
	CheckFetch      = "fetch"
	CheckVersion    = "version"
	CheckRead       = "read"
	CheckDefinition = "definition"
)

// TemplateService is the template service. *templates.Client satisfies it.
type TemplateService interface {
	ReadAddress(ctx context.Context, address string) (*templates.Address, error)
	ListVersions(ctx context.Context, address string) (*templates.Versions, error)
	Check(ctx context.Context, address, ref string) (*templates.Checked, error)
}

// CheckError refuses a template that failed a check. Its message names what
// failed and never quotes the address.
type CheckError struct {
	Check   string
	Message string
}

func (e *CheckError) Error() string { return e.Message }

// InvalidError refuses a request value: a name or a runner image.
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return e.Message }

// ErrNotNewer means the version is not one of the repository's versions
// newer than the template's current one.
var ErrNotNewer = errors.New("the version is not a version of the template's repository newer than its current version")

// Registry registers and changes templates in Menagerie's data.
type Registry struct {
	Data      *store.Store
	Templates TemplateService
	Root      *workspace.Root
}

// ValidName checks a template's display name: one line of 1 to 100
// characters.
func ValidName(name string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(name) {
		return &InvalidError{Message: "name must be one line of 1 to 100 characters"}
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return &InvalidError{Message: "name must be one line of 1 to 100 characters"}
		}
	}
	return nil
}

// ValidRunnerImage checks a runner image: "" for none, or a Docker image
// reference, as the configuration's runner-image is.
func ValidRunnerImage(image string) error {
	if image == "" {
		return nil
	}
	if _, err := reference.ParseNormalizedNamed(image); err != nil {
		return &InvalidError{Message: "runnerImage must be an image reference such as alpine:3.22"}
	}
	return nil
}

// Register checks the template at address and records it: address, the
// template service reads and admits the address; fetch, the repository's
// versions can be listed; version, the list holds a version and an
// address's ref is a version's tag; fetch again, the current version can be
// fetched; read, Toha loads the template and its folder holds no link
// outside its checkout; definition, the content folder holds the
// definition. The current version is the address's tag, else the newest
// version. A template whose source is registered is a
// *store.TemplateExistsError. It joins each workspace that has no template
// and whose applied record names the template's source. With check, it runs
// the same checks and refuses the same way, and answers the template it
// would record, storing nothing; without, it trusts no earlier check.
func (r *Registry) Register(ctx context.Context, name, address, runnerImage string, check bool) (store.Template, error) {
	if err := ValidName(name); err != nil {
		return store.Template{}, err
	}
	if err := ValidRunnerImage(runnerImage); err != nil {
		return store.Template{}, err
	}
	read, err := r.Templates.ReadAddress(ctx, address)
	if err != nil {
		return store.Template{}, refusal(CheckAddress, err)
	}
	existing, err := r.Data.Templates(ctx)
	if err != nil {
		return store.Template{}, err
	}
	for _, t := range existing {
		if t.Source == read.Source {
			return store.Template{}, &store.TemplateExistsError{ID: t.ID, Name: t.Name}
		}
	}
	versions, err := r.Templates.ListVersions(ctx, read.Base)
	if err != nil {
		return store.Template{}, refusal(CheckFetch, err)
	}
	if len(versions.Versions) == 0 {
		return store.Template{}, &CheckError{Check: CheckVersion, Message: "the repository has no version: a version is a tag MAJOR.MINOR.PATCH, with an optional leading v"}
	}
	current := versions.Versions[len(versions.Versions)-1]
	if read.Reference != nil {
		found := false
		for _, v := range versions.Versions {
			if v.Tag == *read.Reference {
				current, found = v, true
			}
		}
		if !found {
			return store.Template{}, &CheckError{Check: CheckVersion, Message: "the address's ref is not a version's tag: a version is a tag MAJOR.MINOR.PATCH, with an optional leading v"}
		}
	}
	checked, err := r.checkVersion(ctx, read.Base, current)
	if err != nil {
		return store.Template{}, err
	}
	joins, err := r.joins(ctx, read.Source)
	if err != nil {
		return store.Template{}, err
	}
	record := r.Data.RegisterTemplate
	if check {
		record = r.Data.TryRegisterTemplate
	}
	return record(ctx, store.Template{
		Name: name, Address: read.Base, Source: read.Source, RunnerImage: runnerImage,
		CurrentVersion: current.Version, CurrentTag: current.Tag, CurrentCommit: checked.Commit,
	}, joins)
}

// checkVersion runs the fetch, read and definition checks of a version.
func (r *Registry) checkVersion(ctx context.Context, address string, v templates.Version) (*templates.Checked, error) {
	checked, err := r.Templates.Check(ctx, address, v.Tag)
	if err != nil {
		return nil, refusal(CheckFetch, err)
	}
	if !checked.Definition {
		return nil, &CheckError{Check: CheckDefinition, Message: fmt.Sprintf("the template's files at version %s have no %s", v.Version, Definition)}
	}
	return checked, nil
}

// joins lists the workspaces with no template whose applied record names
// source.
func (r *Registry) joins(ctx context.Context, source string) ([]string, error) {
	names, err := r.Data.Untemplated(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		folder, err := r.Root.Folder(name)
		if err != nil {
			continue
		}
		if Source(folder) == source {
			out = append(out, name)
		}
	}
	return out, nil
}

// Upgrade makes version, one of the repository's versions newer than the
// template's current one, its current version, once it passes the fetch,
// read and definition checks. Any other version is ErrNotNewer. With check,
// it runs the same checks and refuses the same way, and answers the
// template at version, storing nothing.
func (r *Registry) Upgrade(ctx context.Context, id, version string, check bool) (store.Template, error) {
	tpl, err := r.Data.Template(ctx, id)
	if err != nil {
		return store.Template{}, err
	}
	versions, err := r.Templates.ListVersions(ctx, tpl.Address)
	if err != nil {
		return store.Template{}, refusal(CheckFetch, err)
	}
	var target *templates.Version
	for i, v := range versions.Versions {
		if v.Version == version {
			target = &versions.Versions[i]
		}
	}
	if target == nil || !Newer(target.Version, tpl.CurrentVersion) {
		return store.Template{}, ErrNotNewer
	}
	checked, err := r.checkVersion(ctx, tpl.Address, *target)
	if err != nil {
		return store.Template{}, err
	}
	advance := r.Data.AdvanceTemplate
	if check {
		advance = r.Data.TryAdvanceTemplate
	}
	tpl, err = advance(ctx, id, target.Version, target.Tag, checked.Commit, Newer)
	if errors.Is(err, store.ErrNotNewer) {
		return store.Template{}, ErrNotNewer
	}
	return tpl, err
}

// Edit renames a template, changes its runner image ("" for none), or both;
// nil leaves a value as it is. Its address and source do not change.
func (r *Registry) Edit(ctx context.Context, id string, name, runnerImage *string) (store.Template, error) {
	if name != nil {
		if err := ValidName(*name); err != nil {
			return store.Template{}, err
		}
	}
	if runnerImage != nil {
		if err := ValidRunnerImage(*runnerImage); err != nil {
			return store.Template{}, err
		}
	}
	return r.Data.EditTemplate(ctx, id, name, runnerImage)
}

// refusal is the check a template service failure fails: a refused address
// fails address; a template Toha cannot load, or whose folder or link
// leaves its checkout, fails read; anything else the template service
// refuses fails check. Its message is chosen here for the refusal's kind,
// so it never quotes the address. An unreachable template service is its
// *templates.UnavailableError, unchanged, and a fault, a 5xx reply, is no
// failed check but one too, naming the status and kind alone.
func refusal(check string, err error) error {
	var unavailable *templates.UnavailableError
	if errors.As(err, &unavailable) {
		return err
	}
	var refused *templates.Error
	if !errors.As(err, &refused) {
		return err
	}
	if refused.Status >= http.StatusInternalServerError {
		return &templates.UnavailableError{Err: fmt.Errorf("the template service failed the request (%d %s)", refused.Status, refused.Kind)}
	}
	switch refused.Kind {
	case templates.KindInvalidAddress:
		// The template service's admission refusals are fixed sentences
		// that never quote the address.
		return &CheckError{Check: CheckAddress, Message: "the address is refused: " + refused.Message}
	case "template-invalid":
		return &CheckError{Check: CheckRead, Message: "the template could not be read: Toha refused its manifest or its folder is not in its repository"}
	case "unsafe-template-link":
		return &CheckError{Check: CheckRead, Message: "the template could not be read: its folder, or a symbolic link in it, leads outside its repository"}
	case "template-not-found":
		return &CheckError{Check: check, Message: "the template could not be fetched: the Git host has no such repository"}
	}
	if check == CheckAddress {
		return &CheckError{Check: CheckAddress, Message: "the address could not be read (" + refused.Kind + ")"}
	}
	return &CheckError{Check: check, Message: "the template could not be fetched: the Git host refused or failed the request (" + refused.Kind + ")"}
}
