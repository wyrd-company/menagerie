//! Owned replies built from Toha's interview states and apply outcomes.
use crate::api::{ApplyReply, FileReport, HookResult, InterviewReply, InterviewState, Outcome};
use crate::error::ServiceError;
use serde_json::Value;
use toha::operations::SessionState;
use toha::staging::{CanonicalTarget, StagedRecord};

/// The reply to a start or answer. `session` is the record's signed value.
pub fn interview(
    record: &StagedRecord,
    session: Value,
    target: &CanonicalTarget,
    state: SessionState,
    warnings: Vec<String>,
) -> Result<InterviewReply, ServiceError> {
    let (state, document) = match state {
        SessionState::Asking { document, .. } => (InterviewState::Questions, document),
        SessionState::Completed(_) => (InterviewState::Completed, Value::Null),
        SessionState::Ended(ended) => (
            InterviewState::Ended,
            toha::protocol::ended_document(&ended, &toha::protocol::Context::new(target, record)),
        ),
        other => {
            return Err(ServiceError::internal(format!(
                "unexpected interview state {other:?}"
            )));
        }
    };
    Ok(InterviewReply {
        session,
        state,
        document,
        warnings,
    })
}

/// The completed interview of a reconstructed session. An interview that still
/// asks or that ended cannot be applied.
pub fn completed(state: SessionState) -> Result<toha::Completed, ServiceError> {
    match state {
        SessionState::Completed(done) => Ok(done),
        SessionState::Ended(_) => Err(ServiceError::conflict(
            "interview-ended",
            "the interview ended; nothing can be applied",
        )),
        _ => Err(ServiceError::conflict(
            "interview-not-complete",
            "the interview still has questions",
        )),
    }
}

/// The interview still asks: refuse an answer to a completed or ended session.
pub fn asking(state: &SessionState) -> Result<(), ServiceError> {
    match state {
        SessionState::Asking { .. } => Ok(()),
        SessionState::Completed(_) => Err(ServiceError::conflict(
            "interview-complete",
            "the interview is complete; generate or apply it",
        )),
        _ => Err(ServiceError::conflict(
            "interview-ended",
            "the interview ended; it takes no more answers",
        )),
    }
}

/// A create's apply outcome. Generation is complete only when Toha wrote the
/// applied record; files without a record are a failed generation.
pub fn generated(
    applied: toha::Applied,
    actions: &[toha::protocol::PlannedPath],
    mut messages: Vec<String>,
) -> Result<ApplyReply, ServiceError> {
    match applied {
        toha::Applied::Written {
            files,
            record,
            hooks,
            after_apply,
            ..
        } => {
            let files: Vec<FileReport> = files
                .iter()
                .map(|path| {
                    let path = path.to_string();
                    let action = actions
                        .iter()
                        .find(|planned| planned.path == path)
                        .map_or_else(|| "create".to_owned(), |planned| planned.action.clone());
                    FileReport { path, action }
                })
                .collect();
            let record_path = match record {
                toha::applied::RecordOutcome::Written(path) => path,
                other => {
                    let mut error = ServiceError::new(
                        500,
                        "record-not-written",
                        format!("files were written but the applied record was not ({other:?})"),
                    );
                    error.paths = files.into_iter().map(|f| f.path).collect();
                    return Err(error);
                }
            };
            messages.extend(after_apply);
            Ok(ApplyReply {
                outcome: Outcome::Written,
                files,
                record_path: Some(record_path),
                messages,
                hooks: hooks.iter().map(HookResult::from).collect(),
                conflicts: vec![],
            })
        }
        toha::Applied::NeedsTrust(_) => Ok(ApplyReply::without_writes(
            Outcome::NeedsTrust,
            vec![],
            messages,
        )),
    }
}

pub fn planned_files(paths: Vec<toha::protocol::PlannedPath>) -> Vec<FileReport> {
    paths
        .into_iter()
        .map(|p| FileReport {
            path: p.path,
            action: p.action,
        })
        .collect()
}

fn change(change: &toha::applied::Change) -> FileReport {
    use toha::applied::Action;
    let action = match change.action {
        Action::Added => "added",
        Action::Updated => "updated",
        Action::Merged => "merged",
        Action::Deleted => "deleted",
        Action::Conflicted(_) => "conflicted",
        Action::NotPreviewed => "not-previewed",
    };
    FileReport {
        path: change.path.to_string(),
        action: action.to_owned(),
    }
}

/// An upgrade's merge outcome. A written merge with conflicts is reported as
/// `conflicted`, never as a clean `written`.
pub fn merged(merged: toha::applied::Merged, report: toha::applied::UpdateReport) -> ApplyReply {
    use toha::applied::Merged;
    let hooks = report.hooks.iter().map(HookResult::from).collect();
    match merged {
        Merged::AlreadyCurrent => {
            ApplyReply::without_writes(Outcome::AlreadyCurrent, vec![], report.messages)
        }
        Merged::NeedsTrust => {
            ApplyReply::without_writes(Outcome::NeedsTrust, vec![], report.messages)
        }
        Merged::Planned { changes } => ApplyReply::without_writes(
            Outcome::Planned,
            changes.iter().map(change).collect(),
            report.messages,
        ),
        Merged::Written {
            record,
            changes,
            conflicted,
        } => ApplyReply {
            outcome: if conflicted.is_empty() {
                Outcome::Written
            } else {
                Outcome::Conflicted
            },
            files: changes.iter().map(change).collect(),
            record_path: Some(record),
            messages: report.messages,
            hooks,
            conflicts: conflicted.iter().map(ToString::to_string).collect(),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn files_without_the_applied_record_are_a_failed_generation() {
        for record in [
            toha::applied::RecordOutcome::NotGit,
            toha::applied::RecordOutcome::Internal,
        ] {
            let applied = toha::Applied::Written {
                files: vec![],
                record,
                hooks_run: 0,
                hooks: vec![],
                after_apply: None,
                git: None,
            };
            let error = generated(applied, &[], vec![]).unwrap_err();
            assert_eq!((error.status, error.kind), (500, "record-not-written"));
        }
        let applied = toha::Applied::Written {
            files: vec![],
            record: toha::applied::RecordOutcome::Written(".toha.json".into()),
            hooks_run: 0,
            hooks: vec![],
            after_apply: Some("done".into()),
            git: None,
        };
        let reply = generated(applied, &[], vec!["first".into()]).unwrap();
        assert_eq!(reply.outcome, Outcome::Written);
        assert_eq!(reply.record_path.as_deref(), Some(".toha.json"));
        assert_eq!(reply.messages, ["first", "done"]);
    }
}
