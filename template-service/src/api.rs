//! Request and reply bodies of the internal socket API.
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::path::PathBuf;

/// Starts a create interview or an upgrade.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct StartRequest {
    pub workspace: String,
    pub target: PathBuf,
    pub template_id: String,
}

/// Submits one batch of answers against the session of the latest reply.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct AnswerRequest {
    pub workspace: String,
    pub target: PathBuf,
    pub template_id: String,
    /// Toha's staged record, as returned in the latest reply.
    pub session: Value,
    /// Toha's answers envelope `{"template","answers"}`.
    pub submission: Value,
}

/// Generates a workspace or applies an upgrade from a completed session.
/// `hookToken` is the workspace service's grant for this request's hooks;
/// without it every hook fails.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct ApplyRequest {
    pub workspace: String,
    pub target: PathBuf,
    pub template_id: String,
    pub session: Value,
    #[serde(default)]
    pub hook_token: Option<String>,
}

/// The request members every route checks before any Toha work.
pub struct Selector<'a> {
    pub workspace: &'a str,
    pub target: &'a std::path::Path,
    pub template_id: &'a str,
}

macro_rules! selector {
    ($t:ty) => {
        impl $t {
            pub fn selector(&self) -> Selector<'_> {
                Selector {
                    workspace: &self.workspace,
                    target: &self.target,
                    template_id: &self.template_id,
                }
            }
        }
    };
}
selector!(StartRequest);
selector!(AnswerRequest);
selector!(ApplyRequest);

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum InterviewState {
    Questions,
    Completed,
    Ended,
}

/// The next step of an interview. `document` is Toha's batch document while
/// questions remain, Toha's ended document after a flow end, and `null` once the
/// interview completed. `session` is the whole staged record to send back.
#[derive(Debug, Serialize)]
pub struct InterviewReply {
    pub session: Value,
    pub state: InterviewState,
    pub document: Value,
    pub warnings: Vec<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "kebab-case")]
pub enum Outcome {
    Written,
    AlreadyCurrent,
    NeedsTrust,
    Planned,
    Conflicted,
}

#[derive(Debug, Serialize, PartialEq, Eq)]
pub struct FileReport {
    pub path: String,
    pub action: String,
}

#[derive(Debug, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct HookResult {
    pub id: Option<String>,
    pub exit_code: Option<i32>,
    pub success: bool,
}

/// The result of a generation or an upgrade apply. `written` and `conflicted`
/// wrote files and the applied record at `recordPath`; `conflicted` lists the
/// paths written with merge conflicts.
#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ApplyReply {
    pub outcome: Outcome,
    pub files: Vec<FileReport>,
    pub record_path: Option<String>,
    pub messages: Vec<String>,
    pub hooks: Vec<HookResult>,
    pub conflicts: Vec<String>,
}

impl ApplyReply {
    pub fn without_writes(outcome: Outcome, files: Vec<FileReport>, messages: Vec<String>) -> Self {
        Self {
            outcome,
            files,
            record_path: None,
            messages,
            hooks: vec![],
            conflicts: vec![],
        }
    }
}

impl From<&toha::HookReport> for HookResult {
    fn from(report: &toha::HookReport) -> Self {
        Self {
            id: report.id.as_ref().map(ToString::to_string),
            exit_code: report.exit_code,
            success: report.success,
        }
    }
}
