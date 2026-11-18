package provisioning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/wyrwood/operator"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
	"github.com/wyrd-company/menagerie/backend/internal/xolotl"
)

// sampleAgent is the agent the sample host's names are bound on.
const sampleAgent = "sample-host"

// host is one sample host: a fake runtime, Wyrwood and Xolotl that write what
// each is asked to a shared list, so that a test reads the order across them.
type host struct {
	t *testing.T
	sync.Mutex
	calls       []string
	definitions map[string]devcontainer.Definition
	definedErr  error
	// state and container are the workspace's container before Create.
	state, container string
	// created is the id Create gives; rebuilds replace container with it.
	created string
	// containerName is the name of the workspace's container.
	containerName string
	holders       map[string]string
	failOn        map[string]error
	// bound is the binding of each name on sampleAgent, the agent Bind binds
	// on.
	bound map[string]wyrwood.Selection
	// elsewhere are the bindings on other agents, as agent/name.
	elsewhere map[string]bool
	// status gives a binding's status for each read; nil is a ready binding of
	// the container that Create gave.
	status func(name string) operator.BindingStatus
	// readErr fails a read.
	readErr error
	issued  map[string]bool
	// unbound lists each unbind as agent/name.
	unbound []string
	// data is Menagerie's data of the host.
	data       *store.Store
	logged     strings.Builder
	phases     []string
	xolotlKeys []string
	// mounting and mountingOutside are what the two mount checks report.
	mounting, mountingOutside []string
	// failAfterEmptied fails a removal after it has emptied the folder.
	failAfterEmptied error
	// onCheck runs in the host prerequisites check.
	onCheck func()
	// containerNames are the names of each folder's containers.
	containerNames map[string][]string
	// unreadable are the folders whose definition cannot be read.
	unreadable map[string]bool
}

func newHost(t *testing.T) *host {
	data, err := store.Open(filepath.Join(t.TempDir(), "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	ctx := context.Background()
	if _, err := data.SavePolicySettings(ctx, store.PolicySettings{Required: []string{"workspace", "client-key"}}, []string{"workspace", "client-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := data.Discover(ctx, []string{"sample"}); err != nil {
		t.Fatal(err)
	}
	return &host{
		t: t, state: devcontainer.StateNoContainer, created: "created-id", containerName: "container-sample",
		definitions: map[string]devcontainer.Definition{"/root/sample": definition(sampleBlock)},
		holders:     map[string]string{}, failOn: map[string]error{},
		bound: map[string]wyrwood.Selection{}, elsewhere: map[string]bool{}, issued: map[string]bool{}, data: data,
	}
}

// hold records in the data a name the workspace holds, bound on sampleAgent.
func (h *host) hold(workspace, name string, current bool) {
	h.t.Helper()
	if err := h.data.HoldName(context.Background(), workspace, name, sampleAgent, current); err != nil {
		h.t.Fatal(err)
	}
}

// holdKey records in the data a key the workspace holds.
func (h *host) holdKey(workspace, policy, name string) {
	h.t.Helper()
	if err := h.data.HoldKey(context.Background(), workspace, Key{Policy: policy, Name: name}); err != nil {
		h.t.Fatal(err)
	}
}

// provisioned records what a create of the sample gave it.
func (h *host) provisioned() {
	h.hold("sample", "container-sample", true)
	h.holdKey("sample", "host-sample", "sample")
}

// held is what the data holds for a workspace.
func (h *host) held(workspace string) store.Held {
	h.t.Helper()
	held, err := h.data.Held(context.Background(), workspace)
	if err != nil {
		h.t.Fatal(err)
	}
	return held
}

func (h *host) note(call string) error {
	h.Lock()
	defer h.Unlock()
	h.calls = append(h.calls, call)
	return h.failOn[call]
}

func (h *host) Phase(name string) {
	h.Lock()
	defer h.Unlock()
	h.phases = append(h.phases, name)
}
func (h *host) Log(string) io.Writer { return writerFunc(h.write) }
func (h *host) write(b []byte) (int, error) {
	h.Lock()
	defer h.Unlock()
	return h.logged.Write(b)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func (h *host) Status(context.Context, string) (devcontainer.Status, error) {
	h.Lock()
	defer h.Unlock()
	return devcontainer.Status{State: h.state, ContainerID: h.container}, h.failOn["status"]
}
func (h *host) Start(context.Context, string, devcontainer.Progress) error {
	return h.note("plain-start")
}
func (h *host) Stop(context.Context, string, devcontainer.Progress) error {
	if err := h.note("stop"); err != nil {
		return err
	}
	h.Lock()
	defer h.Unlock()
	if h.state == devcontainer.StateRunning {
		h.state = devcontainer.StateStopped
	}
	return nil
}
func (h *host) Rebuild(context.Context, string, devcontainer.Progress) error {
	h.Lock()
	h.containerName = ""
	h.Unlock()
	return h.note("plain-rebuild")
}
func (h *host) Mounting(context.Context, string) ([]string, error) { return h.mounting, nil }
func (h *host) MountingOutside(context.Context, string, devcontainer.Progress) ([]string, error) {
	return h.mountingOutside, nil
}
func (h *host) RemoveContainers(context.Context, string, devcontainer.Progress) error {
	return h.note("remove-containers")
}
func (h *host) RemoveFolder(_ context.Context, _ string, _ devcontainer.Progress, emptied func() error) error {
	if err := h.note("remove-folder"); err != nil {
		return err
	}
	if err := emptied(); err != nil {
		return err
	}
	return h.failAfterEmptied
}
func (h *host) CheckHostPrerequisites(context.Context, string, devcontainer.Progress) error {
	if h.onCheck != nil {
		h.onCheck()
	}
	return h.note("check")
}
func (h *host) Definition(folder string) (devcontainer.Definition, error) {
	h.Lock()
	defer h.Unlock()
	if h.unreadable[folder] {
		return devcontainer.Definition{}, errUnreadable
	}
	return h.definitions[folder], h.definedErr
}
func (h *host) ContainerHolder(_ context.Context, name string) (string, bool, error) {
	h.Lock()
	defer h.Unlock()
	if folder, found := h.holders[name]; found {
		return folder, true, h.failOn["holder"]
	}
	// The workspace's container has the name it declares, unless a test says
	// that it has another.
	if h.container != "" && name == h.containerName {
		return folder, true, h.failOn["holder"]
	}
	return "", false, h.failOn["holder"]
}
func (h *host) ContainerNames(_ context.Context, folder string) ([]string, error) {
	h.Lock()
	defer h.Unlock()
	return h.containerNames[folder], h.failOn["names"]
}
func (h *host) BeginStart(string, devcontainer.Progress) (BringUp, error) {
	return &fakeBringUp{h: h}, h.note("begin-start")
}
func (h *host) BeginRebuild(string, devcontainer.Progress) (BringUp, error) {
	return &fakeBringUp{h: h, rebuild: true}, h.note("begin-rebuild")
}

type fakeBringUp struct {
	h       *host
	rebuild bool
}

func (b *fakeBringUp) SetXolotlKey(key string) {
	b.h.xolotlKeys = append(b.h.xolotlKeys, key)
}

func (b *fakeBringUp) Build(context.Context) error { return b.h.note("build") }

// Create keeps a container that exists, unless the job rebuilds.
func (b *fakeBringUp) Create(context.Context) (string, error) {
	if err := b.h.note("create"); err != nil {
		return "", err
	}
	b.h.Lock()
	defer b.h.Unlock()
	if b.rebuild || b.h.container == "" {
		b.h.container, b.h.state, b.h.containerName = b.h.created, devcontainer.StateStopped, "container-sample"
	}
	return b.h.container, nil
}
func (b *fakeBringUp) Up(context.Context) error {
	if err := b.h.note("up"); err != nil {
		return err
	}
	b.h.Lock()
	defer b.h.Unlock()
	b.h.state = devcontainer.StateRunning
	return nil
}

func (h *host) Bind(_ context.Context, name string, selection wyrwood.Selection) (operator.Binding, error) {
	if err := h.note("bind " + name); err != nil {
		return operator.Binding{}, err
	}
	h.Lock()
	defer h.Unlock()
	if old, found := h.bound[name]; found && !reflect.DeepEqual(old, selection) {
		return operator.Binding{}, fmt.Errorf("wrapped: %w", operator.ErrBindingDiffers)
	}
	h.bound[name] = selection
	return operator.Binding{}, nil
}
func (h *host) Unbind(_ context.Context, agent, name string) (operator.UnbindResult, error) {
	if err := h.note("unbind " + name); err != nil {
		return operator.UnbindResult{}, err
	}
	h.Lock()
	defer h.Unlock()
	h.unbound = append(h.unbound, agent+"/"+name)
	// Wyrwood removes the binding of that name on that agent only, and
	// answers one that does not exist with success.
	if agent != sampleAgent {
		removed := h.elsewhere[agent+"/"+name]
		delete(h.elsewhere, agent+"/"+name)
		return operator.UnbindResult{Removed: removed}, nil
	}
	delete(h.bound, name)
	return operator.UnbindResult{Removed: true, Warning: "a part\nstayed", TokenLive: true}, nil
}
func (h *host) Read(_ context.Context, name string) (operator.Binding, bool, error) {
	h.Lock()
	defer h.Unlock()
	if h.readErr != nil {
		return operator.Binding{}, false, h.readErr
	}
	selection, found := h.bound[name]
	if !found {
		return operator.Binding{}, false, nil
	}
	group := selection.AccessGroup
	binding := operator.Binding{ID: name, Agent: sampleAgent, Key: operator.Key{Kind: operator.KeyName, Value: name}, Revision: 1, Applied: true, Policies: selection.Policies, Settings: operator.Settings{AccessGroup: &group, Values: selection.Values}}
	if h.status != nil {
		binding.Status = h.status(name)
		return binding, true, nil
	}
	binding.Status = h.readyStatus(group)
	return binding, true, nil
}

func (h *host) readyStatus(group uint32) operator.BindingStatus {
	state := "created"
	if h.state == devcontainer.StateRunning {
		state = "running"
	}
	return operator.BindingStatus{
		AgentConnected: true, Readiness: operator.ReadinessReady,
		Containers: []operator.BindingContainer{{ID: h.container, Name: "container-sample", State: state}},
		Consumers:  []operator.ConsumerStatus{{Directory: "/run/wyrwood/containers/container-sample", ContainerID: h.container, AccessGroup: &group, Sockets: []string{"vault.sock", "ssh-agent.sock"}}},
	}
}

func (h *host) Issue(_ context.Context, policy, name string) (xolotl.Issued, error) {
	key := policy + "/" + name
	if err := h.note("issue " + key); err != nil {
		return xolotl.Issued{}, err
	}
	h.Lock()
	defer h.Unlock()
	created := !h.issued[key]
	h.issued[key] = true
	return xolotl.Issued{Created: created}, nil
}
func (h *host) Revoke(_ context.Context, policy, name string) (xolotl.Revocation, error) {
	return "revoked", h.note("revoke " + policy + "/" + name)
}

func (h *host) steps() *Steps {
	return h.with(Clients{Bindings: h, Agent: sampleAgent, Keys: h, Wait: 200 * time.Millisecond})
}

// with is the host's steps with clients.
func (h *host) with(clients Clients) *Steps {
	if clients.Keys != nil && clients.KeyPolicy == "" {
		clients.KeyPolicy = "host-sample"
	}
	return New(Options{Clients: clients, Runtime: h, Data: h.data, Poll: time.Millisecond})
}

func (h *host) seen() []string {
	h.Lock()
	defer h.Unlock()
	return slices.Clone(h.calls)
}

func (h *host) wantCalls(t *testing.T, want ...string) {
	t.Helper()
	if got := h.seen(); !slices.Equal(got, want) {
		t.Fatalf("calls\n got %v\nwant %v", got, want)
	}
}

func wantKind(t *testing.T, err error, kind string) {
	t.Helper()
	var failure *jobs.Failure
	if !errors.As(err, &failure) || failure.Kind != kind {
		t.Fatalf("error %v, want kind %s", err, kind)
	}
}

const folder = "/root/sample"

func TestACreateRunsEveryStepInOrder(t *testing.T) {
	h := newHost(t)
	save := func(context.Context) error { return h.note("save") }
	if err := h.steps().Create(context.Background(), folder, h, save); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-start", "check", "issue host-sample/sample", "bind container-sample", "save", "build", "create", "up")
	want := []string{"read-definition", "issue-key", "bind", "wait-for-sockets", "verify"}
	if got := h.phases; !slices.Equal(got, want) {
		t.Fatalf("phases %v, want %v", got, want)
	}
	if selection := h.bound["container-sample"]; selection.AccessGroup != 1000 || !slices.Equal(selection.Policies, []string{"client-key", "workspace"}) || selection.Values["key_name"] != "sample" {
		t.Fatalf("selection %+v", selection)
	}
}

// A repeat neither rotates the key nor rewrites the binding, because each
// client treats an equal repeat as done.
func TestARetriedJobContinuesFromWhatIsMissing(t *testing.T) {
	h := newHost(t)
	h.failOn["up"] = errors.New("a lifecycle command failed")
	s := h.steps()
	if err := s.Start(context.Background(), folder, h); err == nil {
		t.Fatal("the first start passed")
	}
	delete(h.failOn, "up")
	if err := s.Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logged.String(), "already exists and was left as it is") {
		t.Errorf("console %s", h.logged.String())
	}
	if strings.Contains(strings.Join(h.seen(), ","), "unbind") {
		t.Errorf("a repeat replaced the binding: %v", h.seen())
	}
}

func TestWithoutADeclarationEachJobRunsAsTheRuntimeDoes(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = devcontainer.Definition{ContainerName: "container-sample"}
	s := h.steps()
	ctx := context.Background()
	if err := s.Start(ctx, folder, h); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(ctx, folder, h); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, folder, h, func(context.Context) error { return h.note("save") }); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "plain-start", "plain-rebuild", "check", "save", "plain-start")
	if len(h.phases) != 0 {
		t.Errorf("phases %v", h.phases)
	}
}

func TestADefinitionThatCannotBeReadIsReportedByTheStart(t *testing.T) {
	h := newHost(t)
	h.definedErr = errors.New("unreadable")
	h.failOn["plain-start"] = errors.New("the start reports it")
	err := h.steps().Start(context.Background(), folder, h)
	if err == nil || err.Error() != "the start reports it" {
		t.Fatalf("%v", err)
	}
}

func TestAnInvalidBlockFailsBeforeAnyStep(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = definition(`{"wyrwood":{"accessGroup":1,"policies":["a"]},"other":1}`)
	wantKind(t, h.steps().Start(context.Background(), folder, h), KindInvalid)
	h.wantCalls(t)
	if !slices.Equal(h.phases, []string{"read-definition"}) {
		t.Errorf("phases %v", h.phases)
	}
}

func TestAMissingSectionFailsBeforeAnyWrite(t *testing.T) {
	for _, name := range []string{"wyrwood", "xolotl"} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			clients := Clients{Keys: h, KeyPolicy: "host-sample"}
			if name == "xolotl" {
				clients = Clients{Bindings: h}
				if _, err := h.data.Identity(context.Background(), "sample", nil, &Key{Policy: "host-sample", Name: "sample"}); err != nil {
					t.Fatal(err)
				}
			}
			err := h.with(clients).Start(context.Background(), folder, h)
			wantKind(t, err, KindUnavailable)
			if !strings.Contains(err.Error(), "provisioning."+name) {
				t.Errorf("message %v", err)
			}
			h.wantCalls(t)
		})
	}
}

func TestANameOrKeyAnotherWorkspaceHoldsIsRefused(t *testing.T) {
	other := "/root/other"
	for name, tc := range map[string]struct {
		prepare func(h *host)
		want    string
	}{
		"a name the data holds for another workspace": {func(h *host) {
			h.hold("other", "container-sample", false)
		}, "Menagerie's data holds it for workspace other"},
		"a key the data holds for another workspace": {func(h *host) {
			h.holdKey("other", "host-sample", "sample")
		}, "Xolotl key host-sample/sample is already taken: Menagerie's data holds it for workspace other"},
		"another workspace's container": {func(h *host) {
			h.holders["container-sample"] = other
		}, "the container of other has that name"},
		"a container of no workspace": {func(h *host) {
			h.holders["container-sample"] = ""
		}, "belongs to no workspace"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			tc.prepare(h)
			err := h.steps().Start(context.Background(), folder, h)
			wantKind(t, err, KindInvalid)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %v, want %q", err, tc.want)
			}
			h.wantCalls(t)
			if held := h.held("sample"); !held.Empty() {
				t.Errorf("the refused workspace holds %+v", held)
			}
		})
	}
}

// Only Menagerie's data and the containers decide who holds a name: another
// workspace's definition that declares it holds nothing until its job records
// it, and that job is then refused.
func TestADeclarationAloneTakesNoName(t *testing.T) {
	h := newHost(t)
	other := definition(sampleBlock)
	h.definitions["/root/other"] = other
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	err := h.steps().Start(context.Background(), "/root/other", h)
	wantKind(t, err, KindInvalid)
	if !strings.Contains(err.Error(), "Menagerie's data holds it for workspace sample") {
		t.Fatalf("message %v", err)
	}
	h.wantCalls(t)
}

// A job that took the name or the key meanwhile is refused by the constraint,
// as read-definition refuses it, and nothing is asked of Wyrwood or Xolotl.
func TestANameOrKeyTakenMeanwhileIsRefusedBeforeItsRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		take     func(h *host)
		notAsked string
	}{
		"key":  {func(h *host) { h.holdKey("other", "host-sample", "sample") }, "issue "},
		"name": {func(h *host) { h.hold("other", "container-sample", true) }, "bind "},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			// The other job records it between this job's check and its write.
			h.onCheck = func() { tc.take(h) }
			err := h.steps().Start(context.Background(), folder, h)
			wantKind(t, err, KindInvalid)
			if !strings.Contains(err.Error(), "workspace other") {
				t.Errorf("message %v", err)
			}
			for _, call := range h.seen() {
				if strings.HasPrefix(call, tc.notAsked) {
					t.Errorf("asked %s", call)
				}
			}
		})
	}
}

func TestItsOwnContainerAndDataDoNotTakeItsName(t *testing.T) {
	h := newHost(t)
	h.holders["container-sample"] = folder
	h.provisioned()
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
}

// The data records the key before Xolotl is asked and the name before Wyrwood
// is, so what a request made is known whatever happens to the job.
func TestAJobRecordsWhatItProvisionsBeforeItsRequests(t *testing.T) {
	h := newHost(t)
	h.failOn["issue host-sample/sample"] = errors.New("the answer was lost")
	if err := h.steps().Start(context.Background(), folder, h); err == nil {
		t.Fatal("passed")
	}
	if held := h.held("sample"); !slices.Equal(held.Keys, []Key{{Policy: "host-sample", Name: "sample"}}) || len(held.Names) != 0 {
		t.Fatalf("held %+v", held)
	}
	delete(h.failOn, "issue host-sample/sample")
	h.failOn["bind container-sample"] = errors.New("the answer was lost")
	if err := h.steps().Start(context.Background(), folder, h); err == nil {
		t.Fatal("passed")
	}
	want := []store.Name{{Name: "container-sample", Agent: sampleAgent, Current: true}}
	if held := h.held("sample"); !slices.Equal(held.Names, want) {
		t.Fatalf("held %+v", held)
	}
	delete(h.failOn, "bind container-sample")
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	want[0].BindingID = "container-sample"
	if held := h.held("sample"); !slices.Equal(held.Names, want) || len(held.Keys) != 1 {
		t.Fatalf("held %+v", held)
	}
}

func TestMissingHostPrerequisitesIssueNothing(t *testing.T) {
	h := newHost(t)
	h.failOn["check"] = &devcontainer.HostPrerequisitesError{Networks: []string{"absent"}}
	var missing *devcontainer.HostPrerequisitesError
	if err := h.steps().Start(context.Background(), folder, h); !errors.As(err, &missing) {
		t.Fatalf("%v", err)
	}
	h.wantCalls(t, "begin-start", "check")
}

func TestABlockWithoutXolotlIssuesNoKey(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = definition(`{"wyrwood":{"accessGroup":1000,"policies":["a"]}}`)
	if err := h.with(Clients{Bindings: h, Agent: sampleAgent, Wait: time.Second}).Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-start", "check", "bind container-sample", "build", "create", "up")
}

func TestAStartOfAnExistingContainerContinuesOnlyWithoutXolotl(t *testing.T) {
	unreachable := &xolotl.Error{Operation: "issue", Reason: "no answer", Unreachable: true}
	refused := &xolotl.Error{Operation: "issue", Reason: "refused"}
	for name, tc := range map[string]struct {
		state, container string
		rebuild          bool
		issue            error
		passes           bool
	}{
		"existing, out of reach":  {devcontainer.StateStopped, "old-id", false, unreachable, true},
		"existing, refused":       {devcontainer.StateStopped, "old-id", false, refused, false},
		"no container, no Xolotl": {devcontainer.StateNoContainer, "", false, unreachable, false},
		"a rebuild, out of reach": {devcontainer.StateStopped, "old-id", true, unreachable, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.state, h.container = tc.state, tc.container
			h.failOn["issue host-sample/sample"] = tc.issue
			s := h.steps()
			var err error
			if tc.rebuild {
				err = s.Rebuild(context.Background(), folder, h)
			} else {
				err = s.Start(context.Background(), folder, h)
			}
			if (err == nil) != tc.passes {
				t.Fatalf("error %v", err)
			}
			if tc.passes && !strings.Contains(h.logged.String(), "Xolotl cannot be reached") {
				t.Errorf("no console note: %s", h.logged.String())
			}
		})
	}
}

func TestACreateNeverContinuesWithoutXolotl(t *testing.T) {
	h := newHost(t)
	h.state, h.container = devcontainer.StateStopped, "old-id"
	h.failOn["issue host-sample/sample"] = &xolotl.Error{Operation: "issue", Unreachable: true}
	if err := h.steps().Create(context.Background(), folder, h, func(context.Context) error { return nil }); err == nil {
		t.Fatal("the create passed")
	}
}

func TestAnExistingKeysStateReachesTheConsole(t *testing.T) {
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	keys := issuedKeys{Issued: xolotl.Issued{Disabled: true, ExpiresAt: &expires}}
	h := newHost(t)
	if err := h.with(Clients{Bindings: h, Agent: sampleAgent, Keys: keys, Wait: time.Second}).Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"host-sample/sample is disabled", "host-sample/sample expires at 2030-01-02T03:04:05Z"} {
		if !strings.Contains(h.logged.String(), want) {
			t.Errorf("console lacks %q: %s", want, h.logged.String())
		}
	}
}

type issuedKeys struct{ xolotl.Issued }

func (k issuedKeys) Issue(context.Context, string, string) (xolotl.Issued, error) {
	return k.Issued, nil
}
func (k issuedKeys) Revoke(context.Context, string, string) (xolotl.Revocation, error) {
	return "", nil
}

func TestABindingThatDiffersChangesWithoutStoppingTheContainer(t *testing.T) {
	h := newHost(t)
	h.state, h.container = devcontainer.StateRunning, "old-id"
	h.bound["container-sample"] = wyrwood.Selection{Policies: []string{"old"}, AccessGroup: 7, Values: map[string]string{"key_name": "sample"}}
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-start", "check", "issue host-sample/sample", "change container-sample", "build", "create", "up")
	if len(h.unbound) != 0 {
		t.Fatal("binding unbound")
	}
	for _, want := range []string{"adding policy workspace", "removing policy old", "setting access group to 1000"} {
		if !strings.Contains(h.logged.String(), want) {
			t.Fatalf("console lacks %s: %s", want, h.logged.String())
		}
	}
}

func TestACreateThatFailsAtItsSaveStartsNothing(t *testing.T) {
	h := newHost(t)
	saved := errors.New("the backup failed")
	err := h.steps().Create(context.Background(), folder, h, func(context.Context) error { return saved })
	if !errors.Is(err, saved) {
		t.Fatalf("%v", err)
	}
	h.wantCalls(t, "begin-start", "check", "issue host-sample/sample", "bind container-sample")
}

// A status with a directory and no container is what the agent reports before
// the container exists, so the bind step passes on it and the wait does not.
func TestTheWaitsReadWhatEachStepNeeds(t *testing.T) {
	h := newHost(t)
	group := uint32(1000)
	var phase string
	h.status = func(string) operator.BindingStatus {
		phase = h.phases[len(h.phases)-1]
		if phase == "bind" {
			return operator.BindingStatus{Readiness: operator.ReadinessFailed, Reasons: []string{"conformance"}, Outcome: "recreate-failed",
				Consumers: []operator.ConsumerStatus{{Directory: "/run/wyrwood/containers/container-sample", Sockets: []string{"vault.sock", "ssh-agent.sock"}, AccessGroup: &group}}}
		}
		return h.readyStatus(group)
	}
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
}

func TestABindWhoseDirectoryNeverComesIsNotReady(t *testing.T) {
	h := newHost(t)
	h.status = func(string) operator.BindingStatus {
		return operator.BindingStatus{Readiness: operator.ReadinessUnknown, Reasons: []string{"agent-disconnected"}}
	}
	err := h.steps().Start(context.Background(), folder, h)
	wantKind(t, err, KindNotReady)
	if !strings.Contains(err.Error(), "the agent is not connected") {
		t.Errorf("message %v", err)
	}
	h.wantCalls(t, "begin-start", "check", "issue host-sample/sample", "bind container-sample")
}

// waitingStatus has the directory of a bound name, so that only the wait for
// sockets looks at the rest.
func waitingStatus(mutate func(*operator.BindingStatus, *uint32), h *host) func(string) operator.BindingStatus {
	return func(string) operator.BindingStatus {
		group := uint32(1000)
		status := h.readyStatus(group)
		if !slices.Contains(h.phases, "wait-for-sockets") {
			return status
		}
		mutate(&status, &group)
		return status
	}
}

func TestTheSocketWaitRefusesWhatIsNotReady(t *testing.T) {
	wrong := uint32(2000)
	for name, tc := range map[string]struct {
		mutate func(*operator.BindingStatus, *uint32)
		want   string
	}{
		"the old container's sockets": {func(s *operator.BindingStatus, _ *uint32) {
			s.Containers[0].ID, s.Consumers[0].ContainerID = "old-id", "old-id"
		}, "did not report its sockets"},
		"no container reported": {func(s *operator.BindingStatus, _ *uint32) { s.Containers = nil }, "did not report its sockets"},
		"a socket missing":      {func(s *operator.BindingStatus, _ *uint32) { s.Consumers[0].Sockets = []string{"vault.sock"} }, "did not report its sockets"},
		"the other socket":      {func(s *operator.BindingStatus, _ *uint32) { s.Consumers[0].Sockets = []string{"ssh-agent.sock"} }, "did not report its sockets"},
		"no access group":       {func(s *operator.BindingStatus, _ *uint32) { s.Consumers[0].AccessGroup = nil }, "did not report its sockets"},
		"waiting": {func(s *operator.BindingStatus, _ *uint32) {
			s.Readiness, s.LastError = operator.ReadinessWaiting, "permission denied\nby policy"
		}, "last error permission denied by policy"},
		"a wrong access group": {func(s *operator.BindingStatus, _ *uint32) { s.Consumers[0].AccessGroup = &wrong }, "has access group 2000, and the definition declares 1000"},
		"a failed consumer":    {func(s *operator.BindingStatus, _ *uint32) { s.Consumers[0].Failed = "access-group" }, "the agent reports access-group"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.status = waitingStatus(tc.mutate, h)
			err := h.steps().Start(context.Background(), folder, h)
			wantKind(t, err, KindNotReady)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %v, want %q", err, tc.want)
			}
			if slices.Contains(h.seen(), "up") {
				t.Errorf("started without sockets: %v", h.seen())
			}
		})
	}
}

func TestTheWaitPassesWhenTheStatusArrives(t *testing.T) {
	h := newHost(t)
	reads := 0
	h.status = waitingStatus(func(s *operator.BindingStatus, _ *uint32) {
		reads++
		if reads < 3 {
			s.Readiness, s.LastError = operator.ReadinessWaiting, fmt.Sprintf("denied %d", reads)
		}
	}, h)
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	// Each change of the status reaches the console while the job waits.
	for _, want := range []string{"denied 1", "denied 2"} {
		if !strings.Contains(h.logged.String(), want) {
			t.Errorf("console lacks %q: %s", want, h.logged.String())
		}
	}
}

func TestARunningContainerIsNotWaitedFor(t *testing.T) {
	h := newHost(t)
	h.state, h.container = devcontainer.StateRunning, "running-id"
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(h.phases, "wait-for-sockets") {
		t.Errorf("phases %v", h.phases)
	}
}

func TestARebuildWaitsOnItsNewContainer(t *testing.T) {
	h := newHost(t)
	h.state, h.container = devcontainer.StateRunning, "old-id"
	if err := h.steps().Rebuild(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-rebuild", "check", "issue host-sample/sample", "bind container-sample", "build", "create", "up")
	if !slices.Contains(h.phases, "wait-for-sockets") {
		t.Errorf("phases %v", h.phases)
	}
	if h.container != "created-id" {
		t.Errorf("container %s", h.container)
	}
}

func TestVerifyFailsAContainerThatDoesNotRun(t *testing.T) {
	h := newHost(t)
	group := uint32(1000)
	h.status = func(string) operator.BindingStatus {
		status := h.readyStatus(group)
		status.Containers[0].State = "exited"
		return status
	}
	err := h.steps().Start(context.Background(), folder, h)
	wantKind(t, err, KindNotReady)
	if !strings.Contains(err.Error(), "its running container and sockets") || !strings.Contains(err.Error(), "the devcontainer started") {
		t.Errorf("message %v", err)
	}
	var after *AfterStartError
	if !errors.As(err, &after) {
		t.Errorf("%T", err)
	}
	if h.phases[len(h.phases)-1] != "verify" {
		t.Errorf("phases %v", h.phases)
	}
}

func TestAReadThatIsUnreachableIsWaitedOutAndAnyOtherFails(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	h.bound["container-sample"] = wyrwood.Selection{Policies: []string{"workspace", "client-key"}, AccessGroup: 1000, Values: map[string]string{"key_name": "sample"}}
	d, err := Parse(h.definitions[folder])
	if err != nil {
		t.Fatal(err)
	}
	h.readErr = fmt.Errorf("wrapped: %w", operator.ErrUnreachable)
	err = s.await(context.Background(), d, h, "its sockets", func(operator.Binding) (bool, string) { return false, "" })
	wantKind(t, err, KindNotReady)
	if !strings.Contains(err.Error(), "the controller cannot be reached") {
		t.Errorf("message %v", err)
	}
	h.readErr = fmt.Errorf("wrapped: %w", operator.ErrOutsideGrant)
	err = s.await(context.Background(), d, h, "its sockets", func(operator.Binding) (bool, string) { return false, "" })
	if !errors.Is(err, operator.ErrOutsideGrant) {
		t.Fatalf("%v", err)
	}
}

func TestABindingThatReadsAsAbsentFailsAtOnce(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	s.opts.Wait = time.Hour
	h.status = func(name string) operator.BindingStatus {
		delete(h.bound, name)
		return operator.BindingStatus{}
	}
	wantKind(t, s.Start(context.Background(), folder, h), KindNotReady)
}

func TestAJobThatEndsWhileItWaitsEndsAtOnce(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	s.opts.Wait = time.Hour
	h.status = func(string) operator.BindingStatus {
		return operator.BindingStatus{Readiness: operator.ReadinessWaiting}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Start(ctx, folder, h); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestAStartedJobRetiresWhatTheDefinitionNoLongerDeclaresLast(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	h.hold("sample", "container-old", false)
	h.holdKey("sample", "host-sample", "old")
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-start", "check", "issue host-sample/sample", "bind container-sample", "build", "create", "up", "unbind container-old", "revoke host-sample/old")
	if h.phases[len(h.phases)-1] != "retire" {
		t.Errorf("phases %v", h.phases)
	}
	// The unbind names the agent the data recorded.
	if !slices.Equal(h.unbound, []string{sampleAgent + "/container-old"}) {
		t.Errorf("unbound %v", h.unbound)
	}
	held := h.held("sample")
	if !slices.Equal(held.Names, []store.Name{{Name: "container-sample", Agent: sampleAgent, Current: true, BindingID: "container-sample"}}) || !slices.Equal(held.Keys, []Key{{Policy: "host-sample", Name: "sample"}}) {
		t.Fatalf("held %+v", held)
	}
}

// provisioning-retries: a declaration of another name binds it, makes it
// current, and retires the old one at the job's end.
func TestADeclarationOfAnotherNameBindsItAndRetiresTheOldOne(t *testing.T) {
	for name, run := range map[string]func(*Steps, *host) error{
		"start":   func(s *Steps, h *host) error { return s.Start(context.Background(), folder, h) },
		"rebuild": func(s *Steps, h *host) error { return s.Rebuild(context.Background(), folder, h) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.hold("sample", "container-old", true)
			h.holdKey("sample", "host-sample", "sample")
			if err := run(h.steps(), h); err != nil {
				t.Fatal(err)
			}
			calls := h.seen()
			bind, unbind := slices.Index(calls, "bind container-sample"), slices.Index(calls, "unbind container-old")
			if bind < 0 || unbind < bind || unbind != len(calls)-1 {
				t.Fatalf("calls %v", calls)
			}
			held := h.held("sample")
			if held.Current() != "container-sample" || len(held.Names) != 1 || len(held.Keys) != 1 {
				t.Fatalf("held %+v", held)
			}
		})
	}
}

// provisioning-retries: no declaration runs no provisioning step and retires
// every name and the key at the end.
func TestNoDeclarationRetiresEveryNameAndTheKey(t *testing.T) {
	for name, run := range map[string]func(*Steps, *host) error{
		"start":   func(s *Steps, h *host) error { return s.Start(context.Background(), folder, h) },
		"rebuild": func(s *Steps, h *host) error { return s.Rebuild(context.Background(), folder, h) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.definitions[folder] = devcontainer.Definition{ContainerName: "container-sample"}
			h.provisioned()
			h.hold("sample", "container-old", false)
			if err := run(h.steps(), h); err != nil {
				t.Fatal(err)
			}
			h.wantCalls(t, "plain-"+name, "unbind container-old", "unbind container-sample", "revoke host-sample/sample")
			if held := h.held("sample"); !held.Empty() {
				t.Fatalf("held %+v", held)
			}
			if !slices.Equal(h.phases, []string{"retire"}) {
				t.Errorf("phases %v", h.phases)
			}
		})
	}
}

// provisioning-retries: a declaration the reader refuses fails in
// read-definition, leaving the binding, the current name and the container as
// they are.
func TestADeclarationTheReaderRefusesLeavesTheBinding(t *testing.T) {
	for name, run := range map[string]func(*Steps, *host) error{
		"start":   func(s *Steps, h *host) error { return s.Start(context.Background(), folder, h) },
		"rebuild": func(s *Steps, h *host) error { return s.Rebuild(context.Background(), folder, h) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.state, h.container = devcontainer.StateRunning, "old-id"
			h.definitions[folder] = definition(`{"wyrwood":{"accessGroup":"not a number","policies":["a"]}}`)
			h.provisioned()
			before := h.held("sample")
			wantKind(t, run(h.steps(), h), KindInvalid)
			h.wantCalls(t)
			if !slices.Equal(h.phases, []string{"read-definition"}) {
				t.Errorf("phases %v", h.phases)
			}
			if held := h.held("sample"); !reflect.DeepEqual(held, before) {
				t.Fatalf("held %+v, before %+v", held, before)
			}
			if h.container != "old-id" || h.state != devcontainer.StateRunning {
				t.Fatalf("container %s %s", h.container, h.state)
			}
		})
	}
}

// A name this workspace's data holds is its own to retire, even when another
// workspace's definition declares it: that workspace's job is refused the name
// until it is gone.
func TestAnotherDeclarationDoesNotKeepARetiredName(t *testing.T) {
	h := newHost(t)
	h.definitions["/root/other"] = func() devcontainer.Definition {
		d := definition(`{"wyrwood":{"accessGroup":1,"policies":["a"]}}`)
		d.ContainerName = "container-old"
		return d
	}()
	h.provisioned()
	h.hold("sample", "container-old", false)
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.seen(), "unbind container-old") {
		t.Errorf("calls %v", h.seen())
	}
}

func TestAFailureWhileRetiringFailsTheJobAndKeepsTheRest(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	h.hold("sample", "container-a", false)
	h.hold("sample", "container-b", false)
	h.holdKey("sample", "host-sample", "old")
	h.failOn["unbind container-b"] = errors.New("refused")
	err := h.steps().Start(context.Background(), folder, h)
	var after *AfterStartError
	if !errors.As(err, &after) || !strings.Contains(err.Error(), "the devcontainer started") {
		t.Fatalf("%v", err)
	}
	if held := h.held("sample"); len(held.Names) != 2 || held.Names[0].Name != "container-b" || len(held.Keys) != 2 {
		t.Fatalf("held %+v", held)
	}
	delete(h.failOn, "unbind container-b")
	h.failOn["revoke host-sample/old"] = errors.New("refused")
	if err := h.steps().Start(context.Background(), folder, h); err == nil {
		t.Fatal("the second start passed")
	}
	if held := h.held("sample"); len(held.Names) != 1 || len(held.Keys) != 2 {
		t.Fatalf("held %+v", held)
	}
	delete(h.failOn, "revoke host-sample/old")
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	if held := h.held("sample"); len(held.Names) != 1 || len(held.Keys) != 1 {
		t.Fatalf("held %+v", held)
	}
}

// A binding the grant hides is not taken for a removed one: it stays in the
// data and fails the job, naming it, with the devcontainer running.
func TestABindingOutsideTheGrantStaysAndFailsTheRetire(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	h.hold("sample", "container-hidden", false)
	h.failOn["unbind container-hidden"] = fmt.Errorf("wrapped: %w", operator.ErrOutsideGrant)
	err := h.steps().Start(context.Background(), folder, h)
	wantKind(t, err, KindBindingOutsideGrant)
	var after *AfterStartError
	if !errors.As(err, &after) || !strings.Contains(err.Error(), "container-hidden") {
		t.Fatalf("%v", err)
	}
	if held := h.held("sample"); !slices.ContainsFunc(held.Names, func(n store.Name) bool { return n.Name == "container-hidden" }) {
		t.Fatalf("held %+v", held)
	}
}

// A name bound on the agent an earlier provisioning.wyrwood.agent named keeps
// that agent in the data until Wyrwood confirms its binding gone: a start on a
// new agent removes it first, so a bind there that fails loses nothing, and a
// delete afterwards leaves no binding on either agent (review round 1 of
// wyrd-company/menagerie#107).
func TestAChangedAgentNeverLosesTheRecordedBinding(t *testing.T) {
	const original = "original-host"
	ctx := context.Background()
	provisionedOnOriginal := func(t *testing.T) *host {
		h := newHost(t)
		if err := h.data.HoldName(ctx, "sample", "container-sample", original, true); err != nil {
			t.Fatal(err)
		}
		h.holdKey("sample", "host-sample", "sample")
		h.elsewhere[original+"/container-sample"] = true
		return h
	}

	t.Run("a failed bind, then a delete", func(t *testing.T) {
		h := provisionedOnOriginal(t)
		h.failOn["bind container-sample"] = errors.New("the controller refused the request")
		if err := h.steps().Start(ctx, folder, h); err == nil {
			t.Fatal("the start passed")
		}
		if h.elsewhere[original+"/container-sample"] {
			t.Fatal("the binding on the original agent stays")
		}
		// The name is recorded on the new agent before its request, as every bind is.
		if held := h.held("sample"); !slices.Equal(held.Names, []store.Name{{Name: "container-sample", Agent: sampleAgent, Current: true}}) {
			t.Fatalf("held %+v", held)
		}
		delete(h.failOn, "bind container-sample")
		if err := h.deleteAt(h.steps(), filepath.Join(t.TempDir(), "sample")); err != nil {
			t.Fatal(err)
		}
		want := []string{original + "/container-sample", sampleAgent + "/container-sample"}
		if !slices.Equal(h.unbound, want) {
			t.Fatalf("unbound %v, want %v", h.unbound, want)
		}
		if len(h.bound) != 0 || len(h.elsewhere) != 0 {
			t.Fatalf("live bindings %v %v", h.bound, h.elsewhere)
		}
		h.wantGone(t, "sample")
	})

	t.Run("the original agent outside the grant", func(t *testing.T) {
		h := provisionedOnOriginal(t)
		h.failOn["unbind container-sample"] = fmt.Errorf("wrapped: %w", operator.ErrOutsideGrant)
		err := h.steps().Start(ctx, folder, h)
		wantKind(t, err, KindBindingOutsideGrant)
		if !strings.Contains(err.Error(), "container-sample on agent "+original) {
			t.Fatalf("message %v", err)
		}
		if slices.Contains(h.seen(), "bind container-sample") {
			t.Fatal("bound on the new agent while the original binding stays")
		}
		recorded := []store.Name{{Name: "container-sample", Agent: original, Current: true}}
		if held := h.held("sample"); !slices.Equal(held.Names, recorded) {
			t.Fatalf("held %+v", held)
		}
		// A delete fails the same way and keeps the workspace's data.
		wantKind(t, h.deleteAt(h.steps(), filepath.Join(t.TempDir(), "sample")), KindBindingOutsideGrant)
		if held := h.held("sample"); !slices.Equal(held.Names, recorded) || len(held.Keys) != 1 {
			t.Fatalf("held %+v", held)
		}
	})

	t.Run("a bind that succeeds", func(t *testing.T) {
		h := provisionedOnOriginal(t)
		if err := h.steps().Start(ctx, folder, h); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.bound["container-sample"]; !ok || len(h.elsewhere) != 0 {
			t.Fatalf("bound %v, elsewhere %v", h.bound, h.elsewhere)
		}
		if held := h.held("sample"); !slices.Equal(held.Names, []store.Name{{Name: "container-sample", Agent: sampleAgent, Current: true, BindingID: "container-sample"}}) {
			t.Fatalf("held %+v", held)
		}
	})
}

func TestAFailedJobLeavesWhatTheDataHolds(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	h.hold("sample", "container-old", false)
	h.failOn["up"] = errors.New("failed")
	if err := h.steps().Start(context.Background(), folder, h); err == nil {
		t.Fatal("passed")
	}
	if slices.Contains(h.seen(), "unbind container-old") {
		t.Error("retired by a failed job")
	}
	if held := h.held("sample"); len(held.Names) != 2 {
		t.Fatalf("held %+v", held)
	}
}

func TestRetiringWithoutWhatItNeedsNamesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		clients func(*host) Clients
		hold    func(*host)
		want    string
	}{
		"wyrwood": {func(h *host) Clients { return Clients{Keys: h} }, func(h *host) { h.hold("sample", "container-old", false) }, "provisioning.wyrwood"},
		"xolotl":  {func(h *host) Clients { return Clients{Bindings: h, Agent: sampleAgent} }, func(h *host) { h.holdKey("sample", "a", "b") }, "provisioning.xolotl"},
		"an imported name with no agent": {func(h *host) Clients { return Clients{Bindings: h, Agent: sampleAgent, Keys: h} }, func(h *host) {
			if err := h.data.HoldName(context.Background(), "sample", "container-old", "", false); err != nil {
				h.t.Fatal(err)
			}
		}, "imported with no provisioning.wyrwood"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.definitions[folder] = devcontainer.Definition{}
			tc.hold(h)
			err := h.with(tc.clients(h)).Start(context.Background(), folder, h)
			wantKind(t, err, KindUnavailable)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %v", err)
			}
			if held := h.held("sample"); held.Empty() {
				t.Fatal("the name or key left the data")
			}
		})
	}
}

func TestAStartReplacesAContainerAFailedRebuildLeft(t *testing.T) {
	h := newHost(t)
	h.state, h.container, h.containerName = devcontainer.StateRunning, "old-id", "container-old"
	h.failOn["issue host-sample/sample"] = &xolotl.Error{Operation: "issue", Unreachable: true}
	s := h.steps()
	// Xolotl out of reach fails it, because the old container is not kept.
	if err := s.Start(context.Background(), folder, h); err == nil {
		t.Fatal("went on without Xolotl")
	}
	delete(h.failOn, "issue host-sample/sample")
	h.calls = nil
	if err := s.Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-rebuild", "check", "issue host-sample/sample", "bind container-sample", "build", "create", "up")
	if h.container != "created-id" {
		t.Errorf("container %s", h.container)
	}
	if !strings.Contains(h.logged.String(), "replacing it") {
		t.Errorf("console %s", h.logged.String())
	}
}

// The block's removal failed in its rebuild, so a start must not unbind what
// the kept container still uses.
func TestAStartWithoutABlockReplacesTheContainerBeforeItRetires(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = devcontainer.Definition{}
	h.state, h.container = devcontainer.StateStopped, "old-id"
	h.hold("sample", "container-sample", true)
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "plain-rebuild", "unbind container-sample")
	if held := h.held("sample"); !held.Empty() {
		t.Fatalf("held %+v", held)
	}
}

func TestAStartWithoutABlockOrANameKeepsItsContainer(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = devcontainer.Definition{}
	h.state, h.container = devcontainer.StateStopped, "old-id"
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "plain-start")
}

func (h *host) Policies(context.Context) (operator.Grant, []operator.Policy, error) {
	return operator.Grant{}, []operator.Policy{{Name: "workspace"}, {Name: "client-key", ValueNames: []string{"key_name"}}, {Name: "old"}, {Name: "a"}}, nil
}
func (h *host) Get(ctx context.Context, id string) (operator.Binding, error) {
	b, found, err := h.Read(ctx, id)
	if err != nil {
		return b, err
	}
	if !found {
		return b, operator.ErrNotFound
	}
	return b, nil
}
func (h *host) Change(_ context.Context, b operator.Binding, selection wyrwood.Selection) (operator.Binding, error) {
	if err := h.note("change " + b.Key.Value); err != nil {
		return operator.Binding{}, err
	}
	h.Lock()
	h.bound[b.Key.Value] = selection
	h.Unlock()
	b.Policies = selection.Policies
	b.Settings.Values = selection.Values
	b.Settings.AccessGroup = &selection.AccessGroup
	b.Revision++
	b.Applied = true
	return b, nil
}

func TestProvisioningPassesStoredXolotlKeyToBringUp(t *testing.T) {
	for _, operation := range []string{"create", "start", "rebuild"} {
		for _, configured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/xolotl=%t", operation, configured), func(t *testing.T) {
				h := newHost(t)
				clients := Clients{Bindings: h, Agent: sampleAgent}
				if configured {
					clients.Keys, clients.KeyPolicy = h, "configured-key"
				}
				steps := h.with(clients)
				ctx := context.Background()
				var err error
				switch operation {
				case "create":
					err = steps.Create(ctx, folder, h, func(context.Context) error { return nil })
				case "start":
					err = steps.Start(ctx, folder, h)
				case "rebuild":
					err = steps.Rebuild(ctx, folder, h)
				}
				if err != nil {
					t.Fatal(err)
				}
				selection, err := h.data.Selection(ctx, "sample")
				if err != nil {
					t.Fatal(err)
				}
				if configured {
					if selection.Key == nil || selection.Key.String() != "configured-key/sample" || !slices.Equal(h.xolotlKeys, []string{selection.Key.String()}) {
						t.Fatalf("stored %+v; BringUp received %v", selection.Key, h.xolotlKeys)
					}
				} else if selection.Key != nil || len(h.xolotlKeys) != 0 {
					t.Fatalf("unconfigured key: %+v; BringUp %v", selection.Key, h.xolotlKeys)
				}
			})
		}
	}
}
