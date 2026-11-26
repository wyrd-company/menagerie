package backuprepos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/gitcreds"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func host(t *testing.T) (*backuptest.Host, *Registry) {
	t.Helper()
	f := backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(f.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	return f, &Registry{Data: open(t), Credentials: &gitcreds.Source{Socket: f.AgentPath, KnownHosts: []string{f.KnownHosts}}}
}

// keyOf is the credential of the host's own agent key.
func keyOf(f *backuptest.Host) store.Credential {
	return store.Credential{Key: backuptest.AuthorizedKey(f.UserKey)}
}

// A registration checks its name, its URL and that Menagerie can write to
// the repository; a checked registration stores nothing and a saved one
// is listed.
func TestRegisterChecksTheRepositoryBeforeItRecordsIt(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	url := f.URL("backups.git")
	tried, err := r.Register(ctx, "  Example backups ", url, keyOf(f), true)
	if err != nil || tried.ID != "example-backups" || tried.Name != "Example backups" {
		t.Fatalf("%+v %v", tried, err)
	}
	if list, _ := r.Data.BackupRepositories(ctx); len(list) != 0 {
		t.Fatalf("a checked registration was stored: %+v", list)
	}
	saved, err := r.Register(ctx, "Example backups", url, keyOf(f), false)
	if err != nil || saved.ID != "example-backups" || saved.URL != url {
		t.Fatalf("%+v %v", saved, err)
	}
	if _, err := r.Register(ctx, "Again", url, keyOf(f), true); !errors.Is(err, store.ErrBackupRepositoryExists) {
		t.Fatalf("%v", err)
	}
}

func TestRegisterRefusals(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, url, check string
	}{
		{"", f.URL("backups.git"), ""},
		{strings.Repeat("n", 101), f.URL("backups.git"), ""},
		{"Two\nlines", f.URL("backups.git"), ""},
		{"Local", filepath.Join(f.Dir, "backups.git"), CheckURL},
		{"File", "file://" + filepath.Join(f.Dir, "backups.git"), CheckURL},
		{"Plain", "http://git.example.invalid/example-org/backups.git", CheckURL},
		{"Spaced", " " + f.URL("backups.git"), CheckURL},
		{"Password", "https://user:secret@git.example.invalid/example-org/backups.git", CheckURL},
		{"Absent", f.URL("absent.git"), CheckWrite},
	} {
		_, err := r.Register(ctx, c.name, c.url, keyOf(f), true)
		var check *CheckError
		var invalid *InvalidError
		switch {
		case c.check == "" && !errors.As(err, &invalid):
			t.Errorf("%q: %v", c.name, err)
		case c.check != "" && (!errors.As(err, &check) || check.Check != c.check):
			t.Errorf("%q: %v", c.name, err)
		}
	}
}

// A registration records the credential it was checked with, in the form
// a credential records it, refuses one that does not fit the URL, and
// checks writing with that credential alone: a key the host refuses fails
// though the agent's other key would be accepted.
func TestRegisterChecksWritingWithItsOwnCredential(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	url := f.URL("backups.git")
	withComment := store.Credential{Key: backuptest.AuthorizedKey(f.UserKey) + " a-comment"}
	saved, err := r.Register(ctx, "Example backups", url, withComment, false)
	if err != nil || saved.Credential != keyOf(f) {
		t.Fatalf("%+v %v", saved, err)
	}
	keyring := agent.NewKeyring()
	refused := sampleKey(t, keyring)
	r.Credentials = &gitcreds.Source{Socket: f.ServeAgent(t, keyring), KnownHosts: []string{f.KnownHosts}}
	_ = r.Data.RemoveBackupRepository(ctx, saved.ID)
	_, err = r.Register(ctx, "Unknown key", url, store.Credential{Key: backuptest.AuthorizedKey(refused)}, true)
	var check *CheckError
	if !errors.As(err, &check) || check.Check != CheckWrite {
		t.Fatalf("a key the host refuses: %v", err)
	}
	for _, c := range []store.Credential{{}, {App: "sample-org"}, {App: "a", Key: backuptest.AuthorizedKey(f.UserKey)}, {Key: "not a key"}} {
		if _, err := r.Register(ctx, "Example backups", url, c, true); !errors.As(err, &check) || check.Check != CheckCredential {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// A write check that fails names the credential the Git host refused: the
// key's fingerprint or the App's name, and for a registration from an
// earlier release, none.
func TestAFailedWriteCheckNamesItsCredential(t *testing.T) {
	f, r := host(t)
	keyring := agent.NewKeyring()
	refused := sampleKey(t, keyring)
	r.Credentials = &gitcreds.Source{Socket: f.ServeAgent(t, keyring), KnownHosts: []string{f.KnownHosts}}
	_, err := r.Register(context.Background(), "Unknown key", f.URL("backups.git"), store.Credential{Key: backuptest.AuthorizedKey(refused)}, true)
	var check *CheckError
	want := "Menagerie could not write to the repository with SSH key " + ssh.FingerprintSHA256(refused) + ": "
	if !errors.As(err, &check) || check.Check != CheckWrite || !strings.HasPrefix(check.Message, want) {
		t.Fatalf("a key the host refuses: %v", err)
	}
	if got := gitcreds.Phrase(store.Credential{App: "sample-org"}); got != " with GitHub App sample-org" {
		t.Errorf("an App: %q", got)
	}
	if got := gitcreds.Phrase(store.Credential{}); got != "" {
		t.Errorf("chosen by URL: %q", got)
	}
}

// A credential change is checked as a registration is, and recorded only
// when its checks pass and check is false.
func TestSetCredentialChecksBeforeItRecords(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	repo, err := r.Register(ctx, "Example backups", f.URL("backups.git"), keyOf(f), false)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	other := sampleKey(t, keyring)
	f.Authorize(other)
	r.Credentials = &gitcreds.Source{Socket: f.ServeAgent(t, keyring), KnownHosts: []string{f.KnownHosts}}
	otherKey := store.Credential{Key: backuptest.AuthorizedKey(other)}
	tried, err := r.SetCredential(ctx, repo.ID, otherKey, true)
	if err != nil || tried.Credential != otherKey {
		t.Fatalf("%+v %v", tried, err)
	}
	if got, _ := r.Data.BackupRepository(ctx, repo.ID); got.Credential != keyOf(f) {
		t.Fatalf("a checked change was stored: %+v", got)
	}
	// The host's own key is not in this agent: the change is refused and
	// the repository keeps its credential.
	var check *CheckError
	if _, err := r.SetCredential(ctx, repo.ID, keyOf(f), false); !errors.As(err, &check) || check.Check != CheckWrite {
		t.Fatalf("a key the agent does not offer: %v", err)
	}
	if _, err := r.SetCredential(ctx, repo.ID, store.Credential{App: "sample-org"}, false); !errors.As(err, &check) || check.Check != CheckCredential {
		t.Fatalf("an App for an SSH URL: %v", err)
	}
	if _, err := r.SetCredential(ctx, "absent", otherKey, false); !errors.Is(err, store.ErrNoBackupRepository) {
		t.Fatalf("absent: %v", err)
	}
	saved, err := r.SetCredential(ctx, repo.ID, otherKey, false)
	if err != nil || saved.Credential != otherKey {
		t.Fatalf("%+v %v", saved, err)
	}
	if got, _ := r.Data.BackupRepository(ctx, repo.ID); got.Credential != otherKey {
		t.Fatalf("not stored: %+v", got)
	}
}

// sampleKey adds a generated key to keyring and returns its public key.
func sampleKey(t *testing.T, keyring agent.Agent) ssh.PublicKey {
	t.Helper()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: private, Comment: "sample-other-key"}); err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// A workspace backs up to the repository its data names, and one with
// none fails before any transfer.
func TestPushGoesToTheWorkspacesOwnRepository(t *testing.T) {
	f, r := host(t)
	ctx := context.Background()
	repo, err := r.Register(ctx, "Example backups", f.URL("backups.git"), keyOf(f), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Data.SetWorkspaceBackupRepository(ctx, "alpha", repo.ID); err != nil {
		t.Fatal(err)
	}
	remote, err := r.For(ctx, "alpha")
	if err != nil || remote.URL != repo.URL {
		t.Fatalf("%+v %v", remote, err)
	}
	_, err = r.Push(ctx, t.TempDir(), "beta", nil)
	var none *NoRepositoryError
	if !errors.As(err, &none) || none.Workspace != "beta" {
		t.Fatalf("%v", err)
	}
}

// The import registers the configured repository once and gives it to
// every workspace that has none; a later start imports nothing, and an
// absent key registers nothing.
func TestImportTakesOverTheConfiguredRepositoryOnce(t *testing.T) {
	ctx := context.Background()
	data := open(t)
	if err := data.BeginCreate(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := data.Created(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	url := "ssh://git@git.example.invalid/example-org/workspace-backups.git"
	if err := Import(ctx, data, url, logf); err != nil {
		t.Fatal(err)
	}
	list, err := data.BackupRepositories(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	if got := list[0]; got.Name != "example-org/workspace-backups" || got.URL != url || !reflect.DeepEqual(got.Workspaces, []string{"alpha"}) {
		t.Fatalf("%+v", got)
	}
	if len(logged) != 1 {
		t.Fatalf("%q", logged)
	}
	if err := data.BeginCreate(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	if err := Import(ctx, data, "ssh://git@git.example.invalid/example-org/other.git", logf); err != nil {
		t.Fatal(err)
	}
	if list, _ := data.BackupRepositories(ctx); len(list) != 1 {
		t.Fatalf("a second start imported again: %+v", list)
	}
	if _, ok, _ := data.WorkspaceBackupRepository(ctx, "beta"); ok {
		t.Fatal("a later workspace was given the imported repository")
	}

	fresh := open(t)
	if err := Import(ctx, fresh, "", logf); err != nil {
		t.Fatal(err)
	}
	if done, _ := fresh.Imported(ctx, ImportID); !done {
		t.Fatal("an import with no key recorded no mark")
	}
	if list, _ := fresh.BackupRepositories(ctx); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

func TestImportName(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:example-org/backups.git":                                  "example-org/backups",
		"https://github.com/example-org/backups":                                  "example-org/backups",
		"ssh://git@git.example.invalid:2222/srv/backups.git":                      "srv/backups",
		"ssh://git@git.example.invalid/":                                          "Workspace backups",
		"https://git.example.invalid/example-org/backups.git?token=sentinel#frag": "example-org/backups",
	} {
		if got := ImportName(url); got != want {
			t.Errorf("%s: %q", url, got)
		}
	}
}

// A URL as a reply shows it holds no password, query or fragment.
func TestPublicURLHoldsNoCredential(t *testing.T) {
	for url, want := range map[string]string{
		"https://user:sentinel@git.example.invalid/example-org/backups.git":  "https://git.example.invalid/example-org/backups.git",
		"https://sentinel@git.example.invalid/example-org/backups.git":       "https://git.example.invalid/example-org/backups.git",
		"https://git.example.invalid/example-org/backups.git?token=sentinel": "https://git.example.invalid/example-org/backups.git",
		"https://git.example.invalid/o/b.git#sentinel":                       "https://git.example.invalid/o/b.git",
		"ssh://git:sentinel@git.example.invalid/o/b.git":                     "ssh://git@git.example.invalid/o/b.git",
		"git:sentinel@git.example.invalid:o/b.git":                           "git@git.example.invalid:o/b.git",
		"git@github.com:example-org/backups.git":                             "git@github.com:example-org/backups.git",
	} {
		if got := PublicURL(url); got != want || strings.Contains(got, "sentinel") {
			t.Errorf("%s: %q", url, got)
		}
	}
}
