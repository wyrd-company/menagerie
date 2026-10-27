---
title: Installation
order: 2
install: true
product: menagerie
relationships:
  references:
    - menagerie
    - configuration
    - authentication
    - menagerie-has-host-root
---

Menagerie runs as one container on a Linux host with Docker Engine, on
`amd64` or `arm64`. The image is `ghcr.io/wyrd-company/menagerie:<version>`.
Both of its services use the host's Docker daemon and a Unix socket inside
the container, so there is no standalone service for macOS or Windows.

Before the container starts, you need:

- an OpenID Connect provider with a public client for Menagerie and a group
  named `menagerie_managers`;
- a Git repository that Menagerie may push workspace backups to, and a Git
  identity for it that Wyrwood holds as an OpenBao Transit key: a GitHub
  App, an SSH key, or both (see [Git identity](#git-identity));
- a Wyrwood agent on the host, and a policy for Menagerie;
- the workspace root: one folder on the host for every workspace.

## Identity provider

Register Menagerie as a **public** client: no client secret, authorization
code flow with PKCE (`S256`). Allow the redirect URI
`https://<menagerie host>/auth/callback`. Release the `groups` claim, and
create the group `menagerie_managers` with the people who manage the fleet.
The group name is fixed.

With Pocket ID, create the OIDC client with **Public client** and **PKCE**
on and that callback URL, then create the group `menagerie_managers` and add
the managers. Pocket ID puts `groups` in the ID token.

Set `oidc.issuer`, `oidc.client-id` and `oidc.redirect-url` in the
configuration. See [Configuration](configuration.md#identity-provider).

## Wyrwood

SSH and GitHub App private keys stay in OpenBao; Menagerie uses Wyrwood for
signing and holds short-lived GitHub App installation tokens in memory.
Separately, it generates and stores the interview signing key at
`/var/lib/menagerie/toha/session-key` in its data volume. Wyrwood gives the
container a directory of sockets:

- `vault.sock` signs the GitHub App's JWTs with the App's Transit key, when
  `github-app` is configured, and reads registry credentials from OpenBao,
  when `registry-credentials` is configured. Both use the binding's own
  token.
- `ssh-agent.sock` signs Toha's SSH template fetches and Menagerie's SSH
  backup pushes with Menagerie's SSH key, when the policy offers one.

### Git identity

Choose how Menagerie authenticates to its Git hosts:

- **GitHub App**: the recommended way for GitHub. Install one App on the
  backup repository and on each private template repository, with the
  Contents permission set to read and write. Menagerie mints a short-lived
  token for one repository at a time: write for backups, read for
  templates. No machine user is needed. Backups and private templates use
  `https://` URLs. See [Configuration](configuration.md#github-app).
- **SSH identity**: for other Git hosts, or for GitHub without an App. One
  SSH key must reach every repository Menagerie uses over SSH; see below.
- **Both**: the App for repositories on GitHub, and SSH for other hosts.
  Each URL uses the identity its scheme and host select.

#### GitHub App

Create a GitHub App on the account that owns the repositories. Give it the
repository permission **Contents: Read and write** and no other permission,
and turn its webhook off. Install it on the backup repository and on each
private template repository. Generate a private key, then import it into
OpenBao's Transit engine as a key that cannot be exported. The import wraps
the key with Transit's wrapping key, so the key is never sent in clear:

```sh
openssl pkcs8 -topk8 -nocrypt -in app.private-key.pem -outform DER | base64 -w0 > app-key.b64
bao transit import transit/keys/menagerie-github-app @app-key.b64 type=rsa-2048
rm app-key.b64 app.private-key.pem
```

The `vault` CLI takes the same arguments. The importing token needs `read`
on `transit/wrapping_key` and `update` on
`transit/keys/menagerie-github-app/import`. If the key is lost, generate a
new one in the App's settings and import it again. Set `github-app.app-id`
and `github-app.transit-key` in the configuration.

#### SSH identity

Toha's template fetches and Menagerie's backup pushes use the same agent, so
the agent must offer one key, and that key must reach every repository
Menagerie uses over SSH. An SSH client offers its keys in turn, and the Git
host signs it in with the first key it knows, whether or not that key can
reach the repository. A second key in the agent does not select a second
repository. GitHub accepts a deploy key on one repository only, and the same
key cannot be registered twice.

- **Only the backup repository is on SSH**: add the Transit key's public
  key to the backup repository as a deploy key with write access.
- **A private template is on SSH too**: create a machine user, an account
  for automation, and add the Transit key's public key as that account's
  SSH key. Give the machine user write access to the backup repository and
  read access to each private template repository. Do not also add the key
  as a deploy key.

### Policy

Write a Wyrwood policy for Menagerie. It grants `update` on
`transit/sign/<key>/sha2-256` for the GitHub App's key, offers exactly one
Transit key as Menagerie's SSH key, grants `read` on
`<kv-mount>/data/<path>` for each configured registry secret, and mounts the
consumer directory at `/run/wyrwood`. This example uses both identities:

```yaml
name: menagerie
openbao:
  agent-token-renewal: true
  policy: |
    path "transit/sign/menagerie-github-app/sha2-256" { capabilities = ["update"] }
    path "transit/keys/menagerie-ssh" { capabilities = ["read"] }
    path "transit/sign/menagerie-ssh" { capabilities = ["update"] }
    path "kv/data/menagerie/ghcr" { capabilities = ["read"] }
ssh:
  transit-keys:
    - menagerie-ssh
contract:
  mount-target: /run/wyrwood
```

With only the GitHub App, leave out the SSH key's two paths and the `ssh`
section. With only SSH, leave out the App key's path.

Bind the container `menagerie` (the name `deploy/compose.yml` gives it) to
the policy. Give the binding an **access group** that Menagerie's user has,
for example the group that owns the workspace root: Menagerie runs as the
workspace root's owner, not as root, and without an access group Wyrwood's
sockets are open only to Wyrwood's own user. The agent creates the consumer
directory `/run/wyrwood/containers/menagerie` when it binds the container.
Create the container first, bind it, then start it.

## The host

`deploy/compose.yml` is a complete example. Copy the `deploy` folder and
edit both files.

| Mount or setting | Why |
| --- | --- |
| The workspace root at the same path on the host and in the container | Devcontainers bind-mount their workspace folder. The host daemon resolves that path, so it must be the host path. Set `MENAGERIE_WORKSPACE_ROOT` and `workspace-root` to the same value. |
| The workspace socket folder at the same path on the host and in the container | Each devcontainer mounts its own socket folder from it, so it must be the host path. Set `MENAGERIE_WORKSPACE_SOCKETS` and `workspace-socket.folder` to the same value; leave both out to run without [workspace sockets](configuration.md#workspace-sockets). |
| `/var/run/docker.sock` | The workspace service drives the host Docker daemon: devcontainers, and the throwaway containers that run template hooks and `initializeCommand`. |
| The Wyrwood consumer directory at `/run/wyrwood` | `vault.sock` and `ssh-agent.sock`. `wyrwood-directory` names the mount target. |
| `SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock` | With an SSH identity: Toha's Git transport runs the image's `ssh` client with this agent. Leave it out with only a GitHub App. |
| `config.yml` at `/etc/menagerie/config.yml` | Both services read it. |
| `SSL_CERT_FILE` and the bundle it names | Optional: a Git server whose certificate a private authority signed. See [Git hosts](#git-hosts). |
| A volume at `/var/lib/menagerie` | Toha's template cache, the template service's session key, the services' home and the folder Toha builds an upgrade's candidate in. Without it they are lost when the container is recreated. |
| `restart: unless-stopped` | Docker starts Menagerie after a host restart, and restarts it after either service stops. |
| Port `8080` | The web UI and workspace API. Publish it on a private address only. |

Create the socket folder before the first start, owned by the workspace
root's owner; Docker would create a missing one owned by root, and
Menagerie then refuses to start:

```sh
sudo install -d -m 0700 -o "$(stat -c %u /srv/menagerie/workspaces)" -g "$(stat -c %g /srv/menagerie/workspaces)" /srv/menagerie/sockets
```

```sh
cp -r deploy ~/menagerie && cd ~/menagerie
docker compose create
# Bind the container "menagerie" in Wyrwood now.
docker compose up -d
docker compose logs menagerie
```

The log names the user the services run as, for example:

```text
menagerie: services run as menagerie (uid 1000, gid 1000, groups 1000,998,999), the owner of /srv/menagerie/workspaces; the template service has groups 1000,998
```

### Who the services run as

Both services run as the owner of the workspace root. Everything Menagerie,
the template hooks and `initializeCommand` write into a workspace belongs to
that user: hooks and `initializeCommand` run in throwaway containers as
that uid and gid, with no other group. The devcontainer's remote user is
remapped to the same uid so it can edit the workspace. If root owns the
workspace root, the services run as root. When `workspace-root` or
`wyrwood-directory` is a symbolic link, its target's owner and group count.

The services' groups are the owner's group and the Wyrwood consumer
directory's group; the workspace service also has the Docker socket's
group. The template service never talks to Docker. For a non-root owner:

- the Wyrwood binding needs an access group this user has;
- `config.yml` must be readable by this user;
- `/var/lib/menagerie` must be a new, empty volume, or one this user's
  Menagerie wrote. Its `toha` and `home` folders must be real folders, not
  symbolic links, holding only files this user wrote. Otherwise Menagerie
  changes nothing, does not start and names the path. If the owner of the
  workspace root changes, remove the `menagerie-data` volume.

### Git hosts

| Host | Identity | What the container needs |
| --- | --- | --- |
| GitHub, `https://` | GitHub App | Nothing more: the image trusts GitHub's certificates. |
| GitHub Enterprise Server, `https://` | GitHub App, with `github-app.server` | The server's certificate authority, when it is not a public one; see below. |
| GitHub, SSH | SSH identity | Nothing more: `/etc/ssh/ssh_known_hosts` holds GitHub's published SSH host keys. |
| Another host, SSH | SSH identity | A known hosts file that also names the host. |
| Another host, `https://` | None | Public templates only: Menagerie gives these URLs no credential. |

For other SSH hosts, mount a known hosts file that also names them:

```yaml
    volumes:
      - ./ssh_known_hosts:/etc/ssh/ssh_known_hosts:ro
```

For a server whose certificate a private authority signed, mount a bundle
that holds that authority and set `SSL_CERT_FILE` to it. The template
service then trusts only the certificates in that file, so build the bundle
from the image's own and the authority:

```sh
docker run --rm --entrypoint cat ghcr.io/wyrd-company/menagerie:<version> /etc/ssl/certs/ca-certificates.crt > ca-bundle.pem
cat private-authority.pem >> ca-bundle.pem
```

```yaml
    environment:
      SSL_CERT_FILE: /etc/menagerie/ca-bundle.pem
    volumes:
      - ./ca-bundle.pem:/etc/menagerie/ca-bundle.pem:ro
```

The workspace service reads the same file, beside the image's certificate
folder.

### Throwaway containers

Template hooks and `initializeCommand` run in throwaway containers on the
host daemon, which need no extra mount or setting. By default they run
Menagerie's own image, which Menagerie finds by inspecting its own
container: by the container id in its mounts or, when they name none, by
its hostname, Docker's default. When it cannot, a job with a hook or an `initializeCommand`
fails as `runner-image-unavailable`; set
[`runner-image`](configuration.md#runner-images). Each container carries
the label `menagerie.throwaway` and is removed when its command ends; any
left by a stopped Menagerie are removed when it starts.

### Container registries

Menagerie pulls and builds devcontainer images anonymously unless
`registry-credentials` names a secret for the registry. It never reads a
Docker client configuration or credential helper. A private image, or a
Docker Hub base image on a host that reaches Docker Hub's anonymous pull
limit, needs an entry for that registry; for Docker Hub use `docker.io` and
a Docker Hub access token as the password. See
[Configuration](configuration.md#registry-credentials).

## Upgrades

Pull the new image tag and recreate the container. Running jobs end with the
container, and job history starts empty; workspace folders, their answers in
`.toha.json`, their containers and the sign-in configuration remain.
Everyone signs in again.

## What Menagerie can do to the host

Menagerie mounts the rootful Docker socket, so whoever controls Menagerie
controls the host: a manager and anyone who gets past sign-in. This is the
access a manager has when running devcontainers on the host by hand.
Template hooks and `initializeCommand` run in throwaway containers that
cannot reach the Docker socket, the Wyrwood sockets or Menagerie's
environment; they can write to their workspace folder and reach the
network. Keep these limits in place:

- Only members of `menagerie_managers` reach the UI. Membership is checked
  at every sign-in; remove someone from the group and restart Menagerie to
  end their session at once.
- Publish the UI on a private address behind your HTTPS reverse proxy, never
  on the public internet.
- Approve only templates you trust; Menagerie uses only the templates its
  configuration lists.
- Without `job-limits`, a job has no time limit, and a hook or build that
  never ends keeps its workspace busy until Menagerie restarts. Set
  [job time limits](configuration.md#job-time-limits) and an alert webhook
  to be told.
- A backup is a force-push of the workspace's own branch. Commits pushed to
  that branch from elsewhere are lost at the next backup unless
  [Apply branch](usage.md#apply-a-reviewed-branch) brings them in first;
  propose changes on another branch and apply that.
- With [workspace sockets](configuration.md#workspace-sockets), anything in
  a devcontainer can commit and back up its own workspace, and nothing
  more: the socket names no other workspace and its replies carry no
  message. Keep `min-interval` above `0` so a loop cannot fill the backup
  branch.
