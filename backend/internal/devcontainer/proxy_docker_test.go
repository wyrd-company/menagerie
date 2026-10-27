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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// pendingChanges names what each pending fork change holds back.
var pendingChanges = map[string]string{
	"1571":      "per-workspace exceptions, roots, identity headers and the restart-policy setting",
	"1814":      "the LLBBridge passthrough that devcontainer builds need",
	"1815":      "exec.allow_any_command",
	"1819":      "label filters kept under ownership",
	"verbosity": "deny reasons in a profile's denials (response.deny_verbosity)",
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
	if !proxyPending("verbosity") && !strings.Contains(d.Reason, want) {
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
