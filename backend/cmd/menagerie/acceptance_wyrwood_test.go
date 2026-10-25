//go:build pocketid && image && acceptance

package main

// TestImageWyrwood runs the built image, MENAGERIE_IMAGE, bound by a real
// Wyrwood agent (see wyrwood_test.go) as its workspace root's owner, uid
// 1000, with the binding's access group as the only way to its sockets.
// Through that binding Menagerie fetches a private template over SSH,
// pulls a private devcontainer image with credentials read from OpenBao,
// and backs workspaces up over SSH. The suite then takes the binding's
// token through renewal, expiry and removal and reads each outcome from a
// Menagerie job. Every key, password and token is generated here.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

const (
	// registryImage is the private registry the devcontainer images come from.
	registryImage = "registry:3.1.2@sha256:01cd312e67026f06f5d89c3e133a4a9adc7cb9eeb9b1194e0b2954dfd770efd2"
	// privateBase is pushed to the private registry as the devcontainer image.
	privateBase = "busybox:1.37"
	// The binding's tokens live one minute, Wyrwood's shortest lifetime, so
	// the suite outlives one within a test-only wait of tokenOutlived.
	tokenTTL      = "1m"
	tokenOutlived = 75 * time.Second
	// The container Wyrwood binds by name.
	menagerieName = "sample-menagerie"
)

// privateRegistry is a registry container that admits one generated user.
type privateRegistry struct {
	addr, user, password string
}

func startPrivateRegistry(t *testing.T, p *pocketID, dir string) *privateRegistry {
	t.Helper()
	ctx := context.Background()
	r := &privateRegistry{addr: fmt.Sprintf("127.0.0.1:%d", freePort(t)), user: "sample-registry-user", password: randomHex()}
	hash, err := bcrypt.GenerateFromPassword([]byte(r.password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	writeTo(t, dir, "htpasswd", r.user+":"+string(hash)+"\n")
	pull(t, p, registryImage)
	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{Image: registryImage, Labels: map[string]string{"menagerie-test": "registry"}, Env: []string{
			"REGISTRY_HTTP_ADDR=" + r.addr, "REGISTRY_AUTH=htpasswd", "REGISTRY_AUTH_HTPASSWD_REALM=sample",
			"REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd"}},
		&container.HostConfig{NetworkMode: "host", Binds: []string{filepath.Join(dir, "htpasswd") + ":/auth/htpasswd:ro"}}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := p.docker.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the registry", func() bool {
		resp, err := http.Get("http://" + r.addr + "/v2/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized
	})
	return r
}

// push tags the private base as repo:tag in the registry and pushes it
// with the registry's user, then removes the local tag so that only the
// registry holds it.
func (r *privateRegistry) push(t *testing.T, p *pocketID, repo, tag string) string {
	t.Helper()
	ctx := context.Background()
	pull(t, p, privateBase)
	ref := r.addr + "/" + repo + ":" + tag
	if err := p.docker.ImageTag(ctx, privateBase, ref); err != nil {
		t.Fatal(err)
	}
	authJSON, _ := json.Marshal(map[string]string{"username": r.user, "password": r.password, "serveraddress": r.addr})
	rc, err := p.docker.ImagePush(ctx, ref, image.PushOptions{RegistryAuth: base64.URLEncoding.EncodeToString(authJSON)})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if strings.Contains(string(body), `"error"`) {
		t.Fatalf("push %s: %s", ref, body)
	}
	r.forget(t, p, ref)
	return ref
}

// forget removes the daemon's tag for ref, so the next start pulls it.
func (r *privateRegistry) forget(t *testing.T, p *pocketID, ref string) {
	t.Helper()
	if _, err := p.docker.ImageRemove(context.Background(), ref, image.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
}

func pull(t *testing.T, p *pocketID, ref string) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.docker.ImageInspect(ctx, ref); err == nil {
		return
	}
	rc, err := p.docker.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		t.Fatalf("pull %s: %v", ref, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
}

// wyrwoodTemplate writes version 1 or 2 of the sample template with a
// devcontainer whose image is ref.
func wyrwoodTemplate(t *testing.T, dir string, version int, ref string) {
	t.Helper()
	browserTemplate(t, dir, version)
	writeTo(t, dir, "template/.devcontainer/devcontainer.json", `{
  "name": "sample",
  "image": "`+ref+`",
  "initializeCommand": "mkdir -p markers && echo initialized > markers/initialize.txt"
}
`)
}

// transitKey reads the public key of an ed25519 Transit key as an SSH key.
func transitKey(t *testing.T, w *wyrwoodSite, name string) ssh.PublicKey {
	t.Helper()
	key, err := w.bao.Logical().Read("transit/keys/" + name)
	if err != nil {
		t.Fatal(err)
	}
	versions, _ := key.Data["keys"].(map[string]any)
	latest, _ := versions[fmt.Sprint(key.Data["latest_version"])].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(fmt.Sprint(latest["public_key"]))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(ed25519.PublicKey(raw))
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestImageWyrwood(t *testing.T) {
	ref := requireEnv(t, "MENAGERIE_IMAGE")
	p := startPocketID(t)
	w := startWyrwood(t, p)
	ctx := context.Background()
	tmp := os.TempDir()

	base := t.TempDir()
	openTo(t, base, tmp)
	reg := startPrivateRegistry(t, p, base)
	v1Image := reg.push(t, p, "sample/private", "1")
	v2Image := reg.push(t, p, "sample/private", "2")
	if _, err := w.bao.Logical().Write("kv/data/sample/registry", map[string]any{
		"data": map[string]any{"username": reg.user, "password": reg.password}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.bao.Logical().Write("transit/keys/sample-git", map[string]any{"type": "ed25519"}); err != nil {
		t.Fatal(err)
	}

	// The Git host serves the template and the workspace repository and
	// admits only Wyrwood's Transit key besides its own agent's.
	host := backuptest.New(t)
	host.Authorize(transitKey(t, w, "sample-git"))
	work := filepath.Join(base, "template-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "-q", "-b", "main")
	wyrwoodTemplate(t, work, 1, v1Image)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "v1")
	gitRun(t, work, "tag", "v1")
	wyrwoodTemplate(t, work, 2, v2Image)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "v2")
	gitRun(t, work, "tag", "v2")
	gitRun(t, host.Dir, "clone", "-q", "--bare", work, filepath.Join(host.Dir, "sample-kit.git"))
	if _, err := git.PlainInit(filepath.Join(host.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(host.KnownHosts, 0o644); err != nil {
		t.Fatal(err)
	}

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
  - {id: sample-kit-v2, name: "Sample kit, version 2", address: '%s@v2'}
registry-credentials:
  kv-mount: kv
  registries:
    - {registry: '%s', path: sample/registry}
`, port, rootDir, host.URL("backups.git"), p.url, client["id"], site+auth.CallbackPath,
		host.URL("sample-kit.git"), host.URL("sample-kit.git"), reg.addr))

	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: ref,
			Env: []string{
				"SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock",
				fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port),
			},
			Labels: map[string]string{"menagerie-test": "image"},
		},
		&container.HostConfig{
			NetworkMode:   "host",
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				tmp + ":" + tmp,
				filepath.Join(base, "config.yml") + ":/etc/menagerie/config.yml:ro",
				host.KnownHosts + ":/etc/ssh/ssh_known_hosts:ro",
				wyrwoodConsumers + "/" + menagerieName + ":/run/wyrwood",
			},
		}, nil, nil, menagerieName)
	if err != nil {
		t.Fatal(err)
	}
	c := &imageContainer{t: t, docker: p.docker, id: created.ID, site: site}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), c.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	t.Cleanup(func() {
		for _, name := range []string{"alpha", "beta"} {
			removeDevcontainers(p, filepath.Join(rootDir, name))
		}
	})

	// Menagerie's policy: read the registry secret, sign with the Transit key.
	const hcl = `path "kv/data/sample/registry" { capabilities = ["read"] }
path "transit/keys/sample-git" { capabilities = ["read"] }
path "transit/sign/sample-git" { capabilities = ["update"] }
`
	w.savePolicy("sample-menagerie", hcl, true, []string{"sample-git"}, "/run/wyrwood")
	w.savePolicy("sample-menagerie-fixed", hcl, false, []string{"sample-git"}, "/run/wyrwood")
	w.bind(menagerieName, []string{"sample-menagerie"}, tokenTTL, "1h", wyrwoodGID)
	waitFor(t, "a token for Menagerie", func() bool { return w.token(menagerieName) == "valid" })

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
	alpha := filepath.Join(rootDir, "alpha")

	vault := func(path string) (string, int) {
		return c.exec("", "curl", "-sS", "-o", "/dev/stdout", "-w", "\n%{http_code}", "--unix-socket", "/run/wyrwood/vault.sock", "http://vault/v1/"+path)
	}
	backupCommits := func() int {
		backups, err := git.PlainOpen(filepath.Join(host.Dir, "backups.git"))
		if err != nil {
			t.Fatal(err)
		}
		ref, err := backups.Reference(plumbing.NewBranchReferenceName("alpha"), false)
		if err != nil {
			return 0
		}
		n := 0
		iter, _ := backups.Log(&git.LogOptions{From: ref.Hash()})
		_ = iter.ForEach(func(*object.Commit) error { n++; return nil })
		return n
	}

	t.Run("the services reach Wyrwood's sockets through the access group", func(t *testing.T) {
		c.t = t
		for _, service := range []string{"template-service", "workspace-service"} {
			out := c.mustExec("sh", "-c", "grep -E '^(Uid|Groups):' /proc/$(/command/s6-svstat -o pid /run/service/"+service+")/status")
			if !strings.Contains(out, fmt.Sprintf("Uid:\t%d", operatorUID)) || !strings.Contains(" "+out+" ", fmt.Sprintf(" %d ", wyrwoodGID)) {
				t.Errorf("%s: %q", service, out)
			}
		}
		if out := c.mustExec("stat", "-c", "%a %u %g", "/run/wyrwood", "/run/wyrwood/vault.sock", "/run/wyrwood/ssh-agent.sock"); out != fmt.Sprintf("750 0 %d\n660 0 %d\n660 0 %d", wyrwoodGID, wyrwoodGID, wyrwoodGID) {
			t.Errorf("consumer directory %q", out)
		}
		// The services' user reaches the agent; a user without the group does not.
		out, code := c.exec(fmt.Sprintf("%d:%d", operatorUID, operatorGID), "sh", "-c", "SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock ssh-add -L")
		if code == 0 {
			t.Errorf("uid %d without the access group listed keys: %q", operatorUID, out)
		}
		out, code = c.exec(fmt.Sprintf("%d:%d", operatorUID, wyrwoodGID), "sh", "-c", "SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock ssh-add -L")
		if code != 0 || !strings.HasSuffix(out, "sample-git") {
			t.Errorf("ssh-add with the access group exited %d: %q", code, out)
		}
	})

	t.Run("create fetches a private template, pulls a private image and backs up through Wyrwood", func(t *testing.T) {
		c.t = t
		snap := runInterviewJob(t, b, site, "create", "alpha", "sample-kit-v1",
			map[string]any{"label": "Sample label", "size": "small", "verbose": false, "tags": []string{"alpha"}})
		if snap.State != jobs.Succeeded {
			t.Fatalf("create ended %s: %+v\n%s", snap.State, snap.Error, c.logs(time.Time{}))
		}
		for _, rel := range []string{"result.txt", "hook-marker.txt", ".toha.json", "markers/initialize.txt"} {
			if _, err := os.Stat(filepath.Join(alpha, rel)); err != nil {
				t.Errorf("%s: %v", rel, err)
			}
		}
		if others := notOperatorOwned(t, alpha); len(others) > 0 {
			t.Errorf("paths the operator does not own: %v", others)
		}
		if got := devcontainerImage(t, p, alpha); got != v1Image {
			t.Errorf("the devcontainer runs %q, not the private %q", got, v1Image)
		}
		commands := strings.Join(host.Commands(), "\n")
		if !strings.Contains(commands, "git-upload-pack") || !strings.Contains(commands, "sample-kit.git") || !strings.Contains(commands, "git-receive-pack") {
			t.Errorf("the Git host ran %q", commands)
		}
		if backupCommits() != 1 {
			t.Errorf("backup commits %d", backupCommits())
		}
	})

	t.Run("the agent renews the token while the controller is down", func(t *testing.T) {
		c.t = t
		w.stopController()
		// Later subtests need the controller, whatever this one's outcome.
		defer w.startController()
		started := time.Now()
		time.Sleep(tokenOutlived)
		if out, code := vault("auth/token/lookup-self"); code != 0 || !strings.HasSuffix(out, "\n200") {
			t.Fatalf("lookup-self after %s: %d %q", time.Since(started).Round(time.Second), code, out)
		}
		// The upgrade pulls version 2's private image with the renewed token.
		snap := runInterviewJob(t, b, site, "upgrade", "alpha", "sample-kit-v2", map[string]any{"region": "north"})
		if snap.State != jobs.Succeeded {
			t.Fatalf("upgrade ended %s: %+v\n%s", snap.State, snap.Error, c.logs(time.Time{}))
		}
		if got := devcontainerImage(t, p, alpha); got != v2Image {
			t.Errorf("the rebuilt devcontainer runs %q, not %q", got, v2Image)
		}
		// The create commit, then the upgrade's; nothing was edited between.
		if got := backupCommits(); got != 2 {
			t.Errorf("backup commits %d after the upgrade", got)
		}
	})

	t.Run("an expired token is a failed job until the agent has a new one", func(t *testing.T) {
		c.t = t
		w.rebind(menagerieName, []string{"sample-menagerie-fixed"}, tokenTTL, tokenTTL, wyrwoodGID)
		waitFor(t, "the fixed-lifetime token", func() bool { return w.token(menagerieName) == "valid" })
		w.stopController()
		defer w.startController()
		waitFor(t, "the token to expire", func() bool {
			out, _ := vault("auth/token/lookup-self")
			return strings.Contains(out, `"no-token"`)
		})
		reg.forget(t, p, v2Image)
		snap := runJob(t, b, site, "rebuild", "alpha")
		if snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != "registry-credentials-unavailable" ||
			!strings.Contains(snap.Error.Message, "Wyrwood holds no OpenBao token for Menagerie (state no-token)") {
			t.Fatalf("rebuild without a token ended %s: %+v", snap.State, snap.Error)
		}
		if strings.Contains(snap.Error.Message, reg.password) {
			t.Error("the job error holds the registry password")
		}
		w.startController()
		waitFor(t, "a replacement token", func() bool { return w.token(menagerieName) == "valid" })
		if snap := runJob(t, b, site, "rebuild", "alpha"); snap.State != jobs.Succeeded {
			t.Fatalf("rebuild with the replacement token ended %s: %+v", snap.State, snap.Error)
		}
	})

	t.Run("an unbound Menagerie fails pulls and private template fetches", func(t *testing.T) {
		c.t = t
		w.unbind(menagerieName)
		waitFor(t, "the sockets to go", func() bool {
			_, code := c.exec("", "test", "-e", "/run/wyrwood/vault.sock")
			return code != 0
		})
		// A new connection finds no socket. Menagerie's client may still hold
		// an idle connection to the agent from the last job, and the agent
		// answers on it that Menagerie is no longer bound. Both are reported
		// as the registry credentials being unavailable.
		if out, code := vault("auth/token/lookup-self"); code == 0 {
			t.Errorf("a new connection reached the vault socket: %q", out)
		}
		reg.forget(t, p, v2Image)
		snap := runJob(t, b, site, "rebuild", "alpha")
		reasons := []string{"Wyrwood holds no OpenBao token for Menagerie (state not-bound)", "the Wyrwood vault socket could not be reached"}
		if snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != "registry-credentials-unavailable" ||
			!(strings.HasSuffix(snap.Error.Message, reasons[0]) || strings.HasSuffix(snap.Error.Message, reasons[1])) {
			t.Fatalf("rebuild unbound ended %s: %+v", snap.State, snap.Error)
		}
		t.Logf("rebuild unbound: %s", snap.Error.Message)
		// The private template is fetched over SSH through the agent, so a
		// create fails before it asks anything and writes no folder.
		header := http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}}
		started := fetch(t, b, http.MethodPost, site+"/api/jobs", strings.NewReader(`{"operation":"create","workspace":"beta","templateId":"sample-kit-v1"}`), header)
		if started.status != http.StatusAccepted {
			t.Fatalf("create answered %d %s", started.status, started.body)
		}
		if err := json.Unmarshal([]byte(started.body), &snap); err != nil {
			t.Fatal(err)
		}
		snap = waitJob(t, b, site, snap.JobID)
		if snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != "template-refused" ||
			!strings.Contains(snap.Error.Message, "were not accepted by the remote") {
			t.Fatalf("create unbound ended %s: %+v", snap.State, snap.Error)
		}
		if _, err := os.Stat(filepath.Join(rootDir, "beta")); !os.IsNotExist(err) {
			t.Errorf("the failed create left a folder: %v", err)
		}
	})
}
