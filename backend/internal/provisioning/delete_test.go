package provisioning

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wyrd-company/wyrwood/operator"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// wholeDelete is the calls of one whole delete of the provisioned sample
// workspace.
var wholeDelete = []string{"stop", "save", "unbind container-sample", "remove-containers", "revoke host-sample/sample", "remove-folder"}

// deleteSample deletes the sample workspace in a real folder with a repository.
func (h *host) deleteSample(s *Steps) error {
	return h.deleteAt(s, sampleAt(h.t, h, true))
}

func (h *host) deleteAt(s *Steps, at string) error {
	return s.Delete(context.Background(), at, h, func(context.Context) error { return h.note("save") })
}

// sampleAt is a real folder of the sample's name that declares what the sample
// declares, with or without a repository.
func sampleAt(t *testing.T, h *host, repository bool) string {
	t.Helper()
	real := filepath.Join(t.TempDir(), "sample")
	path := real
	if repository {
		path = filepath.Join(real, ".git")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	h.definitions = map[string]devcontainer.Definition{real: h.definitions[folder]}
	return real
}

// wantGone checks that the workspace has no data left.
func (h *host) wantGone(t *testing.T, workspace string) {
	t.Helper()
	if exists, err := h.data.Exists(context.Background(), workspace); err != nil || exists {
		t.Fatalf("the data of %s stays: %t %v", workspace, exists, err)
	}
}

func TestDeleteRunsEachStepInOrder(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	if err := h.deleteSample(h.steps()); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, wholeDelete...)
	want := []string{"read-definition", "unbind", "revoke-key"}
	for _, phase := range want {
		if !strings.Contains(strings.Join(h.phases, " "), phase) {
			t.Errorf("phases %v lack %s", h.phases, phase)
		}
	}
	if !strings.Contains(h.logged.String(), "a part stayed") || !strings.Contains(h.logged.String(), "may stay valid") {
		t.Errorf("console %q", h.logged.String())
	}
	if !slices.Equal(h.unbound, []string{sampleAgent + "/container-sample"}) {
		t.Errorf("unbound %v", h.unbound)
	}
	h.wantGone(t, "sample")
}

func TestADeleteStoppedAtEachStepCanBeRetried(t *testing.T) {
	// What a failed delete confirmed gone left the data, so a retry does not
	// ask for it again.
	unbound := slices.DeleteFunc(slices.Clone(wholeDelete), func(c string) bool { return c == "unbind container-sample" })
	revoked := slices.DeleteFunc(slices.Clone(unbound), func(c string) bool { return c == "revoke host-sample/sample" })
	for step, retry := range map[string][]string{
		"stop":                      wholeDelete,
		"save":                      wholeDelete,
		"unbind container-sample":   wholeDelete,
		"remove-containers":         unbound,
		"revoke host-sample/sample": unbound,
		"remove-folder":             revoked,
	} {
		t.Run(step, func(t *testing.T) {
			h := newHost(t)
			h.provisioned()
			s := h.steps()
			// The fake removal touches nothing, so the repository stays, and each
			// retry backs up again.
			real := sampleAt(t, h, true)
			h.failOn[step] = errors.New("step failed")
			if err := h.deleteAt(s, real); err == nil {
				t.Fatal("no failure")
			}
			h.failOn = map[string]error{}
			h.calls = nil
			if err := h.deleteAt(s, real); err != nil {
				t.Fatal(err)
			}
			h.wantCalls(t, retry...)
			h.wantGone(t, "sample")
		})
	}
}

func TestAFailedSaveRemovesNothing(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	h.failOn["save"] = errors.New("no backup")
	if err := h.deleteSample(h.steps()); err == nil {
		t.Fatal("no failure")
	}
	h.wantCalls(t, "stop", "save")
	if !strings.Contains(h.logged.String(), "nothing was removed") {
		t.Errorf("console %q", h.logged.String())
	}
	if held := h.held("sample"); len(held.Names) != 1 || len(held.Keys) != 1 {
		t.Fatalf("held %+v", held)
	}
}

func TestDeleteRefusesAContainerOutsideTheWorkspace(t *testing.T) {
	h := newHost(t)
	h.mountingOutside = []string{"abc123"}
	wantKind(t, h.deleteSample(h.steps()), KindDeleteRefused)
	h.wantCalls(t)
	if !strings.Contains(h.logged.String(), "abc123") {
		t.Errorf("console %q", h.logged.String())
	}
}

func TestDeleteRefusesAContainerThatStillMountsAfterTheStop(t *testing.T) {
	h := newHost(t)
	h.mounting = []string{"abc123"}
	wantKind(t, h.deleteSample(h.steps()), KindDeleteRefused)
	h.wantCalls(t, "stop")
	if !strings.Contains(h.logged.String(), "devcontainer is stopped") {
		t.Errorf("console %q", h.logged.String())
	}
}

// A workspace that holds no name and no key calls neither service, whatever
// its definition declares.
func TestDeleteOfAWorkspaceThatHoldsNothingCallsNeitherService(t *testing.T) {
	h := newHost(t)
	if err := h.deleteSample(h.with(Clients{})); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "stop", "save", "remove-containers", "remove-folder")
	h.wantGone(t, "sample")
}

// wyrd-company/menagerie#75: what a delete removes comes from Menagerie's data,
// never from the definition, so a definition edited so that it no longer
// parses, or no longer declares the binding and the key, still has both
// removed.
func TestADeleteAfterTheDefinitionStopsParsingOrDeclaringRemovesTheBindingAndTheKey(t *testing.T) {
	another := definition(`{"wyrwood":{"accessGroup":1000,"policies":["a"]},"xolotl":{"keyPolicy":"host-sample","keyName":"another"}}`)
	another.ContainerName = "container-another"
	for name, tc := range map[string]struct {
		def devcontainer.Definition
		err error
	}{
		"a definition that does not parse": {err: errors.New("devcontainer.json: invalid character '}'")},
		"no declaration":                   {def: devcontainer.Definition{ContainerName: "container-sample"}},
		"a block that is not valid":        {def: definition(`{"unknown":1}`)},
		"a declaration of another name":    {def: another},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.provisioned()
			real := sampleAt(t, h, true)
			h.definitions[real], h.definedErr = tc.def, tc.err
			if err := h.deleteAt(h.steps(), real); err != nil {
				t.Fatal(err)
			}
			h.wantCalls(t, wholeDelete...)
			h.wantGone(t, "sample")
		})
	}
}

func TestDeleteNeedsTheSectionsWhatItHoldsNeeds(t *testing.T) {
	for name, tc := range map[string]struct {
		clients func(*host) Clients
		agent   string
		want    string
	}{
		"wyrwood":                        {func(h *host) Clients { return Clients{Keys: h} }, sampleAgent, "provisioning.wyrwood"},
		"xolotl":                         {func(h *host) Clients { return Clients{Bindings: h, Agent: sampleAgent} }, sampleAgent, "provisioning.xolotl"},
		"an imported name with no agent": {func(h *host) Clients { return Clients{Bindings: h, Agent: sampleAgent, Keys: h} }, "", "imported with no provisioning.wyrwood"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			if err := h.data.HoldName(context.Background(), "sample", "container-sample", tc.agent, true); err != nil {
				t.Fatal(err)
			}
			h.holdKey("sample", "host-sample", "sample")
			err := h.deleteSample(h.with(tc.clients(h)))
			wantKind(t, err, KindUnavailable)
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was removed") {
				t.Errorf("message %v", err)
			}
			h.wantCalls(t)
		})
	}
}

// A binding that a policy outside the grant hides is not taken for a removed
// one: the delete fails, naming it, before any container, key or folder is
// removed, and keeps its record.
func TestADeleteRefusedByAHiddenBindingKeepsItsRecord(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	h.hold("sample", "container-hidden", false)
	h.failOn["unbind container-hidden"] = fmt.Errorf("wrapped: %w", operator.ErrOutsideGrant)
	err := h.deleteSample(h.steps())
	wantKind(t, err, KindBindingOutsideGrant)
	if !strings.Contains(err.Error(), "container-hidden") {
		t.Fatalf("message %v", err)
	}
	h.wantCalls(t, "stop", "save", "unbind container-hidden")
	held := h.held("sample")
	if !slices.ContainsFunc(held.Names, func(n store.Name) bool { return n.Name == "container-hidden" }) || len(held.Keys) != 1 {
		t.Fatalf("held %+v", held)
	}
}

// A workspace whose folder is gone and that holds a name or a key is deleted by
// the same steps without the stop, the backup and the folder's removal.
func TestDeleteOfAWorkspaceWithNoFolderRemovesWhatTheDataHolds(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	gone := filepath.Join(t.TempDir(), "sample")
	if err := h.deleteAt(h.steps(), gone); err != nil {
		t.Fatal(err)
	}
	// remove-folder removes only the workspace socket's folder of a folder that
	// is gone.
	h.wantCalls(t, "unbind container-sample", "remove-containers", "revoke host-sample/sample", "remove-folder")
	h.wantGone(t, "sample")
	if !strings.Contains(h.logged.String(), "folder is gone") {
		t.Errorf("console %q", h.logged.String())
	}
}

// A removal can stop with any part of the folder left, once it has taken the
// repository. The state that says so is outside the folder, so what is left
// cannot change a retry.
func TestARetryAfterTheRemovalTookTheRepositorySkipsTheSave(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	real := sampleAt(t, h, false)
	h.failOn["remove-folder"] = errors.New("stopped part way")
	if err := h.deleteAt(s, real); err == nil {
		t.Fatal("no failure")
	}
	h.failOn = map[string]error{}
	h.calls = nil
	if err := h.deleteAt(s, real); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(h.seen(), "save") {
		t.Errorf("calls %v", h.seen())
	}
	if !slices.Contains(h.seen(), "remove-folder") {
		t.Errorf("calls %v", h.seen())
	}
	h.wantGone(t, "sample")
}

func TestNoJobStartsWhatARemovalLeft(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	real := sampleAt(t, h, false)
	h.failOn["remove-folder"] = errors.New("stopped part way")
	if err := h.deleteAt(s, real); err == nil {
		t.Fatal("no failure")
	}
	h.calls = nil
	wantKind(t, s.Start(context.Background(), real, h), KindDeleteUnfinished)
	wantKind(t, s.Rebuild(context.Background(), real, h), KindDeleteUnfinished)
	h.wantCalls(t)
}

// A removal that stopped before it took the repository left a folder that an
// upgrade can still change, and whose changes may have no backup. A later create
// of the name makes the same kind of folder, whose generated files may have none.
func TestADeleteBacksUpAFolderThatHasARepository(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	s := h.steps()
	real := sampleAt(t, h, true)
	if err := h.data.SetRemoving(context.Background(), "sample", folderID(real)); err != nil {
		t.Fatal(err)
	}
	if err := h.deleteAt(s, real); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, wholeDelete...)
}

func TestAStartOfAFolderThatHasARepositoryEndsTheState(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	real := sampleAt(t, h, true)
	h.definitions[real] = devcontainer.Definition{}
	if err := h.data.SetRemoving(context.Background(), "sample", folderID(real)); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), real, h); err != nil {
		t.Fatal(err)
	}
	if removing, err := h.data.Removing(context.Background(), "sample"); err != nil || removing != "" {
		t.Errorf("removing %q %v", removing, err)
	}
}

// A folder that lost its repository with no delete under way has files with no
// backup, so the save runs, and it fails the job there.
func TestAFolderWithNoRepositoryAndNoRemovalStillSaves(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	real := sampleAt(t, h, false)
	if err := h.deleteAt(h.steps(), real); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, wholeDelete...)
}

// Another folder imported under the name has no repository either, and its files
// have no backup.
func TestAFolderOfTheNameThatARemovalWasNotAboutStillSaves(t *testing.T) {
	h := newHost(t)
	h.provisioned()
	s := h.steps()
	real := sampleAt(t, h, false)
	if err := h.data.SetRemoving(context.Background(), "sample", "another-folder"); err != nil {
		t.Fatal(err)
	}
	if err := h.deleteAt(s, real); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, wholeDelete...)
}

// A removal that fails after it emptied the folder leaves no state, so it cannot
// reach a later folder of the name.
func TestTheStateEndsWhileTheEmptyFolderStands(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	real := sampleAt(t, h, false)
	h.failAfterEmptied = errors.New("the empty folder stayed")
	if err := h.deleteAt(s, real); err == nil {
		t.Fatal("no failure")
	}
	if removing, err := h.data.Removing(context.Background(), "sample"); err != nil || removing != "" {
		t.Errorf("removing %q %v", removing, err)
	}
}
