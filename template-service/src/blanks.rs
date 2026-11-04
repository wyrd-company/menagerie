//! Questions a submission leaves out. Toha's answers protocol gives a question
//! left out its default, but its update resume skips a recorded batch with no
//! answers at all, so a batch accepted as shown, `{}`, is asked again
//! (wyrd-company/toha#67). Before a batch reaches Toha, each question the
//! reply showed and the submission left out gets the answer Toha's terminal
//! gives a blank: its default, else `null`, the empty answer, when it is
//! optional. A required question without a default stays out, for Toha to
//! refuse. A document question stays out, so Toha reads its input file.
use serde_json::Value;
use toha::operations::SessionState;
use toha::template::Id;
use toha::{RawAnswer, RawAnswers};

/// `answers` with the shown questions it leaves out answered as blanks.
pub fn filled(state: &SessionState, mut answers: RawAnswers) -> RawAnswers {
    let SessionState::Asking { document, .. } = state else {
        return answers;
    };
    let schema = &document["schema"];
    let Some(properties) = schema["properties"].as_object() else {
        return answers;
    };
    let required = |id: &str| {
        schema["required"]
            .as_array()
            .is_some_and(|ids| ids.iter().any(|r| r.as_str() == Some(id)))
    };
    for (name, property) in properties {
        // A document question is the only one whose JSON type is an object.
        let types = match &property["type"] {
            Value::Array(types) => types.clone(),
            one => vec![one.clone()],
        };
        if types.iter().any(|t| t == "object") {
            continue;
        }
        let Ok(id) = Id::parse(name) else {
            continue;
        };
        if answers.contains_key(&id) {
            continue;
        }
        let blank = match property.get("default") {
            Some(default) => default.clone(),
            None if !required(name) => Value::Null,
            None => continue,
        };
        answers.insert(id, RawAnswer(blank));
    }
    answers
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn asking(schema: Value) -> SessionState {
        SessionState::Asking {
            document: json!({ "schema": schema }),
            rejections: Default::default(),
        }
    }

    fn answers(value: Value) -> RawAnswers {
        value
            .as_object()
            .unwrap()
            .iter()
            .map(|(k, v)| (Id::parse(k).unwrap(), RawAnswer(v.clone())))
            .collect()
    }

    fn plain(answers: &RawAnswers) -> Value {
        Value::Object(
            answers
                .iter()
                .map(|(k, v)| (k.to_string(), v.0.clone()))
                .collect(),
        )
    }

    #[test]
    fn a_question_left_out_is_answered_as_a_blank_at_the_terminal() {
        let state = asking(json!({
            "properties": {
                "given": {"type": "string", "default": "recorded"},
                "shown": {"type": "array", "default": ["a", "c"]},
                "optional": {"type": ["string", "null"]},
                "needed": {"type": "string"},
                "catalog": {"type": "object"},
                "inputs": {"type": ["object", "null"]},
            },
            "required": ["given", "shown", "needed"],
        }));
        let filled = filled(&state, answers(json!({"given": "changed"})));
        assert_eq!(
            plain(&filled),
            json!({"given": "changed", "shown": ["a", "c"], "optional": null})
        );
    }

    #[test]
    fn a_batch_accepted_as_shown_is_never_empty() {
        let state = asking(json!({
            "properties": {"detail": {"type": "string", "default": "first"}},
            "required": ["detail"],
        }));
        assert_eq!(
            plain(&filled(&state, RawAnswers::new())),
            json!({"detail": "first"})
        );
    }
}
