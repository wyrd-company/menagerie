//go:build pocketid && image && acceptance

package main

// TestImageWyrwoodGitHubApp runs the built image, MENAGERIE_IMAGE, bound by
// a real Wyrwood agent (see wyrwood_test.go) with a GitHub App as its only
// Git identity: no SSH key, no SSH Transit key and no SSH agent. The App's
// private key is imported into OpenBao's Transit engine as the
// installation documentation tells operators to. Menagerie signs the App's
// JWT through Wyrwood's vault socket, fetches a private template through
// the template service's credential route and backs the workspace up with
// installation tokens. GitHub is a fake served from this process over TLS
// with a certificate authority generated here, which the container trusts
// through SSL_CERT_FILE. Every key and token is generated here.

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

const (
	// The App, its installation on the sample account and the Transit key
	// that holds its private key.
	sampleAppID          = 424242
	sampleInstallation   = 777001
	sampleAppTransitKey  = "sample-github-app"
	sampleAccount        = "example-org"
	sampleTemplateRepo   = "sample-kit"
	sampleBackupRepo     = "sample-backups"
	menagerieAppName     = "sample-menagerie-app"
	sampleTokenLifetime  = time.Hour
	githubCertificateTTL = 24 * time.Hour
)

// appGrant is what one installation token was minted for.
type appGrant struct {
	Repository string // owner/name
	Permission string // read or write
}

// gitRequest is one Git smart HTTP request the fake received.
type gitRequest struct {
	Method, Path, Repository string
	// Grant is the installation token's grant; empty for no credentials.
	Grant appGrant
	// Foreign is an Authorization the fake did not issue.
	Foreign string
	Status  int
}

// fakeGitHub is GitHub's App API and Git smart HTTP on loopback over TLS.
// It admits a JWT only with the App's RS256 signature and a Git request
// only with x-access-token basic authentication and a token it minted for
// that repository with enough permission.
type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server
	// URL is the server's origin, the github-app server.
	URL string
	// Dir holds the repositories as <owner>/<name>.git.
	Dir string
	// CA is the PEM of the authority that signed the server's certificate.
	CA  []byte
	app *rsa.PublicKey

	mu       sync.Mutex
	tokens   map[string]appGrant
	jwts     []string
	git      []gitRequest
	problems []string
}

// startFakeGitHub serves the fake on 127.0.0.1 with a certificate for that
// address, signed by an authority generated for the test.
func startFakeGitHub(t *testing.T, app *rsa.PublicKey) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{t: t, Dir: t.TempDir(), app: app, tokens: map[string]appGrant{}}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Sample test authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(githubCertificateTTL),
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
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(githubCertificateTTL),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	g.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("the fake GitHub's server needs git on PATH")
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Stderr: io.Discard, Env: []string{
		"GIT_PROJECT_ROOT=" + g.Dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + t.TempDir(), "REMOTE_USER=x-access-token",
	}}
	g.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v3/") {
			g.api(w, r)
			return
		}
		g.serveGit(w, r, backend)
	}))
	g.server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	g.server.StartTLS()
	t.Cleanup(g.server.Close)
	g.URL = g.server.URL
	return g
}

// problem records a request the fake refused for a reason a correct
// Menagerie never gives it.
func (g *fakeGitHub) problem(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.problems = append(g.problems, fmt.Sprintf(format, args...))
}

// verifyJWT checks the request's bearer JWT: RS256 with the App's key,
// issued by the App, current.
func (g *fakeGitHub) verifyJWT(r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	g.mu.Lock()
	g.jwts = append(g.jwts, jwt)
	g.mu.Unlock()
	enc := base64.RawURLEncoding
	header, err1 := enc.DecodeString(parts[0])
	payload, err2 := enc.DecodeString(parts[1])
	sig, err3 := enc.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	var h struct{ Alg, Typ string }
	var c struct {
		Iss      string
		Iat, Exp int64
	}
	if json.Unmarshal(header, &h) != nil || h.Alg != "RS256" || json.Unmarshal(payload, &c) != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(g.app, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	now := time.Now().Unix()
	return c.Iss == strconv.Itoa(sampleAppID) && c.Iat <= now+60 && c.Exp > now && c.Exp-c.Iat <= 600
}

var (
	installationPath = regexp.MustCompile(`^/api/v3/repos/([^/]+)/([^/]+)/installation$`)
	tokenPath        = regexp.MustCompile(`^/api/v3/app/installations/([0-9]+)/access_tokens$`)
)

func (g *fakeGitHub) api(w http.ResponseWriter, r *http.Request) {
	if !g.verifyJWT(r) {
		g.problem("%s %s with an invalid JWT", r.Method, r.URL.Path)
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if m := installationPath.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet {
		if m[1] != sampleAccount {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"account":{"login":%q}}`, sampleInstallation, sampleAccount)
		return
	}
	m := tokenPath.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodPost {
		g.problem("%s %s is not an App route", r.Method, r.URL.Path)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	if m[1] != strconv.Itoa(sampleInstallation) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		g.problem("a malformed token request: %v", err)
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return
	}
	perm := body.Permissions["contents"]
	repos := map[string]bool{sampleTemplateRepo: true, sampleBackupRepo: true}
	if len(body.Repositories) != 1 || !repos[body.Repositories[0]] || len(body.Permissions) != 1 || (perm != "read" && perm != "write") {
		g.problem("a token request for %v with %v", body.Repositories, body.Permissions)
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		return
	}
	token := "ghs_" + randomHex() + randomHex()
	g.mu.Lock()
	g.tokens[token] = appGrant{Repository: sampleAccount + "/" + body.Repositories[0], Permission: perm}
	g.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token": token, "expires_at": time.Now().Add(sampleTokenLifetime).UTC().Format(time.RFC3339),
		"permissions": map[string]string{"contents": perm}, "repository_selection": "selected",
	})
}

var gitRepositoryPath = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/`)

// serveGit answers a Git request: 401 without credentials, 403 with a
// token that is not one the fake minted for the repository with enough
// permission, and git http-backend otherwise.
func (g *fakeGitHub) serveGit(w http.ResponseWriter, r *http.Request, backend http.Handler) {
	req := gitRequest{Method: r.Method, Path: r.URL.Path}
	record := func(status int) {
		req.Status = status
		g.mu.Lock()
		g.git = append(g.git, req)
		g.mu.Unlock()
	}
	m := gitRepositoryPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		g.problem("%s %s is not a repository path", r.Method, r.URL.Path)
		record(http.StatusNotFound)
		http.NotFound(w, r)
		return
	}
	req.Repository = m[1] + "/" + m[2]
	user, pass, ok := r.BasicAuth()
	if !ok {
		if a := r.Header.Get("Authorization"); a != "" {
			req.Foreign = a
			record(http.StatusUnauthorized)
			g.problem("%s %s with a credential that is not basic authentication", r.Method, r.URL.Path)
			http.Error(w, "credentials refused", http.StatusUnauthorized)
			return
		}
		record(http.StatusUnauthorized)
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	g.mu.Lock()
	grant, minted := g.tokens[pass]
	g.mu.Unlock()
	if user != "x-access-token" || !minted {
		req.Foreign = r.Header.Get("Authorization")
		record(http.StatusUnauthorized)
		g.problem("%s %s with credentials the fake did not mint", r.Method, r.URL.Path)
		http.Error(w, "credentials refused", http.StatusUnauthorized)
		return
	}
	req.Grant = grant
	push := r.URL.Query().Get("service") == "git-receive-pack" || strings.HasSuffix(r.URL.Path, "/git-receive-pack")
	if grant.Repository != req.Repository || (push && grant.Permission != "write") {
		record(http.StatusForbidden)
		g.problem("%s %s with a token for %s %s", r.Method, r.URL.Path, grant.Repository, grant.Permission)
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	record(http.StatusOK)
	backend.ServeHTTP(w, r)
}

func (g *fakeGitHub) snapshot() (tokens map[string]appGrant, jwts []string, requests []gitRequest, problems []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	tokens = map[string]appGrant{}
	for k, v := range g.tokens {
		tokens[k] = v
	}
	return tokens, append([]string(nil), g.jwts...), append([]gitRequest(nil), g.git...), append([]string(nil), g.problems...)
}

// importAppKey imports the App's private key into OpenBao's Transit engine
// as docs/install.md tells operators to: the PKCS #1 PEM GitHub gives is
// converted to base64 PKCS #8 with openssl, then imported with bao.
func importAppKey(t *testing.T, w *wyrwoodSite, key *rsa.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "app.private-key.pem")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	der, err := exec.Command("openssl", "pkcs8", "-topk8", "-nocrypt", "-in", pemPath, "-outform", "DER").Output()
	if err != nil {
		t.Fatalf("openssl pkcs8: %v", err)
	}
	b64 := filepath.Join(dir, "app-key.b64")
	if err := os.WriteFile(b64, []byte(base64.StdEncoding.EncodeToString(der)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(requireEnv(t, "MENAGERIE_BAO"), "transit", "import", "transit/keys/"+sampleAppTransitKey, "@"+b64, "type=rsa-2048")
	cmd.Env = append(os.Environ(), "BAO_ADDR="+w.baoAddr, "BAO_TOKEN="+w.bao.Token())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bao transit import: %v\n%s", err, out)
	}
	k, err := w.bao.Logical().Read("transit/keys/" + sampleAppTransitKey)
	if err != nil || k == nil || k.Data["type"] != "rsa-2048" || k.Data["exportable"] != false {
		t.Fatalf("the imported key: %v %v", k, err)
	}
}

// jobText is a finished job's snapshot and its whole event stream.
func jobText(t *testing.T, c *http.Client, site, id string) string {
	t.Helper()
	text := fetch(t, c, http.MethodGet, site+"/api/jobs/"+id, nil, nil).body
	resp, err := c.Get(site + "/api/jobs/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	s := bufio.NewScanner(resp.Body)
	s.Buffer(make([]byte, 1<<20), 64<<20)
	var out strings.Builder
	out.WriteString(text)
	event := ""
	for s.Scan() {
		line := s.Text()
		out.WriteString("\n" + line)
		if e, ok := strings.CutPrefix(line, "event: "); ok {
			event = e
		}
		if strings.HasPrefix(line, "data: ") && event == "result" {
			break
		}
	}
	return out.String()
}

func TestImageWyrwoodGitHubApp(t *testing.T) {
	ref := requireEnv(t, "MENAGERIE_IMAGE")
	p := startPocketID(t)
	w := startWyrwood(t, p)
	ctx := context.Background()
	tmp := os.TempDir()
	base := t.TempDir()
	openTo(t, base, tmp)

	// The App's key: generated here, imported into Transit, never given to
	// Menagerie.
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	importAppKey(t, w, appKey)
	gh := startFakeGitHub(t, &appKey.PublicKey)
	writeTo(t, base, "github-ca.pem", string(gh.CA))

	// The private template and an empty backup repository on the fake.
	work := filepath.Join(base, "template-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "-q", "-b", "main")
	browserTemplate(t, work, 1)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "v1")
	gitRun(t, work, "tag", "v1")
	templateDir := filepath.Join(gh.Dir, sampleAccount, sampleTemplateRepo+".git")
	gitRun(t, base, "clone", "-q", "--bare", work, templateDir)
	backupDir := filepath.Join(gh.Dir, sampleAccount, sampleBackupRepo+".git")
	if _, err := git.PlainInit(backupDir, true); err != nil {
		t.Fatal(err)
	}
	pull(t, p, "busybox:1.37")

	rootDir := filepath.Join(base, "workspaces")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chownTree(t, rootDir)
	port := freePort(t)
	site := fmt.Sprintf("http://localhost:%d", port)
	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})
	templateURL := gh.URL + "/" + sampleAccount + "/" + sampleTemplateRepo + ".git"
	backupURL := gh.URL + "/" + sampleAccount + "/" + sampleBackupRepo + ".git"
	writeTo(t, base, "config.yml", fmt.Sprintf(`listen: 127.0.0.1:%d
workspace-root: %s
workspace-repository: %s
wyrwood-directory: /run/wyrwood
oidc:
  issuer: %s
  client-id: %s
  redirect-url: %s
templates:
  - {id: sample-kit-v1, name: "Sample kit, version 1", address: '%s@v1'}
github-app:
  app-id: %d
  transit-key: %s
  server: %s
`, port, rootDir, backupURL, p.url, client["id"], site+auth.CallbackPath, templateURL, sampleAppID, sampleAppTransitKey, gh.URL))

	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: ref,
			// No SSH_AUTH_SOCK and no known hosts: the App is the only
			// identity.
			Env: []string{
				"SSL_CERT_FILE=/etc/menagerie/github-ca.pem",
				fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port),
			},
			Labels: map[string]string{"menagerie-test": "image"},
		},
		&container.HostConfig{
			NetworkMode: "host",
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				tmp + ":" + tmp,
				filepath.Join(base, "config.yml") + ":/etc/menagerie/config.yml:ro",
				filepath.Join(base, "github-ca.pem") + ":/etc/menagerie/github-ca.pem:ro",
				wyrwoodConsumers + "/" + menagerieAppName + ":/run/wyrwood",
			},
		}, nil, nil, menagerieAppName)
	if err != nil {
		t.Fatal(err)
	}
	c := &imageContainer{t: t, docker: p.docker, id: created.ID, site: site}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), c.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	alpha := filepath.Join(rootDir, "alpha")
	t.Cleanup(func() { removeDevcontainers(p, alpha) })

	// Menagerie's policy: sign with the App's Transit key, and no SSH
	// Transit key at all.
	w.savePolicy("sample-menagerie-app", `path "transit/sign/`+sampleAppTransitKey+`/sha2-256" { capabilities = ["update"] }
`, true, nil, "/run/wyrwood")
	w.bind(menagerieAppName, []string{"sample-menagerie-app"}, "1h", "1h", wyrwoodGID)
	waitFor(t, "a token for Menagerie", func() bool { return w.token(menagerieAppName) == "valid" })

	if err := p.docker.ContainerStart(ctx, c.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	c.waitReady(0)
	if t.Failed() {
		return
	}
	b := signInBrowser()
	if s, _ := p.signIn(t, b, "sample-manager", site+"/"); s.status != http.StatusOK {
		t.Fatalf("sign-in answered %d", s.status)
	}

	t.Run("Menagerie has no SSH identity", func(t *testing.T) {
		c.t = t
		if out, code := c.exec(fmt.Sprintf("%d:%d", operatorUID, wyrwoodGID), "sh", "-c", "SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock ssh-add -L"); code == 0 {
			t.Errorf("the agent offers keys: %q", out)
		}
		if out := c.mustExec("sh", "-c", "env | grep -c '^SSH_AUTH_SOCK=' || true"); strings.TrimSpace(out) != "0" {
			t.Errorf("SSH_AUTH_SOCK is set: %q", out)
		}
	})

	t.Run("create fetches a private template and backs up with installation tokens only", func(t *testing.T) {
		c.t = t
		snap := runInterviewJob(t, b, site, "create", "alpha", "sample-kit-v1",
			map[string]any{"label": "Sample label", "size": "small", "verbose": false, "tags": []string{"alpha"}})
		text := jobText(t, b, site, snap.JobID)
		if !strings.Contains(text, "event: result") {
			t.Errorf("the job's event stream has no result: %q", text)
		}
		if snap.State != jobs.Succeeded {
			t.Fatalf("create ended %s: %+v\n%s\n%s", snap.State, snap.Error, text, c.logs(time.Time{}))
		}
		for _, rel := range []string{"result.txt", "hook-marker.txt", ".toha.json"} {
			if _, err := os.Stat(filepath.Join(alpha, rel)); err != nil {
				t.Errorf("%s: %v", rel, err)
			}
		}
		// The pushed branch arrived.
		backups, err := git.PlainOpen(backupDir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backups.Reference(plumbing.NewBranchReferenceName("alpha"), false); err != nil {
			t.Errorf("the backup branch alpha: %v", err)
		}

		tokens, jwts, requests, problems := gh.snapshot()
		if len(problems) > 0 {
			t.Errorf("the fake GitHub refused requests: %q", problems)
		}
		templateRepo, backupRepo := sampleAccount+"/"+sampleTemplateRepo, sampleAccount+"/"+sampleBackupRepo
		fetched, pushed := false, false
		for _, r := range requests {
			switch {
			case r.Foreign != "":
				t.Errorf("%s %s carried credentials the fake did not mint", r.Method, r.Path)
			case r.Grant == appGrant{}:
				// A challenge before the credentials.
			case r.Repository == templateRepo && r.Grant != (appGrant{templateRepo, "read"}):
				t.Errorf("%s %s used a token for %+v", r.Method, r.Path, r.Grant)
			case r.Repository == backupRepo && r.Grant != (appGrant{backupRepo, "write"}):
				t.Errorf("%s %s used a token for %+v", r.Method, r.Path, r.Grant)
			}
			if r.Status == http.StatusOK && r.Repository == templateRepo && strings.HasSuffix(r.Path, "/git-upload-pack") {
				fetched = true
			}
			if r.Status == http.StatusOK && r.Repository == backupRepo && strings.HasSuffix(r.Path, "/git-receive-pack") {
				pushed = true
			}
		}
		if !fetched || !pushed {
			t.Errorf("fetched the template %v, pushed the backup %v: %+v", fetched, pushed, requests)
		}
		grants := map[appGrant]bool{}
		for _, g := range tokens {
			grants[g] = true
		}
		if len(grants) != 2 || !grants[appGrant{templateRepo, "read"}] || !grants[appGrant{backupRepo, "write"}] {
			t.Errorf("minted tokens for %v", grants)
		}

		// No job event, log line or reply holds a token or a JWT.
		logs := c.logs(time.Time{})
		answers := fetch(t, b, http.MethodGet, site+"/api/jobs", nil, nil).body
		for _, secret := range append(jwts, keys(tokens)...) {
			for name, text := range map[string]string{"the job": text, "the container log": logs, "the job list": answers} {
				if strings.Contains(text, secret) {
					t.Errorf("%s holds a token or JWT", name)
				}
			}
		}
		if len(jwts) == 0 {
			t.Error("the fake GitHub saw no JWT")
		}
	})
}

func keys(m map[string]appGrant) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
