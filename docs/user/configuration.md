---
title: Configuration
order: 3
product: menagerie
relationships:
  references:
    - configuration
    - authentication
---

Both services read one YAML file, `/etc/menagerie/config.yml`. Unknown keys,
a second document and an empty file are errors, and the container does not
start. [The schema](https://github.com/wyrd-company/menagerie/blob/main/docs/specifications/assets/configuration/menagerie.schema.json)
is the complete reference.

```yaml
workspace-root: /srv/menagerie/workspaces
workspace-repository: ssh://git@github.com/example-org/workspace-backups.git
wyrwood-directory: /run/wyrwood

oidc:
  issuer: https://id.example.test
  client-id: menagerie
  redirect-url: https://menagerie.example.test/auth/callback

templates:
  - id: sample-workspace-1
    name: Sample workspace 1.0
    address: gh:example-org/workspace-templates@1.0.0#sample-workspace
  - id: sample-workspace-2
    name: Sample workspace 2.0
    address: gh:example-org/workspace-templates@2.0.0#sample-workspace

registry-credentials:
  kv-mount: kv
  registries:
    - {registry: ghcr.io, path: menagerie/ghcr}
    - {registry: docker.io, path: menagerie/docker-hub}
```

## Workspaces

`workspace-root` is the absolute path of the folder that holds the
workspaces, the same path on the host and in the container. Every folder
directly in it is a workspace, whether Menagerie created it or not, and it
is never `/`. Menagerie finds a workspace's container by the
`devcontainer.local_folder` label and shows no other container.

`workspace-repository` is the Git remote for backups. Each workspace is
pushed, by force, to an orphan branch named after its folder. SSH remotes
use the Wyrwood agent and the known hosts in `/etc/ssh/ssh_known_hosts`.

`wyrwood-directory` is where the Wyrwood consumer directory is mounted in
the container, `/run/wyrwood` in the deployment example.

## Templates

`templates` is the approved list; a manager can use only these. Each entry
has a unique `id` (lower-case letters and digits, separated by single
dashes), a display `name`, and a Toha `address`:

- `gh:<owner>/<repository>@<ref>#<path>`, a GitHub repository over HTTPS,
  for public templates;
- `git@<host>:<path>.git@<ref>#<path>` or
  `ssh://git@<host>/<path>.git@<ref>#<path>`, over SSH with the Wyrwood
  agent, for private templates;
- `https://<host>/<path>.git@<ref>#<path>`.

`#<path>` names the template's folder in the repository and is optional.

Pin each entry to a tag. To move workspaces to a new template version, add
an entry for that version; **Upgrade** then asks only the questions the new
version adds and keeps the answers and local edits the workspace has. The
list can be empty.

## Identity provider

`oidc` names the provider and Menagerie's public client:

| Key | Value |
| --- | --- |
| `issuer` | The provider's issuer URL; its discovery document must name the same issuer. |
| `client-id` | The public client's id. There is no client secret. |
| `redirect-url` | The registered redirect URI, `https://<host>/auth/callback`. Its origin is the address browsers use; `https` makes every cookie `Secure`. |
| `scopes` | Optional. Must include `openid`. Without it Menagerie asks for `openid profile groups`. |

Only members of the group `menagerie_managers` get a session. Membership is
read at sign-in from the ID token's `groups` claim, or from userinfo when
the ID token has none. Sessions live in Menagerie's memory: a restart signs
everyone out, and removing someone from the group takes effect at their next
sign-in, or at once after a restart.

## Registry credentials

Without `registry-credentials` every registry is anonymous. With it,
Menagerie reads a registry's username and password from OpenBao, through
Wyrwood's `vault.sock`, when a pull, build, feature or Compose service needs
that registry.

- `kv-mount` is a KV version 2 mount.
- `registries` lists `{registry, path}`. `registry` is a host or
  `host:port`; `docker.io`, `index.docker.io`, `registry-1.docker.io` and
  `https://index.docker.io/v1/` all name Docker Hub, and two entries cannot
  name one registry. `path` is the secret's path in the mount.
- The secret holds the string fields `username` and `password`.
- The Wyrwood policy grants `read` on `<kv-mount>/data/<path>` for each
  entry.

Menagerie sends no token and no namespace; Wyrwood adds the binding's token
and renews it. A registry no entry names is anonymous. When a credential
cannot be read, the job fails with `registry-credentials-unavailable` and one
of the reasons in [Troubleshooting](troubleshooting.md#registry-credentials).

## Listen address and template service

`listen` is the HTTP address, `:8080` by default. `template-service.socket`
is the template service's Unix socket, `/run/menagerie/template.sock` by
default. When you change either, set `MENAGERIE_HEALTH_URL` or
`MENAGERIE_TEMPLATE_SOCKET` in the container's environment to match, so the
health check asks the right place. A changed socket must be in a folder only
the services' user can enter.
