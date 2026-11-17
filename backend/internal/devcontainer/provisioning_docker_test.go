//go:build docker

// The provisioning steps on a real Docker Engine, with the agent's report
// faked from what the Engine shows: the container exists and has never run
// when its sockets are reported, and the first start has them.
package devcontainer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/wyrwood/operator"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
	"github.com/wyrd-company/menagerie/backend/internal/xolotl"
)

// testData is Menagerie's data in a folder of the test's own.
func testData(t *testing.T) *store.Store {
	t.Helper()
	data, err := store.Open(filepath.Join(t.TempDir(), "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	return data
}
func testPolicyData(t *testing.T) *store.Store {
	t.Helper()
	data := testData(t)
	if _, err := data.SavePolicySettings(context.Background(), store.PolicySettings{Required: []string{"sample-policy"}}, []string{"sample-policy"}); err != nil {
		t.Fatal(err)
	}
	return data
}

// reportingAgent answers as Wyrwood does for one name binding: before the
// container exists it reports the directory, and then the container as the
// Engine shows it.
type reportingAgent struct {
	h    *harness
	name string
	mu   sync.Mutex
	// states are the states of the container each read of its status saw.
	states  []string
	bound   bool
	binds   int
	group   uint32
	binding operator.Binding
	// unbound lists each unbind as agent/name.
	unbound []string
}

func (a *reportingAgent) Bind(_ context.Context, name string, selection wyrwood.Selection) (operator.Binding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bound, a.group = true, selection.AccessGroup
	a.binds++
	group := selection.AccessGroup
	a.binding = operator.Binding{ID: "sample-binding", Agent: "sample-host", Key: operator.Key{Kind: operator.KeyName, Value: name}, Policies: selection.Policies, Settings: operator.Settings{AccessGroup: &group, Values: selection.Values}, Applied: true, Revision: 1}
	return a.binding, nil
}

func (a *reportingAgent) Unbind(_ context.Context, agent, name string) (operator.UnbindResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unbound = append(a.unbound, agent+"/"+name)
	removed := a.bound && name == a.name
	if removed {
		a.bound = false
	}
	return operator.UnbindResult{Removed: removed}, nil
}

// keyRegister issues and revokes Xolotl keys as Xolotl answers.
type keyRegister struct {
	mu   sync.Mutex
	live map[string]bool
	// revoked lists each revocation as policy/name.
	revoked []string
}

func (k *keyRegister) Issue(_ context.Context, policy, name string) (xolotl.Issued, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	created := !k.live[policy+"/"+name]
	k.live[policy+"/"+name] = true
	return xolotl.Issued{Created: created}, nil
}

func (k *keyRegister) Revoke(_ context.Context, policy, name string) (xolotl.Revocation, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.revoked = append(k.revoked, policy+"/"+name)
	if !k.live[policy+"/"+name] {
		return xolotl.Absent, nil
	}
	delete(k.live, policy+"/"+name)
	return xolotl.Revoked, nil
}

func (a *reportingAgent) Read(ctx context.Context, name string) (operator.Binding, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.bound || name != a.name {
		return operator.Binding{}, false, nil
	}
	group := a.group
	consumer := operator.ConsumerStatus{Directory: "/run/wyrwood/containers/" + name, AccessGroup: &group, Sockets: []string{"vault.sock", "ssh-agent.sock"}}
	status := operator.BindingStatus{AgentConnected: true, Readiness: operator.ReadinessWaiting}
	info, err := a.h.client.ContainerInspect(ctx, name)
	if err == nil {
		state := info.State.Status
		a.states = append(a.states, state)
		consumer.ContainerID = info.ID
		status.Readiness = operator.ReadinessReady
		status.Containers = []operator.BindingContainer{{ID: info.ID, Name: name, State: state}}
	}
	status.Consumers = []operator.ConsumerStatus{consumer}
	b := a.binding
	b.Status = status
	return b, true, nil
}

func (a *reportingAgent) Policies(context.Context) (operator.Grant, []operator.Policy, error) {
	return operator.Grant{}, []operator.Policy{{Name: "sample-policy"}}, nil
}
func (a *reportingAgent) Get(ctx context.Context, id string) (operator.Binding, error) {
	b, found, err := a.Read(ctx, a.name)
	if err == nil && (!found || b.ID != id) {
		err = operator.ErrNotFound
	}
	return b, err
}
func (a *reportingAgent) Change(ctx context.Context, b operator.Binding, s wyrwood.Selection) (operator.Binding, error) {
	return a.Bind(ctx, b.Key.Value, s)
}

func provisioned(name string) string {
	return `{
  "image": "` + baseImage + `",
  "overrideCommand": true,
  "runArgs": ["--name", "` + name + `"],
  "postCreateCommand": "test -d /proc && echo created >> /tmp/post-create-count",
  "customizations": {"menagerie": {"wyrwood": {"accessGroup": 1000, "policies": ["sample-policy"]}}}
}
`
}

func TestProvisionedStartCreatesThenWaitsThenStarts(t *testing.T) {
	h := newHarness(t)
	name := "menagerie-test-" + h.runID[len(h.runID)-12:]
	folder := h.workspace("provisioned", map[string]string{".devcontainer/devcontainer.json": provisioned(name)})
	agent := &reportingAgent{h: h, name: name}
	steps := provisioning.New(provisioning.Options{
		Clients: provisioning.Clients{Bindings: agent, Agent: "sample-host", Wait: 30 * time.Second},
		Runtime: provisioning.Adapt(h.rt),
		Data:    testPolicyData(t),
		Poll:    100 * time.Millisecond,
	})

	p := &logProgress{t: t}
	var issued error
	save := func(context.Context) error {
		if s := h.status(folder); s.State != devcontainer.StateNoContainer {
			issued = errors.New("a container existed at the save")
		}
		return nil
	}
	if err := steps.Create(h.ctx, folder, p, save); err != nil {
		t.Fatal(err)
	}
	if issued != nil {
		t.Fatal(issued)
	}
	for _, want := range []string{"read-definition", "bind", "create-container", "wait-for-sockets", "up", "verify"} {
		if !slices.Contains(p.phases, want) {
			t.Errorf("phases %v lack %s", p.phases, want)
		}
	}
	if len(agent.states) < 2 || agent.states[0] != "created" || agent.states[len(agent.states)-1] != "running" {
		t.Fatalf("the agent saw %v, want created first and running last", agent.states)
	}
	first := h.status(folder)
	if first.State != devcontainer.StateRunning {
		t.Fatalf("%+v", first)
	}
	if got := h.exec(first.ContainerID, "sh", "-c", "wc -l < /tmp/post-create-count"); got != "1" {
		t.Fatalf("postCreateCommand ran %s times", got)
	}

	// A start of the running container keeps it and waits for nothing.
	p = &logProgress{t: t}
	if err := steps.Start(h.ctx, folder, p); err != nil {
		t.Fatal(err)
	}
	if again := h.status(folder); again.ContainerID != first.ContainerID || again.State != devcontainer.StateRunning {
		t.Fatalf("%+v, want %+v", again, first)
	}
	if slices.Contains(p.phases, "wait-for-sockets") {
		t.Errorf("phases %v", p.phases)
	}

	// A stopped container is waited for, then started.
	if err := h.rt.Stop(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	p = &logProgress{t: t}
	if err := steps.Start(h.ctx, folder, p); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.phases, "wait-for-sockets") {
		t.Errorf("phases %v", p.phases)
	}

	// A rebuild has a new container, and the steps read its id.
	agent.states = nil
	if err := steps.Rebuild(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	rebuilt := h.status(folder)
	if rebuilt.ContainerID == first.ContainerID || rebuilt.State != devcontainer.StateRunning {
		t.Fatalf("%+v after %+v", rebuilt, first)
	}
	// The bind step can read the old container; the wait reads the new one
	// before it runs.
	created := slices.Index(agent.states, "created")
	if created < 0 || agent.states[len(agent.states)-1] != "running" {
		t.Fatalf("the agent saw %v", agent.states)
	}
}

// wyrd-company/menagerie#75: a workspace provisioned with a binding and a key,
// whose definition is then edited so that it does not parse, is deleted with
// both removed, because the delete reads what it holds from Menagerie's data.
func TestADeleteAfterTheDefinitionStopsParsingRemovesTheBindingAndTheKey(t *testing.T) {
	h := newHarness(t)
	name := "menagerie-test-" + h.runID[len(h.runID)-12:]
	keyed := strings.Replace(provisioned(name), `"policies": ["sample-policy"]}`, `"policies": ["sample-policy"]}, "xolotl": {"keyPolicy": "host-sample", "keyName": "unparsed"}`, 1)
	folder := h.workspace("unparsed", map[string]string{".devcontainer/devcontainer.json": keyed})
	agent := &reportingAgent{h: h, name: name}
	keys := &keyRegister{live: map[string]bool{}}
	data := testPolicyData(t)
	steps := provisioning.New(provisioning.Options{
		Clients: provisioning.Clients{Bindings: agent, Agent: "sample-host", Keys: keys, KeyPolicy: "host-sample", Wait: 30 * time.Second},
		Runtime: provisioning.Adapt(h.rt),
		Data:    data,
		Poll:    100 * time.Millisecond,
	})
	nothing := func(context.Context) error { return nil }
	if err := steps.Create(h.ctx, folder, &logProgress{t: t}, nothing); err != nil {
		t.Fatal(err)
	}
	if !agent.bound || !keys.live["host-sample/unparsed"] {
		t.Fatalf("the create left bound %t, keys %v", agent.bound, keys.live)
	}
	running := h.status(folder)

	h.write(folder, map[string]string{".devcontainer/devcontainer.json": "{ \"customizations\": "})
	if _, err := h.rt.Definition(folder); err == nil {
		t.Fatal("the edited definition parses")
	}
	if err := steps.Delete(h.ctx, folder, &logProgress{t: t}, nothing); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(agent.unbound, []string{"sample-host/" + name}) || agent.bound {
		t.Errorf("unbound %v, bound %t", agent.unbound, agent.bound)
	}
	if !slices.Equal(keys.revoked, []string{"host-sample/unparsed"}) || len(keys.live) != 0 {
		t.Errorf("revoked %v, live %v", keys.revoked, keys.live)
	}
	if _, err := h.client.ContainerInspect(h.ctx, running.ContainerID); err == nil {
		t.Error("the container stays")
	}
	if _, err := os.Lstat(folder); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the folder stays: %v", err)
	}
	if exists, err := data.Exists(h.ctx, "unparsed"); err != nil || exists {
		t.Errorf("the workspace's data stays: %t %v", exists, err)
	}
}

func TestAContainerNameAnotherContainerHoldsIsRefused(t *testing.T) {
	h := newHarness(t)
	name := "menagerie-test-" + h.runID[len(h.runID)-12:]
	folder := h.workspace("holding", map[string]string{".devcontainer/devcontainer.json": provisioned(name)})
	other := h.workspace("wanting", map[string]string{".devcontainer/devcontainer.json": provisioned(name)})
	if err := h.rt.Start(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	owner, found, err := h.rt.ContainerHolder(h.ctx, name)
	if err != nil || !found || owner != folder {
		t.Fatalf("holder %q %t %v", owner, found, err)
	}
	if _, found, err := h.rt.ContainerHolder(h.ctx, name+"-absent"); err != nil || found {
		t.Fatalf("absent: %t %v", found, err)
	}
	// The import reads the names of a workspace's containers, stopped ones
	// included, when its definition cannot be read.
	if err := h.rt.Stop(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	if names, err := h.rt.ContainerNames(h.ctx, folder); err != nil || !slices.Equal(names, []string{name}) {
		t.Fatalf("names %v %v", names, err)
	}
	if names, err := h.rt.ContainerNames(h.ctx, other); err != nil || len(names) != 0 {
		t.Fatalf("names of a workspace with no container %v %v", names, err)
	}
	// Menagerie's data holds nothing, so that only the container holds it.
	agent := &reportingAgent{h: h, name: name}
	steps := provisioning.New(provisioning.Options{
		Clients: provisioning.Clients{Bindings: agent, Agent: "sample-host", Wait: time.Second},
		Runtime: provisioning.Adapt(h.rt), Data: testPolicyData(t),
	})
	err = steps.Start(h.ctx, other, &logProgress{t: t})
	var failure *jobs.Failure
	if !errors.As(err, &failure) || failure.Kind != provisioning.KindInvalid || !strings.Contains(err.Error(), "the container of holding") {
		t.Fatalf("%v", err)
	}
	if agent.binds != 0 {
		t.Error("bound a taken name")
	}
}

// A rebuild that failed left the container under its old name, so a start
// replaces it and does not wait for a report that never comes.
func TestAStartReplacesAContainerWithAnOldName(t *testing.T) {
	h := newHarness(t)
	suffix := h.runID[len(h.runID)-12:]
	old, name := "menagerie-test-old-"+suffix, "menagerie-test-new-"+suffix
	folder := h.workspace("renamed", map[string]string{".devcontainer/devcontainer.json": provisioned(old)})
	if err := h.rt.Start(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	first := h.status(folder)
	h.write(folder, map[string]string{".devcontainer/devcontainer.json": provisioned(name)})
	steps := provisioning.New(provisioning.Options{
		Clients: provisioning.Clients{Bindings: &reportingAgent{h: h, name: name}, Agent: "sample-host", Wait: 30 * time.Second},
		Runtime: provisioning.Adapt(h.rt), Data: testPolicyData(t), Poll: 100 * time.Millisecond,
	})
	if err := steps.Start(h.ctx, folder, &logProgress{t: t}); err != nil {
		t.Fatal(err)
	}
	replaced := h.status(folder)
	if replaced.ContainerID == first.ContainerID || replaced.State != devcontainer.StateRunning {
		t.Fatalf("%+v after %+v", replaced, first)
	}
	if h.inspect(replaced.ContainerID).Name != "/"+name {
		t.Fatalf("name %s", h.inspect(replaced.ContainerID).Name)
	}
}

// The container's actual environment proves both the generated address and
// the rule that an existing container attaches it only on rebuild.
func TestXolotlKeyEnvironmentOnCreateStartAndRebuild(t *testing.T) {
	h := newHarness(t)
	folder := h.workspace("key-env", map[string]string{".devcontainer/devcontainer.json": `{"image":"` + baseImage + `"}`})
	p := &logProgress{t: t}
	run := func(rebuild bool, key string) {
		t.Helper()
		b, err := h.rt.BeginStart(folder, p)
		if rebuild {
			b, err = h.rt.BeginRebuild(folder, p)
		}
		if err != nil {
			t.Fatal(err)
		}
		b.SetXolotlKey(key)
		if err = b.Build(h.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = b.Create(h.ctx); err != nil {
			t.Fatal(err)
		}
		if err = b.Up(h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	value := func() string {
		return h.exec(h.status(folder).ContainerID, "sh", "-c", `printf '%s' "${MENAGERIE_XOLOTL_KEY-unset}"`)
	}
	run(false, "")
	first := h.status(folder).ContainerID
	if err := h.rt.Stop(h.ctx, folder, p); err != nil {
		t.Fatal(err)
	}
	run(false, "host-sample/key-env")
	if h.status(folder).ContainerID != first || value() != "unset" {
		t.Fatal("start replaced the old container or changed its environment")
	}
	run(true, "host-sample/key-env")
	if value() != "host-sample/key-env" {
		t.Fatal("rebuild did not attach the stored identity")
	}
	h.write(folder, map[string]string{".devcontainer/devcontainer.json": `{"image":"` + baseImage + `","containerEnv":{"MENAGERIE_XOLOTL_KEY":"explicit/name"}}`})
	run(true, "host-sample/key-env")
	if value() != "explicit/name" {
		t.Fatal("generated identity overrode containerEnv")
	}
}
