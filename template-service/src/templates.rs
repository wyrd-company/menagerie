//! Reading a template's address and listing its versions, for registration
//! and version checks. Neither runs any of the template's content.
use crate::Service;
use crate::address;
use crate::api::{AddressReply, AddressRequest, CheckReply, CheckRequest, VersionsReply};
use crate::error::ServiceError;
use crate::server::Call;

/// Read an address with Toha's grammar; no network is reached.
pub fn address(service: &Service, request: AddressRequest) -> Result<AddressReply, ServiceError> {
    let (config, _, _) = service.host.context()?;
    let admitted = address::admit(&request.address, &service.host, &config)?;
    Ok(AddressReply {
        source: admitted.source,
        reference: admitted.reference,
        base: admitted.base,
    })
}

/// List the versions of the repository an address without a ref names,
/// through the transport and credentials a fetch of that address uses.
pub fn versions(
    service: &Service,
    request: AddressRequest,
    call: &Call,
) -> Result<VersionsReply, ServiceError> {
    let (config, _, _) = service.host.context()?;
    let admitted = address::admit(&request.address, &service.host, &config)?;
    if admitted.reference.is_some() {
        return Err(ServiceError::bad_request(
            "invalid-address",
            "the versions route takes an address without a ref",
        ));
    }
    let tags = crate::versions::list_tags(
        &admitted.repo,
        &service.settings.hook_runner_socket,
        &call.ended,
        &call.handed,
    )?;
    Ok(VersionsReply {
        source: admitted.source,
        versions: crate::versions::versions(tags),
    })
}

/// The file a template's content folder must hold for Menagerie to start
/// what it generates.
pub const DEFINITION: &str = ".devcontainer/devcontainer.json";

/// Fetch one version of a template into Toha's cache and read it, as a
/// create would before its interview, without running any of its hooks or
/// commands: its folder and links stay inside the checkout, Toha loads its
/// manifest, and its content folder holds the definition or not. With
/// `since`, it also answers whether the fetched commit is that commit or
/// descends from it in the repository's history.
pub fn check(service: &Service, request: CheckRequest) -> Result<CheckReply, ServiceError> {
    if let Some(since) = &request.since
        && !crate::revision::is_commit(since)
    {
        return Err(ServiceError::bad_request(
            "malformed-request",
            "since must be a full Git object id",
        ));
    }
    let (config, registry, _) = service.host.context()?;
    let at = address::admit_at(&request.address, &request.reference, &service.host, &config)?;
    let toha::source::Address::Git { path, .. } = &at.address else {
        return Err(ServiceError::internal(
            "an admitted address is a Git address",
        ));
    };
    // Toha resolves the repository's root at the ref, as written, so an SSH
    // address keeps its user name, and this service, not Toha, judges where
    // the address's folder leads.
    let root = address::with_ref(
        address::without_folder(&request.address),
        &request.reference,
    );
    let resolved = toha::resolve::resolve_template(
        &root,
        &config,
        &registry,
        &service.host.paths,
        &service.host.cwd,
    )?;
    let folder = crate::links::checked_folder(&resolved.folder, path.as_deref())?;
    let template = toha::Template::load(&folder)?;
    let definition = template.source_dir.join(DEFINITION).is_file();
    let descends = match &request.since {
        Some(since) => Some(descends(&resolved.folder, &resolved.commit, since)?),
        None => None,
    };
    Ok(CheckReply {
        source: at.source,
        commit: resolved.commit,
        definition,
        descends,
    })
}

/// Whether `commit` is `since` or descends from it, read from the checkout
/// Toha fetched, which holds the whole history of the repository's
/// branches and tags as fetched: a commit that is not among `commit`'s
/// ancestors there is not one of them.
fn descends(checkout: &std::path::Path, commit: &str, since: &str) -> Result<bool, ServiceError> {
    let failed = |e: &dyn std::fmt::Display| {
        ServiceError::internal(format!("reading the fetched history failed: {e}"))
    };
    let repo = gix::open_opts(checkout, gix::open::Options::isolated()).map_err(|e| failed(&e))?;
    let tip = gix::ObjectId::from_hex(commit.as_bytes()).map_err(|e| failed(&e))?;
    let since = gix::ObjectId::from_hex(since.as_bytes()).map_err(|e| failed(&e))?;
    // The walk yields the tip itself first, then each of its ancestors.
    for ancestor in repo.rev_walk([tip]).all().map_err(|e| failed(&e))? {
        if ancestor.map_err(|e| failed(&e))?.id == since {
            return Ok(true);
        }
    }
    Ok(false)
}
