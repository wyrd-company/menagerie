package workflows

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

func (w *world) deleteWorkspace(t *testing.T, name string) (*jobs.Job, jobs.Snapshot) {
	t.Helper()
	job := w.startJob(t, "delete", name, "")
	return job, waitEnded(t, job)
}

func TestDeleteBacksUpThenRemovesAndAllowsALaterCreate(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	if err := os.WriteFile(filepath.Join(w.root, "alpha", "new-file"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := w.remoteBranches(t)["alpha"]
	job, snap := w.deleteWorkspace(t, "alpha")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if want := []string{"stop", "commit", "backup", "remove-container", "remove-folder"}; !slices.Equal(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	after := w.remoteBranches(t)["alpha"]
	if after == before || after.IsZero() {
		t.Errorf("backup at %s, was %s", after, before)
	}
	if _, err := os.Lstat(filepath.Join(w.root, "alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("folder stays: %v", err)
	}
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"delete","workspace":"alpha"}`); status != http.StatusNotFound || !strings.Contains(body, "workspace-not-found") {
		t.Errorf("%d %s", status, body)
	}

	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	var archived bool
	for name, hash := range w.remoteBranches(t) {
		archived = archived || (strings.HasPrefix(name, "archive/alpha-") && hash == after)
	}
	if !archived {
		t.Errorf("branches %v", w.remoteBranches(t))
	}
}

func TestDeleteOfAFolderWithNoAppliedRecordHasNoBackup(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(filepath.Join(w.root, "alpha", "partial"), 0o755); err != nil {
		t.Fatal(err)
	}
	job, snap := w.deleteWorkspace(t, "alpha")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if want := []string{"stop", "remove-container", "remove-folder"}; !slices.Equal(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	if got := w.remoteBranches(t); len(got) != 0 {
		t.Errorf("branches %v", got)
	}
}

// A retry after the removal began finds a repository under another name, and
// must not fail on it or push what is left.
func TestARetryAfterTheRemovalBeganSkipsTheBackup(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	before := w.remoteBranches(t)["alpha"]
	folder := filepath.Join(w.root, "alpha")
	if err := os.Rename(filepath.Join(folder, ".git"), filepath.Join(folder, devcontainer.RemovingMark)); err != nil {
		t.Fatal(err)
	}
	job, snap := w.deleteWorkspace(t, "alpha")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if want := []string{"stop", "remove-container", "remove-folder"}; !slices.Equal(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	if got := w.remoteBranches(t)["alpha"]; got != before {
		t.Errorf("branch moved to %s", got)
	}
}

func TestAFailedBackupRemovesNothing(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	if err := os.RemoveAll(w.remote); err != nil {
		t.Fatal(err)
	}
	job, snap := w.deleteWorkspace(t, "alpha")
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupFailed || !strings.Contains(snap.Error.Message, "nothing was removed") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if strings.Contains(logText(job), "delete the workspace again") {
		t.Errorf("console %s", logText(job))
	}
	if strings.Contains(snap.Error.Message, w.remote) {
		t.Errorf("message holds the repository's address: %s", snap.Error.Message)
	}
	if _, err := os.Lstat(filepath.Join(w.root, "alpha", ".git")); err != nil {
		t.Errorf("repository: %v", err)
	}
	if got := w.rt.seen(); slices.Contains(got, "remove-folder alpha") || slices.Contains(got, "remove-container alpha") {
		t.Errorf("runtime %v", got)
	}
}

// An applied record with no repository and no mark was not this job's removal,
// so its files have no backup, and nothing may go.
func TestARecordWithNoRepositoryRemovesNothing(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	folder := filepath.Join(w.root, "alpha")
	if err := os.RemoveAll(filepath.Join(folder, ".git")); err != nil {
		t.Fatal(err)
	}
	_, snap := w.deleteWorkspace(t, "alpha")
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupFailed {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if _, err := os.Lstat(folder); err != nil {
		t.Errorf("folder: %v", err)
	}
}

func TestAFailureAfterTheBackupSaysToDeleteAgain(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	w.rt.failOn = map[string]error{"remove-folder": errors.New("no removal")}
	job, snap := w.deleteWorkspace(t, "alpha")
	if snap.State != jobs.Failed {
		t.Fatalf("%+v", snap)
	}
	if !strings.Contains(logText(job), "delete the workspace again") {
		t.Errorf("console %s", logText(job))
	}
	w.rt.failOn = nil
	if _, snap := w.deleteWorkspace(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

func TestDeleteRefusesAnInvalidOrAbsentName(t *testing.T) {
	w := newWorld(t)
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"delete","workspace":".dot"}`); status != http.StatusBadRequest || !strings.Contains(body, "invalid-workspace-name") {
		t.Errorf("%d %s", status, body)
	}
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"delete","workspace":"absent"}`); status != http.StatusNotFound || !strings.Contains(body, "workspace-not-found") {
		t.Errorf("%d %s", status, body)
	}
}
