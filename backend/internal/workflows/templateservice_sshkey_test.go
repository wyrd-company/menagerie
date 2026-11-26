//go:build templateservice

// ---
// relationships:
//   references: [menagerie, template-service-api]
// ---

// A template whose credential is an SSH key is registered, listed and
// fetched through the template service binary offering only that key, while
// the agent holds another the Git host also accepts and lists first.

package workflows

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/gitcreds"
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// generated is a new ed25519 key for the test.
func generated(t *testing.T) (ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return private, signer.PublicKey()
}

func TestAnSSHTemplateOffersOnlyItsOwnKeyThroughTheTemplateService(t *testing.T) {
	bin := os.Getenv("MENAGERIE_TEMPLATE_SERVICE")
	if bin == "" {
		t.Fatal("MENAGERIE_TEMPLATE_SERVICE must name the template service binary")
	}
	ctx := context.Background()
	host := backuptest.New(t)
	// The agent lists first a key the host also accepts, so a connection
	// that offered every key would sign in with it.
	keyring := agent.NewKeyring()
	otherPrivate, other := generated(t)
	chosenPrivate, chosen := generated(t)
	for _, k := range []struct {
		private ed25519.PrivateKey
		public  ssh.PublicKey
		name    string
	}{{otherPrivate, other, "sample-other"}, {chosenPrivate, chosen, "sample-template-key"}} {
		host.Authorize(k.public)
		if err := keyring.Add(agent.AddedKey{PrivateKey: k.private, Comment: k.name}); err != nil {
			t.Fatal(err)
		}
	}
	agentPath := host.ServeAgent(t, keyring)

	base := t.TempDir()
	work := filepath.Join(base, "kit")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, work, "init", "-q", "-b", "main")
	sampleKit(t, work, 1)
	gitCLI(t, work, "add", "-A")
	gitCLI(t, work, "commit", "-q", "-m", "v1")
	gitCLI(t, work, "tag", "1.0.0")
	gitCLI(t, base, "clone", "-q", "--bare", work, filepath.Join(host.Dir, "kit.git"))

	sockDir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "template.sock")
	root := filepath.Join(base, "workspaces")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("workspace-root: %s\nworkspace-repository: %s\nwyrwood-directory: /run/sample\noidc: {issuer: 'https://issuer.example.invalid', client-id: sample}\ntemplate-service: {socket: %s, hook-runner-socket: %s, candidate-folder: %s}\n",
		root, host.URL("backups.git"), socket, filepath.Join(sockDir, "hook-runner.sock"), filepath.Join(base, "candidates"))
	writeFile(t, base, "config.yml", config)
	// No user SSH configuration applies: -F /dev/null.
	sshCommand := "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + host.KnownHosts
	startService(t, bin, filepath.Join(base, "config.yml"), filepath.Join(base, "data"), socket, &lockedText{},
		"SSH_AUTH_SOCK="+agentPath, "GIT_SSH_COMMAND="+sshCommand)

	data, err := store.Open(filepath.Join(base, "state", "menagerie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	workspaces, err := workspace.NewRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	service := templates.New(socket)
	r := &registry.Registry{Data: data, Templates: service, Credentials: &gitcreds.Source{Socket: agentPath}, Root: workspaces}
	address := host.URL("kit.git")
	fingerprint := ssh.FingerprintSHA256(chosen)

	// Without a credential, the agent's first key signs in: the control.
	if _, err := service.ListVersions(ctx, address, nil); err != nil {
		t.Fatal(err)
	}
	if offered := host.Offered(); len(offered) == 0 || offered[0] != ssh.FingerprintSHA256(other) {
		t.Fatalf("without a credential: %v", offered)
	}

	tpl, err := r.Register(ctx, "Kit", address, "", store.Credential{Key: backuptest.AuthorizedKey(chosen)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if offered := host.Offered(); len(offered) == 0 || !allOf(offered, fingerprint) {
		t.Fatalf("registration offered %v, not only %s", offered, fingerprint)
	}
	if _, err := r.CheckVersions(ctx, tpl.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := data.Template(ctx, tpl.ID); got.CheckFailure != "" {
		t.Fatalf("version check failed: %s", got.CheckFailure)
	}
	if offered := host.Offered(); len(offered) == 0 || !allOf(offered, fingerprint) {
		t.Fatalf("version check offered %v, not only %s", offered, fingerprint)
	}

	// A key the agent does not hold fails the change, naming it, and
	// reaches no host.
	_, missing := generated(t)
	_, err = r.SetCredential(ctx, tpl.ID, store.Credential{Key: backuptest.AuthorizedKey(missing)}, false)
	if err == nil || !strings.Contains(err.Error(), ssh.FingerprintSHA256(missing)) {
		t.Fatalf("a key the agent does not hold: %v", err)
	}
	if offered := host.Offered(); len(offered) != 0 {
		t.Fatalf("a key the agent does not hold reached the host: %v", offered)
	}
	if got, _ := data.Template(ctx, tpl.ID); !reflect.DeepEqual(got.Credential, tpl.Credential) {
		t.Fatalf("a refused change was stored: %+v", got.Credential)
	}
}

// allOf says whether every fingerprint is want.
func allOf(fingerprints []string, want string) bool {
	for _, f := range fingerprints {
		if f != want {
			return false
		}
	}
	return true
}
