//! Template addresses the service may read. The workspace service names each
//! address on behalf of a manager's registration or a registered template;
//! this service holds no list of templates, so every route admits the
//! address it is given before any other work: a remote Git address of the
//! forms the user documentation lists, and nothing that would make it read a
//! local folder, a Toha registry entry or a `file://` repository as a
//! template. No refusal quotes the address, so a password given by mistake
//! is shown nowhere. A password can hide from Toha's check, which, as Git and
//! gix do, ends a URL's authority at its first `/`: in
//! `https://user:12/secret@host/repo` the fetcher reads host `user`, port 12
//! and a path that holds the secret. So the address must also read one way to
//! a person who takes everything before an `@` for user information, and the
//! user name the fetcher would send must be the one typed.
use crate::error::ServiceError;
use crate::host::TohaHost;
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
    /// The address as given with its ref removed; an `https` address also
    /// loses its user information.
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
const UNCLEAR: &str = "the address's user information can be read more than one way, as when a password holds a /: give it without a password, with no @ after its host but a version's, and no % in its user name";

fn refused(message: &'static str) -> ServiceError {
    ServiceError::bad_request("invalid-address", message)
}

/// Admit `address`: `gh:` with an owner and a repository, `git@host:path`,
/// `ssh://` or `https://`, with no password and no host or user that starts
/// with `-`. Toha's grammar reads it with the hosts of the service's Toha
/// configuration, and a `gh:` address must still be `https` once Toha
/// expands it. Reaches no network and no file.
pub fn admit(
    address: &str,
    host: &TohaHost,
    config: &toha::config::Config,
) -> Result<Admitted, ServiceError> {
    let shorthand = address.starts_with("gh:");
    let remote = shorthand
        || address.starts_with("https://")
        || address.starts_with("ssh://")
        || address.starts_with("git@");
    if !remote {
        return Err(refused(NOT_REMOTE));
    }
    if source::has_password(address) {
        return Err(refused(PASSWORD));
    }
    let (typed_user, after_host) = typed_parts(address);
    // After the host, an `@` may only start the ref: a second one, or a
    // ref with a `/` or a `:`, can as well end a password that holds a `/`.
    if after_host.matches('@').count() > 1 {
        return Err(refused(UNCLEAR));
    }
    if let Some((_, reference)) = after_host.rsplit_once('@')
        && reference.contains(['/', ':'])
    {
        return Err(refused(UNCLEAR));
    }
    let parsed = source::parse(address, &config.hosts, &host.cwd, &host.paths.home)
        .map_err(|_| refused(NOT_REMOTE))?;
    let Address::Git {
        repo,
        reference,
        path,
    } = &parsed
    else {
        return Err(refused(NOT_REMOTE));
    };
    if source::has_password(repo) {
        return Err(refused(PASSWORD));
    }
    let url =
        gix::url::parse(gix::bstr::BStr::new(repo.as_str())).map_err(|_| refused(NOT_REMOTE))?;
    let https = url.scheme == gix::url::Scheme::Https;
    let ssh = url.scheme == gix::url::Scheme::Ssh;
    if !(https || ssh) || (shorthand && !https) {
        return Err(refused(NOT_REMOTE));
    }
    let host_name = url.host().unwrap_or_default();
    if host_name.is_empty() {
        return Err(refused(NOT_REMOTE));
    }
    if host_name.starts_with('-') || url.user().is_some_and(|user| user.starts_with('-')) {
        return Err(refused(DASHED));
    }
    // The fetcher sends the user name as gix reads it, with `%` escapes
    // decoded, which can hide a `:` and a password: it must be the one typed.
    if !shorthand && url.user() != typed_user {
        return Err(refused(UNCLEAR));
    }
    let (written, written_reference) = without_ref(address);
    // The ref this reading finds is the one Toha's grammar found, so the
    // base names exactly the repository and folder Toha fetches.
    if written_reference.as_deref() != reference.as_deref() {
        return Err(refused(NOT_REMOTE));
    }
    // An `https` base loses the user name the fetcher reads, the one typed.
    let base = match url.user() {
        Some(user) if https && !shorthand => {
            written.replacen(&format!("https://{user}@"), "https://", 1)
        }
        _ => written,
    };
    let source = Address::Git {
        repo: repo.clone(),
        reference: None,
        path: path.clone(),
    }
    .formal_name(&config.hosts);
    Ok(Admitted {
        repo: repo.clone(),
        reference: reference.clone(),
        address: parsed,
        source,
        base,
    })
}

const REF_ON_ADDRESS: &str =
    "a template route names the version by its ref; the address must have none";
const NOT_A_REF: &str = "the ref is not a Git tag, branch or commit name";

/// Admit `address`, which names no ref, at `reference`: the repository and
/// folder of the address, at that ref. The ref must read back as exactly
/// the ref Toha's grammar finds.
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
    let at = admit(&with_ref(address, reference), host, config)?;
    if at.reference.as_deref() != Some(reference) || at.source != plain.source {
        return Err(refused(NOT_A_REF));
    }
    Ok(at)
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

/// The user name typed in `address`, and what follows its host: the path,
/// ref and nothing of the `#` folder of a URL, whose authority ends at the
/// first `/`; the path and ref after `git@host:`; all of a `gh:` address.
fn typed_parts(address: &str) -> (Option<&str>, &str) {
    let address = without_folder(address);
    if let Some(rest) = address.strip_prefix("gh:") {
        return (None, rest);
    }
    if let Some(rest) = address.strip_prefix("git@") {
        return (
            Some("git"),
            rest.split_once(':').map_or("", |(_, path)| path),
        );
    }
    let rest = address.split_once("://").map_or(address, |(_, rest)| rest);
    let (authority, after_host) = rest.split_at(rest.find('/').unwrap_or(rest.len()));
    (authority.rsplit_once('@').map(|(user, _)| user), after_host)
}

/// The address with its ref removed, and that ref, read as Toha's grammar
/// splits a Git address: the `#` folder first, then the last `@` after the
/// authority of a URL, the `:` of `git@host:` or the `gh:` prefix.
fn without_ref(address: &str) -> (String, Option<String>) {
    let (before, folder) = address
        .split_once('#')
        .map_or((address, None), |(a, f)| (a, Some(f)));
    let (prefix, rest) = match address.strip_prefix("gh:") {
        Some(_) => before.split_at(3),
        None => ("", before),
    };
    let start = if rest.starts_with("git@") {
        rest.find(':').map_or(0, |p| p + 1)
    } else {
        rest.find("://").map_or(0, |p| {
            let authority = p + 3;
            rest[authority..]
                .find('/')
                .map_or(rest.len(), |slash| authority + slash + 1)
        })
    };
    let (repository, reference) = match rest[start..].rfind('@') {
        Some(at) => (&rest[..start + at], Some(rest[start + at + 1..].to_owned())),
        None => (rest, None),
    };
    let mut base = format!("{prefix}{repository}");
    if let Some(folder) = folder {
        base.push('#');
        base.push_str(folder);
    }
    (base, reference)
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
            (
                "https://sample-user:sample%2Fpassword@git.example.invalid/o/r",
                PASSWORD,
            ),
            (
                "https://sample-user:sample@password@git.example.invalid/o/r",
                PASSWORD,
            ),
            ("git:/sample-password@git.example.invalid:o/r", NOT_REMOTE),
            // A password that starts with `/`, or with digits and a `/`, ends
            // the authority there for Git, gix and Toha.
            (
                "https://sample-user:/sample-password@git.example.invalid/o/r@1.0.0",
                UNCLEAR,
            ),
            (
                "https://sample-user:/sample-password@git.example.invalid/o/r",
                UNCLEAR,
            ),
            (
                "https://sample-user:12/sample-password@git.example.invalid/o/r@1.0.0",
                UNCLEAR,
            ),
            (
                "https://sample-user:12/sample-password@git.example.invalid/o/r#kit",
                UNCLEAR,
            ),
            (
                "https://x-access-token:1234/sample-password@github.com/o/r@1.0.0",
                UNCLEAR,
            ),
            (
                "https://sample-user:/sample@password@git.example.invalid/o/r",
                UNCLEAR,
            ),
            (
                "ssh://sample-user:/sample-password@git.example.invalid/o/r@1.0.0",
                UNCLEAR,
            ),
            (
                "ssh://sample-user:12/sample-password@git.example.invalid/o/r",
                UNCLEAR,
            ),
            (
                "git@sample-user:/sample-password@git.example.invalid:o/r@1.0.0",
                UNCLEAR,
            ),
            (
                "git@sample-user:12/sample-password@git.example.invalid:o/r",
                UNCLEAR,
            ),
            ("gh:sample-user:/sample-password@o/r@1.0.0", UNCLEAR),
            ("gh:sample-user:12/sample-password@o/r", UNCLEAR),
            ("gh:o/r@feature/sample-password", UNCLEAR),
            // gix decodes a user name's `%` escapes, which can hide a `:`.
            (
                "https://sample-user%3Asample-password@git.example.invalid/o/r",
                UNCLEAR,
            ),
            (
                "ssh://sample-user%3Asample-password@git.example.invalid/o/r@1.0.0",
                UNCLEAR,
            ),
            ("https://sample%2Fpassword@git.example.invalid/o/r", UNCLEAR),
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
