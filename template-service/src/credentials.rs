//! GitHub App credentials for Toha's HTTPS fetches. The service holds no
//! credential: when an HTTPS remote challenges, the resolver asks the
//! workspace service's credential route on the hook runner socket for a
//! `contents: read` installation token for that one repository. Every
//! operation runs inside one resolver scope (see `server`), so every fetch
//! of a create, resume, upgrade, catalog update or formal-name lookup asks
//! the route.
//!
//! The token reaches Toha's `Credential`, whose `Debug` output hides it, and
//! nothing else: no error, refusal or log line built here carries it, a
//! reply's text, a URL or an operating system message.
use crate::hooks;
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use toha::source::{Credential, CredentialRequest, CredentialResolver};

/// The credential route on the hook runner socket.
pub const CREDENTIAL_PATH: &str = "/v1/credentials/github";

/// What every refusal starts with, as the workspace service's own GitHub App
/// failures do.
pub const UNAVAILABLE: &str = "GitHub App credentials are unavailable: ";

/// The longest route reason passed through, in bytes. The route's reasons
/// are fixed sentences well within it.
pub const MAX_REASON: usize = 200;

/// The resolver for one operation: it asks the route behind `socket`.
pub fn resolver(socket: &Path) -> Arc<dyn CredentialResolver> {
    Arc::new(RouteResolver {
        socket: socket.to_owned(),
    })
}

/// Asks the workspace service's credential route on the hook runner socket.
#[derive(Debug)]
pub struct RouteResolver {
    socket: PathBuf,
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
            Some(url) => ask(&self.socket, &url),
            None => Ok(None),
        }
    }
}

/// Ask the route behind `socket` for a credential for `url`. The resolver
/// runs on the operation's blocking worker, so the request runs on a
/// runtime of its own, as a hook's does.
pub fn ask(socket: &Path, url: &str) -> Result<Option<Credential>, String> {
    let body = serde_json::to_vec(&Request { url }).map_err(|_| refused(MALFORMED))?;
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_io()
        .build()
        .map_err(|_| refused(UNREACHABLE))?;
    let (status, reply) = runtime
        .block_on(hooks::post(socket, CREDENTIAL_PATH, body))
        .map_err(|_| refused(UNREACHABLE))?;
    answer(status, &reply)
}

/// The route's answer as Toha's: a credential, the default behaviour, or a
/// refusal with a reason this module chose or checked.
fn answer(status: u16, reply: &[u8]) -> Result<Option<Credential>, String> {
    match status {
        200 => match object::<Granted>(reply) {
            Some(g) if !g.username.is_empty() && !g.password.is_empty() => {
                Ok(Some(Credential::new(g.username, g.password)))
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
        let result = ask(&route.socket, URL);
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
        assert_eq!(ask(&route.socket, URL), Ok(None));
    }

    #[test]
    fn a_minting_failure_passes_the_routes_one_line_reason_through() {
        let route = json_route(
            502,
            json!({"reason": "the App is not installed for the repository"}),
        );
        assert_eq!(
            ask(&route.socket, URL),
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
            let result = ask(&route.socket, URL);
            assert_eq!(result, fixed, "{reply}");
        }
        let route = json_route(502, json!({"reason": "x".repeat(MAX_REASON)}));
        assert_eq!(
            ask(&route.socket, URL),
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
            let result = ask(&route.socket, URL);
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
        let result = ask(&route.socket, URL);
        assert!(result.is_err());
        free_of_token(&result);
    }

    #[test]
    fn any_other_status_fails_with_a_fixed_reason() {
        for status in [400, 401, 403, 405, 500, 503] {
            let route = json_route(status, json!({"reason": TOKEN}));
            let result = ask(&route.socket, URL);
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
            ask(&dir.path().join("absent.sock"), URL),
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
    fn the_resolver_hides_the_token_in_debug_output() {
        let credential = Credential::new("x-access-token", TOKEN);
        assert!(!format!("{credential:?}").contains(TOKEN));
        let resolver = RouteResolver {
            socket: "/run/sample/hook-runner.sock".into(),
        };
        assert!(!format!("{resolver:?}").contains(TOKEN));
    }
}
