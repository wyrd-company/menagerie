//! The session value the browser holds between requests carries a signature
//! that binds Toha's staged record to its flow, workspace and approved
//! template. Every route that takes a session verifies it before reading the
//! record, so an edited, unsigned or replayed value never reaches Toha.
use crate::checks::Operation;
use crate::error::ServiceError;
use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::hmac;
use serde_json::{Map, Value};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use toha::staging::StagedRecord;

/// The key file under the service's data folder.
pub const KEY_FILE: &str = "session-key";
const KEY_LEN: usize = 32;
const DOMAIN: &[u8] = b"menagerie-template-session-1";

/// The signing key. It lives in the data folder so that sessions continue
/// across a service restart.
#[derive(Debug, Clone)]
pub struct SessionKey(hmac::Key);

impl SessionKey {
    pub fn new(bytes: &[u8; KEY_LEN]) -> Self {
        Self(hmac::Key::new(hmac::HMAC_SHA256, bytes))
    }

    /// Read the key file in `data`, or create it with fresh random bytes when
    /// it does not exist. A concurrent start that creates it first wins; a
    /// file of the wrong length is refused.
    pub fn load_or_create(data: &Path) -> std::io::Result<Self> {
        std::fs::create_dir_all(data)?;
        let path = data.join(KEY_FILE);
        match std::fs::read(&path) {
            Ok(bytes) => return Self::from_file(&bytes, &path),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
        let mut bytes = [0u8; KEY_LEN];
        ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut bytes)
            .map_err(|_| std::io::Error::other("no random bytes for the session key"))?;
        // Write a private file under a unique name, then link it into place:
        // the key file appears whole or not at all, and never replaces another.
        let staged = data.join(format!("{KEY_FILE}.{}", std::process::id()));
        let written = (|| {
            let mut file = std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .open(&staged)?;
            file.write_all(&bytes)?;
            file.sync_all()?;
            std::fs::hard_link(&staged, &path)
        })();
        let _ = std::fs::remove_file(&staged);
        match written {
            Ok(()) => Ok(Self::new(&bytes)),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                Self::from_file(&std::fs::read(&path)?, &path)
            }
            Err(e) => Err(e),
        }
    }

    fn from_file(bytes: &[u8], path: &Path) -> std::io::Result<Self> {
        let bytes: &[u8; KEY_LEN] = bytes.try_into().map_err(|_| {
            std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                format!("{}: a session key is {KEY_LEN} bytes", path.display()),
            )
        })?;
        Ok(Self::new(bytes))
    }
}

/// What a session is bound to besides its record.
#[derive(Debug, Clone, Copy)]
pub struct Binding<'a> {
    pub operation: Operation,
    pub workspace: &'a str,
    pub template_id: &'a str,
}

impl<'a> Binding<'a> {
    pub fn new(operation: Operation, workspace: &'a str, template_id: &'a str) -> Self {
        Self {
            operation,
            workspace,
            template_id,
        }
    }
}

/// The session value of a reply: `{"record","signature"}`.
pub fn seal(
    key: &SessionKey,
    binding: Binding<'_>,
    record: &StagedRecord,
) -> Result<Value, ServiceError> {
    let record = serde_json::to_value(record)
        .map_err(|e| ServiceError::internal(format!("session does not serialize: {e}")))?;
    sealed(key, binding, record)
}

/// Sign a record value. The signature covers the record's serialization,
/// which is the same for every equal value, so a record that returns
/// unchanged verifies whatever its whitespace or escapes.
pub fn sealed(
    key: &SessionKey,
    binding: Binding<'_>,
    record: Value,
) -> Result<Value, ServiceError> {
    let tag = hmac::sign(&key.0, &message(binding, &record)?);
    let mut session = Map::new();
    session.insert("record".into(), record);
    session.insert(
        "signature".into(),
        Value::String(URL_SAFE_NO_PAD.encode(tag.as_ref())),
    );
    Ok(Value::Object(session))
}

/// Toha's staged record from a request's session value, once its signature
/// verifies for this binding.
pub fn open(
    key: &SessionKey,
    binding: Binding<'_>,
    session: Value,
) -> Result<StagedRecord, ServiceError> {
    crate::checks::session(verify(key, binding, session)?)
}

/// The record of a session value whose signature verifies for this binding.
fn verify(key: &SessionKey, binding: Binding<'_>, session: Value) -> Result<Value, ServiceError> {
    let Value::Object(mut members) = session else {
        return Err(tampered());
    };
    let signature = members.remove("signature");
    let record = members.remove("record");
    let (Some(Value::String(signature)), Some(record), true) =
        (signature, record, members.is_empty())
    else {
        return Err(tampered());
    };
    let signature = URL_SAFE_NO_PAD.decode(signature).map_err(|_| tampered())?;
    hmac::verify(&key.0, &message(binding, &record)?, &signature).map_err(|_| tampered())?;
    Ok(record)
}

fn tampered() -> ServiceError {
    ServiceError::bad_request(
        "session-tampered",
        "the session is unsigned or altered, or was issued for another workspace, template or flow",
    )
}

/// The signed bytes: a domain tag, then each bound value and the record's
/// serialization, each prefixed by its length.
fn message(binding: Binding<'_>, record: &Value) -> Result<Vec<u8>, ServiceError> {
    let record = serde_json::to_vec(record)
        .map_err(|e| ServiceError::internal(format!("session does not serialize: {e}")))?;
    let operation: &[u8] = match binding.operation {
        Operation::Create => b"create",
        Operation::Upgrade => b"upgrade",
    };
    let mut message = Vec::with_capacity(DOMAIN.len() + record.len() + 128);
    for part in [
        DOMAIN,
        operation,
        binding.workspace.as_bytes(),
        binding.template_id.as_bytes(),
        &record,
    ] {
        message.extend_from_slice(&(part.len() as u64).to_be_bytes());
        message.extend_from_slice(part);
    }
    Ok(message)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::os::unix::fs::PermissionsExt;

    fn binding(
        operation: Operation,
        workspace: &'static str,
        template_id: &'static str,
    ) -> Binding<'static> {
        Binding {
            operation,
            workspace,
            template_id,
        }
    }

    fn verifies(key: &SessionKey, binding: Binding<'_>, session: Value) -> bool {
        verify(key, binding, session).is_ok()
    }

    #[test]
    fn a_signature_binds_the_record_flow_workspace_and_template() {
        let key = SessionKey::new(&[7; KEY_LEN]);
        let bound = binding(Operation::Create, "alpha", "sample-v1");
        let record = json!({"template": "sample", "submissions": [{"n": 1.5}]});
        let session = sealed(&key, bound, record.clone()).unwrap();
        assert!(verifies(&key, bound, session.clone()));
        // Reparsing changes nothing the signature covers.
        let text = serde_json::to_string(&session).unwrap();
        assert!(verifies(&key, bound, serde_json::from_str(&text).unwrap()));

        for other in [
            binding(Operation::Upgrade, "alpha", "sample-v1"),
            binding(Operation::Create, "beta", "sample-v1"),
            binding(Operation::Create, "alpha", "sample-v2"),
        ] {
            assert!(!verifies(&key, other, session.clone()), "{other:?}");
        }
        assert!(!verifies(
            &SessionKey::new(&[8; KEY_LEN]),
            bound,
            session.clone()
        ));

        let mut edited = session.clone();
        edited["record"]["submissions"][0]["n"] = json!(2);
        assert!(!verifies(&key, bound, edited));
        let mut extra = session.clone();
        extra["more"] = json!(true);
        assert!(!verifies(&key, bound, extra));
        assert!(!verifies(&key, bound, record));
    }

    #[test]
    fn length_prefixes_keep_bound_values_apart() {
        let record = json!({});
        assert_ne!(
            message(binding(Operation::Create, "ab", "c"), &record).unwrap(),
            message(binding(Operation::Create, "a", "bc"), &record).unwrap()
        );
    }

    #[test]
    fn open_refuses_every_unsigned_shape() {
        let key = SessionKey::new(&[7; KEY_LEN]);
        let bound = binding(Operation::Create, "alpha", "sample-v1");
        for session in [
            json!(null),
            json!("text"),
            json!({}),
            json!({"record": {}}),
            json!({"signature": "AAAA"}),
            json!({"record": {}, "signature": 1}),
            json!({"record": {}, "signature": "not base64!"}),
            json!({"record": {}, "signature": "AAAA"}),
        ] {
            let error = verify(&key, bound, session.clone()).unwrap_err();
            assert_eq!(
                (error.status, error.kind),
                (400, "session-tampered"),
                "{session}"
            );
        }
        // A signed value that is not a staged record is malformed, not tampered.
        let session = sealed(&key, bound, json!({"not": "a record"})).unwrap();
        assert_eq!(
            open(&key, bound, session).unwrap_err().kind,
            "malformed-session"
        );
    }

    #[test]
    fn the_key_file_is_created_once_private_and_reused() {
        let data = tempfile::tempdir().unwrap();
        let folder = data.path().join("toha");
        let first = SessionKey::load_or_create(&folder).unwrap();
        let path = folder.join(KEY_FILE);
        let meta = std::fs::metadata(&path).unwrap();
        assert_eq!(meta.len(), KEY_LEN as u64);
        assert_eq!(meta.permissions().mode() & 0o777, 0o600);
        let bytes = std::fs::read(&path).unwrap();
        let second = SessionKey::load_or_create(&folder).unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
        let bound = binding(Operation::Create, "alpha", "sample-v1");
        let session = sealed(&first, bound, json!({})).unwrap();
        assert!(verifies(&second, bound, session));
        assert_eq!(
            std::fs::read_dir(&folder).unwrap().count(),
            1,
            "staged file left behind"
        );
    }

    #[test]
    fn a_key_file_of_the_wrong_length_is_refused() {
        let data = tempfile::tempdir().unwrap();
        std::fs::write(data.path().join(KEY_FILE), b"short").unwrap();
        let error = SessionKey::load_or_create(data.path()).unwrap_err();
        assert_eq!(error.kind(), std::io::ErrorKind::InvalidData);
    }
}
