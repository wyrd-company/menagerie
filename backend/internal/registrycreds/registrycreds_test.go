package registrycreds

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/registry"
	"github.com/wyrd-company/devcontainer-builder/pkg/registryauth"
)

// Synthetic sentinels: no real credential is used anywhere in these tests.
const (
	sentinelUser     = "sentinel-user-8c1f"
	sentinelPassword = "sentinel-password-4d2a"
	sentinelBody     = "sentinel-body-77e0"
	sentinelToken    = "sentinel-ambient-token-19b3"
)

func TestNormalizeRegistry(t *testing.T) {
	cases := map[string]string{
		"ghcr.io":                     "ghcr.io",
		"GHCR.io":                     "ghcr.io",
		"127.0.0.1:5000":              "127.0.0.1:5000",
		"registry.example.test:443":   "registry.example.test:443",
		"[::1]:5000":                  "[::1]:5000",
		"docker.io":                   DockerHub,
		"index.docker.io":             DockerHub,
		"Registry-1.Docker.io":        DockerHub,
		"https://index.docker.io/v1/": DockerHub,
	}
	for in, want := range cases {
		if got, err := NormalizeRegistry(in); err != nil || got != want {
			t.Errorf("NormalizeRegistry(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "https://ghcr.io", "ghcr.io/team", "-bad.example", "ghcr.io:port", "a b"} {
		if _, err := NormalizeRegistry(bad); err == nil {
			t.Errorf("NormalizeRegistry(%q) accepted", bad)
		}
	}
}

func TestNewRefusesDuplicateAndInvalidRegistries(t *testing.T) {
	f := &countingFetcher{}
	if _, err := New(f, []Entry{{Registry: "docker.io", Path: "a"}, {Registry: "registry-1.docker.io", Path: "b"}}); err == nil || !strings.Contains(err.Error(), "configured twice") {
		t.Fatalf("duplicate alias: %v", err)
	}
	if _, err := New(f, []Entry{{Registry: "https://ghcr.io", Path: "a"}}); err == nil {
		t.Fatal("scheme accepted")
	}
	if _, err := New(nil, []Entry{{Registry: "ghcr.io", Path: "a"}}); err == nil {
		t.Fatal("entries without a fetcher accepted")
	}
}

// countingFetcher returns fixed credentials or an error and counts calls.
type countingFetcher struct {
	calls atomic.Int32
	creds registryauth.Credentials
	err   error
	paths sync.Map
	block chan struct{}
}

func (f *countingFetcher) Fetch(ctx context.Context, path string) (registryauth.Credentials, error) {
	f.calls.Add(1)
	f.paths.Store(path, true)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return registryauth.Credentials{}, &Error{Reason: "the operation ended before OpenBao answered", cause: ctx.Err()}
		}
	}
	return f.creds, f.err
}

func TestProviderAnonymousForUnconfiguredRegistries(t *testing.T) {
	f := &countingFetcher{creds: registryauth.Credentials{Username: sentinelUser, Password: sentinelPassword}}
	r, err := New(f, []Entry{{Registry: "ghcr.io", Path: "registries/ghcr"}})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Operation(context.Background())
	for _, host := range []string{"registry.example.test", "docker.io", "not a host"} {
		if c, err := p.Credentials(host); err != nil || !c.Anonymous() {
			t.Errorf("%s: %+v %v", host, c, err)
		}
	}
	if v, err := p.EngineAuth("busybox:1.37"); err != nil || v != "" {
		t.Errorf("hub image: %q %v", v, err)
	}
	if f.calls.Load() != 0 {
		t.Fatalf("fetcher called %d times for unconfigured registries", f.calls.Load())
	}
	var nilRegistries *Registries
	if c, err := nilRegistries.Operation(context.Background()).Credentials("ghcr.io"); err != nil || !c.Anonymous() {
		t.Fatalf("nil registries: %+v %v", c, err)
	}
}

func TestProviderFetchesOncePerOperationConcurrently(t *testing.T) {
	f := &countingFetcher{creds: registryauth.Credentials{Username: sentinelUser, Password: sentinelPassword}, block: make(chan struct{})}
	r, _ := New(f, []Entry{{Registry: "Registry-1.Docker.io", Path: "registries/hub"}})
	p := r.Operation(context.Background())
	var wg sync.WaitGroup
	results := make([]registryauth.Credentials, 50)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every Docker Hub alias reaches the one configured entry.
			host := []string{"docker.io", "index.docker.io", "registry-1.docker.io"}[i%3]
			results[i], _ = p.Credentials(host)
		}()
	}
	close(f.block)
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("fetcher called %d times", f.calls.Load())
	}
	for i, c := range results {
		if c.Username != sentinelUser || c.Password != sentinelPassword {
			t.Fatalf("result %d: %+v", i, c)
		}
	}
	if _, ok := f.paths.Load("registries/hub"); !ok {
		t.Fatal("configured path not read")
	}
	// A new operation reads again.
	r.Operation(context.Background()).Credentials("docker.io")
	if f.calls.Load() != 2 {
		t.Fatalf("second operation reused the first's result")
	}
}

func TestProviderEngineAuthEncodesCredentials(t *testing.T) {
	f := &countingFetcher{creds: registryauth.Credentials{Username: sentinelUser, Password: sentinelPassword}}
	r, _ := New(f, []Entry{{Registry: "docker.io", Path: "hub"}, {Registry: "127.0.0.1:5000", Path: "local"}})
	p := r.Operation(context.Background())
	for ref, server := range map[string]string{"busybox:1.37": "https://index.docker.io/v1/", "127.0.0.1:5000/team/app:1": "127.0.0.1:5000"} {
		v, err := p.EngineAuth(ref)
		if err != nil {
			t.Fatal(err)
		}
		ac, err := registry.DecodeAuthConfig(v)
		if err != nil || ac.Username != sentinelUser || ac.Password != sentinelPassword || ac.ServerAddress != server {
			t.Fatalf("%s: %+v %v", ref, ac, err)
		}
	}
}

func TestProviderFailureIsTypedAndCached(t *testing.T) {
	f := &countingFetcher{err: &Error{Reason: "the secret does not exist"}}
	r, _ := New(f, []Entry{{Registry: "ghcr.io", Path: "missing"}})
	p := r.Operation(context.Background())
	for range 2 {
		_, err := p.Credentials("ghcr.io")
		var e *Error
		if !errors.As(err, &e) || e.Registry != "ghcr.io" || err.Error() != "registry credentials for ghcr.io are unavailable: the secret does not exist" {
			t.Fatalf("got %v", err)
		}
		if _, err := p.EngineAuth("ghcr.io/team/app:1"); !errors.As(err, &e) {
			t.Fatalf("engine auth: %v", err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("failure fetched %d times", f.calls.Load())
	}
	// A foreign error keeps only a fixed reason.
	r2, _ := New(&countingFetcher{err: errors.New(sentinelPassword)}, []Entry{{Registry: "ghcr.io", Path: "x"}})
	_, err := r2.Operation(context.Background()).Credentials("ghcr.io")
	if err == nil || strings.Contains(err.Error(), sentinelPassword) {
		t.Fatalf("foreign error text leaked: %v", err)
	}
}

func TestProviderPropagatesCancellation(t *testing.T) {
	f := &countingFetcher{block: make(chan struct{})}
	r, _ := New(f, []Entry{{Registry: "ghcr.io", Path: "x"}})
	ctx, cancel := context.WithCancel(context.Background())
	p := r.Operation(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := p.Credentials("ghcr.io")
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not reach the fetch")
	}
}

// fakeVault serves a synthetic OpenBao API on a Unix socket and records
// the requests it receives.
type fakeVault struct {
	address string
	mu      sync.Mutex
	headers []http.Header
	paths   []string
	answer  func(w http.ResponseWriter, r *http.Request)
}

func newFakeVault(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *fakeVault {
	t.Helper()
	dir, err := os.MkdirTemp("", "bao") // short path: socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "vault.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVault{address: SocketAddress(dir), answer: answer}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.headers = append(v.headers, r.Header.Clone())
		v.paths = append(v.paths, r.URL.Path)
		v.mu.Unlock()
		v.answer(w, r)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	if v.address != "unix://"+sock {
		t.Fatalf("address %s", v.address)
	}
	return v
}

func kvAnswer(data any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 1}}})
	}
}

func statusAnswer(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func TestOpenBaoReadsKVv2WithoutAmbientTokenOrNamespace(t *testing.T) {
	for _, name := range []string{"BAO_TOKEN", "VAULT_TOKEN", "BAO_NAMESPACE", "VAULT_NAMESPACE"} {
		t.Setenv(name, sentinelToken)
	}
	t.Setenv("BAO_ADDR", "http://127.0.0.1:1")
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:1")
	v := newFakeVault(t, kvAnswer(map[string]any{"username": sentinelUser, "password": sentinelPassword}))
	src, err := NewOpenBao(v.address, "kv")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := src.Fetch(context.Background(), "menagerie/registries/ghcr")
	if err != nil || creds.Username != sentinelUser || creds.Password != sentinelPassword {
		t.Fatalf("%+v %v", creds, err)
	}
	if len(v.paths) != 1 || v.paths[0] != "/v1/kv/data/menagerie/registries/ghcr" {
		t.Fatalf("paths %v", v.paths)
	}
	for name, values := range v.headers[0] {
		for _, value := range values {
			if strings.Contains(value, sentinelToken) {
				t.Fatalf("ambient value sent in %s", name)
			}
		}
	}
	for _, h := range []string{"X-Vault-Token", "Authorization", "X-Vault-Namespace"} {
		if v.headers[0].Get(h) != "" {
			t.Fatalf("%s sent", h)
		}
	}
}

func TestOpenBaoFailuresAreTypedWithoutResponseText(t *testing.T) {
	leaky := `{"errors":["` + sentinelBody + `"]}`
	cases := []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request)
		reason string
	}{
		{"missing", statusAnswer(404, `{"errors":[]}`), "the secret does not exist"},
		{"denied", statusAnswer(403, leaky), "OpenBao denied reading the secret"},
		{"not bound", statusAnswer(503, `{"errors":["wyrwood: `+sentinelBody+`"],"wyrwood":{"state":"not-bound"}}`), "Wyrwood holds no OpenBao token for Menagerie (state not-bound)"},
		{"no token", statusAnswer(503, `{"errors":["wyrwood: x"],"wyrwood":{"state":"no-token"}}`), "Wyrwood holds no OpenBao token for Menagerie (state no-token)"},
		{"sealed", statusAnswer(503, leaky), "OpenBao answered status 503"},
		{"server error", statusAnswer(500, leaky), "OpenBao answered status 500"},
		{"malformed json", statusAnswer(200, sentinelBody), "OpenBao returned a malformed response"},
		{"deleted version", kvAnswer(nil), "the secret has no current version"},
		{"missing password", kvAnswer(map[string]any{"username": sentinelUser}), "the secret needs non-empty string fields username and password"},
		{"number password", kvAnswer(map[string]any{"username": sentinelUser, "password": 7}), "the secret needs non-empty string fields username and password"},
		{"empty username", kvAnswer(map[string]any{"username": "", "password": sentinelPassword}), "the secret needs non-empty string fields username and password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newFakeVault(t, tc.answer)
			src, err := NewOpenBao(v.address, "kv")
			if err != nil {
				t.Fatal(err)
			}
			src.client.SetMaxRetries(0)
			_, err = src.Fetch(context.Background(), "x")
			var e *Error
			if !errors.As(err, &e) || e.Reason != tc.reason {
				t.Fatalf("got %v", err)
			}
			assertNoSentinel(t, err.Error())
		})
	}
}

func TestOpenBaoUnreachableSocket(t *testing.T) {
	src, err := NewOpenBao(SocketAddress(t.TempDir()), "kv")
	if err != nil {
		t.Fatal(err)
	}
	src.client.SetMaxRetries(0)
	_, err = src.Fetch(context.Background(), "x")
	var e *Error
	if !errors.As(err, &e) || e.Reason != "the Wyrwood vault socket could not be reached" {
		t.Fatalf("got %v", err)
	}
}

func TestOpenBaoCancellation(t *testing.T) {
	release := make(chan struct{})
	v := newFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	src, _ := NewOpenBao(v.address, "kv")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := src.Fetch(ctx, "x")
		done <- err
	}()
	for {
		v.mu.Lock()
		n := len(v.paths)
		v.mu.Unlock()
		if n > 0 {
			break
		}
		runtime.Gosched()
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func assertNoSentinel(t *testing.T, s string) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(sentinelPassword))
	for _, secret := range []string{sentinelPassword, sentinelBody, sentinelUser, encoded} {
		if strings.Contains(s, secret) {
			t.Fatalf("output holds a sentinel: %q", s)
		}
	}
}
