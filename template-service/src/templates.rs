//! Reading a template's address and listing its versions, for registration
//! and version checks. Neither runs any of the template's content.
use crate::Service;
use crate::address;
use crate::api::{AddressReply, AddressRequest, VersionsReply};
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
