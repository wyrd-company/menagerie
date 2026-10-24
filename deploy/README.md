# Deployment example

`compose.yml` runs Menagerie as one container on a Linux host with Docker.
`config.yml` is its configuration. Copy this folder, edit both, then:

```sh
WYRWOOD_CONSUMER_DIRECTORY=/path/to/consumer/directory docker compose up -d
```

## What the container needs

| Mount or setting | Why |
| --- | --- |
| The workspace root at the same path on the host and in the container | Devcontainers bind-mount their workspace folder. The host daemon resolves that path, so it must be the host path. Set `MENAGERIE_WORKSPACE_ROOT` and `workspace-root` to the same value. |
| `/var/run/docker.sock` | The workspace service drives the host Docker daemon. The template service does not use Docker. |
| The Wyrwood consumer directory at `/run/wyrwood` | `ssh-agent.sock` signs Git pushes and private template fetches; `vault.sock` reads registry credentials from OpenBao. `wyrwood-directory` names the mount target. |
| `SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock` | Toha's Git transport runs the image's `ssh` client with this agent. |
| `config.yml` at `/etc/menagerie/config.yml` | Both services read it. |
| A volume at `/var/lib/menagerie` | Toha's template cache and state. Without it they live in the container and are lost when it is recreated. |
| `restart: unless-stopped` | Docker starts Menagerie after a host restart, and restarts it after either service stops. |
| Port `8080` | The web UI and workspace API. Publish it on a private address only. |

Bind the Menagerie container to a Wyrwood policy that grants the workspace
repository's deploy key and read access to the configured registry
credential secrets. Register Menagerie with the identity provider as a public
client whose redirect URI is `oidc.redirect-url`.

## Inside the container

s6-overlay supervises the two services. A setup step makes
`/run/menagerie`, the template service's socket folder, accessible to root
only. The template service starts first; the workspace service follows.

When either service stops, the container stops with that service's exit
code (`128 + n` for signal `n`), and the log names the service. The restart
policy then restarts both. `docker stop` stops both services and the
container exits `0`. Running jobs end with the container; workspace folders,
`.toha.json` answers and containers remain.

The image's health check asks both services: the template service on its
socket and the workspace service at `http://127.0.0.1:8080/healthz`. If
`listen` or `template-service.socket` differ from these defaults, set
`MENAGERIE_HEALTH_URL` or `MENAGERIE_TEMPLATE_SOCKET` to match. A changed
socket must be in a folder that only root can enter.

Both services run as root. Template hooks and `initializeCommand` run inside
this container with the image's tools: a POSIX shell, `git`, `ssh` and
`curl`. `/etc/ssh/ssh_known_hosts` holds GitHub's published SSH host keys;
mount another file there for other Git hosts.

## Platforms

The image is built for `linux/amd64` and `linux/arm64`. Menagerie runs only
as this Linux container on a host with a Docker Engine; both services need
the Unix socket and the Docker daemon the container mounts. There is no
standalone service for macOS or Windows.
