package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

// A write check over SSH writes its ref and removes it: the repository
// keeps no ref of it, and an empty repository stays empty.
func TestCheckWriteLeavesTheRepositoryAsItWas(t *testing.T) {
	f := backuptest.New(t)
	repo := bare(t, f.Dir, "backups.git")
	remote := &Remote{URL: f.URL("backups.git"), Auth: AgentAuth(f.AgentPath, f.KnownHosts)}
	if err := remote.CheckWrite(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	refs, err := repo.References()
	if err != nil {
		t.Fatal(err)
	}
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference {
			t.Errorf("the check left %s", ref.Name())
		}
		return nil
	})
	pushes := 0
	for _, c := range f.Commands() {
		if strings.Contains(c, "git-receive-pack") {
			pushes++
		}
	}
	if pushes != 2 {
		t.Fatalf("commands %q", f.Commands())
	}
}

// A repository Menagerie cannot write to fails the check.
func TestCheckWriteFailsWhereNothingCanBeWritten(t *testing.T) {
	remote := &Remote{URL: filepath.Join(t.TempDir(), "absent.git")}
	if err := remote.CheckWrite(context.Background(), nil); err == nil || !strings.HasPrefix(err.Error(), "write check: ") {
		t.Fatalf("%v", err)
	}
}
