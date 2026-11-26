package workflows

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// contributeRecord pushes to target a commit on the workspace's branch that
// writes record as its applied record, or removes the record when record is
// nil, as a reviewed branch that touches the record does.
func (w *world) contributeRecord(t *testing.T, name, target string, record *string) plumbing.Hash {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: w.remote, ReferenceName: plumbing.NewBranchReferenceName(name), SingleBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	switch {
	case record == nil:
		if _, err := wt.Remove(".toha.json"); err != nil {
			t.Fatal(err)
		}
	case strings.HasPrefix(*record, "link:"):
		// A record that is a symbolic link, here to a copy of the record.
		if err := os.Remove(filepath.Join(dir, ".toha.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(strings.TrimPrefix(*record, "link:"), filepath.Join(dir, ".toha.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(".toha.json"); err != nil {
			t.Fatal(err)
		}
	default:
		writeFile(t, dir, ".toha.json", *record)
		if _, err := wt.Add(".toha.json"); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "Contributor", Email: "contributor@example.invalid", When: time.Now()}
	hash, err := wt.Commit("Change the applied record", &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	spec := config.RefSpec(fmt.Sprintf("+%s:%s", plumbing.NewBranchReferenceName(name), plumbing.NewBranchReferenceName(target)))
	if err := repo.Push(&git.PushOptions{RefSpecs: []config.RefSpec{spec}}); err != nil {
		t.Fatal(err)
	}
	return hash
}

// toha is an applied record of the template at source, ref and commit ("" for
// none).
func toha(source, ref, commit string) *string {
	c := "null"
	if commit != "" {
		c = fmt.Sprintf("%q", commit)
	}
	record := fmt.Sprintf(`{"applications":[{"target":".","template":%q,"source":%q,"commit":%s}]}`, source+"@"+ref, source, c)
	return &record
}

const (
	appliedCommit = "1000000000000000000000000000000000000001"
	laterCommit   = "1100000000000000000000000000000000000011"
	untagged      = "7770000000000000000000000000000000000777"
)

// Apply branch refuses, as not-applicable with nothing changed, each
// planned merge that would move a workspace's applied record backward or
// past what Menagerie can check, naming both versions or commits; a record
// that moves forward is applied and its version and commit recorded.
func TestApplyBranchMovesTheAppliedRecordOnlyForward(t *testing.T) {
	src := sampleTemplate.Source
	cases := []struct {
		name string
		// prepare changes the world after the create, before the apply.
		prepare func(t *testing.T, w *world)
		record  *string
		// refused names what the refusal must name; nil applies.
		refused []string
		// want is the applied version after an apply.
		want store.AppliedVersion
	}{
		{name: "older version", record: toha(src, "0.9.0", "0900000000000000000000000000000000000009"),
			refused: []string{"version 0.9.0", "version 1.0.0", "older"}},
		{name: "older v-prefixed version", record: toha(src, "v0.9.0", "0900000000000000000000000000000000000009"),
			refused: []string{"version 0.9.0", "version 1.0.0", "older"}},
		{name: "removed record", record: nil, refused: []string{"removes the applied record", "version 1.0.0"}},
		{name: "unreadable record", record: ptr(`{"applications": not json`), refused: []string{"Toha cannot read", "version 1.0.0"}},
		{name: "no application at the root", record: ptr(`{"applications":[]}`), refused: []string{"Toha cannot read"}},
		{name: "a link for a record", record: ptr("link:result.txt"), refused: []string{"not a file", "version 1.0.0"}},
		{name: "another source", record: toha("https://git.example.invalid/example-org/other", "2.0.0", ""),
			refused: []string{"another template", "template sample"}},
		{name: "no version tag", record: toha(src, "main", "abc0000000000000000000000000000000000abc"),
			refused: []string{`"main"`, "abc0000000000000000000000000000000000abc", "no version tag", "version 1.0.0"}},
		{name: "no registered template", prepare: func(t *testing.T, w *world) { w.untemplated(t, "alpha") },
			record: toha(src, "2.0.0", laterCommit), refused: []string{"no registered template"}},
		{name: "unknown version off the history", prepare: func(t *testing.T, w *world) {
			w.unknown(t, "alpha")
			w.tmpl.checkSince = func(_, ref, since string) (*templates.Checked, error) {
				return &templates.Checked{Source: src, Commit: laterCommit, Definition: true, Descends: ptr(false)}, nil
			}
		}, record: toha(src, "1.1.0", laterCommit), refused: []string{laterCommit, appliedCommit, "does not descend"}},
		{name: "unknown version, tag elsewhere", prepare: func(t *testing.T, w *world) {
			w.unknown(t, "alpha")
			w.tmpl.checkSince = func(_, ref, since string) (*templates.Checked, error) {
				return &templates.Checked{Source: src, Commit: "1200000000000000000000000000000000000012", Definition: true, Descends: ptr(true)}, nil
			}
		}, record: toha(src, "1.1.0", laterCommit), refused: []string{laterCommit, "1200000000000000000000000000000000000012"}},
		{name: "unknown version and commit", prepare: func(t *testing.T, w *world) {
			if err := w.data.SetAppliedVersion(context.Background(), "alpha", store.AppliedVersion{}); err != nil {
				t.Fatal(err)
			}
		}, record: toha(src, "1.1.0", laterCommit), refused: []string{"not known"}},
		{name: "unknown version, record with no commit", prepare: func(t *testing.T, w *world) { w.unknown(t, "alpha") },
			record: toha(src, "1.1.0", ""), refused: []string{`commit ""`, "not a full Git object id"}},
		{name: "a version no tag gives", record: toha(src, "v9.9.9", untagged),
			refused: []string{`"v9.9.9"`, untagged, "no version tag"}},
		{name: "a version-like ref at an untagged commit", record: toha(src, "1.1.0", untagged),
			refused: []string{`"1.1.0"`, untagged, "no version tag"}},
		{name: "a branch at an untagged commit while versions are found", record: toha(src, "main", untagged),
			refused: []string{`"main"`, untagged, "no version tag"}},
		{name: "a found version at another version's commit", record: toha(src, "1.1.0", "0900000000000000000000000000000000000009"),
			refused: []string{`"1.1.0"`, "version 1.1.0", "gives 0.9.0"}},
		{name: "a found version at the current version's commit", record: toha(src, "1.1.0", appliedCommit),
			refused: []string{`"1.1.0"`, "gives 1.0.0"}},
		{name: "a commit that writes a line", record: toha(src, "1.1.0", "not-a-commit\nfake console line"),
			refused: []string{`"not-a-commit\nfake console line"`, "not a full Git object id"}},
		{name: "a moved current version", prepare: func(t *testing.T, w *world) {
			moved := sampleVersions[:]
			moved = append([]store.Version{}, moved...)
			moved[1].Commit = "1b00000000000000000000000000000000000001"
			if _, err := w.data.RecordCheck(context.Background(), "sample", 0, moved, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, record: toha(src, "1.0.0", "1b00000000000000000000000000000000000001"), refused: []string{"version 1.0.0", "moved"}},
		{name: "a record larger than the bound", prepare: func(t *testing.T, w *world) {
			before := recordLimit
			recordLimit = 64
			t.Cleanup(func() { recordLimit = before })
		}, record: toha(src, "1.1.0", laterCommit), refused: []string{"larger than 64 bytes"}},
		{name: "same version", record: toha(src, "1.0.0", appliedCommit), want: store.AppliedVersion{Version: "1.0.0", Commit: appliedCommit}},
		{name: "newer version", record: toha(src, "1.1.0", laterCommit), want: store.AppliedVersion{Version: "1.1.0", Commit: laterCommit}},
		{name: "version at its commit", prepare: func(t *testing.T, w *world) {
			if _, err := w.data.RecordCheck(context.Background(), "sample", 0, []store.Version{
				{Version: "1.0.0", Tag: "1.0.0", Commit: appliedCommit}, {Version: "1.1.0", Tag: "1.1.0", Commit: laterCommit},
			}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, record: toha(src, "main", laterCommit), want: store.AppliedVersion{Version: "1.1.0", Commit: laterCommit}},
		{name: "unknown version that descends", prepare: func(t *testing.T, w *world) {
			w.unknown(t, "alpha")
			w.tmpl.checkSince = func(_, ref, since string) (*templates.Checked, error) {
				if ref != "1.1.0" || since != appliedCommit {
					return nil, errors.New("asked " + ref + " since " + since)
				}
				return &templates.Checked{Source: src, Commit: laterCommit, Definition: true, Descends: ptr(true)}, nil
			}
		}, record: toha(src, "1.1.0", laterCommit), want: store.AppliedVersion{Version: "1.1.0", Commit: laterCommit}},
		{name: "unknown version, a branch at a tagged commit asks by the tag", prepare: func(t *testing.T, w *world) {
			w.unknown(t, "alpha")
			w.tmpl.checkSince = func(_, ref, since string) (*templates.Checked, error) {
				if ref != "v2.0.0" || since != appliedCommit {
					return nil, errors.New("asked " + ref + " since " + since)
				}
				return &templates.Checked{Source: src, Commit: "2000000000000000000000000000000000000002", Definition: true, Descends: ptr(true)}, nil
			}
		}, record: toha(src, "main", "2000000000000000000000000000000000000002"),
			want: store.AppliedVersion{Version: "2.0.0", Commit: "2000000000000000000000000000000000000002"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.createWorkspace(t, "alpha")
			folder := filepath.Join(w.root, "alpha")
			if tc.prepare != nil {
				tc.prepare(t, w)
			}
			ctx := context.Background()
			before, _ := w.data.AppliedVersion(ctx, "alpha")
			record, _ := os.ReadFile(filepath.Join(folder, ".toha.json"))
			pushed := w.contributeRecord(t, "alpha", "review/record", tc.record)
			job, snap := w.applyBranch(t, "alpha", "review/record")
			after, _ := w.data.AppliedVersion(ctx, "alpha")
			now, _ := os.ReadFile(filepath.Join(folder, ".toha.json"))
			if tc.refused == nil {
				if snap.State != jobs.Succeeded {
					t.Fatalf("a forward record was not applied: the job %s: %+v\n%s", snap.State, snap.Error, logText(job))
				}
				if after != tc.want {
					t.Fatalf("applied %+v, want %+v", after, tc.want)
				}
				if tc.record != nil && string(now) != *tc.record {
					t.Fatalf("the folder's record is %s", now)
				}
				return
			}
			if snap.State != jobs.Failed || snap.Error.Kind != KindNotApplicable {
				t.Fatalf("the record was not refused as not-applicable: the job %s: %+v\n%s", snap.State, snap.Error, logText(job))
			}
			for _, named := range tc.refused {
				if !strings.Contains(snap.Error.Message, named) {
					t.Errorf("the refusal does not name %s: %s", named, snap.Error.Message)
				}
			}
			for _, line := range strings.Split(logText(job), "\n") {
				if strings.TrimSpace(line) == "fake console line" {
					t.Errorf("a record's value wrote a console line")
				}
			}
			if !strings.Contains(snap.Error.Message, "[paths: .toha.json]") {
				t.Errorf("the refusal does not name the record: %s", snap.Error.Message)
			}
			// Nothing changed: the record, the applied version, the
			// devcontainer and the workspace branch.
			if string(now) != string(record) {
				t.Errorf("the folder's record changed to %s", now)
			}
			if after != before {
				t.Errorf("the applied version changed from %+v to %+v", before, after)
			}
			if got := w.rt.seen(); len(got) != 1 || got[0] != "up alpha" {
				t.Errorf("runtime %v", got)
			}
			if history := w.remoteHistory(t, "alpha"); history[0].Hash == pushed {
				t.Errorf("the workspace branch took the refused commit")
			}
			w.fetchedRefGone(t, "alpha")
		})
	}
}

// unknown makes the workspace's applied version unknown, at the create's
// commit, as a record whose ref is no version gives.
func (w *world) unknown(t *testing.T, name string) {
	t.Helper()
	if err := w.data.SetAppliedVersion(context.Background(), name, store.AppliedVersion{Commit: appliedCommit}); err != nil {
		t.Fatal(err)
	}
}

// untemplated leaves the workspace with no registered template, as one
// whose applied record names a source no template has.
func (w *world) untemplated(t *testing.T, name string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+w.data.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE workspaces SET template = NULL WHERE name = ?`, name); err != nil {
		t.Fatal(err)
	}
}

// While the template service cannot say whether a record moves an unknown
// applied version forward, the apply fails as template-service-unavailable
// with nothing applied.
func TestApplyBranchWithNoAnswerOnDescentAppliesNothing(t *testing.T) {
	w := newWorld(t)
	w.createWorkspace(t, "alpha")
	folder := filepath.Join(w.root, "alpha")
	w.unknown(t, "alpha")
	w.tmpl.checkSince = func(_, _, _ string) (*templates.Checked, error) {
		return nil, &templates.UnavailableError{Err: errors.New("no socket")}
	}
	record, _ := os.ReadFile(filepath.Join(folder, ".toha.json"))
	w.contributeRecord(t, "alpha", "review/record", toha(sampleTemplate.Source, "1.1.0", laterCommit))
	job, snap := w.applyBranch(t, "alpha", "review/record")
	if snap.State != jobs.Failed || snap.Error.Kind != KindTemplateUnavailable {
		t.Fatalf("%+v %+v\n%s", snap, snap.Error, logText(job))
	}
	if now, _ := os.ReadFile(filepath.Join(folder, ".toha.json")); string(now) != string(record) {
		t.Fatalf("the record changed to %s", now)
	}
	if got, _ := w.data.AppliedVersion(context.Background(), "alpha"); got != (store.AppliedVersion{Commit: appliedCommit}) {
		t.Fatalf("%+v", got)
	}
}
