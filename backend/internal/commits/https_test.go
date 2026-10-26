package commits

import (
	"context"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

const sentinel = "sentinel-workspace-repository"

// refusedTokens serves every URL with a token the host refuses; the
// token carries the sentinel.
type refusedTokens struct{}

func (refusedTokens) Serves(string) bool { return true }

func (refusedTokens) Token(context.Context, string) (string, string, error) {
	return "x-access-token", sentinel + "-token", nil
}

// TestABackupRefusedOverHTTPSNamesNeitherTheRepositoryNorTheToken: the
// job's error quotes the push error, which over https is a fixed reason.
func TestABackupRefusedOverHTTPSNamesNeitherTheRepositoryNorTheToken(t *testing.T) {
	f := newFixture(t)
	host := backuptest.NewHTTPS(t, t.TempDir(), "x-access-token", "another-token")
	remote := &backup.Remote{URL: host.URL(sentinel + ".git"), Auth: backup.TokenAuth(refusedTokens{}, nil)}
	f.commits = New(Options{Root: f.root, Remote: remote, Jobs: f.jobs})
	folder := f.workspace(t, "alpha")
	write(t, folder, "new.txt", "new\n")
	job, _, err := f.commits.Start("alpha", "chore: x")
	if err != nil {
		t.Fatal(err)
	}
	snap := wait(t, job)
	if snap.State != jobs.Failed || snap.Error.Kind != KindBackupFailed || !strings.Contains(snap.Error.Message, "did not accept the credentials") {
		t.Fatalf("job %+v %+v", snap, snap.Error)
	}
	host127 := strings.TrimPrefix(host.Server.URL, "https://")
	all, _, _ := job.EventsAfter(0)
	for _, e := range all {
		if strings.Contains(string(e.Data), sentinel) || strings.Contains(string(e.Data), host127) {
			t.Fatalf("event %s names the repository or the token: %s", e.Type, e.Data)
		}
	}
}
