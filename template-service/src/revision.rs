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
/// installation has moved on.
pub fn admit(
    formal: &str,
    commit: &str,
    host: &TohaHost,
    config: &toha::config::Config,
) -> Result<(), ServiceError> {
    let Some(repo) = git_source(formal, host, config) else {
        return Ok(());
    };
    if authentic(formal, commit, &repo, host) {
        return Ok(());
    }
    let resolved = toha::resolve::resume_template(
        formal,
        commit,
        false,
        config,
        &Registry::default(),
        &host.paths,
        &host.cwd,
    )?;
    if resolved.formal_name != formal
        || resolved.commit != commit
        || !authentic(formal, commit, &repo, host)
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

/// The session's revision is one a start of the approved template admitted.
/// When the cache holds no admission at all for the approved template, as
/// after the cache was cleared, the session is admitted only when the
/// approved address resolves to exactly its commit now; nothing is refreshed.
pub fn check(
    record: &StagedRecord,
    formal: &str,
    template: &ApprovedTemplate,
    host: &TohaHost,
    config: &toha::config::Config,
    registry: &Registry,
) -> Result<(), ServiceError> {
    let admitted = match source::parse(formal, &config.hosts, &host.cwd, &host.paths.home) {
        Ok(Address::Folder(_)) => record.commit.is_empty(),
        Ok(Address::Git { repo, .. }) => {
            is_commit(&record.commit)
                && (authentic(formal, &record.commit, &repo, host)
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

/// The source repository of a Git formal name, as Toha's resume parses it.
fn git_source(formal: &str, host: &TohaHost, config: &toha::config::Config) -> Option<String> {
    match source::parse(formal, &config.hosts, &host.cwd, &host.paths.home) {
        Ok(Address::Git { repo, .. }) => Some(repo),
        _ => None,
    }
}

/// A full Git object id in Toha's spelling (lower-case SHA-1 or SHA-256).
fn is_commit(commit: &str) -> bool {
    matches!(commit.len(), 40 | 64)
        && commit
            .bytes()
            .all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

/// Toha's cache entry for `formal` at `commit` is a checkout whose HEAD is
/// that commit and whose origin is `repo`. The entry path is the one Toha's
/// resume reads and must resolve inside the service's cache.
fn authentic(formal: &str, commit: &str, repo: &str, host: &TohaHost) -> bool {
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
    head.as_deref() == Some(commit) && origin.as_deref() == Some(repo)
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
