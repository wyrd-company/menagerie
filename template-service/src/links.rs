//! A template's folder and its symbolic links stay inside the checkout Toha
//! fetched, so no address makes the service read its own files, such as
//! its session key, as a template.
use crate::error::ServiceError;
use std::path::{Component, Path, PathBuf};

fn unsafe_link() -> ServiceError {
    ServiceError::refused(
        "unsafe-template-link",
        "the template's folder, or a symbolic link in it, leads outside its repository",
    )
}

/// The template folder of the checkout at `root`, `root` joined with the
/// address's `#path` when it has one, with its symbolic links resolved. It
/// must be inside the checkout, and every symbolic link in it, followed or
/// not, must resolve inside the checkout too. The checkout's own `.git` is
/// not template content and is not walked.
pub fn checked_folder(root: &Path, path: Option<&str>) -> Result<PathBuf, ServiceError> {
    let root = root.canonicalize().map_err(|_| unsafe_link())?;
    let joined = match path {
        Some(path) => root.join(path),
        None => root.clone(),
    };
    if std::fs::symlink_metadata(&joined).is_err() {
        return Err(ServiceError::refused(
            "template-invalid",
            "the template's folder is not in its repository",
        ));
    }
    let folder = joined.canonicalize().map_err(|_| unsafe_link())?;
    if !folder.starts_with(&root) {
        return Err(unsafe_link());
    }
    let mut pending = vec![folder.clone()];
    while let Some(dir) = pending.pop() {
        let entries = std::fs::read_dir(&dir).map_err(|_| unsafe_link())?;
        for entry in entries {
            let entry = entry.map_err(|_| unsafe_link())?;
            let path = entry.path();
            if dir == root && entry.file_name() == ".git" {
                continue;
            }
            let kind = entry.file_type().map_err(|_| unsafe_link())?;
            if kind.is_symlink() {
                if !resolves_inside(&path, &root) {
                    return Err(unsafe_link());
                }
            } else if kind.is_dir() {
                pending.push(path);
            }
        }
    }
    Ok(folder)
}

/// Check the template a session's record names, at its commit, where
/// Toha's resume reads it: Toha's cache entry for the commit, with the
/// formal name's `#path`, or the installation that holds the commit. Call
/// it before Toha reads any of the template's files.
pub fn check_session(
    record: &toha::staging::StagedRecord,
    host: &crate::host::TohaHost,
    config: &toha::config::Config,
    registry: &toha::registry::Registry,
) -> Result<(), ServiceError> {
    let resolved = toha::resolve::resume_template(
        &record.template,
        &record.commit,
        record.named,
        config,
        registry,
        &host.paths,
        &host.cwd,
    )?;
    let path =
        match toha::source::parse(&record.template, &config.hosts, &host.cwd, &host.paths.home) {
            Ok(toha::source::Address::Git { path, .. }) => path,
            _ => None,
        };
    let entry = host
        .paths
        .cache
        .join("sources")
        .join(toha::source::install_key(&record.template))
        .join(&record.commit);
    let cached = entry
        .canonicalize()
        .is_ok_and(|entry| resolved.folder.starts_with(entry));
    if cached {
        checked_folder(&entry, path.as_deref())?;
    } else {
        checked_folder(&resolved.folder, None)?;
    }
    Ok(())
}

/// Where the link at `link` leads: its real path when its target exists,
/// else its target cleaned lexically against the link's folder.
fn resolves_inside(link: &Path, root: &Path) -> bool {
    if let Ok(real) = link.canonicalize() {
        return real.starts_with(root);
    }
    let Ok(target) = std::fs::read_link(link) else {
        return false;
    };
    let joined = match link.parent() {
        Some(parent) => parent.join(target),
        None => target,
    };
    let mut cleaned = PathBuf::new();
    for component in joined.components() {
        match component {
            Component::ParentDir => {
                cleaned.pop();
            }
            Component::CurDir => {}
            other => cleaned.push(other),
        }
    }
    cleaned.starts_with(root)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    fn checkout() -> (tempfile::TempDir, PathBuf) {
        let temp = tempfile::tempdir().unwrap();
        let root = temp.path().canonicalize().unwrap().join("checkout");
        std::fs::create_dir_all(root.join("kit/template/sub")).unwrap();
        std::fs::write(root.join("kit/template.yml"), "name: kit\n").unwrap();
        std::fs::create_dir_all(root.join(".git")).unwrap();
        (temp, root)
    }

    #[test]
    fn a_folder_with_links_inside_its_checkout_is_admitted() {
        let (_temp, root) = checkout();
        symlink("../template.yml", root.join("kit/template/copy.yml")).unwrap();
        symlink("missing-here", root.join("kit/template/dangling")).unwrap();
        symlink("/elsewhere", root.join(".git/ignored")).unwrap();
        assert_eq!(
            checked_folder(&root, Some("kit")).unwrap(),
            root.join("kit")
        );
        assert_eq!(checked_folder(&root, None).unwrap(), root);
    }

    #[test]
    fn a_folder_or_link_that_leaves_the_checkout_is_refused() {
        let (temp, root) = checkout();
        let outside = temp.path().canonicalize().unwrap().join("outside");
        std::fs::create_dir_all(&outside).unwrap();
        std::fs::write(outside.join("session-key"), "secret").unwrap();
        symlink(&outside, root.join("away")).unwrap();
        for path in ["..", "../outside", "away"] {
            let error = checked_folder(&root, Some(path)).unwrap_err();
            assert_eq!(error.kind, "unsafe-template-link", "{path}");
        }
        let error = checked_folder(&root, Some("missing")).unwrap_err();
        assert_eq!(error.kind, "template-invalid");
        for (name, target) in [
            ("absolute", outside.join("session-key")),
            ("relative", PathBuf::from("../../../../outside/session-key")),
            ("dangling", PathBuf::from("../../../../outside/absent")),
            ("folder", outside.clone()),
        ] {
            let link = root.join("kit/template/sub").join(name);
            symlink(&target, &link).unwrap();
            let error = checked_folder(&root, Some("kit")).unwrap_err();
            assert_eq!(error.kind, "unsafe-template-link", "{name}");
            assert!(
                !error.message.contains(outside.to_str().unwrap()),
                "{}",
                error.message
            );
            std::fs::remove_file(&link).unwrap();
        }
    }
}
