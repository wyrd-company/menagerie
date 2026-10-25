//! Numbers Toha can hold exactly.
//!
//! Toha keeps JSON numbers as 64-bit integers or 64-bit floating-point values
//! (serde_json without `arbitrary_precision`, which Toha's rendering does not
//! support). A request number whose value that representation would change is
//! refused before any Toha conversion, so no value is lost silently. A number is
//! accepted when the text the service will emit for it has the same decimal
//! value: `1.0`, `1e2` and `1.50` are accepted (emitted as `1.0`, `100.0`,
//! `1.5`), as are every `i64` and `u64`; `18446744073709551616` and
//! `0.1000000000000000000001` are refused. Zero is accepted with either sign.
use crate::error::ServiceError;

/// The deepest nesting serde_json parses into a value: its default recursion
/// limit, which every `serde_json::Value` the service and Toha read already
/// obeys. A deeper request could not be read anyway.
pub const MAX_DEPTH: usize = 127;

/// Every number of a request body that Toha cannot hold exactly, with its path.
#[derive(Debug, Default)]
pub struct Scanned {
    refused: Vec<(Vec<String>, String)>,
}

/// Scan a request body without recursion: refuse nesting deeper than
/// [`MAX_DEPTH`] and collect the numbers Toha cannot hold exactly. The scan
/// does not judge syntax; the caller checks that separately.
pub fn scan(body: &[u8]) -> Result<Scanned, ServiceError> {
    let mut scanned = Scanned::default();
    let mut frames: Vec<Frame> = vec![];
    let mut at = 0;
    while at < body.len() {
        match body[at] {
            open @ (b'{' | b'[') => {
                if frames.len() == MAX_DEPTH {
                    return Err(ServiceError::bad_request(
                        "nesting-too-deep",
                        format!(
                            "the request nests deeper than {MAX_DEPTH} levels, the most a JSON value can"
                        ),
                    ));
                }
                frames.push(if open == b'{' {
                    Frame::Object {
                        key: String::new(),
                        expect_key: true,
                    }
                } else {
                    Frame::Array { index: 0 }
                });
                at += 1;
            }
            b'}' | b']' => {
                frames.pop();
                at += 1;
            }
            b',' => {
                match frames.last_mut() {
                    Some(Frame::Object { expect_key, .. }) => *expect_key = true,
                    Some(Frame::Array { index }) => *index += 1,
                    None => {}
                }
                at += 1;
            }
            b'"' => {
                let start = at;
                at += 1;
                while at < body.len() {
                    match body[at] {
                        b'\\' => at += 2,
                        b'"' => {
                            at += 1;
                            break;
                        }
                        _ => at += 1,
                    }
                }
                if let Some(Frame::Object { key, expect_key }) = frames.last_mut()
                    && *expect_key
                {
                    *key = serde_json::from_slice(&body[start..at.min(body.len())])
                        .unwrap_or_default();
                    *expect_key = false;
                }
            }
            b'-' | b'0'..=b'9' => {
                let start = at;
                while at < body.len()
                    && matches!(body[at], b'-' | b'+' | b'.' | b'e' | b'E' | b'0'..=b'9')
                {
                    at += 1;
                }
                let token = String::from_utf8_lossy(&body[start..at]).into_owned();
                if !representable(&token) {
                    let path = frames.iter().map(Frame::segment).collect();
                    scanned.refused.push((path, token));
                }
            }
            _ => at += 1,
        }
    }
    Ok(scanned)
}

/// One open container on the scan's path.
enum Frame {
    Object { key: String, expect_key: bool },
    Array { index: usize },
}

impl Frame {
    fn segment(&self) -> String {
        match self {
            Frame::Object { key, .. } => key.clone(),
            Frame::Array { index } => index.to_string(),
        }
    }
}

impl Scanned {
    /// Refuse the request when it holds a number Toha cannot hold exactly,
    /// naming each by its JSON Pointer and each answered question.
    pub fn check(self) -> Result<(), ServiceError> {
        if self.refused.is_empty() {
            return Ok(());
        }
        let mut questions: Vec<String> = self
            .refused
            .iter()
            .filter_map(|(path, _)| match path.as_slice() {
                [submission, answers, id, ..]
                    if submission == "submission" && answers == "answers" =>
                {
                    Some(id.clone())
                }
                _ => None,
            })
            .collect();
        questions.dedup();
        let locations: Vec<String> = self.refused.iter().map(|(path, _)| pointer(path)).collect();
        let listed = self
            .refused
            .iter()
            .map(|(path, token)| format!("{token} at {}", pointer(path)))
            .collect::<Vec<_>>()
            .join(", ");
        let mut error = ServiceError::bad_request(
            "number-not-representable",
            format!(
                "Toha holds numbers as 64-bit integers or floating-point values and cannot keep the exact value of: {listed}"
            ),
        );
        error.locations = locations;
        error.questions = questions;
        Err(error)
    }
}

/// Scan `body` and refuse it for too deep nesting or a number Toha cannot hold.
pub fn check(body: &[u8]) -> Result<(), ServiceError> {
    scan(body)?.check()
}

/// Whether serde_json's value for `token` is emitted with the same decimal value.
pub fn representable(token: &str) -> bool {
    let Ok(value) = serde_json::from_str::<serde_json::Value>(token) else {
        return false;
    };
    let emitted = value.to_string();
    match (canonical(token), canonical(&emitted)) {
        (Some(given), Some(kept)) => given == kept,
        _ => false,
    }
}

/// A JSON number's value as sign, significant digits and decimal exponent, with
/// no leading or trailing zero digits. Zero has no digits.
fn canonical(token: &str) -> Option<(bool, String, i128)> {
    let (negative, rest) = match token.strip_prefix('-') {
        Some(rest) => (true, rest),
        None => (false, token),
    };
    let (mantissa, exponent) = match rest.find(['e', 'E']) {
        Some(i) => (&rest[..i], Some(&rest[i + 1..])),
        None => (rest, None),
    };
    let (whole, fraction) = mantissa.split_once('.').unwrap_or((mantissa, ""));
    let digits = format!("{whole}{fraction}");
    let significant = digits.trim_start_matches('0');
    if significant.is_empty() {
        return Some((negative, String::new(), 0));
    }
    let trimmed = significant.trim_end_matches('0');
    let exponent: i128 = match exponent {
        Some(text) => text.trim_start_matches('+').parse().ok()?,
        None => 0,
    };
    // Checked: an exponent at the i128 boundary has no representable scale.
    let scale = exponent
        .checked_sub(fraction.len() as i128)?
        .checked_add((significant.len() - trimmed.len()) as i128)?;
    Some((negative, trimmed.to_owned(), scale))
}

fn pointer(path: &[String]) -> String {
    path.iter()
        .map(|part| format!("/{}", part.replace('~', "~0").replace('/', "~1")))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_values_toha_holds_exactly() {
        for token in [
            "0",
            "-0",
            "0.0",
            "-0.0",
            "0e999999999999999999999",
            "1",
            "1.0",
            "1.50",
            "1e2",
            "1E+2",
            "-1.5e-3",
            "0.1",
            "0.3",
            "1e30",
            "123.456",
            "9223372036854775807",
            "-9223372036854775808",
            "18446744073709551615",
            "5e-324",
            "2.2250738585072014e-308",
            "1.7976931348623157e308",
            "-1.7976931348623157e308",
        ] {
            assert!(representable(token), "{token} refused");
        }
    }

    #[test]
    fn refuses_values_toha_would_change() {
        for token in [
            "18446744073709551616",
            "-9223372036854775809",
            "123456789012345678901234567890",
            "0.1000000000000000000001",
            "1e400",
            "-1e400",
            "1e-400",
            "4e-325",
            "1.00000000000000000000000001",
            "1e999999999999999999999999999999999999999",
        ] {
            assert!(!representable(token), "{token} accepted");
        }
    }

    #[test]
    fn names_each_refused_number_with_its_pointer_and_question() {
        let body = br#"{"session":{"submissions":[{"a":[1,2.5,{"b/c~":18446744073709551616}]}]},
            "submission":{"template":"t","answers":{"size":123456789012345678901234567890,
            "nested":{"list":[0.1,1e-400]},"fine":1.50}}}"#;
        let error = check(body).unwrap_err();
        assert_eq!(error.status, 400);
        assert_eq!(error.kind, "number-not-representable");
        assert_eq!(
            error.locations,
            [
                "/session/submissions/0/a/2/b~1c~0",
                "/submission/answers/size",
                "/submission/answers/nested/list/1"
            ]
        );
        assert_eq!(error.questions, ["size", "nested"]);
        assert!(
            error
                .message
                .contains("123456789012345678901234567890 at /submission/answers/size")
        );
    }

    #[test]
    fn passes_bodies_without_refused_numbers() {
        check(br#"{"a":[1,-0,1e2,{"b":"1e400","c":true,"d":null}],"e\"\\":"x"}"#).unwrap();
        check(b"[]").unwrap();
    }

    /// Exponents at the i128 boundaries: the scale arithmetic overflows and the
    /// value is refused instead of panicking; zero stays zero at any scale.
    #[test]
    fn exponent_extremes_are_refused_without_overflow() {
        const MIN: &str = "170141183460469231731687303715884105728";
        const MAX: &str = "170141183460469231731687303715884105727";
        for token in [
            format!("1.0e-{MIN}"),
            format!("1.5e-{MIN}"),
            format!("10e{MAX}"),
            format!("1e{MAX}"),
            format!("-1.0e-{MIN}"),
        ] {
            assert!(!representable(&token), "{token} accepted");
        }
        for token in [
            format!("0.0e-{MIN}"),
            format!("0e{MAX}"),
            format!("-0.00e{MAX}"),
        ] {
            assert!(representable(&token), "{token} refused");
        }
    }

    fn nested(depth: usize) -> String {
        format!("{}0{}", "[".repeat(depth), "]".repeat(depth))
    }

    /// The depth bound is serde_json's own: the deepest value serde_json
    /// parses passes, one level deeper is refused by both.
    #[test]
    fn the_nesting_bound_is_serde_json_s() {
        assert!(serde_json::from_str::<serde_json::Value>(&nested(MAX_DEPTH)).is_ok());
        assert!(serde_json::from_str::<serde_json::Value>(&nested(MAX_DEPTH + 1)).is_err());
        assert!(scan(nested(MAX_DEPTH).as_bytes()).is_ok());
        let error = scan(nested(MAX_DEPTH + 1).as_bytes()).unwrap_err();
        assert_eq!((error.status, error.kind), (400, "nesting-too-deep"));
        let object = format!(
            "{}0{}",
            "{\"a\":".repeat(MAX_DEPTH + 1),
            "}".repeat(MAX_DEPTH + 1)
        );
        assert!(serde_json::from_str::<serde_json::Value>(&object).is_err());
        assert_eq!(
            scan(object.as_bytes()).unwrap_err().kind,
            "nesting-too-deep"
        );
    }

    /// A body far deeper than any stack is refused without recursion.
    #[test]
    fn a_very_deep_body_is_refused_without_recursion() {
        let error = scan(nested(1_000_000).as_bytes()).unwrap_err();
        assert_eq!(error.kind, "nesting-too-deep");
        let error = scan("[".repeat(1_000_000).as_bytes()).unwrap_err();
        assert_eq!(error.kind, "nesting-too-deep");
    }
}
