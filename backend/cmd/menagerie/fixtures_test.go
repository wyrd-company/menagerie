//go:build pocketid

package main

// Fixtures the Pocket ID-backed suites share: required settings, test Git
// repositories, the generic two-version sample template and the HTTPS host
// that serves template repositories.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is required", name)
	}
	return v
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Example User", "GIT_AUTHOR_EMAIL=user@example.invalid",
		"GIT_COMMITTER_NAME=Example User", "GIT_COMMITTER_EMAIL=user@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeTo(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// browserTemplate writes version 1 or 2 of the generic sample template.
// Version 2 adds one required question and changes one generated line.
// The hook and the devcontainer's initializeCommand leave markers.
func browserTemplate(t *testing.T, dir string, version int) {
	region, regionLine := "", ""
	if version == 2 {
		region = "  - {id: region, type: select, prompt: Region, description: Added by version 2 of the sample template., options: [north, south], required: true}\n"
		regionLine = "region: {{ region }}\n"
	}
	writeTo(t, dir, "template.yml", "name: sample-kit\ninterview:\n"+
		"  - {id: label, type: text, prompt: Label, description: Shown in result.txt., placeholder: Sample label, required: true, validate: {min: 3, max: 40}}\n"+
		"  - {id: size, type: select, prompt: Size, options: [small, large], default: small}\n"+
		"  - {id: verbose, type: confirm, prompt: Verbose logging?, default: false}\n"+
		"  - {id: tags, type: multiselect, prompt: Tags, options: [alpha, beta, gamma], default: [alpha]}\n"+
		region+
		"  - message: \"Sample kit for {{ label }}.\"\n"+
		"hooks:\n  - run: [sh, -c, 'echo ran > hook-marker.txt']\n")
	writeTo(t, dir, "template/result.txt", "label: {{ label }}\nsize: {{ size }}\n"+regionLine)
	writeTo(t, dir, "template/local.txt", fmt.Sprintf("first: template\nsecond: template\nthird: stable\nfourth: stable\nfifth: v%d\n", version))
	writeTo(t, dir, "template/.gitignore", "/markers/\n")
	writeTo(t, dir, "template/.devcontainer/devcontainer.json", `{
  "name": "sample",
  "image": "busybox:1.37",
  "initializeCommand": "mkdir -p markers && echo initialized > markers/initialize.txt"
}
`)
}

// templateHost serves the template repositories under Dir anonymously,
// as a Git host over HTTPS on 127.0.0.1, with a certificate signed by an
// authority generated for the test. The template service, which refuses
// file:// and folder addresses, trusts it through GIT_SSL_CAINFO when it
// runs here and through SSL_CERT_FILE in the image. Git runs here only as
// the host's http-backend program.
type templateHost struct {
	Dir string
	// URL is the host's origin.
	URL string
	// Authority is the PEM file of the authority that signed its
	// certificate, readable by the image's services.
	Authority string
}

func newTemplateHost(t *testing.T, dir string) *templateHost {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Sample template host authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("the template host needs git on PATH")
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Stderr: io.Discard, Env: []string{
		"GIT_PROJECT_ROOT=" + dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + t.TempDir(),
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: backend, TLSConfig: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close() })
	authority := filepath.Join(dir, ".authority.pem")
	if err := os.WriteFile(authority, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	return &templateHost{Dir: dir, URL: "https://" + listener.Addr().String(), Authority: authority}
}

// Address is the address of the repository at name under Dir.
func (h *templateHost) Address(name string) string { return h.URL + "/" + name }

// kitRepository writes the sample template under the host as name, at
// version 1 tagged 1.0.0 and, when both, at version 2 tagged 2.0.0.
func (h *templateHost) kitRepository(t *testing.T, name string, both bool) string {
	t.Helper()
	repo := filepath.Join(h.Dir, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	browserTemplate(t, repo, 1)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v1")
	gitRun(t, repo, "tag", "1.0.0")
	if both {
		h.newVersion(t, name, "2.0.0")
	}
	return repo
}

// newVersion commits version 2 of the sample template to the repository
// at name and tags it, as a template author releases a version.
func (h *templateHost) newVersion(t *testing.T, name, tag string) {
	t.Helper()
	repo := filepath.Join(h.Dir, name)
	browserTemplate(t, repo, 2)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v2")
	gitRun(t, repo, "tag", tag)
}
