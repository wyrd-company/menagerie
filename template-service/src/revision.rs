//! Which template revision a session may use, and refreshing an installed
//! branch when an operation starts.
//!
//! A start admits the revision it resolves. The admission is recorded in
//! Toha's source cache in the service's data folder, the same cache Toha
//! replays a resumed revision from: an entry for the approved template's
//! formal name holding a checkout of exactly that commit from the approved
//! source. A resumed session may use only an admitted revision, so editing a
//! session cannot select a revision no start of that approved template
//! admitted, and an admitted revision replays without reaching its source.
use crate::config::ApprovedTemplate;
use crate::error::ServiceError;
use crate::host::TohaHost;
use std::path::PathBuf;
use toha::registry::{Layer, Registry};
use toha::source::{self, Address};
use toha::staging::StagedRecord;

/// Record that a start admitted `commit` of the approved template `formal`.
/// A Git template resolved from Toha's cache is already recorded; one
/// resolved from an installation is materialized into the cache through
/// Toha's resume with no registry, the way Toha replays it once the
/// installation has moved on. When Toha cannot fetch it that way, the
/// installation itself admits the commit while it holds it, as Toha's resume
/// replays from it.
pub fn admit(
    template: &ApprovedTemplate,
    formal: &str,
    commit: &str,
    host: &TohaHost,
    config: &toha::config::Config,
    registry: &Registry,
) -> Result<(), ServiceError> {
    let sources = sources(template, formal, host, config);
    if sources.is_empty() || authentic(formal, commit, &sources, host) {
        return Ok(());
    }
    let resolved = match toha::resolve::resume_template(
        formal,
        commit,
        false,
        config,
        &Registry::default(),
        &host.paths,
        &host.cwd,
    ) {
        Ok(resolved) => resolved,
        Err(_) if installed(formal, commit, registry) => return Ok(()),
        Err(error) => return Err(error.into()),
    };
    if resolved.formal_name != formal
        || resolved.commit != commit
        || !authentic(formal, commit, &sources, host)
    {
        return Err(ServiceError::conflict(
            "template-cache-conflict",
            format!(
                "Toha's cache entry for {formal} at {commit} does not hold that commit from that source"
            ),
        ));
    }
    Ok(())
}

/// The session's revision is one a start of the approved template admitted:
/// its authentic cache entry, or the installation that holds it. When the
/// cache holds no admission at all for the approved template, as after the
/// cache was cleared, the session is admitted only when the approved address
/// resolves to exactly its commit now; nothing is refreshed.
pub fn check(
    record: &StagedRecord,
    formal: &str,
    template: &ApprovedTemplate,
    host: &TohaHost,
    config: &toha::config::Config,
    registry: &Registry,
) -> Result<(), ServiceError> {
    let sources = sources(template, formal, host, config);
    let admitted = match source::parse(formal, &config.hosts, &host.cwd, &host.paths.home) {
        Ok(Address::Folder(_)) => record.commit.is_empty(),
        Ok(Address::Git { .. }) => {
            is_commit(&record.commit)
                && (authentic(formal, &record.commit, &sources, host)
                    || installed(formal, &record.commit, registry)
                    || uncached(formal, host)
                        && toha::resolve::resolve_template(
                            &template.address,
                            config,
                            registry,
                            &host.paths,
                            &host.cwd,
                        )?
                        .commit
                            == record.commit)
        }
        _ => {
            toha::resolve::resolve_template(formal, config, registry, &host.paths, &host.cwd)?
                .commit
                == record.commit
        }
    };
    if admitted {
        Ok(())
    } else {
        Err(ServiceError::conflict(
            "session-mismatch",
            format!(
                "session revision {:?} was not admitted for the approved template {formal:?}",
                record.commit
            ),
        ))
    }
}

/// The repositories Toha fetches the approved template from, as written by
/// `toha::source::parse`: the approved address, which a start fetches, and
/// the template's formal name, which Toha's resume fetches. Toha's formal
/// name drops a `.git` suffix and maps a known host to its shorthand, so the
/// two can differ for one template. Empty for a template that is not Git.
fn sources(
    template: &ApprovedTemplate,
    formal: &str,
    host: &TohaHost,
    config: &toha::config::Config,
) -> Vec<String> {
    let mut sources = Vec::new();
    for address in [template.address.as_str(), formal] {
        if let Ok(Address::Git { repo, .. }) =
            source::parse(address, &config.hosts, &host.cwd, &host.paths.home)
            && !sources.contains(&repo)
        {
            sources.push(repo);
        }
    }
    sources
}

/// Toha's installation of `formal` holds exactly `commit`, so Toha's resume
/// replays it from the installation.
fn installed(formal: &str, commit: &str, registry: &Registry) -> bool {
    registry
        .entries
        .get(formal)
        .and_then(|listed| listed.entry.commit.as_deref())
        == Some(commit)
}

/// A full Git object id in Toha's spelling (lower-case SHA-1 or SHA-256).
fn is_commit(commit: &str) -> bool {
    matches!(commit.len(), 40 | 64)
        && commit
            .bytes()
            .all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

/// Toha's cache entry for `formal` at `commit` is a checkout whose HEAD is
/// that commit and whose origin is one of `sources`, compared exactly. The
/// entry path is the one Toha's resume reads and must resolve inside the
/// service's cache.
fn authentic(formal: &str, commit: &str, sources: &[String], host: &TohaHost) -> bool {
    let Some(root) = cache_entry(formal, commit, host) else {
        return false;
    };
    let Ok(checkout) = gix::open_opts(&root, gix::open::Options::isolated()) else {
        return false;
    };
    let head = checkout.head_id().map(|id| id.to_string()).ok();
    let origin = checkout.find_remote("origin").ok().and_then(|remote| {
        remote
            .url(gix::remote::Direction::Fetch)
            .map(|url| url.to_bstring().to_string())
    });
    head.as_deref() == Some(commit) && origin.is_some_and(|origin| sources.contains(&origin))
}

/// No admission of the approved template is cached: its cache key holds no entry.
fn uncached(formal: &str, host: &TohaHost) -> bool {
    let key = host
        .paths
        .cache
        .join("sources")
        .join(source::install_key(formal));
    std::fs::read_dir(key).map_or(true, |mut entries| entries.next().is_none())
}

fn cache_entry(formal: &str, commit: &str, host: &TohaHost) -> Option<PathBuf> {
    if !is_commit(commit) {
        return None;
    }
    let key = source::install_key(formal);
    if key.is_empty() || key == "." || key == ".." {
        return None;
    }
    let sources = host.paths.cache.join("sources").canonicalize().ok()?;
    let root = sources.join(key).join(commit).canonicalize().ok()?;
    root.starts_with(&sources).then_some(root)
}

/// Refresh the approved template's installation when an operation starts, as
/// Toha's catalog update does: an installed branch moves to its current
/// commit, an installed tag or commit stays. Only a Git template installed in
/// the user registry layer, which Toha's catalog updates, is refreshed.
/// Returns whether the registry may have changed.
pub fn refresh(
    template: &ApprovedTemplate,
    host: &TohaHost,
    config: &toha::config::Config,
    registry: &Registry,
) -> Result<bool, ServiceError> {
    let formal =
        toha::resolve::formal_name(&template.address, config, registry, &host.paths, &host.cwd)?;
    let Some(listed) = registry.entries.get(&formal) else {
        return Ok(false);
    };
    if listed.layer != Layer::User || !is_git(&listed.entry.source) {
        return Ok(false);
    }
    let failed = |e: toha::catalog::CatalogError| {
        ServiceError::refused("template-refresh-failed", e.to_string())
    };
    let catalog = toha::catalog::Catalog::load(host.paths.clone(), &host.cwd).map_err(failed)?;
    toha::catalog::update(&catalog, Some(formal)).map_err(failed)?;
    Ok(true)
}

/// Toha's test for a Git source in its catalog update.
fn is_git(source: &str) -> bool {
    source.contains("://") || source.starts_with("git@")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::Path;
    use std::process::Command;

    fn git(dir: &Path, args: &[&str]) -> String {
        let output = Command::new("git")
            .arg("-C")
            .arg(dir)
            .args(args)
            .env("GIT_AUTHOR_NAME", "Example User")
            .env("GIT_AUTHOR_EMAIL", "user@example.invalid")
            .env("GIT_COMMITTER_NAME", "Example User")
            .env("GIT_COMMITTER_EMAIL", "user@example.invalid")
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "git {args:?}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        String::from_utf8(output.stdout).unwrap().trim().to_owned()
    }

    fn approved(address: &str) -> ApprovedTemplate {
        ApprovedTemplate {
            id: "sample".into(),
            name: "Sample".into(),
            address: address.into(),
        }
    }

    /// A cache entry for each address's formal name whose origin is set to
    /// each candidate; only the repositories Toha fetches from match, exactly.
    #[test]
    fn cached_origins_are_the_sources_toha_fetches_from() {
        let temp = tempfile::tempdir().unwrap();
        let root = temp.path().canonicalize().unwrap();
        let source = root.join("source");
        std::fs::create_dir_all(&source).unwrap();
        git(&source, &["init", "-q", "-b", "main"]);
        std::fs::write(source.join("file.txt"), "content\n").unwrap();
        git(&source, &["add", "."]);
        git(&source, &["commit", "-q", "-m", "sample"]);
        let commit = git(&source, &["rev-parse", "HEAD"]);
        let host = TohaHost::new(&root.join("data"));
        host.prepare().unwrap();
        let (config, registry, _) = host.context().unwrap();
        for (address, accepted, refused) in [
            (
                "https://git.example.invalid/o/r.git@v1",
                &[
                    "https://git.example.invalid/o/r.git",
                    "https://git.example.invalid/o/r",
                ][..],
                &[
                    "https://git.example.invalid/o/other.git",
                    "https://other.example.invalid/o/r.git",
                    "https://git.example.invalid/fork/r.git",
                    "http://git.example.invalid/o/r.git",
                ][..],
            ),
            (
                "git@github.com:o/r.git@v1",
                &["git@github.com:o/r.git", "https://github.com/o/r"][..],
                &[
                    "git@github.com:o/other.git",
                    "git@gitlab.com:o/r.git",
                    "git@github.com:fork/r.git",
                    "ssh://git@github.com/o/r.git",
                    "https://github.com/o/r.git",
                ][..],
            ),
            (
                "gh:o/r@v1",
                &["https://github.com/o/r"][..],
                &["git@github.com:o/r.git", "https://gitlab.com/o/r"][..],
            ),
        ] {
            let template = approved(address);
            let formal =
                toha::resolve::formal_name(address, &config, &registry, &host.paths, &host.cwd)
                    .unwrap();
            let sources = sources(&template, &formal, &host, &config);
            assert_eq!(sources, accepted, "{address}");
            let entry = host
                .paths
                .cache
                .join("sources")
                .join(source::install_key(&formal))
                .join(&commit);
            // The SSH address formalizes to the `gh` shorthand, so two cases share an entry.
            if !entry.exists() {
                std::fs::create_dir_all(entry.parent().unwrap()).unwrap();
                git(
                    &root,
                    &[
                        "clone",
                        "-q",
                        source.to_str().unwrap(),
                        entry.to_str().unwrap(),
                    ],
                );
            }
            for origin in accepted {
                git(&entry, &["remote", "set-url", "origin", origin]);
                assert!(
                    authentic(&formal, &commit, &sources, &host),
                    "{address}: {origin} refused"
                );
            }
            for origin in refused {
                git(&entry, &["remote", "set-url", "origin", origin]);
                assert!(
                    !authentic(&formal, &commit, &sources, &host),
                    "{address}: {origin} accepted"
                );
            }
        }
    }
}
