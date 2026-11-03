package provisioning

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
)

// wholeDelete is the calls of one whole delete of the sample workspace.
var wholeDelete = []string{"stop", "save", "unbind container-sample", "remove-containers", "revoke host-sample/sample", "remove-folder"}

func (h *host) deleteSample(s *Steps) error {
	return h.deleteAt(s, folder)
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

func TestDeleteRunsEachStepInOrder(t *testing.T) {
	h := newHost(t)
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
}

func TestADeleteStoppedAtEachStepCanBeRetried(t *testing.T) {
	for _, step := range wholeDelete {
		t.Run(step, func(t *testing.T) {
			h := newHost(t)
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
			h.wantCalls(t, wholeDelete...)
		})
	}
}

func TestAFailedSaveRemovesNothing(t *testing.T) {
	h := newHost(t)
	h.failOn["save"] = errors.New("no backup")
	if err := h.deleteSample(h.steps()); err == nil {
		t.Fatal("no failure")
	}
	h.wantCalls(t, "stop", "save")
	if !strings.Contains(h.logged.String(), "nothing was removed") {
		t.Errorf("console %q", h.logged.String())
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

func TestDeleteWithNoDeclarationOrRecordCallsNeitherService(t *testing.T) {
	h := newHost(t)
	h.definitions[folder] = devcontainer.Definition{}
	s := New(Options{Runtime: h, Workspaces: h, Records: Records{Folder: t.TempDir()}})
	if err := h.deleteSample(s); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "stop", "save", "remove-containers", "remove-folder")
}

func TestDeleteOfAnUnreadableDefinitionStillRemovesTheFolder(t *testing.T) {
	h := newHost(t)
	h.definedErr = errors.New("no definition")
	if err := h.deleteSample(h.steps()); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "stop", "save", "remove-containers", "remove-folder")
	if !strings.Contains(h.logged.String(), "cannot be read") {
		t.Errorf("console %q", h.logged.String())
	}
}

func TestDeleteTakesWhatTheRecordNamesAndRemovesTheRecord(t *testing.T) {
	h := newHost(t)
	s := h.steps()
	record := Record{Names: []string{"container-old"}, Keys: []Key{{Policy: "host-sample", Name: "old-key"}}}
	if err := s.opts.Records.Write("sample", record); err != nil {
		t.Fatal(err)
	}
	if err := h.deleteSample(s); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "stop", "save", "unbind container-old", "unbind container-sample", "remove-containers", "revoke host-sample/old-key", "revoke host-sample/sample", "remove-folder")
	if _, err := os.Stat(s.opts.Records.path("sample")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record stays: %v", err)
	}
}

func TestDeleteLeavesWhatAnotherWorkspaceDeclares(t *testing.T) {
	h := newHost(t)
	h.definitions["/root/other"] = definition(sampleBlock)
	if err := h.deleteSample(h.steps()); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "stop", "save", "remove-containers", "remove-folder")
	if !strings.Contains(h.logged.String(), "the definition of other declares it") {
		t.Errorf("console %q", h.logged.String())
	}
}

func TestDeleteLeavesADeclarationWhoseNameAnotherContainerHolds(t *testing.T) {
	h := newHost(t)
	h.holders["container-sample"] = "/root/other"
	if err := h.deleteSample(h.steps()); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, "stop", "save", "remove-containers", "remove-folder")
}

func TestDeleteNeedsTheSectionsWhatItHoldsNeeds(t *testing.T) {
	for name, clients := range map[string]func(*host) Clients{
		"wyrwood": func(h *host) Clients { return Clients{Keys: h} },
		"xolotl":  func(h *host) Clients { return Clients{Bindings: h} },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			s := New(Options{Clients: clients(h), Runtime: h, Workspaces: h, Records: Records{Folder: t.TempDir()}})
			err := h.deleteSample(s)
			wantKind(t, err, KindUnavailable)
			if !strings.Contains(err.Error(), "provisioning."+name) {
				t.Errorf("message %v", err)
			}
			h.wantCalls(t)
		})
	}
}

func TestDeleteOfAnInvalidBlockActsOnWhatItNames(t *testing.T) {
	const invalid = `{"wyrwood":{},"xolotl":{"keyPolicy":"host-sample","keyName":"sample"}}`
	h := newHost(t)
	h.definitions[folder] = definition(invalid)
	if err := h.deleteSample(h.steps()); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, wholeDelete...)

	h = newHost(t)
	h.definitions[folder] = definition(invalid)
	s := New(Options{Runtime: h, Workspaces: h, Records: Records{Folder: t.TempDir()}})
	if err := h.deleteSample(s); err != nil {
		t.Fatalf("an invalid block held up the delete: %v", err)
	}
	h.wantCalls(t, "stop", "save", "remove-containers", "remove-folder")
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
	if _, err := os.Stat(s.opts.Records.path("sample")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record stays: %v", err)
	}
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
	s := h.steps()
	real := sampleAt(t, h, true)
	if err := s.opts.Records.Write("sample", Record{Removing: true}); err != nil {
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
	if err := s.opts.Records.Write("sample", Record{Removing: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), real, h); err != nil {
		t.Fatal(err)
	}
	record, err := s.opts.Records.Read("sample")
	if err != nil || record.Removing {
		t.Errorf("record %+v %v", record, err)
	}
}

// A folder that lost its repository with no delete under way has files with no
// backup, so the save runs, and it fails the job there.
func TestAFolderWithNoRepositoryAndNoRemovalStillSaves(t *testing.T) {
	h := newHost(t)
	real := sampleAt(t, h, false)
	if err := h.deleteAt(h.steps(), real); err != nil {
		t.Fatal(err)
	}
	h.wantCalls(t, wholeDelete...)
}
