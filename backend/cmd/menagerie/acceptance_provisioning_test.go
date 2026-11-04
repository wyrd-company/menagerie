//go:build pocketid && browser && image && acceptance

package main

// What the demo's fixture adds to TestAcceptanceFixture, when
// MENAGERIE_XOLOTL_IMAGE names a Xolotl image: the host preparation of
// docs/user/install.md's Provisioning section, and a template with a
// provisioning declaration. Xolotl runs on the Engine's host network, and its
// secrets are in the suite's OpenBao. Every key and token is generated here.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/wyrd-company/wyrwood/operator"
	"golang.org/x/crypto/ssh"
)

const (
	provisionedKeyPolicy = "host-sample"
	provisionedPolicy    = "sample-workspace"
	operatorKeyName      = "sample-operator"
	workspaceKeyName     = "sample-workspace-ssh"
	xolotlPort           = 8317
)

// provisionedHost is what the fixture's Menagerie needs from a prepared host.
type provisionedHost struct {
	// config is the provisioning section of Menagerie's configuration.
	config string
	// policy and transitKeys extend Menagerie's Wyrwood policy.
	policy      string
	transitKeys []string
}

// transitSigner signs with an OpenBao Transit key, so that the fixture can
// present Menagerie's operator key to the controller before Menagerie starts.
type transitSigner struct {
	t    *testing.T
	w    *wyrwoodSite
	name string
	key  ssh.PublicKey
}

func (s transitSigner) PublicKey() ssh.PublicKey { return s.key }

func (s transitSigner) Sign(_ io.Reader, data []byte) (*ssh.Signature, error) {
	signed, err := s.w.bao.Logical().Write("transit/sign/"+s.name, map[string]any{"input": base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return nil, err
	}
	parts := strings.Split(fmt.Sprint(signed.Data["signature"]), ":")
	blob, err := base64.StdEncoding.DecodeString(parts[len(parts)-1])
	if err != nil {
		return nil, err
	}
	return &ssh.Signature{Format: ssh.KeyAlgoED25519, Blob: blob}, nil
}

// startXolotl runs Xolotl with an AppRole of its own, and puts Menagerie's
// scoped key in the secret Menagerie reads, issued from a provisioner policy
// as the configuration page describes.
func startXolotl(t *testing.T, w *wyrwoodSite, p *pocketID, ref, dir string) {
	t.Helper()
	ctx := context.Background()
	if err := w.bao.Sys().PutPolicy("xolotl", `path "kv/data/xolotl/*" { capabilities = ["create", "read", "update", "delete"] }
path "kv/metadata/xolotl/*" { capabilities = ["list", "read", "delete"] }
path "kv/destroy/xolotl/*" { capabilities = ["update"] }
`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.bao.Logical().Write("auth/approle/role/xolotl", map[string]any{"token_policies": "xolotl", "token_ttl": "1h", "token_max_ttl": "24h"}); err != nil {
		t.Fatal(err)
	}
	role, err := w.bao.Logical().Read("auth/approle/role/xolotl/role-id")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := w.bao.Logical().Write("auth/approle/role/xolotl/secret-id", nil)
	if err != nil {
		t.Fatal(err)
	}
	admin := randomHex()
	if _, err := w.bao.KVv2("kv").Put(ctx, "xolotl/admin", map[string]any{"token": admin}); err != nil {
		t.Fatal(err)
	}
	writeTo(t, dir, "xolotl.toml", fmt.Sprintf("[vault]\naddress = %q\nmount = \"kv\"\nprefix = \"xolotl\"\n", w.baoAddr))
	issuer := provisionedKeyPolicy + "-issuer"
	writeTo(t, dir, "xolotl-policies/"+provisionedKeyPolicy+".toml", fmt.Sprintf("name = %q\ndescription = \"Client keys of this host's workspaces\"\nscopes = [\"proxy\", \"mcp\"]\nproviders = [\"*\"]\n", provisionedKeyPolicy))
	writeTo(t, dir, "xolotl-policies/"+issuer+".toml", fmt.Sprintf("name = %q\ndescription = \"Issues keys for this host's workspaces\"\nscopes = [\"keys\"]\nkey_policies = [%q]\n", issuer, provisionedKeyPolicy))

	rc, err := p.docker.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		t.Fatalf("pull %s: %v", ref, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{Image: ref,
			Env: []string{
				fmt.Sprintf("XOLOTL_VAULT_ROLE_ID=%s", role.Data["role_id"]),
				fmt.Sprintf("XOLOTL_VAULT_SECRET_ID=%s", secret.Data["secret_id"]),
			},
			Labels: map[string]string{"menagerie-test": "xolotl"}},
		&container.HostConfig{NetworkMode: "host", Binds: []string{
			filepath.Join(dir, "xolotl.toml") + ":/etc/xolotl/config.toml:ro",
			filepath.Join(dir, "xolotl-policies") + ":/policies:ro",
			"sample-xolotl-data:/data",
		}}, nil, nil, "sample-xolotl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := p.docker.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	x := &imageContainer{t: t, docker: p.docker, id: created.ID}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Xolotl logs:\n%s", x.logs(time.Time{}))
		}
	})
	// The image has no shell, so the admin token goes on the command line.
	cli := func(group string, args ...string) (string, int) {
		return x.exec("", append([]string{"xolotl", group, "--token", admin}, args...)...)
	}
	waitFor(t, "Xolotl", func() bool {
		_, code := cli("policies", "list")
		return code == 0
	})
	for _, name := range []string{provisionedKeyPolicy, issuer} {
		if out, code := cli("policies", "put", "/policies/"+name+".toml"); code != 0 {
			t.Fatalf("policy %s: %s", name, out)
		}
	}
	// Its output holds the key, so a failure reports only the exit status.
	if _, code := cli("keys", "issue", "--policy", issuer, "--name", "menagerie"); code != 0 {
		t.Fatalf("scoped key exited %d", code)
	}
	issued, err := w.bao.KVv2("kv").Get(ctx, "xolotl/keys/"+issuer+"/menagerie")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.bao.KVv2("kv").Put(ctx, "menagerie/xolotl", map[string]any{"api_key": issued.Data["api_key"]}); err != nil {
		t.Fatal(err)
	}
}

// prepareProvisionedHost does the host preparation of docs/user/install.md, up
// to the approved operator key, and returns what Menagerie's configuration and
// policy need. The nil result means the run has no provisioning.
func prepareProvisionedHost(t *testing.T, w *wyrwoodSite, p *pocketID, dir string) *provisionedHost {
	t.Helper()
	ref := os.Getenv("MENAGERIE_XOLOTL_IMAGE")
	if ref == "" {
		return nil
	}
	startXolotl(t, w, p, ref, dir)
	for _, key := range []string{operatorKeyName, workspaceKeyName} {
		if _, err := w.bao.Logical().Write("transit/keys/"+key, map[string]any{"type": "ed25519"}); err != nil {
			t.Fatal(err)
		}
	}
	w.savePolicy(provisionedPolicy, fmt.Sprintf(`path "auth/token/lookup-self" { capabilities = ["read"] }
path "kv/data/xolotl/keys/%s/{{binding.xolotl_key_name}}" { capabilities = ["read"] }
path "transit/keys/%[2]s" { capabilities = ["read"] }
path "transit/sign/%[2]s" { capabilities = ["update"] }
`, provisionedKeyPolicy, workspaceKeyName), true, []string{workspaceKeyName}, "/run/wyrwood")

	// The first connection records the key as pending; the operator approves it.
	out, err := exec.Command(w.bin, "controller", "host-key", "--config", w.controllerArgs[len(w.controllerArgs)-1]).Output()
	if err != nil {
		t.Fatalf("controller host-key: %v", err)
	}
	hostKey := strings.TrimSpace(string(out))
	signer := transitSigner{t: t, w: w, name: operatorKeyName, key: transitKey(t, w, operatorKeyName)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := operator.Dial(ctx, operator.Config{URL: "ssh://" + w.sshAddr, Signer: signer, HostKey: hostKey, Label: "menagerie-" + w.agentName})
	if !errors.Is(err, operator.ErrNotApproved) {
		if client != nil {
			_ = client.Close()
		}
		t.Fatalf("a new operator key answered %v", err)
	}
	w.post("/operator-keys/approve", url.Values{
		"fingerprint": {ssh.FingerprintSHA256(signer.key)},
		"label":       {"menagerie-" + w.agentName},
		"agents":      {w.agentName},
		"policies":    {provisionedPolicy},
		"actions":     {"read", "bind"},
	})
	return &provisionedHost{
		config: fmt.Sprintf(`provisioning:
  wyrwood:
    controller: ssh://%s
    host-key: %s
    agent: %s
    operator-key: %s
  xolotl:
    url: http://127.0.0.1:%d
    key-secret:
      kv-mount: kv
      path: menagerie/xolotl
`, w.sshAddr, hostKey, w.agentName, operatorKeyName, xolotlPort),
		policy: fmt.Sprintf(`path "transit/keys/%[1]s" { capabilities = ["read"] }
path "transit/sign/%[1]s" { capabilities = ["update"] }
path "kv/data/menagerie/xolotl" { capabilities = ["read"] }
`, operatorKeyName),
		transitKeys: []string{operatorKeyName},
	}
}

// provisionedTemplate turns the sample template's version into one whose
// definition declares a Wyrwood binding and a Xolotl key, has the T3 Code
// Feature, and proves its credentials on every start. The check discards what
// it reads, so no key reaches the job's log.
func provisionedTemplate(t *testing.T, dir string, version int) {
	t.Helper()
	browserTemplate(t, dir, version)
	path := filepath.Join(dir, "template.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTo(t, dir, "template.yml", strings.Replace(string(body), "interview:\n", "interview:\n  - {id: workspace, computed: toha_target_name}\n", 1))
	writeTo(t, dir, "template/.devcontainer/check-credentials.sh", fmt.Sprintf(`#!/bin/sh
# postStartCommand: fails the job unless the Wyrwood sockets work.
set -eu
curl -fsS --max-time 30 --unix-socket /run/wyrwood/vault.sock -o /dev/null "http://vault/v1/kv/data/xolotl/keys/%s/$1"
key=$(mktemp)
timeout 30 ssh-add -L > "$key"
printf check | timeout 30 ssh-keygen -Y sign -n check -f "$key" > /dev/null 2>&1
rm -f "$key" "$key.sig"
echo "check-credentials: the Xolotl key reads through vault.sock, and ssh-agent.sock signs."
`, provisionedKeyPolicy))
	writeTo(t, dir, "template/.devcontainer/devcontainer.json", fmt.Sprintf(`{
  "name": "sample",
  "image": "ghcr.io/wyrd-company/devcontainers/base:noble@sha256:36576d15a0d696b8b0262f3f621edee0af2f82543520f0142c8b7561727e7cc0",
  "features": {"ghcr.io/wyrd-company/devcontainers/t3code-server:latest": {}},
  "mounts": [{"source": "/run/wyrwood/containers/devcontainer-{{ workspace }}", "target": "/run/wyrwood", "type": "bind"}],
  "customizations": {
    "menagerie": {
      "wyrwood": {"accessGroup": 1000, "policies": [%q], "values": {"xolotl_key_name": "{{ workspace }}"}},
      "xolotl": {"keyPolicy": %q, "keyName": "{{ workspace }}"}
    }
  },
  "containerEnv": {"SSH_AUTH_SOCK": "/run/wyrwood/ssh-agent.sock"},
  "postStartCommand": "sh .devcontainer/check-credentials.sh {{ workspace }}",
  "overrideCommand": false,
  "runArgs": ["--name", "devcontainer-{{ workspace }}"]
}
`, provisionedPolicy, provisionedKeyPolicy))
}
