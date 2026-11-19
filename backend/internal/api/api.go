// ---
// relationships:
//   implements: workspace-api
//   references: menagerie
// ---

// Package api serves the workspace service's browser API and, when given,
// the web UI's static files. Authentication wraps the returned handler.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// StateNoFolder is the state of a workspace whose folder is gone and that
// still holds a container name or a Xolotl key in Menagerie's data.
const StateNoFolder = "no-folder"

// StatusSource reports a workspace folder's container state.
type StatusSource interface {
	Status(ctx context.Context, folder string) (devcontainer.Status, error)
}

// Options compose a Server.
type Options struct {
	Root *workspace.Root
	// Data is Menagerie's data, which the workspace list joins to the folders.
	Data       *store.Store
	Status     StatusSource
	Jobs       *jobs.Manager
	Operations Operations
	// Interviews takes answers for interview jobs. Nil takes none.
	Interviews Interviews
	// PairingTokens mints T3 Code pairing tokens. Nil mints none.
	PairingTokens     PairingTokens
	Policies          PolicySource
	PolicyApplication PolicyApplication
	Provisioned       bool
	// Registry registers and changes templates.
	Registry Registry
	// AppliedRecord is the source a workspace folder's applied record
	// names, or "", and the applied version it gives a template of that
	// source; a folder listed for the first time joins the registered
	// template of that source with that applied version. Nil joins none.
	AppliedRecord func(folder string) (string, func(store.Template) store.AppliedVersion)
	// UI holds the built web UI (index.html and assets/). Nil serves no UI.
	UI fs.FS
}

// Server is the workspace service's HTTP surface.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// Route is one API route.
type Route struct {
	Method  string
	Pattern string
}

func (s *Server) routes() []struct {
	Route
	handler http.HandlerFunc
} {
	return []struct {
		Route
		handler http.HandlerFunc
	}{
		{Route{http.MethodGet, "/healthz"}, health},
		{Route{http.MethodGet, "/api/workspaces"}, s.listWorkspaces},
		{Route{http.MethodPost, "/api/workspaces/{name}/t3-code-pairing-tokens"}, s.mintPairingToken},
		{Route{http.MethodGet, "/api/policies"}, s.listPolicies},
		{Route{http.MethodPut, "/api/policies/settings"}, s.savePolicySettings},
		{Route{http.MethodPut, "/api/workspaces/{name}/policies"}, s.saveWorkspacePolicies},
		{Route{http.MethodGet, "/api/templates"}, s.listTemplates},
		{Route{http.MethodPost, "/api/templates"}, s.registerTemplate},
		{Route{http.MethodPatch, "/api/templates/{id}"}, s.editTemplate},
		{Route{http.MethodDelete, "/api/templates/{id}"}, s.removeTemplate},
		{Route{http.MethodPost, "/api/templates/{id}/upgrades"}, s.upgradeTemplate},
		{Route{http.MethodPost, "/api/templates/{id}/version-checks"}, s.checkTemplateVersions},
		{Route{http.MethodGet, "/api/jobs"}, s.listJobs},
		{Route{http.MethodPost, "/api/jobs"}, s.startJob},
		{Route{http.MethodGet, "/api/jobs/{id}"}, s.getJob},
		{Route{http.MethodGet, "/api/jobs/{id}/events"}, s.jobEvents},
		{Route{http.MethodPost, "/api/jobs/{id}/answers"}, s.answerJob},
	}
}

// Routes lists the API routes the Server implements.
func (s *Server) Routes() []Route {
	var out []Route
	for _, r := range s.routes() {
		out = append(out, r.Route)
	}
	return out
}

// New returns a Server.
func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	for _, r := range s.routes() {
		s.mux.HandleFunc(r.Method+" "+r.Pattern, r.handler)
	}
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, apiError{Kind: "not-found", Message: "no such API route"})
	})
	if opts.UI != nil {
		s.mountUI(opts.UI)
	}
	return s
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Handle registers an additional route, such as a later operation's
// routes. Patterns use net/http ServeMux syntax.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

func (s *Server) mountUI(ui fs.FS) {
	index := func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, ui, "index.html")
	}
	s.mux.Handle("GET /assets/", http.FileServerFS(ui))
	s.mux.HandleFunc("GET /{$}", index)
	s.mux.HandleFunc("GET /create", index)
	s.mux.HandleFunc("GET /policies", index)
	s.mux.HandleFunc("GET /templates", index)
	s.mux.HandleFunc("GET /workspaces/{name}/upgrade", index)
}

type apiError struct {
	Kind        string `json:"kind"`
	Message     string `json:"message"`
	ActiveJobID string `json:"activeJobId,omitempty"`
	// Check names the registration check a template-check-failed refusal
	// failed.
	Check string `json:"check,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e apiError) {
	writeJSON(w, status, map[string]apiError{"error": e})
}

type activeJob struct {
	JobID     string     `json:"jobId"`
	Operation string     `json:"operation"`
	State     jobs.State `json:"state"`
}

type workspaceRow struct {
	Policies          []string            `json:"policies"`
	SelectionRevision int64               `json:"selectionRevision"`
	AppliedRevision   *int64              `json:"appliedRevision"`
	ApplyFailure      *store.ApplyFailure `json:"applyFailure"`
	SelectionImported bool                `json:"selectionImported"`
	XolotlKey         *string             `json:"xolotlKey"`
	NeedsChoice       []string            `json:"needsChoice"`
	Template          *templateRef        `json:"template"`
	// AppliedVersion is the template version last applied, or null while
	// it is unknown; AppliedCommit is its commit, or null.
	AppliedVersion *string `json:"appliedVersion"`
	AppliedCommit  *string `json:"appliedCommit"`
	// NeedsUpgrade says that the template's current version is newer than
	// the applied version, which is known.
	NeedsUpgrade bool       `json:"needsUpgrade"`
	Name         string     `json:"name"`
	State        string     `json:"state"`
	ContainerID  *string    `json:"containerId"`
	ActiveJob    *activeJob `json:"activeJob"`
}

// listWorkspaces lists the workspace folders, each joined to its container and
// to its workspace in Menagerie's data, and the workspaces whose folder is gone
// that still hold a name or a key, as no-folder.
func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	names, err := s.opts.Root.Names()
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "workspace-root-unreadable", Message: err.Error()})
		return
	}
	unknown, err := s.opts.Data.Unknown(r.Context(), names)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	noFolder, err := s.opts.Data.Discover(r.Context(), names)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	// A folder listed for the first time joins the registered template
	// its applied record's source names.
	if s.opts.AppliedRecord != nil && len(unknown) > 0 {
		_, err := s.opts.Data.JoinTemplates(r.Context(), unknown, func(name string) (string, func(store.Template) store.AppliedVersion) {
			folder, err := s.opts.Root.Folder(name)
			if err != nil {
				return "", nil
			}
			return s.opts.AppliedRecord(folder)
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
			return
		}
	}
	rows := make([]workspaceRow, 0, len(names)+len(noFolder))
	for _, name := range names {
		folder, err := s.opts.Root.FolderPath(name)
		if err != nil {
			continue
		}
		status, err := s.opts.Status.Status(r.Context(), folder)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, apiError{Kind: "docker-unavailable", Message: err.Error()})
			return
		}
		row := workspaceRow{Name: name, State: status.State}
		if status.ContainerID != "" {
			id := status.ContainerID
			row.ContainerID = &id
		}
		rows = append(rows, s.withJob(row))
	}
	for _, name := range noFolder {
		rows = append(rows, s.withJob(workspaceRow{Name: name, State: StateNoFolder}))
	}
	settings, selections, err := s.opts.Data.Policies(r.Context())
	if err != nil {
		writeError(w, 500, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	used, err := s.opts.Data.WorkspaceTemplates(r.Context())
	if err != nil {
		writeError(w, 500, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	applied, err := s.opts.Data.AppliedVersions(r.Context())
	if err != nil {
		writeError(w, 500, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	for i := range rows {
		a := applied[rows[i].Name]
		if a.Version != "" {
			version := a.Version
			rows[i].AppliedVersion = &version
		}
		if a.Commit != "" {
			commit := a.Commit
			rows[i].AppliedCommit = &commit
		}
		if t, ok := used[rows[i].Name]; ok {
			rows[i].Template = &templateRef{ID: t.ID, Name: t.Name}
			rows[i].NeedsUpgrade = a.Version != "" && registry.Newer(t.CurrentVersion, a.Version)
		}
		selection := selections[rows[i].Name]
		rows[i].Policies = selection.Policies
		if rows[i].Policies == nil {
			rows[i].Policies = []string{}
		}
		rows[i].NeedsChoice = []string{}
		if selection.Imported {
			rows[i].NeedsChoice = settings.NeedsChoice(selection.Policies)
		}
		rows[i].SelectionRevision = selection.Revision
		rows[i].AppliedRevision = selection.AppliedRevision
		rows[i].ApplyFailure = selection.ApplyFailure
		rows[i].SelectionImported = selection.Imported
		if selection.Key != nil {
			key := selection.Key.String()
			rows[i].XolotlKey = &key
		}
	}
	slices.SortFunc(rows, func(a, b workspaceRow) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, map[string][]workspaceRow{"workspaces": rows})
}

// withJob names the workspace's unfinished job on its row.
func (s *Server) withJob(row workspaceRow) workspaceRow {
	if job, ok := s.opts.Jobs.Active(row.Name); ok {
		snap := job.Snapshot()
		row.ActiveJob = &activeJob{JobID: snap.JobID, Operation: snap.Operation, State: snap.State}
	}
	return row
}

func (s *Server) listJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]jobs.Snapshot{"jobs": s.opts.Jobs.List()})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.opts.Jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, apiError{Kind: "job-not-found", Message: "no job with this id in this process"})
		return
	}
	writeJSON(w, http.StatusOK, job.Snapshot())
}

// maxRequestBody bounds a job request body. An interview answer is not
// held to it: it carries the whole session.
const maxRequestBody = 1 << 20

func (s *Server) startJob(w http.ResponseWriter, r *http.Request) {
	var req JobRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: err.Error()})
		return
	}
	// The body must end after one JSON document: a second decode must reach
	// EOF, which also rejects trailing delimiters that More does not see.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: "request body must hold exactly one JSON document"})
		return
	}
	if req.Operation != OperationCreate && req.Policies != nil {
		s.writeRequestError(w, &RequestError{400, "invalid-request", "policies is only accepted for create."})
		return
	}
	if req.Operation == OperationCreate {
		if err := s.createPolicies(r.Context(), req); err != nil {
			s.writeRequestError(w, err)
			return
		}
	}
	op, ok := s.opts.Operations[req.Operation]
	if !ok {
		writeError(w, http.StatusBadRequest, apiError{Kind: "unknown-operation", Message: "operation " + quote(req.Operation) + " is not available"})
		return
	}
	runner, err := op(req)
	if err != nil {
		var re *RequestError
		if errors.As(err, &re) {
			writeError(w, re.Status, apiError{Kind: re.Kind, Message: re.Message})
			return
		}
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	job, err := s.opts.Jobs.Start(req.Workspace, req.Operation, runner)
	var busy *jobs.BusyError
	if errors.As(err, &busy) {
		writeError(w, http.StatusConflict, apiError{Kind: "workspace-busy", Message: busy.Error(), ActiveJobID: busy.ActiveJobID})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	w.Header().Set("Location", "/api/jobs/"+job.ID())
	writeJSON(w, http.StatusAccepted, job.Snapshot())
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
