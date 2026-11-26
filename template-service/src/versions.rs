//! Template versions: the release tags of a template's repository. A
//! version is a tag `MAJOR.MINOR.PATCH`, with an optional leading `v`, whose
//! numbers have no leading zero; a tag with a pre-release or build part is
//! not one. Versions order by their numbers, and when `1.2.0` and `v1.2.0`
//! both exist, the tag without `v` is the version's.
//!
//! Toha lists no tags, so this service lists them with gix, the library Toha
//! fetches with, through the same transports: the remote's references are
//! read without fetching its history, an HTTPS challenge is answered by the
//! request's credential route as a Toha fetch's is, and an annotated tag is
//! peeled to its commit.
//!
//! Toha's SSH key scope does not reach this connection, so a request that
//! names an SSH key pins it here: the agent must hold the key, and `ssh`
//! runs with `IdentitiesOnly`, `IdentityFile` and `IdentityAgent` naming
//! only that key and agent. Unlike Toha, this does not run `ssh -G` to
//! check that no SSH configuration adds another identity; Menagerie's
//! container sets none (wyrd-company/toha#94 would list tags through Toha).
use crate::credentials::{Ended, Handed, Scope};
use crate::error::ServiceError;
use serde::Serialize;
use std::path::Path;
use std::sync::mpsc;
use std::time::Duration;

/// A version's numbers.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct Version(pub u64, pub u64, pub u64);

impl std::fmt::Display for Version {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}.{}.{}", self.0, self.1, self.2)
    }
}

impl Version {
    /// The version a tag names, or `None` for a tag that is not a version.
    pub fn of_tag(tag: &str) -> Option<Self> {
        let numbers = tag.strip_prefix('v').unwrap_or(tag);
        let mut parts = numbers.split('.');
        let mut next = || -> Option<u64> {
            let part = parts.next()?;
            let digits = !part.is_empty() && part.bytes().all(|b| b.is_ascii_digit());
            if !digits || (part.len() > 1 && part.starts_with('0')) {
                return None;
            }
            part.parse().ok()
        };
        let version = Self(next()?, next()?, next()?);
        parts.next().is_none().then_some(version)
    }
}

/// One version of a repository: its numbers, the tag that gives it and the
/// commit that tag names.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Found {
    pub version: String,
    pub tag: String,
    pub commit: String,
}

/// The versions among `tags`, each `(tag, commit)`, in ascending order.
pub fn versions(tags: impl IntoIterator<Item = (String, String)>) -> Vec<Found> {
    let mut chosen: std::collections::BTreeMap<Version, (String, String)> = Default::default();
    for (tag, commit) in tags {
        let Some(version) = Version::of_tag(&tag) else {
            continue;
        };
        let plain = !tag.starts_with('v');
        match chosen.get(&version) {
            Some((kept, _)) if !kept.starts_with('v') || !plain => {}
            _ => {
                chosen.insert(version, (tag, commit));
            }
        }
    }
    chosen
        .into_iter()
        .map(|(version, (tag, commit))| Found {
            version: version.to_string(),
            tag,
            commit,
        })
        .collect()
}

/// Why a repository's tags could not be listed.
#[derive(Debug)]
pub enum ListError {
    /// The remote answered that the repository does not exist.
    NotFound,
    /// Any other failure.
    Failed,
    /// The request's SSH key could not be offered: the message says why and
    /// names the key.
    Key(String),
    /// The caller went away.
    Stopped,
}

impl From<ListError> for ServiceError {
    fn from(error: ListError) -> Self {
        match error {
            ListError::NotFound => ServiceError::refused(
                "template-not-found",
                "the template's repository was not found on the Git host",
            ),
            ListError::Failed => ServiceError::refused(
                "template-resolve-failed",
                "the template's repository could not be listed; the Git host refused or failed the request",
            ),
            ListError::Key(message) => ServiceError::refused("template-resolve-failed", message),
            ListError::Stopped => ServiceError::stopped(),
        }
    }
}

/// How often a waiting request checks whether its caller went away.
const POLL: Duration = Duration::from_millis(50);

/// The tags of the repository `repo`, each `(tag, commit)`. The listing runs
/// on a thread of its own, so a caller that goes away gets its worker back
/// at once; gix cannot interrupt a transfer that waits for the remote, so
/// that thread keeps its connection until the remote acts, as an abandoned
/// Toha fetch does.
pub fn list_tags(
    repo: &str,
    hook_runner_socket: &Path,
    scope: &Scope,
    ended: &Ended,
    handed: &Handed,
) -> Result<Vec<(String, String)>, ListError> {
    ended.check().map_err(|_| ListError::Stopped)?;
    let (sent, received) = mpsc::channel();
    let (repo, socket, scope, ended_for_listing, handed) = (
        repo.to_owned(),
        hook_runner_socket.to_owned(),
        scope.clone(),
        ended.clone(),
        handed.clone(),
    );
    std::thread::Builder::new()
        .name("version listing".into())
        .spawn(move || {
            let _ = sent.send(remote_tags(
                &repo,
                &socket,
                &scope,
                &ended_for_listing,
                &handed,
            ));
        })
        .map_err(|_| ListError::Failed)?;
    loop {
        match received.recv_timeout(POLL) {
            Ok(result) => return result,
            Err(mpsc::RecvTimeoutError::Disconnected) => return Err(ListError::Failed),
            Err(mpsc::RecvTimeoutError::Timeout) => {
                if ended.check().is_err() {
                    return Err(ListError::Stopped);
                }
            }
        }
    }
}

fn remote_tags(
    repo: &str,
    socket: &Path,
    scope: &Scope,
    ended: &Ended,
    handed: &Handed,
) -> Result<Vec<(String, String)>, ListError> {
    use gix::protocol::handshake::Ref;
    // The listing needs a repository for gix's configuration; an empty
    // bare one, opened as a clone's is, receives nothing.
    let scratch = tempfile::tempdir().map_err(|_| ListError::Failed)?;
    let mut local = gix::init_bare(scratch.path()).map_err(|_| ListError::Failed)?;
    // Kept until the listing ends: the file `IdentityFile` names.
    let _pinned = match &scope.key {
        Some(key) => pin(&mut local, repo, key)?,
        None => None,
    };
    let remote = local
        .remote_at(repo)
        .and_then(|remote| {
            remote.with_refspecs(["+refs/tags/*:refs/tags/*"], gix::remote::Direction::Fetch)
        })
        .map_err(|_| ListError::Failed)?;
    let mut connection = remote
        .connect(gix::remote::Direction::Fetch)
        .map_err(|_| ListError::Failed)?;
    let fallback = connection.configured_credentials_for_current_url();
    connection.set_credentials(authenticate(socket, scope, ended, handed, fallback));
    quiet(&mut connection);
    let (map, _) = connection
        .ref_map(
            gix::progress::Discard,
            gix::remote::ref_map::Options::default(),
        )
        .map_err(|error| {
            if ended.check().is_err() {
                ListError::Stopped
            } else if missing(&format!("{error:#}")) {
                ListError::NotFound
            } else {
                ListError::Failed
            }
        })?;
    let mut tags = Vec::new();
    for reference in map.remote_refs {
        let (name, commit) = match reference {
            Ref::Peeled {
                full_ref_name,
                object,
                ..
            }
            | Ref::Direct {
                full_ref_name,
                object,
            } => (full_ref_name, object),
            Ref::Symbolic { .. } | Ref::Unborn { .. } => continue,
        };
        let Some(tag) = name
            .to_string()
            .strip_prefix("refs/tags/")
            .map(str::to_owned)
        else {
            continue;
        };
        tags.push((tag, commit.to_string()));
    }
    Ok(tags)
}

/// Make `local`'s SSH connections to `repo` offer only `key`, once the agent
/// at `SSH_AUTH_SOCK` has shown that it holds it, and return the public key
/// file `IdentityFile` names. A `repo` that is not SSH, after Git's
/// `url.<base>.insteadOf` rewrites, needs nothing.
fn pin(
    local: &mut gix::Repository,
    repo: &str,
    key: &toha::source::SshKey,
) -> Result<Option<tempfile::NamedTempFile>, ListError> {
    use std::io::Write;
    let ssh = local
        .remote_at(repo)
        .ok()
        .and_then(|remote| remote.url(gix::remote::Direction::Fetch).cloned())
        .is_some_and(|url| url.scheme == gix::url::Scheme::Ssh);
    if !ssh {
        return Ok(None);
    }
    let failed = |why: String| ListError::Key(format!("the SSH key {key} {why}"));
    let agent = std::env::var_os("SSH_AUTH_SOCK")
        .filter(|socket| !socket.is_empty())
        .ok_or_else(|| failed("has no agent to come from: SSH_AUTH_SOCK is not set".into()))?;
    let blob = agent_key(Path::new(&agent), &key.fingerprint())
        .map_err(|e| failed(format!("could not be read from the SSH agent: {e}")))?
        .ok_or_else(|| failed("is not offered by the SSH agent".into()))?;
    let mut file = tempfile::Builder::new()
        .prefix("menagerie_ssh_key")
        .suffix(".pub")
        .tempfile()
        .map_err(|e| failed(format!("could not be written: {e}")))?;
    {
        use base64::Engine;
        writeln!(
            file,
            "{} {}",
            key.algorithm(),
            base64::engine::general_purpose::STANDARD.encode(&blob)
        )
        .and_then(|()| file.flush())
        .map_err(|e| failed(format!("could not be written: {e}")))?;
    }
    let configured = local
        .ssh_connect_options()
        .map_err(|e| failed(format!("could not be offered: {e}")))?;
    let program = match configured.command.as_ref().filter(|c| !c.is_empty()) {
        Some(command) => {
            let command = command.to_str().ok_or_else(|| {
                failed("could not be offered: the SSH command is not UTF-8".into())
            })?;
            // `GIT_SSH` names a program, which runs without a shell.
            if configured.disallow_shell {
                quoted(command)
            } else {
                command.to_owned()
            }
        }
        None => "ssh".to_owned(),
    };
    let utf8 = |path: &Path| {
        path.to_str().map(str::to_owned).ok_or_else(|| {
            failed(format!(
                "could not be offered: the path {} is not UTF-8",
                path.display()
            ))
        })
    };
    let command = format!(
        "{program} -o IdentitiesOnly=yes -o {} -o {}",
        quoted(&format!("IdentityFile={}", utf8(file.path())?)),
        quoted(&format!("IdentityAgent={}", utf8(Path::new(&agent))?)),
    );
    let mut config = local.config_snapshot_mut();
    config
        .append_config(
            [
                format!("core.sshCommand={command}"),
                "ssh.variant=ssh".into(),
            ],
            gix::config::Source::Api,
        )
        .map_err(|e| failed(format!("could not be offered: {e}")))?;
    config
        .commit()
        .map_err(|e| failed(format!("could not be offered: {e}")))?;
    Ok(Some(file))
}

/// `value` quoted for `sh`.
fn quoted(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

/// The key blob among the identities the agent at `socket` lists whose
/// SHA-256 fingerprint is `fingerprint`, or `None` when it lists none.
fn agent_key(socket: &Path, fingerprint: &str) -> Result<Option<Vec<u8>>, String> {
    use base64::Engine;
    use std::io::{Read, Write};
    const REQUEST_IDENTITIES: u8 = 11;
    const IDENTITIES_ANSWER: u8 = 12;
    const LONGEST_ANSWER: usize = 256 << 10;
    let mut agent = std::os::unix::net::UnixStream::connect(socket).map_err(|e| e.to_string())?;
    agent
        .write_all(&[0, 0, 0, 1, REQUEST_IDENTITIES])
        .map_err(|e| e.to_string())?;
    let mut length = [0; 4];
    agent.read_exact(&mut length).map_err(|e| e.to_string())?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > LONGEST_ANSWER {
        return Err("the agent's answer has no usable length".into());
    }
    let mut answer = vec![0; length];
    agent.read_exact(&mut answer).map_err(|e| e.to_string())?;
    let malformed = || "the agent's answer is malformed".to_owned();
    let (&kind, mut rest) = answer.split_first().ok_or_else(malformed)?;
    if kind != IDENTITIES_ANSWER {
        return Err("the agent refused to list its keys".into());
    }
    let take = |rest: &mut &[u8]| -> Option<Vec<u8>> {
        let (length, tail) = rest.split_first_chunk::<4>()?;
        let length = u32::from_be_bytes(*length) as usize;
        let (value, tail) = (tail.get(..length)?, tail.get(length..)?);
        *rest = tail;
        Some(value.to_vec())
    };
    let count = rest
        .split_first_chunk::<4>()
        .map(|(count, tail)| {
            rest = tail;
            u32::from_be_bytes(*count)
        })
        .ok_or_else(malformed)?;
    for _ in 0..count {
        let blob = take(&mut rest).ok_or_else(malformed)?;
        take(&mut rest).ok_or_else(malformed)?;
        let digest = ring::digest::digest(&ring::digest::SHA256, &blob);
        let listed = format!(
            "SHA256:{}",
            base64::engine::general_purpose::STANDARD_NO_PAD.encode(digest.as_ref())
        );
        if listed == fingerprint {
            return Ok(Some(blob));
        }
    }
    Ok(None)
}

/// Whether a listing failure is the remote's answer that the repository
/// does not exist: an HTTP 404, or the answer Git hosts give over SSH.
fn missing(message: &str) -> bool {
    let lower = message.to_ascii_lowercase();
    lower.contains("http status 404")
        || lower.contains("repository not found")
        || lower.contains("does not appear to be a git repository")
        || lower.contains("project you were looking for could not be found")
}

type Connection<'a, 'b, 'c> = gix::remote::Connection<
    'a,
    'b,
    'c,
    Box<dyn gix::protocol::transport::client::blocking_io::Transport + Send>,
>;

/// curl's verbose output writes request headers, a credential among them,
/// to standard error, so it stays off, as in Toha's fetches.
fn quiet(connection: &mut Connection<'_, '_, '_>) {
    use gix::bstr::ByteSlice;
    let url = connection.transport_mut().to_url().into_owned();
    let remote = connection.remote();
    let Ok(Some(mut options)) = remote.repo().transport_options(
        url.as_bytes().as_bstr(),
        remote.name().map(gix::remote::Name::as_bstr),
    ) else {
        return;
    };
    if let Some(http) =
        options.downcast_mut::<gix::protocol::transport::client::blocking_io::http::Options>()
    {
        http.verbose = false;
        if http.ssl_ca_info.is_none()
            && http.ssl_verify
            && let Some(bundle) = native_roots()
        {
            http.ssl_ca_info = Some(bundle.to_owned());
        }
    }
    connection.set_transport_options(options);
}

/// The platform's trusted roots as one bundle, written once for the
/// process, for curl's bundled rustls, which reads no platform store, when
/// Git configuration names no bundle: what Toha gives its own fetches.
fn native_roots() -> Option<&'static Path> {
    use base64::Engine;
    use std::io::Write;
    static BUNDLE: std::sync::OnceLock<Option<tempfile::NamedTempFile>> =
        std::sync::OnceLock::new();
    BUNDLE
        .get_or_init(|| {
            let loaded = rustls_native_certs::load_native_certs();
            if loaded.certs.is_empty() {
                return None;
            }
            let mut bundle = tempfile::NamedTempFile::new().ok()?;
            for cert in loaded.certs {
                let encoded = base64::engine::general_purpose::STANDARD.encode(cert.as_ref());
                writeln!(bundle, "-----BEGIN CERTIFICATE-----").ok()?;
                for line in encoded.as_bytes().chunks(64) {
                    bundle.write_all(line).ok()?;
                    bundle.write_all(b"\n").ok()?;
                }
                writeln!(bundle, "-----END CERTIFICATE-----").ok()?;
            }
            bundle.flush().ok()?;
            Some(bundle)
        })
        .as_ref()
        .map(tempfile::NamedTempFile::path)
}

/// Answer each HTTPS challenge from the request's credential route, as the
/// resolver of a Toha fetch does; other challenges keep the credentials of
/// the Git configuration.
#[allow(clippy::result_large_err)]
fn authenticate(
    socket: &Path,
    scope: &Scope,
    ended: &Ended,
    handed: &Handed,
    mut fallback: gix::remote::AuthenticateFn<'static>,
) -> impl FnMut(
    gix::credentials::helper::Action,
) -> gix::Result<Option<gix::credentials::protocol::Outcome>>
+ use<> {
    let (socket, ended, handed) = (socket.to_owned(), ended.clone(), handed.clone());
    // A template whose credential is an SSH key has no App, as in Toha's
    // fetches (`credentials::RouteResolver`).
    let asks = scope.key.is_none();
    let app = scope.app.clone();
    let mut answered = false;
    move |action| {
        if let gix::credentials::helper::Action::Get(context) = &action {
            answered = false;
            let url = context
                .url
                .clone()
                .or_else(|| context.to_url())
                .and_then(|url| gix::url::parse(gix::bstr::BStr::new(url.as_slice())).ok())
                .filter(|url| asks && url.scheme == gix::url::Scheme::Https)
                .and_then(|url| {
                    let path = String::from_utf8_lossy(&url.path).into_owned();
                    crate::credentials::url(
                        "https",
                        &url.host()?.to_ascii_lowercase(),
                        url.port_or_default()?,
                        &path,
                    )
                });
            if let Some(url) = url {
                match crate::credentials::ask_pair(&socket, &url, app.as_deref(), &ended, &handed) {
                    Ok(Some((username, password))) => {
                        answered = true;
                        return Ok(Some(gix::credentials::protocol::Outcome {
                            identity: gix::sec::identity::Account {
                                username,
                                password,
                                oauth_refresh_token: None,
                            },
                            next: context.clone().into(),
                        }));
                    }
                    Ok(None) => {}
                    Err(_) => {
                        return Err(gix::error::message(
                            "The handler asked to stop trying to obtain credentials",
                        )
                        .cancelled_error());
                    }
                }
            }
        } else if answered {
            return Ok(None);
        }
        fallback(action)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_version_is_a_release_tag() {
        for (tag, version) in [
            ("1.2.3", Some(Version(1, 2, 3))),
            ("v1.2.3", Some(Version(1, 2, 3))),
            ("0.0.0", Some(Version(0, 0, 0))),
            ("10.20.30", Some(Version(10, 20, 30))),
            ("1.2.3-rc.1", None),
            ("1.2.3+build", None),
            ("v1.2", None),
            ("1", None),
            ("v1", None),
            ("01.2.3", None),
            ("1.02.3", None),
            ("1.2.03", None),
            ("1.2.3.4", None),
            ("V1.2.3", None),
            ("vv1.2.3", None),
            ("1..3", None),
            ("", None),
            ("main", None),
            ("18446744073709551616.0.0", None),
        ] {
            assert_eq!(Version::of_tag(tag), version, "{tag}");
        }
    }

    #[test]
    fn versions_order_by_number_and_prefer_the_plain_tag() {
        let tag = |t: &str, c: &str| (t.to_owned(), c.to_owned());
        let found = versions([
            tag("v1.10.0", "c"),
            tag("1.2.0", "a"),
            tag("v1.2.0", "b"),
            tag("1.9.0-rc.1", "x"),
            tag("v2.0.0", "d"),
            tag("latest", "y"),
            tag("1.3.0", "e"),
            tag("v1.3.0", "f"),
            // The plain tag also wins when the prefixed one comes first.
            tag("v1.4.0", "g"),
            tag("1.4.0", "h"),
        ]);
        let found: Vec<(&str, &str, &str)> = found
            .iter()
            .map(|f| (f.version.as_str(), f.tag.as_str(), f.commit.as_str()))
            .collect();
        assert_eq!(
            found,
            [
                ("1.2.0", "1.2.0", "a"),
                ("1.3.0", "1.3.0", "e"),
                ("1.4.0", "1.4.0", "h"),
                ("1.10.0", "v1.10.0", "c"),
                ("2.0.0", "v2.0.0", "d"),
            ]
        );
    }

    #[test]
    fn a_missing_repository_is_told_apart() {
        assert!(missing(
            "Received HTTP status 404 while fetching https://host/r"
        ));
        assert!(missing("ERROR: Repository not found."));
        assert!(!missing("Received HTTP status 500"));
        assert!(!missing("Received HTTP status 403"));
    }
}
