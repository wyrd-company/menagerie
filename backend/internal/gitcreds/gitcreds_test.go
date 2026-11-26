package gitcreds

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/githubapp"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

type keySigner struct{ key *rsa.PrivateKey }

func (s keySigner) Sign(_ context.Context, input []byte) ([]byte, error) {
	sum := sha256.Sum256(input)
	return rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
}

// sampleApp is an App named name on a TLS fake of a GitHub server whose
// tokens are sample-token-{name}.
func sampleApp(t *testing.T, name string) githubapp.App {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	github := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"sample-token-%s","expires_at":%q}`, name, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(github.Close)
	m, err := githubapp.New(githubapp.Options{AppID: 1, Signer: keySigner{key}, Installations: map[string]int64{"example-org": 2}, Server: github.URL, Client: github.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return githubapp.App{Name: name, Server: github.URL, Minter: m}
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

// The key list is the agent's keys, each with its name, fingerprint and
// public key, without provisioning's operator key however it is selected.
func TestKeysListsTheAgentsKeysWithoutTheOperatorKey(t *testing.T) {
	f := backuptest.New(t)
	keyring := agent.NewKeyring()
	git := sampleKey(t, keyring, "sample-git-key")
	operator := sampleKey(t, keyring, "sample-operator-key")
	socket := f.ServeAgent(t, keyring)
	for _, excluded := range []string{"sample-operator-key", ssh.FingerprintSHA256(operator), authorized(operator), authorized(operator) + " a-comment"} {
		keys, err := (&Source{Socket: socket, Excluded: excluded}).Keys(context.Background())
		want := Key{Name: "sample-git-key", Fingerprint: ssh.FingerprintSHA256(git), PublicKey: authorized(git)}
		if err != nil || len(keys) != 1 || keys[0] != want {
			t.Fatalf("excluded %q: %+v %v", excluded, keys, err)
		}
	}
	keys, err := (&Source{Socket: socket}).Keys(context.Background())
	if err != nil || len(keys) != 2 || keys[1].Name != "sample-operator-key" {
		t.Fatalf("none excluded: %+v %v", keys, err)
	}
	empty, err := (&Source{Socket: f.ServeAgent(t, agent.NewKeyring())}).Keys(context.Background())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("an empty agent: %#v %v", empty, err)
	}
	if _, err := (&Source{Socket: filepath.Join(t.TempDir(), "absent.sock")}).Keys(context.Background()); err == nil || err.Error() != "the SSH agent could not be reached" {
		t.Fatalf("no agent: %v", err)
	}
}

func TestNormalKeyDropsTheComment(t *testing.T) {
	key := sampleKey(t, agent.NewKeyring(), "k")
	for _, given := range []string{authorized(key), authorized(key) + " some comment", " " + authorized(key) + "\n"} {
		if got, ok := NormalKey(given); !ok || got != authorized(key) {
			t.Fatalf("%q: %q %v", given, got, ok)
		}
	}
	for _, given := range []string{"", "not a key", authorized(key) + "\n" + authorized(key)} {
		if _, ok := NormalKey(given); ok {
			t.Fatalf("%q was taken as a key", given)
		}
	}
}

// Fit refuses a credential of the wrong kind for the URL, a missing one
// where one fits, an App that does not serve the URL or is not configured,
// and the operator key.
func TestFitRefusesACredentialThatDoesNotFitTheURL(t *testing.T) {
	first, second := sampleApp(t, "first-org"), sampleApp(t, "second-org")
	keyring := agent.NewKeyring()
	key := authorized(sampleKey(t, keyring, "sample-git-key"))
	operator := sampleKey(t, keyring, "sample-operator-key")
	s := &Source{Apps: githubapp.Apps{first, second}, Socket: backuptest.New(t).ServeAgent(t, keyring), Excluded: "sample-operator-key"}
	sshURL := "git@git.example.invalid:example-org/backups.git"
	onFirst := first.Server + "/example-org/backups.git"
	elsewhere := "https://elsewhere.example.invalid/example-org/backups.git"
	cases := []struct {
		name, url string
		c         store.Credential
		want      string
	}{
		{"ssh key", sshURL, store.Credential{Key: key}, ""},
		{"ssh none", sshURL, store.Credential{}, "choose the SSH key"},
		{"ssh app", sshURL, store.Credential{App: "first-org"}, "an SSH URL takes an SSH key"},
		{"ssh not a key", sshURL, store.Credential{Key: "ssh-ed25519 nonsense"}, "not a public key"},
		{"ssh operator key", sshURL, store.Credential{Key: authorized(operator)}, "provisioning's operator key"},
		{"https app", onFirst, store.Credential{App: "first-org"}, ""},
		{"https app any case", onFirst, store.Credential{App: "FIRST-ORG"}, ""},
		{"https none on a server", onFirst, store.Credential{}, "choose the GitHub App for the repository: first-org"},
		{"https another server's app", onFirst, store.Credential{App: "second-org"}, "GitHub App second-org does not serve the URL"},
		{"https unknown app", onFirst, store.Credential{App: "absent-org"}, "no GitHub App named absent-org is configured"},
		{"https key", onFirst, store.Credential{Key: key}, "an https URL takes a GitHub App"},
		{"https elsewhere none", elsewhere, store.Credential{}, ""},
		{"https elsewhere app", elsewhere, store.Credential{App: "first-org"}, "does not serve the URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Fit(context.Background(), tc.url, tc.c)
			var fit *FitError
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (!errors.As(err, &fit) || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want %q", err, tc.want)
			case err != nil && (strings.Contains(err.Error(), "backups.git") || strings.Contains(err.Error(), "elsewhere")):
				t.Fatalf("the refusal names the URL: %v", err)
			}
		})
	}
}

// An App credential sends only its own App's token, and refuses a URL it
// does not serve and an App the configuration no longer names; the zero
// credential keeps the first App that serves the URL, and the agent for
// SSH.
func TestAuthUsesOnlyTheCredentialsApp(t *testing.T) {
	first, second := sampleApp(t, "first-org"), sampleApp(t, "second-org")
	f := backuptest.New(t)
	s := &Source{Apps: githubapp.Apps{first, second}, Socket: f.AgentPath}
	ctx := context.Background()
	token := func(c store.Credential, url string) (string, error) {
		auth, release, err := s.Auth(c)(ctx, url)
		if err != nil {
			return "", err
		}
		release()
		basic, ok := auth.(*githttp.BasicAuth)
		if !ok {
			return fmt.Sprintf("%T", auth), nil
		}
		return basic.Password, nil
	}
	onSecond := second.Server + "/example-org/backups.git"
	if got, err := token(store.Credential{App: "second-org"}, onSecond); err != nil || got != "sample-token-second-org" {
		t.Fatalf("its own App: %q %v", got, err)
	}
	var credentials *backup.CredentialsError
	if _, err := token(store.Credential{App: "first-org"}, onSecond); !errors.As(err, &credentials) || !strings.Contains(err.Error(), "GitHub App first-org does not serve the repository's URL") {
		t.Fatalf("an App that does not serve the URL: %v", err)
	}
	if _, err := token(store.Credential{App: "absent-org"}, onSecond); !errors.As(err, &credentials) || !strings.Contains(err.Error(), "no GitHub App named absent-org is configured") {
		t.Fatalf("an App no longer configured: %v", err)
	}
	if got, err := token(store.Credential{}, onSecond); err != nil || got != "sample-token-second-org" {
		t.Fatalf("the earlier rule: %q %v", got, err)
	}
	if got, err := token(store.Credential{}, "https://elsewhere.example.invalid/example-org/backups.git"); err != nil || got != "<nil>" {
		t.Fatalf("the earlier rule off the Apps' servers: %q %v", got, err)
	}
	if got, err := token(store.Credential{}, f.URL("backups.git")); err != nil || got != fmt.Sprintf("%T", &gitssh.PublicKeysCallback{}) {
		t.Fatalf("the earlier rule over SSH: %q %v", got, err)
	}
	if got, err := token(store.Credential{Key: authorized(f.UserKey)}, f.URL("backups.git")); err != nil || got != fmt.Sprintf("%T", &gitssh.PublicKeysCallback{}) {
		t.Fatalf("a key: %q %v", got, err)
	}
}

func TestAStalledAgentFailsDiscoveryAndTheFitInTime(t *testing.T) {
	s := &Source{Socket: backuptest.StalledAgent(t), Excluded: "provisioning.wyrwood.operator-key", ListWait: 50 * time.Millisecond}
	key := backuptest.AuthorizedKey(sampleKey(t, agent.NewKeyring(), "sample-git-key"))
	ctx := context.Background()
	err := backuptest.Within(t, 2*time.Second, func() error { _, err := s.Keys(ctx); return err })
	if err == nil || err.Error() != "the SSH agent did not list its keys" {
		t.Fatalf("keys: %v", err)
	}
	// The key may be the operator key, which only the agent's list names.
	err = backuptest.Within(t, 2*time.Second, func() error {
		return s.Fit(ctx, "git@git.example.test:example-org/backups.git", store.Credential{Key: key})
	})
	var fit *FitError
	if !errors.As(err, &fit) || !strings.Contains(fit.Message, "could not check that the SSH key is not provisioning's operator key") {
		t.Fatalf("fit: %v", err)
	}
}

// A saved key that the configuration has since excluded, as provisioning's
// operator key, is refused at each transfer, before any connection, by
// its name in the agent; the repository's other keys still reach the host.
func TestAuthRefusesASavedKeyThatIsNowTheOperatorKey(t *testing.T) {
	f := backuptest.New(t)
	keyring := agent.NewKeyring()
	git := sampleKey(t, keyring, "sample-git-key")
	operator := sampleKey(t, keyring, "sample-operator-key")
	s := &Source{Socket: f.ServeAgent(t, keyring), Excluded: "sample-operator-key"}
	url := "ssh://git@git.example.invalid/example-org/backups.git"
	_, _, err := s.Auth(store.Credential{Key: authorized(operator)})(context.Background(), url)
	if err == nil || !strings.Contains(err.Error(), "the repository's key "+ssh.FingerprintSHA256(operator)+" is provisioning's operator key") {
		t.Fatalf("the operator key: %v", err)
	}
	_, release, err := s.Auth(store.Credential{Key: authorized(git)})(context.Background(), url)
	if err != nil {
		t.Fatalf("a Git key: %v", err)
	}
	release()
}

// The service reads each URL form in the shared table as the backups
// screen's picker does; web/test/contract/credential-urls.test.ts reads the same
// table.
func TestTheServiceReadsURLFormsAsThePickerDoes(t *testing.T) {
	data, err := os.ReadFile("../../../docs/specifications/assets/workspace-api/credential-url-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Server string `json:"server"`
		Cases  []struct {
			URL    string  `json:"url"`
			Server string  `json:"server"`
			Kind   *string `json:"kind"`
			Served bool    `json:"served"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	for _, c := range table.Cases {
		server := c.Server
		if server == "" {
			server = table.Server
		}
		m, err := githubapp.New(githubapp.Options{AppID: 1, Signer: keySigner{}, Server: server})
		if err != nil {
			t.Fatalf("%s: %v", server, err)
		}
		apps := githubapp.Apps{{Name: "sample-org", Server: server, Minter: m}}
		kind := ""
		if e, err := transport.NewEndpoint(c.URL); err == nil && (e.Protocol == "ssh" || e.Protocol == "https") {
			kind = e.Protocol
		}
		want := ""
		if c.Kind != nil {
			want = *c.Kind
		}
		if kind != want {
			t.Errorf("%s: kind %q, the table says %q", c.URL, kind, want)
		}
		if _, _, served := apps.Serving(c.URL); served != c.Served {
			t.Errorf("%s: served %v, the table says %v", c.URL, served, c.Served)
		}
	}
}
