---
title: Installation
order: 2
docs: true
relationships:
  references:
    - menagerie
    - configuration
    - authentication
    - menagerie-has-host-root
    - buildkit-traffic-not-inspectable
    - proxy-ignores-unknown-config-keys
---

Menagerie runs as one container on a Linux host with Docker Engine, on
`amd64` or `arm64`. The image is `ghcr.io/wyrd-company/menagerie:<version>`.
Beside it runs a Docker proxy, Sockguard
(`ghcr.io/wyrd-company/sockguard:<version>`): Menagerie reaches the host's
Docker daemon only through the proxy, which admits what Menagerie's
profile allows and denies the rest. Both use Unix sockets and host paths,
so there is no standalone service for macOS or Windows.

Before the container starts, you need:

- an OpenID Connect provider with a public client for Menagerie and a group
  named `menagerie_managers`;
- a Git repository that Menagerie may push workspace backups to, and a Git
  identity for it that Wyrwood holds as an OpenBao Transit key: a GitHub
  App, an SSH key, or both (see [Git identity](#git-identity));
- a Wyrwood agent on the host, and a policy for Menagerie;
- the workspace root: one folder on the host for every workspace, at a
  path with no symbolic link in it.

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
edit `config.yml` and `sockguard.yml`; [The Docker proxy](#the-docker-proxy)
explains the second.

| Mount or setting | Why |
| --- | --- |
| The workspace root at the same path on the host and in the container | Devcontainers bind-mount their workspace folder. The host daemon resolves that path, so it must be the host path. Set `MENAGERIE_WORKSPACE_ROOT` and `workspace-root` to the same value. |
| The workspace socket folder at the same path on the host and in the container | Each devcontainer mounts its own socket folder from it, so it must be the host path. Set `MENAGERIE_WORKSPACE_SOCKETS` and `workspace-socket.folder` to the same value; leave both out to run without [workspace sockets](configuration.md#workspace-sockets). |
| The Compose staging folder at the same path on the host and in the container | A Compose workspace's services bind-mount their configs and secrets from it, so it must be the host path. Set `MENAGERIE_COMPOSE_STAGING` and `compose-staging-folder` to the same value. |
| `DOCKER_HOST=unix:///run/sockguard/menagerie.sock` and the proxy's socket volume at `/run/sockguard`, read-only | The workspace service drives the host Docker daemon through the proxy: devcontainers, and the throwaway containers that run template hooks and `initializeCommand`. Menagerie never mounts `/var/run/docker.sock`. |
| The exceptions folder at `/etc/sockguard/exceptions`, read-only | The [workspace exceptions](#workspace-exceptions) the proxy applies. Menagerie never writes it. |
| The Wyrwood consumer directory at `/run/wyrwood` | `vault.sock` and `ssh-agent.sock`. `wyrwood-directory` names the mount target. |
| `SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock` | With an SSH identity: Toha's Git transport runs the image's `ssh` client with this agent. Leave it out with only a GitHub App. |
| `config.yml` at `/etc/menagerie/config.yml` | Both services read it. |
| `SSL_CERT_FILE` and the bundle it names | Optional: a Git server whose certificate a private authority signed. See [Git hosts](#git-hosts). |
| A volume at `/var/lib/menagerie` | Toha's template cache, the template service's session key, the services' home and the folder Toha builds an upgrade's candidate in. Without it they are lost when the container is recreated. |
| `restart: unless-stopped` | Docker starts Menagerie after a host restart, and restarts it after either service stops. |
| Port `8080` | The web UI and workspace API. Publish it on a private address only. |

Create the workspace root, the socket folder and the Compose staging
folder before the first start, the last two owned by the workspace root's
owner. The proxy mounts all three with `create_host_path: false`, so
Docker does not create a missing one empty and owned by root: the proxy's
container fails to start, and so does Menagerie, which starts after it.
The proxy itself refuses to start when one of its bind roots, or the
exceptions' workspace root, is missing in its mount namespace, rather than
check bind sources by their path alone:

```sh
owner="$(stat -c %u /srv/menagerie/workspaces)"
group="$(stat -c %g /srv/menagerie/workspaces)"
sudo install -d -m 0700 -o "$owner" -g "$group" /srv/menagerie/sockets
sudo install -d -m 0700 -o "$owner" -g "$group" /srv/menagerie/compose-staging
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
directory's group; the workspace service also has the group of the Docker
proxy's socket, which `DOCKER_HOST` names. The template service never
talks to Docker, and without that group it cannot reach the proxy's
socket. At start-up Menagerie waits up to 30 seconds for the proxy to bind
its socket. For a non-root owner:

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
host daemon, through the proxy, which need no extra mount or setting. By
default they run Menagerie's own image, which Menagerie finds by
inspecting its own
container: by the container id in its mounts or, when they name none, by
its hostname, Docker's default. When it cannot, a job with a hook or an `initializeCommand`
fails as `runner-image-unavailable`; set
[`runner-image`](configuration.md#runner-images). Each container carries
the labels `menagerie.throwaway` and `menagerie.throwaway.workspace`, never
`devcontainer.local_folder`, so no workspace exception applies to it. It is
removed when its command ends; any left by a stopped Menagerie are removed
when it starts.

### Container registries

Menagerie pulls and builds devcontainer images anonymously unless
`registry-credentials` names a secret for the registry. It never reads a
Docker client configuration or credential helper. A private image, or a
Docker Hub base image on a host that reaches Docker Hub's anonymous pull
limit, needs an entry for that registry; for Docker Hub use `docker.io` and
a Docker Hub access token as the password. See
[Configuration](configuration.md#registry-credentials).

The proxy pulls only from the registries its profile lists: add each
registry your workspaces' images, base images and runner images come from
to both registry lists in `sockguard.yml`. A pull from another registry
fails as `proxy-denied`.

## The Docker proxy

The `sockguard` service in `deploy/compose.yml` is the Docker proxy. It is
the only container that mounts `/var/run/docker.sock`. It runs as root with
the `DAC_READ_SEARCH` capability only, no network and a read-only file
system. Root reaches the Docker socket; `DAC_READ_SEARCH` lets it resolve
a bind source below a folder it could not otherwise read, such as the
workspace socket folder, which is `0700` and the services' user's. As the
image's own user, every devcontainer that mounts its workspace socket
would be denied. It is in the listener socket's group (`group_add`), so it
gives the socket that group without `CHOWN`. `deploy/sockguard.yml` is its
configuration, with one Unix listener and one profile, `menagerie`.

| Proxy mount | Why |
| --- | --- |
| `/var/run/docker.sock` | The host Docker daemon. |
| The `menagerie-proxy` volume at `/run/sockguard` | The listener socket `menagerie.sock`, which Menagerie mounts read-only. Menagerie's container starts after the proxy's (`depends_on` with `condition: service_started`): the proxy's image has no health check, so Menagerie's start-up waits for the socket itself. |
| `sockguard.yml` at `/etc/sockguard/sockguard.yaml`, read-only | The configuration. |
| The exceptions folder at `/etc/sockguard/exceptions`, read-only | The folder, not the file, so the proxy sees a replaced file. |
| The workspace root, the workspace socket folder and the Compose staging folder, each read-only at its host path, never created | The proxy resolves every bind source in its own mount namespace, as the daemon does. |

Edit these values of `sockguard.yml` to match the host:

| Setting | Value |
| --- | --- |
| `clients.unix_peer_profiles[0].uids` | The workspace root's owner: `stat -c %u /srv/menagerie/workspaces`. The proxy selects Menagerie's profile by the connecting process's uid; any other uid is denied. |
| `listeners[0].socket_gid` | A group no Menagerie service has otherwise: not the workspace root's group. Menagerie gives it to the workspace service only. Set the proxy's `group_add` in `compose.yml` to the same number. |
| `allowed_bind_mounts` | The workspace root, the workspace socket folder and the Compose staging folder: the values in `config.yml`. |
| `workspace_exceptions.workspace_root` | The workspace root. |
| `image_pull.allowed_registries` and `buildkit.session.auth.allowed_registries` | Every registry the workspaces pull from. |

Sockguard ignores keys it does not know: a misspelled key keeps its
default without an error. Change values of the shipped file, not keys.
After an edit, restart the proxy with `docker compose restart sockguard`.

### What the profile admits

- Devcontainers: build, create, start, stop, exec and remove, for single
  containers and Compose. Builds run on the daemon's BuildKit through the
  proxy. The build's own requests pass the proxy uninspected; the proxy
  still denies insecure entitlements and the host network for a build.
- Throwaway containers: create, attach, wait, copy in and out, and remove.
- Pulls from the listed registries. Image push, save and load, and every
  path the profile does not name, are denied.
- Bind mounts from the workspace root, the socket folder and the staging
  folder. A devcontainer binds only its own workspace's folders; another
  workspace's folder or socket folder is denied.
- No privileged container, no host network, PID or IPC namespace and no
  restart policy, unless a workspace exception grants it.

The profile sets the proxy-wide `insecure_allow_read_exfiltration: true`.
It admits `POST /containers/{id}/attach` and `GET
/containers/{id}/archive`, requests that read a container's output and
files, on the containers the proxy marks as Menagerie's. Throwaway
containers need both: attach streams a hook's or `initializeCommand`'s
output to the job console, and the archive read copies an upgrade's
candidate back out. Without it the proxy refuses the profile. Menagerie is
the proxy's only client.

Every container, network and volume Menagerie creates carries the proxy's
owner label, and Menagerie sees only those: a container that another tool
started is not in Menagerie's lists. Each request also names its workspace
in a header that Menagerie sets from the workspace root and the
workspace's name, never from the workspace's definition. A definition that
sets a label Menagerie or Compose uses to find containers
(`devcontainer.local_folder`, `devcontainer.config_file`, `menagerie.*`,
`com.docker.compose.*` or `com.sockguard.*`), in `runArgs` or in a Compose
service's `labels:`, fails as `identity-label-refused` before anything
runs.

A denied request fails the job as `proxy-denied`. The job's error names
the proxy's reason, such as `container create denied: privileged
containers are not allowed` or the bind source that is outside the
allowed folders.

### Compose workspaces

Compose workspaces work through the proxy, with these limits:

- `extra_hosts` and `group_add` are always denied, and so are fixed
  addresses (`ipv4_address`, `ipv6_address`) and `mac_address`.
- `restart:` is denied unless the workspace's exception grants that
  restart policy. Menagerie never restarts a container itself.
- Only the primary service carries `devcontainer.local_folder`, so only it
  gets the workspace's exception. A sidecar service gets one only when its
  Compose file sets that label on it, which Menagerie refuses: a sidecar
  needs no grant.
- A service that mounts a Compose config or secret mounts it from the
  staging folder. With an exception, add the staging folder to its
  `extraBindSources`.
- Compose networks stay on the host after a workspace's devcontainer is
  removed; remove them with `docker network rm`.

### Workspace exceptions

A workspace whose definition needs more than the profile admits, such as a
privileged container or a bind mount of a host path outside the allowed
folders, needs an exception. You, the operator, write the exceptions file
`sockguard-exceptions/workspace-exceptions.json`; neither Menagerie nor
the proxy writes it.

```json
{
  "version": 1,
  "exceptions": [
    {
      "workspace": "/srv/menagerie/workspaces/alpha",
      "grants": {
        "privileged": true,
        "extraBindSources": ["/var/run/docker.sock"],
        "restartPolicy": ["unless-stopped"]
      },
      "approval": {
        "by": {"issuer": "https://id.example.org", "subject": "user-0001", "name": "Manager One"},
        "at": "2026-01-01T09:00:00Z",
        "note": "docker-in-docker feature"
      }
    }
  ]
}
```

- `workspace` is the workspace's folder: the workspace root and the
  workspace's name, with no symbolic link and no trailing slash.
- `grants` opens gates for that workspace only. A grant that is absent is
  not granted. The grants are `privileged`, `capAdd`, `namespaces`,
  `devices`, `deviceCgroupRules`, `gpus`, `extraBindSources`,
  `securityOpt`, `runtimes`, `sysctls` and `restartPolicy`; Sockguard's
  workspace exceptions documentation describes each.
- `approval` records who approved the entry and when: the OIDC issuer and
  subject of the approver, a UTC time ending in `Z`, and an optional name
  and note. The proxy checks their shape, not their truth.

To grant, add or change an entry. To revoke, remove the entry or a grant.
Write the whole file to a temporary file in the same folder and rename it
over `workspace-exceptions.json`, so the proxy never reads half a file.
The proxy reloads it at once; an invalid file means no exceptions until a
valid one is written.

A grant applies when the proxy creates a container. A revoked grant ends
for the workspace at its next rebuild, which creates the container again;
until then the existing container keeps it. Stop the workspace to stop
using the grant at once, and rebuild it before the next start.

## Upgrades

Pull the new image tag and recreate the container. Running jobs end with the
container, and job history starts empty; workspace folders, their answers in
`.toha.json`, their containers and the sign-in configuration remain.
Everyone signs in again.

## What Menagerie can do to the host

Menagerie does not mount the Docker socket. It reaches the daemon through
the Docker proxy, so what Menagerie can do to the host is what the proxy's
profile and the exceptions file admit:

- A devcontainer, and so a definition in a workspace, gets no privileged
  mode, no host network, PID or IPC namespace, no added capability or
  device, no restart policy and no bind mount outside its own workspace's
  folders, unless an exception names that workspace. An exception is a
  grant of host access to that workspace's definition: anyone who can
  change the definition, in the workspace or through a backup branch
  Apply branch brings in, uses it.
- A workspace cannot mount another workspace's folder or socket folder,
  and Menagerie names each request's workspace from its own root and the
  workspace's name, so a definition cannot pass itself off as another
  workspace.
- Builds still run on the host daemon's BuildKit. A build's steps pass the
  proxy uninspected; the proxy keeps the build from the host network and
  from insecure entitlements, and BuildKit enforces those. A build step
  runs code from the definition's Dockerfile and features with the
  network the daemon gives builds.
- Menagerie can read the output and files of the containers it created,
  and run commands in them as root: that is how lifecycle commands, hooks
  and upgrades work.

Whoever controls Menagerie, a manager or anyone who gets past sign-in,
controls what this admits. Whoever can write the exceptions file, or the
proxy's configuration, controls the host. Template hooks and
`initializeCommand` run in throwaway containers that cannot reach the
proxy's socket, the Wyrwood sockets or Menagerie's environment; they can
write to their workspace folder and reach the network. Keep these limits
in place:

- Grant an exception only to a workspace whose definition you trust, and
  remove it when the workspace no longer needs it. Keep the exceptions
  file and `sockguard.yml` writable by the host's administrators only.
- Use the shipped `sockguard.yml` and change values, not keys: the proxy
  ignores a key it does not know.
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
