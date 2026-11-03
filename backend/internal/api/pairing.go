// ---
// relationships:
//   implements: workspace-api
//   references: t3-code-pairing-token
// ---

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/t3code"
)

// PairingTokens mints T3 Code pairing tokens. *t3code.Minter satisfies it.
type PairingTokens interface {
	Mint(ctx context.Context, containerID string) (t3code.PairingToken, error)
}

// mintPairingToken answers a pairing token from the workspace's running
// container. It is no job: job logs and events keep what they carry, and
// nothing here may keep the token. It takes no job slot, since it changes
// neither the workspace nor its container.
func (s *Server) mintPairingToken(w http.ResponseWriter, r *http.Request) {
	folder, err := existingFolder(s.opts.Root, r.PathValue("name"))
	if err != nil {
		var re *RequestError
		if errors.As(err, &re) {
			writeError(w, re.Status, apiError{Kind: re.Kind, Message: re.Message})
			return
		}
		writeError(w, http.StatusInternalServerError, apiError{Kind: "internal-error", Message: err.Error()})
		return
	}
	if s.opts.PairingTokens == nil {
		writeError(w, http.StatusNotFound, apiError{Kind: "not-found", Message: "this service mints no pairing tokens"})
		return
	}
	status, err := s.opts.Status.Status(r.Context(), folder)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, apiError{Kind: "docker-unavailable", Message: err.Error()})
		return
	}
	if status.State != devcontainer.StateRunning {
		writePairingError(w, t3code.ErrNotRunning)
		return
	}
	token, err := s.opts.PairingTokens.Mint(r.Context(), status.ContainerID)
	if err != nil {
		writePairingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, token)
}

func writePairingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, t3code.ErrNotRunning):
		writeError(w, http.StatusConflict, apiError{Kind: "workspace-not-running", Message: err.Error()})
	case errors.Is(err, t3code.ErrNotInstalled):
		writeError(w, http.StatusConflict, apiError{Kind: "t3-code-not-installed", Message: err.Error()})
	case errors.Is(err, t3code.ErrFailed):
		writeError(w, http.StatusBadGateway, apiError{Kind: "pairing-token-failed", Message: err.Error()})
	default:
		writeError(w, http.StatusServiceUnavailable, apiError{Kind: "docker-unavailable", Message: err.Error()})
	}
}
