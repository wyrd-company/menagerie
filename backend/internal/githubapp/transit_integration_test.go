//go:build openbao

// OpenBao integration test. It starts a disposable `bao server -dev` (the
// bao binary must be on PATH), creates an RSA Transit key and serves the
// server on a Unix socket through a proxy that applies the Wyrwood vault
// socket contract: client credential headers are removed and a token
// allowed only to sign with that key is sent.
//
//	go test -tags openbao ./backend/internal/githubapp/
package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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
	"testing"
	"time"

	openbao "github.com/openbao/openbao/api/v2"
)

func TestTransitSignsAnAppJWTThroughTheWyrwoodContract(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	rootToken := hex.EncodeToString(b)
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
	if err := root.Sys().Mount("transit", &openbao.MountInput{Type: "transit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Logical().Write("transit/keys/sample-app-key", map[string]any{"type": "rsa-2048"}); err != nil {
		t.Fatal(err)
	}
	policy := `path "transit/sign/sample-app-key/sha2-256" { capabilities = ["update"] }`
	if err := root.Sys().PutPolicy("menagerie-github-app", policy); err != nil {
		t.Fatal(err)
	}
	signOnly, err := root.Auth().Token().Create(&openbao.TokenCreateRequest{Policies: []string{"menagerie-github-app"}, NoDefaultPolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	key, err := root.Logical().Read("transit/keys/sample-app-key")
	if err != nil {
		t.Fatal(err)
	}
	public := publicKey(t, key)

	// The Wyrwood contract: client credentials removed, the binding's
	// token sent.
	dir, err := os.MkdirTemp("", "wyrwood")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock, err := net.Listen("unix", filepath.Join(dir, "vault.sock"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("http://" + addr)
	proxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range []string{"X-Vault-Token", "Authorization", "X-Vault-Namespace"} {
			if r.Header.Get(h) != "" {
				t.Errorf("the client sent %s", h)
			}
			r.Header.Del(h)
		}
		r.Header.Set("X-Vault-Token", signOnly.Auth.ClientToken)
		proxy.ServeHTTP(w, r)
	}))
	srv.Listener = sock
	srv.Start()
	t.Cleanup(srv.Close)

	t.Setenv("BAO_TOKEN", "sentinel-ambient-token-3d70")
	t.Setenv("BAO_ADDR", "http://127.0.0.1:1")
	address := "unix://" + filepath.Join(dir, "vault.sock")
	signer, err := NewTransit(address, "sample-app-key")
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{AppID: sampleAppID, Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := m.jwt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(parts) != 3 {
		t.Fatalf("JWT %d parts, signature %v", len(parts), err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("the App JWT does not verify with the Transit key: %v", err)
	}

	// A key the token may not use is denied, with a fixed reason.
	other, err := NewTransit(address, "another-key")
	if err != nil {
		t.Fatal(err)
	}
	var e *Error
	if _, err := other.Sign(context.Background(), []byte("x")); !errors.As(err, &e) || e.Reason != "OpenBao denied signing with the Transit key" {
		t.Fatalf("another key: %v", err)
	}
}

// publicKey is the public key of version 1 of a Transit RSA key.
func publicKey(t *testing.T, key *openbao.Secret) *rsa.PublicKey {
	t.Helper()
	var doc struct {
		Keys map[string]struct {
			PublicKey string `json:"public_key"`
		} `json:"keys"`
	}
	data, _ := json.Marshal(key.Data)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(doc.Keys["1"].PublicKey))
	if block == nil {
		t.Fatal("the Transit key has no public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	public, ok := parsed.(*rsa.PublicKey)
	if !ok {
		t.Fatal("the Transit key is not RSA")
	}
	return public
}
