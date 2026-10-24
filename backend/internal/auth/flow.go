// ---
// relationships:
//   implements: authentication
// ---

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// cookieNames are the session and login-binding cookie names. With https
// they take the __Host- prefix, which browsers accept only from a secure
// origin, with Path=/ and no Domain.
type cookieNames struct {
	session string
	login   string
}

func newCookieNames(secure bool) cookieNames {
	if secure {
		return cookieNames{session: "__Host-menagerie-session", login: "__Host-menagerie-login"}
	}
	return cookieNames{session: "menagerie-session", login: "menagerie-login"}
}

// cookie returns a browser-session cookie: no Expires or Max-Age, so it
// ends with the browser session or when Menagerie forgets its value.
// SameSite=Lax sends it on the identity provider's top-level redirect back
// to the callback and on links into Menagerie, not on cross-site
// subrequests or form posts.
func (a *Authenticator) cookie(name, value string) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode}
}

func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	c := a.cookie(name, "")
	c.MaxAge = -1
	http.SetCookie(w, c)
}

// provider discovers the identity provider. Discovery runs at each
// sign-in step, so Menagerie follows the provider's current endpoints and
// keys without a restart.
func (a *Authenticator) provider(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	ctx = oidc.ClientContext(ctx, a.cfg.HTTPClient)
	p, err := oidc.NewProvider(ctx, a.cfg.Issuer)
	if err != nil {
		return nil, nil, err
	}
	endpoint := p.Endpoint()
	// A public client sends its id in the form and no secret.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return p, &oauth2.Config{ClientID: a.cfg.ClientID, Endpoint: endpoint, RedirectURL: a.cfg.RedirectURL, Scopes: a.cfg.Scopes}, nil
}

// login starts a sign-in: GET /auth/login?return=<local path>.
func (a *Authenticator) login(w http.ResponseWriter, r *http.Request) {
	returnTo := localReturn(r.URL.Query().Get("return"))
	if _, ok := a.sessions.get(a.sessionID(r)); ok {
		redirect(w, returnTo)
		return
	}
	_, oc, err := a.provider(r.Context())
	if err != nil {
		log.Printf("auth: discovery at %s failed: %v", a.cfg.Issuer, err)
		page(w, http.StatusBadGateway, pageSignInFailed, "identity-provider-unavailable", "The identity provider cannot be reached. Try again later.")
		return
	}
	binding := ""
	if c, err := r.Cookie(a.cookies.login); err == nil && len(c.Value) == tokenLength {
		binding = c.Value
	}
	if binding == "" {
		binding = randomToken()
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	a.pending.add(state, pendingLogin{binding: binding, verifier: verifier, nonce: nonce, returnTo: returnTo})
	http.SetCookie(w, a.cookie(a.cookies.login, binding))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, oc.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusFound)
}

// callback completes a sign-in: GET /auth/callback?code&state, or
// ?error&state when the provider refused it.
func (a *Authenticator) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pending, ok := a.pending.take(q.Get("state"))
	var binding string
	if c, err := r.Cookie(a.cookies.login); err == nil {
		binding = c.Value
	}
	if !ok || !equalTokens(binding, pending.binding) {
		page(w, http.StatusBadRequest, pageSignInFailed, "sign-in-expired", "This sign-in is no longer pending in this browser. Start it again.")
		return
	}
	// RFC 9207: an authorization response that names its issuer must name
	// ours, so a code from another provider is never sent to our token
	// endpoint.
	if q.Has("iss") && q.Get("iss") != a.cfg.Issuer {
		log.Printf("auth: authorization response from issuer %q", sanitize(q.Get("iss")))
		page(w, http.StatusUnauthorized, pageSignInFailed, "sign-in-rejected", "The sign-in came from another identity provider. Start it again.")
		return
	}
	if code := q.Get("error"); code != "" {
		log.Printf("auth: identity provider refused sign-in: %s", sanitize(code))
		page(w, http.StatusUnauthorized, pageSignInFailed, "sign-in-refused", "The identity provider refused the sign-in ("+sanitize(code)+").")
		return
	}
	code := q.Get("code")
	if code == "" {
		page(w, http.StatusBadRequest, pageSignInFailed, "sign-in-invalid", "The identity provider sent no authorization code.")
		return
	}
	ctx := oidc.ClientContext(r.Context(), a.cfg.HTTPClient)
	p, oc, err := a.provider(r.Context())
	if err != nil {
		log.Printf("auth: discovery at %s failed: %v", a.cfg.Issuer, err)
		page(w, http.StatusBadGateway, pageSignInFailed, "identity-provider-unavailable", "The identity provider cannot be reached. Try again later.")
		return
	}
	var meta struct {
		IssParameter bool `json:"authorization_response_iss_parameter_supported"`
	}
	if err := p.Claims(&meta); err == nil && meta.IssParameter && !q.Has("iss") {
		log.Printf("auth: authorization response without the iss parameter its provider advertises")
		page(w, http.StatusUnauthorized, pageSignInFailed, "sign-in-rejected", "The sign-in did not name its identity provider. Start it again.")
		return
	}
	token, err := oc.Exchange(ctx, code, oauth2.VerifierOption(pending.verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode < 500 {
			log.Printf("auth: code exchange refused: status %d %s", re.Response.StatusCode, sanitize(re.ErrorCode))
			page(w, http.StatusUnauthorized, pageSignInFailed, "sign-in-rejected", "The identity provider did not accept the sign-in. Start it again.")
			return
		}
		log.Printf("auth: code exchange failed: %v", redactedError(err))
		page(w, http.StatusBadGateway, pageSignInFailed, "identity-provider-unavailable", "The identity provider cannot be reached. Try again later.")
		return
	}
	who, err := a.verify(ctx, p, token, pending.nonce)
	if err != nil {
		log.Printf("auth: sign-in rejected: %v", err)
		page(w, http.StatusUnauthorized, pageSignInFailed, "sign-in-rejected", "The sign-in could not be verified. Start it again.")
		return
	}
	// Whoever signed in now owns this browser: end any earlier session
	// before deciding, so a refused user never inherits it.
	if old := a.sessionID(r); old != "" {
		a.sessions.end(old)
		a.clearCookie(w, a.cookies.session)
	}
	if !slices.Contains(who.groups, ManagersGroup) {
		log.Printf("auth: %s signed in without %s membership", sanitize(who.Subject), ManagersGroup)
		page(w, http.StatusForbidden, pageForbidden, "not-a-manager", "You signed in as "+who.Name+", who is not a member of the "+ManagersGroup+" group.")
		return
	}
	// A fresh id at every sign-in, so no id chosen before sign-in survives
	// it.
	id := a.sessions.create(who.Session)
	http.SetCookie(w, a.cookie(a.cookies.session, id))
	redirect(w, pending.returnTo)
}

type identity struct {
	Session
	groups []string
}

// claims are the ID token and userinfo members Menagerie reads.
type claims struct {
	Subject           string          `json:"sub"`
	Name              string          `json:"name"`
	PreferredUsername string          `json:"preferred_username"`
	Email             string          `json:"email"`
	Groups            json.RawMessage `json:"groups"`
}

func (c claims) displayName() string {
	for _, v := range []string{c.Name, c.PreferredUsername, c.Email, c.Subject} {
		if v != "" {
			return v
		}
	}
	return ""
}

// verify checks the ID token and nonce and reads the groups claim, from the
// ID token or, when the ID token carries none, from userinfo.
func (a *Authenticator) verify(ctx context.Context, p *oidc.Provider, token *oauth2.Token, nonce string) (identity, error) {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return identity{}, errors.New("token response has no id_token")
	}
	idToken, err := p.Verifier(&oidc.Config{ClientID: a.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return identity{}, err
	}
	if !equalTokens(idToken.Nonce, nonce) {
		return identity{}, errors.New("id token nonce does not match the sign-in")
	}
	var c claims
	if err := idToken.Claims(&c); err != nil {
		return identity{}, err
	}
	who := identity{Session: Session{Subject: idToken.Subject, Name: c.displayName()}}
	groups := c.Groups
	if len(groups) == 0 && p.UserInfoEndpoint() != "" {
		info, err := p.UserInfo(ctx, oauth2.StaticTokenSource(token))
		if err != nil {
			return identity{}, errors.New("userinfo request failed")
		}
		// OIDC Core 5.3.2: userinfo must describe the ID token's subject.
		if info.Subject != idToken.Subject {
			return identity{}, errors.New("userinfo subject differs from the id token")
		}
		var u claims
		if err := info.Claims(&u); err != nil {
			return identity{}, err
		}
		groups = u.Groups
	}
	if len(groups) > 0 && string(groups) != "null" {
		if err := json.Unmarshal(groups, &who.groups); err != nil {
			return identity{}, errors.New("groups claim is not an array of strings")
		}
	}
	return who, nil
}

// logout ends the browser's session: POST /auth/logout. It needs no
// session, so a browser whose session already ended can still sign out,
// and cross-origin protection refuses a forged request. The identity
// provider's own session is left alone.
func (a *Authenticator) logout(w http.ResponseWriter, r *http.Request) {
	if id := a.sessionID(r); id != "" {
		a.sessions.end(id)
	}
	a.clearCookie(w, a.cookies.session)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", SignedOutPath)
	w.WriteHeader(http.StatusSeeOther)
}

func signedOut(w http.ResponseWriter, _ *http.Request) {
	page(w, http.StatusOK, pageSignedOut, "", "You are signed out of Menagerie.")
}

// localReturn returns raw when it is a path on this origin, and "/"
// otherwise. A path under /auth/ returns to "/", so sign-in cannot loop.
func localReturn(raw string) string {
	if raw == "" || raw[0] != '/' || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\x00") {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	if u.Path == "/auth" || strings.HasPrefix(u.Path, "/auth/") {
		return "/"
	}
	return raw
}

func redirect(w http.ResponseWriter, location string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}

// sanitize keeps a provider-supplied code or subject printable and short
// for logs and pages.
func sanitize(s string) string {
	const maxLen = 64
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxLen {
			break
		}
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// redactedError drops a URL error's URL, which names the provider's token
// endpoint and can carry query values.
func redactedError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
