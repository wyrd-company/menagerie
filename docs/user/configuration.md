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
    runner-image: ghcr.io/example-org/sample-workspace-runner:2.0.0

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

auto-commit:
  every: 1h

workspace-socket:
  folder: /srv/menagerie/sockets
  min-interval: 1m

github-app:
  app-id: 123456
  transit-key: menagerie-github-app
  installations:
    example-org: 7890
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
An `https` remote on the [GitHub App](#github-app)'s server uses an
installation token.

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
- `https://<host>/<path>.git@<ref>#<path>`, with a
  [GitHub App](#github-app) token for private templates on its server.

`#<path>` names the template's folder in the repository and is optional.

An entry's optional `runner-image` is the image its hooks run in; see
[Runner images](#runner-images).

Pin each entry to a tag. To move workspaces to a new template version, add
an entry for that version; **Upgrade** then asks only the questions the new
version adds and keeps the answers and local edits the workspace has. The
list can be empty.

## Runner images

Template hooks and `initializeCommand` run in throwaway containers, one per
command. A container mounts only the workspace folder, at its own path, and
runs as the owner of the workspace root on Docker's default bridge network,
unable to gain privileges through setuid programs. An upgrade's hooks mount
nothing: they run in a copy of Toha's candidate, the upgraded checkout
Toha merges into the workspace afterwards, so they never write to the
workspace folder themselves.
It cannot reach the Docker socket, the Wyrwood sockets or Menagerie's
environment, and it is removed when its command ends.

- A template's hooks run in its `runner-image`, else the top-level
  `runner-image`.
- `initializeCommand` runs in the top-level `runner-image`.
- Without either, both run in Menagerie's own image, which has `sh`, `git`,
  `ssh` and `curl`.

The image needs the programs the hooks run; a hook runs its program as
given, found on the image's own `PATH`. Toha checks a template's
`requires` against Menagerie's own image, so a template with its own
runner image should list only programs both images have. Menagerie pulls
a missing image, with `registry-credentials` when they name its registry.
A hook gets no environment variables beyond its image's own, and no SSH
agent: a hook that fetches a private repository cannot authenticate.

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

## GitHub App

With `github-app`, Menagerie authenticates to GitHub with a GitHub App's
installation tokens, for repositories on the App's server whose path is
`/<owner>/<repository>` or `/<owner>/<repository>.git`:

- Backups, with `contents: write` on the repository, when
  `workspace-repository` is such an `https` URL with no user information,
  query or fragment. A `workspace-repository` with a user name gets no
  token and no other credential.
- Template fetches, with `contents: read` on the repository, from every
  `https` address of such a repository. A user name in the address does
  not opt out: the template service asks for the token by the address
  without it, and the fetch sends the token in its place.

Every other URL keeps its own authentication: SSH URLs use the Wyrwood
agent, and Menagerie gives other `https` URLs no credential. Both can be
configured, for example backups and templates on GitHub through the App
and templates on another host over SSH.

| Key | Value |
| --- | --- |
| `app-id` | The App's id. |
| `transit-key` | The Transit key, at the mount `transit`, that holds the App's private key: letters, digits, `_`, `.` and `-`. |
| `installations` | Optional. The App's installation id by account name, compared in lower case. A repository whose owner is not listed has its installation looked up. |
| `server` | Optional. The GitHub server's `https` origin; without it, `https://github.com`. The API is `https://api.github.com` for GitHub, `<server>/api/v3` for GitHub Enterprise Server. |

Install the App on the backup repository with Contents read and write, and
on each private template repository with Contents read. Import its private
key into OpenBao's Transit engine as an RSA key, as
[Installation](install.md#github-app) shows; the Wyrwood policy grants
`update` on `transit/sign/<transit-key>/sha2-256`. For a `server` whose
certificate a private authority signed, see
[Git hosts](install.md#git-hosts). Menagerie signs the App's
JWT through Wyrwood's `vault.sock`, sending no token, and exchanges it for a
token that covers one repository and one permission. Tokens stay in
Menagerie's memory until five minutes before they expire; nothing is written
to disk. A push or fetch with a token follows no redirect: set
`workspace-repository` to the repository's current address. A failure names
a fixed reason, never a token, a URL or GitHub's answer, and the job console
shows the remote's own text with the URL and the token replaced by
`[redacted]`.

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
- `initializeCommand` gets only the listed variables, beside its runner
  image's own environment. Its `PATH` is always the image's: a listed
  `PATH` is readable through `${localEnv:PATH}` but never reaches
  `initializeCommand`.
- `build.args` reach the build as written: devcontainer-builder does not
  substitute variables in them yet.

## Job time limits

Without `job-limits` a job runs until it ends. With it, a job past a limit
is cancelled and fails as `timed-out`, and its workspace takes the next job
once the cancelled work has stopped.

- `default` limits every operation.
- `operations` gives `start`, `stop`, `rebuild`, `create`, `upgrade`,
  `commit` or `apply-branch` its own limit, which replaces `default`.
- `phases` limits each stay in a phase, inside any operation:
  `read-definition`, `initialize-command`, `build`, `remove-container`,
  `up`, `stop`, `interview`, `initialize-repository`, `generate`,
  `fetch`, `plan`, `apply`, `commit`, `backup-local-changes` or
  `backup`. Template hooks run inside `generate` and `apply`; a hook
  still running when its job is cancelled has its container killed and
  removed before the job ends.

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

## Automatic commits

With `auto-commit`, Menagerie checks every workspace each `every`, the first
time one interval after it starts. A workspace with uncommitted changes and
no job in progress gets a commit job: every change is committed, as
`git add --all` and a commit would, with the message
`chore(menagerie): automatic commit`, and the workspace's branch is backed
up. A workspace with a job in progress is skipped until the next check.
Files the workspace's ignore rules exclude, and repositories cloned inside
it, are not committed. The ignore rules are the workspace's `.gitignore`
files, its `.git/info/exclude`, and the file its repository's own
`core.excludesFile` names when that file is a regular file of at most
1 MiB inside the workspace folder, with symbolic links resolved. Any other
exclude file, elsewhere or larger, is skipped with a note in the job's
console, and the files it excludes can be committed.
Menagerie's own Git configuration does not apply. Without `auto-commit`
nothing is committed on a schedule.

## Workspace sockets

With `workspace-socket`, every workspace's devcontainer can commit and back
up its own workspace with `workspace-commit`, as
[Usage](usage.md#commit-from-inside-a-workspace) shows. Menagerie keeps one
folder per workspace in `folder`, with a Unix socket and the client, and
mounts it read-only at `/run/menagerie` in that workspace's devcontainer.
The socket only knows its own workspace, and needs no token.

- `folder` is an absolute folder that is mounted at the same path on the
  host and in Menagerie's container, like the workspace root. It must exist
  before Menagerie starts and belong to the owner of the workspace root,
  the user the services run as; Menagerie sets its mode to 0700. It must
  not hold a comma, it must not itself be a symbolic link, and every
  folder above it must belong to root or that user and be writable by no
  other user unless it is sticky, as `/tmp` is. A symbolic link above it,
  such as `/var/run` to `/run`, is followed. It must be neither inside nor
  around the workspace root, the Wyrwood directory or the candidate
  folder; the workspace root is also compared with both paths' symbolic
  links resolved.
- `min-interval` is the shortest time between two commits of one workspace
  through its socket, `1m` by default; `0` allows any number. A request
  that comes sooner is refused with the time to wait, so a loop in a
  devcontainer cannot fill its backup branch.

A devcontainer gets the socket when its container is created or rebuilt.
A start keeps a container that was created without the socket as it is,
so it is not recreated, and the job's console notes that the socket
attaches at the next rebuild. A start that recreates the container for
another reason, such as a changed `containerEnv`, also creates it without
the socket, and the console says so; the socket attaches at the next
rebuild. A Compose workspace gets the socket on its primary service only.

Each workspace's socket takes at most 32 connections at once, and all
sockets together 256. A request has 10 seconds for its header and 10 for
its body, and an idle connection is closed after 60 seconds; the wait for
the commit job is bounded only by the `commit` [job time limit](#job-time-limits).

## Listen address and template service

`listen` is the HTTP address, `:8080` by default. `template-service.socket`
is the template service's Unix socket, `/run/menagerie/template.sock` by
default. When you change either, set `MENAGERIE_HEALTH_URL` or
`MENAGERIE_TEMPLATE_SOCKET` in the container's environment to match, so the
health check asks the right place. A changed socket must be in a folder only
the services' user can enter.

`template-service.candidate-folder` is the template service's temporary
folder, `/var/lib/menagerie/candidates` by default, where Toha builds an
upgrade's candidate. Menagerie runs an upgrade's hooks only against a
folder inside it. It must be an absolute folder other than `/`, neither
inside `workspace-root` nor containing it, and only the services' user may
enter it; the template service creates it when it is missing.
