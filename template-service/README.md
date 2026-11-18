# Template service

The template service is Menagerie's Rust service. It embeds
[Toha](https://github.com/wyrd-company/toha) as a library and serves the
workspace service: it reads template addresses, lists and checks template
versions, and runs create interviews, workspace generation and upgrades. It
uses JSON over HTTP on a private Unix socket. It holds no list of templates:
the workspace service names each template by its address and the version's
ref in every request, and the service refuses an address it must never read,
such as a local folder or a `file://` repository (`src/address.rs`). It keeps
no state between requests: each request carries Toha's whole interview
session, signed with the key in `session-key` under the data folder. The
service refuses a session it did not sign for that workspace, address, ref
and flow, and rebuilds the interview from a session it did, so an interview
continues after a restart.

The API is specified in
[`docs/specifications/template-service-api.yml`](../docs/specifications/template-service-api.yml).

## Running

```sh
menagerie-template-service -config /etc/menagerie/config.yml -data /var/lib/menagerie/toha
```

- `-config` is Menagerie's configuration file. The service reads only
  `workspace-root`, `template-service.socket`,
  `template-service.hook-runner-socket` and
  `template-service.candidate-folder` from it, and nothing of
  `templates`. The candidate folder is the
  service's temporary folder, where Toha builds an upgrade's candidate, in
  its subfolder `.menagerie-work`. At start-up, once its socket is bound and
  before its first request, the service removes what an ended service left
  in that subfolder, unless another service holds its lock. A service that
  cannot take the lock does not start (`src/candidates.rs`).
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
  route answers with a read token for that one repository. The URL carries
  no user name, so a user name in a template address does not opt out of
  the App. If the route answers 404, the fetch keeps Toha's default
  behaviour. If the route cannot mint a token, the fetch fails with the
  route's reason. The token stays in memory for the fetch. When a request
  fails after the resolver handed out a token, the reply carries a fixed
  sentence for its error kind and no text from Toha, gix or the Git host,
  which could echo the token in forms no redaction can list. Every reply
  also passes one boundary in `src/server.rs` that removes each written
  form of the handed-out tokens. When the workspace service's request ends
  early, the resolver's credential request ends with it. Toha refuses a Git
  URL that holds a password; use the credential route or SSH agent instead.

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

Toha comes from crates.io as `toha = "0.5"`, with default features off.
Every request worker runs inside `toha::stop::with_stop_signal`; dropping
the request fires its signal, including during template loads, Git fetches,
generation and upgrade preparation. See the
[time limit reference](https://wyrd.foo/docs/menagerie/configuration#job-time-limits)
for the transport and name lookup limits.

The stop regressions prove that a stalled Git fetch returns the request worker,
that all six routes close a stalled schema connection, and that a stop between
file writes prevents the next file. To prove the guards by removing each one in
a disposable source copy, run from the repository root:

```sh
python3 scripts/prove-template-stop-guards.py --report stop-guards.json
```

The report passes only after its baseline tests pass and every compiling mutant
fails its regression test.

The admission regressions prove that every route refuses an address it must
never read, that a template folder or link leaving its checkout is refused
before a start, a generate or an apply reads it, that a ref resolving to
another commit than the recorded one is stale, and that a session cannot be
replayed into another ref. To prove each guard by removing it in a
disposable source copy, run from the repository root:

```sh
python3 scripts/prove-template-admission-guards.py --report admission-guards.json
```
