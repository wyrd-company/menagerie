// ---
// relationships:
//   references: authentication
// ---

// Package authtest is an OpenID Connect provider for tests.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// User is an identity the Issuer signs in.
type User struct {
	Subject string
	Name    string
	// Groups is the groups claim value; nil omits the claim.
	Groups any
}

// grant is an authorization code the test issuer handed out.
type grant struct {
	clientID, redirectURI, challenge, nonce string
	user                                    User
}

// Issuer is an OpenID Connect provider over real HTTP: discovery,
// JWKS, an authorization endpoint that signs in its current user without a
// prompt, a token endpoint that checks the client, redirect URI, PKCE S256
// verifier and single use of each code, and userinfo. Fields change its
// behavior to model faulty or hostile providers.
type Issuer struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	keyID  string
	client string
	// redirect is the registered redirect URI.
	redirect string

	mu sync.Mutex
	// User signs in at the authorization endpoint.
	User User
	// Refuse, when set, answers the authorization request with this error.
	Refuse string
	// Tamper edits the ID token claims before signing.
	Tamper func(map[string]any)
	// SignWith, when set, signs ID tokens with this key instead.
	SignWith *rsa.PrivateKey
	// SignHS256 advertises HS256 and signs ID tokens with HMAC-SHA256
	// keyed by a digest of the client id, as a provider that allows
	// symmetric algorithms might.
	SignHS256 bool
	// UserinfoGroups, when set, is the userinfo groups claim.
	UserinfoGroups any
	// UserinfoSubject, when set, replaces the userinfo subject.
	UserinfoSubject string
	// OmitUserinfo leaves userinfo_endpoint out of discovery.
	OmitUserinfo bool
	// Down makes discovery fail.
	Down bool
	// AdvertiseIss sets authorization_response_iss_parameter_supported.
	AdvertiseIss bool
	// Iss, when set, is sent as the iss parameter of every authorization
	// response (RFC 9207).
	Iss string

	codes          map[string]grant
	access         map[string]User
	requests       []string
	authorizations []url.Values
	tokenForms     []url.Values
}

// New starts an Issuer for one public client and redirect URI; it stops
// when the test ends.
func New(t *testing.T, clientID, redirect string) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{t: t, key: key, keyID: "test-key", client: clientID, redirect: redirect,
		codes: map[string]grant{}, access: map[string]User{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", is.discovery)
	mux.HandleFunc("GET /jwks", is.jwks)
	mux.HandleFunc("GET /authorize", is.authorize)
	mux.HandleFunc("POST /token", is.token)
	mux.HandleFunc("GET /userinfo", is.userinfo)
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		is.mu.Lock()
		is.requests = append(is.requests, r.Method+" "+r.URL.Path)
		is.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(is.srv.Close)
	return is
}

// URL is the issuer URL.
func (is *Issuer) URL() string { return is.srv.URL }

// Set changes the Issuer's behavior under its lock.
func (is *Issuer) Set(f func(is *Issuer)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	f(is)
}

// RequestCount counts requests whose "METHOD /path" starts with prefix.
func (is *Issuer) RequestCount(prefix string) int {
	is.mu.Lock()
	defer is.mu.Unlock()
	n := 0
	for _, r := range is.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// Authorizations returns each authorization request's query.
func (is *Issuer) Authorizations() []url.Values {
	is.mu.Lock()
	defer is.mu.Unlock()
	return slices.Clone(is.authorizations)
}

// TokenForms returns each token request's form.
func (is *Issuer) TokenForms() []url.Values {
	is.mu.Lock()
	defer is.mu.Unlock()
	return slices.Clone(is.tokenForms)
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (is *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	if is.Down {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	doc := map[string]any{
		"issuer":                                is.srv.URL,
		"authorization_endpoint":                is.srv.URL + "/authorize",
		"token_endpoint":                        is.srv.URL + "/token",
		"jwks_uri":                              is.srv.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	if is.SignHS256 {
		doc["id_token_signing_alg_values_supported"] = []string{"RS256", "HS256"}
	}
	if is.AdvertiseIss {
		doc["authorization_response_iss_parameter_supported"] = true
	}
	if !is.OmitUserinfo {
		doc["userinfo_endpoint"] = is.srv.URL + "/userinfo"
	}
	writeJSONBody(w, http.StatusOK, doc)
}

func (is *Issuer) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSONBody(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: is.key.Public(), KeyID: is.keyID, Algorithm: "RS256", Use: "sig"}}})
}

func (is *Issuer) redirectWith(w http.ResponseWriter, r *http.Request, target string, v url.Values) {
	if is.Iss != "" {
		v.Set("iss", is.Iss)
	}
	u, _ := url.Parse(target)
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (is *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	is.mu.Lock()
	defer is.mu.Unlock()
	is.authorizations = append(is.authorizations, q)
	if q.Get("client_id") != is.client || q.Get("redirect_uri") != is.redirect {
		http.Error(w, "unknown client or redirect", http.StatusBadRequest)
		return
	}
	if q.Get("response_type") != "code" || !strings.Contains(" "+q.Get("scope")+" ", " openid ") ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		is.redirectWith(w, r, is.redirect, url.Values{"error": {"invalid_request"}, "state": {q.Get("state")}})
		return
	}
	if is.Refuse != "" {
		is.redirectWith(w, r, is.redirect, url.Values{"error": {is.Refuse}, "state": {q.Get("state")}})
		return
	}
	code := randomToken()
	is.codes[code] = grant{clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), user: is.User}
	is.redirectWith(w, r, is.redirect, url.Values{"code": {code}, "state": {q.Get("state")}})
}

func tokenError(w http.ResponseWriter, code string) {
	writeJSONBody(w, http.StatusBadRequest, map[string]string{"error": code})
}

func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	f := r.PostForm
	is.mu.Lock()
	defer is.mu.Unlock()
	is.tokenForms = append(is.tokenForms, f)
	grant, ok := is.codes[f.Get("code")]
	delete(is.codes, f.Get("code")) // a code is single-use
	switch {
	case f.Get("grant_type") != "authorization_code" || !ok:
		tokenError(w, "invalid_grant")
		return
	case f.Get("client_id") != grant.clientID || f.Get("redirect_uri") != grant.redirectURI:
		tokenError(w, "invalid_grant")
		return
	case f.Has("client_secret"):
		tokenError(w, "invalid_client")
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge {
		tokenError(w, "invalid_grant")
		return
	}
	claims := map[string]any{
		"iss": is.srv.URL, "aud": grant.clientID, "sub": grant.user.Subject, "name": grant.user.Name,
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "nonce": grant.nonce,
	}
	if grant.user.Groups != nil {
		claims["groups"] = grant.user.Groups
	}
	if is.Tamper != nil {
		is.Tamper(claims)
	}
	key := is.key
	if is.SignWith != nil {
		key = is.SignWith
	}
	idToken := is.sign(key, claims)
	if is.SignHS256 {
		sum := sha256.Sum256([]byte(grant.clientID))
		idToken = is.signHS256(sum[:], claims)
	}
	access := randomToken()
	is.access[access] = grant.user
	writeJSONBody(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": idToken,
	})
}

func (is *Issuer) sign(key *rsa.PrivateKey, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", is.keyID))
	if err != nil {
		is.t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	sig, err := signer.Sign(payload)
	if err != nil {
		is.t.Fatal(err)
	}
	out, err := sig.CompactSerialize()
	if err != nil {
		is.t.Fatal(err)
	}
	return out
}

func (is *Issuer) signHS256(key []byte, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", is.keyID))
	if err != nil {
		is.t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	sig, err := signer.Sign(payload)
	if err != nil {
		is.t.Fatal(err)
	}
	out, _ := sig.CompactSerialize()
	return out
}

func (is *Issuer) userinfo(w http.ResponseWriter, r *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	user, ok := is.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body := map[string]any{"sub": user.Subject, "name": user.Name}
	if is.UserinfoSubject != "" {
		body["sub"] = is.UserinfoSubject
	}
	if is.UserinfoGroups != nil {
		body["groups"] = is.UserinfoGroups
	}
	writeJSONBody(w, http.StatusOK, body)
}
