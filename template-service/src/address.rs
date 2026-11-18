//! Template addresses the service may read. The workspace service names each
//! address on behalf of a manager's registration or a registered template;
//! this service holds no list of templates, so every route admits the
//! address it is given before any other work: a remote Git address of the
//! forms the user documentation lists, and nothing that would make it read a
//! local folder, a Toha registry entry or a `file://` repository as a
//! template. No refusal quotes the address, so a password given by mistake
//! is shown nowhere.
//!
//! A password can hide from a password check, since Git, gix and Toha end a
//! URL's authority at its first `/`: `https://user:12/secret@host` reads as
//! host `user`, port 12, path `/secret` and ref `host`. So an address is
//! admitted only when it reads one way: the parts the fetcher (gix) and
//! Toha's grammar read, each in a strict character set, written back by a
//! strict writer, are the address exactly; and where the `@` before a ref
//! could end such a password, after `git@host:path`, after a port with no
//! user name, or before a ref that could be a host name, the ref is a
//! version's tag.
use crate::error::ServiceError;
use crate::host::TohaHost;
use crate::versions::Version;
use toha::source::{self, Address};

/// An address this service may read, with what Toha's grammar makes of it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Admitted {
    /// The address with its ref, as Toha parses it.
    pub address: Address,
    /// The repository Toha fetches, with the user name an SSH address keeps.
    pub repo: String,
    /// The ref the address names, or `None`.
    pub reference: Option<String>,
    /// Toha's formal name of the repository and folder without a ref: the
    /// `source` an applied record names.
    pub source: String,
    /// The address as the strict writer writes it without its ref; an
    /// `https` address also loses its user name.
    pub base: String,
}

impl Admitted {
    /// The address at `reference`, as Toha's address type writes it.
    pub fn at(&self, reference: &str) -> Address {
        match &self.address {
            Address::Git { repo, path, .. } => Address::Git {
                repo: repo.clone(),
                reference: Some(reference.to_owned()),
                path: path.clone(),
            },
            other => other.clone(),
        }
    }
}

const NOT_REMOTE: &str = "the address is not a remote Git address: it must be gh:owner/repository, git@host:path, ssh:// or https://";
const PASSWORD: &str = "the address holds a password; give it without one, and let the GitHub App or the SSH agent supply the credential";
const DASHED: &str = "the address's host or user starts with -";
const UNWRITTEN: &str = "the address does not read one way: give it without a password, with a host in lower-case letters, digits, . and -, and a user name, path, ref and folder of letters, digits and . _ ~ - with no % escapes";
const AFTER_PORT: &str = "the @ before the ref could also end a user name or password that holds a /: a ref that holds a ., or follows git@host:path or a port with no user name, must be a version's tag";

fn refused(message: &'static str) -> ServiceError {
    ServiceError::bad_request("invalid-address", message)
}

/// The form an admitted address is written in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Form {
    Shorthand,
    Scp,
    Url(&'static str),
}

/// Whether every character of `part` is one `allowed` admits, and there is
/// at least one.
fn strictly(part: &str, allowed: fn(char) -> bool) -> bool {
    !part.is_empty() && part.chars().all(allowed)
}

fn user_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '~' | '-')
}

fn host_char(c: char) -> bool {
    c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '.' | '-')
}

fn path_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '~' | '-' | '/')
}

fn ref_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-')
}

fn folder_char(c: char) -> bool {
    c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-' | '/')
}

/// The address the strict writer writes from the parts gix reads of the
/// repository, the ref and the folder: `user@` only when `with_user`.
fn write(
    form: Form,
    url: &gix::Url,
    with_user: bool,
    reference: Option<&str>,
    folder: Option<&str>,
) -> Option<String> {
    let path = std::str::from_utf8(url.path.as_ref()).ok()?;
    let host = url.host()?;
    let mut written = match form {
        Form::Shorthand => format!("gh:{}", path.strip_prefix('/')?),
        Form::Scp => format!("{}@{host}:{path}", url.user()?),
        Form::Url(scheme) => {
            let mut written = format!("{scheme}://");
            if let Some(user) = url.user().filter(|_| with_user) {
                written.push_str(user);
                written.push('@');
            }
            written.push_str(host);
            if let Some(port) = url.port {
                written.push_str(&format!(":{port}"));
            }
            // gix reads `ssh://host/~user/repo` as path `~user/repo`.
            if !path.starts_with('/') {
                written.push('/');
            }
            written.push_str(path);
            written
        }
    };
    if let Some(reference) = reference {
        written.push('@');
        written.push_str(reference);
    }
    if let Some(folder) = folder {
        written.push('#');
        written.push_str(folder);
    }
    Some(written)
}

/// Admit `address`: `gh:` with an owner and a repository, `git@host:path`,
/// `ssh://` or `https://`, with no password and no host or user that starts
/// with `-`, that reads one way (see the module's description). Toha's
/// grammar reads it with the hosts of the service's Toha configuration, and
/// a `gh:` address must still be `https` once Toha expands it. Reaches no
/// network and no file.
pub fn admit(
    address: &str,
    host: &TohaHost,
    config: &toha::config::Config,
) -> Result<Admitted, ServiceError> {
    let form = if address.starts_with("gh:") {
        Form::Shorthand
    } else if address.starts_with("git@") {
        Form::Scp
    } else if address.starts_with("https://") {
        Form::Url("https")
    } else if address.starts_with("ssh://") {
        Form::Url("ssh")
    } else {
        return Err(refused(NOT_REMOTE));
    };
    if source::has_password(address) {
        return Err(refused(PASSWORD));
    }
    let parsed = source::parse(address, &config.hosts, &host.cwd, &host.paths.home)
        .map_err(|_| refused(NOT_REMOTE))?;
    let Address::Git {
        repo,
        reference,
        path: folder,
    } = &parsed
    else {
        return Err(refused(NOT_REMOTE));
    };
    let url =
        gix::url::parse(gix::bstr::BStr::new(repo.as_str())).map_err(|_| refused(NOT_REMOTE))?;
    let https = url.scheme == gix::url::Scheme::Https;
    let ssh = url.scheme == gix::url::Scheme::Ssh;
    if !(https || ssh) || (form == Form::Shorthand && !https) {
        return Err(refused(NOT_REMOTE));
    }
    let host_name = url.host().unwrap_or_default();
    if host_name.starts_with('-') || url.user().is_some_and(|user| user.starts_with('-')) {
        return Err(refused(DASHED));
    }
    let reference = reference.as_deref();
    let folder = folder.as_deref();
    if url.user().is_some_and(|user| !strictly(user, user_char)) {
        return Err(refused(UNWRITTEN));
    }
    if !strictly(host_name, host_char) {
        return Err(refused(UNWRITTEN));
    }
    if !strictly(&String::from_utf8_lossy(url.path.as_ref()), path_char) {
        return Err(refused(UNWRITTEN));
    }
    if reference.is_some_and(|reference| !strictly(reference, ref_char)) {
        return Err(refused(UNWRITTEN));
    }
    if folder.is_some_and(|folder| !strictly(folder, folder_char)) {
        return Err(refused(UNWRITTEN));
    }
    // Every part the fetcher and Toha read, written back, is the address:
    // nothing was decoded, dropped or read across a separator.
    if write(form, &url, true, reference, folder).as_deref() != Some(address) {
        return Err(refused(UNWRITTEN));
    }
    // `https://user:12/secret@host`, `git@user:/secret@host` and
    // `https://token/secret@host.example` read as a host, a port or path, and
    // a ref: there, and wherever the ref could be a host name, only a
    // version's tag reads one way.
    let host_like = reference.is_some_and(|reference| reference.contains('.'));
    let after_scp = form == Form::Scp;
    let after_port = url.port.is_some() && url.user().is_none();
    if (host_like || after_scp || after_port)
        && reference.is_some_and(|reference| Version::of_tag(reference).is_none())
    {
        return Err(refused(AFTER_PORT));
    }
    // An `https` base loses the user name the fetcher reads.
    let base = write(form, &url, !https || form == Form::Shorthand, None, folder)
        .ok_or_else(|| refused(UNWRITTEN))?;
    let source = Address::Git {
        repo: repo.clone(),
        reference: None,
        path: folder.map(str::to_owned),
    }
    .formal_name(&config.hosts);
    Ok(Admitted {
        repo: repo.clone(),
        reference: reference.map(str::to_owned),
        address: parsed.clone(),
        source,
        base,
    })
}

const REF_ON_ADDRESS: &str =
    "a template route names the version by its ref; the address must have none";
const NOT_A_REF: &str = "the ref is not a Git tag, branch or commit name";

/// Admit `address`, which names no ref, at `reference`: the repository and
/// folder of the address, at that ref, which is a ref the strict writer
/// writes back.
pub fn admit_at(
    address: &str,
    reference: &str,
    host: &TohaHost,
    config: &toha::config::Config,
) -> Result<Admitted, ServiceError> {
    let plain = admit(address, host, config)?;
    if plain.reference.is_some() {
        return Err(refused(REF_ON_ADDRESS));
    }
    let valid = !reference.is_empty()
        && !reference.starts_with('-')
        && !reference
            .chars()
            .any(|c| c.is_whitespace() || c.is_control() || matches!(c, '@' | '#' | ':'));
    if !valid {
        return Err(refused(NOT_A_REF));
    }
    admit(&with_ref(address, reference), host, config)
}

/// `address`, which names no ref, with `@reference` before its `#` folder.
pub fn with_ref(address: &str, reference: &str) -> String {
    match address.split_once('#') {
        Some((repository, folder)) => format!("{repository}@{reference}#{folder}"),
        None => format!("{address}@{reference}"),
    }
}

/// The address with no `#` folder.
pub fn without_folder(address: &str) -> &str {
    address
        .split_once('#')
        .map_or(address, |(repository, _)| repository)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn context() -> (tempfile::TempDir, TohaHost, toha::config::Config) {
        let temp = tempfile::tempdir().unwrap();
        let host = TohaHost::new(temp.path());
        host.prepare().unwrap();
        let (config, _, _) = host.context().unwrap();
        (temp, host, config)
    }

    #[test]
    fn admits_each_documented_form() {
        let (_temp, host, config) = context();
        for (address, reference, source, base) in [
            ("gh:owner/repo", None, "gh:owner/repo", "gh:owner/repo"),
            (
                "gh:owner/repo@1.2.0#templates/kit",
                Some("1.2.0"),
                "gh:owner/repo#templates/kit",
                "gh:owner/repo#templates/kit",
            ),
            (
                "https://github.com/owner/repo.git@v1.0.0",
                Some("v1.0.0"),
                "gh:owner/repo",
                "https://github.com/owner/repo.git",
            ),
            (
                "https://sample-user@git.example.invalid/owner/repo@1.0.0#kit",
                Some("1.0.0"),
                "https://git.example.invalid/owner/repo#kit",
                "https://git.example.invalid/owner/repo#kit",
            ),
            (
                "https://git.example.invalid:8443/owner/repo@1.0.0",
                Some("1.0.0"),
                "https://git.example.invalid:8443/owner/repo",
                "https://git.example.invalid:8443/owner/repo",
            ),
            (
                "https://sample-user@git.example.invalid:8443/owner/repo@main",
                Some("main"),
                "https://git.example.invalid:8443/owner/repo",
                "https://git.example.invalid:8443/owner/repo",
            ),
            (
                "git@git.example.invalid:owner/repo.git@1.0.0",
                Some("1.0.0"),
                "ssh://git.example.invalid/owner/repo",
                "git@git.example.invalid:owner/repo.git",
            ),
            (
                "ssh://git@git.example.invalid:2222/owner/repo@2.0.0",
                Some("2.0.0"),
                "ssh://git.example.invalid:2222/owner/repo",
                "ssh://git@git.example.invalid:2222/owner/repo",
            ),
        ] {
            let admitted = admit(address, &host, &config).unwrap();
            assert_eq!(admitted.reference.as_deref(), reference, "{address}");
            assert_eq!(admitted.source, source, "{address}");
            assert_eq!(admitted.base, base, "{address}");
            // The base reads back as the same repository and folder.
            let again = admit(&admitted.base, &host, &config).unwrap();
            assert_eq!(again.source, admitted.source, "{address}");
            assert_eq!(again.reference, None, "{address}");
        }
    }

    #[test]
    fn refuses_every_other_address_without_quoting_it() {
        let (temp, host, config) = context();
        let folder = temp.path().join("kit");
        std::fs::create_dir(&folder).unwrap();
        let folder = folder.display().to_string();
        for (address, message) in [
            (folder.as_str(), NOT_REMOTE),
            ("./kit", NOT_REMOTE),
            ("~/kit", NOT_REMOTE),
            ("sample-kit", NOT_REMOTE),
            ("file:///srv/kit", NOT_REMOTE),
            ("file://localhost/srv/kit@1.0.0", NOT_REMOTE),
            ("git://git.example.invalid/owner/repo", NOT_REMOTE),
            ("http://git.example.invalid/owner/repo", NOT_REMOTE),
            ("HTTPS://git.example.invalid/owner/repo", NOT_REMOTE),
            ("ftp://git.example.invalid/owner/repo", NOT_REMOTE),
            ("sample-user@git.example.invalid:owner/repo", NOT_REMOTE),
            ("gh:repo", NOT_REMOTE),
            ("gl:owner/repo", NOT_REMOTE),
            ("git:/sample-password@git.example.invalid:o/r", NOT_REMOTE),
            ("ssh://-oProxyCommand=sample/owner/repo", DASHED),
            ("ssh://-sample@git.example.invalid/owner/repo", DASHED),
            ("git@-oProxyCommand=sample:owner/repo", DASHED),
            (
                "https://sample-user:sample-password@git.example.invalid/owner/repo",
                PASSWORD,
            ),
            (
                "https://sample-user:sample%40password@git.example.invalid/owner/repo@1.0.0",
                PASSWORD,
            ),
            ("https://:sample-password@git.example.invalid/o/r", PASSWORD),
            (
                "ssh://git:sample-password@git.example.invalid/o/r",
                PASSWORD,
            ),
            // An empty password is still a password.
            ("https://sample-user:@git.example.invalid/o/r", PASSWORD),
            ("ssh://sample-user:@git.example.invalid/o/r", PASSWORD),
            // Each guard of the strict reading is the only one that refuses
            // one of these. A host gix reads with the `:` of an empty port:
            (
                "https://sample-user:/sample-password@git.example.invalid",
                UNWRITTEN,
            ),
            // a path that holds an `@`:
            (
                "https://sample-user:12/sample-password@git.example.invalid/o/r@1.0.0",
                UNWRITTEN,
            ),
            // a ref that holds a `/`:
            ("https://git.example.invalid/o/r@feature/sample", UNWRITTEN),
            // a folder that holds an `@`:
            (
                "https://sample-user:12/repo@1.0.0#/sample-password@git.example.invalid",
                UNWRITTEN,
            ),
            // a user name gix decodes, so the writer does not write it back:
            (
                "https://sample-user%3Asample-password@git.example.invalid/o/r",
                UNWRITTEN,
            ),
            ("https://git.example.invalid/o%2Fr", UNWRITTEN),
            // a user name that holds an `@`:
            (
                "https://sample-user@sample-password@git.example.invalid/o/r",
                UNWRITTEN,
            ),
            // an `@` after a port, with no user name, before no version:
            (
                "https://sample-user:12/sample-password@git.example.invalid",
                AFTER_PORT,
            ),
            (
                "ssh://git:22/sample-password@git.example.invalid",
                AFTER_PORT,
            ),
            (
                "https://git.example.invalid:8443/owner/repo@main",
                AFTER_PORT,
            ),
            (
                "git@sample-user:/sample-password@git.example.invalid",
                AFTER_PORT,
            ),
            ("git@git.example.invalid:owner/repo.git@main", AFTER_PORT),
            ("git@sample-user:/sample-password@localhost", AFTER_PORT),
            (
                "https://sample-user:12/sample-password@localhost",
                AFTER_PORT,
            ),
            ("https://12/sample-password@git.example.invalid", AFTER_PORT),
            // The reviewed forms with no path after the host, and the `#`
            // forms that hid user information.
            (
                "https://sample-user:12#x/sample-password@git.example.invalid/o/r",
                UNWRITTEN,
            ),
            (
                "gh:sample-user:12/sample-password@git.example.invalid",
                UNWRITTEN,
            ),
        ] {
            let error = admit(address, &host, &config).unwrap_err();
            assert_eq!(
                (error.status, error.kind, error.message.as_str()),
                (400, "invalid-address", message),
                "{address}"
            );
            let body = String::from_utf8(error.body()).unwrap();
            assert!(!body.contains("sample-password"), "{body}");
            assert!(!body.contains("sample-user"), "{body}");
            assert!(!body.contains("example.invalid"), "{body}");
            assert!(!body.contains("/srv/"), "{body}");
        }
    }

    /// A password in every shape, on every transport, before every tail, is
    /// refused; and were one admitted, the password would be in none of what
    /// admission answers or the fetcher reads.
    #[test]
    fn every_password_shape_is_refused_on_every_transport_and_tail() {
        let (_temp, host, config) = context();
        const MARKER: &str = "sample-marker";
        let shapes = [
            format!("/{MARKER}"),
            format!("12/{MARKER}"),
            String::new(),
            format!("?{MARKER}"),
            format!("#{MARKER}"),
            format!("{MARKER}@x"),
            format!("x@{MARKER}"),
            format!("%2F{MARKER}"),
            format!("%40{MARKER}"),
            format!("{MARKER}%2F"),
        ];
        let url_tails = ["", "/o/r", "/o/r@1.0.0", "#kit", "/o/r#kit"];
        let scp_tails = ["", ":o/r", ":o/r@1.0.0", "#kit", ":o/r#kit"];
        let gh_tails = ["", "/r", "/r@1.0.0", "#kit", "/r#kit"];
        let mut cases = 0;
        for password in &shapes {
            let forms: [(String, &[&str]); 5] = [
                (
                    format!("https://sample-user:{password}@git.example.invalid"),
                    &url_tails,
                ),
                (
                    format!("https://{password}@git.example.invalid"),
                    &url_tails,
                ),
                (
                    format!("ssh://git:{password}@git.example.invalid"),
                    &url_tails,
                ),
                (
                    format!("git@sample-user:{password}@git.example.invalid"),
                    &scp_tails,
                ),
                (format!("gh:sample-user:{password}@example-org"), &gh_tails),
            ];
            for (head, tails) in &forms {
                for tail in *tails {
                    let address = format!("{head}{tail}");
                    cases += 1;
                    match admit(&address, &host, &config) {
                        Err(error) => {
                            assert_eq!(error.kind, "invalid-address", "{address}");
                            assert!(!error.message.contains(MARKER), "{address}");
                        }
                        Ok(admitted) => {
                            let url = gix::url::parse(gix::bstr::BStr::new(admitted.repo.as_str()))
                                .unwrap();
                            for (what, value) in [
                                ("source", admitted.source.as_str()),
                                ("base", admitted.base.as_str()),
                                ("repository", admitted.repo.as_str()),
                                ("user", url.user().unwrap_or_default()),
                            ] {
                                assert!(!value.contains(MARKER), "{address}: {what} {value}");
                            }
                            panic!("admitted {address}");
                        }
                    }
                }
            }
        }
        assert_eq!(cases, shapes.len() * 5 * 5);
    }

    #[test]
    fn a_version_is_named_by_its_ref_beside_an_address_without_one() {
        let (_temp, host, config) = context();
        let at = admit_at("gh:owner/repo#kit", "1.2.0", &host, &config).unwrap();
        assert_eq!(at.reference.as_deref(), Some("1.2.0"));
        assert_eq!(at.source, "gh:owner/repo#kit");
        assert_eq!(
            at.at("v1.0.0").formal_name(&config.hosts),
            "gh:owner/repo@v1.0.0#kit"
        );
        assert_eq!(
            with_ref("gh:owner/repo#kit", "1.2.0"),
            "gh:owner/repo@1.2.0#kit"
        );
        assert_eq!(without_folder("gh:owner/repo#kit"), "gh:owner/repo");
        let error = admit_at("gh:owner/repo@1.0.0", "1.2.0", &host, &config).unwrap_err();
        assert_eq!(error.message, REF_ON_ADDRESS);
        for reference in ["", "-x", "a b", "a@b", "a#b", "a:b", "1.0\n"] {
            let error = admit_at("gh:owner/repo", reference, &host, &config).unwrap_err();
            assert_eq!(
                (error.kind, error.message.as_str()),
                ("invalid-address", NOT_A_REF),
                "{reference:?}"
            );
        }
    }

    #[test]
    fn a_shorthand_must_stay_https_once_expanded() {
        let temp = tempfile::tempdir().unwrap();
        let host = TohaHost::new(temp.path());
        host.prepare().unwrap();
        std::fs::create_dir_all(temp.path().join("config")).unwrap();
        std::fs::write(
            temp.path().join("config/config.yml"),
            "hosts:\n  gh: ssh://git.example.invalid\n",
        )
        .unwrap();
        let (config, _, _) = host.context().unwrap();
        let error = admit("gh:owner/repo", &host, &config).unwrap_err();
        assert_eq!(error.kind, "invalid-address");
    }
}
