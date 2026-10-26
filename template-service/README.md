# Template service

The template service is Menagerie's Rust service. It embeds
[Toha](https://github.com/wyrd-company/toha) as a library and serves create
interviews, workspace generation and upgrades to the workspace service. It uses
JSON over HTTP on a private Unix socket. It keeps no state between requests:
each request carries Toha's whole interview session, signed with the key in
`session-key` under the data folder. The service refuses a session it did not
sign for that workspace, template and flow, and rebuilds the interview from a
session it did, so an interview continues after a restart.

The API is specified in
[`docs/specifications/template-service-api.yml`](../docs/specifications/template-service-api.yml).

## Running

```sh
menagerie-template-service -config /etc/menagerie/config.yml -data /var/lib/menagerie/toha
```

- `-config` is Menagerie's configuration file. The service reads only
  `workspace-root`, `templates`, `template-service.socket`,
  `template-service.hook-runner-socket` and
  `template-service.candidate-folder` from it. The candidate folder is the
  service's temporary folder, where Toha builds an upgrade's candidate.
- `-data` holds Toha's configuration, registry, template cache and state.

The service runs no hook itself. Its Toha hook runner (`src/hooks.rs`) sends
each hook, with the token of its generate or apply request and Toha's
target, to the workspace service's hook runner socket; the workspace
service runs it in a throwaway container against that target (the
workspace folder for a generate, the candidate for an upgrade) and returns
its exit code and captured streams.

Private templates are fetched in one of two ways:

- Over SSH, by Toha's Git transport, which runs the image's `ssh` client
  with `SSH_AUTH_SOCK`.
- Over `https` with a GitHub App installation token. Each request runs its
  Toha work inside one Toha credential resolver scope (`src/credentials.rs`).
  When an `https` remote asks for credentials, the resolver posts the URL to
  the credential route on the workspace service's hook runner socket. The
  route answers with a read token for that one repository. If the route
  answers 404, the fetch keeps Toha's default behaviour. If the route cannot
  mint a token, the fetch fails with the route's reason. The token stays in
  memory for the fetch, and no error or log line carries it.

The image builds curl with rustls. Toha's transport then trusts only the
certificates in `SSL_CERT_FILE` or `SSL_CERT_DIR` when either is set, and
the platform's otherwise. A system libcurl keeps its own trust.

## Checks

Run from this folder:

```sh
task check
```

`task check` checks formatting and runs Clippy, the dependency check and every
test. The dependency check confirms that Toha resolves with no features, so
no CLI crate enters the build. The tests run the service on a Unix socket
against Toha's library code and generic local Git template repositories. The
credential tests also serve those repositories over TLS behind basic
authentication, and play the credential route on the hook runner socket.

Toha is pinned in `Cargo.toml` to a full commit of its public repository, with
default features off.
