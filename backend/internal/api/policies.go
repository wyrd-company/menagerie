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
	"slices"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/wyrwood/operator"
)

// PolicySource reads Wyrwood's current grant and policy list for each request.
type PolicySource interface {
	Policies(context.Context) (operator.Grant, []operator.Policy, error)
}

type policyRow struct {
	Name       string   `json:"name"`
	ValueNames []string `json:"valueNames"`
	InGrant    bool     `json:"inGrant"`
}

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	var grant *operator.Grant
	policies := []policyRow{}
	if s.opts.Policies != nil {
		current, listed, err := s.opts.Policies.Policies(r.Context())
		if err != nil {
			writeError(w, 503, apiError{Kind: "wyrwood-unavailable", Message: err.Error()})
			return
		}
		// Wyrwood may omit empty grant lists; the browser contract uses arrays.
		if current.Agents == nil {
			current.Agents = []string{}
		}
		if current.Policies == nil {
			current.Policies = []string{}
		}
		if current.Actions == nil {
			current.Actions = []operator.Action{}
		}
		grant = &current
		for _, p := range listed {
			values := p.ValueNames
			if values == nil {
				values = []string{}
			}
			policies = append(policies, policyRow{p.Name, values, true})
		}
	}
	settings, selections, err := s.opts.Data.Policies(r.Context())
	if err != nil {
		writeError(w, 500, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	names := settings.Names()
	for _, selection := range selections {
		names = append(names, selection.Policies...)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		if !slices.ContainsFunc(policies, func(p policyRow) bool { return p.Name == name }) {
			policies = append(policies, policyRow{name, []string{}, false})
		}
	}
	slices.SortFunc(policies, func(a, b policyRow) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, 200, struct {
		Grant    *operator.Grant      `json:"grant"`
		Policies []policyRow          `json:"policies"`
		Settings store.PolicySettings `json:"settings"`
	}{grant, policies, settings})
}

func (s *Server) savePolicySettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Required *[]string `json:"required"`
		Groups   *[]struct {
			ID       *int64   `json:"id"`
			Name     string   `json:"name"`
			Mode     string   `json:"mode"`
			Policies []string `json:"policies"`
		} `json:"groups"`
		Revision *int64 `json:"revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, apiError{Kind: "invalid-request", Message: err.Error()})
		return
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) || request.Required == nil || request.Groups == nil || request.Revision == nil || *request.Revision < 0 {
		writeError(w, 400, apiError{Kind: "invalid-request", Message: "Send required, groups and a non-negative revision in one JSON document."})
		return
	}
	if s.opts.Policies == nil {
		writeError(w, 409, apiError{Kind: "provisioning-unavailable", Message: "Configure provisioning.wyrwood before changing policy settings."})
		return
	}
	_, policies, err := s.opts.Policies.Policies(r.Context())
	if err != nil {
		writeError(w, 503, apiError{Kind: "wyrwood-unavailable", Message: err.Error()})
		return
	}
	allowed := make([]string, 0, len(policies))
	for _, p := range policies {
		allowed = append(allowed, p.Name)
	}
	groups := make([]store.PolicyGroup, 0, len(*request.Groups))
	for _, group := range *request.Groups {
		g := store.PolicyGroup{Name: group.Name, Mode: group.Mode, Policies: group.Policies}
		if group.ID != nil {
			if *group.ID <= 0 {
				writeError(w, 422, apiError{Kind: "invalid-settings", Message: "A group id must name an existing group; omit id for a new group."})
				return
			}
			g.ID = *group.ID
		}
		groups = append(groups, g)
	}
	_, before, err := s.opts.Data.Policies(r.Context())
	if err != nil {
		s.writeRequestError(w, err)
		return
	}
	settings, err := s.opts.Data.SavePolicySettings(r.Context(), store.PolicySettings{Required: *request.Required, Groups: groups, Revision: *request.Revision}, allowed)
	if err != nil {
		var refusal *store.PolicyError
		if errors.As(err, &refusal) {
			status := 422
			if refusal.Kind == "settings-changed" {
				status = 409
			}
			writeError(w, status, apiError{Kind: refusal.Kind, Message: refusal.Message})
			return
		}
		writeError(w, 500, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	_, selections, err := s.opts.Data.Policies(r.Context())
	if err != nil {
		s.writeRequestError(w, err)
		return
	}
	for name, selection := range selections {
		if selection.Revision != before[name].Revision {
			if _, err = s.queuePolicies(name); err != nil {
				s.writeRequestError(w, err)
				return
			}
		}
	}
	writeJSON(w, 200, settings)
}
