package xolotl

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/openbaokv"
)

// Synthetic values only.
const (
	sampleKey  = "sentinel-scoped-key+/0a91"
	secretPath = "sample/scoped"
)

// fixedSecret answers with fixed fields or an error.
type fixedSecret struct {
	fields map[string]any
	err    error
	paths  []string
}

func (f *fixedSecret) Read(_ context.Context, path string) (map[string]any, error) {
	f.paths = append(f.paths, path)
	return f.fields, f.err
}

func scoped() *fixedSecret { return &fixedSecret{fields: map[string]any{"api_key": sampleKey}} }

func newClient(t *testing.T, rawURL string, secrets SecretReader) *Client {
	t.Helper()
	c, err := New(rawURL, secrets, secretPath)
	if err != nil {
		t.Fatal(err)
	}
	c.delay = time.Millisecond
	return c
}

// fakeXolotl keeps keys as Xolotl's keys API does and counts what would change
// a key. Its answers follow docs/user/policies-and-keys.md of Xolotl 0.6.0.
type fakeXolotl struct {
	t        *testing.T
	mu       sync.Mutex
	versions map[string]int
	revoked  map[string]bool
	bodies   []map[string]any
}

func newFakeXolotl(t *testing.T) (*fakeXolotl, *httptest.Server) {
	f := &fakeXolotl{t: t, versions: map[string]int{}, revoked: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/keys", f.issue)
	mux.HandleFunc("DELETE /admin/keys/{policy}/{name}", f.revoke)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeXolotl) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+sampleKey {
		http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeXolotl) issue(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("issue body: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, req)
	id := req["policy"].(string) + "/" + req["name"].(string)
	if f.versions[id] > 0 && !f.revoked[id] {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"exists","disabled":false,"expires_at":null,"vault_path":"keys/x"}`)
		return
	}
	f.versions[id]++
	f.revoked[id] = false
	w.WriteHeader(http.StatusCreated)
	fmt.Fprint(w, `{"status":"created","disabled":false,"expires_at":null,"vault_path":"keys/x"}`)
}

func (f *fakeXolotl) revoke(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := r.PathValue("policy") + "/" + r.PathValue("name")
	status := "revoked"
	switch {
	case f.versions[id] == 0:
		status = "absent"
	case f.revoked[id]:
		status = "already_revoked"
	}
	f.revoked[id] = f.versions[id] > 0
	fmt.Fprintf(w, `{"revoked":%q,"status":%q}`, id, status)
}

func TestIssueAndRevokeLifecycle(t *testing.T) {
	f, srv := newFakeXolotl(t)
	secrets := scoped()
	c := newClient(t, srv.URL, secrets)
	ctx := context.Background()

	got, err := c.Issue(ctx, "host-sample", "sample")
	if err != nil || !got.Created {
		t.Fatalf("first issue: %+v, %v", got, err)
	}
	got, err = c.Issue(ctx, "host-sample", "sample")
	if err != nil || got.Created {
		t.Fatalf("repeated issue: %+v, %v", got, err)
	}
	if v := f.versions["host-sample/sample"]; v != 1 {
		t.Fatalf("a repeated issue changed the key: %d versions", v)
	}
	for _, want := range []Revocation{Revoked, AlreadyRevoked} {
		if r, err := c.Revoke(ctx, "host-sample", "sample"); err != nil || r != want {
			t.Fatalf("revoke: got %q, %v, want %q", r, err, want)
		}
	}
	if r, err := c.Revoke(ctx, "host-sample", "other"); err != nil || r != Absent {
		t.Fatalf("revoke of no key: %q, %v", r, err)
	}
	if got, err := c.Issue(ctx, "host-sample", "sample"); err != nil || !got.Created {
		t.Fatalf("issue after a revoke: %+v, %v", got, err)
	}
	for _, path := range secrets.paths {
		if path != secretPath {
			t.Fatalf("read secret %q", path)
		}
	}
	if len(secrets.paths) != 6 {
		t.Fatalf("each operation reads the scoped key once: %d reads", len(secrets.paths))
	}
}

func TestIssueAsksForNoExpiry(t *testing.T) {
	f, srv := newFakeXolotl(t)
	if _, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample"); err != nil {
		t.Fatal(err)
	}
	if _, set := f.bodies[0]["ttl_secs"]; set {
		t.Fatalf("issue sent a lifetime: %v", f.bodies[0])
	}
}

func TestIssueReportsAStoredKeyThatIsNotUsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"exists","disabled":true,"expires_at":"2030-01-02T03:04:05Z"}`)
	}))
	defer srv.Close()
	got, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if got.Created || !got.Disabled || got.ExpiresAt == nil || !got.ExpiresAt.Equal(want) {
		t.Fatalf("got %+v", got)
	}
}

func TestURLPathPrefixIsKept(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"status":"created"}`)
			return
		}
		fmt.Fprint(w, `{"status":"absent"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL+"/gateway/", scoped())
	if _, err := c.Issue(context.Background(), "host-sample", "sample"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Revoke(context.Background(), "host-sample", "sample"); err != nil {
		t.Fatal(err)
	}
	want := "POST /gateway/admin/keys,DELETE /gateway/admin/keys/host-sample/sample"
	if got := strings.Join(paths, ","); got != want {
		t.Fatalf("got %s", got)
	}
}

// answering is a server that gives each answer in turn, then repeats the last.
func answering(t *testing.T, answers ...func(http.ResponseWriter)) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		i := min(count, len(answers)-1)
		count++
		mu.Unlock()
		answers[i](w)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

func status(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
}

func TestSendsAgainWhenXolotlSaysSo(t *testing.T) {
	for _, code := range []int{http.StatusConflict, http.StatusInternalServerError} {
		t.Run("issue "+strconv.Itoa(code), func(t *testing.T) {
			srv, count := answering(t, status(code, ""), status(http.StatusOK, `{"status":"exists"}`))
			if _, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample"); err != nil {
				t.Fatal(err)
			}
			if *count != 2 {
				t.Fatalf("%d requests", *count)
			}
		})
		t.Run("revoke "+strconv.Itoa(code), func(t *testing.T) {
			srv, count := answering(t, status(code, ""), status(http.StatusOK, `{"status":"revoked"}`))
			if r, err := newClient(t, srv.URL, scoped()).Revoke(context.Background(), "host-sample", "sample"); err != nil || r != Revoked {
				t.Fatalf("%q, %v", r, err)
			}
			if *count != 2 {
				t.Fatalf("%d requests", *count)
			}
		})
	}
}

func TestSendingAgainIsBounded(t *testing.T) {
	srv, count := answering(t, status(http.StatusInternalServerError, ""))
	_, err := newClient(t, srv.URL, scoped()).Revoke(context.Background(), "host-sample", "sample")
	var e *Error
	if !errors.As(err, &e) || e.Unreachable || !strings.Contains(e.Reason, "a version may still hold the key") {
		t.Fatalf("got %v", err)
	}
	if *count != attempts {
		t.Fatalf("%d requests", *count)
	}
}

func TestRefusalsAreNotSentAgainAndAreNotUnreachable(t *testing.T) {
	for _, tc := range []struct {
		code   int
		reason string
	}{
		{http.StatusBadRequest, "the key policy does not exist"},
		{http.StatusUnauthorized, "did not accept the scoped key"},
		{http.StatusForbidden, "must grant only proxy and mcp"},
		{http.StatusNotFound, "answered status 404"},
		{http.StatusFound, "answered status 302"},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			srv, count := answering(t, status(tc.code, ""))
			_, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample")
			var e *Error
			if !errors.As(err, &e) || e.Unreachable || !strings.Contains(e.Reason, tc.reason) {
				t.Fatalf("got %v", err)
			}
			if *count != 1 {
				t.Fatalf("%d requests", *count)
			}
		})
	}
}

func TestAnAnswerFromSomethingElseIsNotAKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"login page", http.StatusOK, "<html>sign in</html>"},
		{"no status", http.StatusOK, `{"disabled":false}`},
		{"unknown status", http.StatusOK, `{"status":"pending"}`},
		{"created as 200", http.StatusOK, `{"status":"created"}`},
		{"exists as 201", http.StatusCreated, `{"status":"exists"}`},
	} {
		t.Run("issue "+tc.name, func(t *testing.T) {
			srv, _ := answering(t, status(tc.code, tc.body))
			_, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample")
			var e *Error
			if !errors.As(err, &e) || e.Unreachable || e.Reason != unexpectedAnswer {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"login page", http.StatusOK, "<html>sign in</html>"},
		{"unknown status", http.StatusOK, `{"status":"pending"}`},
		{"revoked as 201", http.StatusCreated, `{"status":"revoked"}`},
	} {
		t.Run("revoke "+tc.name, func(t *testing.T) {
			srv, _ := answering(t, status(tc.code, tc.body))
			_, err := newClient(t, srv.URL, scoped()).Revoke(context.Background(), "host-sample", "sample")
			var e *Error
			if !errors.As(err, &e) || e.Unreachable || e.Reason != unexpectedAnswer {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestUnreachable(t *testing.T) {
	assert := func(t *testing.T, err error) {
		t.Helper()
		var e *Error
		if !errors.As(err, &e) || !e.Unreachable {
			t.Fatalf("got %v", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Xolotl's silence reads as the job's own deadline: %v", err)
		}
	}
	t.Run("no listener", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := l.Addr().String()
		l.Close()
		_, err = newClient(t, "http://"+address, scoped()).Issue(context.Background(), "host-sample", "sample")
		assert(t, err)
		if !strings.Contains(err.Error(), "could not be reached") {
			t.Fatalf("got %v", err)
		}
	})
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run("gateway "+strconv.Itoa(code), func(t *testing.T) {
			srv, count := answering(t, status(code, ""))
			_, err := newClient(t, srv.URL, scoped()).Revoke(context.Background(), "host-sample", "sample")
			assert(t, err)
			if *count != 1 {
				t.Fatalf("%d requests", *count)
			}
		})
	}
	t.Run("no answer in time", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		c := newClient(t, srv.URL, scoped())
		c.timeout = 50 * time.Millisecond
		_, err := c.Issue(context.Background(), "host-sample", "sample")
		assert(t, err)
		if !strings.Contains(err.Error(), "did not answer within 50ms") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an answer that breaks off", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, `{"status":`)
		}))
		defer srv.Close()
		_, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample")
		assert(t, err)
	})
}

func TestCallerEndingTheOperation(t *testing.T) {
	assert := func(t *testing.T, err error) {
		t.Helper()
		var e *Error
		if !errors.As(err, &e) || e.Unreachable || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	}
	t.Run("during a request", func(t *testing.T) {
		arrived := make(chan struct{})
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(arrived)
			<-release
		}))
		defer srv.Close()
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-arrived
			cancel()
		}()
		_, err := newClient(t, srv.URL, scoped()).Issue(ctx, "host-sample", "sample")
		assert(t, err)
	})
	t.Run("before sending again", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		srv, count := answering(t, func(w http.ResponseWriter) {
			// Later than the answer, so that the wait is what ends.
			time.AfterFunc(50*time.Millisecond, cancel)
			w.WriteHeader(http.StatusConflict)
		})
		c := newClient(t, srv.URL, scoped())
		c.delay = time.Hour
		_, err := c.Issue(ctx, "host-sample", "sample")
		assert(t, err)
		if *count != 1 {
			t.Fatalf("%d requests", *count)
		}
	})
	t.Run("during the scoped key read", func(t *testing.T) {
		srv, count := answering(t, status(http.StatusCreated, `{"status":"created"}`))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(10*time.Millisecond, cancel)
		_, err := newClient(t, srv.URL, hungSecret{}).Revoke(ctx, "host-sample", "sample")
		assert(t, err)
		if *count != 0 {
			t.Fatalf("%d requests", *count)
		}
	})
	t.Run("before the scoped key is read", func(t *testing.T) {
		srv, count := answering(t, status(http.StatusCreated, `{"status":"created"}`))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newClient(t, srv.URL, scoped()).Issue(ctx, "host-sample", "sample")
		assert(t, err)
		if *count != 0 {
			t.Fatalf("%d requests", *count)
		}
	})
}

func TestScopedKeyFailureSendsNothingAndIsNotUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets *fixedSecret
		reason  string
	}{
		{"read failure", &fixedSecret{err: errors.New(sampleKey)}, "the scoped key could not be read"},
		{"no field", &fixedSecret{fields: map[string]any{"other": sampleKey}}, "field api_key"},
		{"not a string", &fixedSecret{fields: map[string]any{"api_key": 7}}, "field api_key"},
		{"empty", &fixedSecret{fields: map[string]any{"api_key": ""}}, "field api_key"},
		{"new line", &fixedSecret{fields: map[string]any{"api_key": sampleKey + "\n"}}, "field api_key"},
		{"space", &fixedSecret{fields: map[string]any{"api_key": "a " + sampleKey}}, "field api_key"},
		{"not ASCII", &fixedSecret{fields: map[string]any{"api_key": sampleKey + "é"}}, "field api_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, count := answering(t, status(http.StatusCreated, `{"status":"created"}`))
			_, err := newClient(t, srv.URL, tc.secrets).Issue(context.Background(), "host-sample", "sample")
			var e *Error
			if !errors.As(err, &e) || e.Unreachable || !strings.Contains(e.Reason, tc.reason) {
				t.Fatalf("got %v", err)
			}
			if strings.Contains(fmt.Sprintf("%+v %#v", err, e), sampleKey) {
				t.Fatalf("the key reached the error: %+v", e)
			}
			if *count != 0 {
				t.Fatalf("%d requests", *count)
			}
		})
	}
}

// hungSecret holds a read until its context ends.
type hungSecret struct{}

func (hungSecret) Read(ctx context.Context, _ string) (map[string]any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAHungScopedKeyReadEndsAndIsNotUnreachable(t *testing.T) {
	srv, count := answering(t, status(http.StatusCreated, `{"status":"created"}`))
	c := newClient(t, srv.URL, hungSecret{})
	c.timeout = 50 * time.Millisecond
	_, err := c.Issue(context.Background(), "host-sample", "sample")
	var e *Error
	if !errors.As(err, &e) || e.Unreachable || !strings.Contains(e.Reason, "the scoped key could not be read") {
		t.Fatalf("got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the read's time limit reads as the job's own deadline: %v", err)
	}
	if *count != 0 {
		t.Fatalf("%d requests", *count)
	}
}

func TestALongAnswerIsNotRead(t *testing.T) {
	long := `{"status":"exists"}` + strings.Repeat(" ", maxAnswer)
	srv, _ := answering(t, status(http.StatusOK, long))
	_, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample")
	if err != nil {
		t.Fatalf("the status fits the bound: %v", err)
	}
	long = strings.Repeat(" ", maxAnswer) + `{"status":"exists"}`
	srv, _ = answering(t, status(http.StatusOK, long))
	_, err = newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample")
	var e *Error
	if !errors.As(err, &e) || e.Reason != unexpectedAnswer {
		t.Fatalf("got %v", err)
	}
}

// vaultSocket serves one KV version 2 answer on a Unix socket, as Wyrwood's
// vault socket does, and returns a reader on it.
func vaultSocket(t *testing.T, handler http.HandlerFunc) *openbaokv.Reader {
	t.Helper()
	dir, err := os.MkdirTemp("", "vault")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "vault.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	reader, err := openbaokv.New("unix://"+socket, "kv")
	if err != nil {
		t.Fatal(err)
	}
	reader.SetMaxRetries(0)
	return reader
}

func TestScopedKeyComesFromTheVaultSocket(t *testing.T) {
	_, srv := newFakeXolotl(t)
	var requests []string
	reader := vaultSocket(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path+" "+r.Header.Get("X-Vault-Token"))
		fmt.Fprintf(w, `{"data":{"data":{"api_key":%q}}}`, sampleKey)
	})
	got, err := newClient(t, srv.URL, reader).Issue(context.Background(), "host-sample", "sample")
	if err != nil || !got.Created {
		t.Fatalf("%+v, %v", got, err)
	}
	if want := "/v1/kv/data/" + secretPath + " "; len(requests) != 1 || requests[0] != want {
		t.Fatalf("got %q, want one %q", requests, want)
	}
}

func TestVaultSocketRefusalKeepsItsReason(t *testing.T) {
	srv, count := answering(t, status(http.StatusCreated, `{"status":"created"}`))
	reader := vaultSocket(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	_, err := newClient(t, srv.URL, reader).Issue(context.Background(), "host-sample", "sample")
	var e *Error
	if !errors.As(err, &e) || e.Unreachable || !strings.Contains(e.Reason, "OpenBao denied reading the secret") {
		t.Fatalf("got %v", err)
	}
	if *count != 0 {
		t.Fatalf("%d requests", *count)
	}
}

func TestInvalidNamesSendNothing(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, tc := range [][2]string{
		{"", "sample"}, {"host-sample", ""}, {"..", "sample"}, {"host-sample", ".."},
		{"a/b", "sample"}, {"host-sample", "a/b"}, {"Upper", "sample"}, {"host-sample", "-dash"},
		{"host-sample", "a%2Fb"}, {long, "sample"}, {"host-sample", long},
	} {
		t.Run(tc[0]+" "+tc[1], func(t *testing.T) {
			srv, count := answering(t, status(http.StatusOK, `{"status":"absent"}`))
			secrets := scoped()
			c := newClient(t, srv.URL, secrets)
			if _, err := c.Issue(context.Background(), tc[0], tc[1]); err == nil {
				t.Fatal("issue accepted the name")
			}
			if _, err := c.Revoke(context.Background(), tc[0], tc[1]); err == nil {
				t.Fatal("revoke accepted the name")
			}
			if *count != 0 || len(secrets.paths) != 0 {
				t.Fatalf("%d requests, %d reads", *count, len(secrets.paths))
			}
		})
	}
	if !ValidName(strings.Repeat("a", 63)) {
		t.Fatal("a name of 63 is valid")
	}
}

func TestValidURL(t *testing.T) {
	for _, good := range []string{"http://xolotl:8317", "https://xolotl.example.test/gateway/"} {
		if err := ValidURL(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "xolotl:8317", "ftp://xolotl", "http://", "http://user:secret-value@xolotl",
		"http://xolotl?token=secret-value", "http://xolotl?", "http://xolotl#secret-value", "http://xol otl",
	} {
		err := ValidURL(bad)
		if err == nil {
			t.Errorf("%s was accepted", bad)
		} else if strings.Contains(err.Error(), "secret-value") {
			t.Errorf("the error names the value: %v", err)
		}
	}
	if _, err := New("http://user:secret-value@xolotl", scoped(), secretPath); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("got %v", err)
	}
}

func TestRedirectDoesNotCarryTheScopedKey(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			other, otherCount := answering(t, status(http.StatusCreated, `{"status":"created"}`))
			for _, target := range []string{other.URL + "/admin/keys", "/moved"} {
				var mu sync.Mutex
				requests := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					requests++
					mu.Unlock()
					http.Redirect(w, r, target, code)
				}))
				_, err := newClient(t, srv.URL, scoped()).Issue(context.Background(), "host-sample", "sample")
				srv.Close()
				var e *Error
				if !errors.As(err, &e) || e.Unreachable || !strings.Contains(e.Reason, "answered status "+strconv.Itoa(code)) {
					t.Fatalf("%s: got %v", target, err)
				}
				if requests != 1 {
					t.Fatalf("%s: %d requests", target, requests)
				}
			}
			if *otherCount != 0 {
				t.Fatalf("the redirect target got %d requests", *otherCount)
			}
		})
	}
}

// spellings are the forms in which a hostile server can echo the scoped key.
func spellings() map[string]string {
	return map[string]string{
		"raw":          sampleKey,
		"quoted":       strconv.Quote(sampleKey),
		"query":        url.QueryEscape(sampleKey),
		"path":         url.PathEscape(sampleKey),
		"base64":       base64.StdEncoding.EncodeToString([]byte(sampleKey)),
		"base64 url":   base64.URLEncoding.EncodeToString([]byte(sampleKey)),
		"base64 bare":  base64.RawStdEncoding.EncodeToString([]byte(sampleKey)),
		"url64 bare":   base64.RawURLEncoding.EncodeToString([]byte(sampleKey)),
		"bearer value": "Bearer " + sampleKey,
	}
}

// rawAnswer writes bytes that net/http would not write, such as a reason
// phrase of the server's own.
func rawAnswer(w http.ResponseWriter, text string) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString(text)
	_ = buf.Flush()
}

// TestHostileEchoNeverReachesAnErrorOrALog sends each spelling through each
// channel the server controls. The operations reach each answer's handling:
// a status with a body, a reason phrase, a redirect and a malformed success.
func TestHostileEchoNeverReachesAnErrorOrALog(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	channels := map[string]func(echo string) func(http.ResponseWriter){
		"error body": func(echo string) func(http.ResponseWriter) {
			return status(http.StatusBadRequest, `{"error":"`+echo+`"}`)
		},
		"500 body": func(echo string) func(http.ResponseWriter) {
			return status(http.StatusInternalServerError, echo)
		},
		"unknown status body": func(echo string) func(http.ResponseWriter) {
			return status(http.StatusTeapot, echo)
		},
		"malformed success": func(echo string) func(http.ResponseWriter) {
			return status(http.StatusOK, echo)
		},
		"status field": func(echo string) func(http.ResponseWriter) {
			return status(http.StatusOK, fmt.Sprintf(`{"status":%q}`, echo))
		},
		"header": func(echo string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				w.Header().Set("Warning", echo)
				w.WriteHeader(http.StatusForbidden)
			}
		},
		"redirect": func(echo string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				w.Header().Set("Location", "http://other.example.test/"+url.PathEscape(echo))
				w.WriteHeader(http.StatusFound)
			}
		},
		"reason phrase": func(echo string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				rawAnswer(w, "HTTP/1.1 418 "+echo+"\r\nContent-Length: 0\r\n\r\n")
			}
		},
		"malformed status line": func(echo string) func(http.ResponseWriter) {
			return func(w http.ResponseWriter) {
				rawAnswer(w, echo+"\r\n\r\n")
			}
		},
	}
	for channel, answer := range channels {
		for spelling, echo := range spellings() {
			t.Run(channel+" "+spelling, func(t *testing.T) {
				srv, count := answering(t, answer(echo))
				c := newClient(t, srv.URL, scoped())
				_, issueErr := c.Issue(context.Background(), "host-sample", "sample")
				_, revokeErr := c.Revoke(context.Background(), "host-sample", "sample")
				if *count == 0 {
					t.Fatal("the fake was not reached")
				}
				for _, err := range []error{issueErr, revokeErr} {
					if err == nil {
						t.Fatal("the hostile answer was accepted")
					}
					var e *Error
					if !errors.As(err, &e) {
						t.Fatalf("got %T", err)
					}
					out := fmt.Sprintf("%v %+v %#v", err, err, *e)
					for name, form := range spellings() {
						if strings.Contains(out, form) {
							t.Fatalf("spelling %s reached the error: %s", name, out)
						}
					}
				}
			})
		}
	}
	if logged.Len() != 0 {
		t.Fatalf("the client logged: %s", logged.String())
	}
}
