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

Intentional versions Menagerie and writes `CHANGELOG.md`; tags are bare
versions such as `0.1.0`. To release, on an up-to-date `main`:

```sh
task release
git push --atomic --follow-tags origin main
```

`task release` applies the pending intents, commits them as
`chore(release): <version>` and tags that commit with Intentional's release
record. The first release also tags its parent as the `0.0.0` baseline. The
tag push starts the CD workflow, which delivers only Intentional release
records: it pushes `ghcr.io/wyrd-company/menagerie:<version>` for linux/amd64
and linux/arm64. `task release:rehearse` runs the same steps in a scratch
clone and changes nothing. A release whose pending intents are all for the
`menagerie-docs` unit tags only that unit (`menagerie-docs@<version>`); CD
delivers no image for it.

The Publish docs workflow then publishes `docs/user/` to wyrd.foo through
wyrd.foo's reusable workflow: when CD succeeds for a release tag, and when a
`menagerie-docs@*` tag is pushed. The publisher reads the release tag's
GitHub Release, so create that Release first; to publish a tag or commit
again, dispatch Publish docs with its `ref`. A push to `main` does not
publish.
