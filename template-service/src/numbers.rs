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

/// Refuse every number in the (syntactically valid) JSON `body` that Toha
/// cannot hold exactly, naming each by its JSON Pointer.
pub fn check(body: &[u8]) -> Result<(), ServiceError> {
    let mut scan = Scan {
        text: body,
        at: 0,
        path: vec![],
        refused: vec![],
    };
    scan.value();
    if scan.refused.is_empty() {
        return Ok(());
    }
    let mut questions: Vec<String> = scan
        .refused
        .iter()
        .filter_map(|(path, _)| match path.as_slice() {
            [submission, answers, id, ..] if submission == "submission" && answers == "answers" => {
                Some(id.clone())
            }
            _ => None,
        })
        .collect();
    questions.dedup();
    let locations: Vec<String> = scan.refused.iter().map(|(path, _)| pointer(path)).collect();
    let listed = scan
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
    let scale = exponent - fraction.len() as i128 + (significant.len() - trimmed.len()) as i128;
    Some((negative, trimmed.to_owned(), scale))
}

fn pointer(path: &[String]) -> String {
    path.iter()
        .map(|part| format!("/{}", part.replace('~', "~0").replace('/', "~1")))
        .collect()
}

/// A recursive scan of valid JSON that records each number token with its path.
/// The caller has checked the syntax; serde_json's nesting limit bounds the depth.
struct Scan<'a> {
    text: &'a [u8],
    at: usize,
    path: Vec<String>,
    refused: Vec<(Vec<String>, String)>,
}

impl Scan<'_> {
    fn space(&mut self) {
        while self.at < self.text.len() && self.text[self.at].is_ascii_whitespace() {
            self.at += 1;
        }
    }

    fn peek(&self) -> u8 {
        self.text.get(self.at).copied().unwrap_or(0)
    }

    fn value(&mut self) {
        self.space();
        match self.peek() {
            b'{' => {
                self.at += 1;
                loop {
                    self.space();
                    if self.peek() == b'}' {
                        self.at += 1;
                        return;
                    }
                    let key = self.string();
                    self.space();
                    self.at += 1; // ':'
                    self.path.push(key);
                    self.value();
                    self.path.pop();
                    self.space();
                    if self.peek() == b',' {
                        self.at += 1;
                    }
                }
            }
            b'[' => {
                self.at += 1;
                let mut index = 0usize;
                loop {
                    self.space();
                    if self.peek() == b']' {
                        self.at += 1;
                        return;
                    }
                    self.path.push(index.to_string());
                    self.value();
                    self.path.pop();
                    index += 1;
                    self.space();
                    if self.peek() == b',' {
                        self.at += 1;
                    }
                }
            }
            b'"' => {
                self.string();
            }
            b'-' | b'0'..=b'9' => {
                let start = self.at;
                while matches!(self.peek(), b'-' | b'+' | b'.' | b'e' | b'E' | b'0'..=b'9') {
                    self.at += 1;
                }
                let token = String::from_utf8_lossy(&self.text[start..self.at]).into_owned();
                if !representable(&token) {
                    self.refused.push((self.path.clone(), token));
                }
            }
            _ => {
                while self.peek().is_ascii_alphabetic() {
                    self.at += 1;
                }
            }
        }
    }

    /// A string token, decoded.
    fn string(&mut self) -> String {
        let start = self.at;
        self.at += 1;
        while self.at < self.text.len() {
            match self.text[self.at] {
                b'\\' => self.at += 2,
                b'"' => {
                    self.at += 1;
                    break;
                }
                _ => self.at += 1,
            }
        }
        serde_json::from_slice(&self.text[start..self.at]).unwrap_or_default()
    }
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
}
