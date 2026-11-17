// ---
// relationships:
//   implements: workspace-api
//   references: menagerie
// ---

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

const OperationApplyPolicies = "apply-policies"

// PolicyApplication imports and applies a workspace's stored policy selection.
type PolicyApplication interface {
	EnsureImported(context.Context, string) error
	ApplyPolicies(context.Context, string, devcontainer.Progress) error
}

func selectionError(err error) *RequestError {
	var policy *store.PolicyError
	if errors.As(err, &policy) {
		status := 422
		if policy.Kind == "selection-changed" || policy.Kind == "settings-changed" {
			status = 409
		}
		return &RequestError{status, policy.Kind, policy.Message}
	}
	return &RequestError{500, "internal-error", err.Error()}
}

func validPolicies(names []string) bool {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] || len(name) > 64 || !regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`).MatchString(name) {
			return false
		}
		seen[name] = true
	}
	return true
}

func (s *Server) createPolicies(ctx context.Context, req JobRequest) error {
	if !validPolicies(req.Policies) {
		return &RequestError{400, "invalid-request", "Policies must be a list of unique Wyrwood policy names."}
	}
	settings, _, err := s.opts.Data.Policies(ctx)
	if err != nil {
		return err
	}
	selected := slices.Concat(req.Policies, settings.Required)
	if s.opts.Provisioned && (len(req.Workspace) == 0 || len(req.Workspace) > 63 || !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`).MatchString(req.Workspace)) {
		return &RequestError{400, "invalid-workspace-name", "A provisioned workspace name must be 1 to 63 lowercase letters, digits, _ or -, starting with a letter or digit."}
	}
	if s.opts.Policies == nil {
		if len(selected) > 0 {
			return &RequestError{409, "provisioning-unavailable", "Configure provisioning.wyrwood before choosing workspace policies."}
		}
		if err = settings.ValidateChoices(selected, nil, true); err != nil {
			return selectionError(err)
		}
		return nil
	}
	_, policies, err := s.opts.Policies.Policies(ctx)
	if err != nil {
		return &RequestError{503, "wyrwood-unavailable", err.Error()}
	}
	allowed := []string{}
	for _, p := range policies {
		allowed = append(allowed, p.Name)
	}
	if err = store.CheckAllowed(selected, allowed); err != nil {
		return selectionError(err)
	}
	if err = settings.ValidateChoices(selected, nil, true); err != nil {
		return selectionError(err)
	}
	return nil
}

func (s *Server) saveWorkspacePolicies(w http.ResponseWriter, r *http.Request) {
	folder, err := ExistingFolder(s.opts.Root, r.PathValue("name"))
	if err != nil {
		s.writeRequestError(w, err)
		return
	}
	var req struct {
		Policies *[]string `json:"policies"`
		Revision *int64    `json:"revision"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&req); err != nil {
		writeError(w, 400, apiError{Kind: "invalid-request", Message: err.Error()})
		return
	}
	if err = dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) || req.Policies == nil || req.Revision == nil || *req.Revision < 0 || !validPolicies(*req.Policies) {
		writeError(w, 400, apiError{Kind: "invalid-request", Message: "Send unique policies and a non-negative revision in one JSON document."})
		return
	}
	if s.opts.Policies == nil || s.opts.PolicyApplication == nil {
		writeError(w, 409, apiError{Kind: "provisioning-unavailable", Message: "Configure provisioning.wyrwood before changing workspace policies."})
		return
	}
	// Import before any revision check, so a read before import cannot overwrite it.
	if err = s.opts.PolicyApplication.EnsureImported(r.Context(), folder); err != nil {
		writeError(w, 503, apiError{Kind: "wyrwood-unavailable", Message: err.Error()})
		return
	}
	_, policies, err := s.opts.Policies.Policies(r.Context())
	if err != nil {
		writeError(w, 503, apiError{Kind: "wyrwood-unavailable", Message: err.Error()})
		return
	}
	allowed := []string{}
	for _, p := range policies {
		allowed = append(allowed, p.Name)
	}
	current, err := s.opts.Data.SaveSelection(r.Context(), r.PathValue("name"), *req.Policies, *req.Revision, allowed)
	if err != nil {
		s.writeRequestError(w, selectionError(err))
		return
	}
	job, err := s.queuePolicies(r.PathValue("name"))
	if err != nil {
		s.writeRequestError(w, err)
		return
	}
	var id *string
	if job != nil {
		value := job.ID()
		id = &value
	}
	writeJSON(w, 200, struct {
		Revision int64   `json:"selectionRevision"`
		JobID    *string `json:"jobId"`
	}{current.Revision, id})
}

func (s *Server) writeRequestError(w http.ResponseWriter, err error) {
	var refusal *RequestError
	if errors.As(err, &refusal) {
		writeError(w, refusal.Status, apiError{Kind: refusal.Kind, Message: refusal.Message})
		return
	}
	writeError(w, 500, apiError{Kind: "internal-error", Message: err.Error()})
}

func (s *Server) queuePolicies(name string) (*jobs.Job, error) {
	if s.opts.PolicyApplication == nil {
		return nil, nil
	}
	return s.opts.Jobs.Enqueue(name, OperationApplyPolicies, func(ctx context.Context, run *jobs.Run) error {
		folder, err := ExistingFolder(s.opts.Root, name)
		if err != nil {
			return err
		}
		return s.opts.PolicyApplication.ApplyPolicies(ctx, folder, run)
	})
}

// ApplyPending starts each imported workspace whose binding lacks its selection.
// Failed attempts run once at start-up and are not repeated on a timer.
func (s *Server) ApplyPending(ctx context.Context) error {
	_, selections, err := s.opts.Data.Policies(ctx)
	if err != nil {
		return err
	}
	for name, selection := range selections {
		if !selection.Imported || selection.AppliedRevision == nil || *selection.AppliedRevision != selection.Revision {
			if _, err := ExistingFolder(s.opts.Root, name); err != nil {
				continue
			}
			if _, err = s.queuePolicies(name); err != nil {
				return err
			}
		}
	}
	return nil
}
