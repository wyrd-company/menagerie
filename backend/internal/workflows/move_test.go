package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// destination registers a second backup repository, an empty bare
// repository, and returns it.
func (w *world) destination(t *testing.T, name string) store.BackupRepository {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".git")
	if _, err := git.PlainInit(path, true); err != nil {
		t.Fatal(err)
	}
	r, err := w.data.RegisterBackupRepository(context.Background(), store.BackupRepository{Name: name, URL: path})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (w *world) moveBackup(t *testing.T, name, id string) (*jobs.Job, jobs.Snapshot) {
	t.Helper()
	status, body := w.do(t, http.MethodPost, "/api/jobs", fmt.Sprintf(`{"operation":"move-backup","workspace":%q,"backupRepositoryId":%q}`, name, id))
	if status != http.StatusAccepted {
		t.Fatalf("start move-backup: %d %s", status, body)
	}
	var snap jobs.Snapshot
	_ = json.Unmarshal([]byte(body), &snap)
	job, ok := w.manager.Get(snap.JobID)
	if !ok {
		t.Fatalf("job %s not found", snap.JobID)
	}
	return job, waitEnded(t, job)
}

func branchAt(t *testing.T, url, name string) plumbing.Hash {
	t.Helper()
	hash, found, err := (&backup.Remote{URL: url}).BranchCommit(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return plumbing.ZeroHash
	}
	return hash
}

func backsUpTo(t *testing.T, w *world, name string) string {
	t.Helper()
	r, ok, err := w.data.WorkspaceBackupRepository(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return ""
	}
	return r.ID
}

// A move checks the destination, commits what changed, pushes it there,
// reads it back and only then records the destination. The earlier
// repository keeps its branch as it was.
func TestAMoveWritesAndVerifiesABackupBeforeItSwitchesRepositories(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	before := branchAt(t, w.remote, "alpha")
	writeFile(t, folder, "notes.txt", "written in the workspace\n")
	other := w.destination(t, "Other")

	job, snap := w.moveBackup(t, "alpha", other.ID)
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v\n%s", snap.Error, logText(job))
	}
	if want := []string{"verify", "commit", "backup"}; !reflect.DeepEqual(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	repo, _ := git.PlainOpen(folder)
	head, _ := repo.Reference(plumbing.NewBranchReferenceName("alpha"), false)
	if got := branchAt(t, other.URL, "alpha"); got != head.Hash() || got == before {
		t.Fatalf("the destination's branch is at %s, the workspace at %s", got, head.Hash())
	}
	if got := branchAt(t, w.remote, "alpha"); got != before {
		t.Fatalf("the earlier repository's branch moved to %s", got)
	}
	if got := backsUpTo(t, w, "alpha"); got != other.ID {
		t.Fatalf("backs up to %q", got)
	}
	log := logText(job)
	for _, line := range []string{"moving the backups of alpha from Backups (backups) to Other (other)", "Menagerie can write to Other", "workspace alpha now backs up to Other; its earlier backups stay on branch alpha in Backups"} {
		if !strings.Contains(log, line) {
			t.Errorf("log lacks %q:\n%s", line, log)
		}
	}
	status, body := w.do(t, http.MethodGet, "/api/workspaces", "")
	if status != http.StatusOK || !strings.Contains(body, `"backupRepository":{"id":"other","name":"Other","url":"`+other.URL+`"}`) {
		t.Fatalf("%d %s", status, body)
	}
}

// A destination whose branch of the workspace's name holds another history
// is refused before anything is committed or pushed; one that holds the
// workspace's own earlier commit, as a move that failed after its push
// leaves, is taken over.
func TestAMoveRefusesABranchThatHoldsAnotherHistory(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	other := w.destination(t, "Other")

	stranger := t.TempDir()
	if err := backup.Init(stranger, "alpha"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, stranger, ".toha.json", "{}\n")
	if _, err := backup.CommitRoot(stranger, "alpha", backup.Output{Record: ".toha.json"}, "another alpha", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := (&backup.Remote{URL: other.URL}).Push(context.Background(), stranger, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	foreign := branchAt(t, other.URL, "alpha")
	writeFile(t, folder, "notes.txt", "uncommitted\n")

	job, snap := w.moveBackup(t, "alpha", other.ID)
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupBranchConflict {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if !strings.Contains(snap.Error.Message, "not in the workspace's history; nothing was pushed") || !strings.Contains(snap.Error.Message, "the workspace still backs up to Backups") {
		t.Fatalf("%s", snap.Error.Message)
	}
	if got := branchAt(t, other.URL, "alpha"); got != foreign {
		t.Fatal("the conflicting branch was replaced")
	}
	if pending, _ := backup.Pending(folder, "alpha"); !pending {
		t.Fatal("the refused move committed the workspace's changes")
	}
	if got := backsUpTo(t, w, "alpha"); got != "backups" {
		t.Fatalf("backs up to %q", got)
	}

	// The workspace's own commit at the destination is its history.
	own := w.destination(t, "Own")
	if _, err := (&backup.Remote{URL: own.URL}).Push(context.Background(), folder, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	if job, snap := w.moveBackup(t, "alpha", own.ID); snap.State != jobs.Succeeded {
		t.Fatalf("%+v\n%s", snap.Error, logText(job))
	}
	if got := backsUpTo(t, w, "alpha"); got != own.ID {
		t.Fatalf("backs up to %q", got)
	}
}

// A destination Menagerie cannot write to fails the move with its reason,
// and the workspace keeps its repository, so a retry can follow.
func TestAFailedMoveKeepsTheWorkspacesRepository(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	absent, err := w.data.RegisterBackupRepository(context.Background(), store.BackupRepository{Name: "Absent", URL: filepath.Join(t.TempDir(), "absent.git")})
	if err != nil {
		t.Fatal(err)
	}
	job, snap := w.moveBackup(t, "alpha", absent.ID)
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupMoveFailed {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if !strings.HasPrefix(snap.Error.Message, "Menagerie cannot write to Absent: write check: ") || !strings.HasSuffix(snap.Error.Message, "; the workspace still backs up to Backups") {
		t.Fatalf("%s", snap.Error.Message)
	}
	if want := []string{"verify"}; !reflect.DeepEqual(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	if got := backsUpTo(t, w, "alpha"); got != "backups" {
		t.Fatalf("backs up to %q", got)
	}
}

// A workspace with no recorded repository gets its first one by a move.
func TestAMoveGivesAWorkspaceWithoutARepositoryItsFirst(t *testing.T) {
	w := newWorld(t)
	folder := filepath.Join(w.root, "beta")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := backup.Init(folder, "beta"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, folder, ".toha.json", "{}\n")
	if _, err := backup.CommitRoot(folder, "beta", backup.Output{Record: ".toha.json"}, "copied in", io.Discard); err != nil {
		t.Fatal(err)
	}
	// Listing finds the folder, which has no repository.
	if status, body := w.do(t, http.MethodGet, "/api/workspaces", ""); status != http.StatusOK || !strings.Contains(body, `"backupRepository":null`) {
		t.Fatalf("%d %s", status, body)
	}
	other := w.destination(t, "Other")
	job, snap := w.moveBackup(t, "beta", other.ID)
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v\n%s", snap.Error, logText(job))
	}
	if !strings.Contains(logText(job), "giving beta its first backup repository, Other (other)") {
		t.Fatalf("log:\n%s", logText(job))
	}
	if got := backsUpTo(t, w, "beta"); got != other.ID {
		t.Fatalf("backs up to %q", got)
	}
}

func TestMoveRequestRefusals(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	for _, c := range []struct {
		body   string
		status int
		kind   string
	}{
		{`{"operation":"move-backup","workspace":"alpha"}`, http.StatusBadRequest, "invalid-request"},
		{`{"operation":"move-backup","workspace":"alpha","backupRepositoryId":"absent"}`, http.StatusNotFound, KindUnknownBackupRepository},
		{`{"operation":"move-backup","workspace":"alpha","backupRepositoryId":"backups"}`, http.StatusConflict, KindBackupRepositoryUnchanged},
		{`{"operation":"move-backup","workspace":"nowhere","backupRepositoryId":"backups"}`, http.StatusNotFound, "workspace-not-found"},
	} {
		status, body := w.do(t, http.MethodPost, "/api/jobs", c.body)
		if status != c.status || !strings.Contains(body, `"kind":"`+c.kind+`"`) {
			t.Errorf("%s: %d %s", c.body, status, body)
		}
	}
}

// hooked registers a destination on an SSH host whose Git runs hook,
// one of its server hooks, with the script given.
func (w *world) hooked(t *testing.T, hook, script string) store.BackupRepository {
	t.Helper()
	host := backuptest.New(t)
	path := filepath.Join(host.Dir, "guarded.git")
	if _, err := git.PlainInit(path, true); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "hooks", hook), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	w.backups.Auth = backup.AgentAuth(host.AgentPath, host.KnownHosts)
	r, err := w.data.RegisterBackupRepository(context.Background(), store.BackupRepository{Name: "Guarded", URL: host.URL("guarded.git")})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A destination that takes the write check's ref but refuses the
// workspace's branch, or that moves the branch once it took it, fails the
// move after its push, and the workspace keeps its repository.
func TestAMoveWhoseBackupDoesNotLandKeepsTheWorkspacesRepository(t *testing.T) {
	for _, c := range []struct {
		name, hook, script, message string
	}{
		{"refused", "pre-receive", "#!/bin/sh\nwhile read old new ref; do case \"$ref\" in refs/heads/*) echo refused >&2; exit 1;; esac; done\n",
			"pushing branch alpha to Guarded failed"},
		{"moved", "post-receive", "#!/bin/sh\nwhile read old new ref; do case \"$ref\" in refs/heads/*) tree=$(git hash-object -w -t tree /dev/null); git update-ref \"$ref\" \"$(echo elsewhere | git commit-tree \"$tree\")\";; esac; done\n",
			"branch alpha in Guarded is at "},
	} {
		t.Run(c.name, func(t *testing.T) {
			testABackupThatDoesNotLand(t, c.hook, c.script, c.message)
		})
	}
}

func testABackupThatDoesNotLand(t *testing.T, hook, script, message string) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	guarded := w.hooked(t, hook, script)
	job, snap := w.moveBackup(t, "alpha", guarded.ID)
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupMoveFailed || !strings.HasPrefix(snap.Error.Message, message) {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if want := []string{"verify", "commit", "backup"}; !reflect.DeepEqual(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	if got := backsUpTo(t, w, "alpha"); got != "backups" {
		t.Fatalf("backs up to %q", got)
	}
}
