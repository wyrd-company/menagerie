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

impl From<toha::resolve::ResolveError> for ServiceError {
    fn from(error: toha::resolve::ResolveError) -> Self {
        use toha::resolve::ResolveError as E;
        let kind = match &error {
            E::NotFound(_) => "template-not-found",
            E::StaleRevision { .. } => "template-stale-revision",
            E::Ambiguous { .. } => "template-ambiguous",
            _ => "template-resolve-failed",
        };
        let message = match error {
            E::Error(m) | E::NotFound(m) => m,
            E::StaleRevision { message } => message,
            E::Ambiguous { name, matches } => {
                format!("template {name:?} is ambiguous: {}", matches.join(", "))
            }
            other => format!("{other:?}"),
        };
        Self::refused(kind, message)
    }
}

impl From<toha::ApplyError> for ServiceError {
    fn from(error: toha::ApplyError) -> Self {
        use toha::ApplyError as E;
        let message = error.to_string();
        match error {
            E::Conflicts(paths) => Self::conflict("target-conflict", message)
                .with_paths(paths.iter().map(ToString::to_string).collect()),
            E::Drift(regions) => Self::conflict("target-drift", message)
                .with_paths(regions.iter().map(|(p, _)| p.to_string()).collect()),
            E::Symlink(path) => {
                Self::conflict("target-symlink", message).with_paths(vec![path.to_string()])
            }
            E::Hook {
                index,
                hook,
                outcome,
            } => Self::refused("hook-failed", hook_failure(index, &hook, &outcome)),
            E::HookOutput { .. } => Self::refused("hook-failed", message),
            E::HookIo {
                index,
                hook,
                source,
            } => Self::new(
                500,
                "hook-io-failed",
                format!("{} could not run: {source}", hook_name(index, &hook)),
            ),
            E::Io { .. } => Self::new(500, "io-failed", message),
            E::Record(_) => Self::new(500, "record-failed", message),
            _ => Self::refused("apply-failed", message),
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

impl From<toha::operations::OperationError> for ServiceError {
    fn from(error: toha::operations::OperationError) -> Self {
        use toha::operations::OperationError as E;
        let message = error.to_string();
        match error {
            E::Resolve(e) => e.into(),
            E::Apply(e) => (*e).into(),
            E::Load(_) => Self::refused("template-invalid", message),
            E::Requirements(_) => Self::refused("missing-requirements", message),
            E::Document(_) => Self::refused("document-invalid", message),
            E::Parts(_) => Self::refused("parts-invalid", message),
            E::Context(_) => Self::refused("context-invalid", message),
            E::Staging(e) => e.into(),
            E::Evaluation(_) => Self::refused("evaluation-failed", message),
            E::Plan(_) => Self::refused("plan-failed", message),
            E::Record(_) => Self::refused("record-invalid", message),
            E::Seed(_) => Self::bad_request("malformed-session", message),
            E::Input(_) => Self::conflict("session-mismatch", message),
            _ => Self::refused("operation-refused", message),
        }
    }
}

impl From<toha::staging::StagingError> for ServiceError {
    fn from(error: toha::staging::StagingError) -> Self {
        use toha::staging::StagingError as E;
        let message = error.to_string();
        match error {
            E::EnvironmentTrustRequired { .. } | E::EnvironmentAdmission { .. } => {
                Self::refused("environment-refused", message)
            }
            E::Io(_) => Self::new(500, "io-failed", message),
            _ => Self::bad_request("malformed-session", message),
        }
    }
}

impl From<toha::updates::UpdateError> for ServiceError {
    fn from(error: toha::updates::UpdateError) -> Self {
        use toha::updates::UpdateError as E;
        let message = error.to_string();
        match error {
            E::WithWarnings { warnings, source } => Self::from(*source).with_warnings(warnings),
            E::Admission(e) => (*e).into(),
            E::Resolve(e) => e.into(),
            E::Requirements(_) => Self::refused("missing-requirements", message),
            E::NotGit => Self::conflict("not-git", message),
            E::DirtyTarget => Self::conflict("dirty-target", message),
            E::StaleApplication => Self::conflict("stale-application", message),
            E::InvalidApplication => Self::bad_request("malformed-session", message),
            E::Base { .. } => Self::refused("merge-base-failed", message),
            E::Project(_) => Self::refused("project-invalid", message),
            _ => Self::refused("update-refused", message),
        }
    }
}

impl From<toha::applied::MergeError> for ServiceError {
    fn from(error: toha::applied::MergeError) -> Self {
        use toha::applied::MergeError as E;
        let message = error.to_string();
        match error {
            E::WrongTarget => Self::conflict("session-mismatch", message),
            E::Apply(e) => (*e).into(),
            E::Occupied(paths) => Self::conflict("target-occupied", message)
                .with_paths(paths.iter().map(ToString::to_string).collect()),
            E::Changed => Self::conflict("target-changed", message),
            E::Io(_) => Self::new(500, "io-failed", message),
            _ => Self::refused("merge-failed", message),
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
            E::Evaluation(e) => Self::refused("evaluation-failed", e.to_string()),
            other => Self::refused("update-refused", other.to_string()),
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
    use toha::hook::{HookError, HookOutcome};
    use toha::plan::{PlannedHook, PlannedProgram, TargetPath};

    const SECRET: &str = "captured-secret";

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

    #[test]
    fn a_failed_hook_names_its_position_and_id_but_never_its_arguments() {
        let failed = ServiceError::from(toha::ApplyError::Hook {
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
        let io = ServiceError::from(toha::ApplyError::HookIo {
            index: 0,
            hook: rendered_hook(),
            source: HookError::EmptyCommand,
        });
        assert_eq!(io.kind, "hook-io-failed");
        assert_eq!(
            io.message,
            "hook 0 `step` could not run: empty hook command"
        );
        for error in [failed, io] {
            assert!(!String::from_utf8(error.body()).unwrap().contains(SECRET));
        }
    }
}
