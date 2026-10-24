// ---
// relationships:
//   implements: authentication
//   references: menagerie
// ---

// Package auth signs managers in through OpenID Connect and admits only
// requests that carry a manager session.
//
// Menagerie is a public OIDC client: it holds no client secret and uses the
// authorization code flow with PKCE (S256), a state value bound to the
// browser and a nonce. go-oidc verifies the ID token's issuer, audience,
// signature and expiry; golang.org/x/oauth2 builds the authorization request
// and exchanges the code. A signed-in browser holds a random session id;
// sessions live in this process's memory only, so a restart signs everyone
// out. Membership of the managers group is checked at every sign-in.
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// ManagersGroup is the group claim value that admits a user.
const ManagersGroup = "menagerie_managers"

// Routes of the sign-in flow.
const (
	LoginPath     = "/auth/login"
	CallbackPath  = "/auth/callback"
	LogoutPath    = "/auth/logout"
	SignedOutPath = "/auth/signed-out"
)

// DefaultScopes are requested when the configuration names none.
var DefaultScopes = []string{"openid", "profile", "groups"}

// Config names the identity provider and Menagerie's client registration.
type Config struct {
	// Issuer is the OIDC issuer URL; discovery reads
	// {Issuer}/.well-known/openid-configuration.
	Issuer string
	// ClientID is the public client's id.
	ClientID string
	// RedirectURL is the registered redirect URI. Its path is CallbackPath;
	// an https scheme makes every cookie Secure.
	RedirectURL string
	// Scopes are the requested scopes; empty means DefaultScopes.
	Scopes []string
	// HTTPClient makes requests to the identity provider; nil means
	// http.DefaultClient.
	HTTPClient *http.Client
}

// ValidateRedirectURL checks that raw is an absolute http or https URL
// whose path is CallbackPath, with no user info, query or fragment.
func ValidateRedirectURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must use http or https")
	case u.Host == "":
		return errors.New("must name a host")
	case u.User != nil:
		return errors.New("must not hold user information")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return errors.New("must not have a query or fragment")
	case u.EscapedPath() != CallbackPath:
		return fmt.Errorf("must have the path %s", CallbackPath)
	}
	return nil
}

// ValidateScopes checks that scopes, when given, include openid.
func ValidateScopes(scopes []string) error {
	if len(scopes) > 0 && !slices.Contains(scopes, "openid") {
		return errors.New("must include openid")
	}
	for _, s := range scopes {
		if s == "" || strings.ContainsAny(s, " \t\r\n\"\\") {
			return fmt.Errorf("%q is not a scope token", s)
		}
	}
	return nil
}

func (c *Config) validate() error {
	var errs []error
	if u, err := url.Parse(c.Issuer); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("issuer must be an http or https URL"))
	}
	if c.ClientID == "" {
		errs = append(errs, errors.New("client id is required"))
	}
	if err := ValidateRedirectURL(c.RedirectURL); err != nil {
		errs = append(errs, fmt.Errorf("redirect URL %w", err))
	}
	if err := ValidateScopes(c.Scopes); err != nil {
		errs = append(errs, fmt.Errorf("scopes %w", err))
	}
	return errors.Join(errs...)
}

// Authenticator runs the sign-in flow and gates every other route.
type Authenticator struct {
	cfg      Config
	secure   bool
	origin   string
	cookies  cookieNames
	pending  *pendingLogins
	sessions *sessions
}

// New returns an Authenticator. It does not contact the identity provider:
// discovery happens at each sign-in, so Menagerie starts while the provider
// is unreachable.
func New(cfg Config) (*Authenticator, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = DefaultScopes
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	u, _ := url.Parse(cfg.RedirectURL)
	secure := u.Scheme == "https"
	return &Authenticator{
		cfg:      cfg,
		secure:   secure,
		origin:   u.Scheme + "://" + u.Host,
		cookies:  newCookieNames(secure),
		pending:  newPendingLogins(maxPendingLogins),
		sessions: newSessions(),
	}, nil
}

// Mux registers routes; *api.Server and *http.ServeMux satisfy it.
type Mux interface {
	Handle(pattern string, h http.Handler)
}

// Register adds the sign-in routes to m.
func (a *Authenticator) Register(m Mux) {
	m.Handle("GET "+LoginPath, http.HandlerFunc(a.login))
	m.Handle("GET "+CallbackPath, http.HandlerFunc(a.callback))
	m.Handle("POST "+LogoutPath, http.HandlerFunc(a.logout))
	m.Handle("GET "+SignedOutPath, http.HandlerFunc(signedOut))
}

// publicPaths need no session. Login and callback must run before a
// session exists; logout must work for a browser whose session already
// ended; the signed-out page follows logout; health is for the supervisor
// and reveals nothing. Every other path, including routes added later,
// requires a manager session.
var publicPaths = map[string]bool{
	"/healthz":    true,
	LoginPath:     true,
	CallbackPath:  true,
	LogoutPath:    true,
	SignedOutPath: true,
}

// Wrap returns next behind cross-origin protection and the session gate.
// Cross-origin protection covers every route, including the public ones,
// so logout cannot be forged from another site.
func (a *Authenticator) Wrap(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	// The configured public origin is the browser's origin even when a
	// reverse proxy changes the Host header.
	if err := cop.AddTrustedOrigin(a.origin); err != nil {
		panic(fmt.Sprintf("auth: trusted origin %q: %v", a.origin, err))
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "cross-origin-request", "cross-origin requests that change state are refused")
	}))
	return cop.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.EscapedPath()] {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.sessions.get(a.sessionID(r)); ok {
			next.ServeHTTP(w, r)
			return
		}
		a.unauthenticated(w, r)
	}))
}

// unauthenticated sends a page request to sign-in and answers 401 to an
// API request, whose client starts sign-in itself.
func (a *Authenticator) unauthenticated(w http.ResponseWriter, r *http.Request) {
	isPage := (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/")
	if isPage {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", LoginPath+"?return="+url.QueryEscape(r.URL.RequestURI()))
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in at "+LoginPath)
}

func (a *Authenticator) sessionID(r *http.Request) string {
	c, err := r.Cookie(a.cookies.session)
	if err != nil {
		return ""
	}
	return c.Value
}
