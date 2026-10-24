//go:build docker

package devcontainer_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic registry credentials; nothing real is used.
const (
	registryUser     = "sentinel-registry-user-5e2d"
	registryPassword = "sentinel-registry-password-a7f4"
)

// registryTLS is the test registry's certificate, signed by a CA that this
// process trusts through SSL_CERT_DIR. The Docker daemon treats a
// 127.0.0.0/8 registry as insecure and accepts it without the CA.
var registryTLS tls.Certificate

func init() {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "menagerie test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	registryTLS = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	dir, err := os.MkdirTemp("", "menagerie-test-ca-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		panic(err)
	}
	// Keep the system roots: Go reads SSL_CERT_DIR in place of its default
	// folders, after the system bundle file.
	os.Setenv("SSL_CERT_DIR", "/etc/ssl/certs:/etc/pki/tls/certs:"+dir)
}

type manifest struct {
	mediaType string
	body      []byte
}

// testRegistry is an in-memory OCI distribution registry that requires
// basic authentication on every request.
type testRegistry struct {
	addr string

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]manifest // repo + ":" + tag or digest

	authorized   atomic.Int64
	unauthorized atomic.Int64
	pulls        sync.Map // repo -> count of authorized manifest reads
}

func startRegistry(t *testing.T) *testRegistry {
	t.Helper()
	r := &testRegistry{blobs: map[string][]byte{}, manifests: map[string]manifest{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(r.serve))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{registryTLS}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r.addr = strings.TrimPrefix(srv.URL, "https://")
	return r
}

func (r *testRegistry) serve(w http.ResponseWriter, req *http.Request) {
	user, pass, ok := req.BasicAuth()
	if !ok || user != registryUser || pass != registryPassword {
		r.unauthorized.Add(1)
		w.Header().Set("WWW-Authenticate", `Basic realm="menagerie-test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.authorized.Add(1)
	path := req.URL.Path
	if path == "/v2/" || path == "/v2" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := strings.LastIndex(path, "/manifests/"); i > 0 {
		repo, ref := strings.TrimPrefix(path[:i], "/v2/"), path[i+len("/manifests/"):]
		m, ok := r.manifests[repo+":"+ref]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n, _ := r.pulls.LoadOrStore(repo, new(atomic.Int64))
		n.(*atomic.Int64).Add(1)
		w.Header().Set("Content-Type", m.mediaType)
		w.Header().Set("Docker-Content-Digest", digest(m.body))
		w.Header().Set("Content-Length", fmt.Sprint(len(m.body)))
		if req.Method == http.MethodGet {
			w.Write(m.body)
		}
		return
	}
	if i := strings.LastIndex(path, "/blobs/"); i > 0 {
		b, ok := r.blobs[path[i+len("/blobs/"):]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", digest(b))
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		if req.Method == http.MethodGet {
			w.Write(b)
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (r *testRegistry) pullCount(repo string) int64 {
	if n, ok := r.pulls.Load(repo); ok {
		return n.(*atomic.Int64).Load()
	}
	return 0
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// put stores a manifest under repo:tag and its digest.
func (r *testRegistry) put(repo, tag, mediaType string, body []byte, blobs ...[]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range blobs {
		r.blobs[digest(b)] = b
	}
	m := manifest{mediaType: mediaType, body: body}
	r.manifests[repo+":"+tag] = m
	r.manifests[repo+":"+digest(body)] = m
}

// pushImage copies a local image into the registry as a single-platform
// OCI image and returns its reference.
func (r *testRegistry) pushImage(t *testing.T, h *harness, src, repo, tag string) string {
	t.Helper()
	rc, err := h.client.ImageSave(h.ctx, []string{src})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	files := map[string][]byte{}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			files[hdr.Name] = b
		}
	}
	var saved []struct {
		Config string
		Layers []string
	}
	if err := json.Unmarshal(files["manifest.json"], &saved); err != nil || len(saved) != 1 {
		t.Fatalf("docker save manifest: %v", err)
	}
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	config := files[saved[0].Config]
	blobs := [][]byte{config}
	var layers []descriptor
	for _, name := range saved[0].Layers {
		b := files[name]
		mediaType := "application/vnd.oci.image.layer.v1.tar"
		if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
			mediaType += "+gzip"
		}
		layers = append(layers, descriptor{mediaType, digest(b), len(b)})
		blobs = append(blobs, b)
	}
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        descriptor{"application/vnd.oci.image.config.v1+json", digest(config), len(config)},
		"layers":        layers,
	})
	r.put(repo, tag, "application/vnd.oci.image.manifest.v1+json", body, blobs...)
	return r.addr + "/" + repo + ":" + tag
}

// pushFeature stores a devcontainer feature artifact and returns its
// reference.
func (r *testRegistry) pushFeature(t *testing.T, repo, tag string, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	layer, config := buf.Bytes(), []byte("{}")
	body, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.devcontainers", "digest": digest(config), "size": len(config)},
		"layers":        []map[string]any{{"mediaType": "application/vnd.devcontainers.layer.v1+tar", "digest": digest(layer), "size": len(layer)}},
	})
	r.put(repo, tag, "application/vnd.oci.image.manifest.v1+json", body, config, layer)
	return r.addr + "/" + repo + ":" + tag
}

// ensureImage pulls a public image anonymously when the daemon lacks it.
func ensureImage(t *testing.T, h *harness, ref string) {
	t.Helper()
	if _, err := h.client.ImageInspect(h.ctx, ref); err == nil {
		return
	}
	rc, err := h.client.ImagePull(context.Background(), ref, pullOptions())
	if err != nil {
		t.Fatalf("pull %s: %v", ref, err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()
	if _, err := h.client.ImageInspect(h.ctx, ref); err != nil {
		t.Fatalf("%s is required: %v", ref, err)
	}
}
