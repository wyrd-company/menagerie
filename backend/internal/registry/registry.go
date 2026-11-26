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
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/distribution/reference"

	"github.com/wyrd-company/menagerie/backend/internal/gitcreds"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// Definition is the file a template's content folder must hold.
const Definition = ".devcontainer/devcontainer.json"

// The registration checks, in the order they run.
const (
	CheckAddress    = "address"
	CheckCredential = "credential"
	CheckFetch      = "fetch"
	CheckVersion    = "version"
	CheckRead       = "read"
	CheckDefinition = "definition"
)

// TemplateService is the template service. *templates.Client satisfies it.
type TemplateService interface {
	ReadAddress(ctx context.Context, address string) (*templates.Address, error)
	ListVersions(ctx context.Context, address string, credential *templates.Credential) (*templates.Versions, error)
	Check(ctx context.Context, address, ref string, credential *templates.Credential) (*templates.Checked, error)
}

// Credentials checks that a credential fits a template's URL and
// authenticates to it. *gitcreds.Source satisfies it.
type Credentials interface {
	// Fit refuses, with a *gitcreds.FitError, a credential a template at
	// url cannot have.
	Fit(ctx context.Context, url string, c store.Credential) error
	// Authenticate refuses, with a *gitcreds.FitError, a credential that
	// fits url but cannot authenticate to it: a GitHub App that cannot
	// mint a token for the repository, which a public repository's fetch
	// never asks it for.
	Authenticate(ctx context.Context, url string, c store.Credential) error
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

// ErrNotNewer means the version is not a found version of the template
// newer than its current one, nor its current version while that version's
// tag has moved.
var ErrNotNewer = errors.New("the version is not a found version of the template newer than its current one, nor its current version while that version's tag has moved; check the template's versions to find new ones")

// Registry registers and changes templates in Menagerie's data, and checks
// them for new versions.
type Registry struct {
	Data      *store.Store
	Templates TemplateService
	// Credentials checks a registration's credential; nil, which only a
	// version check's Registry has, refuses every credential but the zero
	// one.
	Credentials Credentials
	Root        *workspace.Root
	// Now is the clock of the version checks; nil is time.Now.
	Now func() time.Time
	// CheckWait bounds one version check; zero is DefaultCheckWait.
	CheckWait time.Duration

	mu      sync.Mutex
	running map[string]*checkRun
	// shared, when set, is told under mu of each check that shares a
	// template's running check: a test's hook, nil otherwise.
	shared func(id string)
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

// Register checks the template at address and records it with its
// credential: address, the template service reads and admits the address;
// credential, the credential fits the address's URL and authenticates to
// it (Credentials.Fit, Credentials.Authenticate);
// fetch, the repository's
// versions can be listed; version, the list holds a version and an
// address's ref is a version's tag; fetch again, the current version can be
// fetched; read, Toha loads the template and its folder holds no link
// outside its checkout; definition, the content folder holds the
// definition. The current version is the address's tag, else the newest
// version, and the versions listed are its found versions, as of a version
// check. Every fetch uses the credential, and a refused fetch names it. A
// template whose source is registered is a *store.TemplateExistsError. It
// joins each workspace that has no template
// and whose applied record names the template's source. With check, it runs
// the same checks and refuses the same way, and answers the template it
// would record, storing nothing; without, it trusts no earlier check.
func (r *Registry) Register(ctx context.Context, name, address, runnerImage string, credential store.Credential, check bool) (store.Template, error) {
	if err := ValidName(name); err != nil {
		return store.Template{}, err
	}
	if err := ValidRunnerImage(runnerImage); err != nil {
		return store.Template{}, err
	}
	read, err := r.Templates.ReadAddress(ctx, address)
	if err != nil {
		return store.Template{}, refusal(CheckAddress, err, "")
	}
	if credential, err = r.fit(ctx, read.URL, credential); err != nil {
		return store.Template{}, err
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
	versions, err := r.Templates.ListVersions(ctx, read.Base, requested(credential))
	if err != nil {
		return store.Template{}, refusal(CheckFetch, err, gitcreds.Phrase(credential))
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
	checked, err := r.checkVersion(ctx, read.Base, credential, current, "register the template again")
	if err != nil {
		return store.Template{}, err
	}
	tpl := store.Template{
		Name: name, Address: read.Base, Source: read.Source, RunnerImage: runnerImage, Credential: credential,
		CurrentVersion: current.Version, CurrentTag: current.Tag, CurrentCommit: checked.Commit,
		Versions: found(versions.Versions), CheckedAt: r.now(),
	}
	joins, err := r.joins(ctx, tpl)
	if err != nil {
		return store.Template{}, err
	}
	record := r.Data.RegisterTemplate
	if check {
		record = r.Data.TryRegisterTemplate
	}
	return record(ctx, tpl, joins)
}

// fit is credential in the form a template records, once it fits url, or
// a *CheckError of the credential check.
func (r *Registry) fit(ctx context.Context, url string, credential store.Credential) (store.Credential, error) {
	credential, err := gitcreds.NormalCredential(credential)
	switch {
	case err != nil:
	case r.Credentials != nil:
		if err = r.Credentials.Fit(ctx, url, credential); err == nil {
			err = r.Credentials.Authenticate(ctx, url, credential)
		}
	case !credential.IsZero():
		err = &gitcreds.FitError{Message: "Menagerie cannot check credentials here"}
	}
	var fit *gitcreds.FitError
	if errors.As(err, &fit) {
		return credential, &CheckError{Check: CheckCredential, Message: fit.Message}
	}
	return credential, err
}

// requested is c as the template service's requests name it.
func requested(c store.Credential) *templates.Credential {
	return templates.CredentialOf(c.App, c.Key)
}

// found is the template service's versions as Menagerie's data keeps them.
func found(versions []templates.Version) []store.Version {
	out := make([]store.Version, 0, len(versions))
	for _, v := range versions {
		out = append(out, store.Version{Version: v.Version, Tag: v.Tag, Commit: v.Commit})
	}
	return out
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// checkVersion runs the fetch, read and definition checks of a version, at
// the commit its listing found: a fetch that resolves the tag to another
// commit means the tag moved since, which fails version, and again says
// what to do then.
func (r *Registry) checkVersion(ctx context.Context, address string, credential store.Credential, v templates.Version, again string) (*templates.Checked, error) {
	checked, err := r.Templates.Check(ctx, address, v.Tag, requested(credential))
	if err != nil {
		return nil, refusal(CheckFetch, err, gitcreds.Phrase(credential))
	}
	if checked.Commit != v.Commit {
		return nil, &CheckError{Check: CheckVersion, Message: fmt.Sprintf("the tag of version %s moved after its versions were listed: it was at commit %s and is now at %s; %s", v.Version, v.Commit, checked.Commit, again)}
	}
	if !checked.Definition {
		return nil, &CheckError{Check: CheckDefinition, Message: fmt.Sprintf("the template's files at version %s have no %s", v.Version, Definition)}
	}
	return checked, nil
}

// joins maps the workspaces with no template whose applied record names
// tpl's source to the applied version their record gives tpl.
func (r *Registry) joins(ctx context.Context, tpl store.Template) (map[string]store.AppliedVersion, error) {
	names, err := r.Data.Untemplated(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]store.AppliedVersion{}
	for _, name := range names {
		folder, err := r.Root.Folder(name)
		if err != nil {
			continue
		}
		if source, version := Join(folder); source == tpl.Source {
			out[name] = version(tpl)
		}
	}
	return out, nil
}

// Upgrade makes version, a found version of the template newer than its
// current one, its current version, once it passes the fetch, read and
// definition checks; and it takes the current version again, with the
// commit those checks fetch, while that version's tag has moved. Any other
// version is ErrNotNewer, before any check. With check, it runs the same
// checks and refuses the same way, and answers the template at version,
// storing nothing.
func (r *Registry) Upgrade(ctx context.Context, id, version string, check bool) (store.Template, error) {
	tpl, err := r.Data.Template(ctx, id)
	if err != nil {
		return store.Template{}, err
	}
	target, ok := tpl.Found(version)
	again := version == tpl.CurrentVersion && tpl.CurrentMoved()
	if !ok || !again && !Newer(target.Version, tpl.CurrentVersion) {
		return store.Template{}, ErrNotNewer
	}
	checked, err := r.checkVersion(ctx, tpl.Address, tpl.Credential, templates.Version{Version: target.Version, Tag: target.Tag, Commit: target.Commit},
		"check the template's versions, then upgrade it again")
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

// URL is the Git URL of address, the URL a template's credential has to
// fit, once the template service admits it, reaching no network; a refused
// address fails the address check.
func (r *Registry) URL(ctx context.Context, address string) (string, error) {
	read, err := r.Templates.ReadAddress(ctx, address)
	if err != nil {
		return "", refusal(CheckAddress, err, "")
	}
	return read.URL, nil
}

// SetCredential checks the template id with credential, as Register checks
// a new one: the credential fits the template's URL, its versions can be
// listed, and its current version, at the commit the listing finds for
// its tag, passes the fetch, read and definition checks. It then records
// credential as the template's. With check, it runs the checks alone,
// answers the template it would record, and stores nothing.
func (r *Registry) SetCredential(ctx context.Context, id string, credential store.Credential, check bool) (store.Template, error) {
	tpl, err := r.Data.Template(ctx, id)
	if err != nil {
		return store.Template{}, err
	}
	read, err := r.Templates.ReadAddress(ctx, tpl.Address)
	if err != nil {
		return store.Template{}, refusal(CheckAddress, err, "")
	}
	if tpl.Credential, err = r.fit(ctx, read.URL, credential); err != nil {
		return store.Template{}, err
	}
	versions, err := r.Templates.ListVersions(ctx, tpl.Address, requested(tpl.Credential))
	if err != nil {
		return store.Template{}, refusal(CheckFetch, err, gitcreds.Phrase(tpl.Credential))
	}
	var current *templates.Version
	for i, v := range versions.Versions {
		if v.Tag == tpl.CurrentTag {
			current = &versions.Versions[i]
		}
	}
	if current == nil {
		return store.Template{}, &CheckError{Check: CheckVersion, Message: fmt.Sprintf("the repository no longer has the tag %s of the current version %s", tpl.CurrentTag, tpl.CurrentVersion)}
	}
	if _, err := r.checkVersion(ctx, tpl.Address, tpl.Credential, *current, "change the credential again"); err != nil {
		return store.Template{}, err
	}
	if check {
		return tpl, nil
	}
	return r.Data.SetTemplateCredential(ctx, id, tpl.Credential)
}

// refusal is the check a template service failure fails: a refused address
// fails address; a template Toha cannot load, or whose folder or link
// leaves its checkout, fails read; anything else the template service
// refuses fails check. Its message is chosen here for the refusal's kind,
// so it never quotes the address. An unreachable template service is its
// *templates.UnavailableError, unchanged, and a fault, a 5xx reply, is no
// failed check but one too, naming the status and kind alone. A refused
// fetch names the credential it used by phrase, gitcreds.Phrase's.
func refusal(check string, err error, phrase string) error {
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
		return &CheckError{Check: check, Message: "the template could not be fetched" + phrase + ": the Git host has no such repository"}
	}
	if check == CheckAddress {
		return &CheckError{Check: CheckAddress, Message: "the address could not be read (" + refused.Kind + ")"}
	}
	return &CheckError{Check: check, Message: "the template could not be fetched" + phrase + ": the Git host refused or failed the request (" + refused.Kind + ")"}
}
