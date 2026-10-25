// ---
// relationships:
//   implements: authentication
// ---

package auth

import (
	"encoding/json"
	"html/template"
	"net/http"
)

// writeError answers with the workspace API's error envelope.
func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{"error": {"kind": kind, "message": message}})
}

// Page titles.
const (
	pageSignInFailed = "Sign-in failed"
	pageForbidden    = "Not a manager"
	pageSignedOut    = "Signed out"
)

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Menagerie</title>
<style>
body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;line-height:1.5}
a{color:inherit}
</style>
</head>
<body>
<main{{if .Kind}} data-kind="{{.Kind}}"{{end}}>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
<p><a href="/auth/login">Sign in</a></p>
</main>
</body>
</html>
`))

// page answers with a small HTML page. kind, when given, is the page's
// machine-readable reason, as in the API's error kinds.
func page(w http.ResponseWriter, status int, title, kind, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, struct{ Title, Kind, Message string }{title, kind, message})
}
