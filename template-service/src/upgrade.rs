//! Upgrade interviews and merges. Toha's update session holds a Git repository
//! that is not `Send`: each call builds, uses and drops it on the calling thread
//! (the server runs every call on a blocking worker) and returns only owned values.
use crate::Service;
use crate::api::{AnswerRequest, ApplyReply, ApplyRequest, InterviewReply, StartRequest};
use crate::checks::{self, Operation};
use crate::error::{ServiceError, ValueFree};
use crate::hooks::CallbackRunner;
use crate::host::{self, ProcessEnvironment};
use crate::reply;
use crate::seal::{self, Binding};
use toha::RawAnswers;
use toha::staging::{CanonicalTarget, StagedRecord};
use toha::updates::{self, Environment, PrepareMode, UpdateBase, UpdateSession};

/// Start an upgrade of an existing workspace to the approved template version:
/// replay the workspace's recorded answers and ask only what they do not cover.
/// Current document inputs and defaults are admitted now and kept for the
/// session.
pub fn start(service: &Service, request: StartRequest) -> Result<InterviewReply, ServiceError> {
    let selected = checks::select(&service.settings, request.selector())?;
    if !selected.folder.is_dir() {
        return Err(ServiceError::new(
            404,
            "workspace-not-found",
            format!(
                "workspace folder {} does not exist",
                selected.folder.display()
            ),
        ));
    }
    let (mut config, mut registry, mut warnings) = service.host.context()?;
    if crate::revision::refresh(selected.template, &service.host, &config, &registry)? {
        (config, registry, warnings) = service.host.context()?;
    }
    let mut source = ProcessEnvironment;
    let mut environment = environment(service, &config, &registry, &mut source);
    let prepared = updates::prepare(
        &UpdateBase::Update,
        &selected.template.address,
        &selected.target,
        &service.host.paths,
        true,
        PrepareMode::Stage,
        host::INTERACTIVE,
        &mut environment,
    )?;
    crate::revision::admit(
        selected.template,
        &prepared.resolved.formal_name,
        &prepared.resolved.commit,
        &service.host,
        &config,
        &registry,
    )?;
    warnings.extend(prepared.warnings.iter().cloned());
    let session = UpdateSession::start(
        prepared,
        selected.target.clone(),
        UpdateBase::Update,
        false,
        Default::default(),
        &[],
        &RawAnswers::new(),
    )
    .map_err(|e| ServiceError::from(e).with_warnings(warnings.clone()))?;
    let state = session.state()?;
    let sealed = seal::seal(
        &service.key,
        Binding::new(Operation::Upgrade, &request.workspace, &request.template_id),
        &session.record,
    )?;
    reply::interview(&session.record, sealed, &selected.target, state, warnings)
}

/// Rebuild the upgrade session and submit one batch.
pub fn answer(service: &Service, request: AnswerRequest) -> Result<InterviewReply, ServiceError> {
    let selected = checks::select(&service.settings, request.selector())?;
    let binding = Binding::new(Operation::Upgrade, &request.workspace, &request.template_id);
    let record = seal::open(&service.key, binding, request.session)?;
    let (config, registry, _) = service.host.context()?;
    let expected = checks::identity(selected.template, &service.host, &config, &registry)?;
    checks::pinned(&record, &expected, &selected.target, Operation::Upgrade)?;
    crate::revision::check(
        &record,
        &expected.formal_name,
        selected.template,
        &service.host,
        &config,
        &registry,
    )?;
    let answers = checks::submission(&record, &request.submission)?;
    let mut source = ProcessEnvironment;
    let mut environment = environment(service, &config, &registry, &mut source);
    let mut session = resume(service, record, &selected.target, &mut environment)?;
    let warnings = session.prepared.warnings.clone();
    reply::asking(&session.state()?)?;
    let state = session.answer(answers)?;
    let sealed = seal::seal(&service.key, binding, &session.record)?;
    reply::interview(&session.record, sealed, &selected.target, state, warnings)
}

/// Rebuild a completed upgrade session and merge it into the workspace.
pub fn apply(service: &Service, request: ApplyRequest) -> Result<ApplyReply, ServiceError> {
    let selected = checks::select(&service.settings, request.selector())?;
    let binding = Binding::new(Operation::Upgrade, &request.workspace, &request.template_id);
    let record = seal::open(&service.key, binding, request.session)?;
    let (config, registry, _) = service.host.context()?;
    let expected = checks::identity(selected.template, &service.host, &config, &registry)?;
    checks::pinned(&record, &expected, &selected.target, Operation::Upgrade)?;
    crate::revision::check(
        &record,
        &expected.formal_name,
        selected.template,
        &service.host,
        &config,
        &registry,
    )?;
    let mut source = ProcessEnvironment;
    let mut environment = environment(service, &config, &registry, &mut source);
    // From here Toha renders the template and runs its hooks, so every
    // refusal is value-free.
    let session = resume(service, record, &selected.target, &mut environment)
        .map_err(ValueFree::value_free)?;
    let done = reply::completed(session.state().map_err(ValueFree::value_free)?)?;
    let runner = CallbackRunner {
        socket: &service.settings.hook_runner_socket,
        token: request.hook_token.as_deref(),
    };
    let (merged, report) = session
        .apply(&done, false, &runner, None)
        .map_err(ValueFree::value_free)?;
    Ok(reply::merged(merged, report))
}

fn environment<'a>(
    service: &'a Service,
    config: &'a toha::config::Config,
    registry: &'a toha::registry::Registry,
    source: &'a mut ProcessEnvironment,
) -> Environment<'a, ProcessEnvironment> {
    Environment {
        config,
        registry,
        cwd: &service.host.cwd,
        now: host::now(),
        host: host::host_facts(),
        is_admin: host::is_admin(),
        source,
    }
}

fn resume(
    service: &Service,
    record: StagedRecord,
    target: &CanonicalTarget,
    environment: &mut Environment<'_, ProcessEnvironment>,
) -> Result<UpdateSession, toha::updates::SessionError> {
    UpdateSession::resume(
        record,
        target.clone(),
        &service.host.paths,
        true,
        environment,
    )
}
