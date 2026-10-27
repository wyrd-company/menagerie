---
title: Configuration
order: 3
docs: true
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

local-env: [TZ, LANG]

job-limits:
  default: 2h
  operations:
    rebuild: 1h
  phases:
    initialize-command: 10m
    build: 45m

alerts:
  webhook: https://ntfy.example.test/menagerie
  long-running: 45m
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

## Local environment

A devcontainer definition reads Menagerie's own environment through
`${localEnv:NAME}` (and `${env:NAME}`). `local-env` lists the variables it
may read; without it, it reads none. Each entry is a variable name: letters,
digits and underscores, not starting with a digit, and each name once.

- A reference to any other variable resolves to its default value, such as
  `fallback` in `${localEnv:NAME:fallback}`, or to an empty string, as an
  unset variable does. The job log notes each one:
  `localEnv NAME is not in local-env; it resolves to its default value`.
- Compose interpolation of `${VAR}` in a workspace's Compose files sees only
  the listed variables.
- `initializeCommand` runs with only the listed variables and `PATH`. `PATH`
  is readable through `${localEnv:PATH}` only when it is listed.
- `build.args` reach the build as written: devcontainer-builder does not
  substitute variables in them yet.

## Job time limits

Without `job-limits` a job runs until it ends. With it, a job past a limit
is cancelled and fails as `timed-out`, and its workspace takes the next job
once the cancelled work has stopped.

- `default` limits every operation.
- `operations` gives `start`, `stop`, `rebuild`, `create` or `upgrade` its
  own limit, which replaces `default`.
- `phases` limits each stay in a phase, inside any operation:
  `read-definition`, `initialize-command`, `build`, `remove-container`,
  `up`, `stop`, `interview`, `initialize-repository`, `generate`, `apply`,
  `commit`, `backup-local-changes` or `backup`. Template hooks run inside
  `generate` and `apply`.

A duration is whole hours, minutes and seconds, such as `2h`, `45m` or
`1h30m`. A key without a value, such as `rebuild:` or `default: ""`, is an
error, also when an alias or a merge key gives it; leave the key out for
no limit. Limits count only the time
Menagerie works: while a job waits for answers to an interview, no limit
counts and the interview has no expiry.
The job console shows the limit that ends first and the time left.

## Alerts

With `alerts`, Menagerie posts one JSON message to `webhook` for every job
that fails, timed-out jobs included. With `long-running`, it also posts one
when a job's running time passes that duration. Ended and succeeded jobs
send nothing.

```json
{
  "event": "job-failed",
  "jobId": "4f1c...",
  "workspace": "sample-space",
  "operation": "rebuild",
  "phase": "build",
  "state": "failed",
  "error": { "kind": "timed-out", "message": "the build phase passed its 45m limit" },
  "console": "https://menagerie.example.test/?job=4f1c..."
}
```

`event` is `job-failed` or `job-long-running`; a long-running alert has
`"state": "running"` and `"error": null`. `console` opens the job's console
at the origin of `oidc.redirect-url`. The body suits a generic webhook such
as ntfy, Slack through a relay, or Dagu.

Menagerie makes one attempt per alert with a 10 second limit and does not
follow redirects. Alerts are posted one at a time in the order they were
raised, so a job's `job-long-running` alert arrives before its
`job-failed` alert. An alert never delays or fails a job: up to 64 alerts
wait for delivery, and an alert raised while 64 wait is dropped with a log
line. At shutdown Menagerie waits at most 10 seconds for waiting alerts. A
failed delivery writes one line to the container's log that names only
what failed: `timeout`, `transport error`, `redirect refused` or
`status 503`, for example. The webhook URL can carry a token,
so Menagerie never writes it to a log, an event or an error. A
configuration error names the field and what it expects, never the value
of the webhook or of a duration.

## Listen address and template service

`listen` is the HTTP address, `:8080` by default. `template-service.socket`
is the template service's Unix socket, `/run/menagerie/template.sock` by
default. When you change either, set `MENAGERIE_HEALTH_URL` or
`MENAGERIE_TEMPLATE_SOCKET` in the container's environment to match, so the
health check asks the right place. A changed socket must be in a folder only
the services' user can enter.
