// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

// Package githubapp mints GitHub App installation tokens for single
// repositories. The App's private key stays in OpenBao as a Transit key:
// the App JWT is signed through the Wyrwood vault socket and exchanged for
// an installation token scoped to one repository and one contents
// permission. Tokens are cached in memory until shortly before they
// expire; nothing is written to disk. Every error is a fixed, value-free
// reason: no token, JWT, signature, response body, URL or transport text.
package githubapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultServer is the GitHub server when configuration names none.
const DefaultServer = "https://github.com"

// Username is the user name that goes with an installation token in Git's
// HTTP basic authentication.
const Username = "x-access-token"

// Permission is the contents permission a token grants on its repository.
type Permission string

const (
	// Read lets a token fetch the repository.
	Read Permission = "read"
	// Write lets a token push to the repository.
	Write Permission = "write"
)

// Token lifetimes.
const (
	// issuedAtSkew backdates a JWT's iat for clock drift, as GitHub
	// recommends.
	issuedAtSkew = 60 * time.Second
	// jwtLifetime keeps a JWT's exp inside GitHub's ten minute limit.
	jwtLifetime = 9 * time.Minute
	// refreshBefore is how long before its expiry a cached token is
	// replaced.
	refreshBefore = 5 * time.Minute
)

// maxResponse bounds a response body read from GitHub or OpenBao.
const maxResponse = 1 << 20

// Error reports that an installation token could not be minted. Reason is
// a fixed description; it never holds a token, a JWT, a signature, a
// response body, a URL or a transport error's text.
type Error struct {
	Reason string
	cause  error
}

func (e *Error) Error() string { return "GitHub App credentials are unavailable: " + e.Reason }

// Unwrap returns the context error that ended the request, if any.
func (e *Error) Unwrap() error { return e.cause }

// fail is an *Error with reason, or the context's error when ctx ended.
func fail(ctx context.Context, reason, ended string) error {
	if err := ctx.Err(); err != nil {
		return &Error{Reason: ended, cause: err}
	}
	return &Error{Reason: reason}
}

// Signer signs a JWT signing input with RS256 (RSASSA-PKCS1-v1_5 over
// SHA-256) and returns the raw signature.
type Signer interface {
	Sign(ctx context.Context, input []byte) ([]byte, error)
}

// Options configure a Minter.
type Options struct {
	// AppID is the GitHub App's id, the JWT's issuer.
	AppID int64
	// Signer signs the App's JWTs.
	Signer Signer
	// Installations maps an account name, in any case, to the App's
	// installation on it. A repository whose owner is not listed has its
	// installation looked up.
	Installations map[string]int64
	// Server is the GitHub server's https origin; empty is DefaultServer.
	Server string
	// Client sends the GitHub API requests; nil is a client with the
	// default transport. Redirects are never followed.
	Client *http.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Minter mints and caches installation tokens. It is safe for concurrent
// use.
type Minter struct {
	appID         int64
	signer        Signer
	installations map[string]int64
	host          string
	api           string
	client        *http.Client
	now           func() time.Time

	mu       sync.Mutex
	tokens   map[tokenKey]cachedToken
	resolved map[string]int64
	minting  flights[tokenKey, string]
	lookups  flights[string, int64]
}

type tokenKey struct {
	installation int64
	repository   string
	permission   Permission
}

type cachedToken struct {
	token     string
	refreshAt time.Time
}

// New returns a Minter. It refuses options whose server is not a valid
// GitHub origin.
func New(opts Options) (*Minter, error) {
	server := opts.Server
	if server == "" {
		server = DefaultServer
	}
	host, api, err := origin(server)
	if err != nil {
		return nil, err
	}
	if opts.AppID <= 0 || opts.Signer == nil {
		return nil, errors.New("github app: an app id and a signer are required")
	}
	installations := make(map[string]int64, len(opts.Installations))
	for owner, id := range opts.Installations {
		installations[strings.ToLower(owner)] = id
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{}
	}
	// A copy, so the caller's client keeps its own redirect policy.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Minter{
		appID:         opts.AppID,
		signer:        opts.Signer,
		installations: installations,
		host:          host,
		api:           api,
		client:        &c,
		now:           now,
		tokens:        map[tokenKey]cachedToken{},
		resolved:      map[string]int64{},
	}, nil
}

// ValidServer reports whether server is a GitHub origin Menagerie
// accepts: an https URL with a host and no user information, path, query
// or fragment. The error names no value.
func ValidServer(server string) error {
	_, _, err := origin(server)
	return err
}

// origin returns the server's host, lower case with any :443 removed, and
// its API base URL.
func origin(server string) (host, api string, err error) {
	u, perr := url.Parse(server)
	if perr != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", errors.New("must be an https URL with a host and no user information, path, query or fragment")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", "", errors.New("must be an https URL with a valid port")
		}
	}
	host = canonicalHost(u)
	if host == "github.com" {
		return host, "https://api.github.com", nil
	}
	return host, "https://" + host + "/api/v3", nil
}

// canonicalHost is u's host in lower case, without the https default port.
func canonicalHost(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Host), ":443")
}

// Repository is a repository on the Minter's server.
type Repository struct {
	Owner string
	Name  string
}

func (r Repository) key() string { return strings.ToLower(r.Owner + "/" + r.Name) }

var (
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// ValidOwner reports whether owner has the shape of a GitHub account name.
func ValidOwner(owner string) bool { return len(owner) <= 100 && ownerPattern.MatchString(owner) }

// Repository returns the repository rawURL names, and false unless rawURL
// is an https URL on the Minter's server with no user information, query
// or fragment, whose path is exactly /{owner}/{repo} or /{owner}/{repo}.git.
// Only such a URL is ever given a token.
func (m *Minter) Repository(rawURL string) (Repository, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawPath != "" || canonicalHost(u) != m.host {
		return Repository{}, false
	}
	owner, name, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if !ok || !strings.HasPrefix(u.Path, "/") {
		return Repository{}, false
	}
	name = strings.TrimSuffix(name, ".git")
	if !ValidOwner(owner) || len(name) > 100 || !namePattern.MatchString(name) || name == "." || name == ".." {
		return Repository{}, false
	}
	return Repository{Owner: owner, Name: name}, true
}

// Token returns an installation token for repo alone with the contents
// permission perm. A cached token is returned until five minutes before
// it expires; concurrent callers for one repository and permission share
// one request. Requests are bounded by ctx only.
func (m *Minter) Token(ctx context.Context, repo Repository, perm Permission) (string, error) {
	if perm != Read && perm != Write {
		return "", &Error{Reason: "the permission is not read or write"}
	}
	installation, err := m.installation(ctx, repo)
	if err != nil {
		return "", err
	}
	key := tokenKey{installation: installation, repository: repo.key(), permission: perm}
	return m.minting.do(ctx, key, func(ctx context.Context) (string, error) {
		if token, ok := m.cached(key); ok {
			return token, nil
		}
		token, expires, err := m.mint(ctx, installation, repo, perm)
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Reason == reasonInstallationNotFound {
				m.forget(repo)
			}
			return "", err
		}
		m.store(key, token, expires)
		return token, nil
	})
}

func (m *Minter) cached(key tokenKey) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.tokens[key]
	if !ok || !m.now().Before(c.refreshAt) {
		return "", false
	}
	return c.token, true
}

// store caches token until refreshBefore its expiry, and drops the
// entries that are due for a refresh. A token that is already due is
// stored too; cached never returns it.
func (m *Minter) store(key tokenKey, token string, expires time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, c := range m.tokens {
		if !now.Before(c.refreshAt) {
			delete(m.tokens, k)
		}
	}
	m.tokens[key] = cachedToken{token: token, refreshAt: expires.Add(-refreshBefore)}
}

// installation is the installation configured for repo's owner, else the
// one GitHub names for repo, looked up once and remembered.
func (m *Minter) installation(ctx context.Context, repo Repository) (int64, error) {
	if id, ok := m.installations[strings.ToLower(repo.Owner)]; ok {
		return id, nil
	}
	m.mu.Lock()
	id, ok := m.resolved[repo.key()]
	m.mu.Unlock()
	if ok {
		return id, nil
	}
	return m.lookups.do(ctx, repo.key(), func(ctx context.Context) (int64, error) {
		jwt, err := m.jwt(ctx)
		if err != nil {
			return 0, err
		}
		id, err := m.lookup(ctx, jwt, repo)
		if err != nil {
			return 0, err
		}
		m.mu.Lock()
		m.resolved[repo.key()] = id
		m.mu.Unlock()
		return id, nil
	})
}

// forget drops repo's looked-up installation, so the next token looks it
// up again.
func (m *Minter) forget(repo Repository) {
	m.mu.Lock()
	delete(m.resolved, repo.key())
	m.mu.Unlock()
}

// jwt is a fresh App JWT, signed through the Signer.
func (m *Minter) jwt(ctx context.Context) (string, error) {
	input := signingInput(m.appID, m.now())
	sig, err := m.signer.Sign(ctx, []byte(input))
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return "", err
		}
		return "", fail(ctx, "the App JWT could not be signed", reasonEnded)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// signingInput is base64url(header) "." base64url(claims) of an App JWT
// issued at now.
func signingInput(appID int64, now time.Time) string {
	header := `{"alg":"RS256","typ":"JWT"}`
	claims, _ := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{now.Add(-issuedAtSkew).Unix(), now.Add(jwtLifetime).Unix(), strconv.FormatInt(appID, 10)})
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString(claims)
}

// Reasons shared between paths.
const (
	reasonEnded                = "the operation ended before GitHub answered"
	reasonUnreachable          = "GitHub could not be reached"
	reasonMalformed            = "GitHub returned a malformed response"
	reasonJWTRefused           = "GitHub refused the App's JWT"
	reasonInstallationNotFound = "the App's installation was not found"
)

// lookup asks GitHub for the App's installation on repo.
func (m *Minter) lookup(ctx context.Context, jwt string, repo Repository) (int64, error) {
	path := "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/installation"
	status, body, err := m.call(ctx, http.MethodGet, path, jwt, nil)
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return 0, &Error{Reason: reasonJWTRefused}
	case http.StatusNotFound:
		return 0, &Error{Reason: "the App is not installed for the repository"}
	default:
		return 0, &Error{Reason: fmt.Sprintf("GitHub answered status %d to the installation lookup", status)}
	}
	var doc struct {
		ID int64 `json:"id"`
	}
	// A body that is not JSON leaves doc empty.
	_ = json.Unmarshal(body, &doc)
	if doc.ID <= 0 {
		return 0, &Error{Reason: reasonMalformed}
	}
	return doc.ID, nil
}

// mint asks GitHub for a token of installation for repo alone with perm.
func (m *Minter) mint(ctx context.Context, installation int64, repo Repository, perm Permission) (string, time.Time, error) {
	jwt, err := m.jwt(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	request, _ := json.Marshal(map[string]any{
		"repositories": []string{repo.Name},
		"permissions":  map[string]Permission{"contents": perm},
	})
	path := "/app/installations/" + strconv.FormatInt(installation, 10) + "/access_tokens"
	status, body, err := m.call(ctx, http.MethodPost, path, jwt, request)
	if err != nil {
		return "", time.Time{}, err
	}
	switch status {
	case http.StatusCreated:
	case http.StatusUnauthorized:
		return "", time.Time{}, &Error{Reason: reasonJWTRefused}
	case http.StatusForbidden:
		return "", time.Time{}, &Error{Reason: "GitHub refused the token: the App lacks the permission"}
	case http.StatusNotFound:
		return "", time.Time{}, &Error{Reason: reasonInstallationNotFound}
	case http.StatusUnprocessableEntity:
		return "", time.Time{}, &Error{Reason: "GitHub refused the token: the repository is not in the installation, or the permission is not granted"}
	default:
		return "", time.Time{}, &Error{Reason: fmt.Sprintf("GitHub answered status %d to the token request", status)}
	}
	var doc struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	// A body that is not JSON leaves doc empty.
	_ = json.Unmarshal(body, &doc)
	if doc.Token == "" || doc.ExpiresAt.IsZero() {
		return "", time.Time{}, &Error{Reason: reasonMalformed}
	}
	return doc.Token, doc.ExpiresAt, nil
}

// call sends one GitHub API request with the JWT and returns the answer's
// status and body, read up to maxResponse.
func (m *Minter) call(ctx context.Context, method, path, jwt string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.api+path, reader)
	if err != nil {
		return 0, nil, fail(ctx, "the GitHub API request could not be built", reasonEnded)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "menagerie")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, nil, fail(ctx, reasonUnreachable, reasonEnded)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, fail(ctx, reasonUnreachable, reasonEnded)
	}
	return resp.StatusCode, data, nil
}

// flights runs one call per key at a time; callers for a key in flight
// wait for its result.
type flights[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flight[V]
}

type flight[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// do runs fn for key, or waits for the call in flight for key. A waiter
// whose own context is still live retries when the call it waited for
// ended with its caller's context.
func (f *flights[K, V]) do(ctx context.Context, key K, fn func(context.Context) (V, error)) (V, error) {
	for {
		f.mu.Lock()
		if f.calls == nil {
			f.calls = map[K]*flight[V]{}
		}
		c, waiting := f.calls[key]
		if !waiting {
			c = &flight[V]{done: make(chan struct{})}
			f.calls[key] = c
		}
		f.mu.Unlock()
		if !waiting {
			c.val, c.err = fn(ctx)
			f.mu.Lock()
			delete(f.calls, key)
			f.mu.Unlock()
			close(c.done)
			return c.val, c.err
		}
		select {
		case <-c.done:
		case <-ctx.Done():
			var zero V
			return zero, &Error{Reason: reasonEnded, cause: ctx.Err()}
		}
		if c.err != nil && ctx.Err() == nil && (errors.Is(c.err, context.Canceled) || errors.Is(c.err, context.DeadlineExceeded)) {
			continue
		}
		return c.val, c.err
	}
}

// Tokens is the Minter's source of tokens with one permission, for Git's
// HTTP basic authentication.
type Tokens struct {
	m    *Minter
	perm Permission
}

// Tokens returns the Minter's source of tokens with perm.
func (m *Minter) Tokens(perm Permission) Tokens { return Tokens{m: m, perm: perm} }

// Serves reports whether url names a repository on the Minter's server.
func (t Tokens) Serves(url string) bool {
	_, ok := t.m.Repository(url)
	return ok
}

// Token returns Username and a token for the repository url names.
func (t Tokens) Token(ctx context.Context, url string) (username, token string, err error) {
	repo, ok := t.m.Repository(url)
	if !ok {
		return "", "", &Error{Reason: "the URL is not an https repository URL on the GitHub App's server"}
	}
	token, err = t.m.Token(ctx, repo, t.perm)
	if err != nil {
		return "", "", err
	}
	return Username, token, nil
}
