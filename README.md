<!-- markdownlint-disable-next-line MD033 MD041 -->
<img src="docs/user/assets/logo.svg" alt="" width="120" align="right" />

# Menagerie

[![CI](https://github.com/wyrd-company/menagerie/actions/workflows/ci.yml/badge.svg)](https://github.com/wyrd-company/menagerie/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/wyrd-company/menagerie)](LICENSE)

Menagerie manages workspaces and fleets of development containers through a
web UI.

A workspace is a folder created from a [Toha](https://github.com/wyrd-company/toha)
template, together with the devcontainer it defines. Managers sign in with
your identity provider, answer a template's interview to create a workspace,
see every workspace and its container, start, stop and rebuild them, and
upgrade a workspace to a newer template version. An upgrade asks only the new
questions and keeps local edits. A reviewed branch, such as a pull request's
head, can be applied to a workspace. Every workspace is a Git repository that
Menagerie backs up to its own branch, on a schedule or when its
devcontainer asks through its own socket.
[devcontainer-builder](https://github.com/wyrd-company/devcontainer-builder)
builds and runs the devcontainers, and
[Wyrwood](https://github.com/wyrd-company/wyrwood) gives Menagerie its SSH
key or GitHub App key and registry credentials.

![Creating a workspace through its interview, then upgrading it](docs/user/assets/demo.gif)

Documentation is at [wyrd.foo/docs/menagerie](https://wyrd.foo/docs/menagerie),
published from [`docs/user/`](docs/user).

## Install

Menagerie is one container image for Linux hosts with Docker Engine, on
`amd64` and `arm64`:

| Method | Command |
| --- | --- |
| Container image | `docker pull ghcr.io/wyrd-company/menagerie:0.1.0` |
| From source | `task build:image` (tags `menagerie:dev`) |

[Installation](docs/user/install.md) covers the host, the identity provider and
the Wyrwood binding.

## Quickstart

Copy [`deploy/`](deploy) and set the paths and names in `config.yml`. Create
the container, bind the container `menagerie` to a Wyrwood policy so that
the agent makes its consumer directory, then start it:

```sh
cp -r deploy ~/menagerie && cd ~/menagerie
docker compose create
docker compose up -d
```

Open the address that `oidc.redirect-url` names and sign in as a member of
the `menagerie_managers` group. Choose **New workspace**, pick a template,
name the workspace and answer the interview. The job console shows each
step until the devcontainer runs.

## Development

Builds need Go, Rust, Node.js with pnpm, and Task. `task --list` names every
verb; `task check` runs what CI runs, including the live suites that need a
Docker Engine able to run privileged containers.
