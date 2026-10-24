# Contributing to Menagerie

Thank you for helping. This page covers how to propose a change and what
gets accepted. Everything that runs is a `task` verb; `task --list` shows them.

## Before you start

- Open an issue first for a new capability or a change in behavior, so we can
  agree on the shape before you write it. Small fixes can go straight to a
  pull request.
- Menagerie works from its technical design,
  [`docs/technical-designs/menagerie.yml`](docs/technical-designs/menagerie.yml),
  and the specifications in [`docs/specifications/`](docs/specifications/). A
  change that departs from them changes the document in the same pull request.
- Say "workspace" for the folder and its devcontainer, in code, API and UI.

## Development environment

The repository's devcontainer has every tool the checks use. Without it you
need Go, Rust, Node.js with pnpm, Task, Docker and the linters that
`.devcontainer/devcontainer.json` installs.

## Making a change

- Write tests with the change. The live suites run real services: a
  disposable Docker daemon, OpenBao, Pocket ID and the built image. They do
  not replace a real dependency with a mock.
- Test fixtures use generic names and generated keys only.
- Run `task check` before you push. It runs everything CI runs, so it needs
  Docker that may start privileged containers.
- A user-visible change carries a change intent written with `intentional add`.
  Do not edit `CHANGELOG.md`; releases write it.
- Commit messages and pull request titles are conventional commits. Pull
  requests are squash-merged, so the title becomes the commit on `main`.

## Releases

A maintainer releases by dispatching the release workflow for a commit on
`main` that passed CI. Nobody tags or publishes by hand.
