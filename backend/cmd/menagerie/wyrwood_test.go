//go:build pocketid && image && acceptance

package main

// A real Wyrwood site for the acceptance suites: the released wyrwood
// executable (MENAGERIE_WYRWOOD) as controller and agent, and a disposable
// OpenBao dev server (MENAGERIE_BAO). The controller and OpenBao run in this
// process's network namespace; the agent and its Engine proxy run in one
// labelled container on the Docker Engine named by DOCKER_HOST, so its
// paths are that container's own and its consumer directories are the
// Engine host's /run/wyrwood/containers, which containers bind. Operators
// sign in to the controller through the suite's Pocket ID. Every token,
// key and secret is generated here; nothing is read from the host.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	openbao "github.com/openbao/openbao/api/v2"
)

const (
	// wyrwoodConsumers is the agent's default consumer root on the Engine host.
	wyrwoodConsumers = "/run/wyrwood/containers"
	// wyrwoodAgentImage carries the shell the agent container runs; the
	// wyrwood executable is static and bound in from the work folder.
	wyrwoodAgentImage = "busybox:1.37"
	// wyrwoodOperatorsGroup is the Pocket ID group the controller admits.
	wyrwoodOperatorsGroup = "wyrwood_operators"
	// Test-only bound, wyrwoodInterval apart, on the controller, the agent
	// and a binding reaching their next state. Each took under 5 s on the
	// shared host.
	wyrwoodAttempts = 240
	wyrwoodInterval = 250 * time.Millisecond
)

// wyrwoodSite is a running controller, OpenBao and approved agent.
type wyrwoodSite struct {
	t         *testing.T
	p         *pocketID
	bin       string
	bao       *openbao.Client
	baoAddr   string
	site      string
	sshAddr   string
	agent     *imageContainer
	agentName string
	operator  *http.Client
	// The controller's command line and log, so it can stop and start again.
	controllerArgs []string
	controllerLog  string
	controller     *process
}

// process is a test-owned server process, stopped by its own PID.
type process struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// startProcess starts a server process, appending its output to log, and
// stops it when the test ends.
func startProcess(t *testing.T, log string, name string, args ...string) *process {
	t.Helper()
	out, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(log))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &process{cmd: cmd, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.exited); out.Close() }()
	t.Cleanup(p.stop)
	return p
}

// running reports whether the process has not exited.
func (p *process) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// stop ends the process and waits for it; it is safe to repeat.
func (p *process) stop() {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

// waitFor polls ok until it holds, failing the test with what.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; !ok(); i++ {
		if i == wyrwoodAttempts {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(wyrwoodInterval)
	}
}

// startOpenBao starts an OpenBao dev server on loopback with the mounts the
// controller is configured with.
func startOpenBao(t *testing.T, dir string) (*openbao.Client, string) {
	t.Helper()
	bin := requireEnv(t, "MENAGERIE_BAO")
	port := freePort(t)
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	root := randomHex()
	startProcess(t, filepath.Join(dir, "openbao.log"), bin, "server", "-dev",
		"-dev-root-token-id="+root, fmt.Sprintf("-dev-listen-address=127.0.0.1:%d", port), "-dev-no-store-token")
	cfg := openbao.DefaultConfig()
	cfg.Address = addr
	client, err := openbao.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.SetToken(root)
	waitFor(t, "OpenBao", func() bool {
		h, err := client.Sys().Health()
		return err == nil && h.Initialized && !h.Sealed
	})
	sys := client.Sys()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(sys.EnableAuthWithOptions("approle", &openbao.EnableAuthOptions{Type: "approle"}))
	must(sys.Mount("kv", &openbao.MountInput{Type: "kv", Options: map[string]string{"version": "2"}}))
	must(sys.Mount("transit", &openbao.MountInput{Type: "transit"}))
	must(sys.Mount("ssh", &openbao.MountInput{Type: "ssh"}))
	// The controller's own policy, as Wyrwood's installation documents it.
	must(sys.PutPolicy("controller", `
path "sys/policies/acl/wyrwood-*" { capabilities = ["create", "read", "update", "delete", "list"] }
path "auth/approle/role/wyrwood-*" { capabilities = ["create", "read", "update", "delete", "list"] }
path "auth/approle/role/wyrwood-*/role-id" { capabilities = ["read"] }
path "auth/approle/role/wyrwood-*/secret-id" { capabilities = ["create", "update"] }
path "sys/wrapping/wrap" { capabilities = ["update"] }
path "kv/metadata" { capabilities = ["list"] }
path "kv/metadata/*" { capabilities = ["list"] }
path "transit/keys" { capabilities = ["list"] }
path "transit/keys/*" { capabilities = ["create", "read", "update", "list"] }
path "ssh/roles" { capabilities = ["list"] }
path "ssh/roles/*" { capabilities = ["create", "read", "update", "list"] }
path "auth/token/renew-self" { capabilities = ["update"] }
path "auth/token/revoke-accessor" { capabilities = ["update"] }
`))
	_, err = client.Logical().Write("auth/approle/role/controller", map[string]any{"token_policies": "controller", "token_ttl": "1h"})
	must(err)
	return client, addr
}

// pageData reads the JSON a controller page embeds for its interface.
func pageData(t *testing.T, s step, into any) {
	t.Helper()
	m := regexp.MustCompile(`<script id="wyrwood-page" type="application/json">(.*?)</script>`).FindStringSubmatch(s.body)
	if m == nil {
		t.Fatalf("%s answered %d without page data: %.300q", s.url, s.status, s.body)
	}
	var page struct {
		Page string          `json:"page"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(m[1]), &page); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(page.Data, into); err != nil {
		t.Fatalf("%s page data: %v", page.Page, err)
	}
}

// wyrwoodLayout is the part of every signed-in page the suite reads.
type wyrwoodLayout struct {
	CSRF    string
	Notice  string
	Problem string
}

// startWyrwood starts OpenBao, the controller and the agent, signs an
// operator in and approves the agent.
func startWyrwood(t *testing.T, p *pocketID) *wyrwoodSite {
	t.Helper()
	ctx := context.Background()
	bin := requireEnv(t, "MENAGERIE_WYRWOOD")
	dir := t.TempDir()
	w := &wyrwoodSite{t: t, p: p, bin: bin}
	w.bao, w.baoAddr = startOpenBao(t, dir)

	role, err := w.bao.Logical().Read("auth/approle/role/controller/role-id")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := w.bao.Logical().Write("auth/approle/role/controller/secret-id", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeSecret := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	secretID := writeSecret("secret-id", secret.Data["secret_id"].(string))

	// The controller signs operators in through Pocket ID as a confidential client.
	httpPort, sshPort := freePort(t), freePort(t)
	w.site = fmt.Sprintf("http://localhost:%d", httpPort)
	w.sshAddr = fmt.Sprintf("127.0.0.1:%d", sshPort)
	operators := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Wyrwood operators", "name": wyrwoodOperatorsGroup})
	p.user("sample-operator", "Sample", "Operator", operators["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Wyrwood sample", "callbackURLs": []string{w.site + "/auth/callback"}, "isPublic": false, "pkceEnabled": false})
	// Pocket ID returns a confidential client's secret once, on creation.
	created, _ := client["createdSecret"].(map[string]any)
	value, ok := created["secret"].(string)
	if !ok {
		keys := []string{}
		for k := range created {
			keys = append(keys, k)
		}
		t.Fatalf("Pocket ID created no client secret; createdSecret has %v", keys)
	}
	clientSecret := writeSecret("client-secret", value)

	config := filepath.Join(dir, "controller.yml")
	writeTo(t, dir, "controller.yml", fmt.Sprintf(`http:
  listen: 127.0.0.1:%d
ssh:
  listen: %s
state:
  directory: %s
openbao:
  address: %s
  secret-mounts:
    - path: kv
      type: kv-v2
  approle:
    role-id: %s
    secret-id-file: %s
oidc:
  provider-name: Pocket ID
  issuer: %s
  client-id: %s
  client-secret-file: %s
  redirect-url: %s/auth/callback
  allowed-groups:
    - %s
`, httpPort, w.sshAddr, filepath.Join(dir, "state"), w.baoAddr, role.Data["role_id"], secretID, p.url, client["id"], clientSecret, w.site, wyrwoodOperatorsGroup))
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	w.controllerArgs = []string{"controller", "serve", "--config", config}
	w.controllerLog = filepath.Join(dir, "controller.log")
	w.startController()
	fingerprint, err := exec.Command(bin, "controller", "host-key", "--config", config).Output()
	if err != nil {
		t.Fatalf("controller host-key: %v", err)
	}

	// The agent and its Engine proxy, in one container on the Engine host.
	if _, err := p.docker.ImageInspect(ctx, wyrwoodAgentImage); err != nil {
		rc, err := p.docker.ImagePull(ctx, wyrwoodAgentImage, image.PullOptions{})
		if err != nil {
			t.Fatalf("pull %s: %v", wyrwoodAgentImage, err)
		}
		_, _ = bytes.NewBuffer(nil).ReadFrom(rc)
		rc.Close()
	}
	w.agentName = "sample-host"
	agent, err := p.docker.ContainerCreate(ctx,
		&container.Config{Image: wyrwoodAgentImage, Hostname: w.agentName, Cmd: []string{"sleep", "infinity"},
			Labels: map[string]string{"menagerie-test": "wyrwood-agent"}},
		&container.HostConfig{NetworkMode: "host", Binds: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			bin + ":/usr/local/bin/wyrwood:ro",
			wyrwoodConsumers + ":" + wyrwoodConsumers,
		}}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	w.agent = &imageContainer{t: t, docker: p.docker, id: agent.ID}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), agent.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := p.docker.ContainerStart(ctx, agent.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Wyrwood agent logs:\n%s", w.logs())
		}
	})
	w.agent.mustExec("sh", "-c", fmt.Sprintf(`set -e
install -d -m 0755 /etc/wyrwood /var/lib/wyrwood /run/wyrwood
install -d -m 0750 /run/wyrwood-engine
cat > /etc/wyrwood/agent.yml <<EOF
controller:
  url: ssh://%s
engine:
  socket: /var/run/docker.sock
  allow-recreate: false
EOF`, w.sshAddr))
	// The first login asks the controller for approval and reports pending.
	out, code := w.agent.exec("", "wyrwood", "agent", "login", "ssh://"+w.sshAddr, "--accept-fingerprint", strings.TrimSpace(string(fingerprint)), "--output", "json")
	var login struct{ Status string }
	if err := json.Unmarshal([]byte(out), &login); err != nil || login.Status != "pending" {
		t.Fatalf("agent login exited %d: %s", code, out)
	}

	// An operator signs in and approves the agent's key.
	w.operator = signInBrowser()
	w.signIn()
	var agents struct {
		wyrwoodLayout
		PendingKeys []struct{ Name, Fingerprint string }
	}
	waitFor(t, "the agent's approval request", func() bool {
		pageData(t, fetch(t, w.operator, http.MethodGet, w.site+"/agents", nil, nil), &agents)
		return len(agents.PendingKeys) == 1
	})
	w.post("/agents/decide", url.Values{"fingerprint": {agents.PendingKeys[0].Fingerprint}, "decision": {"approve"}})

	w.agent.mustExec("sh", "-c", "wyrwood agent engine-proxy > /tmp/engine-proxy.log 2>&1 &")
	waitFor(t, "the Engine proxy", func() bool {
		_, code := w.agent.exec("", "test", "-S", "/run/wyrwood-engine/engine.sock")
		return code == 0
	})
	w.agent.mustExec("sh", "-c", "wyrwood agent daemon > /tmp/agent.log 2>&1 &")
	waitFor(t, "the agent's connection", func() bool {
		out, code := w.agent.exec("", "wyrwood", "agent", "status", "--output", "json")
		return code == 0 && strings.Contains(out, `"session":"connected"`)
	})
	return w
}

// startController starts the controller, unless it runs, and waits for it
// to answer.
func (w *wyrwoodSite) startController() {
	w.t.Helper()
	if w.controller != nil && w.controller.running() {
		return
	}
	w.controller = startProcess(w.t, w.controllerLog, w.bin, w.controllerArgs...)
	waitFor(w.t, "the controller", func() bool {
		resp, err := http.Get(w.site + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusNoContent
	})
}

// stopController stops the controller; agents keep serving their tokens.
func (w *wyrwoodSite) stopController() { w.controller.stop() }

// signIn signs the operator in to the controller through Pocket ID.
func (w *wyrwoodSite) signIn() {
	t := w.t
	t.Helper()
	if s := fetch(t, w.operator, http.MethodPost, w.p.url+"/api/one-time-access-token/"+w.p.signInLink("sample-operator"), nil, nil); s.status != http.StatusOK {
		t.Fatalf("one-time sign-in answered %d", s.status)
	}
	s := fetch(t, w.operator, http.MethodPost, w.site+"/auth/start", strings.NewReader(""), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	if s.status >= 300 && s.status < 400 && strings.HasPrefix(s.location, "/interaction?interaction=") {
		id := strings.TrimPrefix(s.location, "/interaction?interaction=")
		consent := fetch(t, w.operator, http.MethodPost, w.p.url+"/api/oidc/interactions/"+id+"/complete", strings.NewReader(`{"step":"consent"}`), http.Header{"Content-Type": {"application/json"}})
		if consent.status != http.StatusOK {
			t.Fatalf("consent answered %d %s", consent.status, consent.body)
		}
		s = fetch(t, w.operator, http.MethodGet, w.p.url+"/authorize?interaction="+id, nil, nil)
	}
	if s.status != http.StatusOK || s.url.Path == "/sign-in" {
		t.Fatalf("controller sign-in ended at %s with %d: %.300q", s.url, s.status, s.body)
	}
}

// post submits a controller form with the session's anti-forgery token and
// returns the page it redirects to, failing on a problem.
func (w *wyrwoodSite) post(path string, form url.Values) step {
	t := w.t
	t.Helper()
	var page wyrwoodLayout
	pageData(t, fetch(t, w.operator, http.MethodGet, w.site+"/agents", nil, nil), &page)
	form.Set("csrf", page.CSRF)
	s := fetch(t, w.operator, http.MethodPost, w.site+path, strings.NewReader(form.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	var after wyrwoodLayout
	pageData(t, s, &after)
	if after.Problem != "" || s.status != http.StatusOK {
		t.Fatalf("POST %s answered %d at %s: %q", path, s.status, s.url, after.Problem)
	}
	return s
}

// wyrwoodRow is a container row the controller shows.
type wyrwoodRow struct {
	Agent, ID, Name, State, KeyKind, KeyValue, BindingID, Token string
	Bound                                                       bool
	Revision                                                    int
}

// row returns the controller's row for a container name, once reported.
func (w *wyrwoodSite) row(name string) wyrwoodRow {
	w.t.Helper()
	var found wyrwoodRow
	waitFor(w.t, "the agent to report "+name, func() bool {
		var page struct {
			wyrwoodLayout
			Rows []wyrwoodRow
		}
		pageData(w.t, fetch(w.t, w.operator, http.MethodGet, w.site+"/containers?agent="+url.QueryEscape(w.agentName), nil, nil), &page)
		for _, r := range page.Rows {
			if r.Name == name {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

// savePolicy saves a Wyrwood policy whose contract mounts the consumer
// directory at target.
func (w *wyrwoodSite) savePolicy(name, hcl string, renewal bool, transitKeys []string, target string) {
	w.t.Helper()
	w.post("/policies", url.Values{
		"new": {"yes"}, "name": {name}, "hcl": {hcl}, "agent-token-renewal": {fmt.Sprint(renewal)},
		"transit-keys": {strings.Join(transitKeys, "\n")}, "mount-target": {target},
	})
}

// bind binds the named container to the policies with the given settings.
func (w *wyrwoodSite) bind(name string, policies []string, ttl, maxTTL string, accessGroup int) {
	w.t.Helper()
	r := w.row(name)
	form := url.Values{"agent": {r.Agent}, "container": {r.ID}, "key": {"name:" + name}, "policies": policies,
		"token-ttl": {ttl}, "token-max-ttl": {maxTTL}, "enforcement": {"report"}}
	if accessGroup >= 0 {
		form.Set("access-group", fmt.Sprint(accessGroup))
	}
	w.post("/bindings", form)
}

// rebind replaces the named container's binding with new policies and
// settings, at the binding's current revision.
func (w *wyrwoodSite) rebind(name string, policies []string, ttl, maxTTL string, accessGroup int) {
	w.t.Helper()
	r := w.row(name)
	w.post("/bindings/"+url.PathEscape(r.BindingID)+"/policy", url.Values{"agent": {r.Agent}, "container": {r.ID},
		"policies": policies, "token-ttl": {ttl}, "token-max-ttl": {maxTTL}, "enforcement": {"report"},
		"access-group": {fmt.Sprint(accessGroup)}, "revision": {fmt.Sprint(r.Revision)}})
}

// token is the agent's own token state for a container: valid, none, ...
func (w *wyrwoodSite) token(name string) string {
	w.t.Helper()
	var list struct {
		Containers []struct{ Name, Token, Conformance string }
	}
	if err := json.Unmarshal([]byte(w.agentContainers()), &list); err != nil {
		w.t.Fatalf("agent containers: %v", err)
	}
	for _, c := range list.Containers {
		if c.Name == name {
			return c.Token
		}
	}
	return ""
}

// unbind deletes the named container's binding.
func (w *wyrwoodSite) unbind(name string) {
	w.t.Helper()
	r := w.row(name)
	if r.BindingID == "" {
		w.t.Fatalf("%s is not bound", name)
	}
	w.post("/bindings/"+url.PathEscape(r.BindingID)+"/unbind", url.Values{})
}

// agentContainer is the agent's own view of one container.
func (w *wyrwoodSite) agentContainers() string {
	w.t.Helper()
	out, _ := w.agent.exec("", "wyrwood", "agent", "containers", "--output", "json")
	return out
}

// logs returns the agent's and the Engine proxy's logs, for failures.
func (w *wyrwoodSite) logs() string {
	out, _ := w.agent.exec("", "sh", "-c", "tail -n 50 /tmp/agent.log /tmp/engine-proxy.log")
	return out
}
