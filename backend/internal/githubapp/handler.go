// ---
// relationships:
//   implements: menagerie
//   references: template-service-api
// ---

package githubapp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// CredentialPath is the template service's credential route on the hook
// runner socket.
const CredentialPath = "/v1/credentials/github"

// maxCredentialRequest bounds a credential request body: one URL.
const maxCredentialRequest = 8 << 10

// Handler serves CredentialPath: a contents read token for the one
// repository an https URL names, from the first of apps whose server holds
// it. No Apps, which is no github-app or github-apps configuration, serves
// no URL.
//
//	POST {"url": "https://github.com/{owner}/{repo}[.git]"}
//	200 {"username": "x-access-token", "password": token}
//	400, 405 {"reason"}  not a credential request
//	404 {"reason"}       no github-app, or not a repository URL on its server
//	502 {"reason"}       the token could not be minted; a fixed reason
func Handler(apps Apps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			reply(w, http.StatusMethodNotAllowed, map[string]string{"reason": "the route accepts POST only"})
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCredentialRequest))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || req.URL == "" || !errors.Is(dec.Decode(&struct{}{}), io.EOF) {
			reply(w, http.StatusBadRequest, map[string]string{"reason": "the request is not a credential request"})
			return
		}
		if len(apps) == 0 {
			reply(w, http.StatusNotFound, map[string]string{"reason": "no GitHub App is configured"})
			return
		}
		app, repo, ok := apps.Serving(req.URL)
		if !ok {
			reply(w, http.StatusNotFound, map[string]string{"reason": "the URL is not an https repository URL on the GitHub App's server"})
			return
		}
		token, err := app.Token(r.Context(), repo, Read)
		if err != nil {
			reason := "the token could not be minted"
			var e *Error
			if errors.As(err, &e) {
				reason = e.Reason
			}
			reply(w, http.StatusBadGateway, map[string]string{"reason": reason})
			return
		}
		reply(w, http.StatusOK, map[string]string{"username": Username, "password": token})
	})
}

func reply(w http.ResponseWriter, status int, body map[string]string) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
