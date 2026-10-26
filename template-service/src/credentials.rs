//! GitHub App credentials for Toha's HTTPS fetches. The service holds no
//! credential: when an HTTPS remote challenges, the resolver asks the
//! workspace service's credential route on the hook runner socket for a
//! `contents: read` installation token for that one repository. Every
//! operation runs inside one resolver scope (see `server`), so every fetch
//! of a create, resume, upgrade, catalog update or formal-name lookup asks
//! the route.
//!
//! The token reaches Toha's `Credential`, whose `Debug` output hides it, and
//! the operation's `Handed` record. No error or refusal built here carries
//! it, a reply's text, a URL or an operating system message. A remote can
//! still echo the credential into a Toha error, so `server` removes every
//! written form of what `Handed` holds from each reply before it leaves the
//! service.
//!
//! An operation's `Ended` signal fires when its caller goes away: the job
//! was cancelled or reached its limit. A credential request races it, and
//! loses its connection when it fires.
use crate::hooks;
use base64::Engine;
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, PoisonError};
use toha::source::{Credential, CredentialRequest, CredentialResolver};

/// The credential route on the hook runner socket.
pub const CREDENTIAL_PATH: &str = "/v1/credentials/github";

/// What every refusal starts with, as the workspace service's own GitHub App
/// failures do.
pub const UNAVAILABLE: &str = "GitHub App credentials are unavailable: ";

/// The longest route reason passed through, in bytes. The route's reasons
/// are fixed sentences well within it.
pub const MAX_REASON: usize = 200;

/// What replaces a credential in a reply.
pub const REDACTED: &str = "<redacted>";

/// The resolver for one operation: it asks the route behind `socket`, keeps
/// what it hands out in `handed`, and stops asking when `ended` fires.
pub fn resolver(socket: &Path, handed: Handed, ended: Ended) -> Arc<dyn CredentialResolver> {
    Arc::new(RouteResolver {
        socket: socket.to_owned(),
        handed,
        ended,
    })
}

/// Asks the workspace service's credential route on the hook runner socket.
#[derive(Debug)]
pub struct RouteResolver {
    socket: PathBuf,
    handed: Handed,
    ended: Ended,
}

/// The credentials one operation's resolver handed out.
#[derive(Clone, Default)]
pub struct Handed(Arc<Mutex<Vec<(String, String)>>>);

impl std::fmt::Debug for Handed {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Handed(<redacted>)")
    }
}

impl Handed {
    fn keep(&self, username: &str, password: &str) {
        self.lock().push((username.to_owned(), password.to_owned()));
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Vec<(String, String)>> {
        self.0.lock().unwrap_or_else(PoisonError::into_inner)
    }

    /// `bytes` with every written form of each handed-out credential
    /// replaced by `REDACTED`.
    pub fn scrub(&self, mut bytes: Vec<u8>) -> Vec<u8> {
        let forms: Vec<String> = self
            .lock()
            .iter()
            .flat_map(|(username, password)| written_forms(username, password))
            .collect();
        for form in &forms {
            bytes = replace(&bytes, form.as_bytes(), REDACTED.as_bytes());
        }
        bytes
    }
}

/// Every way a password, or the basic-authentication pair it is sent in,
/// can be written into a message: raw; the pair and the password in
/// base64 and base64url, padded and unpadded; and each of those Rust-escaped,
/// JSON-escaped and as a decimal byte list, as Toha's and gix's diagnostics
/// print a byte string.
pub fn written_forms(username: &str, password: &str) -> Vec<String> {
    use base64::engine::general_purpose::{STANDARD, STANDARD_NO_PAD, URL_SAFE, URL_SAFE_NO_PAD};
    if password.is_empty() {
        return vec![];
    }
    let pair = format!("{username}:{password}");
    let mut values = vec![password.to_owned()];
    for plain in [pair.as_str(), password] {
        for engine in [&STANDARD, &STANDARD_NO_PAD, &URL_SAFE, &URL_SAFE_NO_PAD] {
            values.push(engine.encode(plain));
        }
    }
    let mut forms = vec![];
    for value in values {
        let quoted = format!("{value:?}");
        let json = serde_json::to_string(&value).unwrap_or_default();
        let bytes = value.as_bytes();
        forms.push(quoted[1..quoted.len() - 1].to_owned());
        forms.push(value.escape_default().to_string());
        if json.len() >= 2 {
            forms.push(json[1..json.len() - 1].to_owned());
        }
        for separator in [", ", ","] {
            forms.push(
                bytes
                    .iter()
                    .map(u8::to_string)
                    .collect::<Vec<_>>()
                    .join(separator),
            );
        }
        forms.push(value);
    }
    forms.retain(|form| !form.is_empty());
    forms
}

/// `haystack` with every `needle` replaced by `with`.
fn replace(haystack: &[u8], needle: &[u8], with: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(haystack.len());
    let mut rest = haystack;
    while let Some(at) = rest.windows(needle.len()).position(|w| w == needle) {
        out.extend_from_slice(&rest[..at]);
        out.extend_from_slice(with);
        rest = &rest[at + needle.len()..];
    }
    out.extend_from_slice(rest);
    out
}

/// Fires when the operation's caller went away. `lifetime` makes one with
/// the `Alive` handle whose drop fires it.
#[derive(Clone, Debug)]
pub struct Ended(tokio::sync::watch::Receiver<()>);

/// Held for as long as the operation's caller waits for it.
#[derive(Debug)]
pub struct Alive(#[allow(dead_code)] tokio::sync::watch::Sender<()>);

/// An operation's caller handle and its ended signal.
pub fn lifetime() -> (Alive, Ended) {
    let (alive, ended) = tokio::sync::watch::channel(());
    (Alive(alive), Ended(ended))
}

impl Ended {
    /// Wait until the caller went away. No value is ever sent, so the wait
    /// ends only when `Alive` is dropped.
    async fn wait(mut self) {
        while self.0.changed().await.is_ok() {}
    }
}

#[derive(Serialize)]
struct Request<'a> {
    url: &'a str,
}

/// A 200 answer. Its `Debug` output is never needed, so it has none.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Granted {
    username: String,
    password: String,
}

/// A 404 or 502 answer.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Refused {
    reason: String,
}

/// Fixed reasons for a failure of the route itself.
const ENDED: &str = "the operation ended before credentials arrived";
const UNREACHABLE: &str = "the workspace service's credential route could not be reached";
const UNEXPECTED: &str =
    "the workspace service's credential route answered with an unexpected status";
const MALFORMED: &str = "the workspace service's credential route gave a malformed answer";
const NOT_MINTED: &str = "the token could not be minted";

fn refused(reason: &str) -> String {
    format!("{UNAVAILABLE}{reason}")
}

impl CredentialResolver for RouteResolver {
    fn resolve(&self, request: &CredentialRequest) -> Result<Option<Credential>, String> {
        match url(&request.scheme, &request.host, request.port, &request.path) {
            Some(url) => ask(&self.socket, &url, &self.ended, &self.handed),
            None => Ok(None),
        }
    }
}

/// Ask the route behind `socket` for a credential for `url`, unless
/// `ended` fires first; then the request's connection is closed. A granted
/// credential is kept in `handed`. The resolver runs on the operation's
/// blocking worker, so the request runs on a runtime of its own, as a
/// hook's does.
pub fn ask(
    socket: &Path,
    url: &str,
    ended: &Ended,
    handed: &Handed,
) -> Result<Option<Credential>, String> {
    let body = serde_json::to_vec(&Request { url }).map_err(|_| refused(MALFORMED))?;
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_io()
        .build()
        .map_err(|_| refused(UNREACHABLE))?;
    // The ended signal is polled first, so a caller already gone sends no
    // request. Dropping the runtime drops the request and its connection
    // task, which closes the connection.
    let reply = runtime.block_on(async {
        tokio::select! {
            biased;
            () = ended.clone().wait() => None,
            reply = hooks::post(socket, CREDENTIAL_PATH, body) => Some(reply),
        }
    });
    drop(runtime);
    let (status, reply) = reply
        .ok_or_else(|| refused(ENDED))?
        .map_err(|_| refused(UNREACHABLE))?;
    let granted = answer(status, &reply)?;
    Ok(granted.map(|(username, password)| {
        handed.keep(&username, &password);
        Credential::new(username, password)
    }))
}

/// The route's answer as Toha's: a username and password, the default
/// behaviour, or a refusal with a reason this module chose or checked.
fn answer(status: u16, reply: &[u8]) -> Result<Option<(String, String)>, String> {
    match status {
        200 => match object::<Granted>(reply) {
            Some(g) if !g.username.is_empty() && !g.password.is_empty() => {
                Ok(Some((g.username, g.password)))
            }
            _ => Err(refused(MALFORMED)),
        },
        404 => Ok(None),
        502 => Err(refused(
            object::<Refused>(reply)
                .map(|r| r.reason)
                .filter(|r| one_line(r))
                .as_deref()
                .unwrap_or(NOT_MINTED),
        )),
        _ => Err(refused(UNEXPECTED)),
    }
}

/// A reply that is one JSON object of `T`'s members only. Serde would
/// also read a struct from an array.
fn object<T: serde::de::DeserializeOwned>(reply: &[u8]) -> Option<T> {
    let members =
        serde_json::from_slice::<serde_json::Map<String, serde_json::Value>>(reply).ok()?;
    serde_json::from_value(serde_json::Value::Object(members)).ok()
}

/// Whether a route reason may be passed through: one bounded line.
fn one_line(reason: &str) -> bool {
    !reason.is_empty() && reason.len() <= MAX_REASON && !reason.chars().any(char::is_control)
}

/// `https://host[:port]/path`, without the default port, with the path's
/// bytes outside the unreserved set and `/` percent-encoded. Only an
/// `https` request has one: a token never goes to another scheme.
pub fn url(scheme: &str, host: &str, port: u16, path: &str) -> Option<String> {
    if scheme != "https" {
        return None;
    }
    let mut url = String::from("https://");
    if host.contains(':') && !host.starts_with('[') {
        url.push('[');
        url.push_str(host);
        url.push(']');
    } else {
        url.push_str(host);
    }
    if port != 443 {
        url.push(':');
        url.push_str(&port.to_string());
    }
    if !path.starts_with('/') {
        url.push('/');
    }
    for byte in path.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~' | b'/') {
            url.push(char::from(byte));
        } else {
            url.push_str(&format!("%{byte:02X}"));
        }
    }
    Some(url)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::scripted::Scripted;
    use base64::engine::general_purpose::{STANDARD, STANDARD_NO_PAD, URL_SAFE, URL_SAFE_NO_PAD};
    use serde_json::{Value, json};

    /// A token the route grants. No refusal, error or `Debug` output may
    /// hold it.
    const TOKEN: &str = "ghs_sample0token0value";
    const URL: &str = "https://git.example.invalid/example-org/kit.git";

    fn route(status: u16, reply: impl Into<Vec<u8>>) -> Scripted {
        Scripted::start(CREDENTIAL_PATH, status, reply.into())
    }

    fn json_route(status: u16, reply: Value) -> Scripted {
        route(status, serde_json::to_vec(&reply).unwrap())
    }

    /// Ask with a caller that stays.
    fn asked(socket: &Path, url: &str) -> Result<Option<Credential>, String> {
        let (_alive, ended) = lifetime();
        ask(socket, url, &ended, &Handed::default())
    }

    fn free_of_token(result: &Result<Option<Credential>, String>) {
        let text = format!("{result:?}");
        assert!(!text.contains(TOKEN), "{text}");
    }

    #[test]
    fn a_granted_token_is_the_credential_for_the_url_it_names() {
        let route = json_route(
            200,
            json!({"username": "x-access-token", "password": TOKEN}),
        );
        let result = asked(&route.socket, URL);
        assert_eq!(
            result,
            Ok(Some(Credential::new("x-access-token", TOKEN))),
            "the credential"
        );
        free_of_token(&result);
        assert_eq!(route.requests(), [json!({"url": URL})]);
    }

    #[test]
    fn a_url_not_for_the_app_keeps_the_default_behaviour() {
        let route = json_route(404, json!({"reason": "no GitHub App is configured"}));
        assert_eq!(asked(&route.socket, URL), Ok(None));
    }

    #[test]
    fn a_minting_failure_passes_the_routes_one_line_reason_through() {
        let route = json_route(
            502,
            json!({"reason": "the App is not installed for the repository"}),
        );
        assert_eq!(
            asked(&route.socket, URL),
            Err("GitHub App credentials are unavailable: the App is not installed for the repository".into())
        );
    }

    /// A reason that is not one bounded line, or no reason, gives the fixed
    /// one: the route's text reaches a job only when it is a fixed sentence.
    #[test]
    fn a_minting_failure_without_a_usable_reason_has_a_fixed_one() {
        let fixed = Err::<Option<Credential>, _>(
            "GitHub App credentials are unavailable: the token could not be minted".to_owned(),
        );
        for reply in [
            json!({"reason": format!("first line\n{TOKEN}")}),
            json!({"reason": format!("tab\t{TOKEN}")}),
            json!({"reason": "x".repeat(MAX_REASON + 1)}),
            json!({"reason": ""}),
            json!({"reason": "a reason", "extra": TOKEN}),
            json!({"message": TOKEN}),
            json!([TOKEN]),
        ] {
            let route = json_route(502, reply.clone());
            let result = asked(&route.socket, URL);
            assert_eq!(result, fixed, "{reply}");
        }
        let route = json_route(502, json!({"reason": "x".repeat(MAX_REASON)}));
        assert_eq!(
            asked(&route.socket, URL),
            Err(format!("{UNAVAILABLE}{}", "x".repeat(MAX_REASON)))
        );
    }

    #[test]
    fn a_malformed_grant_fails_without_its_text() {
        for reply in [
            json!({"username": "x-access-token"}),
            json!({"username": "x-access-token", "password": ""}),
            json!({"username": "", "password": TOKEN}),
            json!({"username": "x-access-token", "password": TOKEN, "extra": TOKEN}),
            json!({"username": "x-access-token", "password": [TOKEN]}),
            json!(TOKEN),
        ] {
            let route = json_route(200, reply.clone());
            let result = asked(&route.socket, URL);
            assert_eq!(
                result,
                Err(
                    "GitHub App credentials are unavailable: the workspace service's credential route gave a malformed answer"
                        .into()
                ),
                "{reply}"
            );
            free_of_token(&result);
        }
        let route = route(200, format!("not json {TOKEN}"));
        let result = asked(&route.socket, URL);
        assert!(result.is_err());
        free_of_token(&result);
    }

    #[test]
    fn any_other_status_fails_with_a_fixed_reason() {
        for status in [400, 401, 403, 405, 500, 503] {
            let route = json_route(status, json!({"reason": TOKEN}));
            let result = asked(&route.socket, URL);
            assert_eq!(
                result,
                Err(
                    "GitHub App credentials are unavailable: the workspace service's credential route answered with an unexpected status"
                        .into()
                ),
                "{status}"
            );
        }
    }

    #[test]
    fn an_unreachable_route_fails_with_a_fixed_reason() {
        let dir = tempfile::tempdir().unwrap();
        assert_eq!(
            asked(&dir.path().join("absent.sock"), URL),
            Err(
                "GitHub App credentials are unavailable: the workspace service's credential route could not be reached"
                    .into()
            )
        );
    }

    #[test]
    fn the_url_names_the_origin_and_repository_without_the_default_port() {
        let url = |scheme, host, port, path| super::url(scheme, host, port, path);
        assert_eq!(
            url("https", "github.com", 443, "/example-org/kit.git").as_deref(),
            Some("https://github.com/example-org/kit.git")
        );
        assert_eq!(
            url("https", "git.example.invalid", 8443, "/example-org/kit").as_deref(),
            Some("https://git.example.invalid:8443/example-org/kit")
        );
        assert_eq!(
            url("https", "::1", 8443, "/o/r.git").as_deref(),
            Some("https://[::1]:8443/o/r.git")
        );
        // A decoded path is encoded again, so the route sees the bytes
        // Toha named and no query or fragment.
        assert_eq!(
            url("https", "github.com", 443, "/o/r?x#y z%").as_deref(),
            Some("https://github.com/o/r%3Fx%23y%20z%25")
        );
        assert_eq!(
            url("https", "github.com", 443, "o/r").as_deref(),
            Some("https://github.com/o/r")
        );
        assert_eq!(url("http", "github.com", 443, "/o/r"), None);
    }

    #[test]
    fn a_granted_credential_is_kept_and_scrubbed() {
        let route = json_route(
            200,
            json!({"username": "x-access-token", "password": TOKEN}),
        );
        let (_alive, ended) = lifetime();
        let handed = Handed::default();
        ask(&route.socket, URL, &ended, &handed).unwrap().unwrap();
        let echoed = format!(
            "{:?}",
            format!(
                "Basic {}",
                STANDARD.encode(format!("x-access-token:{TOKEN}"))
            )
            .as_bytes()
        );
        let scrubbed = String::from_utf8(handed.scrub(echoed.into_bytes())).unwrap();
        assert_eq!(scrubbed, format!("[66, 97, 115, 105, 99, 32, {REDACTED}]"));
        // Nothing else changes, and without a handed credential nothing does.
        let plain = b"no credential here, [1, 2, 3]".to_vec();
        assert_eq!(handed.scrub(plain.clone()), plain);
        assert_eq!(Handed::default().scrub(TOKEN.into()), TOKEN.as_bytes());
        assert!(!format!("{handed:?}").contains(TOKEN));
    }

    /// Each written form goes whole. The password and the user name are
    /// chosen so that every encoding pads and differs between base64 and
    /// base64url, and the pair's encoding does not hold the password's.
    #[test]
    fn every_written_form_is_scrubbed_whole() {
        let (username, password) = ("user", "ghs_???a");
        let handed = Handed::default();
        handed.keep(username, password);
        let listed = |v: &str, separator: &str| {
            v.bytes()
                .map(|b| b.to_string())
                .collect::<Vec<_>>()
                .join(separator)
        };
        let mut forms = vec![password.to_owned()];
        for plain in [format!("{username}:{password}"), password.to_owned()] {
            for engine in [&STANDARD, &STANDARD_NO_PAD, &URL_SAFE, &URL_SAFE_NO_PAD] {
                let encoded = engine.encode(&plain);
                forms.push(listed(&encoded, ", "));
                forms.push(listed(&encoded, ","));
                forms.push(encoded);
            }
        }
        assert_eq!(
            forms.iter().collect::<std::collections::HashSet<_>>().len(),
            forms.len(),
            "{forms:?}"
        );
        for form in forms {
            let scrubbed =
                String::from_utf8(handed.scrub(format!("before [{form}] after").into_bytes()))
                    .unwrap();
            assert_eq!(scrubbed, format!("before [{REDACTED}] after"), "{form}");
        }
    }

    /// A password with characters that Rust's `Debug`, `escape_default` and
    /// JSON each escape differently is scrubbed raw and in each of those
    /// forms.
    #[test]
    fn an_escaped_password_is_scrubbed_in_each_escaping() {
        let password = "a'b\u{1}c\"d";
        let handed = Handed::default();
        handed.keep("x-access-token", password);
        let debug = format!("{password:?}");
        let json = serde_json::to_string(password).unwrap();
        let forms = [
            password.to_owned(),
            debug[1..debug.len() - 1].to_owned(),
            password.escape_default().to_string(),
            json[1..json.len() - 1].to_owned(),
        ];
        assert_eq!(
            forms.iter().collect::<std::collections::HashSet<_>>().len(),
            4,
            "{forms:?}"
        );
        for form in forms {
            let scrubbed =
                String::from_utf8(handed.scrub(format!("[{form}]").into_bytes())).unwrap();
            assert_eq!(scrubbed, format!("[{REDACTED}]"), "{form}");
        }
    }

    #[test]
    fn an_ended_operation_asks_nothing() {
        let route = json_route(
            200,
            json!({"username": "x-access-token", "password": TOKEN}),
        );
        let (alive, ended) = lifetime();
        drop(alive);
        assert_eq!(
            ask(&route.socket, URL, &ended, &Handed::default()),
            Err("GitHub App credentials are unavailable: the operation ended before credentials arrived".into())
        );
        assert!(route.requests().is_empty());
    }

    /// A request in flight when the caller goes away loses its connection
    /// and ends with a fixed refusal.
    #[test]
    fn a_caller_going_away_closes_the_credential_request() {
        use std::io::Read;
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("hook-runner.sock");
        let listener = std::os::unix::net::UnixListener::bind(&socket).unwrap();
        let (alive, ended) = lifetime();
        let asking = std::thread::spawn(move || ask(&socket, URL, &ended, &Handed::default()));
        let (mut held, _) = listener.accept().unwrap();
        held.set_read_timeout(Some(std::time::Duration::from_secs(10)))
            .unwrap();
        let mut request = [0u8; 4096];
        let mut seen = 0;
        while !String::from_utf8_lossy(&request[..seen]).contains(URL) {
            seen += held.read(&mut request[seen..]).unwrap();
        }
        drop(alive);
        assert_eq!(
            held.read(&mut request).unwrap(),
            0,
            "the connection stayed open"
        );
        assert_eq!(
            asking.join().unwrap(),
            Err("GitHub App credentials are unavailable: the operation ended before credentials arrived".into())
        );
    }

    #[test]
    fn the_resolver_hides_the_token_in_debug_output() {
        let credential = Credential::new("x-access-token", TOKEN);
        assert!(!format!("{credential:?}").contains(TOKEN));
        let (_alive, ended) = lifetime();
        let resolver = RouteResolver {
            socket: "/run/sample/hook-runner.sock".into(),
            handed: Handed::default(),
            ended,
        };
        resolver.handed.keep("x-access-token", TOKEN);
        assert!(!format!("{resolver:?}").contains(TOKEN));
    }
}
