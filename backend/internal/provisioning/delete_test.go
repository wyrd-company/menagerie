package provisioning

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
)

// wholeDelete is the calls of one whole delete of the sample workspace.
var wholeDelete = []string{"stop", "save", "unbind container-sample", "remove-containers", "revoke host-sample/sample", "remove-folder"}

func (h *host) deleteSample(s *Steps) error {
	return s.Delete(context.Background(), folder, h, func(context.Context) error { return h.note("save") })
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
			h.failOn[step] = errors.New("step failed")
			if err := h.deleteSample(s); err == nil {
				t.Fatal("no failure")
			}
			h.failOn = map[string]error{}
			h.calls = nil
			if err := h.deleteSample(s); err != nil {
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
