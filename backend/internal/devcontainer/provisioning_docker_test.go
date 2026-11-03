//go:build docker

// The provisioning steps on a real Docker Engine, with the agent's report
// faked from what the Engine shows: the container exists and has never run
// when its sockets are reported, and the first start has them.
package devcontainer_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/wyrwood/operator"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
)

// reportingAgent answers as Wyrwood does for one name binding: before the
// container exists it reports the directory, and then the container as the
// Engine shows it.
type reportingAgent struct {
	h    *harness
	name string
	mu   sync.Mutex
	// states are the states of the container each read of its status saw.
	states []string
	bound  bool
	binds  int
	group  uint32
}

func (a *reportingAgent) Bind(_ context.Context, _ string, selection wyrwood.Selection) (operator.Binding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bound, a.group = true, selection.AccessGroup
	a.binds++
	return operator.Binding{}, nil
}

func (a *reportingAgent) Unbind(context.Context, string) (operator.UnbindResult, error) {
	return operator.UnbindResult{}, errors.New("nothing unbinds in this test")
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
	return operator.Binding{Status: status}, true, nil
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
	root, err := workspace.NewRoot(h.root)
	if err != nil {
		t.Fatal(err)
	}
	steps := provisioning.New(provisioning.Options{
		Clients:    provisioning.Clients{Bindings: agent, Wait: 30 * time.Second},
		Runtime:    provisioning.Adapt(h.rt),
		Workspaces: root,
		Records:    provisioning.Records{Folder: t.TempDir()},
		Poll:       100 * time.Millisecond,
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
	// No root lists the other workspace, so that only the container holds it.
	agent := &reportingAgent{h: h, name: name}
	steps := provisioning.New(provisioning.Options{
		Clients: provisioning.Clients{Bindings: agent, Wait: time.Second},
		Runtime: provisioning.Adapt(h.rt), Records: provisioning.Records{Folder: t.TempDir()},
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
		Clients: provisioning.Clients{Bindings: &reportingAgent{h: h, name: name}, Wait: 30 * time.Second},
		Runtime: provisioning.Adapt(h.rt), Records: provisioning.Records{Folder: t.TempDir()}, Poll: 100 * time.Millisecond,
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
