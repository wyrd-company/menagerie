package provisioning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
	"github.com/wyrd-company/menagerie/backend/internal/xolotl"
)

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
	holders map[string]string
	failOn  map[string]error
	// bound is the binding of each name.
	bound map[string]wyrwood.Selection
	// status gives a binding's status for each read; nil is a ready binding of
	// the container that Create gave.
	status func(name string) operator.BindingStatus
	// readErr fails a read.
	readErr error
	issued  map[string]bool
	logged  strings.Builder
	phases  []string
}

func newHost(t *testing.T) *host {
	return &host{
		t: t, state: devcontainer.StateNoContainer, created: "created-id",
		definitions: map[string]devcontainer.Definition{"/root/sample": definition(sampleBlock)},
		holders:     map[string]string{}, failOn: map[string]error{},
		bound: map[string]wyrwood.Selection{}, issued: map[string]bool{},
	}
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
	return h.note("plain-rebuild")
}
func (h *host) Mounting(context.Context, string) ([]string, error) { return nil, nil }
func (h *host) CheckHostPrerequisites(context.Context, string, devcontainer.Progress) error {
	return h.note("check")
}
func (h *host) Definition(folder string) (devcontainer.Definition, error) {
	h.Lock()
	defer h.Unlock()
	return h.definitions[folder], h.definedErr
}
func (h *host) ContainerHolder(_ context.Context, name string) (string, bool, error) {
	folder, found := h.holders[name]
	return folder, found, h.failOn["holder"]
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

func (b *fakeBringUp) Build(context.Context) error { return b.h.note("build") }

// Create keeps a container that exists, unless the job rebuilds.
func (b *fakeBringUp) Create(context.Context) (string, error) {
	if err := b.h.note("create"); err != nil {
		return "", err
	}
	b.h.Lock()
	defer b.h.Unlock()
	if b.rebuild || b.h.container == "" {
		b.h.container, b.h.state = b.h.created, devcontainer.StateStopped
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
func (h *host) Unbind(_ context.Context, name string) (operator.UnbindResult, error) {
	if err := h.note("unbind " + name); err != nil {
		return operator.UnbindResult{}, err
	}
	h.Lock()
	defer h.Unlock()
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
	binding := operator.Binding{Policies: selection.Policies, Settings: operator.Settings{AccessGroup: &group, Values: selection.Values}}
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

func (h *host) Names() ([]string, error) {
	var names []string
	for folder := range h.definitions {
		names = append(names, filepath.Base(folder))
	}
	slices.Sort(names)
	return names, nil
}
func (h *host) FolderPath(name string) (string, error) { return "/root/" + name, nil }

func (h *host) steps() *Steps {
	return New(Options{
		Clients: Clients{Bindings: h, Keys: h, Wait: 200 * time.Millisecond}, Runtime: h, Workspaces: h,
		Records: Records{Folder: filepath.Join(h.t.TempDir(), "records")}, Poll: time.Millisecond,
	})
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
	if selection := h.bound["container-sample"]; selection.AccessGroup != 1000 || !slices.Equal(selection.Policies, []string{"workspace", "client-key"}) || selection.Values["key_name"] != "sample" {
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
	for name, clients := range map[string]Clients{
		"wyrwood": {Keys: &host{}},
		"xolotl":  {Bindings: &host{}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			s := New(Options{Clients: clients, Runtime: h, Workspaces: h, Records: Records{Folder: t.TempDir()}})
			err := s.Start(context.Background(), folder, h)
			wantKind(t, err, KindUnavailable)
			if !strings.Contains(err.Error(), "provisioning."+name) {
				t.Errorf("message %v", err)
			}
			h.wantCalls(t, "begin-start")
		})
	}
}

func TestANameOrKeyAnotherWorkspaceHoldsIsRefused(t *testing.T) {
	other := "/root/other"
	for name, tc := range map[string]struct {
		prepare func(h *host, s *Steps)
		want    string
	}{
		"declared name": {func(h *host, _ *Steps) {
			h.definitions[other] = definition(`{"wyrwood":{"accessGroup":1,"policies":["a"]}}`)
		}, "the definition of other declares it"},
		"declared key": {func(h *host, _ *Steps) {
			def := definition(sampleBlock)
			def.ContainerName = "container-other"
			h.definitions[other] = def
		}, "Xolotl key host-sample/sample is already taken"},
		"an invalid block still holds its name": {func(h *host, _ *Steps) {
			h.definitions[other] = definition(`{"unknown":1}`)
		}, "the definition of other declares it"},
		"name in a record": {func(h *host, s *Steps) {
			h.definitions[other] = devcontainer.Definition{}
			if err := s.opts.Records.Write("other", Record{Names: []string{"container-sample"}}); err != nil {
				h.t.Fatal(err)
			}
		}, "the retirement record of other names it"},
		"key in a record": {func(h *host, s *Steps) {
			h.definitions[other] = devcontainer.Definition{}
			if err := s.opts.Records.Write("other", Record{Keys: []Key{{Policy: "host-sample", Name: "sample"}}}); err != nil {
				h.t.Fatal(err)
			}
		}, "the retirement record of other names it"},
		"another workspace's container": {func(h *host, _ *Steps) {
			h.holders["container-sample"] = other
		}, "the container of other has that name"},
		"a container of no workspace": {func(h *host, _ *Steps) {
			h.holders["container-sample"] = ""
		}, "belongs to no workspace"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			s := h.steps()
			tc.prepare(h, s)
			err := s.Start(context.Background(), folder, h)
			wantKind(t, err, KindInvalid)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %v, want %q", err, tc.want)
			}
			h.wantCalls(t, "begin-start")
		})
	}
}

func TestItsOwnContainerAndRecordDoNotTakeItsName(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	h.holders["container-sample"] = folder
	if err := s.opts.Records.Write("sample", Record{Names: []string{"container-sample"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
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
	s := New(Options{Clients: Clients{Bindings: h, Wait: time.Second}, Runtime: h, Workspaces: h, Records: Records{Folder: t.TempDir()}, Poll: time.Millisecond})
	if err := s.Start(context.Background(), folder, h); err != nil {
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
	s := New(Options{Clients: Clients{Bindings: h, Keys: keys, Wait: time.Second}, Runtime: h, Workspaces: h, Records: Records{Folder: t.TempDir()}, Poll: time.Millisecond})
	if err := s.Start(context.Background(), folder, h); err != nil {
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

func TestABindingThatDiffersIsReplacedWhileTheContainerIsStopped(t *testing.T) {
	h := newHost(t)
	h.state, h.container = devcontainer.StateRunning, "old-id"
	h.bound["container-sample"] = wyrwood.Selection{Policies: []string{"old"}, AccessGroup: 7, Values: map[string]string{"key_name": "sample"}}
	if err := h.steps().Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-start", "check", "issue host-sample/sample", "bind container-sample", "stop", "unbind container-sample", "bind container-sample", "build", "create", "up")
	console := h.logged.String()
	for _, want := range []string{"differs from the definition in policies, access group", "a part stayed", "token may stay valid"} {
		if !strings.Contains(console, want) {
			t.Errorf("console lacks %q: %s", want, console)
		}
	}
	if strings.Contains(console, "values") {
		t.Errorf("console names a field that did not change: %s", console)
	}
}

func TestDifferencesNameEachField(t *testing.T) {
	group := uint32(1)
	current := operator.Binding{Policies: []string{"b", "a"}, Settings: operator.Settings{AccessGroup: &group, Values: map[string]string{"x": "1"}}}
	same := wyrwood.Selection{Policies: []string{"a", "b"}, AccessGroup: 1, Values: map[string]string{"x": "1"}}
	if got := differences(current, same); !slices.Equal(got, []string{"its settings"}) {
		t.Errorf("same: %v", got)
	}
	for field, selection := range map[string]wyrwood.Selection{
		"policies":     {Policies: []string{"a"}, AccessGroup: 1, Values: map[string]string{"x": "1"}},
		"access group": {Policies: []string{"a", "b"}, AccessGroup: 2, Values: map[string]string{"x": "1"}},
		"values":       {Policies: []string{"a", "b"}, AccessGroup: 1, Values: map[string]string{"x": "2"}},
	} {
		if got := differences(current, selection); !slices.Equal(got, []string{field}) {
			t.Errorf("%s: %v", field, got)
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
	if !strings.Contains(err.Error(), "its running container and sockets") {
		t.Errorf("message %v", err)
	}
	if h.phases[len(h.phases)-1] != "verify" {
		t.Errorf("phases %v", h.phases)
	}
}

func TestAReadThatIsUnreachableIsWaitedOutAndAnyOtherFails(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	h.bound["container-sample"] = wyrwood.Selection{Policies: []string{"workspace", "client-key"}, AccessGroup: 1000, Values: map[string]string{"key_name": "sample"}}
	h.readErr = fmt.Errorf("wrapped: %w", operator.ErrUnreachable)
	err := s.Start(context.Background(), folder, h)
	wantKind(t, err, KindNotReady)
	if !strings.Contains(err.Error(), "the controller cannot be reached") {
		t.Errorf("message %v", err)
	}
	h.readErr = fmt.Errorf("wrapped: %w", operator.ErrOutsideGrant)
	err = s.Start(context.Background(), folder, h)
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

func TestRetireRecordsWhatTheDefinitionDeclares(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	if err := s.Retire(folder, h); err != nil {
		t.Fatal(err)
	}
	record, err := s.opts.Records.Read("sample")
	if err != nil {
		t.Fatal(err)
	}
	want := Record{Names: []string{"container-sample"}, Keys: []Key{{Policy: "host-sample", Name: "sample"}}}
	if !reflect.DeepEqual(record, want) {
		t.Fatalf("%+v", record)
	}
	// A repeat adds nothing.
	if err := s.Retire(folder, h); err != nil {
		t.Fatal(err)
	}
	if record, _ = s.opts.Records.Read("sample"); !reflect.DeepEqual(record, want) {
		t.Fatalf("after a repeat: %+v", record)
	}
	// It only grows.
	def := definition(sampleBlock)
	def.ContainerName = "container-next"
	h.definitions[folder] = def
	if err := s.Retire(folder, h); err != nil {
		t.Fatal(err)
	}
	record, _ = s.opts.Records.Read("sample")
	if !slices.Equal(record.Names, []string{"container-sample", "container-next"}) || len(record.Keys) != 1 {
		t.Fatalf("%+v", record)
	}
}

func TestRetireWithNothingDeclaredWritesNothing(t *testing.T) {
	for name, def := range map[string]devcontainer.Definition{
		"no block":      {ContainerName: "a"},
		"invalid block": definition(`{"other":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.definitions[folder] = def
			s := h.steps()
			if err := s.Retire(folder, h); err != nil {
				t.Fatal(err)
			}
			if entries, err := os.ReadDir(s.opts.Records.Folder); err == nil && len(entries) != 0 {
				t.Fatalf("%v", entries)
			}
		})
	}
}

func TestAStartedJobConsumesTheRecordLast(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	retired := Record{Names: []string{"container-old"}, Keys: []Key{{Policy: "host-sample", Name: "old"}}}
	if err := s.opts.Records.Write("sample", retired); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "begin-start", "check", "issue host-sample/sample", "bind container-sample", "build", "create", "up", "unbind container-old", "revoke host-sample/old")
	if h.phases[len(h.phases)-1] != "retire" {
		t.Errorf("phases %v", h.phases)
	}
	assertNoRecord(t, s, "sample")
}

func assertNoRecord(t *testing.T, s *Steps, name string) {
	t.Helper()
	if _, err := os.Stat(s.opts.Records.path(name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record stays: %v", err)
	}
}

func TestAJobWithNoDeclarationConsumesTheRecordToo(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = devcontainer.Definition{}
	s := h.steps()
	if err := s.opts.Records.Write("sample", Record{Names: []string{"container-old"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "plain-rebuild", "unbind container-old")
	assertNoRecord(t, s, "sample")
}

func TestWhatIsStillDeclaredIsRemovedWithoutActing(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	def := definition(`{"wyrwood":{"accessGroup":1,"policies":["a"]},"xolotl":{"keyPolicy":"host-sample","keyName":"other"}}`)
	def.ContainerName = "container-other"
	h.definitions["/root/other"] = def
	record := Record{
		Names: []string{"container-sample", "container-other"},
		Keys:  []Key{{Policy: "host-sample", Name: "sample"}, {Policy: "host-sample", Name: "other"}},
	}
	if err := s.opts.Records.Write("sample", record); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	for _, call := range h.seen() {
		if strings.HasPrefix(call, "unbind") || strings.HasPrefix(call, "revoke") {
			t.Errorf("acted: %s", call)
		}
	}
	assertNoRecord(t, s, "sample")
}

// Another workspace's record entry is its own to retire, so it does not keep a
// name in use as a declaration does.
func TestAnotherWorkspacesRecordDoesNotKeepARetiredName(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	h.definitions["/root/other"] = devcontainer.Definition{}
	for _, name := range []string{"sample", "other"} {
		if err := s.opts.Records.Write(name, Record{Names: []string{"container-old"}, Keys: []Key{{Policy: "host-sample", Name: "old"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"unbind container-old", "revoke host-sample/old"} {
		if !slices.Contains(h.seen(), call) {
			t.Errorf("no %s in %v", call, h.seen())
		}
	}
}

func TestAFailureWhileConsumingFailsTheJobAndKeepsTheRest(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	record := Record{Names: []string{"container-a", "container-b"}, Keys: []Key{{Policy: "host-sample", Name: "old"}}}
	if err := s.opts.Records.Write("sample", record); err != nil {
		t.Fatal(err)
	}
	h.failOn["unbind container-b"] = errors.New("refused")
	err := s.Start(context.Background(), folder, h)
	if err == nil || !strings.Contains(err.Error(), "the devcontainer is running") {
		t.Fatalf("%v", err)
	}
	kept, _ := s.opts.Records.Read("sample")
	if !slices.Equal(kept.Names, []string{"container-b"}) || len(kept.Keys) != 1 {
		t.Fatalf("%+v", kept)
	}
	delete(h.failOn, "unbind container-b")
	h.failOn["revoke host-sample/old"] = errors.New("refused")
	if err := s.Start(context.Background(), folder, h); err == nil {
		t.Fatal("the second start passed")
	}
	kept, _ = s.opts.Records.Read("sample")
	if len(kept.Names) != 0 || len(kept.Keys) != 1 {
		t.Fatalf("%+v", kept)
	}
	delete(h.failOn, "revoke host-sample/old")
	if err := s.Start(context.Background(), folder, h); err != nil {
		t.Fatal(err)
	}
	assertNoRecord(t, s, "sample")
}

func TestAFailedJobLeavesTheRecord(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	if err := s.opts.Records.Write("sample", Record{Names: []string{"container-old"}}); err != nil {
		t.Fatal(err)
	}
	h.failOn["up"] = errors.New("failed")
	if err := s.Start(context.Background(), folder, h); err == nil {
		t.Fatal("passed")
	}
	if slices.Contains(h.seen(), "unbind container-old") {
		t.Error("consumed by a failed job")
	}
}

func TestConsumingWithoutASectionNamesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		clients Clients
		record  Record
	}{
		"wyrwood": {Clients{Keys: &host{}}, Record{Names: []string{"container-old"}}},
		"xolotl":  {Clients{Bindings: &host{}}, Record{Keys: []Key{{Policy: "a", Name: "b"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.definitions[folder] = devcontainer.Definition{}
			s := New(Options{Clients: tc.clients, Runtime: h, Workspaces: h, Records: Records{Folder: t.TempDir()}})
			if err := s.opts.Records.Write("sample", tc.record); err != nil {
				t.Fatal(err)
			}
			wantKind(t, s.Start(context.Background(), folder, h), KindUnavailable)
		})
	}
}

func TestAnUnreadableRecordFailsTheJob(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	if err := os.MkdirAll(s.opts.Records.Folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.opts.Records.path("sample"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), folder, h); err == nil || !strings.Contains(err.Error(), "retirement record") {
		t.Fatalf("%v", err)
	}
}
