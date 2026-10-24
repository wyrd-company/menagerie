//! The template service's settings, read from Menagerie's shared configuration file.
//!
//! The workspace service owns the file's full schema and strict validation. This
//! service reads only the members it uses: `workspace-root`, `templates` and
//! `template-service.socket`, with the workspace service's rules for each:
//! the root is cleaned lexically and never `/`, an empty socket takes the
//! default, template ids are unique. Other members are ignored here. Toha's
//! storage folder is this service's own `-data` flag, not a configuration key.
use serde::Deserialize;
use std::path::{Component, Path, PathBuf};

pub const DEFAULT_CONFIG: &str = "/etc/menagerie/config.yml";
pub const DEFAULT_SOCKET: &str = "/run/menagerie/template.sock";
pub const DEFAULT_DATA: &str = "/var/lib/menagerie/toha";

/// One approved template: its configuration id, display name and Toha address.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ApprovedTemplate {
    pub id: String,
    pub name: String,
    pub address: String,
}

#[derive(Debug, Clone)]
pub struct Settings {
    /// Absolute, cleaned workspace root, used as written.
    pub workspace_root: PathBuf,
    pub templates: Vec<ApprovedTemplate>,
    pub socket: PathBuf,
    /// Absolute folder holding Toha's configuration, registry, cache and state.
    pub data: PathBuf,
}

#[derive(Deserialize)]
struct File {
    #[serde(rename = "workspace-root")]
    workspace_root: Option<String>,
    #[serde(default)]
    templates: Vec<ApprovedTemplate>,
    #[serde(rename = "template-service")]
    template_service: Option<Service>,
}

#[derive(Deserialize)]
struct Service {
    socket: Option<String>,
}

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("{path}: {source}")]
    Read {
        path: PathBuf,
        source: std::io::Error,
    },
    #[error("{path}: {message}")]
    Parse { path: PathBuf, message: String },
    #[error("{0}")]
    Invalid(String),
}

impl Settings {
    /// Read the configuration file at `path`, with Toha's data under `data`.
    pub fn load(path: &Path, data: &Path) -> Result<Self, ConfigError> {
        let text = std::fs::read_to_string(path).map_err(|source| ConfigError::Read {
            path: path.to_owned(),
            source,
        })?;
        Self::parse(&text, data).map_err(|e| match e {
            ConfigError::Parse { message, .. } => ConfigError::Parse {
                path: path.to_owned(),
                message,
            },
            other => other,
        })
    }

    pub fn parse(text: &str, data: &Path) -> Result<Self, ConfigError> {
        let file: File = serde_norway::from_str(text).map_err(|e| ConfigError::Parse {
            path: PathBuf::new(),
            message: e.to_string(),
        })?;
        let root = file
            .workspace_root
            .ok_or_else(|| ConfigError::Invalid("workspace-root is required".into()))?;
        let workspace_root = clean_absolute(&root)
            .filter(|p| p != Path::new("/"))
            .ok_or_else(|| {
                ConfigError::Invalid(format!(
                    "workspace-root {root:?} must be an absolute folder other than /"
                ))
            })?;
        let socket = match file.template_service.and_then(|s| s.socket) {
            Some(socket) if !socket.is_empty() => {
                if !Path::new(&socket).is_absolute() {
                    return Err(ConfigError::Invalid(format!(
                        "template-service.socket {socket:?} must be absolute"
                    )));
                }
                PathBuf::from(socket)
            }
            _ => PathBuf::from(DEFAULT_SOCKET),
        };
        let data = clean_absolute(&data.to_string_lossy()).ok_or_else(|| {
            ConfigError::Invalid(format!("data folder {} must be absolute", data.display()))
        })?;
        let mut seen = std::collections::HashSet::new();
        for template in &file.templates {
            if !valid_id(&template.id) {
                return Err(ConfigError::Invalid(format!(
                    "template id {:?} must be lower-case letters and digits separated by single dashes",
                    template.id
                )));
            }
            if !seen.insert(template.id.as_str()) {
                return Err(ConfigError::Invalid(format!(
                    "template id {:?} is repeated",
                    template.id
                )));
            }
            if template.name.is_empty() || template.address.is_empty() {
                return Err(ConfigError::Invalid(format!(
                    "template {:?} needs a name and an address",
                    template.id
                )));
            }
        }
        Ok(Self {
            workspace_root,
            templates: file.templates,
            socket,
            data,
        })
    }

    pub fn template(&self, id: &str) -> Option<&ApprovedTemplate> {
        self.templates.iter().find(|t| t.id == id)
    }
}

/// `^[a-z0-9]+(-[a-z0-9]+)*$`, the workspace service's template id rule.
fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.split('-').all(|part| {
            !part.is_empty()
                && part
                    .bytes()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit())
        })
}

/// The path cleaned lexically (`.`, `..`, repeated separators), when absolute.
pub fn clean_absolute(path: &str) -> Option<PathBuf> {
    let path = Path::new(path);
    if !path.is_absolute() {
        return None;
    }
    let mut cleaned = PathBuf::new();
    for component in path.components() {
        match component {
            Component::ParentDir => {
                cleaned.pop();
            }
            Component::CurDir => {}
            other => cleaned.push(other),
        }
    }
    Some(cleaned)
}

#[cfg(test)]
mod tests {
    use super::*;

    const DATA: &str = "/var/lib/sample";

    #[test]
    fn reads_only_its_members_and_applies_defaults() {
        let settings = Settings::parse(
            "workspace-root: /srv/spaces/./\nworkspace-repository: git@example.invalid:o/r\noidc: {issuer: x, client-id: y}\nwyrwood-directory: /run/wyrwood\nregistry-credentials:\n  kv-mount: kv\n  registries:\n    - {registry: registry.example.invalid, path: registries/sample}\ntemplates:\n  - {id: sample-one, name: Sample, address: 'https://example.invalid/t'}\n",
            Path::new(DATA),
        )
        .unwrap();
        assert_eq!(settings.workspace_root, PathBuf::from("/srv/spaces"));
        assert_eq!(settings.socket, PathBuf::from(DEFAULT_SOCKET));
        assert_eq!(
            settings.template("sample-one").unwrap().address,
            "https://example.invalid/t"
        );
        assert!(settings.template("other").is_none());
    }

    #[test]
    fn reads_the_configured_socket() {
        let settings = Settings::parse(
            "workspace-root: /srv\ntemplate-service: {socket: /run/sample/./t.sock}\n",
            Path::new(DATA),
        )
        .unwrap();
        assert_eq!(settings.socket.as_os_str(), "/run/sample/./t.sock");
        let settings = Settings::parse(
            "workspace-root: /srv\ntemplate-service: {socket: ''}\n",
            Path::new(DATA),
        )
        .unwrap();
        assert_eq!(settings.socket, PathBuf::from(DEFAULT_SOCKET));
    }

    #[test]
    fn rejects_invalid_members() {
        for (text, needle) in [
            ("templates: []\n", "workspace-root is required"),
            ("workspace-root: relative\n", "must be an absolute folder"),
            ("workspace-root: /\n", "must be an absolute folder"),
            ("workspace-root: /srv/..\n", "must be an absolute folder"),
            (
                "workspace-root: /srv\ntemplate-service: {socket: rel.sock}\n",
                "must be absolute",
            ),
            (
                "workspace-root: /srv\ntemplates: [{id: Bad, name: n, address: a}]\n",
                "lower-case",
            ),
            (
                "workspace-root: /srv\ntemplates: [{id: a--b, name: n, address: a}]\n",
                "lower-case",
            ),
            (
                "workspace-root: /srv\ntemplates: [{id: a, name: n, address: a}, {id: a, name: m, address: b}]\n",
                "repeated",
            ),
            (
                "workspace-root: /srv\ntemplates: [{id: a, name: '', address: a}]\n",
                "needs a name",
            ),
            (
                "workspace-root: /srv\ntemplates: [{id: a, name: n, address: a, extra: 1}]\n",
                "unknown field",
            ),
        ] {
            let error = Settings::parse(text, Path::new(DATA))
                .unwrap_err()
                .to_string();
            assert!(error.contains(needle), "{text:?}: {error}");
        }
        let error = Settings::parse("workspace-root: /srv\n", Path::new("relative")).unwrap_err();
        assert!(error.to_string().contains("data folder"), "{error}");
    }
}
