//! Private HTTPS templates through the workspace service's credential route.
//! A Git host over TLS, with a certificate authority generated here, admits
//! a fetch only with `x-access-token` basic authentication and a token the
//! route granted for that repository. The service trusts the authority
//! through `GIT_SSL_CAINFO`, which both of curl's TLS backends read; the
//! image's `SSL_CERT_FILE` trust is the live suite's. Git runs here only as
//! the host's `http-backend` program; the service runs no Git command.
mod common;
use common::*;
use serde_json::{Value, json};
use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, Condvar, Mutex, OnceLock};
use std::time::Duration;

/// The authority every host in this binary is signed by.
struct Authority {
    params: rcgen::CertificateParams,
    key: rcgen::KeyPair,
    _dir: tempfile::TempDir,
}

/// Trust the authority through `GIT_SSL_CAINFO`. Every test calls this
/// before it starts a thread, so the variable is set once, before any
/// thread reads the environment.
fn authority() -> &'static Authority {
    static AUTHORITY: OnceLock<Authority> = OnceLock::new();
    AUTHORITY.get_or_init(|| {
        let key = rcgen::KeyPair::generate().unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "Sample test authority");
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::DigitalSignature,
        ];
        let cert = params.self_signed(&key).unwrap();
        let dir = tempfile::tempdir().unwrap();
        let file = dir.path().join("authority.pem");
        std::fs::write(&file, cert.pem()).unwrap();
        // SAFETY: no other thread exists yet that reads the environment;
        // see `run`.
        unsafe { std::env::set_var("GIT_SSL_CAINFO", &file) };
        Authority {
            params,
            key,
            _dir: dir,
        }
    })
}

/// Run `test` on a runtime started after the authority is trusted.
fn run(test: impl std::future::Future<Output = ()>) {
    authority();
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .unwrap()
        .block_on(test);
}

/// One request the Git host received: its path and the basic
/// authentication it carried.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Seen {
    path: String,
    user: Option<(String, String)>,
}

#[derive(Default)]
struct HostState {
    seen: Vec<Seen>,
    /// The repository each token is valid for.
    tokens: HashMap<String, String>,
    /// Repositories whose first unauthenticated request arrived.
    challenged: Vec<String>,
    /// Whether a held challenge gave up waiting for the others.
    gave_up: bool,
    /// Whether an admitted advertisement request is answered with its own
    /// `Authorization` header as the service name, as a misbehaving host
    /// does.
    echo: bool,
}

/// A smart HTTP Git host over TLS on loopback.
struct GitHost {
    origin: String,
    root: PathBuf,
    state: Arc<(Mutex<HostState>, Condvar)>,
    /// Hold each repository's first challenge until this many repositories
    /// were challenged, so their operations overlap.
    rendezvous: Arc<Mutex<usize>>,
    _dir: tempfile::TempDir,
}

impl GitHost {
    fn start() -> Self {
        let ca = authority();
        let key = rcgen::KeyPair::generate().unwrap();
        let params = rcgen::CertificateParams::new(vec!["localhost".to_owned()]).unwrap();
        let cert = params
            .signed_by(&key, &rcgen::Issuer::from_params(&ca.params, &ca.key))
            .unwrap();
        let config = Arc::new(
            rustls::ServerConfig::builder()
                .with_no_client_auth()
                .with_single_cert(
                    vec![cert.der().clone()],
                    rustls::pki_types::PrivateKeyDer::Pkcs8(key.serialize_der().into()),
                )
                .unwrap(),
        );
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        let state = Arc::new((Mutex::new(HostState::default()), Condvar::new()));
        let rendezvous = Arc::new(Mutex::new(0));
        let (served_root, served_state, served_rendezvous) =
            (root.clone(), state.clone(), rendezvous.clone());
        std::thread::spawn(move || {
            for connection in listener.incoming() {
                let Ok(connection) = connection else { return };
                let (config, root, state, rendezvous) = (
                    config.clone(),
                    served_root.clone(),
                    served_state.clone(),
                    served_rendezvous.clone(),
                );
                std::thread::spawn(move || serve(connection, config, &root, &state, &rendezvous));
            }
        });
        Self {
            origin: format!("https://localhost:{port}"),
            root,
            state,
            rendezvous,
            _dir: dir,
        }
    }

    /// Serve a bare clone of `source` at `/<name>`.
    fn publish(&self, name: &str, source: &Path) {
        let dest = self.root.join(name);
        std::fs::create_dir_all(dest.parent().unwrap()).unwrap();
        git(
            &self.root,
            &[
                "clone",
                "-q",
                "--bare",
                source.to_str().unwrap(),
                dest.to_str().unwrap(),
            ],
        );
    }

    fn url(&self, name: &str) -> String {
        format!("{}/{name}", self.origin)
    }

    /// Admit `token` for the repository at `/<name>` only.
    fn admit(&self, token: &str, name: &str) {
        self.state
            .0
            .lock()
            .unwrap()
            .tokens
            .insert(token.to_owned(), format!("/{name}"));
    }

    fn hold_until(&self, repositories: usize) {
        *self.rendezvous.lock().unwrap() = repositories;
    }

    fn echo(&self) {
        self.state.0.lock().unwrap().echo = true;
    }

    fn gave_up(&self) -> bool {
        self.state.0.lock().unwrap().gave_up
    }

    fn seen(&self) -> Vec<Seen> {
        self.state.0.lock().unwrap().seen.clone()
    }
}

/// The repository a request path names: up to and including `.git`.
fn repository(path: &str) -> String {
    match path.find(".git") {
        Some(end) => path[..end + 4].to_owned(),
        None => path.to_owned(),
    }
}

fn serve(
    connection: TcpStream,
    config: Arc<rustls::ServerConfig>,
    root: &Path,
    state: &(Mutex<HostState>, Condvar),
    rendezvous: &Mutex<usize>,
) {
    let tls = rustls::ServerConnection::new(config).unwrap();
    let mut reader = BufReader::new(rustls::StreamOwned::new(tls, connection));
    let mut line = String::new();
    if reader.read_line(&mut line).unwrap_or(0) == 0 {
        return;
    }
    let mut parts = line.split_whitespace();
    let (method, target) = (
        parts.next().unwrap_or("").to_owned(),
        parts.next().unwrap_or("").to_owned(),
    );
    let mut headers = HashMap::new();
    loop {
        let mut header = String::new();
        reader.read_line(&mut header).unwrap();
        let header = header.trim_end();
        if header.is_empty() {
            break;
        }
        let (name, value) = header.split_once(':').unwrap();
        headers.insert(name.trim().to_ascii_lowercase(), value.trim().to_owned());
    }
    let body = read_body(&mut reader, &headers);
    let (path, query) = target.split_once('?').unwrap_or((&target, ""));
    let user = headers
        .get("authorization")
        .and_then(|v| v.strip_prefix("Basic "))
        .and_then(|v| {
            use base64::Engine;
            base64::engine::general_purpose::STANDARD.decode(v).ok()
        })
        .and_then(|v| String::from_utf8(v).ok())
        .and_then(|v| v.split_once(':').map(|(u, p)| (u.to_owned(), p.to_owned())));
    let repo = repository(path);
    let (lock, arrived) = state;
    let mut host = lock.lock().unwrap();
    host.seen.push(Seen {
        path: path.to_owned(),
        user: user.clone(),
    });
    let out = reader.get_mut();
    // Credentials the host did not issue are challenged, as GitHub does; a
    // token issued for another repository is refused.
    let user =
        user.filter(|(name, token)| name == "x-access-token" && host.tokens.contains_key(token));
    let Some((_, token)) = user else {
        if !host.challenged.contains(&repo) {
            host.challenged.push(repo);
            arrived.notify_all();
            let wanted = *rendezvous.lock().unwrap();
            let (mut held, waited) = arrived
                .wait_timeout_while(host, Duration::from_secs(30), |h| {
                    h.challenged.len() < wanted
                })
                .unwrap();
            held.gave_up |= waited.timed_out();
            drop(held);
        } else {
            drop(host);
        }
        respond(
            out,
            "401 Unauthorized",
            &[("WWW-Authenticate", "Basic realm=\"sample\"")],
            b"",
        );
        return;
    };
    if host.tokens.get(&token) != Some(&repo) {
        drop(host);
        respond(out, "403 Forbidden", &[], b"");
        return;
    }
    if host.echo && path.ends_with("/info/refs") {
        drop(host);
        let line = format!("# service={}\n", headers["authorization"]);
        let body = format!("{:04x}{line}0000", line.len() + 4);
        respond(
            out,
            "200 OK",
            &[(
                "Content-Type",
                "application/x-git-upload-pack-advertisement",
            )],
            body.as_bytes(),
        );
        return;
    }
    drop(host);
    let (status, headers, body) = http_backend(root, &method, path, query, &headers, &body);
    let headers: Vec<(&str, &str)> = headers
        .iter()
        .map(|(n, v)| (n.as_str(), v.as_str()))
        .collect();
    respond(out, &status, &headers, &body);
}

fn read_body(reader: &mut impl BufRead, headers: &HashMap<String, String>) -> Vec<u8> {
    let mut body = Vec::new();
    if headers
        .get("transfer-encoding")
        .is_some_and(|v| v.eq_ignore_ascii_case("chunked"))
    {
        loop {
            let mut size = String::new();
            reader.read_line(&mut size).unwrap();
            let size = usize::from_str_radix(size.trim().split(';').next().unwrap(), 16).unwrap();
            if size == 0 {
                let mut trailer = String::new();
                while reader.read_line(&mut trailer).unwrap() > 2 {
                    trailer.clear();
                }
                break;
            }
            let mut chunk = vec![0; size + 2];
            reader.read_exact(&mut chunk).unwrap();
            body.extend_from_slice(&chunk[..size]);
        }
    } else if let Some(length) = headers.get("content-length") {
        body.resize(length.parse().unwrap(), 0);
        reader.read_exact(&mut body).unwrap();
    }
    body
}

type Tls = rustls::StreamOwned<rustls::ServerConnection, TcpStream>;

/// Answer and close: rustls clients refuse a TCP close without TLS's
/// `close_notify`.
fn respond(out: &mut Tls, status: &str, headers: &[(&str, &str)], body: &[u8]) {
    let mut head = format!("HTTP/1.1 {status}\r\n");
    for (name, value) in headers {
        head.push_str(&format!("{name}: {value}\r\n"));
    }
    head.push_str(&format!(
        "Content-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    ));
    let _ = out.write_all(head.as_bytes());
    let _ = out.write_all(body);
    out.conn.send_close_notify();
    let _ = out.flush();
}

type Answer = (String, Vec<(String, String)>, Vec<u8>);

/// Run Git's smart HTTP CGI program for one request.
fn http_backend(
    root: &Path,
    method: &str,
    path: &str,
    query: &str,
    headers: &HashMap<String, String>,
    body: &[u8],
) -> Answer {
    let mut command = Command::new("git");
    command
        .arg("http-backend")
        .env_clear()
        .env("PATH", std::env::var_os("PATH").unwrap())
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .env("GIT_PROJECT_ROOT", root)
        .env("GIT_HTTP_EXPORT_ALL", "1")
        .env("REQUEST_METHOD", method)
        .env("PATH_INFO", path)
        .env("QUERY_STRING", query)
        .env("REMOTE_USER", "x-access-token")
        .env("REMOTE_ADDR", "127.0.0.1")
        .env("CONTENT_LENGTH", body.len().to_string())
        .stdin(Stdio::piped())
        .stdout(Stdio::piped());
    for (header, variable) in [
        ("content-type", "CONTENT_TYPE"),
        ("content-encoding", "HTTP_CONTENT_ENCODING"),
        ("git-protocol", "GIT_PROTOCOL"),
    ] {
        if let Some(value) = headers.get(header) {
            command.env(variable, value);
        }
    }
    let mut child = command.spawn().unwrap();
    child.stdin.take().unwrap().write_all(body).unwrap();
    let output = child.wait_with_output().unwrap();
    let split = output
        .stdout
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .map(|i| (i, 4))
        .or_else(|| {
            output
                .stdout
                .windows(2)
                .position(|w| w == b"\n\n")
                .map(|i| (i, 2))
        })
        .unwrap();
    let head = String::from_utf8_lossy(&output.stdout[..split.0]).into_owned();
    let mut status = "200 OK".to_owned();
    let mut fields = Vec::new();
    for line in head.lines() {
        let (name, value) = line.split_once(':').unwrap();
        if name.eq_ignore_ascii_case("status") {
            status = value.trim().to_owned();
        } else {
            fields.push((name.trim().to_owned(), value.trim().to_owned()));
        }
    }
    (status, fields, output.stdout[split.0 + split.1..].to_vec())
}

/// Tokens the route grants; no reply may hold one.
const TOKEN_A: &str = "ghs_sample0token0a";
const TOKEN_B: &str = "ghs_sample0token0b";

/// The route's answer for the GitHub App: a token for each URL in `grants`.
fn grants(grants: Vec<(String, &'static str)>) -> impl Fn(&str) -> (u16, Value) + Send + Sync {
    move |url| match grants.iter().find(|(u, _)| u == url) {
        Some((_, token)) => (
            200,
            json!({"username": "x-access-token", "password": token}),
        ),
        None => (
            404,
            json!({"reason": "the URL is not an https repository URL on the GitHub App's server"}),
        ),
    }
}

fn token_free(reply: &Reply) {
    let body = String::from_utf8_lossy(&reply.body);
    for token in [TOKEN_A, TOKEN_B] {
        assert!(!body.contains(token), "{body}");
    }
}

/// A world with the sample template served by `host` as
/// `example-org/kit-a.git` and `example-org/kit-b.git`.
fn world(host: &GitHost) -> World {
    let (a, b) = (
        format!("{}@v1", host.url("example-org/kit-a.git")),
        format!("{}@v1", host.url("example-org/kit-b.git")),
    );
    let world = World::with_templates(&[("private-a", &a), ("private-b", &b)]);
    host.publish("example-org/kit-a.git", &world.sample);
    host.publish("example-org/kit-b.git", &world.sample);
    world
}

#[test]
fn a_private_https_template_is_fetched_with_the_routes_token() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        let repo = host.url("example-org/kit-a.git");
        host.admit(TOKEN_A, "example-org/kit-a.git");
        world
            .hooks
            .answer_credentials(grants(vec![(repo.clone(), TOKEN_A)]));
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "private-a"))
            .await;
        assert_eq!(start.status, 200, "{}", start.json);
        token_free(&start);
        let urls = world.hooks.credential_urls();
        assert!(
            !urls.is_empty() && urls.iter().all(|u| *u == repo),
            "{urls:?}"
        );
        let seen = host.seen();
        assert_eq!(seen[0].user, None, "{seen:?}");
        assert!(
            seen.iter()
                .any(|s| s.user == Some(("x-access-token".to_owned(), TOKEN_A.to_owned()))),
            "{seen:?}"
        );
        // The interview's answers and the generate read the fetched
        // revision; the route answers them as well.
        let done = service
            .post(
                "/v1/interviews/answers",
                &with(
                    world.select("alpha", "private-a"),
                    json!({"session": start.json["session"],
                    "submission": submission(&start, json!({"label": "one",
                        "catalog": {"title": "Sample"}}))}),
                ),
            )
            .await;
        assert_eq!(done.json["state"], "completed", "{}", done.json);
        token_free(&done);
        world.initialize("alpha");
        let generated = service
            .post(
                "/v1/workspaces/generate",
                &with(
                    world.select("alpha", "private-a"),
                    json!({"session": done.json["session"]}),
                ),
            )
            .await;
        assert_eq!(generated.status, 200, "{}", generated.json);
        token_free(&generated);
        assert!(world.folder("alpha").join("result.txt").exists());
        service.stop().await;
    });
}

#[test]
fn a_url_the_route_does_not_serve_gets_no_credential() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "private-a"))
            .await;
        assert_eq!(
            (start.status, start.kind()),
            (422, "template-resolve-failed"),
            "{}",
            start.json
        );
        assert_eq!(
            world.hooks.credential_urls(),
            [host.url("example-org/kit-a.git")]
        );
        assert!(host.seen().iter().all(|s| s.user.is_none()));
        service.stop().await;
    });
}

#[test]
fn a_minting_failure_fails_the_fetch_with_the_routes_reason() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        world.hooks.answer_credentials(|_| {
            (
                502,
                json!({"reason": "the App is not installed for the repository"}),
            )
        });
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "private-a"))
            .await;
        assert_eq!(start.status, 422, "{}", start.json);
        let message = start.json["error"]["message"].as_str().unwrap();
        assert!(
            message.contains(
                "GitHub App credentials are unavailable: the App is not installed for the repository"
            ),
            "{message}"
        );
        assert!(host.seen().iter().all(|s| s.user.is_none()));
        service.stop().await;
    });
}

/// Two operations at once each answer their own challenge from their own
/// resolver scope: the host holds both challenges until both arrived, and
/// admits each token for its own repository only.
#[test]
fn concurrent_operations_each_fetch_with_their_own_token() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        host.admit(TOKEN_A, "example-org/kit-a.git");
        host.admit(TOKEN_B, "example-org/kit-b.git");
        host.hold_until(2);
        world.hooks.answer_credentials(grants(vec![
            (host.url("example-org/kit-a.git"), TOKEN_A),
            (host.url("example-org/kit-b.git"), TOKEN_B),
        ]));
        let service = Running::start(&world).await;
        let (a, b) = (
            world.select("alpha", "private-a"),
            world.select("beta", "private-b"),
        );
        let (a, b) = tokio::join!(
            service.post("/v1/interviews", &a),
            service.post("/v1/interviews", &b),
        );
        assert!(!host.gave_up(), "the two challenges did not overlap");
        for reply in [&a, &b] {
            assert_eq!(reply.status, 200, "{}", reply.json);
            token_free(reply);
        }
        let seen = host.seen();
        for (repo, token) in [
            ("/example-org/kit-a.git", TOKEN_A),
            ("/example-org/kit-b.git", TOKEN_B),
        ] {
            assert!(
                seen.iter()
                    .filter(|s| s.path.starts_with(repo))
                    .all(|s| s.user.as_ref().is_none_or(|(_, t)| t == token)),
                "{seen:?}"
            );
        }
        service.stop().await;
    });
}

/// Every written form of a granted credential, however a remote echoes it
/// into Toha's error, is removed from the reply.
#[test]
fn a_credential_a_remote_echoes_never_reaches_the_reply() {
    use base64::Engine;
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        host.admit(TOKEN_A, "example-org/kit-a.git");
        host.echo();
        world
            .hooks
            .answer_credentials(grants(vec![(host.url("example-org/kit-a.git"), TOKEN_A)]));
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "private-a"))
            .await;
        assert_eq!(start.status, 422, "{}", start.json);
        let body = String::from_utf8_lossy(&start.body).into_owned();
        assert!(body.contains("<redacted>"), "{body}");
        let pair =
            base64::engine::general_purpose::STANDARD.encode(format!("x-access-token:{TOKEN_A}"));
        let listed: Vec<String> = pair.bytes().map(|b| b.to_string()).collect();
        for leaked in [TOKEN_A.to_owned(), pair.clone(), listed.join(", ")] {
            assert!(!body.contains(&leaked), "{body}");
        }
        // The host saw the credential, so the echo is the one it sent.
        assert!(
            host.seen()
                .iter()
                .any(|s| s.user == Some(("x-access-token".to_owned(), TOKEN_A.to_owned())))
        );
        service.stop().await;
    });
}

/// A user name in a template address does not opt out of the App: the
/// route is asked for the address without it, and the fetch uses the token.
#[test]
fn a_template_address_with_a_user_name_fetches_with_the_app_token() {
    run(async {
        let host = GitHost::start();
        let repo = host.url("example-org/kit-a.git");
        let address = format!(
            "{}@v1",
            repo.replacen("https://", "https://sample-user@", 1)
        );
        let world = World::with_templates(&[("named-user", &address)]);
        host.publish("example-org/kit-a.git", &world.sample);
        host.admit(TOKEN_A, "example-org/kit-a.git");
        world
            .hooks
            .answer_credentials(grants(vec![(repo.clone(), TOKEN_A)]));
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "named-user"))
            .await;
        assert_eq!(start.status, 200, "{}", start.json);
        token_free(&start);
        let urls = world.hooks.credential_urls();
        assert!(
            !urls.is_empty() && urls.iter().all(|u| *u == repo),
            "{urls:?}"
        );
        assert!(
            host.seen()
                .iter()
                .any(|s| s.user == Some(("x-access-token".to_owned(), TOKEN_A.to_owned())))
        );
        service.stop().await;
    });
}

/// The threads of this process.
fn threads() -> usize {
    std::fs::read_dir("/proc/self/task").unwrap().count()
}

/// Wait up to ten seconds for `ok`.
async fn eventually(ok: impl Fn() -> bool) -> bool {
    for _ in 0..1000 {
        if ok() {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    ok()
}

/// A caller that goes away while the credential route holds its request,
/// as a cancelled job or one at its limit does, ends the request: the
/// route's connection closes, the worker returns, and repeats add no
/// thread.
#[test]
fn a_caller_going_away_ends_its_credential_request_and_worker() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        world.hooks.hold_credentials(true);
        let service = Running::start(&world).await;
        let mut settled = None;
        for n in 1..=5 {
            let socket = service.socket.clone();
            let selector = world.select(&format!("w{n}"), "private-a");
            let caller = tokio::spawn(async move {
                post_bytes(
                    &socket,
                    "/v1/interviews",
                    body_of("/v1/interviews", &selector),
                )
                .await
            });
            assert!(
                eventually(|| world.hooks.held() == n).await,
                "request {n} did not reach the credential route"
            );
            caller.abort();
            assert!(
                eventually(|| world.hooks.released() == n).await,
                "the credential request of caller {n} stayed open"
            );
            match settled {
                // The first repeat starts the blocking pool's worker.
                None => settled = Some(threads()),
                Some(limit) => assert!(
                    eventually(|| threads() <= limit).await,
                    "caller {n} left a thread: {} > {limit}",
                    threads()
                ),
            }
        }
        // The service still answers, so no worker holds it.
        world.hooks.hold_credentials(false);
        world
            .hooks
            .answer_credentials(|_| (404, json!({"reason": "no GitHub App is configured"})));
        let start = service
            .post("/v1/interviews", &world.select("after", "private-a"))
            .await;
        assert_eq!(start.kind(), "template-resolve-failed", "{}", start.json);
        service.stop().await;
    });
}
