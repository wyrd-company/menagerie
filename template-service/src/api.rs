//! Request and reply bodies of the internal socket API.
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::path::PathBuf;

/// Names a template by its address, for the address and versions routes.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AddressRequest {
    pub address: String,
}

/// What Toha's grammar makes of an address, reaching no network.
#[derive(Debug, Serialize)]
pub struct AddressReply {
    pub source: String,
    pub reference: Option<String>,
    pub base: String,
}

/// A repository's versions, in ascending order.
#[derive(Debug, Serialize)]
pub struct VersionsReply {
    pub source: String,
    pub versions: Vec<crate::versions::Found>,
}

/// Checks one version of a template: its address, without a ref, and the
/// version's tag.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CheckRequest {
    pub address: String,
    #[serde(rename = "ref")]
    pub reference: String,
}

/// A checked version: the commit its ref resolved to, and whether the
/// template's content folder holds `.devcontainer/devcontainer.json`.
#[derive(Debug, Serialize)]
pub struct CheckReply {
    pub source: String,
    pub commit: String,
    pub definition: bool,
}

/// Starts a create interview.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct StartRequest {
    pub workspace: String,
    pub target: PathBuf,
    /// The template's address, without a ref.
    pub address: String,
    /// The version's tag, or another ref of the repository.
    #[serde(rename = "ref")]
    pub reference: String,
    /// The commit the workspace service recorded for the ref, when it has
    /// one: a ref that resolves to another is refused.
    #[serde(default)]
    pub commit: Option<String>,
}

/// Starts an upgrade. `reanswer` asks every question again, with the
/// workspace's recorded answers as their defaults.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct UpgradeRequest {
    pub workspace: String,
    pub target: PathBuf,
    /// The template's address, without a ref.
    pub address: String,
    /// The version's tag, or another ref of the repository.
    #[serde(rename = "ref")]
    pub reference: String,
    /// The commit the workspace service recorded for the ref, when it has
    /// one: a ref that resolves to another is refused.
    #[serde(default)]
    pub commit: Option<String>,
    #[serde(default)]
    pub reanswer: bool,
}

/// Submits one batch of answers against the session of the latest reply.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct AnswerRequest {
    pub workspace: String,
    pub target: PathBuf,
    /// The template's address, without a ref.
    pub address: String,
    /// The version's tag, or another ref of the repository.
    #[serde(rename = "ref")]
    pub reference: String,
    /// The commit the workspace service recorded for the ref, when it has
    /// one: a ref that resolves to another is refused.
    #[serde(default)]
    pub commit: Option<String>,
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
    /// The template's address, without a ref.
    pub address: String,
    /// The version's tag, or another ref of the repository.
    #[serde(rename = "ref")]
    pub reference: String,
    /// The commit the workspace service recorded for the ref, when it has
    /// one: a ref that resolves to another is refused.
    #[serde(default)]
    pub commit: Option<String>,
    pub session: Value,
    #[serde(default)]
    pub hook_token: Option<String>,
}

/// The request members every route checks before any Toha work.
pub struct Selector<'a> {
    pub workspace: &'a str,
    pub target: &'a std::path::Path,
    pub address: &'a str,
    pub reference: &'a str,
    pub commit: Option<&'a str>,
}

macro_rules! selector {
    ($t:ty) => {
        impl $t {
            pub fn selector(&self) -> Selector<'_> {
                Selector {
                    workspace: &self.workspace,
                    target: &self.target,
                    address: &self.address,
                    reference: &self.reference,
                    commit: self.commit.as_deref(),
                }
            }
        }
    };
}
selector!(StartRequest);
selector!(UpgradeRequest);
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
