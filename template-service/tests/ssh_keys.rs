//! A template whose credential is an SSH key offers only that key. A fake
//! SSH agent holds one generated key, and a fake `ssh` program records the
//! options each connection runs it with instead of reaching a host. Both are
//! named in the environment of this test binary alone.
mod common;
use common::*;
use serde_json::{Value, json};
use std::io::{Read, Write};
use std::path::PathBuf;
use std::sync::OnceLock;

/// Generated public keys: the agent holds the first only.
const HELD: &str =
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBHK2Ry9R6NHMnm7k0aS9sH1cKXqXlCgS2tJ1x0m2Zq1 sample-held";
const NOT_HELD: &str =
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq0sZq8m0p3nL7wD2vR4yT6uE1iO9aS5dF3gH7jK2lM sample-other";

struct Fakes {
    agent: PathBuf,
    log: PathBuf,
}

/// The agent and the `ssh` program, started once for the binary.
fn fakes() -> &'static Fakes {
    static FAKES: OnceLock<Fakes> = OnceLock::new();
    FAKES.get_or_init(|| {
        let dir = tempfile::tempdir().unwrap().keep();
        let agent = dir.join("agent.sock");
        let log = dir.join("ssh.log");
        let listener = std::os::unix::net::UnixListener::bind(&agent).unwrap();
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(stream) = stream else { continue };
                std::thread::spawn(move || serve_agent(stream));
            }
        });
        // `-G` prints what OpenSSH would, from the options given; any other
        // run records its options and the identity file, then fails as an
        // unreachable host does.
        let ssh = dir.join("fake-ssh");
        std::fs::write(
            &ssh,
            format!(
                r#"#!/bin/sh
log={log}
dump=false
for arg in "$@"; do [ "$arg" = -G ] && dump=true; done
if $dump; then
  for arg in "$@"; do
    case "$arg" in
      IdentitiesOnly=*|IdentityFile=*|IdentityAgent=*)
        printf '%s\n' "$arg" | sed 's/=/ /' | sed 's/^Identities[Oo]nly/identitiesonly/;s/^IdentityFile/identityfile/;s/^IdentityAgent/identityagent/';;
    esac
  done
  exit 0
fi
line=run
for arg in "$@"; do line="$line $arg"; done
for arg in "$@"; do
  case "$arg" in IdentityFile=*) line="$line key=$(cat "${{arg#IdentityFile=}}")";; esac
done
# One write per run, so concurrent runs do not interleave.
printf '%s\n' "$line" >> "$log"
echo "ssh: connect to host sample port 22: Connection refused" >&2
exit 255
"#,
                log = log.display()
            ),
        )
        .unwrap();
        std::fs::set_permissions(&ssh, std::os::unix::fs::PermissionsExt::from_mode(0o755))
            .unwrap();
        // SAFETY: set once, before any test of this binary starts a service
        // or a thread that reads the environment.
        unsafe {
            std::env::set_var("SSH_AUTH_SOCK", &agent);
            std::env::set_var("GIT_SSH_COMMAND", &ssh);
        }
        Fakes { agent, log }
    })
}

/// Answer one connection's identity requests with `HELD` alone.
fn serve_agent(mut stream: std::os::unix::net::UnixStream) {
    use base64::Engine;
    let blob = base64::engine::general_purpose::STANDARD
        .decode(HELD.split(' ').nth(1).unwrap())
        .unwrap();
    loop {
        let mut length = [0; 4];
        if stream.read_exact(&mut length).is_err() {
            return;
        }
        let mut request = vec![0; u32::from_be_bytes(length) as usize];
        if stream.read_exact(&mut request).is_err() {
            return;
        }
        let answer = if request.first() == Some(&11) {
            let mut answer = vec![12];
            answer.extend(1u32.to_be_bytes());
            for part in [&blob[..], b"sample-held"] {
                answer.extend((part.len() as u32).to_be_bytes());
                answer.extend(part);
            }
            answer
        } else {
            vec![5]
        };
        let mut framed = (answer.len() as u32).to_be_bytes().to_vec();
        framed.extend(answer);
        if stream.write_all(&framed).is_err() {
            return;
        }
    }
}

/// The `ssh` runs recorded so far that reached `host`.
fn runs(host: &str) -> Vec<String> {
    std::fs::read_to_string(&fakes().log)
        .unwrap_or_default()
        .lines()
        .filter(|line| line.contains(host))
        .map(str::to_owned)
        .collect()
}

fn fingerprint(key: &str) -> String {
    toha::source::SshKey::parse(key).unwrap().fingerprint()
}

fn key_only(key: &str) -> String {
    key.split(' ').take(2).collect::<Vec<_>>().join(" ")
}

async fn post(service: &Running, path: &str, body: Value) -> Reply {
    service.post(path, &body).await
}

/// Each run reached the host with only `key` from the fake agent.
fn pinned(runs: &[String], key: &str) {
    let agent = fakes().agent.display().to_string();
    assert!(!runs.is_empty());
    for run in runs {
        assert!(run.contains("IdentitiesOnly=yes"), "{run}");
        assert!(run.contains(&format!("IdentityAgent={agent}")), "{run}");
        assert!(run.contains(&format!("key={}", key_only(key))), "{run}");
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_version_listing_offers_only_the_templates_key() {
    fakes();
    let world = World::new();
    let service = Running::start(&world).await;
    let address = "ssh://git@listing-held.example.invalid/example-org/kit.git";
    let reply = post(
        &service,
        "/v1/templates/versions",
        json!({"address": address, "credential": {"sshKey": HELD}}),
    )
    .await;
    assert_eq!(reply.status, 422, "{}", reply.json);
    pinned(&runs("listing-held.example.invalid"), HELD);
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_toha_fetch_offers_only_the_templates_key() {
    fakes();
    let world = World::new();
    let service = Running::start(&world).await;
    let address = "ssh://git@fetch-held.example.invalid/example-org/kit.git";
    let reply = post(
        &service,
        "/v1/templates/checks",
        json!({"address": address, "ref": "1.0.0", "credential": {"sshKey": HELD}}),
    )
    .await;
    assert_eq!(reply.status, 422, "{}", reply.json);
    pinned(&runs("fetch-held.example.invalid"), HELD);
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_key_the_agent_does_not_hold_fails_naming_it_without_connecting() {
    fakes();
    let world = World::new();
    let service = Running::start(&world).await;
    for (route, host, body) in [
        (
            "/v1/templates/versions",
            "listing-other.example.invalid",
            json!({}),
        ),
        (
            "/v1/templates/checks",
            "fetch-other.example.invalid",
            json!({"ref": "1.0.0"}),
        ),
    ] {
        let address = format!("ssh://git@{host}/example-org/kit.git");
        let reply = post(
            &service,
            route,
            with(
                body,
                json!({"address": address, "credential": {"sshKey": NOT_HELD}}),
            ),
        )
        .await;
        assert_eq!(reply.status, 422, "{route} {}", reply.json);
        let message = reply.json["error"]["message"].as_str().unwrap();
        assert!(
            message.contains(&fingerprint(NOT_HELD)),
            "{route} {message}"
        );
        assert_eq!(runs(host), Vec::<String>::new(), "{route}");
    }
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_template_without_a_credential_keeps_every_agent_key() {
    fakes();
    let world = World::new();
    let service = Running::start(&world).await;
    let address = "ssh://git@listing-any.example.invalid/example-org/kit.git";
    let reply = post(
        &service,
        "/v1/templates/versions",
        json!({"address": address}),
    )
    .await;
    assert_eq!(reply.status, 422, "{}", reply.json);
    let runs = runs("listing-any.example.invalid");
    assert!(!runs.is_empty());
    assert!(
        runs.iter().all(|run| !run.contains("IdentitiesOnly")),
        "{runs:?}"
    );
    service.stop().await;
}
