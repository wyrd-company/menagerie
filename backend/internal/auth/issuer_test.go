package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// testUser is an identity the test issuer signs in.
type testUser struct {
	Subject string
	Name    string
	// Groups is the groups claim value; nil omits the claim.
	Groups any
}

// issuedGrant is an authorization code the test issuer handed out.
type issuedGrant struct {
	clientID, redirectURI, challenge, nonce string
	user                                    testUser
}

// testIssuer is an OpenID Connect provider over real HTTP: discovery,
// JWKS, an authorization endpoint that signs in its current user without a
// prompt, a token endpoint that checks the client, redirect URI, PKCE S256
// verifier and single use of each code, and userinfo. Fields change its
// behavior to model faulty or hostile providers.
type testIssuer struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	keyID  string
	client string
	// redirect is the registered redirect URI.
	redirect string

	mu sync.Mutex
	// user signs in at the authorization endpoint.
	user testUser
	// refuse, when set, answers the authorization request with this error.
	refuse string
	// tamper edits the ID token claims before signing.
	tamper func(map[string]any)
	// signWith, when set, signs ID tokens with this key instead.
	signWith *rsa.PrivateKey
	// userinfoGroups, when set, is the userinfo groups claim.
	userinfoGroups any
	// userinfoSubject, when set, replaces the userinfo subject.
	userinfoSubject string
	// omitUserinfo leaves userinfo_endpoint out of discovery.
	omitUserinfo bool
	// down makes discovery fail.
	down bool

	codes    map[string]issuedGrant
	access   map[string]testUser
	requests []string
	// authorizations records each authorization request's query.
	authorizations []url.Values
	// tokenForms records each token request's form.
	tokenForms []url.Values
}

func newTestIssuer(t *testing.T, clientID, redirect string) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &testIssuer{t: t, key: key, keyID: "test-key", client: clientID, redirect: redirect,
		codes: map[string]issuedGrant{}, access: map[string]testUser{}}
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

func (is *testIssuer) URL() string { return is.srv.URL }

func (is *testIssuer) set(f func(is *testIssuer)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	f(is)
}

func (is *testIssuer) requestCount(prefix string) int {
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

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (is *testIssuer) discovery(w http.ResponseWriter, _ *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	if is.down {
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
	if !is.omitUserinfo {
		doc["userinfo_endpoint"] = is.srv.URL + "/userinfo"
	}
	writeJSONBody(w, http.StatusOK, doc)
}

func (is *testIssuer) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSONBody(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: is.key.Public(), KeyID: is.keyID, Algorithm: "RS256", Use: "sig"}}})
}

func redirectWith(w http.ResponseWriter, r *http.Request, target string, v url.Values) {
	u, _ := url.Parse(target)
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (is *testIssuer) authorize(w http.ResponseWriter, r *http.Request) {
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
		redirectWith(w, r, is.redirect, url.Values{"error": {"invalid_request"}, "state": {q.Get("state")}})
		return
	}
	if is.refuse != "" {
		redirectWith(w, r, is.redirect, url.Values{"error": {is.refuse}, "state": {q.Get("state")}})
		return
	}
	code := randomToken()
	is.codes[code] = issuedGrant{clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), user: is.user}
	redirectWith(w, r, is.redirect, url.Values{"code": {code}, "state": {q.Get("state")}})
}

func tokenError(w http.ResponseWriter, code string) {
	writeJSONBody(w, http.StatusBadRequest, map[string]string{"error": code})
}

func (is *testIssuer) token(w http.ResponseWriter, r *http.Request) {
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
	if is.tamper != nil {
		is.tamper(claims)
	}
	key := is.key
	if is.signWith != nil {
		key = is.signWith
	}
	access := randomToken()
	is.access[access] = grant.user
	writeJSONBody(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": is.sign(key, claims),
	})
}

func (is *testIssuer) sign(key *rsa.PrivateKey, claims map[string]any) string {
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

func (is *testIssuer) userinfo(w http.ResponseWriter, r *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	user, ok := is.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body := map[string]any{"sub": user.Subject, "name": user.Name}
	if is.userinfoSubject != "" {
		body["sub"] = is.userinfoSubject
	}
	if is.userinfoGroups != nil {
		body["groups"] = is.userinfoGroups
	}
	writeJSONBody(w, http.StatusOK, body)
}
