//go:build docker

// Docker proxy tests. They run when the suite runs through the Docker
// proxy (task test:docker:proxy): DOCKER_HOST names the proxy, which loads
// deploy/sockguard.yml as scripts/proxy-config.sh renders it, and
// MENAGERIE_TEST_ENGINE names the Engine behind it, which the tests
// observe through. MENAGERIE_TEST_PROXY_ROOT is the proxy's workspace
// root and MENAGERIE_TEST_PROXY_EXCEPTIONS the folder of its exceptions
// file. MENAGERIE_TEST_PROXY_PENDING lists the Sockguard fork changes the
// pinned proxy lacks, by issue number; a case that needs one waits.
package devcontainer_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// pendingChanges names what each pending fork change holds back.
var pendingChanges = map[string]string{
	"1571":          "per-workspace exceptions, roots, identity headers and the restart-policy setting",
	"1814":          "the LLBBridge passthrough that devcontainer builds need",
	"1819":          "label filters kept under ownership",
	"1822":          "deny reasons in a profile's denials (response.deny_verbosity)",
	"missing-roots": "refusing to start when a bind root or the workspace root is missing",
}

// proxied reports whether the suite runs through the Docker proxy.
func proxied() bool { return os.Getenv("MENAGERIE_TEST_PROXY_ROOT") != "" }

// waitsOn skips t, in a run through the proxy, while a fork change it
// needs is pending. A run straight against the Engine never waits.
func waitsOn(t *testing.T, changes ...string) {
	t.Helper()
	if !proxied() {
		return
	}
	pending := strings.Fields(os.Getenv("MENAGERIE_TEST_PROXY_PENDING"))
	for _, c := range changes {
		if slices.Contains(pending, c) {
			t.Skipf("waiting on the Sockguard fork change %s: %s", c, pendingChanges[c])
		}
	}
}

// notThroughTheProxy skips t in a run through the proxy, for why.
func notThroughTheProxy(t *testing.T, why string) {
	t.Helper()
	if proxied() {
		t.Skip("not run through the Docker proxy: " + why)
	}
}

// proxyHarness is a harness whose workspaces are direct children of the
// proxy's workspace root, as Menagerie's are, each with a name of its own.
type proxyHarness struct {
	*harness
	exceptions string
}

func newProxyHarness(t *testing.T) *proxyHarness {
	t.Helper()
	if !proxied() {
		t.Skip("runs through the Docker proxy only (task test:docker:proxy)")
	}
	root := os.Getenv("MENAGERIE_TEST_PROXY_ROOT")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	h := newHarnessAt(t, root, "proxy-"+hex.EncodeToString(raw))
	// The root is shared: cleanup takes only this test's workspaces.
	h.prefix = filepath.Join(root, h.runID) + "-"
	p := &proxyHarness{harness: h, exceptions: os.Getenv("MENAGERIE_TEST_PROXY_EXCEPTIONS")}
	t.Cleanup(func() { p.writeExceptions(nil) })
	return p
}

// workspace writes a workspace folder named after the test and name.
func (p *proxyHarness) workspace(name string, files map[string]string) string {
	p.t.Helper()
	folder := p.harness.workspace(p.runID+"-"+name, files)
	p.t.Cleanup(func() { _ = os.RemoveAll(folder) })
	return folder
}

// exception is one entry of the exceptions file.
type exception struct {
	Workspace string         `json:"workspace"`
	Grants    map[string]any `json:"grants"`
	Approval  map[string]any `json:"approval"`
}

// writeExceptions replaces the exceptions file as an operator does: a
// whole document renamed over the old one.
func (p *proxyHarness) writeExceptions(entries []exception) {
	p.t.Helper()
	for i := range entries {
		entries[i].Approval = map[string]any{"by": map[string]string{"issuer": "https://id.example.test", "subject": "user-0001"}, "at": "2026-01-01T00:00:00Z"}
	}
	if entries == nil {
		entries = []exception{}
	}
	body, err := json.Marshal(map[string]any{"version": 1, "exceptions": entries})
	if err != nil {
		p.t.Fatal(err)
	}
	tmp := filepath.Join(p.exceptions, ".workspace-exceptions.json.new")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		p.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(p.exceptions, "workspace-exceptions.json")); err != nil {
		p.t.Fatal(err)
	}
}

// startDenied starts folder and requires the job error a proxy denial
// gives: kind proxy-denied, naming the reason, which holds want.
func (p *proxyHarness) startDenied(folder, want string) {
	p.t.Helper()
	err := p.rt.Start(p.ctx, folder, &logProgress{t: p.t})
	var f *jobs.Failure
	if !errors.As(api.ContainerFailure(err), &f) || f.Kind != api.KindProxyDenied {
		p.t.Fatalf("start error %v, want a %s job error", err, api.KindProxyDenied)
	}
	d := dockerproxy.Denial(err)
	if d == nil {
		p.t.Fatalf("start error %v names no denial", err)
	}
	if !proxyPending("1822") && !strings.Contains(d.Reason, want) {
		p.t.Errorf("deny reason %q, want it to name %q", d.Reason, want)
	}
	if s := p.status(folder); s.State != devcontainer.StateNoContainer {
		p.t.Errorf("a denied start left a container: %+v", s)
	}
}

// startAfterReload starts folder until the proxy has loaded the new
// exceptions file: its watcher debounces a replace.
func (p *proxyHarness) startAfterReload(folder string) {
	p.t.Helper()
	var err error
	// Test-only bound on the proxy's reload; it reloaded within a second.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if err = p.rt.Start(p.ctx, folder, &logProgress{t: p.t}); dockerproxy.Denial(err) == nil {
			break
		}
	}
	if err != nil {
		p.t.Fatalf("start with the exception: %v", err)
	}
}

func proxyPending(change string) bool {
	return slices.Contains(strings.Fields(os.Getenv("MENAGERIE_TEST_PROXY_PENDING")), change)
}

// TestTheProxyDeniesPrivilegedUntilAnExceptionNamesIt is the proxy's
// privilege bound: a workspace that asks for a privileged container fails
// with a proxy-denied job error, and starts once the exceptions file
// grants it privileged.
func TestTheProxyDeniesPrivilegedUntilAnExceptionNamesIt(t *testing.T) {
	p := newProxyHarness(t)
	folder := p.workspace("privileged", map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "privileged": true}`})
	p.startDenied(folder, "privileged")
	waitsOn(t, "1571")
	p.writeExceptions([]exception{{Workspace: folder, Grants: map[string]any{"privileged": true}}})
	p.startAfterReload(folder)
	if info := p.inspect(p.status(folder).ContainerID); !info.HostConfig.Privileged {
		t.Error("the granted container is not privileged")
	}
}

// TestTheProxyDeniesABindOutsideTheRootUntilAnExceptionNamesIt binds a
// host folder outside every allowed root: denied, then admitted once the
// exceptions file lists it as an extra bind source.
func TestTheProxyDeniesABindOutsideTheRootUntilAnExceptionNamesIt(t *testing.T) {
	p := newProxyHarness(t)
	outside := filepath.Join("/menagerie-work", "outside-"+p.runID)
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	folder := p.workspace("outside", map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "mounts": ["source=` + outside + `,target=/outside,type=bind"]}`})
	p.startDenied(folder, outside)
	waitsOn(t, "1571")
	p.writeExceptions([]exception{{Workspace: folder, Grants: map[string]any{"extraBindSources": []string{outside}}}})
	p.startAfterReload(folder)
}

// TestTheProxyDeniesAnotherWorkspacesFolders binds another workspace's
// folder, and another workspace's socket folder: both are inside an
// allowed root, and both are denied, with no exception involved.
func TestTheProxyDeniesAnotherWorkspacesFolders(t *testing.T) {
	p := newProxyHarness(t)
	waitsOn(t, "1571")
	other := p.workspace("other", map[string]string{"marker": "other"})
	sockets := filepath.Join(filepath.Dir(p.root), "sockets", filepath.Base(other))
	if err := os.MkdirAll(sockets, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockets) })
	for name, source := range map[string]string{"folder": other, "socket": sockets} {
		folder := p.workspace("binds-other-"+name, map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "mounts": ["source=` + source + `,target=/other,type=bind"]}`})
		p.startDenied(folder, source)
	}
}

// TestTheProxyDeniesACreateNamingAnotherWorkspace sends a create for one
// workspace's devcontainer with the other's identity header: denied.
func TestTheProxyDeniesACreateNamingAnotherWorkspace(t *testing.T) {
	p := newProxyHarness(t)
	waitsOn(t, "1571")
	alpha := p.workspace("alpha", map[string]string{"marker": "alpha"})
	beta := p.workspace("beta", map[string]string{"marker": "beta"})
	client, err := dockerproxy.NewClient(map[string]string{dockerproxy.HeaderWorkspace: beta})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.ContainerCreate(context.Background(), &container.Config{Image: baseImage, Labels: map[string]string{"devcontainer.local_folder": alpha}},
		&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: alpha, Target: "/workspace"}}}, nil, nil, "")
	if dockerproxy.Denial(err) == nil {
		t.Fatalf("create for %s with %s's header: %v, want a denial", filepath.Base(alpha), filepath.Base(beta), err)
	}
}

// TestAForgedIdentityLabelIsRefusedBeforeTheProxy sets another
// workspace's devcontainer label in runArgs: Menagerie refuses it with
// identity-label-refused, and nothing is created.
func TestAForgedIdentityLabelIsRefusedBeforeTheProxy(t *testing.T) {
	p := newProxyHarness(t)
	other := p.workspace("forged-target", map[string]string{"marker": "other"})
	folder := p.workspace("forges", map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "runArgs": ["--label", "devcontainer.local_folder=` + other + `"]}`})
	err := p.rt.Start(p.ctx, folder, &logProgress{t: t})
	var f *jobs.Failure
	if !errors.As(api.ContainerFailure(err), &f) || f.Kind != api.KindIdentityLabelRefused || strings.Contains(err.Error(), other) {
		t.Fatalf("start error %v, want %s without the value", err, api.KindIdentityLabelRefused)
	}
	for _, w := range []string{folder, other} {
		if s := p.status(w); s.State != devcontainer.StateNoContainer {
			t.Errorf("%s has a container: %+v", filepath.Base(w), s)
		}
	}
}

// TestTheShippedProfileDeniesHostNetworkAndRestartPolicies asserts
// security settings of deploy/sockguard.yml that no other case reaches:
// the proxy ignores a key it does not know, so each must show its effect.
func TestTheShippedProfileDeniesHostNetworkAndRestartPolicies(t *testing.T) {
	p := newProxyHarness(t)
	for _, tc := range []struct{ name, args, want, waits string }{
		{"host-network", `["--network=host"]`, "host network", ""},
		{"host-pid", `["--pid=host"]`, "host PID", ""},
		{"restart", `["--restart=always"]`, "restart policy", "1571"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.waits != "" {
				waitsOn(t, tc.waits)
			}
			folder := p.workspace(tc.name, map[string]string{".devcontainer/devcontainer.json": fmt.Sprintf(`{"image": %q, "runArgs": %s}`, baseImage, tc.args)})
			p.startDenied(folder, tc.want)
		})
	}
}

// TestTheProxyDoesNotStartWithoutARoot starts a second proxy, as the
// suite's own runs, with a configuration that names a folder its mount
// namespace lacks: as an allowed bind root, and as the exceptions'
// workspace root. It exits rather than fall back to a lexical check.
func TestTheProxyDoesNotStartWithoutARoot(t *testing.T) {
	p := newProxyHarness(t)
	waitsOn(t, "missing-roots")
	running, err := p.client.ContainerInspect(p.ctx, "sockguard")
	if err != nil {
		t.Fatalf("the suite's proxy: %v", err)
	}
	config, err := os.ReadFile("/menagerie-work/proxy/config/sockguard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The control: the same proxy with the configuration as it is keeps
	// running, so an exit below is the missing root's.
	for name, edit := range map[string]func(string, string) string{
		"control": func(c, _ string) string { return c },
		"bind root": func(c, missing string) string {
			return strings.Replace(c, "allowed_bind_mounts:\n", "allowed_bind_mounts:\n            - "+missing+"\n", 1)
		},
		"workspace root": func(c, missing string) string {
			i := strings.Index(c, "  workspace_root: ")
			j := i + strings.Index(c[i:], "\n")
			return c[:i] + "  workspace_root: " + missing + c[j:]
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(os.TempDir(), p.runID+"-"+strings.ReplaceAll(name, " ", "-"))
			missing := filepath.Join(dir, "missing")
			if err := os.MkdirAll(filepath.Join(dir, "socket"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			edited := edit(string(config), missing)
			if name != "control" && (edited == string(config) || !strings.Contains(edited, missing)) {
				t.Fatalf("the %s was not replaced in the configuration", name)
			}
			if err := os.WriteFile(filepath.Join(dir, "sockguard.yaml"), []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			created, err := p.client.ContainerCreate(p.ctx,
				&container.Config{Image: running.Config.Image, User: "0:0", Labels: map[string]string{"menagerie.test.run": p.runID}},
				&container.HostConfig{
					GroupAdd: []string{"2375"}, CapDrop: []string{"ALL"}, CapAdd: []string{"DAC_READ_SEARCH"}, ReadonlyRootfs: true,
					Tmpfs: map[string]string{"/tmp": ""}, NetworkMode: "none", SecurityOpt: []string{"no-new-privileges"},
					Binds: []string{
						"/var/run/docker.sock:/var/run/docker.sock", "/menagerie-work:/menagerie-work:ro",
						filepath.Join(dir, "socket") + ":/run/sockguard",
						filepath.Join(dir, "sockguard.yaml") + ":/etc/sockguard/sockguard.yaml:ro",
						p.exceptions + ":/etc/sockguard/exceptions:ro",
					},
				}, nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			waited, waitErr := p.client.ContainerWait(p.ctx, created.ID, container.WaitConditionNextExit)
			if err := p.client.ContainerStart(p.ctx, created.ID, container.StartOptions{}); err != nil {
				t.Fatal(err)
			}
			// Test-only bound: the suite's proxy binds its socket within a
			// second, and a refused start exits as fast.
			select {
			case res := <-waited:
				if name == "control" || res.StatusCode == 0 {
					t.Errorf("the proxy exited %d with the %s configuration:\n%s", res.StatusCode, name, p.logsOf(created.ID))
				}
			case err := <-waitErr:
				t.Fatal(err)
			case <-time.After(20 * time.Second):
				if name != "control" {
					t.Errorf("the proxy runs with a missing %s", name)
				}
			}
		})
	}
}

// logsOf is the end of a container's log, for a failure message.
func (p *proxyHarness) logsOf(id string) string {
	rc, err := p.client.ContainerLogs(p.ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "40"})
	if err != nil {
		return err.Error()
	}
	defer rc.Close()
	var out strings.Builder
	_, _ = stdcopy.StdCopy(&out, &out, rc)
	return out.String()
}

// TestOnlyTheWorkspaceServicesGroupReachesTheProxy acts as Menagerie's
// services, a non-root uid the profile maps to Menagerie's: the workspace
// service, which has the socket's group, connects and gets the profile;
// the template service, the same uid without the group, cannot open the
// socket at all. The socket's mode and group are the profile's.
func TestOnlyTheWorkspaceServicesGroupReachesTheProxy(t *testing.T) {
	newProxyHarness(t)
	uid, err := strconv.Atoi(os.Getenv("MENAGERIE_TEST_PROXY_SERVICE_UID"))
	if err != nil {
		t.Fatal("MENAGERIE_TEST_PROXY_SERVICE_UID names no uid")
	}
	var cfg struct {
		Listeners []struct {
			Socket     string `yaml:"socket"`
			SocketMode string `yaml:"socket_mode"`
			SocketGID  int    `yaml:"socket_gid"`
		} `yaml:"listeners"`
	}
	raw, err := os.ReadFile("/menagerie-work/proxy/config/sockguard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil || len(cfg.Listeners) != 1 {
		t.Fatalf("the proxy's configuration: %v", err)
	}
	socket := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	mode, _ := strconv.ParseUint(cfg.Listeners[0].SocketMode, 8, 32)
	if info.Mode()&os.ModeSocket == 0 || uint64(info.Mode().Perm()) != mode || int(st.Gid) == uid || int(st.Gid) != cfg.Listeners[0].SocketGID {
		t.Fatalf("socket %s mode %v gid %d, want a socket of mode %s and group %d", socket, info.Mode(), st.Gid, cfg.Listeners[0].SocketMode, cfg.Listeners[0].SocketGID)
	}
	// ping asks the proxy for /_ping as uid with groups, through this test
	// binary's TestProxyPingHelper.
	ping := func(uid int, groups ...uint32) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProxyPingHelper$")
		cmd.Env = append(os.Environ(), "MENAGERIE_TEST_PING_SOCKET="+socket, "TMPDIR=/tmp")
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: groups}}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := ping(uid, st.Gid); err != nil || !strings.HasPrefix(out, "200 OK\n") {
		t.Errorf("the workspace service's ping: %q %v, want OK", out, err)
	}
	// The profile is the uid's: another uid with the group opens the
	// socket and gets none.
	if out, err := ping(uid+1, st.Gid); err != nil || !strings.HasPrefix(out, "403 ") || !strings.Contains(out, "client profile not allowed on this listener") {
		t.Errorf("another uid's ping: %q %v, want a denial", out, err)
	}
	if out, err := ping(uid); err != nil || !strings.HasPrefix(out, "dial: ") || !strings.Contains(out, "permission denied") {
		t.Errorf("the template service's ping: %q %v, want a permission denied open", out, err)
	}
}

// TestProxyPingHelper is the process TestOnlyTheWorkspaceServicesGroupReachesTheProxy
// runs as another user: it dials MENAGERIE_TEST_PING_SOCKET and prints
// "dial: <error>", or the status line and body of GET /_ping.
func TestProxyPingHelper(t *testing.T) {
	socket := os.Getenv("MENAGERIE_TEST_PING_SOCKET")
	if socket == "" {
		t.Skip("a helper process of TestOnlyTheWorkspaceServicesGroupReachesTheProxy")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		fmt.Printf("dial: %v\n", err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "GET /_ping HTTP/1.1\r\nHost: proxy\r\nConnection: close\r\n\r\n"); err != nil {
		fmt.Printf("write: %v\n", err)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		fmt.Printf("read: %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("%s\n%s\n", resp.Status, body)
}
