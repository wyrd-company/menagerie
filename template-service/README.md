# Template service

The template service is Menagerie's Rust service. It embeds
[Toha](https://github.com/wyrd-company/toha) as a library and serves create
interviews, workspace generation and upgrades to the workspace service. It uses
JSON over HTTP on a private Unix socket. It keeps no state between requests:
each request carries Toha's whole interview session. The service rebuilds the
interview from that session, so an interview continues after a restart.

The API is specified in
[`docs/specifications/template-service-api.yml`](../docs/specifications/template-service-api.yml).

## Running

```sh
menagerie-template-service -config /etc/menagerie/config.yml -data /var/lib/menagerie/toha
```

- `-config` is Menagerie's configuration file. The service reads only
  `workspace-root`, `templates` and `template-service.socket` from it.
- `-data` holds Toha's configuration, registry, template cache and state.

The service runs template hooks inside its own process environment, with Toha's
process hook runner. Private templates are fetched over SSH by Toha's Git
transport, which runs the image's `ssh` client with `SSH_AUTH_SOCK`.

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
