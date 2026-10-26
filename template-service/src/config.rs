//! The template service's settings, read from Menagerie's shared configuration file.
//!
//! The workspace service owns the file's full schema and strict validation. This
//! service reads only the members it uses: `workspace-root`, `templates`,
//! `template-service.socket`, `template-service.hook-runner-socket` and
//! `template-service.candidate-folder`, with the workspace service's rules
//! for each: the root is cleaned lexically and never `/`, an empty socket or
//! folder takes the default, the candidate folder is clean, never `/`, and
//! neither inside the root nor containing it, template ids are unique.
//! Other members are ignored here. Toha's storage folder is this service's
//! own `-data` flag, not a configuration key.
use serde::Deserialize;
use std::path::{Component, Path, PathBuf};

pub const DEFAULT_CONFIG: &str = "/etc/menagerie/config.yml";
pub const DEFAULT_SOCKET: &str = "/run/menagerie/template.sock";
pub const DEFAULT_HOOK_RUNNER_SOCKET: &str = "/run/menagerie/hook-runner.sock";
pub const DEFAULT_DATA: &str = "/var/lib/menagerie/toha";
pub const DEFAULT_CANDIDATE_FOLDER: &str = "/var/lib/menagerie/candidates";

/// One approved template: its configuration id, display name and Toha address.
/// Its `runner-image` belongs to the workspace service, which runs the hooks.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ApprovedTemplate {
    pub id: String,
    pub name: String,
    pub address: String,
    #[serde(rename = "runner-image", default)]
    pub runner_image: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Settings {
    /// Absolute, cleaned workspace root, used as written.
    pub workspace_root: PathBuf,
    pub templates: Vec<ApprovedTemplate>,
    pub socket: PathBuf,
    /// The workspace service's hook runner, which runs every hook.
    pub hook_runner_socket: PathBuf,
    /// This service's temporary folder, where Toha builds an upgrade's
    /// candidate; the hook runner accepts an upgrade's hook target only
    /// inside it.
    pub candidate_folder: PathBuf,
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
    #[serde(rename = "hook-runner-socket")]
    hook_runner_socket: Option<String>,
    #[serde(rename = "candidate-folder")]
    candidate_folder: Option<String>,
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
        let (socket, hook_runner_socket, candidate_folder) = match file.template_service {
            Some(s) => (s.socket, s.hook_runner_socket, s.candidate_folder),
            None => (None, None, None),
        };
        let socket = socket_path(socket, "template-service.socket", DEFAULT_SOCKET)?;
        let hook_runner_socket = socket_path(
            hook_runner_socket,
            "template-service.hook-runner-socket",
            DEFAULT_HOOK_RUNNER_SOCKET,
        )?;
        let candidate_folder = candidate_folder_path(candidate_folder, &workspace_root)?;
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
            hook_runner_socket,
            candidate_folder,
            data,
        })
    }

    pub fn template(&self, id: &str) -> Option<&ApprovedTemplate> {
        self.templates.iter().find(|t| t.id == id)
    }
}

/// A configured socket path, absolute; absent or empty takes `default`.
fn socket_path(value: Option<String>, key: &str, default: &str) -> Result<PathBuf, ConfigError> {
    match value {
        Some(socket) if !socket.is_empty() => {
            if !Path::new(&socket).is_absolute() {
                return Err(ConfigError::Invalid(format!(
                    "{key} {socket:?} must be absolute"
                )));
            }
            Ok(PathBuf::from(socket))
        }
        _ => Ok(PathBuf::from(default)),
    }
}

/// The configured candidate folder: absent or empty takes the default; it
/// must be absolute and clean, not `/`, and neither inside the workspace
/// root nor containing it.
fn candidate_folder_path(value: Option<String>, root: &Path) -> Result<PathBuf, ConfigError> {
    let folder = match value {
        Some(folder) if !folder.is_empty() => folder,
        _ => return Ok(PathBuf::from(DEFAULT_CANDIDATE_FOLDER)),
    };
    if clean_absolute(&folder)
        .is_none_or(|clean| clean.as_os_str() != folder.as_str() || clean == Path::new("/"))
    {
        return Err(ConfigError::Invalid(format!(
            "template-service.candidate-folder {folder:?} must be an absolute clean folder other than /"
        )));
    }
    let folder = PathBuf::from(folder);
    if folder.starts_with(root) || root.starts_with(&folder) {
        return Err(ConfigError::Invalid(
            "template-service.candidate-folder must be neither inside workspace-root nor contain it".into(),
        ));
    }
    Ok(folder)
}

/// Create the candidate folder when it is missing, private to this user,
/// and make it the temporary folder of every `tempfile` call in this
/// process, Toha's included. Call it once, before any temporary file.
pub fn prepare_candidate_folder(folder: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(folder)?;
    let meta = std::fs::symlink_metadata(folder)?;
    if !meta.is_dir() {
        return Err(std::io::Error::other(
            "the candidate folder is not a folder",
        ));
    }
    std::fs::set_permissions(folder, std::fs::Permissions::from_mode(0o700))?;
    tempfile::env::override_temp_dir(folder)
        .map_err(|_| std::io::Error::other("the temporary folder was already chosen"))
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
            settings.hook_runner_socket,
            PathBuf::from(DEFAULT_HOOK_RUNNER_SOCKET)
        );
        assert_eq!(
            settings.candidate_folder,
            PathBuf::from(DEFAULT_CANDIDATE_FOLDER)
        );
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
        let settings = Settings::parse(
            "workspace-root: /srv\ntemplate-service: {hook-runner-socket: /run/sample/h.sock}\ntemplates: [{id: a, name: n, address: a, runner-image: 'alpine:3.22'}]\n",
            Path::new(DATA),
        )
        .unwrap();
        assert_eq!(
            settings.hook_runner_socket.as_os_str(),
            "/run/sample/h.sock"
        );
        assert_eq!(settings.socket, PathBuf::from(DEFAULT_SOCKET));
        let settings = Settings::parse(
            "workspace-root: /srv/spaces
template-service: {candidate-folder: /srv/candidates}
",
            Path::new(DATA),
        )
        .unwrap();
        assert_eq!(settings.candidate_folder.as_os_str(), "/srv/candidates");
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
                "workspace-root: /srv\ntemplate-service: {hook-runner-socket: rel.sock}\n",
                "template-service.hook-runner-socket \"rel.sock\" must be absolute",
            ),
            (
                "workspace-root: /srv\ntemplate-service: {candidate-folder: rel}\n",
                "template-service.candidate-folder \"rel\" must be an absolute clean folder",
            ),
            (
                "workspace-root: /srv\ntemplate-service: {candidate-folder: /var/sample/../c}\n",
                "must be an absolute clean folder",
            ),
            (
                "workspace-root: /srv\ntemplate-service: {candidate-folder: /}\n",
                "must be an absolute clean folder",
            ),
            (
                "workspace-root: /srv\ntemplate-service: {candidate-folder: /srv/c}\n",
                "neither inside workspace-root nor contain it",
            ),
            (
                "workspace-root: /srv/spaces\ntemplate-service: {candidate-folder: /srv}\n",
                "neither inside workspace-root nor contain it",
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
