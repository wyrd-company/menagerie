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
its exit code and captured streams. Private templates
are fetched over SSH by Toha's Git transport, which runs the image's `ssh`
client with `SSH_AUTH_SOCK`.

## Checks

Run from this folder:

```sh
task check
```

`task check` checks formatting and runs Clippy, the dependency check and every
test. The dependency check confirms that Toha resolves with no features, so
no CLI crate enters the build. The tests run the service on a Unix socket
against Toha's library code and generic local Git template repositories.

Toha is pinned in `Cargo.toml` to a full commit of its public repository, with
default features off.
