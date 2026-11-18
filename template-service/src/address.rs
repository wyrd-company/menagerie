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
//! host `user`, port 12, path `/secret` and ref `host`, and
//! `https://alice@host:12/secret@gitserver` as a port, a path and a ref. So
//! an address is admitted only when it reads one way: in an `https://` or
//! `ssh://` address, the text after `://` holds no `:` before an `@`,
//! checked on the text before any parser reads it, which refuses a password
//! and also a port followed by a ref; the parts the fetcher (gix) and
//! Toha's grammar read, each in a strict character set, written back by a
//! strict writer, are the address exactly; and a ref that follows
//! `git@host:path`, or could be a host name, is a version's tag. User
//! information with no `:` is a user name, not a password. A route that
//! names the version beside the address admits the address alone and
//! checks the ref by itself, so a port and a version meet there.
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
const COLON_BEFORE_AT: &str = "after its scheme the address holds a : before an @, as a password or a port before a ref does: give it without a password, and with a port give it without a ref, which takes the newest version; let the GitHub App or the SSH agent supply the credential";
const DASHED: &str = "the address's host or user starts with -";
const UNWRITTEN: &str = "the address does not read one way: give it without a password, with a host in lower-case letters, digits, . and -, and a user name, path, ref and folder of letters, digits and . _ ~ - with no % escapes";
const REF_NOT_VERSION: &str = "the @ before the ref could also end a user name or password that holds a /: a ref that holds a ., or follows git@host:path, must be a version's tag";

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
    if matches!(form, Form::Url(_)) && colon_before_at(address) {
        return Err(refused(COLON_BEFORE_AT));
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
    // `git@user:/secret@host` and `https://token/secret@host.example` read
    // as a host, a path and a ref: there, and wherever the ref could be a
    // host name, only a version's tag reads one way.
    let host_like = reference.is_some_and(|reference| reference.contains('.'));
    let after_scp = form == Form::Scp;
    if (host_like || after_scp)
        && reference.is_some_and(|reference| Version::of_tag(reference).is_none())
    {
        return Err(refused(REF_NOT_VERSION));
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

/// Whether, in an `https://` or `ssh://` address, the text after `://`
/// holds a `:` before an `@`: a password, or a port followed by a ref.
fn colon_before_at(address: &str) -> bool {
    let rest = address.split_once("://").map_or(address, |(_, rest)| rest);
    rest.find(':')
        .is_some_and(|colon| rest[colon..].contains('@'))
}

/// Admit `address`, which names no ref, at `reference`, which a route names
/// beside it: the address alone is admitted, the ref is letters, digits,
/// `.`, `_` and `-`, not starting with `-`, and Toha reads the address at
/// that ref as the admitted repository and folder at exactly that ref.
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
    if !strictly(reference, ref_char) || reference.starts_with('-') {
        return Err(refused(NOT_A_REF));
    }
    let at = source::parse(
        &with_ref(address, reference),
        &config.hosts,
        &host.cwd,
        &host.paths.home,
    )
    .map_err(|_| refused(NOT_A_REF))?;
    if !reads_at(&plain.address, &at, reference) {
        return Err(refused(NOT_A_REF));
    }
    Ok(Admitted {
        address: at,
        reference: Some(reference.to_owned()),
        ..plain
    })
}

/// Whether `at` is `plain`'s repository and folder at exactly `reference`.
fn reads_at(plain: &Address, at: &Address, reference: &str) -> bool {
    let (
        Address::Git {
            repo, path: folder, ..
        },
        Address::Git {
            repo: at_repo,
            reference: at_reference,
            path: at_folder,
        },
    ) = (plain, at)
    else {
        return false;
    };
    if at_repo != repo {
        return false;
    }
    if at_folder != folder {
        return false;
    }
    at_reference.as_deref() == Some(reference)
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
            // A host with a port names no ref; the route names it beside.
            (
                "https://git.example.invalid:8443/owner/repo",
                None,
                "https://git.example.invalid:8443/owner/repo",
                "https://git.example.invalid:8443/owner/repo",
            ),
            (
                "https://sample-user@git.example.invalid:8443/owner/repo",
                None,
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
                "ssh://git@git.example.invalid/owner/repo.git@2.0.0",
                Some("2.0.0"),
                "ssh://git.example.invalid/owner/repo",
                "ssh://git@git.example.invalid/owner/repo.git",
            ),
            (
                "ssh://git@git.example.invalid:2222/owner/repo",
                None,
                "ssh://git.example.invalid:2222/owner/repo",
                "ssh://git@git.example.invalid:2222/owner/repo",
            ),
            // gix reads `/~user/repo` as `~user/repo`; the writer puts the
            // slash back.
            (
                "ssh://git@git.example.invalid/~sample/repo",
                None,
                "ssh://git.example.invalid/~sample/repo",
                "ssh://git@git.example.invalid/~sample/repo",
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
            // A `:` before an `@` after the scheme: a password, empty ones
            // included, or a port and a ref.
            (
                "https://sample-user:sample-password@git.example.invalid/owner/repo",
                COLON_BEFORE_AT,
            ),
            (
                "https://sample-user:sample%40password@git.example.invalid/owner/repo@1.0.0",
                COLON_BEFORE_AT,
            ),
            (
                "https://:sample-password@git.example.invalid/o/r",
                COLON_BEFORE_AT,
            ),
            (
                "ssh://git:sample-password@git.example.invalid/o/r",
                COLON_BEFORE_AT,
            ),
            (
                "https://sample-user:@git.example.invalid/o/r",
                COLON_BEFORE_AT,
            ),
            (
                "https://sample-user:/sample-password@git.example.invalid",
                COLON_BEFORE_AT,
            ),
            (
                "https://sample-user:12/sample-password@git.example.invalid",
                COLON_BEFORE_AT,
            ),
            (
                "https://alice@git.example.invalid:12/sample-password@gitserver",
                COLON_BEFORE_AT,
            ),
            (
                "ssh://alice@git.example.invalid:12/sample-password@gitserver",
                COLON_BEFORE_AT,
            ),
            (
                "https://sample-user:12/sample-password@1.2.3",
                COLON_BEFORE_AT,
            ),
            (
                "https://sample-user:12/sample-password@127.0.1",
                COLON_BEFORE_AT,
            ),
            (
                "https://git.example.invalid:8443/owner/repo@1.0.0",
                COLON_BEFORE_AT,
            ),
            // Each other guard of the strict reading is the only one that
            // refuses one of these. A user name that holds an `@`:
            (
                "https://sample-user@sample-password@git.example.invalid/o/r",
                UNWRITTEN,
            ),
            // a host outside its set:
            ("https://git_server.example.invalid/o/r", UNWRITTEN),
            // a path that holds a `?`:
            ("https://git.example.invalid/o/r?sample", UNWRITTEN),
            // a ref that holds a `/`:
            ("https://git.example.invalid/o/r@feature/sample", UNWRITTEN),
            // a folder that holds an `@`:
            ("https://git.example.invalid/o/r#kit/sam@ple", UNWRITTEN),
            // a user name gix decodes, so the writer does not write it back:
            (
                "https://sample-user%3Asample-password@git.example.invalid/o/r",
                UNWRITTEN,
            ),
            ("https://git.example.invalid/o%2Fr", UNWRITTEN),
            // a ref that could be a host name, or follows `git@host:path`,
            // but is no version's tag:
            (
                "https://12/sample-password@git.example.invalid",
                REF_NOT_VERSION,
            ),
            (
                "git@sample-user:/sample-password@git.example.invalid",
                REF_NOT_VERSION,
            ),
            (
                "git@git.example.invalid:owner/repo.git@main",
                REF_NOT_VERSION,
            ),
            (
                "git@sample-user:/sample-password@localhost",
                REF_NOT_VERSION,
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

    /// The strict sets refuse an empty part, and a host takes lower-case
    /// letters only; gix lowercases a host before admission sees it.
    #[test]
    fn a_strict_part_is_never_empty_and_a_host_is_lower_case() {
        for allowed in [user_char, host_char, path_char, ref_char, folder_char] {
            assert!(!strictly("", allowed));
            assert!(strictly("a", allowed));
        }
        assert!(!strictly("Git.example.invalid", host_char));
        assert!(strictly("git.example.invalid", host_char));
    }

    /// Every combination of transport, user name, password, host, port, ref
    /// and folder: whatever the rule calls a password, or a `:` before an
    /// `@`, is refused; whatever is admitted carries no password; and a
    /// plain address of each documented form is admitted.
    #[test]
    fn every_address_the_rule_refuses_is_refused_and_every_plain_one_admitted() {
        let (_temp, host, config) = context();
        const MARKER: &str = "sample-marker";
        let users = [None, Some("alice"), Some("al@ice"), Some("al/ice")];
        let passwords = [
            None,
            Some(format!("{MARKER}x")),
            Some(format!("12/{MARKER}")),
            Some(format!("{MARKER}@x")),
            Some(format!("{MARKER}#x")),
        ];
        let hosts = ["gitserver", "git.example.invalid", "1.2.3", "127.0.1"];
        let ports = [None, Some("8443")];
        let refs = [
            None,
            Some("main"),
            Some("1.2.0"),
            Some("v1.2.0"),
            Some("127.0.1"),
        ];
        let folders = [None, Some("kit")];
        let mut cases = 0;
        let mut admitted_plain = 0;
        for transport in ["https", "ssh", "scp", "gh"] {
            for user in users {
                for password in &passwords {
                    for host_name in hosts {
                        for port in ports {
                            for reference in refs {
                                for folder in folders {
                                    let userinfo = match (user, password) {
                                        (None, None) => String::new(),
                                        (Some(user), None) => format!("{user}@"),
                                        (user, Some(password)) => {
                                            format!("{}:{password}@", user.unwrap_or(""))
                                        }
                                    };
                                    let port_part =
                                        port.map(|p| format!(":{p}")).unwrap_or_default();
                                    let tail = format!(
                                        "{}{}",
                                        reference.map(|r| format!("@{r}")).unwrap_or_default(),
                                        folder.map(|f| format!("#{f}")).unwrap_or_default()
                                    );
                                    let address = match transport {
                                        "https" | "ssh" => format!(
                                            "{transport}://{userinfo}{host_name}{port_part}/org/repo{tail}"
                                        ),
                                        "scp" => {
                                            let userinfo = if userinfo.is_empty() {
                                                "git@".to_owned()
                                            } else {
                                                userinfo.clone()
                                            };
                                            format!(
                                                "{userinfo}{host_name}{port_part}:org/repo{tail}"
                                            )
                                        }
                                        _ => format!(
                                            "gh:{userinfo}{host_name}{port_part}/repo{tail}"
                                        ),
                                    };
                                    cases += 1;
                                    let url_form = matches!(transport, "https" | "ssh");
                                    let after_scheme =
                                        address.split_once("://").map_or("", |(_, rest)| rest);
                                    let rule_refuses = url_form
                                        && after_scheme
                                            .find(':')
                                            .is_some_and(|c| after_scheme[c..].contains('@'));
                                    let result = admit(&address, &host, &config);
                                    if password.is_some() || rule_refuses {
                                        let error = result
                                            .as_ref()
                                            .err()
                                            .unwrap_or_else(|| panic!("admitted {address}"));
                                        assert_eq!(error.kind, "invalid-address", "{address}");
                                        assert!(!error.message.contains(MARKER), "{address}");
                                    }
                                    if let Ok(admitted) = &result {
                                        let url = gix::url::parse(gix::bstr::BStr::new(
                                            admitted.repo.as_str(),
                                        ))
                                        .unwrap();
                                        for (what, value) in [
                                            ("source", admitted.source.as_str()),
                                            ("base", admitted.base.as_str()),
                                            ("repository", admitted.repo.as_str()),
                                            ("user", url.user().unwrap_or_default()),
                                        ] {
                                            assert!(
                                                !value.contains(MARKER),
                                                "{address}: {what} {value}"
                                            );
                                        }
                                    }
                                    // A plain address of a documented form:
                                    // no password, a user name of letters,
                                    // a dotted or dotless host, and a ref
                                    // only where the rule and the version
                                    // rule allow one.
                                    let plain_user = match transport {
                                        "https" => user.is_none() || user == Some("alice"),
                                        "ssh" => user.is_none() || user == Some("alice"),
                                        "scp" => user.is_none(),
                                        _ => user.is_none(),
                                    };
                                    let plain_host =
                                        matches!(host_name, "gitserver" | "git.example.invalid")
                                            && (transport != "gh" || host_name == "gitserver");
                                    let ref_fits = match reference {
                                        None => true,
                                        Some(r) if Version::of_tag(r).is_some() => true,
                                        Some(r) => !r.contains('.') && transport != "scp",
                                    };
                                    let plain = password.is_none()
                                        && plain_user
                                        && plain_host
                                        && (port.is_none() || (url_form && reference.is_none()))
                                        && ref_fits;
                                    if plain {
                                        admitted_plain += 1;
                                        assert!(result.is_ok(), "refused {address}: {result:?}");
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        assert_eq!(cases, 4 * 4 * 5 * 4 * 2 * 5 * 2);
        assert!(admitted_plain > 100, "{admitted_plain}");
    }

    #[test]
    fn a_version_is_named_by_its_ref_beside_an_address_without_one() {
        let (_temp, host, config) = context();
        let at = admit_at("gh:owner/repo#kit", "1.2.0", &host, &config).unwrap();
        assert_eq!(at.reference.as_deref(), Some("1.2.0"));
        assert_eq!(at.source, "gh:owner/repo#kit");
        assert_eq!(at.base, "gh:owner/repo#kit");
        assert_eq!(
            at.at("v1.0.0").formal_name(&config.hosts),
            "gh:owner/repo@v1.0.0#kit"
        );
        assert_eq!(
            with_ref("gh:owner/repo#kit", "1.2.0"),
            "gh:owner/repo@1.2.0#kit"
        );
        assert_eq!(without_folder("gh:owner/repo#kit"), "gh:owner/repo");
        // A host with a port takes its version beside the address, which
        // the address alone could not name; a branch or a commit too.
        for (address, reference) in [
            ("https://git.example.invalid:8443/owner/repo", "1.2.0"),
            (
                "ssh://git@git.example.invalid:2222/owner/repo#kit",
                "v1.0.0",
            ),
            ("git@git.example.invalid:owner/repo.git", "main"),
        ] {
            let at = admit_at(address, reference, &host, &config).unwrap();
            assert_eq!(at.reference.as_deref(), Some(reference), "{address}");
            let Address::Git {
                reference: read, ..
            } = &at.address
            else {
                panic!("{address}");
            };
            assert_eq!(read.as_deref(), Some(reference), "{address}");
        }
        let error = admit_at("gh:owner/repo@1.0.0", "1.2.0", &host, &config).unwrap_err();
        assert_eq!(error.message, REF_ON_ADDRESS);
        for reference in [
            "",
            "-x",
            "a b",
            "a@b",
            "a#b",
            "a:b",
            "1.0\n",
            "feature/x",
            "a~b",
        ] {
            let error = admit_at("gh:owner/repo", reference, &host, &config).unwrap_err();
            assert_eq!(
                (error.kind, error.message.as_str()),
                ("invalid-address", NOT_A_REF),
                "{reference:?}"
            );
        }
    }

    /// The read-back takes exactly the admitted repository and folder at
    /// exactly the ref.
    #[test]
    fn the_ref_beside_an_address_reads_back_as_that_repository_folder_and_ref() {
        let git = |repo: &str, reference: Option<&str>, path: Option<&str>| Address::Git {
            repo: repo.to_owned(),
            reference: reference.map(str::to_owned),
            path: path.map(str::to_owned),
        };
        let plain = git("https://git.example.invalid/o/r", None, Some("kit"));
        assert!(reads_at(
            &plain,
            &git(
                "https://git.example.invalid/o/r",
                Some("1.0.0"),
                Some("kit")
            ),
            "1.0.0"
        ));
        for at in [
            git(
                "https://git.example.invalid/o/other",
                Some("1.0.0"),
                Some("kit"),
            ),
            git(
                "https://git.example.invalid/o/r",
                Some("1.0.0"),
                Some("other"),
            ),
            git(
                "https://git.example.invalid/o/r",
                Some("1.0.1"),
                Some("kit"),
            ),
            git("https://git.example.invalid/o/r", None, Some("kit")),
        ] {
            assert!(!reads_at(&plain, &at, "1.0.0"), "{at:?}");
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
