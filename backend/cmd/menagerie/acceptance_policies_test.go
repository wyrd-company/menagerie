//go:build pocketid && image && acceptance

package main

// The ruling of 2026-11-17: an ID read and a filtered binding-list cannot
// prove removal. Only bind can distinguish absence from a hidden replacement.
// The second operator here is Wyrwood's signed-in web interface.
import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
	"github.com/wyrd-company/wyrwood/operator"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type policyDefinition struct {
	provisioning.Runtime
	name    string
	invalid bool
}

func (r *policyDefinition) Definition(string) (devcontainer.Definition, error) {
	if r.invalid {
		return devcontainer.Definition{}, errors.New("definition no longer parses")
	}
	return devcontainer.Definition{ContainerName: r.name, Block: []byte(`{"wyrwood":{"accessGroup":1000}}`)}, nil
}

type policyProgress struct{ strings.Builder }

func (*policyProgress) Phase(string)           {}
func (p *policyProgress) Log(string) io.Writer { return &p.Builder }

func TestImageWorkspacePolicies(t *testing.T) {
	p := startPocketID(t)
	w := startWyrwood(t, p)
	ctx := context.Background()
	for _, name := range []string{"workspace", "ssh-choice", "hidden"} {
		w.savePolicy(name, `path "auth/token/lookup-self" { capabilities = ["read"] }`, true, nil, "/run/wyrwood")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _ = agent.ServeAgent(keyring, c) }()
		}
	}()
	out, err := exec.Command(w.bin, "controller", "host-key", "--config", w.controllerArgs[len(w.controllerArgs)-1]).Output()
	if err != nil {
		t.Fatal(err)
	}
	hostKey := strings.TrimSpace(string(out))
	conn, err := operator.Dial(ctx, operator.Config{URL: "ssh://" + w.sshAddr, Signer: signer, HostKey: hostKey, Label: "policy-test"})
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, operator.ErrNotApproved) {
		t.Fatalf("pending key: %v", err)
	}
	w.post("/operator-keys/approve", url.Values{"fingerprint": {ssh.FingerprintSHA256(signer.PublicKey())}, "label": {"policy-test"}, "agents": {w.agentName}, "policies": {"workspace", "ssh-choice"}, "actions": {"read", "bind"}})
	client, err := wyrwood.New(wyrwood.Config{Controller: "ssh://" + w.sshAddr, HostKey: hostKey, Agent: w.agentName, OperatorKey: ssh.FingerprintSHA256(signer.PublicKey()), Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			label := "removed"
			if hidden {
				label = "hidden"
			}
			if empty {
				label += "-empty"
			} else {
				label += "-selected"
			}
			t.Run(label, func(t *testing.T) {
				name := "devcontainer-" + label
				data, err := store.Open(filepath.Join(t.TempDir(), "menagerie.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer data.Close()
				must := func(e error) {
					t.Helper()
					if e != nil {
						t.Fatal(e)
					}
				}
				must(data.BeginCreate(ctx, label))
				must(data.SetCreateSelection(ctx, label, []string{"workspace"}, []string{"workspace", "ssh-choice"}))
				must(data.HoldName(ctx, label, name, w.agentName, true))
				runtime := &policyDefinition{name: name}
				steps := provisioning.New(provisioning.Options{Clients: provisioning.Clients{Bindings: client, Agent: w.agentName, Wait: 10 * time.Second}, Runtime: runtime, Data: data, Poll: time.Millisecond})
				progress := &policyProgress{}
				must(steps.ApplyPolicies(ctx, "/workspaces/"+label, progress))
				before, err := data.Selection(ctx, label)
				must(err)
				binding, found, err := client.Read(ctx, name)
				must(err)
				if !found || !binding.Applied {
					t.Fatalf("not applied: %+v", binding)
				}
				// A malformed definition does not stop an existing binding's edit or
				// change its access group. No container operation is available to call.
				runtime.invalid = true
				changed, err := data.SaveSelection(ctx, label, []string{"workspace", "ssh-choice"}, before.Revision, []string{"workspace", "ssh-choice"})
				must(err)
				must(steps.ApplyPolicies(ctx, "/workspaces/"+label, progress))
				binding, found, err = client.Read(ctx, name)
				must(err)
				if !found || !slices.Equal(binding.Policies, []string{"ssh-choice", "workspace"}) || binding.Settings.AccessGroup == nil || *binding.Settings.AccessGroup != 1000 {
					t.Fatalf("edit lost settings: %+v", binding)
				}
				runtime.invalid = false
				// Remove through the web operator, never an Unbind probe.
				w.post("/bindings/"+url.PathEscape(binding.ID)+"/unbind", url.Values{})
				if hidden {
					w.post("/bindings", url.Values{"typed": {"yes"}, "agent": {w.agentName}, "key": {"name:" + name}, "policies": {"hidden"}, "enforcement": {"report"}, "access-group": {"1000"}})
				}
				desired := []string{"workspace"}
				if empty {
					desired = []string{}
				}
				saved, err := data.SaveSelection(ctx, label, desired, changed.Revision, []string{"workspace", "ssh-choice"})
				must(err)
				err = steps.ApplyPolicies(ctx, "/workspaces/"+label, progress)
				after, e := data.Selection(ctx, label)
				must(e)
				if hidden || empty {
					kind := provisioning.KindBindingRefused
					if hidden {
						kind = provisioning.KindBindingOutsideGrant
					}
					var failure *jobs.Failure
					if !errors.As(err, &failure) || failure.Kind != kind {
						t.Fatalf("apply: %v, want %s", err, kind)
					}
					if after.AppliedRevision == nil || *after.AppliedRevision != changed.Revision || after.ApplyFailure == nil || after.ApplyFailure.Kind != kind {
						t.Fatalf("refusal recorded applied: %+v", after)
					}
				} else {
					must(err)
					binding, found, e = client.Read(ctx, name)
					must(e)
					if !found || !binding.Applied || !slices.Equal(binding.Policies, desired) || after.AppliedRevision == nil || *after.AppliedRevision != saved.Revision {
						t.Fatalf("recreated binding: %+v selection: %+v", binding, after)
					}
				}
				held, e := data.Held(ctx, label)
				must(e)
				if held.Current() != name {
					t.Fatalf("read recorded removal: %+v", held)
				}
			})
		}
	}
}
