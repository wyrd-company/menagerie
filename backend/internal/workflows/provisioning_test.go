package workflows

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
)

func (w *world) create(t *testing.T, name string) (*jobs.Job, jobs.Snapshot) {
	t.Helper()
	job := w.startJob(t, "create", name, "sample-v1")
	waitWaiting(t, job, 0)
	w.answer(t, job, sessionAsking)
	return job, waitEnded(t, job)
}

func (w *world) start(t *testing.T, name string) (*jobs.Job, jobs.Snapshot) {
	t.Helper()
	status, body := w.do(t, http.MethodPost, "/api/jobs", fmt.Sprintf(`{"operation":"start","workspace":%q}`, name))
	if status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, body)
	}
	id := body[strings.Index(body, `"jobId":"`)+len(`"jobId":"`):]
	job, ok := w.manager.Get(id[:strings.Index(id, `"`)])
	if !ok {
		t.Fatalf("no job in %s", body)
	}
	return job, waitEnded(t, job)
}

func (w *world) remoteBranches(t *testing.T) map[string]plumbing.Hash {
	t.Helper()
	bare, err := git.PlainOpen(w.remote)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := bare.Branches()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]plumbing.Hash{}
	_ = refs.ForEach(func(r *plumbing.Reference) error {
		out[r.Name().Short()] = r.Hash()
		return nil
	})
	return out
}

// A failed check leaves an applied record and no commit, and the workspace's
// start then finishes the create.
func TestAStartFinishesACreateThatFailedAfterGeneration(t *testing.T) {
	w := newWorld(t)
	w.rt.failOn = map[string]error{"host-prerequisites": &devcontainer.HostPrerequisitesError{Networks: []string{"absent"}}}
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Failed {
		t.Fatalf("%+v", snap)
	}
	if got := w.remoteBranches(t); len(got) != 0 {
		t.Fatalf("branches %v", got)
	}
	w.rt.failOn = nil
	job, snap := w.start(t, "alpha")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if want := []string{"host-prerequisites", "commit", "backup", "up"}; !slices.Equal(phases(job), want) {
		t.Fatalf("phases %v", phases(job))
	}
	if _, ok := w.remoteBranches(t)["alpha"]; !ok {
		t.Error("no backup")
	}
	if got := w.rt.seen(); !slices.Equal(got, []string{"up alpha"}) {
		t.Errorf("runtime %v", got)
	}
	// With the commit in place, a further start only starts.
	job, snap = w.start(t, "alpha")
	if snap.State != jobs.Succeeded || !slices.Equal(phases(job), []string{"up"}) {
		t.Fatalf("%+v %v", snap, phases(job))
	}
}

func TestAStartOfANewOrRefusedNameKeepsItsAnswers(t *testing.T) {
	w := newWorld(t)
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"start","workspace":"absent"}`); status != http.StatusNotFound || !strings.Contains(body, "workspace-not-found") {
		t.Errorf("%d %s", status, body)
	}
	if status, body := w.do(t, http.MethodPost, "/api/jobs", `{"operation":"start","workspace":".dot"}`); status != http.StatusBadRequest || !strings.Contains(body, "invalid-workspace-name") {
		t.Errorf("%d %s", status, body)
	}
}

func TestAStartFailureAfterItsSaveNamesTheCommit(t *testing.T) {
	w := newWorld(t)
	w.rt.failOn = map[string]error{"up": errors.New("no start")}
	_, snap := w.create(t, "alpha")
	if snap.State != jobs.Failed || !strings.Contains(snap.Error.Message, "generated and backed up as") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

func TestACreateKeepsTheBackupOfADeletedWorkspace(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	// A commit moved by hand: two creates in one second give one root commit.
	first := w.contribute(t, "alpha", "alpha", map[string]string{"file": "content\n"})
	if err := os.RemoveAll(filepath.Join(w.root, "alpha")); err != nil {
		t.Fatal(err)
	}
	job, snap := w.create(t, "alpha")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	branches := w.remoteBranches(t)
	if branches["alpha"] == first || branches["alpha"].IsZero() {
		t.Errorf("branch %s, first %s", branches["alpha"], first)
	}
	var archive string
	for name, hash := range branches {
		if strings.HasPrefix(name, backup.ArchivePrefix+"alpha-") && hash == first {
			archive = name
		}
	}
	if archive == "" {
		t.Fatalf("no archive of %s in %v", first, branches)
	}
	if logs := strings.Join(events(job, "log"), "\n"); !strings.Contains(logs, archive) {
		t.Errorf("the console does not name %s: %s", archive, logs)
	}
}

func TestAnUpgradeAndAnApplyRetireBeforeTheyChangeTheDefinition(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	if _, snap := w.upgraded(t); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if !slices.Equal(w.rt.retired, []string{"alpha"}) {
		t.Fatalf("retired %v", w.rt.retired)
	}
	w.rt.retired = nil
	w.contribute(t, "alpha", "feature", map[string]string{"file": "content\n"})
	if _, snap := w.applyBranch(t, "alpha", "feature"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if !slices.Equal(w.rt.retired, []string{"alpha"}) {
		t.Fatalf("retired %v", w.rt.retired)
	}
}

func TestARetireThatFailsChangesNothing(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	w.rt.failOn = map[string]error{"retire": errors.New("no record")}
	if _, snap := w.upgraded(t); snap.State != jobs.Failed || !strings.Contains(snap.Error.Message, "nothing was upgraded") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	w.contribute(t, "alpha", "feature", map[string]string{"file": "content\n"})
	if _, snap := w.applyBranch(t, "alpha", "feature"); snap.State != jobs.Failed || !strings.Contains(snap.Error.Message, "nothing was applied") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

// A verify or retire that fails comes after the start, so no message says that
// the devcontainer did not start or that it is stopped.
func TestAFailureAfterTheStartIsNotAFailedStart(t *testing.T) {
	failed := &provisioning.AfterStartError{Step: "a later step", Err: errors.New("refused")}
	w := newWorld(t)
	w.rt.failOn = map[string]error{"up": failed}
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Failed || strings.Contains(snap.Error.Message, "did not start") || !strings.Contains(snap.Error.Message, "the devcontainer started") {
		t.Fatalf("create: %+v %+v", snap, snap.Error)
	}
	w.rt.failOn = map[string]error{"rebuild": failed}
	if _, snap := w.upgraded(t); snap.State != jobs.Failed || strings.Contains(snap.Error.Message, "did not rebuild") || !strings.Contains(snap.Error.Message, "the devcontainer started") {
		t.Fatalf("upgrade: %+v %+v", snap, snap.Error)
	}
	w.rt.failOn = map[string]error{"rebuild": failed}
	w.contribute(t, "alpha", "feature", map[string]string{"file": "content\n"})
	job, snap := w.applyBranch(t, "alpha", "feature")
	if snap.State != jobs.Failed || strings.Contains(snap.Error.Message, "did not rebuild") || !strings.Contains(snap.Error.Message, "the devcontainer started") {
		t.Fatalf("apply: %+v %+v", snap, snap.Error)
	}
	if strings.Contains(strings.Join(events(job, "log"), "\n"), StoppedNotice) {
		t.Error("the console says the devcontainer is stopped")
	}
}

// breakRemote makes every transfer to the workspace repository fail, and
// returns the function that repairs it.
func (w *world) breakRemote(t *testing.T) func() {
	t.Helper()
	aside := w.remote + ".aside"
	if err := os.Rename(w.remote, aside); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Rename(aside, w.remote); err != nil {
			t.Fatal(err)
		}
	}
}

// A backup that fails after the root commit still owes the archive, and no
// later job's push may replace the branch without it.
func TestAFailedArchiveIsNotLostToALaterPush(t *testing.T) {
	w := newWorld(t)
	if _, snap := w.create(t, "alpha"); snap.State != jobs.Succeeded {
		t.Fatalf("%+v", snap)
	}
	first := w.contribute(t, "alpha", "alpha", map[string]string{"file": "content\n"})
	if err := os.RemoveAll(filepath.Join(w.root, "alpha")); err != nil {
		t.Fatal(err)
	}
	job := w.startJob(t, "create", "alpha", "sample-v1")
	waitWaiting(t, job, 0)
	repair := w.breakRemote(t)
	w.answer(t, job, sessionAsking)
	snap := waitEnded(t, job)
	repair()
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupFailed {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if got := w.remoteBranches(t)["alpha"]; got != first {
		t.Fatalf("branch %s, want %s", got, first)
	}
	retry, snap := w.start(t, "alpha")
	if snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if slices.Contains(phases(retry), "commit") {
		t.Errorf("committed again: %v", phases(retry))
	}
	branches := w.remoteBranches(t)
	found := false
	for name, hash := range branches {
		found = found || (strings.HasPrefix(name, backup.ArchivePrefix+"alpha-") && hash == first)
	}
	if !found || branches["alpha"] == first {
		t.Fatalf("branches %v, first %s", branches, first)
	}
}
