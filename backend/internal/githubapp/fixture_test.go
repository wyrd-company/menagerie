package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic values only.
const (
	sampleAppID        = 424242
	sampleInstallation = 7890
)

var appKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// localSigner signs as Transit would, with a key generated for the tests.
type localSigner struct {
	key *rsa.PrivateKey

	mu     sync.Mutex
	inputs []string
}

func (s *localSigner) Sign(_ context.Context, input []byte) ([]byte, error) {
	s.mu.Lock()
	s.inputs = append(s.inputs, string(input))
	s.mu.Unlock()
	sum := sha256.Sum256(input)
	return rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
}

// clock is a settable time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type recorded struct {
	method, path string
	body         map[string]any
	header       http.Header
}

// fakeGitHub is a GitHub API on a TLS server under /api/v3. It refuses a
// JWT that the test key did not sign or whose claims are not the App's at
// the clock's time.
type fakeGitHub struct {
	t      *testing.T
	clock  *clock
	server *httptest.Server
	// installed maps lower-case owner/repo to an installation.
	installed map[string]int64
	// expires is how long after the clock a token expires.
	expires time.Duration
	// answer, when set, answers instead of the fake's own routes.
	answer func(w http.ResponseWriter, r *http.Request) bool
	// gate, when set, holds every token request until it is closed.
	gate chan struct{}

	mu       sync.Mutex
	requests []recorded
	minted   int
	arrived  chan struct{}
}

var installationRoute = regexp.MustCompile(`^/api/v3/repos/([^/]+)/([^/]+)/installation$`)
var tokenRoute = regexp.MustCompile(`^/api/v3/app/installations/([0-9]+)/access_tokens$`)

func newGitHub(t *testing.T, c *clock) *fakeGitHub {
	g := &fakeGitHub{t: t, clock: c, installed: map[string]int64{}, expires: time.Hour, arrived: make(chan struct{}, 64)}
	g.server = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	rec := recorded{method: r.Method, path: r.URL.Path, header: r.Header.Clone()}
	_ = json.NewDecoder(r.Body).Decode(&rec.body)
	g.mu.Lock()
	g.requests = append(g.requests, rec)
	g.mu.Unlock()
	g.arrived <- struct{}{}
	if g.answer != nil && g.answer(w, r) {
		return
	}
	if err := g.checkJWT(r.Header.Get("Authorization")); err != nil {
		g.t.Errorf("JWT refused: %v", err)
		http.Error(w, "bad JWT", http.StatusUnauthorized)
		return
	}
	if m := installationRoute.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet {
		id, ok := g.installed[strings.ToLower(m[1]+"/"+m[2])]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"account":{"login":%q}}`, id, m[1])
		return
	}
	if m := tokenRoute.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodPost {
		if g.gate != nil {
			<-g.gate
		}
		g.mu.Lock()
		g.minted++
		n := g.minted
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"sample-token-%d","expires_at":%q}`, n, g.clock.Now().Add(g.expires).UTC().Format(time.RFC3339))
		return
	}
	http.NotFound(w, r)
}

func (g *fakeGitHub) checkJWT(authorization string) error {
	jwt, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return fmt.Errorf("no bearer JWT")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&appKey().PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return err
	}
	var header, claims map[string]any
	for seg, v := range map[string]*map[string]any{parts[0]: &header, parts[1]: &claims} {
		data, err := base64.RawURLEncoding.DecodeString(seg)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, v); err != nil {
			return err
		}
	}
	now := g.clock.Now().Unix()
	if header["alg"] != "RS256" || header["typ"] != "JWT" || len(header) != 2 {
		return fmt.Errorf("header %v", header)
	}
	if claims["iss"] != strconv.Itoa(sampleAppID) || claims["iat"] != float64(now-60) || claims["exp"] != float64(now+540) || len(claims) != 3 {
		return fmt.Errorf("claims %v at %d", claims, now)
	}
	return nil
}

func (g *fakeGitHub) seen() []recorded {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]recorded(nil), g.requests...)
}

func (g *fakeGitHub) count(method string) int {
	n := 0
	for _, r := range g.seen() {
		if r.method == method {
			n++
		}
	}
	return n
}

// minter is a Minter for g with installations.
func (g *fakeGitHub) minter(t *testing.T, installations map[string]int64) (*Minter, *localSigner) {
	t.Helper()
	signer := &localSigner{key: appKey()}
	m, err := New(Options{AppID: sampleAppID, Signer: signer, Installations: installations, Server: g.server.URL, Client: g.server.Client(), Now: g.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return m, signer
}

func newClock() *clock { return &clock{now: time.Unix(1_900_000_000, 0)} }

// fakeVault serves answer on a vault socket in a new short folder and
// returns its OpenBao address.
type fakeVault struct {
	address string

	mu      sync.Mutex
	headers []http.Header
	paths   []string
	bodies  []map[string]any
}

func newFakeVault(t *testing.T, answer func(w http.ResponseWriter, body map[string]any)) *fakeVault {
	t.Helper()
	dir, err := os.MkdirTemp("", "bao") // short path: socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "vault.sock"))
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVault{address: "unix://" + filepath.Join(dir, "vault.sock")}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		v.mu.Lock()
		v.headers = append(v.headers, r.Header.Clone())
		v.paths = append(v.paths, r.URL.Path)
		v.bodies = append(v.bodies, body)
		v.mu.Unlock()
		answer(w, body)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return v
}

// transitAnswer signs the request's input with the test key, as Transit
// does with pkcs1v15 and sha2-256, and answers it under prefix.
func transitAnswer(prefix string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, body map[string]any) {
		input, _ := body["input"].(string)
		data, err := base64.StdEncoding.DecodeString(input)
		if err != nil {
			http.Error(w, `{"errors":["bad input"]}`, http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(data)
		sig, _ := rsa.SignPKCS1v15(rand.Reader, appKey(), crypto.SHA256, sum[:])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"signature": prefix + base64.StdEncoding.EncodeToString(sig), "key_version": 1}})
	}
}

// statusAnswer answers status with body.
func statusAnswer(status int, body string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// testTransit is a Transit signer at address that does not retry, so a
// failure answers at once.
func testTransit(t *testing.T, address string) *Transit {
	t.Helper()
	signer, err := NewTransit(address, "sample-app-key")
	if err != nil {
		t.Fatal(err)
	}
	signer.client.SetMaxRetries(0)
	return signer
}
