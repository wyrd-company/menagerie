//! Upgrade interviews and merges. Toha's update session holds a Git repository
//! that is not `Send`: each call builds, uses and drops it on the calling thread
//! (the server runs every call on a blocking worker) and returns only owned values.
use crate::Service;
use crate::api::{AnswerRequest, ApplyReply, ApplyRequest, InterviewReply, UpgradeRequest};
use crate::checks::{self, Operation};
use crate::credentials::Ended;
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
/// session. With `reanswer` every question is asked again, with the recorded
/// answers as its defaults.
pub fn start(service: &Service, request: UpgradeRequest) -> Result<InterviewReply, ServiceError> {
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
    let mut prepared = updates::prepare(
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
    if request.reanswer {
        recorded_defaults(&mut prepared);
    }
    let session = UpdateSession::start(
        prepared,
        selected.target.clone(),
        UpdateBase::Update,
        request.reanswer,
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
/// Writes nothing when the caller went away before the first write.
pub fn apply(
    service: &Service,
    request: ApplyRequest,
    ended: &Ended,
) -> Result<ApplyReply, ServiceError> {
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
    ended.check()?;
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
    let mut session = UpdateSession::resume(
        record,
        target.clone(),
        &service.host.paths,
        true,
        environment,
    )?;
    // Toha rebuilds the prepared operation on every resume, so a re-answered
    // session gets its recorded defaults again.
    if session.record.reanswer {
        recorded_defaults(&mut session.prepared);
    }
    Ok(session)
}

/// Make the workspace's recorded answers the defaults of a re-answered
/// upgrade, so its form starts from them and a question left out of a
/// submission keeps its recorded answer rather than the template's default.
/// Toha's own re-answer offers only the template's defaults. A recorded
/// answer takes the place of a configured default. Only an answer whose JSON
/// type fits its question in the new version is used, since Toha fails the
/// interview on a default of the wrong type; documents are read again from
/// the workspace instead. The first recorded value of a question is used,
/// for every pass of a loop.
fn recorded_defaults(prepared: &mut updates::Prepared) {
    let mut kinds = std::collections::HashMap::new();
    question_kinds(&prepared.template.interview, &mut kinds);
    let mut seeded = std::collections::HashSet::new();
    for submission in &prepared.recorded {
        for (id, raw) in submission {
            if seeded.contains(id) {
                continue;
            }
            let Some(kind) = kinds.get(id) else {
                continue;
            };
            if fits(kind, &raw.0) {
                seeded.insert(id.clone());
                prepared.configured_defaults.insert(id.clone(), raw.clone());
            }
        }
    }
}

fn question_kinds<'a>(
    nodes: &'a [toha::template::Node],
    kinds: &mut std::collections::HashMap<toha::template::Id, &'a toha::template::QuestionKind>,
) {
    use toha::template::Node;
    for node in nodes {
        match node {
            Node::Question(question) => {
                kinds.insert(question.id.clone(), &question.kind);
            }
            Node::Group(group) => question_kinds(&group.nodes, kinds),
            _ => {}
        }
    }
}

/// Whether a recorded value has the JSON type Toha parses for the question:
/// a string, a boolean or a list of strings. `null`, the recorded empty
/// answer, fits every question but a document.
fn fits(kind: &toha::template::QuestionKind, value: &serde_json::Value) -> bool {
    use serde_json::Value;
    use toha::template::QuestionKind as K;
    match kind {
        K::Document { .. } => false,
        _ if value.is_null() => true,
        K::Text { .. } | K::Multiline { .. } | K::Git { .. } | K::Select { .. } => {
            value.is_string()
        }
        K::Confirm { .. } => value.is_boolean(),
        K::MultiSelect { .. } | K::TextLoop { .. } => value
            .as_array()
            .is_some_and(|items| items.iter().all(Value::is_string)),
    }
}

#[cfg(test)]
mod tests {
    use super::fits;
    use serde_json::json;
    use toha::template::QuestionKind as K;

    #[test]
    fn only_a_recorded_value_of_the_question_s_json_type_becomes_a_default() {
        let text = K::Text { default: None };
        let flag = K::Confirm { default: None };
        let list = K::TextLoop {
            default: None,
            min: None,
            max: None,
        };
        assert!(fits(&text, &json!("a")));
        assert!(fits(&text, &json!(null)));
        assert!(!fits(&text, &json!(["a"])));
        assert!(fits(&flag, &json!(true)));
        assert!(!fits(&flag, &json!("true")));
        assert!(fits(&list, &json!(["a", "b"])));
        assert!(!fits(&list, &json!(["a", 1])));
        assert!(!fits(&list, &json!("a")));
    }
}
