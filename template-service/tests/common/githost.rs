//! A smart HTTP Git host over TLS on loopback, with a certificate authority
//! generated in the test binary. The template service refuses `file://` and
//! folder addresses, so every test template is fetched from one of these.
//! The service trusts the authority through `GIT_SSL_CAINFO`, which both of
//! curl's TLS backends read; the image's `SSL_CERT_FILE` trust is the live
//! suite's. Git runs here only as the host's `http-backend` program; the
//! service runs no Git command.
//!
//! `served` gives the anonymous address of a repository under the system's
//! temporary folder, where every test's repositories are. `GitHost::start`
//! is a private host that admits only `x-access-token` basic authentication
//! with a token granted for that repository, as GitHub does.
use serde_json::{Value, json};
use std::collections::HashMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, Condvar, Mutex, OnceLock};
use std::time::Duration;

use super::git;

/// The authority every host in this binary is signed by.
pub struct Authority {
    params: rcgen::CertificateParams,
    key: rcgen::KeyPair,
    _dir: tempfile::TempDir,
}

/// Trust the authority through `GIT_SSL_CAINFO`. Every test reaches this,
/// through `World::new`, `served` or `GitHost::start`, before it fetches or
/// starts a thread of its own, so the variable is set once, before any
/// thread reads the environment.
pub fn authority() -> &'static Authority {
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
        // SAFETY: no other thread reads the environment yet; see above.
        unsafe { std::env::set_var("GIT_SSL_CAINFO", &file) };
        Authority {
            params,
            key,
            _dir: dir,
        }
    })
}

/// One request the Git host received: its path and the basic
/// authentication it carried.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Seen {
    pub path: String,
    pub user: Option<(String, String)>,
}

#[derive(Default)]
struct HostState {
    /// Whether every repository is served without authentication.
    public: bool,
    seen: Vec<Seen>,
    /// The repository each token is valid for.
    tokens: HashMap<String, String>,
    /// Repositories whose first unauthenticated request arrived.
    challenged: Vec<String>,
    /// Whether a held challenge gave up waiting for the others.
    gave_up: bool,
    /// When set, an admitted advertisement request is answered with this
    /// service name, built from the request's `Authorization` header and
    /// its token, as a misbehaving host does.
    echo: Option<fn(&str, &str) -> String>,
    /// While set, the pack request after this many others waits here, as a
    /// hung remote does.
    stall_after: Option<usize>,
    /// How many requests waited.
    stalls: usize,
    schema: Option<Value>,
    hold_schema: bool,
    schema_stalls: usize,
    schema_closed: usize,
}

/// A smart HTTP Git host over TLS on loopback.
pub struct GitHost {
    pub origin: String,
    pub root: PathBuf,
    state: Arc<(Mutex<HostState>, Condvar)>,
    /// Hold each repository's first challenge until this many repositories
    /// were challenged, so their operations overlap.
    rendezvous: Arc<Mutex<usize>>,
    _dir: Option<tempfile::TempDir>,
}

/// The address of the repository at `path`, under the system's temporary
/// folder, on the anonymous host every test shares.
pub fn served(path: &Path) -> String {
    static PUBLIC: OnceLock<GitHost> = OnceLock::new();
    let host = PUBLIC.get_or_init(|| {
        let root = std::env::temp_dir().canonicalize().unwrap();
        GitHost::listen(root, None, true)
    });
    let path = path.canonicalize().unwrap();
    let relative = path
        .strip_prefix(&host.root)
        .unwrap_or_else(|_| panic!("{} is not under the temporary folder", path.display()));
    format!("{}/{}", host.origin, relative.display())
}

impl GitHost {
    /// A private host over its own folder, which admits only a token
    /// granted for the repository.
    pub fn start() -> Self {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().canonicalize().unwrap();
        Self::listen(root, Some(dir), false)
    }

    fn listen(root: PathBuf, dir: Option<tempfile::TempDir>, public: bool) -> Self {
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
        let state = Arc::new((
            Mutex::new(HostState {
                public,
                ..HostState::default()
            }),
            Condvar::new(),
        ));
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

    pub fn schema(&self) -> String {
        let url = format!("{}/schema.json", self.origin);
        self.state.0.lock().unwrap().schema = Some(json!({"$id": url, "type": "object"}));
        url
    }

    pub fn hold_schema(&self, hold: bool) {
        self.state.0.lock().unwrap().hold_schema = hold;
    }

    /// Serve a bare clone of `source` at `/<name>`.
    pub fn publish(&self, name: &str, source: &Path) {
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

    pub fn url(&self, name: &str) -> String {
        format!("{}/{name}", self.origin)
    }

    /// Admit `token` for the repository at `/<name>` only.
    pub fn admit(&self, token: &str, name: &str) {
        self.state
            .0
            .lock()
            .unwrap()
            .tokens
            .insert(token.to_owned(), format!("/{name}"));
    }

    pub fn hold_until(&self, repositories: usize) {
        *self.rendezvous.lock().unwrap() = repositories;
    }

    /// Make the pack request after `others` others wait until `resume`.
    pub fn stall(&self, others: usize) {
        self.state.0.lock().unwrap().stall_after = Some(others);
    }

    pub fn resume(&self) {
        let (lock, arrived) = &*self.state;
        lock.lock().unwrap().stall_after = None;
        arrived.notify_all();
    }

    pub fn stalls(&self) -> usize {
        self.state.0.lock().unwrap().stalls
    }

    pub fn echo(&self, service: fn(&str, &str) -> String) {
        self.state.0.lock().unwrap().echo = Some(service);
    }

    pub fn gave_up(&self) -> bool {
        self.state.0.lock().unwrap().gave_up
    }

    /// How many schema requests were held.
    pub fn schema_stalls(&self) -> usize {
        self.state.0.lock().unwrap().schema_stalls
    }

    /// How many held schema requests ended with their connection closed.
    pub fn schema_closed(&self) -> usize {
        self.state.0.lock().unwrap().schema_closed
    }

    pub fn seen(&self) -> Vec<Seen> {
        self.state.0.lock().unwrap().seen.clone()
    }
}

/// The repository a request path names: up to and including `.git`, or up to
/// the smart HTTP route for a name without that suffix.
fn repository(path: &str) -> String {
    if let Some(end) = path.find(".git") {
        return path[..end + 4].to_owned();
    }
    ["/info/refs", "/git-upload-pack"]
        .iter()
        .find_map(|route| path.strip_suffix(route))
        .unwrap_or(path)
        .to_owned()
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
    if path == "/schema.json" {
        if host.hold_schema {
            host.schema_stalls += 1;
            drop(host);
            // A stopped schema fetch must close its TLS connection even
            // though this host has never sent a response header or body.
            reader
                .get_mut()
                .sock
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let mut byte = [0];
            let closed = match reader.read(&mut byte) {
                Ok(0) => true,
                Err(e) => !matches!(
                    e.kind(),
                    std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
                ),
                Ok(_) => false,
            };
            if closed {
                state.0.lock().unwrap().schema_closed += 1;
            }
        } else {
            let body = host.schema.as_ref().unwrap().to_string();
            drop(host);
            respond(
                reader.get_mut(),
                "200 OK",
                &[("Content-Type", "application/json")],
                body.as_bytes(),
            );
        }
        return;
    }
    let out = reader.get_mut();
    if host.public {
        drop(host);
        let (status, headers, body) = http_backend(root, &method, path, query, &headers, &body);
        let headers: Vec<(&str, &str)> = headers
            .iter()
            .map(|(n, v)| (n.as_str(), v.as_str()))
            .collect();
        respond(out, &status, &headers, &body);
        return;
    }
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
    if path.ends_with("/git-upload-pack") {
        match host.stall_after {
            Some(0) => {
                host.stalls += 1;
                host = arrived
                    .wait_while(host, |h| h.stall_after.is_some())
                    .unwrap();
            }
            Some(others) => host.stall_after = Some(others - 1),
            None => {}
        }
    }
    if let Some(service) = host.echo.filter(|_| path.ends_with("/info/refs")) {
        drop(host);
        let line = format!("# service={}\n", service(&headers["authorization"], &token));
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
