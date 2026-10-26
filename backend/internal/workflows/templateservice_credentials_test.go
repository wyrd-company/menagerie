//go:build templateservice

package workflows

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// TestAJobLimitEndsTheTemplateServicesCredentialRequest creates a
// workspace from an https template whose host challenges every request,
// while the credential route holds the template service's request. The
// interview's limit ends the job; the template service then ends its
// credential request, so the route's request context ends.
func TestAJobLimitEndsTheTemplateServicesCredentialRequest(t *testing.T) {
	host, authority := challengingHost(t)
	held := make(chan struct{}, 8)
	ended := make(chan struct{}, 8)
	// Released when the test ends, so a request the template service never
	// ends cannot keep it from stopping.
	release := make(chan struct{})
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http sees the connection close only once the body is read,
		// as the workspace service's route reads it.
		_, _ = io.Copy(io.Discard, r.Body)
		held <- struct{}{}
		select {
		case <-r.Context().Done():
			ended <- struct{}{}
		case <-release:
			http.Error(w, `{"reason":"the token could not be minted"}`, http.StatusBadGateway)
		}
	})
	a := newAcceptWith(t, &fakeRuntime{}, acceptOptions{
		containers: localContainers{}, image: "runner:local",
		limits:      jobs.Limits{Phases: map[string]time.Duration{"interview": 2 * time.Second}},
		templates:   map[string]string{"private": host + "/example-org/sample-kit.git@v1"},
		credentials: route,
		// Both of curl's TLS backends read it.
		env: []string{"GIT_SSL_CAINFO=" + authority},
	})
	t.Cleanup(func() { close(release) })
	job, _ := a.drive(t, "create", "alpha", "private", nil)
	if snap := job.Snapshot(); snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != jobs.KindTimedOut {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	select {
	case <-held:
	default:
		t.Fatal("the template service never asked the credential route")
	}
	// Test-only bound: the request ends as soon as the template service
	// sees its caller go away.
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the credential request outlived its job")
	}
}

// challengingHost serves, on loopback over TLS, a Git host that challenges
// every request, and returns its origin and the PEM file of the authority
// that signed its certificate. The certificate is a server certificate,
// not a CA: curl built with rustls, as the image and CI build it, refuses
// a self-signed CA certificate as a server's, which httptest's own is.
func challengingHost(t *testing.T) (origin, authority string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Sample test authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="sample"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}))
	host.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf}, PrivateKey: key}}}
	host.StartTLS()
	t.Cleanup(host.Close)
	authority = filepath.Join(t.TempDir(), "authority.pem")
	if err := os.WriteFile(authority, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return host.URL, authority
}
