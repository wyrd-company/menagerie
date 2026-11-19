// ---
// relationships:
//   implements: [workspace-api, admit-templates-through-the-workspace-service]
//   references: menagerie
// ---

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// Registry registers and changes templates, and checks them for new
// versions. *registry.Registry satisfies it.
type Registry interface {
	// With check, each runs its checks alone and stores nothing.
	Register(ctx context.Context, name, address, runnerImage string, check bool) (store.Template, error)
	Upgrade(ctx context.Context, id, version string, check bool) (store.Template, error)
	Edit(ctx context.Context, id string, name, runnerImage *string) (store.Template, error)
	// CheckVersions answers the template with its version check's record,
	// whatever the check's outcome.
	CheckVersions(ctx context.Context, id string) (store.Template, error)
}

// Template is a registered template as the browser sees it.
type Template struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Address        string   `json:"address"`
	CurrentVersion string   `json:"currentVersion"`
	RunnerImage    *string  `json:"runnerImage"`
	Workspaces     []string `json:"workspaces"`
	// NewestVersion is the newest found version, or null when none is.
	NewestVersion *string `json:"newestVersion"`
	// Versions are the found versions, in ascending order.
	Versions []TemplateVersion `json:"versions"`
	// CurrentMoved says that a check found the current version's tag at
	// another commit than the recorded one.
	CurrentMoved bool `json:"currentMoved"`
	// LastCheck is the last version check's end and failure, or null.
	LastCheck *LastCheck `json:"lastCheck"`
}

// TemplateVersion is a found version of a template.
type TemplateVersion struct {
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Commit  string `json:"commit"`
}

// LastCheck is when a template's last version check ended, and why it
// failed, or null when it succeeded.
type LastCheck struct {
	At      time.Time `json:"at"`
	Failure *string   `json:"failure"`
}

func templateOf(t store.Template) Template {
	out := Template{ID: t.ID, Name: t.Name, Address: t.Address, CurrentVersion: t.CurrentVersion, Workspaces: t.Workspaces,
		Versions: []TemplateVersion{}, CurrentMoved: t.CurrentMoved()}
	if t.RunnerImage != "" {
		image := t.RunnerImage
		out.RunnerImage = &image
	}
	if out.Workspaces == nil {
		out.Workspaces = []string{}
	}
	for _, v := range t.Versions {
		out.Versions = append(out.Versions, TemplateVersion{Version: v.Version, Tag: v.Tag, Commit: v.Commit})
	}
	if newest := t.Newest(); newest != "" {
		out.NewestVersion = &newest
	}
	if !t.CheckedAt.IsZero() {
		out.LastCheck = &LastCheck{At: t.CheckedAt.UTC()}
		if t.CheckFailure != "" {
			failure := t.CheckFailure
			out.LastCheck.Failure = &failure
		}
	}
	return out
}

// templateRef names a workspace's registered template on its row.
type templateRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	registered, err := s.opts.Data.Templates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	out := make([]Template, 0, len(registered))
	for _, t := range registered {
		out = append(out, templateOf(t))
	}
	writeJSON(w, http.StatusOK, map[string][]Template{"templates": out})
}

// decodeOne decodes the body, exactly one JSON object with only v's
// members, into v. No error quotes a value: an address can hold a password.
func decodeOne(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "the body must be one JSON object with only the route's members: " + jsonProblem(err)})
		return false
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "request body must hold exactly one JSON document"})
		return false
	}
	return true
}

// jsonProblem names a decode failure without quoting a value: an unknown
// member by its name, a mistyped one by its member.
func jsonProblem(err error) string {
	var typed *json.UnmarshalTypeError
	if errors.As(err, &typed) {
		return "member " + typed.Field + " has the wrong type"
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "the body is not JSON"
	}
	if errors.Is(err, io.EOF) {
		return "the body is empty"
	}
	// encoding/json's unknown-member error names the member, not a value.
	return err.Error()
}

func (s *Server) registerTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.registering(w) {
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Address     *string `json:"address"`
		RunnerImage *string `json:"runnerImage"`
		Check       bool    `json:"check"`
	}
	if !decodeOne(w, r, &req) {
		return
	}
	if req.Name == nil || req.Address == nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "name and address are required"})
		return
	}
	image := ""
	if req.RunnerImage != nil {
		image = *req.RunnerImage
	}
	tpl, err := s.opts.Registry.Register(r.Context(), *req.Name, *req.Address, image, req.Check)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	if req.Check {
		// The checks passed; nothing was stored.
		writeJSON(w, http.StatusOK, templateOf(tpl))
		return
	}
	w.Header().Set("Location", "/api/templates/"+tpl.ID)
	writeJSON(w, http.StatusCreated, templateOf(tpl))
}

func (s *Server) upgradeTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.registering(w) {
		return
	}
	var req struct {
		Version *string `json:"version"`
		Check   bool    `json:"check"`
	}
	if !decodeOne(w, r, &req) {
		return
	}
	if req.Version == nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "version is required"})
		return
	}
	tpl, err := s.opts.Registry.Upgrade(r.Context(), r.PathValue("id"), *req.Version, req.Check)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, templateOf(tpl))
}

// checkTemplateVersions checks a template for new versions on request and
// answers it with the check's record: a failed check, an unreachable
// template service included, is 200 with lastCheck's failure.
func (s *Server) checkTemplateVersions(w http.ResponseWriter, r *http.Request) {
	if !s.registering(w) {
		return
	}
	tpl, err := s.opts.Registry.CheckVersions(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, templateOf(tpl))
}

func (s *Server) editTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.registering(w) {
		return
	}
	var req struct {
		Name        *string         `json:"name"`
		RunnerImage json.RawMessage `json:"runnerImage"`
	}
	if !decodeOne(w, r, &req) {
		return
	}
	var image *string
	switch string(req.RunnerImage) {
	case "":
	case "null":
		none := ""
		image = &none
	default:
		var value string
		if err := json.Unmarshal(req.RunnerImage, &value); err != nil {
			writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "runnerImage must be a string or null"})
			return
		}
		image = &value
	}
	tpl, err := s.opts.Registry.Edit(r.Context(), r.PathValue("id"), req.Name, image)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, templateOf(tpl))
}

func (s *Server) removeTemplate(w http.ResponseWriter, r *http.Request) {
	if err := s.opts.Data.RemoveTemplate(r.Context(), r.PathValue("id")); err != nil {
		writeRegistryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// registering refuses a change when the server was composed with no
// registry to change templates with.
func (s *Server) registering(w http.ResponseWriter) bool {
	if s.opts.Registry == nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: "templates cannot be changed: the workspace service has no template registry"})
		return false
	}
	return true
}

// KindUnknownTemplate refuses a request that names no registered template.
const KindUnknownTemplate = "unknown-template"

func writeRegistryError(w http.ResponseWriter, err error) {
	var (
		check       *registry.CheckError
		invalid     *registry.InvalidError
		exists      *store.TemplateExistsError
		inUse       *store.TemplateInUseError
		unavailable *templates.UnavailableError
	)
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: invalid.Message})
	case errors.As(err, &check):
		writeError(w, http.StatusUnprocessableEntity, apiError{Kind: "template-check-failed", Message: check.Message, Check: check.Check})
	case errors.As(err, &exists):
		writeError(w, http.StatusConflict, apiError{Kind: "template-exists", Message: exists.Error()})
	case errors.As(err, &inUse):
		writeError(w, http.StatusConflict, apiError{Kind: "template-in-use", Message: inUse.Error() + "; delete those workspaces first"})
	case errors.Is(err, registry.ErrNotNewer):
		writeError(w, http.StatusConflict, apiError{Kind: "version-not-newer", Message: err.Error()})
	case errors.Is(err, store.ErrNoTemplate):
		writeError(w, http.StatusNotFound, apiError{Kind: KindUnknownTemplate, Message: "no registered template has this id"})
	case errors.As(err, &unavailable):
		writeError(w, http.StatusServiceUnavailable, apiError{Kind: "template-service-unavailable", Message: "the template service could not be reached or failed the request; try again"})
	default:
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
	}
}
