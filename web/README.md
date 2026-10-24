# Menagerie web UI

The browser UI of the workspace service: the workspace list, the create
interview and the upgrade interview. It is a React and TypeScript single-page
app built with Vite. The Go workspace service serves the build and owns every
route under `/api` and `/auth`; the UI holds no state of its own besides the
current interview session.

## Screens

| Path                          | Screen                                                                         |
| ----------------------------- | ------------------------------------------------------------------------------ |
| `/`                           | Every workspace folder, its container state and its job; start, stop, rebuild. |
| `/create`                     | Choose an approved template and a workspace name, then answer the interview.   |
| `/workspaces/{name}/upgrade`  | Choose the template version, then answer the questions the upgrade asks.       |
| any of these with `?job={id}` | The same screen following that job's events.                                   |

## Contracts

- Workspaces, templates, jobs and interview jobs: `docs/specifications/workspace-api.yml`
  in the repository root (owned by the workspace service).
- How the browser answers interviews: [`docs/interview-form.yml`](docs/interview-form.yml).
- Sign-in: any `401` sends the browser to `/auth/login?return=<path>`;
  **Sign out** posts to `/auth/logout`.

## Commands

Node 24 and pnpm 11 (pinned by `packageManager`) are required.

```sh
pnpm install --frozen-lockfile
pnpm build          # type-checks, then writes dist/
pnpm test           # unit and component tests (Vitest, jsdom)
pnpm test:browser   # browser tests (Playwright, Chromium) against dist/
pnpm lint           # ESLint and Prettier
pnpm check          # all of the above, in order
```

`pnpm test:browser` needs a build first and the Chromium revision that
`@playwright/test` names (`pnpm exec playwright install chromium`).
Screenshots of every screen are written to `evidence/screenshots/`
(ignored by Git).

For local work, run `node test/fixture-server/main.ts` (port 4318) and
`pnpm dev`; Vite proxies `/api` and `/auth` to `MENAGERIE_URL`, which defaults
to that fixture server. Point `MENAGERIE_URL` at a running workspace service to
use the real one.

## Embedding

`embed.go` (package `web`) embeds `dist/` and exports `web.FS`, rooted at the
build output. `dist/.gitkeep` keeps the embed pattern valid before a build;
the Vite build writes it back after emptying `dist/`. Build the UI before the
Go binary to serve it.

## Test fixtures

`test/fixture-server/` is a test-only stand-in for the workspace service's
browser routes. It has no OIDC, Docker or Toha, and Menagerie never runs it.

`test/fixtures/toha/documents/` holds interview documents produced by a real
Toha build from the generic `sample-kit` template; `PRODUCER` names the build.
`generate.sh` reproduces them. Two documents are not plain producer output:

- `upgrade-batch-1.json` is the Toha CLI's staged update batch, which asks
  every question with the recorded answers as defaults.
- `upgrade-batch-uncovered.json` is that document narrowed by the generator
  to the one question the first version did not ask. It shows how the UI
  renders a one-question batch; it is not evidence of Toha's replay.

## Dependency notes

TypeScript stays on 6.0 because `typescript-eslint` 8.72 supports TypeScript
below 6.1 only. Move to TypeScript 7 when `typescript-eslint` supports it.
