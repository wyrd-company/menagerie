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
// repository an https URL names, from the App githubApp names, a
// template's credential, or without it from the first of apps whose server
// holds it. No Apps, which is no github-app or github-apps configuration,
// serves no URL.
//
//	POST {"url": "https://github.com/{owner}/{repo}[.git]", "githubApp"?: name}
//	200 {"username": "x-access-token", "password": token}
//	400, 405 {"reason"}  not a credential request
//	404 {"reason"}       without githubApp: no github-app, or not a
//	                     repository URL on its server
//	502 {"reason"}       the named App is not configured or does not serve
//	                     the URL, or the token could not be minted; a fixed
//	                     reason, naming the App
func Handler(apps Apps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			reply(w, http.StatusMethodNotAllowed, map[string]string{"reason": "the route accepts POST only"})
			return
		}
		var req struct {
			URL       string  `json:"url"`
			GitHubApp *string `json:"githubApp"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCredentialRequest))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || req.URL == "" || !errors.Is(dec.Decode(&struct{}{}), io.EOF) {
			reply(w, http.StatusBadRequest, map[string]string{"reason": "the request is not a credential request"})
			return
		}
		var (
			app  App
			repo Repository
			ok   bool
		)
		switch {
		case req.GitHubApp != nil:
			// A template that names its App fails without it, rather than
			// fetching as Toha does by default.
			name := *req.GitHubApp
			if !ValidName(name) {
				reply(w, http.StatusBadRequest, map[string]string{"reason": "the request is not a credential request"})
				return
			}
			if app, ok = apps.Named(name); !ok {
				reply(w, http.StatusBadGateway, map[string]string{"reason": "no GitHub App named " + name + " is configured"})
				return
			}
			if repo, ok = app.Repository(req.URL); !ok {
				reply(w, http.StatusBadGateway, map[string]string{"reason": "GitHub App " + app.Name + " does not serve the URL"})
				return
			}
		case len(apps) == 0:
			reply(w, http.StatusNotFound, map[string]string{"reason": "no GitHub App is configured"})
			return
		default:
			if app, repo, ok = apps.Serving(req.URL); !ok {
				reply(w, http.StatusNotFound, map[string]string{"reason": "the URL is not an https repository URL on the GitHub App's server"})
				return
			}
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
