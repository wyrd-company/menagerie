//! Structured service errors and their mapping from Toha's typed results.
use serde::Serialize;

/// One refused request: a stable `kind`, its HTTP status and a message.
/// Library errors keep their typed distinction in `kind`; the message is the
/// library's own text.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ServiceError {
    pub status: u16,
    pub kind: &'static str,
    pub message: String,
    /// Paths named by a conflict, drift or occupied-path refusal.
    pub paths: Vec<String>,
    /// JSON Pointers into the request of a refused value.
    pub locations: Vec<String>,
    /// Question ids a refused value answers.
    pub questions: Vec<String>,
    /// Service warnings gathered before the refusal.
    pub warnings: Vec<String>,
}

#[derive(Serialize)]
struct Envelope<'a> {
    error: Body<'a>,
}

#[derive(Serialize)]
struct Body<'a> {
    kind: &'a str,
    message: &'a str,
    #[serde(skip_serializing_if = "<[String]>::is_empty")]
    paths: &'a [String],
    #[serde(skip_serializing_if = "<[String]>::is_empty")]
    locations: &'a [String],
    #[serde(skip_serializing_if = "<[String]>::is_empty")]
    questions: &'a [String],
    #[serde(skip_serializing_if = "<[String]>::is_empty")]
    warnings: &'a [String],
}

impl ServiceError {
    pub fn new(status: u16, kind: &'static str, message: impl Into<String>) -> Self {
        Self {
            status,
            kind,
            message: message.into(),
            paths: vec![],
            locations: vec![],
            questions: vec![],
            warnings: vec![],
        }
    }
    pub fn bad_request(kind: &'static str, message: impl Into<String>) -> Self {
        Self::new(400, kind, message)
    }
    pub fn conflict(kind: &'static str, message: impl Into<String>) -> Self {
        Self::new(409, kind, message)
    }
    pub fn refused(kind: &'static str, message: impl Into<String>) -> Self {
        Self::new(422, kind, message)
    }
    pub fn internal(message: impl Into<String>) -> Self {
        Self::new(500, "internal-error", message)
    }
    fn with_paths(mut self, paths: Vec<String>) -> Self {
        self.paths = paths;
        self
    }
    pub fn with_warnings(mut self, mut warnings: Vec<String>) -> Self {
        warnings.append(&mut self.warnings);
        self.warnings = warnings;
        self
    }
    /// The JSON body `{"error":{"kind","message","paths"?,"locations"?,"questions"?,"warnings"?}}`.
    pub fn body(&self) -> Vec<u8> {
        serde_json::to_vec(&Envelope {
            error: Body {
                kind: self.kind,
                message: &self.message,
                paths: &self.paths,
                locations: &self.locations,
                questions: &self.questions,
                warnings: &self.warnings,
            },
        })
        .expect("error body serializes")
    }
}

/// A Toha error as a refusal whose message the service chose for its variant.
/// The message and paths carry no value Toha rendered: no answer, no hook's
/// captured output, no rendered argument, path or working directory. Generate
/// and apply return only these, because a hook's captured output can reach
/// any value rendered after the hook ran.
pub trait ValueFree {
    fn value_free(self) -> ServiceError;
}

impl ServiceError {
    fn with_message(mut self, message: String) -> Self {
        self.message = message;
        self
    }
}

impl ValueFree for toha::resolve::ResolveError {
    fn value_free(self) -> ServiceError {
        use toha::resolve::ResolveError as E;
        match self {
            E::NotFound(_) => {
                ServiceError::refused("template-not-found", "the template was not found")
            }
            E::StaleRevision { .. } => ServiceError::refused(
                "template-stale-revision",
                "the template's approved revision is stale",
            ),
            E::Ambiguous { .. } => {
                ServiceError::refused("template-ambiguous", "the template's name is ambiguous")
            }
            _ => ServiceError::refused(
                "template-resolve-failed",
                "the template could not be resolved",
            ),
        }
    }
}

impl From<toha::resolve::ResolveError> for ServiceError {
    fn from(error: toha::resolve::ResolveError) -> Self {
        use toha::resolve::ResolveError as E;
        let message = match &error {
            E::Error(m) | E::NotFound(m) => m.clone(),
            E::StaleRevision { message } => message.clone(),
            E::Ambiguous { name, matches } => {
                format!("template {name:?} is ambiguous: {}", matches.join(", "))
            }
            other => format!("{other:?}"),
        };
        error.value_free().with_message(message)
    }
}

/// Every apply error is value-free: Toha builds them while it renders the
/// template and runs its hooks. The match has no wildcard, so a new variant
/// fails to compile until the service chooses its message.
impl From<toha::ApplyError> for ServiceError {
    fn from(error: toha::ApplyError) -> Self {
        use toha::ApplyError as E;
        match error {
            // Conflicts and drift are found from the plan before any hook
            // runs, so their paths carry no captured output.
            E::Conflicts(paths) => Self::conflict(
                "target-conflict",
                "the template would write files that already exist",
            )
            .with_paths(paths.iter().map(ToString::to_string).collect()),
            E::Drift(regions) => Self::conflict(
                "target-drift",
                "regions the template owns were edited in the workspace",
            )
            .with_paths(regions.iter().map(|(p, _)| p.to_string()).collect()),
            // A deferred hook's working directory is checked after earlier
            // hooks ran, so the path stays out.
            E::Symlink(_) => Self::conflict(
                "target-symlink",
                "a path the template writes or runs a hook in contains a symbolic link",
            ),
            E::Hook {
                index,
                hook,
                outcome,
            } => Self::refused("hook-failed", hook_failure(index, &hook, &outcome)),
            E::HookOutput { index, id, fault } => Self::refused(
                "hook-failed",
                format!("hook {index} `{id}` output: {}", output_fault(&fault)),
            ),
            E::HookIo {
                index,
                hook,
                source,
            } => Self::new(
                500,
                "hook-io-failed",
                format!(
                    "{} could not run: {}",
                    hook_name(index, &hook),
                    crate::hooks::reason(&source)
                ),
            ),
            E::Deferred(_) => Self::refused(
                "apply-failed",
                "a hook or message that reads an earlier hook's result could not be rendered",
            ),
            E::Region(_) => Self::refused("apply-failed", "a region edit could not be resolved"),
            E::Json(_) => Self::refused("apply-failed", "a JSON edit could not be resolved"),
            E::Git(_) => Self::refused(
                "apply-failed",
                "the workspace's repository could not be set up",
            ),
            E::Io { .. } => Self::new(500, "io-failed", "a workspace file could not be written"),
            E::Record(_) => Self::new(
                500,
                "record-failed",
                "the applied record could not be captured or written",
            ),
        }
    }
}

/// A hook named by its position and its template id. Toha's own message
/// prints the planned hook, whose rendered arguments can carry an earlier
/// hook's captured output, so the service never uses it.
fn hook_name(index: usize, hook: &toha::plan::PlannedHook) -> String {
    match &hook.id {
        Some(id) => format!("hook {index} `{id}`"),
        None => format!("hook {index}"),
    }
}

fn hook_failure(
    index: usize,
    hook: &toha::plan::PlannedHook,
    outcome: &toha::hook::HookOutcome,
) -> String {
    match outcome.code {
        Some(code) => format!("{} failed with exit code {code}", hook_name(index, hook)),
        None => format!("{} failed without an exit code", hook_name(index, hook)),
    }
}

/// A captured stream's fault, by position only.
fn output_fault(fault: &toha::apply::OutputFault) -> String {
    use toha::apply::OutputFault as F;
    match fault {
        F::NotUtf8 { valid_up_to } => {
            format!("the captured stream is not UTF-8 (valid up to byte {valid_up_to})")
        }
        F::Empty => "parse: json but the captured stdout was empty".to_owned(),
        F::NotJson { line, column } => {
            format!(
                "parse: json but the captured stdout is not JSON at line {line} column {column}"
            )
        }
    }
}

impl ValueFree for toha::operations::OperationError {
    fn value_free(self) -> ServiceError {
        use toha::operations::OperationError as E;
        match self {
            E::Resolve(e) => e.value_free(),
            E::Apply(e) => (*e).into(),
            E::Staging(e) => e.value_free(),
            E::Load(_) => ServiceError::refused("template-invalid", "the template is invalid"),
            E::Requirements(_) => ServiceError::refused(
                "missing-requirements",
                "programs the template requires are missing",
            ),
            E::Document(_) => ServiceError::refused(
                "document-invalid",
                "the template's documents could not be prepared",
            ),
            E::Parts(_) => {
                ServiceError::refused("parts-invalid", "the template's parts are invalid")
            }
            E::Context(_) => ServiceError::refused(
                "context-invalid",
                "the template's context could not be built",
            ),
            E::Evaluation(_) => {
                ServiceError::refused("evaluation-failed", "the template could not be evaluated")
            }
            E::Plan(_) => ServiceError::refused(
                "plan-failed",
                "the template's files and hooks could not be planned",
            ),
            E::Record(_) => {
                ServiceError::refused("record-invalid", "the applied record is invalid")
            }
            E::Seed(_) => {
                ServiceError::bad_request("malformed-session", "the session is malformed")
            }
            E::Input(_) => {
                ServiceError::conflict("session-mismatch", "the session does not match the request")
            }
            _ => ServiceError::refused("operation-refused", "the template operation was refused"),
        }
    }
}

impl From<toha::operations::OperationError> for ServiceError {
    fn from(error: toha::operations::OperationError) -> Self {
        use toha::operations::OperationError as E;
        match error {
            E::Resolve(e) => e.into(),
            E::Apply(e) => (*e).into(),
            E::Staging(e) => e.into(),
            other => {
                let message = other.to_string();
                other.value_free().with_message(message)
            }
        }
    }
}

impl ValueFree for toha::staging::StagingError {
    fn value_free(self) -> ServiceError {
        use toha::staging::StagingError as E;
        match self {
            E::EnvironmentTrustRequired { .. } | E::EnvironmentAdmission { .. } => {
                ServiceError::refused(
                    "environment-refused",
                    "the template's environment access was refused",
                )
            }
            E::Io(_) => ServiceError::new(500, "io-failed", "the session could not be staged"),
            E::Json(_) | E::Replay(_) => {
                ServiceError::bad_request("malformed-session", "the session is malformed")
            }
        }
    }
}

impl From<toha::staging::StagingError> for ServiceError {
    fn from(error: toha::staging::StagingError) -> Self {
        let message = error.to_string();
        error.value_free().with_message(message)
    }
}

impl ValueFree for toha::updates::UpdateError {
    fn value_free(self) -> ServiceError {
        use toha::updates::UpdateError as E;
        match self {
            // The warnings are Toha's text, so they stay out as well.
            E::WithWarnings { source, .. } => source.value_free(),
            E::Admission(e) => e.value_free(),
            E::Resolve(e) => e.value_free(),
            E::Requirements(_) => ServiceError::refused(
                "missing-requirements",
                "programs the template requires are missing",
            ),
            E::NotGit => {
                ServiceError::conflict("not-git", "the workspace is not inside a Git repository")
            }
            E::DirtyTarget => {
                ServiceError::conflict("dirty-target", "the workspace has uncommitted changes")
            }
            E::StaleApplication => {
                ServiceError::conflict("stale-application", "the applied record changed")
            }
            E::InvalidApplication => {
                ServiceError::bad_request("malformed-session", "the staged application is invalid")
            }
            E::Base { .. } => ServiceError::refused(
                "merge-base-failed",
                "the workspace's recorded template revision could not be rebuilt",
            ),
            E::Project(_) => {
                ServiceError::refused("project-invalid", "the workspace's project is invalid")
            }
            _ => ServiceError::refused("update-refused", "the upgrade was refused"),
        }
    }
}

impl From<toha::updates::UpdateError> for ServiceError {
    fn from(error: toha::updates::UpdateError) -> Self {
        use toha::updates::UpdateError as E;
        match error {
            E::WithWarnings { warnings, source } => Self::from(*source).with_warnings(warnings),
            E::Admission(e) => (*e).into(),
            E::Resolve(e) => e.into(),
            other => {
                let message = other.to_string();
                other.value_free().with_message(message)
            }
        }
    }
}

impl ValueFree for toha::applied::MergeError {
    fn value_free(self) -> ServiceError {
        use toha::applied::MergeError as E;
        match self {
            E::WrongTarget => ServiceError::conflict(
                "session-mismatch",
                "the upgrade was prepared for another workspace",
            ),
            E::Apply(e) => (*e).into(),
            // Occupied paths are found after the template's hooks ran, so
            // they stay out.
            E::Occupied(_) => ServiceError::conflict(
                "target-occupied",
                "paths the upgrade adds are occupied by untracked files",
            ),
            E::Changed => ServiceError::conflict(
                "target-changed",
                "the workspace or its index changed during the upgrade",
            ),
            E::Io(_) => ServiceError::new(500, "io-failed", "the upgrade could not be written"),
            _ => ServiceError::refused("merge-failed", "the upgrade could not be merged"),
        }
    }
}

impl From<toha::applied::MergeError> for ServiceError {
    fn from(error: toha::applied::MergeError) -> Self {
        use toha::applied::MergeError as E;
        let message = error.to_string();
        let paths = match &error {
            E::Occupied(paths) => paths.iter().map(ToString::to_string).collect(),
            _ => vec![],
        };
        match error {
            E::Apply(e) => (*e).into(),
            other => other.value_free().with_message(message).with_paths(paths),
        }
    }
}

impl ValueFree for toha::updates::SessionError {
    fn value_free(self) -> ServiceError {
        use toha::updates::SessionError as E;
        match self {
            E::Update(e) => e.value_free(),
            E::Operation(e) => e.value_free(),
            E::Staging(e) => e.value_free(),
            E::Merge(e) => e.value_free(),
            E::Evaluation(_) => {
                ServiceError::refused("evaluation-failed", "the template could not be evaluated")
            }
            _ => ServiceError::refused("update-refused", "the upgrade was refused"),
        }
    }
}

impl From<toha::updates::SessionError> for ServiceError {
    fn from(error: toha::updates::SessionError) -> Self {
        use toha::updates::SessionError as E;
        match error {
            E::Update(e) => e.into(),
            E::Operation(e) => e.into(),
            E::Staging(e) => e.into(),
            E::Merge(e) => e.into(),
            other => {
                let message = other.to_string();
                other.value_free().with_message(message)
            }
        }
    }
}

impl From<toha::protocol::AnswersDocumentError> for ServiceError {
    fn from(error: toha::protocol::AnswersDocumentError) -> Self {
        use toha::protocol::AnswersDocumentError as E;
        let kind = match &error {
            E::TemplateMismatch { .. } => "submission-template-mismatch",
            _ => "malformed-submission",
        };
        Self::bad_request(kind, error.to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use toha::ApplyError as A;
    use toha::applied::{MergeError, RepoPath};
    use toha::hook::{HookError, HookOutcome};
    use toha::operations::OperationError;
    use toha::plan::{PlanError, PlannedHook, PlannedProgram, TargetPath};
    use toha::staging::StagingError;
    use toha::updates::{SessionError, UpdateError};

    /// What an earlier hook captured. Every value below that Toha could
    /// render after that hook ran carries it.
    const SECRET: &str = "captured-sample-value";

    fn secret_io() -> std::io::Error {
        std::io::Error::other(format!("no such folder /{SECRET}"))
    }

    /// A hook whose rendered argument and working directory carry an
    /// earlier hook's captured output.
    fn rendered_hook() -> PlannedHook {
        PlannedHook {
            program: PlannedProgram::Run(vec!["sh".into(), "-c".into(), format!("use {SECRET}")]),
            cwd: Some(TargetPath::parse(&format!("dir/{SECRET}")).unwrap()),
            template_root: "/var/lib/sample/cache/t".into(),
            id: Some(toha::template::Id::parse("step").unwrap()),
            capture: Default::default(),
            allow_failure: false,
            parse_json: false,
            status_id: None,
        }
    }

    /// Every apply error that can carry a rendered value, each carrying
    /// the captured one.
    fn apply_errors() -> Vec<A> {
        vec![
            A::Deferred(PlanError::Path(format!("invalid target path: /{SECRET}"))),
            A::Symlink(TargetPath::parse(SECRET).unwrap()),
            A::Io {
                path: format!("/srv/spaces/alpha/{SECRET}").into(),
                source: secret_io(),
            },
            A::HookIo {
                index: 1,
                hook: rendered_hook(),
                source: HookError::Io(secret_io()),
            },
            A::Hook {
                index: 1,
                hook: rendered_hook(),
                outcome: HookOutcome {
                    success: false,
                    code: Some(3),
                    stdout: Some(SECRET.into()),
                    stderr: Some(SECRET.into()),
                },
            },
            A::Record(format!("cannot read {SECRET}")),
            A::Git(toha::git_target::GitTargetError::Discover {
                path: SECRET.into(),
                message: SECRET.into(),
            }),
        ]
    }

    /// The other Toha errors generate and apply can return, each carrying
    /// the captured value, with the kind their detailed form has.
    fn wrapped_errors() -> Vec<(ServiceError, Box<dyn FnOnce() -> ServiceError>)> {
        let mut all: Vec<(ServiceError, Box<dyn FnOnce() -> ServiceError>)> = vec![];
        for build in [
            || OperationError::Plan(PlanError::Path(SECRET.into())),
            || OperationError::Input(SECRET.into()),
            || OperationError::Parts(SECRET.into()),
            || OperationError::Staging(StagingError::Replay(SECRET.into())),
            || OperationError::Staging(StagingError::Io(secret_io())),
            || {
                OperationError::Evaluation(toha::EvalError {
                    id: toha::template::Id::parse("q").unwrap(),
                    field: "default",
                    message: SECRET.into(),
                    expression: Some(SECRET.into()),
                    config_key: None,
                })
            },
            || OperationError::Resolve(toha::resolve::ResolveError::Error(SECRET.into())),
        ] {
            all.push((build().into(), Box::new(move || build().value_free())));
        }
        for build in [
            || MergeError::Git(SECRET.into()),
            || MergeError::Io(SECRET.into()),
            || MergeError::Occupied(vec![RepoPath::parse(SECRET).unwrap()]),
        ] {
            all.push((build().into(), Box::new(move || build().value_free())));
        }
        for build in [
            || SessionError::Update(UpdateError::Error(SECRET.into())),
            || {
                SessionError::Update(UpdateError::WithWarnings {
                    warnings: vec![SECRET.into()],
                    source: Box::new(UpdateError::Base {
                        template: SECRET.into(),
                        revision: toha::applied::Revision::Unversioned,
                        reason: SECRET.into(),
                    }),
                })
            },
            || SessionError::Merge(MergeError::Git(SECRET.into())),
            || SessionError::Operation(OperationError::Input(SECRET.into())),
        ] {
            all.push((build().into(), Box::new(move || build().value_free())));
        }
        all
    }

    fn leaks(error: &ServiceError) -> bool {
        String::from_utf8(error.body()).unwrap().contains(SECRET)
    }

    #[test]
    fn no_refusal_of_generate_or_apply_carries_a_captured_value() {
        // `ApplyError` is not `Clone`, so each wrapping takes a fresh one.
        let nth = |i: usize| apply_errors().swap_remove(i);
        for i in 0..apply_errors().len() {
            for refusal in [
                ServiceError::from(nth(i)),
                OperationError::from(nth(i)).value_free(),
                MergeError::Apply(Box::new(nth(i))).value_free(),
                SessionError::Merge(MergeError::Apply(Box::new(nth(i)))).value_free(),
            ] {
                assert!(!leaks(&refusal), "{:?}: {refusal:?}", nth(i));
            }
        }
        for (detailed, value_free) in wrapped_errors() {
            let refusal = value_free();
            assert!(!leaks(&refusal), "{detailed:?}: {refusal:?}");
            assert_eq!(
                (refusal.status, refusal.kind),
                (detailed.status, detailed.kind),
                "{detailed:?}"
            );
        }
    }

    #[test]
    fn a_failed_hook_names_its_position_and_id_but_never_its_arguments() {
        let failed = ServiceError::from(A::Hook {
            index: 1,
            hook: rendered_hook(),
            outcome: HookOutcome {
                success: false,
                code: Some(3),
                stdout: None,
                stderr: None,
            },
        });
        assert_eq!(
            (failed.kind, failed.message.as_str()),
            ("hook-failed", "hook 1 `step` failed with exit code 3")
        );
        let io = ServiceError::from(A::HookIo {
            index: 0,
            hook: rendered_hook(),
            source: HookError::EmptyCommand,
        });
        assert_eq!(
            (io.kind, io.message.as_str()),
            (
                "hook-io-failed",
                "hook 0 `step` could not run: its command is empty"
            )
        );
    }

    #[test]
    fn conflicts_found_before_any_hook_keep_their_paths() {
        let conflict =
            ServiceError::from(A::Conflicts(vec![TargetPath::parse("out.txt").unwrap()]));
        assert_eq!(
            (conflict.kind, conflict.paths.as_slice()),
            ("target-conflict", ["out.txt".to_owned()].as_slice())
        );
    }
}
