// ---
// relationships:
//   implements: [menagerie, choose-a-credential-per-backup-repository]
//   references: configuration
// ---

package githubapp

import (
	"context"
	"regexp"
	"strings"
)

// appNamePattern is the shape of an App's name: one name of letters, digits,
// '_', '.' and '-', as a Transit key's.
var appNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidName reports whether name is an App name Menagerie accepts:
// letters, digits, '_', '.' and '-', starting with a letter or digit, of
// at most 100 characters.
func ValidName(name string) bool { return len(name) <= 100 && appNamePattern.MatchString(name) }

// App is a configured GitHub App: the name a backup repository's
// credential calls it, its server's origin, and its Minter.
type App struct {
	Name   string
	Server string
	*Minter
}

// Apps are the configured Apps, in configuration order.
type Apps []App

// Named is the App of name, compared without regard to case, as the
// configuration compares names.
func (a Apps) Named(name string) (App, bool) {
	for _, app := range a {
		if strings.EqualFold(app.Name, name) {
			return app, true
		}
	}
	return App{}, false
}

// Serving is the first App whose server holds the repository url names,
// the App an earlier release, with one App, used for url.
func (a Apps) Serving(url string) (App, Repository, bool) {
	for _, app := range a {
		if repo, ok := app.Repository(url); ok {
			return app, repo, true
		}
	}
	return App{}, Repository{}, false
}

// Tokens gives each URL a token of perm from the first App that serves it,
// as Serving chooses.
func (a Apps) Tokens(perm Permission) FirstTokens { return FirstTokens{apps: a, perm: perm} }

// FirstTokens supplies tokens of one permission from the first App that
// serves a URL.
type FirstTokens struct {
	apps Apps
	perm Permission
}

// Serves reports whether an App serves url.
func (t FirstTokens) Serves(url string) bool {
	_, _, ok := t.apps.Serving(url)
	return ok
}

// Token returns Username and a token for the repository url names, from
// the first App that serves it.
func (t FirstTokens) Token(ctx context.Context, url string) (username, token string, err error) {
	app, _, ok := t.apps.Serving(url)
	if !ok {
		return "", "", &Error{Reason: "the URL is not an https repository URL on a GitHub App's server"}
	}
	return app.Tokens(t.perm).Token(ctx, url)
}
