package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

func TestPushReplacesTheRemoteBranchAndFetchesNothing(t *testing.T) {
	f := backuptest.New(t)
	remoteRepo := bare(t, f.Dir, "backups.git")
	ctx := context.Background()

	// Another history already on the remote branch, plus an unrelated branch.
	elsewhere := newWorkspace(t, "alpha")
	write(t, elsewhere, "elsewhere.txt", "pushed from elsewhere\n")
	foreign, err := CommitRoot(elsewhere, "alpha", Output{Record: "elsewhere.txt"}, "elsewhere", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	local := &Remote{URL: filepath.Join(f.Dir, "backups.git")}
	if _, err := local.Push(ctx, elsewhere, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	other := newWorkspace(t, "beta")
	write(t, other, "beta.txt", "beta\n")
	betaRoot, _ := CommitRoot(other, "beta", Output{Record: "beta.txt"}, "beta", &bytes.Buffer{})
	if _, err := local.Push(ctx, other, "beta", nil); err != nil {
		t.Fatal(err)
	}

	folder := newWorkspace(t, "alpha")
	out := generate(t, folder)
	root, err := CommitRoot(folder, "alpha", out, "Create workspace alpha", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	remote := &Remote{URL: f.URL("backups.git"), Auth: AgentAuth(f.AgentPath, f.KnownHosts)}
	if pushed, err := remote.Push(ctx, folder, "alpha", nil); err != nil || !pushed {
		t.Fatalf("push: %v %v", pushed, err)
	}
	commits := history(t, remoteRepo, "alpha")
	if len(commits) != 1 || commits[0].Hash != root.Hash {
		t.Fatalf("remote branch was not replaced by the independent root: %v", commits)
	}
	if beta := history(t, remoteRepo, "beta"); beta[0].Hash != betaRoot.Hash {
		t.Fatal("another workspace's branch changed")
	}
	repo, _ := git.PlainOpen(folder)
	for _, hash := range []plumbing.Hash{foreign.Hash, betaRoot.Hash} {
		if _, err := repo.CommitObject(hash); err == nil {
			t.Fatalf("push fetched remote commit %s", hash)
		}
	}
	refs, _ := repo.References()
	_ = refs.ForEach(func(r *plumbing.Reference) error {
		if r.Name().IsRemote() {
			t.Errorf("push left remote-tracking ref %s", r.Name())
		}
		return nil
	})
	cfg, _ := repo.Config()
	if len(cfg.Remotes) != 0 {
		t.Fatalf("push wrote remotes %v", cfg.Remotes)
	}
}

// TestPushToAMissingRepositoryFails shows a push the host cannot serve ends
// with an error, as a Git host's exit does, rather than waiting.
func TestPushToAMissingRepositoryFails(t *testing.T) {
	f := backuptest.New(t)
	folder := newWorkspace(t, "alpha")
	out := generate(t, folder)
	if _, err := CommitRoot(folder, "alpha", out, "Create workspace alpha", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// Test-only bound: the refusal arrives in milliseconds, so a push still
	// open after it is waiting on the host.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote := &Remote{URL: f.URL("missing.git"), Auth: AgentAuth(f.AgentPath, f.KnownHosts)}
	if _, err := remote.Push(ctx, folder, "alpha", nil); err == nil || ctx.Err() != nil {
		t.Fatalf("push to a missing repository: %v (context %v)", err, ctx.Err())
	}
}

func TestPushVerifiesTheHostKeyAndUsesOnlyTheAgent(t *testing.T) {
	f := backuptest.New(t)
	remoteRepo := bare(t, f.Dir, "backups.git")
	folder := newWorkspace(t, "alpha")
	if _, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	other := backuptest.New(t)
	wrongHosts := filepath.Join(t.TempDir(), "known_hosts")
	f.WriteKnownHosts(t, wrongHosts, other.HostKey)
	emptyAgent := f.ServeAgent(t, agent.NewKeyring())
	cases := []struct {
		name string
		auth Auth
		want string
	}{
		{"unknown host key", AgentAuth(f.AgentPath, wrongHosts), "key mismatch"},
		{"agent without the key", AgentAuth(emptyAgent, f.KnownHosts), "unable to authenticate"},
		{"no agent", AgentAuth(filepath.Join(t.TempDir(), "absent.sock"), f.KnownHosts), "ssh agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Remote{URL: f.URL("backups.git"), Auth: tc.auth}).Push(ctx, folder, "alpha", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if _, err := remoteRepo.Reference("refs/heads/alpha", false); err == nil {
				t.Fatal("refused push wrote the remote branch")
			}
		})
	}
}

func TestAgentAuthUsesTheDefaultKnownHostsWhenNoneAreGiven(t *testing.T) {
	f := backuptest.New(t)
	t.Setenv("SSH_KNOWN_HOSTS", f.KnownHosts)
	bare(t, f.Dir, "backups.git")
	folder := newWorkspace(t, "alpha")
	if _, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Remote{URL: f.URL("backups.git"), Auth: AgentAuth(f.AgentPath)}).Push(context.Background(), folder, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	other := backuptest.New(t)
	bare(t, other.Dir, "x.git")
	t.Setenv("SSH_KNOWN_HOSTS", filepath.Join(t.TempDir(), "absent"))
	_, err := (&Remote{URL: other.URL("x.git"), Auth: AgentAuth(other.AgentPath)}).Push(context.Background(), folder, "alpha", nil)
	if err == nil || !strings.Contains(err.Error(), "knownhosts") && !strings.Contains(err.Error(), "known hosts") {
		t.Fatalf("push to a host with no known key: %v", err)
	}
}

// AgentKeyAuth offers the Git host only the repository's key, whatever
// else the agent holds, and fails naming the key when the agent does not
// offer it.
func TestAgentKeyAuthOffersOnlyTheRepositorysKey(t *testing.T) {
	f := backuptest.New(t)
	bare(t, f.Dir, "backups.git")
	folder := newWorkspace(t, "alpha")
	if _, err := CommitRoot(folder, "alpha", generate(t, folder), "Create", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The decoy is the agent's first key, which an agent offering every key
	// offers before the repository's.
	keyring := agent.NewKeyring()
	decoy, repoKey := sampleKey(t, keyring, "sample-decoy-key"), sampleKey(t, keyring, "sample-repository-key")
	f.Authorize(repoKey)
	socket := f.ServeAgent(t, keyring)
	push := func(auth Auth) error {
		_, err := (&Remote{URL: f.URL("backups.git"), Auth: auth}).Push(ctx, folder, "alpha", nil)
		return err
	}
	// Every key, as a registration from an earlier release has it: the
	// host sees the decoy first, so the fixture records each offer.
	f.Offered()
	if err := push(AgentAuth(socket, f.KnownHosts)); err != nil {
		t.Fatal(err)
	}
	if offered := f.Offered(); len(offered) == 0 || offered[0] != ssh.FingerprintSHA256(decoy) {
		t.Fatalf("every key offered %v, not the decoy first", offered)
	}
	write(t, folder, "later.txt", "a later backup\n")
	if _, err := CommitAll(folder, "alpha", "Later", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := push(AgentKeyAuth(socket, backuptest.AuthorizedKey(repoKey), f.KnownHosts)); err != nil {
		t.Fatal(err)
	}
	offered := f.Offered()
	for _, fingerprint := range offered {
		if fingerprint != ssh.FingerprintSHA256(repoKey) {
			t.Fatalf("offered %s besides the repository's key", fingerprint)
		}
	}
	if len(offered) == 0 {
		t.Fatal("no key offered")
	}
	err := push(AgentKeyAuth(socket, backuptest.AuthorizedKey(decoy), f.KnownHosts))
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("a key the host refuses: %v", err)
	}
	for _, fingerprint := range f.Offered() {
		if fingerprint != ssh.FingerprintSHA256(decoy) {
			t.Fatalf("offered %s besides the repository's key", fingerprint)
		}
	}
	absent := sampleKey(t, agent.NewKeyring(), "sample-absent-key")
	err = push(AgentKeyAuth(socket, backuptest.AuthorizedKey(absent), f.KnownHosts))
	if err == nil || !strings.Contains(err.Error(), "does not offer the repository's key "+ssh.FingerprintSHA256(absent)) {
		t.Fatalf("a key the agent does not offer: %v", err)
	}
	if offered := f.Offered(); len(offered) != 0 {
		t.Fatalf("a key the agent does not offer reached the host: %v", offered)
	}
	if err := push(AgentKeyAuth(socket, "not a key", f.KnownHosts)); err == nil || !strings.Contains(err.Error(), "not a public key") {
		t.Fatalf("not a key: %v", err)
	}
	_, _, err = AgentKeyAuth(socket, backuptest.AuthorizedKey(repoKey))(ctx, "https://git.example.invalid/example-org/backups.git")
	var credentials *CredentialsError
	if !errors.As(err, &credentials) {
		t.Fatalf("an https URL: %v", err)
	}
}

func TestAStalledAgentFailsTheTransferInTime(t *testing.T) {
	socket := backuptest.StalledAgent(t)
	key := backuptest.AuthorizedKey(sampleKey(t, agent.NewKeyring(), "sample-repository-key"))
	url := "ssh://git@git.example.invalid/example-org/backups.git"
	// The operation's end closes the connection.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := backuptest.Within(t, 2*time.Second, func() error { _, _, err := AgentKeyAuth(socket, key)(ctx, url); return err })
	if err == nil {
		t.Fatal("a stalled agent gave the repository's key")
	}
}

// A transfer has no wait of its own: a slow agent's keys and signature
// arrive whenever the agent gives them, within the operation.
func TestATransferWaitsForASlowAgent(t *testing.T) {
	f := backuptest.New(t)
	keyring := agent.NewKeyring()
	key := sampleKey(t, keyring, "sample-repository-key")
	socket := f.ServeAgent(t, slowAgent{keyring, 2 * time.Second})
	err := backuptest.Within(t, 30*time.Second, func() error {
		method, release, err := AgentKeyAuth(socket, backuptest.AuthorizedKey(key))(context.Background(), "ssh://git@git.example.invalid/example-org/backups.git")
		if err != nil {
			return err
		}
		defer release()
		signers, err := method.(*gitssh.PublicKeysCallback).Callback()
		if err != nil {
			return err
		}
		_, err = signers[0].Sign(rand.Reader, []byte("sample data"))
		return err
	})
	if err != nil {
		t.Fatalf("a slow agent's signature: %v", err)
	}
}

// changingAgent lists its keys without hidden on the first listing only,
// as Wyrwood may when one OpenBao lookup fails.
type changingAgent struct {
	agent.Agent
	hidden ssh.PublicKey
	lists  *atomic.Int32
}

func (a changingAgent) List() ([]*agent.Key, error) {
	keys, err := a.Agent.List()
	if a.lists.Add(1) > 1 || err != nil {
		return keys, err
	}
	var shown []*agent.Key
	for _, k := range keys {
		if !bytes.Equal(k.Marshal(), a.hidden.Marshal()) {
			shown = append(shown, k)
		}
	}
	return shown, nil
}

// The refusal checks every name the listing gives the key: an alias listed
// first does not hide the operator key's own name.
func TestTheRefusalChecksEveryNameOfTheKey(t *testing.T) {
	f := backuptest.New(t)
	keyring := agent.NewKeyring()
	key := sampleKey(t, keyring, "sample-operator-key")
	socket := f.ServeAgent(t, backuptest.AliasAgent(keyring, key, "sample-alias"))
	refuse := func(k *agent.Key) error {
		if k.Comment == "sample-operator-key" {
			return errors.New("is the operator key")
		}
		return nil
	}
	_, release, err := AgentKeyAuthRefusing(socket, backuptest.AuthorizedKey(key), refuse)(context.Background(), "ssh://git@git.example.invalid/example-org/backups.git")
	if err == nil {
		release()
		t.Fatal("the transfer signed with the operator key under another name")
	}
	// A key with no refused name still signs under its alias.
	_, release, err = AgentKeyAuthRefusing(socket, backuptest.AuthorizedKey(key), func(*agent.Key) error { return nil })(context.Background(), "ssh://git@git.example.invalid/example-org/backups.git")
	if err != nil {
		t.Fatalf("a key with an alias: %v", err)
	}
	release()
}

// The refusal checks the key the transfer signs with, from the same
// listing: a key the first listing omits is not offered by a later one.
func TestTheRefusalChecksTheListingThatGivesTheSigner(t *testing.T) {
	f := backuptest.New(t)
	keyring := agent.NewKeyring()
	key := sampleKey(t, keyring, "sample-operator-key")
	socket := f.ServeAgent(t, changingAgent{keyring, key, new(atomic.Int32)})
	refuse := func(k *agent.Key) error {
		if k.Comment == "sample-operator-key" {
			return errors.New("is the operator key")
		}
		return nil
	}
	_, release, err := AgentKeyAuthRefusing(socket, backuptest.AuthorizedKey(key), refuse)(context.Background(), "ssh://git@git.example.invalid/example-org/backups.git")
	if err == nil {
		release()
		t.Fatal("the transfer signed with a key its check never saw")
	}
}

// slowAgent is an agent whose signatures each take delay, as Wyrwood's do
// while OpenBao signs.
type slowAgent struct {
	agent.Agent
	delay time.Duration
}

func (a slowAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	time.Sleep(a.delay)
	return a.Agent.Sign(key, data)
}

// A wait, as the key list gives one, bounds a request's reply alone: a connection idle between
// requests for longer than the wait still makes the next request, and a
// signature that takes most of the wait still arrives.
func TestAgentWaitBoundsEachRequestAlone(t *testing.T) {
	f := backuptest.New(t)
	keyring := agent.NewKeyring()
	key := sampleKey(t, keyring, "sample-repository-key")
	wait := 300 * time.Millisecond
	client, release, err := DialAgent(context.Background(), f.ServeAgent(t, slowAgent{keyring, wait / 2}), wait)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := client.List(); err != nil {
		t.Fatal(err)
	}
	// Idle for twice the wait, as a handshake is between listing the keys
	// and signing.
	time.Sleep(2 * wait)
	err = backuptest.Within(t, 5*time.Second, func() error {
		_, err := client.Sign(key, []byte("sample data"))
		return err
	})
	if err != nil {
		t.Fatalf("a signature after an idle wait: %v", err)
	}
}

// sampleKey adds a generated key to keyring under name and returns its
// public key.
func sampleKey(t *testing.T, keyring agent.Agent, name string) ssh.PublicKey {
	t.Helper()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Add(agent.AddedKey{PrivateKey: private, Comment: name}); err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}
