//! Which template revision an approved address admits, and refreshing an
//! installed branch when an operation starts.
//!
//! A tag or commit address fixes one revision: a session on any other
//! revision is refused. A branch address (or an address without a reference)
//! moves: an operation starts on the branch's current commit, and its session
//! keeps that commit until it ends.
use crate::config::ApprovedTemplate;
use crate::error::ServiceError;
use crate::host::TohaHost;
use toha::registry::{Layer, Registry};
use toha::source::{self, Address, RefKind};
use toha::staging::StagedRecord;

/// The revisions an approved address admits.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Admitted {
    /// Exactly this commit; empty for a folder template, which has none.
    Fixed(String),
    /// The commit an operation started on.
    Moving,
}

/// What the approved address admits, from Toha's own address parsing and
/// fetch. A named reference is fetched into a temporary folder to learn
/// whether it is a tag, a commit or a branch; an installed template keeps the
/// commit Toha installed.
pub fn admitted(
    template: &ApprovedTemplate,
    host: &TohaHost,
    config: &toha::config::Config,
    registry: &Registry,
) -> Result<Admitted, ServiceError> {
    let address = source::parse(
        &template.address,
        &config.hosts,
        &host.cwd,
        &host.paths.home,
    )
    .map_err(|e| ServiceError::refused("template-resolve-failed", e.to_string()))?;
    match &address {
        Address::Folder(_) => Ok(Admitted::Fixed(String::new())),
        Address::Git { reference, .. } => {
            let installed = registry
                .entries
                .get(&address.formal_name(&config.hosts))
                .and_then(|listed| listed.entry.commit.clone());
            by_reference(&address, reference.as_deref(), installed, host)
        }
        Address::Name(name) => match registry.resolve(name) {
            Ok(resolved) if is_git(&resolved.entry.source) => {
                let git = Address::Git {
                    repo: resolved.entry.source.clone(),
                    reference: resolved.entry.reference.clone(),
                    path: None,
                };
                by_reference(
                    &git,
                    resolved.entry.reference.as_deref(),
                    resolved.entry.commit.clone(),
                    host,
                )
            }
            _ => {
                let resolved = toha::resolve::resolve_template(
                    name,
                    config,
                    registry,
                    &host.paths,
                    &host.cwd,
                )?;
                Ok(Admitted::Fixed(resolved.commit))
            }
        },
    }
}

fn by_reference(
    address: &Address,
    reference: Option<&str>,
    installed: Option<String>,
    host: &TohaHost,
) -> Result<Admitted, ServiceError> {
    let Some(reference) = reference else {
        return Ok(Admitted::Moving);
    };
    if reference.len() == 40 && reference.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Ok(Admitted::Fixed(reference.to_ascii_lowercase()));
    }
    let scratch = tempfile::tempdir_in(&host.cwd)
        .map_err(|e| ServiceError::new(500, "io-failed", e.to_string()))?;
    let fetched = source::fetch(address, &scratch.path().join("repo"))
        .map_err(|e| ServiceError::refused("template-resolve-failed", e.to_string()))?;
    Ok(match fetched.reference_kind {
        RefKind::Tag | RefKind::Commit => Admitted::Fixed(installed.unwrap_or(fetched.commit)),
        _ => Admitted::Moving,
    })
}

/// The session's revision is one the approved address admits.
pub fn check(record: &StagedRecord, admitted: &Admitted) -> Result<(), ServiceError> {
    match admitted {
        Admitted::Fixed(commit) if &record.commit != commit => Err(ServiceError::conflict(
            "session-mismatch",
            format!(
                "session revision {:?} is not the approved revision {commit:?}",
                record.commit
            ),
        )),
        _ => Ok(()),
    }
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
