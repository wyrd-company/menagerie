//go:build templateservice

package workflows

// Regressions against the real template service: an answer whose session
// and submission exceed the bound of a job request, operator files that
// appear while an upgrade waits or just before it applies, and a conflicted
// upgrade's manual resolution. The operator-file and conflict cases adopt
// the independent review's probes.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

type interviewReply struct {
	Session  json.RawMessage
	State    string
	Document struct{ Context struct{ Template string } }
}

// startWaiting starts an operation and returns its job, waiting for
// answers, with its first reply.
func (a *accept) startWaiting(t *testing.T, operation, name, template string) (*jobs.Job, interviewReply) {
	t.Helper()
	status, body := a.post(t, "/api/jobs", fmt.Sprintf(`{"operation":%q,"workspace":%q,"templateId":%q}`, operation, name, template))
	if status != 202 {
		t.Fatalf("start: %d %s", status, body)
	}
	var snap jobs.Snapshot
	_ = json.Unmarshal([]byte(body), &snap)
	job, _ := a.manager.Get(snap.JobID)
	e, ok := nextInterview(job, 0)
	if !ok {
		t.Fatalf("ended: %+v", job.Snapshot())
	}
	waitWaiting(t, job, e.ID)
	var r interviewReply
	if err := json.Unmarshal(e.Data, &r); err != nil {
		t.Fatal(err)
	}
	return job, r
}

// answerBody is an answers request for reply's batch.
func answerBody(t *testing.T, r interviewReply, answers map[string]string) string {
	t.Helper()
	sub, err := json.Marshal(map[string]any{"template": r.Document.Context.Template, "answers": answers})
	if err != nil {
		t.Fatal(err)
	}
	return `{"session":` + string(r.Session) + `,"submission":` + string(sub) + `}`
}

// snapshot hashes every file under root outside .git folders.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func localHead(t *testing.T, folder string) string {
	t.Helper()
	repo, err := git.PlainOpen(folder)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash().String()
}

func TestASessionAboveOneMebibyteCompletesThroughTheAPI(t *testing.T) {
	a := newAccept(t, &fakeRuntime{})
	job, first := a.startWaiting(t, "create", "alpha", "flows")
	label := strings.Repeat("a", 600<<10)
	status, raw := a.post(t, "/api/jobs/"+job.ID()+"/answers", answerBody(t, first, map[string]string{"label": label}))
	if status != 200 {
		t.Fatalf("first answer: %d %.200s", status, raw)
	}
	var next interviewReply
	_ = json.Unmarshal([]byte(raw), &next)
	if next.State != "questions" {
		t.Fatalf("state %s", next.State)
	}
	waitWaiting(t, job, 0)
	body := answerBody(t, next, map[string]string{"second": strings.Repeat("b", 600<<10)})
	if len(body) <= 1<<20 {
		t.Fatalf("second answer is only %d bytes", len(body))
	}
	t.Logf("first answer value %d bytes, returned session %d bytes, second request %d bytes", len(label), len(next.Session), len(body))
	status, raw = a.post(t, "/api/jobs/"+job.ID()+"/answers", body)
	var done interviewReply
	_ = json.Unmarshal([]byte(raw), &done)
	if status != 200 || done.State != "completed" {
		t.Fatalf("second answer: %d %.200s", status, raw)
	}
	if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	history := a.history(t, "alpha")
	if len(history) != 1 || fileAt(t, history[0], "out.txt") != "label: "+label+"\n" {
		t.Fatal("the generated workspace was not backed up with the long answer")
	}
}

func TestAnOperatorFileDuringAWaitingUpgradeIsRefusedAndKept(t *testing.T) {
	rt := &fakeRuntime{}
	a := newAccept(t, rt)
	if job, _ := a.drive(t, "create", "alpha", "sample-v1", map[string]any{"label": "first"}); job.Snapshot().State != jobs.Succeeded {
		t.Fatal(job.Snapshot())
	}
	folder := filepath.Join(a.root, "alpha")
	job, r := a.startWaiting(t, "upgrade", "alpha", "sample-v2")
	head, remote := localHead(t, folder), a.history(t, "alpha")[0].Hash
	writeFile(t, folder, "notes.txt", "operator notes written while the interview waits\n")
	before := snapshot(t, folder)
	body := answerBody(t, r, map[string]string{"extra": "next"})
	if status, reply := a.post(t, "/api/jobs/"+job.ID()+"/answers", body); status != 409 || !strings.Contains(reply, "dirty-target") {
		t.Fatalf("answer: %d %s", status, reply)
	}
	if !reflect.DeepEqual(snapshot(t, folder), before) || localHead(t, folder) != head || a.history(t, "alpha")[0].Hash != remote {
		t.Fatal("the refused answer changed, committed or pushed the workspace")
	}
	waitWaiting(t, job, 0)
	if got := rt.seen(); !reflect.DeepEqual(got, []string{"up alpha"}) {
		t.Fatalf("runtime %v", got)
	}
	// Once the operator moves the file away, the same answer completes.
	if err := os.Remove(filepath.Join(folder, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if status, reply := a.post(t, "/api/jobs/"+job.ID()+"/answers", body); status != 200 {
		t.Fatalf("answer after the file is gone: %d %s", status, reply)
	}
	if snap := waitEnded(t, job); snap.State != jobs.Succeeded {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
}

// fileBeforeApply writes an operator file into the workspace just before
// the upgrade apply reaches the template service.
type fileBeforeApply struct {
	Templates
	t            *testing.T
	before       map[string]string
	head, folder string
}

func (p *fileBeforeApply) ApplyUpgrade(ctx context.Context, s templates.Selector, session json.RawMessage, hookToken string) (*templates.Applied, error) {
	p.folder = s.Target
	p.head = localHead(p.t, s.Target)
	writeFile(p.t, s.Target, "notes.txt", "operator notes written just before the apply\n")
	p.before = snapshot(p.t, s.Target)
	return p.Templates.ApplyUpgrade(ctx, s, session, hookToken)
}

func TestAnOperatorFileBeforeTheApplyIsRefusedWithoutWrites(t *testing.T) {
	rt := &fakeRuntime{}
	a := newAccept(t, rt)
	if job, _ := a.drive(t, "create", "alpha", "sample-v1", map[string]any{"label": "first"}); job.Snapshot().State != jobs.Succeeded {
		t.Fatal(job.Snapshot())
	}
	remote := a.history(t, "alpha")[0].Hash
	p := &fileBeforeApply{Templates: a.flows.opts.Templates, t: t}
	a.flows.opts.Templates = p
	job, _ := a.drive(t, "upgrade", "alpha", "sample-v2", map[string]any{"extra": "next"})
	snap := job.Snapshot()
	if snap.State != jobs.Failed || snap.Error.Kind != KindUpgradeFailed || !strings.Contains(snap.Error.Message, "dirty-target") {
		t.Fatalf("%+v %+v", snap, snap.Error)
	}
	if localHead(t, p.folder) != p.head || a.history(t, "alpha")[0].Hash != remote || !reflect.DeepEqual(snapshot(t, p.folder), p.before) {
		t.Fatal("the refused apply wrote, committed or pushed")
	}
	if got := rt.seen(); !reflect.DeepEqual(got, []string{"up alpha"}) {
		t.Fatalf("runtime %v", got)
	}
	// The failed job released the slot; once the file is gone the upgrade
	// completes.
	a.flows.opts.Templates = p.Templates
	if err := os.Remove(filepath.Join(p.folder, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if job, _ := a.drive(t, "upgrade", "alpha", "sample-v2", map[string]any{"extra": "next"}); job.Snapshot().State != jobs.Succeeded {
		t.Fatalf("%+v", job.Snapshot())
	}
}

func TestAConflictedUpgradeNeedsManualResolution(t *testing.T) {
	rt := &fakeRuntime{}
	a := newAccept(t, rt)
	if job, _ := a.drive(t, "create", "alpha", "sample-v1", map[string]any{"label": "first"}); job.Snapshot().State != jobs.Succeeded {
		t.Fatal(job.Snapshot())
	}
	folder := filepath.Join(a.root, "alpha")
	local, err := os.ReadFile(filepath.Join(folder, "local.txt"))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(local), "fifth: v1", "fifth: operator", 1)
	writeFile(t, folder, "local.txt", edited)
	job, _ := a.drive(t, "upgrade", "alpha", "sample-v2", map[string]any{"extra": "next"})
	snap := job.Snapshot()
	history := a.history(t, "alpha")
	if snap.State != jobs.Failed || snap.Error.Kind != KindMergeConflicted || len(history) != 3 {
		t.Fatalf("%+v %+v history %d", snap, snap.Error, len(history))
	}
	for _, part := range []string{
		"the upgrade is not complete",
		"which need manual resolution",
		"committed and backed up as " + history[0].Hash.String(),
		"the state before the upgrade as " + history[1].Hash.String(),
		"resolve the conflicts, then rebuild",
	} {
		if !strings.Contains(snap.Error.Message, part) {
			t.Errorf("message lacks %q: %s", part, snap.Error.Message)
		}
	}
	if fileAt(t, history[1], "local.txt") != edited || !strings.Contains(fileAt(t, history[0], "local.txt"), "<<<<<<<") {
		t.Fatal("the pre-upgrade edit or the conflict markers are missing from the backup")
	}
	if history[0].ParentHashes[0] != history[1].Hash || history[1].ParentHashes[0] != history[2].Hash || history[2].NumParents() != 0 {
		t.Fatal("the backup history lost a commit")
	}
	if !reflect.DeepEqual(rt.seen(), []string{"up alpha"}) || !strings.Contains(result(job), `"state":"failed"`) || !strings.Contains(result(job), `"outcome":"conflicted"`) {
		t.Fatal("a conflicted upgrade reported success or rebuilt")
	}
}
