# Menagerie: agent notes

Menagerie is one container image that runs a Go workspace service (web UI,
OIDC sign-in, Docker through devcontainer-builder, go-git backups) and a Rust
template service (Toha) under s6-overlay.

## Commands

Use `task` verbs: `task --list`. `task check` is CI. The live suites
(`task test:live`) need Docker that may run privileged containers.

## Conventions an agent gets wrong

- The managed thing is a **workspace** (`docs/terms/workspace.yml`), never a
  "devcontainer" or "project", in code, API, UI and docs.
- Application code never spawns Git, Docker, Toha or devcontainer-builder
  CLIs; it uses their libraries. Template hooks and `initializeCommand` run
  as throwaway containers through the Docker library, not as Menagerie
  processes.
- Builds and devcontainers in tests run on the disposable daemon from
  `scripts/isolated-docker.sh`, never on a shared daemon's BuildKit. Cleanup
  removes only objects carrying the run's own label.
- Test fixtures use generic names and generated keys. Never read real
  credentials.
- Versions live in Intentional tags without a `v`. Before 0.1.0 there is one
  intent; do not add another.

## Do not touch

- `CHANGELOG.md` (Intentional writes it), `.intentional/intents/` except
  through `intentional add`.
- Lockfiles (`go.sum`, `Cargo.lock`, `web/pnpm-lock.yaml`) except through
  their package managers.
- `web/dist/` is build output; only `.gitkeep` is tracked.

## Documentation

Design and specifications: `docs/technical-designs/`, `docs/specifications/`,
`docs/terms/`, `docs/risks/`. User pages (`docs: true`, published to wyrd.foo
through `docs/docs.yml`): `docs/*.md`. Deployment example: `deploy/`. Package
notes: `web/README.md`, `template-service/README.md`.
