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
- a Git repository that Menagerie may push workspace backups to, and one SSH
  identity for it that Wyrwood holds as an OpenBao Transit key (see
  [One SSH identity](#one-ssh-identity));
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

Menagerie holds no key, token or password. Wyrwood gives the container a
directory of sockets:

- `ssh-agent.sock` signs Toha's private template fetches and Menagerie's
  backup pushes with Menagerie's SSH key;
- `vault.sock` reads registry credentials from OpenBao with the binding's
  own token, when `registry-credentials` is configured.

Write a Wyrwood policy for Menagerie. It offers exactly one Transit key,
Menagerie's SSH key, grants `read` on `<kv-mount>/data/<path>` for each
configured registry secret, and mounts the consumer directory at
`/run/wyrwood`:

```yaml
name: menagerie
openbao:
  agent-token-renewal: true
  policy: |
    path "transit/keys/menagerie-github" { capabilities = ["read"] }
    path "transit/sign/menagerie-github" { capabilities = ["update"] }
    path "kv/data/menagerie/ghcr" { capabilities = ["read"] }
ssh:
  transit-keys:
    - menagerie-github
contract:
  mount-target: /run/wyrwood
```

### One SSH identity

Toha's template fetches and Menagerie's backup pushes use the same agent, so
the agent must offer one key, and that key must reach every repository
Menagerie uses over SSH. An SSH client offers its keys in turn, and the Git
host signs it in with the first key it knows, whether or not that key can
reach the repository. A second key in the agent does not select a second
repository. GitHub accepts a deploy key on one repository only, and the same
key cannot be registered twice.

- **Every template is public** (`gh:` or `https://` addresses): only backups
  use SSH. Add the Transit key's public key to the backup repository as a
  deploy key with write access.
- **Any template is private:** create a machine user, a GitHub account for
  automation, and add the Transit key's public key as that account's SSH
  key. Give the machine user write access to the backup repository and read
  access to each private template repository. Do not also add the key as a
  deploy key.

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
| `/var/run/docker.sock` | The workspace service drives the host Docker daemon. |
| The Wyrwood consumer directory at `/run/wyrwood` | `ssh-agent.sock` and `vault.sock`. `wyrwood-directory` names the mount target. |
| `SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock` | Toha's Git transport runs the image's `ssh` client with this agent. |
| `config.yml` at `/etc/menagerie/config.yml` | Both services read it. |
| A volume at `/var/lib/menagerie` | Toha's template cache and the services' home. Without it they are lost when the container is recreated. |
| `restart: unless-stopped` | Docker starts Menagerie after a host restart, and restarts it after either service stops. |
| Port `8080` | The web UI and workspace API. Publish it on a private address only. |

```sh
cp -r deploy ~/menagerie && cd ~/menagerie
docker compose create
# Bind the container "menagerie" in Wyrwood now.
docker compose up -d
docker compose logs menagerie
```

The log names the user the services run as, for example:

```text
menagerie: services run as menagerie (uid 1000, gid 1000, groups 1000,999), the owner of /srv/menagerie/workspaces
```

### Who the services run as

Both services run as the owner of the workspace root. Everything Menagerie,
the template hooks and `initializeCommand` write into a workspace belongs to
that user, and the devcontainer's remote user is remapped to the same uid so
it can edit the workspace. If root owns the workspace root, the services run
as root. When `workspace-root` or `wyrwood-directory` is a symbolic link, its
target's owner and group count.

The services' groups are the owner's group, the Docker socket's group and
the Wyrwood consumer directory's group. For a non-root owner:

- the Wyrwood binding needs an access group this user has;
- `config.yml` must be readable by this user;
- `/var/lib/menagerie` must be a new, empty volume, or one this user's
  Menagerie wrote. Its `toha` and `home` folders must be real folders, not
  symbolic links, holding only files this user wrote. Otherwise Menagerie
  changes nothing, does not start and names the path. If the owner of the
  workspace root changes, remove the `menagerie-data` volume.

### Git hosts

`/etc/ssh/ssh_known_hosts` holds GitHub's published SSH host keys. For other
Git hosts, mount a known hosts file that also names them:

```yaml
    volumes:
      - ./ssh_known_hosts:/etc/ssh/ssh_known_hosts:ro
```

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
controls the host: a manager, anyone who gets past sign-in, and every
template hook and `initializeCommand`, which run inside the Menagerie
container. This is the access a manager has when running devcontainers on
the host by hand. Keep these limits in place:

- Only members of `menagerie_managers` reach the UI. Membership is checked
  at every sign-in; remove someone from the group and restart Menagerie to
  end their session at once.
- Publish the UI on a private address behind your HTTPS reverse proxy, never
  on the public internet.
- Approve only templates you trust; Menagerie uses only the templates its
  configuration lists.
- A job has no time limit. A hook or build that never ends keeps its
  workspace busy until Menagerie restarts.
- A backup is a force-push of the workspace's own branch. Commits pushed to
  that branch from elsewhere are lost at the next backup.
