//! Create interviews and workspace generation. Every call rebuilds the
//! interview from the request's session; nothing is retained between calls.
use crate::Service;
use crate::api::{AnswerRequest, ApplyReply, ApplyRequest, InterviewReply, Outcome, StartRequest};
use crate::checks::{self, Operation};
use crate::error::ServiceError;
use crate::host::{self, ProcessEnvironment};
use crate::reply;
use toha::context::{EnvironmentDecision, ExecutionFacts};
use toha::operations::{Preparation, Session};
use toha::{ApplyOptions, RawAnswers, Template};

/// Start the interview of an approved template for a workspace folder that does
/// not exist yet.
pub fn start(service: &Service, request: StartRequest) -> Result<InterviewReply, ServiceError> {
    let selected = checks::select(&service.settings, request.selector())?;
    if std::fs::symlink_metadata(&selected.folder).is_ok() {
        return Err(ServiceError::conflict(
            "workspace-exists",
            format!(
                "workspace folder {} already exists",
                selected.folder.display()
            ),
        ));
    }
    let (mut config, mut registry, mut warnings) = service.host.context()?;
    if crate::revision::refresh(selected.template, &service.host, &config, &registry)? {
        (config, registry, warnings) = service.host.context()?;
    }
    let resolved = toha::resolve::resolve_template(
        &selected.template.address,
        &config,
        &registry,
        &service.host.paths,
        &service.host.cwd,
    )?;
    let template = Template::load(&resolved.folder)
        .map_err(|e| ServiceError::refused("template-invalid", e.to_string()))?;
    let preparation = Preparation::new(
        template,
        resolved,
        selected.target.clone(),
        &[],
        &RawAnswers::new(),
    )?;
    let context = preparation.context(
        EnvironmentDecision::CarryStageGrant,
        host::host_facts(),
        ExecutionFacts::new(host::is_admin(), host::INTERACTIVE),
        &mut ProcessEnvironment,
    )?;
    let session = preparation.start(
        context,
        host::now(),
        config,
        Default::default(),
        &RawAnswers::new(),
        None,
    )?;
    warnings.extend(session.warnings()?);
    let state = session.state()?;
    reply::interview(&session.record, &selected.target, state, warnings)
}

/// Rebuild the session and submit one batch. A rejected batch returns the same
/// questions with their errors and an unchanged session.
pub fn answer(service: &Service, request: AnswerRequest) -> Result<InterviewReply, ServiceError> {
    let selected = checks::select(&service.settings, request.selector())?;
    let record = checks::session(request.session)?;
    let (config, registry, warnings) = service.host.context()?;
    let expected = checks::identity(selected.template, &service.host, &config, &registry)?;
    checks::pinned(&record, &expected, &selected.target, Operation::Create)?;
    crate::revision::check(
        &record,
        &crate::revision::admitted(selected.template, &service.host, &config, &registry)?,
    )?;
    let answers = checks::submission(&record, &request.submission)?;
    let mut session = resume(service, record, &selected.target, config, &registry)?;
    reply::asking(&session.state()?)?;
    let state = session.answer(answers)?;
    reply::interview(&session.record, &selected.target, state, warnings)
}

/// Rebuild a completed session and apply it into the folder the workspace
/// service created and initialized. Complete only when the applied record was
/// written.
pub fn generate(service: &Service, request: ApplyRequest) -> Result<ApplyReply, ServiceError> {
    let selected = checks::select(&service.settings, request.selector())?;
    let record = checks::session(request.session)?;
    let (config, registry, _) = service.host.context()?;
    let expected = checks::identity(selected.template, &service.host, &config, &registry)?;
    checks::pinned(&record, &expected, &selected.target, Operation::Create)?;
    crate::revision::check(
        &record,
        &crate::revision::admitted(selected.template, &service.host, &config, &registry)?,
    )?;
    checks::fresh_repository(&selected.folder, &selected.target)?;
    let session = resume(service, record, &selected.target, config, &registry)?;
    let done = reply::completed(session.state()?)?;
    let plan = session.plan(&done)?;
    let actions = toha::operations::classify_paths(&plan, &selected.target, false)?;
    let messages = done.messages.clone();
    if toha::updates::effective_dry_run(false, &done) {
        return Ok(ApplyReply::without_writes(
            Outcome::Planned,
            reply::planned_files(actions),
            messages,
        ));
    }
    let applied = session.apply_plan(
        plan,
        ApplyOptions {
            force: false,
            trusted: true,
        },
        &toha::hook::ProcessRunner,
        None,
        &mut |_| {},
    )?;
    reply::generated(applied, &actions, messages)
}

fn resume(
    service: &Service,
    record: toha::staging::StagedRecord,
    target: &toha::staging::CanonicalTarget,
    config: toha::config::Config,
    registry: &toha::registry::Registry,
) -> Result<Session, ServiceError> {
    Ok(Session::resume(
        record,
        target.clone(),
        &service.host.paths,
        config,
        registry,
        &service.host.cwd,
    )?)
}
