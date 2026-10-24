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

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// StatusSource reports a workspace folder's container state.
type StatusSource interface {
	Status(ctx context.Context, folder string) (devcontainer.Status, error)
}

// Template is an approved template as the browser sees it.
type Template struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Options compose a Server.
type Options struct {
	Root       *workspace.Root
	Status     StatusSource
	Jobs       *jobs.Manager
	Operations Operations
	// Interviews takes answers for interview jobs. Nil takes none.
	Interviews Interviews
	Templates  []Template
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
		{Route{http.MethodGet, "/api/templates"}, s.listTemplates},
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
	s.mux.HandleFunc("GET /workspaces/{name}/upgrade", index)
}

type apiError struct {
	Kind        string `json:"kind"`
	Message     string `json:"message"`
	ActiveJobID string `json:"activeJobId,omitempty"`
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

func (s *Server) listTemplates(w http.ResponseWriter, _ *http.Request) {
	templates := s.opts.Templates
	if templates == nil {
		templates = []Template{}
	}
	writeJSON(w, http.StatusOK, map[string][]Template{"templates": templates})
}

type activeJob struct {
	JobID     string     `json:"jobId"`
	Operation string     `json:"operation"`
	State     jobs.State `json:"state"`
}

type workspaceRow struct {
	Name        string     `json:"name"`
	State       string     `json:"state"`
	ContainerID *string    `json:"containerId"`
	ActiveJob   *activeJob `json:"activeJob"`
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	names, err := s.opts.Root.Names()
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Kind: "workspace-root-unreadable", Message: err.Error()})
		return
	}
	rows := make([]workspaceRow, 0, len(names))
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
		if job, ok := s.opts.Jobs.Active(name); ok {
			snap := job.Snapshot()
			row.ActiveJob = &activeJob{JobID: snap.JobID, Operation: snap.Operation, State: snap.State}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string][]workspaceRow{"workspaces": rows})
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

// maxRequestBody bounds a JSON request body.
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
