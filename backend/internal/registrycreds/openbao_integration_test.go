//go:build openbao

// OpenBao integration tests. They start a disposable `bao server -dev`
// (the bao binary must be on PATH) and serve it on a Unix socket through a
// test proxy that applies the Wyrwood vault socket contract: client tokens,
// Authorization and namespace headers are removed and the binding's token
// is sent; an unbound container is answered with Wyrwood's 503 shape.
//
//	go test -tags openbao ./backend/internal/registrycreds/
package registrycreds_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	openbao "github.com/openbao/openbao/api/v2"

	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
)

// Synthetic values only.
const (
	sampleUser     = "sentinel-user-0a91"
	samplePassword = "sentinel-password-6b3c"
)

type devServer struct {
	addr string
	root *openbao.Client
}

func randomHex(t *testing.T) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func startDevServer(t *testing.T) *devServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	rootToken := randomHex(t)
	cmd := exec.Command("bao", "server", "-dev", "-dev-root-token-id="+rootToken, "-dev-listen-address="+addr)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bao dev server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	cfg := openbao.NewConfig()
	cfg.Address = "http://" + addr
	root, err := openbao.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	root.SetToken(rootToken)
	for i := 0; ; i++ {
		if health, err := root.Sys().Health(); err == nil && health.Initialized && !health.Sealed {
			break
		}
		if i == 200 {
			t.Fatal("bao dev server did not become ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return &devServer{addr: addr, root: root}
}

func (d *devServer) token(t *testing.T, policies ...string) string {
	t.Helper()
	secret, err := d.root.Auth().Token().Create(&openbao.TokenCreateRequest{Policies: policies, NoDefaultPolicy: len(policies) > 0})
	if err != nil {
		t.Fatal(err)
	}
	return secret.Auth.ClientToken
}

// wyrwoodSocket serves the dev server on vault.sock with the binding token
// returned by token(); an empty token answers as an unbound container.
type wyrwoodSocket struct {
	dir string
	mu  sync.Mutex
	// clientHeaders records the credential headers clients sent.
	clientHeaders []string
	token         string
}

func newWyrwoodSocket(t *testing.T, upstream string) *wyrwoodSocket {
	t.Helper()
	dir, err := os.MkdirTemp("", "wyrwood")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "vault.sock"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("http://" + upstream)
	w := &wyrwoodSocket{dir: dir}
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		for _, h := range []string{"X-Vault-Token", "Authorization", "X-Vault-Namespace"} {
			if v := r.Header.Get(h); v != "" {
				w.clientHeaders = append(w.clientHeaders, h)
			}
			r.Header.Del(h)
		}
		token := w.token
		w.mu.Unlock()
		if token == "" {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(rw).Encode(map[string]any{
				"errors":  []string{"wyrwood: this container is not bound to a Wyrwood policy"},
				"wyrwood": map[string]string{"state": "not-bound"},
			})
			return
		}
		r.Header.Set("X-Vault-Token", token)
		proxy.ServeHTTP(rw, r)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return w
}

func (w *wyrwoodSocket) bind(token string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.token = token
}

func TestOpenBaoThroughWyrwoodContract(t *testing.T) {
	d := startDevServer(t)
	kv := d.root.KVv2("secret")
	ctx := context.Background()
	if _, err := kv.Put(ctx, "menagerie/registries/sample", map[string]any{"username": sampleUser, "password": samplePassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx, "menagerie/registries/partial", map[string]any{"username": sampleUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx, "menagerie/registries/deleted", map[string]any{"username": sampleUser, "password": samplePassword}); err != nil {
		t.Fatal(err)
	}
	if err := kv.Delete(ctx, "menagerie/registries/deleted"); err != nil {
		t.Fatal(err)
	}
	policy := `path "secret/data/menagerie/registries/*" { capabilities = ["read"] }`
	if err := d.root.Sys().PutPolicy("menagerie-registry-read", policy); err != nil {
		t.Fatal(err)
	}
	reader := d.token(t, "menagerie-registry-read")
	unrelated := d.token(t, "default")

	sock := newWyrwoodSocket(t, d.addr)
	// Ambient OpenBao settings must not reach the request.
	t.Setenv("BAO_TOKEN", "sentinel-ambient-token-71aa")
	t.Setenv("BAO_NAMESPACE", "sentinel-ambient-namespace")
	t.Setenv("BAO_ADDR", "http://127.0.0.1:1")
	src, err := registrycreds.NewOpenBao(registrycreds.SocketAddress(sock.dir), "secret")
	if err != nil {
		t.Fatal(err)
	}
	registries, err := registrycreds.New(src, []registrycreds.Entry{
		{Registry: "registry.example.test", Path: "menagerie/registries/sample"},
		{Registry: "partial.example.test", Path: "menagerie/registries/partial"},
		{Registry: "deleted.example.test", Path: "menagerie/registries/deleted"},
		{Registry: "missing.example.test", Path: "menagerie/registries/missing"},
	})
	if err != nil {
		t.Fatal(err)
	}

	sock.bind(reader)
	p := registries.Operation(ctx)
	creds, err := p.Credentials("registry.example.test")
	if err != nil || creds.Username != sampleUser || creds.Password != samplePassword {
		t.Fatalf("read through the socket: %+v %v", creds, err)
	}
	if c, err := p.Credentials("other.example.test"); err != nil || !c.Anonymous() {
		t.Fatalf("unconfigured registry: %+v %v", c, err)
	}
	reasons := map[string]string{
		"partial.example.test": "the secret needs non-empty string fields username and password",
		"deleted.example.test": "the secret does not exist",
		"missing.example.test": "the secret does not exist",
	}
	for host, reason := range reasons {
		_, err := p.Credentials(host)
		assertReason(t, err, host, reason)
	}

	sock.bind(unrelated)
	_, err = registries.Operation(ctx).Credentials("registry.example.test")
	assertReason(t, err, "registry.example.test", "OpenBao denied reading the secret")

	sock.bind("")
	_, err = registries.Operation(ctx).Credentials("registry.example.test")
	assertReason(t, err, "registry.example.test", "Wyrwood holds no OpenBao token for Menagerie (state not-bound)")

	if len(sock.clientHeaders) != 0 {
		t.Fatalf("client sent credential headers: %v", sock.clientHeaders)
	}
}

func assertReason(t *testing.T, err error, host, reason string) {
	t.Helper()
	var e *registrycreds.Error
	if !errors.As(err, &e) || e.Registry != host || e.Reason != reason {
		t.Fatalf("%s: got %v, want reason %q", host, err, reason)
	}
	if strings.Contains(err.Error(), samplePassword) || strings.Contains(err.Error(), sampleUser) {
		t.Fatalf("%s: error holds a credential: %v", host, err)
	}
}
