//! Checks every route makes before Toha replays or writes anything: the
//! workspace and target, the approved template, and the session's pinned
//! template, target and operation.
use crate::api::Selector;
use crate::config::{ApprovedTemplate, Settings, clean_absolute};
use crate::error::ServiceError;
use crate::host::TohaHost;
use serde_json::Value;
use std::path::{Path, PathBuf};
use toha::staging::{CanonicalTarget, StagedRecord, canonical_target};

/// A request's checked workspace folder and approved template.
pub struct Selected<'a> {
    pub template: &'a ApprovedTemplate,
    /// The workspace folder under the root, as configured.
    pub folder: PathBuf,
    pub target: CanonicalTarget,
}

/// Which flow a session belongs to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Operation {
    Create,
    Upgrade,
}

/// The workspace is a plain folder name, the target is exactly that folder
/// under the workspace root, and the template id is approved. The folder's
/// real path, with symbolic links resolved, must still be that direct child of
/// the root's real path, so a link cannot move a workspace elsewhere.
pub fn select<'a>(
    settings: &'a Settings,
    request: Selector<'_>,
) -> Result<Selected<'a>, ServiceError> {
    let name = request.workspace;
    let plain = !name.is_empty() && name != "." && name != ".." && !name.contains(['/', '\0']);
    if !plain {
        return Err(ServiceError::bad_request(
            "invalid-workspace",
            format!("workspace {name:?} is not a folder name"),
        ));
    }
    let folder = settings.workspace_root.join(name);
    let given = clean_absolute(&request.target.to_string_lossy());
    if given.as_deref() != Some(folder.as_path()) {
        return Err(ServiceError::bad_request(
            "target-mismatch",
            format!(
                "target {} is not workspace {name:?} under the workspace root",
                request.target.display()
            ),
        ));
    }
    let template = settings.template(request.template_id).ok_or_else(|| {
        ServiceError::new(
            404,
            "unknown-template",
            format!("template {:?} is not approved", request.template_id),
        )
    })?;
    let target = contained(&settings.workspace_root, &folder, name)?;
    Ok(Selected {
        template,
        folder,
        target,
    })
}

fn contained(root: &Path, folder: &Path, name: &str) -> Result<CanonicalTarget, ServiceError> {
    let real_root = std::fs::canonicalize(root).map_err(|e| {
        ServiceError::new(
            500,
            "workspace-root-unavailable",
            format!("workspace root {}: {e}", root.display()),
        )
    })?;
    let target = canonical_target(folder).map_err(ServiceError::from)?;
    let path = target.as_path();
    if path.parent() != Some(real_root.as_path()) || path.file_name() != Some(name.as_ref()) {
        return Err(ServiceError::conflict(
            "target-outside-root",
            format!(
                "workspace folder {} resolves to {}, which is not a direct child of the workspace root",
                folder.display(),
                path.display()
            ),
        ));
    }
    Ok(target)
}

/// Toha's staged record from the request's opaque session value.
pub fn session(value: Value) -> Result<StagedRecord, ServiceError> {
    serde_json::from_value(value).map_err(|e| {
        ServiceError::bad_request("malformed-session", format!("invalid session: {e}"))
    })
}

/// The pinned identity an approved template's sessions must carry.
pub struct Identity {
    pub formal_name: String,
    pub named: bool,
}

/// The approved address's formal name, computed without fetching.
pub fn identity(
    template: &ApprovedTemplate,
    host: &TohaHost,
    config: &toha::config::Config,
    registry: &toha::registry::Registry,
) -> Result<Identity, ServiceError> {
    let formal_name =
        toha::resolve::formal_name(&template.address, config, registry, &host.paths, &host.cwd)?;
    let named = matches!(
        toha::source::parse(
            &template.address,
            &config.hosts,
            &host.cwd,
            &host.paths.home
        ),
        Ok(toha::source::Address::Name(_))
    );
    Ok(Identity { formal_name, named })
}

/// The session was produced for this approved template, this target and this flow.
pub fn pinned(
    record: &StagedRecord,
    expected: &Identity,
    target: &CanonicalTarget,
    operation: Operation,
) -> Result<(), ServiceError> {
    if record.template != expected.formal_name || record.named != expected.named {
        return Err(ServiceError::conflict(
            "session-mismatch",
            format!(
                "session template {:?} is not the approved template {:?}",
                record.template, expected.formal_name
            ),
        ));
    }
    record.invocation_context(target).map_err(|_| {
        ServiceError::conflict(
            "session-mismatch",
            format!("session was not started for target {target}"),
        )
    })?;
    let upgrade = match toha::updates::base_of(record) {
        Err(_) if record.base.is_none() => false,
        Ok(toha::updates::UpdateBase::Update) => true,
        _ => {
            return Err(ServiceError::bad_request(
                "malformed-session",
                "session carries an unsupported update base",
            ));
        }
    };
    let wanted = operation == Operation::Upgrade;
    if upgrade != wanted {
        return Err(ServiceError::conflict(
            "session-mismatch",
            if wanted {
                "a create session cannot answer or apply an upgrade"
            } else {
                "an upgrade session cannot answer or generate a create"
            },
        ));
    }
    Ok(())
}

/// The answers envelope, verified against the session's template before any
/// raw answer exists.
pub fn submission(
    record: &StagedRecord,
    submission: &Value,
) -> Result<toha::RawAnswers, ServiceError> {
    Ok(toha::protocol::document_answers(
        &record.template,
        &submission.to_string(),
    )?)
}

/// A create applies only into a folder the workspace service just made: it
/// exists, is a Git repository, and holds nothing but `.git`.
pub fn fresh_repository(folder: &Path, target: &CanonicalTarget) -> Result<(), ServiceError> {
    let entries = std::fs::read_dir(folder).map_err(|e| {
        ServiceError::conflict(
            "target-not-new",
            format!("workspace folder {} is not ready: {e}", folder.display()),
        )
    })?;
    let mut extra = vec![];
    for entry in entries {
        let entry = entry.map_err(|e| ServiceError::new(500, "io-failed", e.to_string()))?;
        if entry.file_name() != ".git" {
            extra.push(entry.file_name().to_string_lossy().into_owned());
        }
    }
    if !extra.is_empty() {
        extra.sort();
        return Err(ServiceError::conflict(
            "target-not-new",
            format!(
                "workspace folder {} already holds files: {}",
                folder.display(),
                extra.join(", ")
            ),
        ));
    }
    match toha::applied::Project::open(target) {
        Ok(Some(_)) => Ok(()),
        Ok(None) => Err(ServiceError::conflict(
            "not-git",
            format!(
                "workspace folder {} has no Git repository; the workspace service initializes it before generation",
                folder.display()
            ),
        )),
        Err(e) => Err(ServiceError::refused("project-invalid", e.to_string())),
    }
}
