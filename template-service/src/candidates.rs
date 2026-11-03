//! The service's work folder inside the candidate folder, and start-up removal
//! of what an ended service left there: Toha's temporary entries, an upgrade's
//! candidate among them, and the workspace service's copy-back folders.
//!
//! Only this service makes temporary entries in the work folder, so another
//! process that shares the candidate folder keeps its own. Each service holds
//! a shared lock on the work folder for its lifetime, and removal needs the
//! lock alone, so a live service keeps its candidates. The lock goes with the
//! process, however it ends. Even then, an entry is removed only when its name
//! has the exact shape its maker gives it, it has the kind of file that maker
//! makes, and it belongs to this user. Nothing is followed through a symbolic
//! link.
use rustix::fd::{AsFd, AsRawFd, BorrowedFd, OwnedFd};
use rustix::fs::{
    AtFlags, CWD, Dir, FileType, FlockOperation, Mode, OFlags, chmodat, fchmod, flock, fstat,
    mkdirat, openat, statat, unlinkat,
};
use rustix::io::Errno;
use std::ffi::{CStr, CString, OsStr, OsString};
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};

/// The work folder's name inside the candidate folder.
pub const WORK_FOLDER: &str = ".menagerie-work";

/// Create the work folder when it is missing and return its path. It must be
/// a folder of this user, and is private to this user.
pub fn prepare_work_folder(candidates: &Path) -> std::io::Result<PathBuf> {
    let parent = open_folder(CWD, candidates)?;
    match mkdirat(&parent, WORK_FOLDER, Mode::RWXU) {
        Ok(()) | Err(Errno::EXIST) => {}
        Err(e) => return Err(e.into()),
    }
    let work = open_folder(&parent, WORK_FOLDER)?;
    if fstat(&work)?.st_uid != rustix::process::geteuid().as_raw() {
        return Err(std::io::Error::other(
            "the work folder belongs to another user",
        ));
    }
    fchmod(&work, Mode::RWXU)?;
    Ok(candidates.join(WORK_FOLDER))
}

/// This service's hold on the work folder. Keep it for the process's lifetime.
#[derive(Debug)]
pub struct Claim {
    _work: OwnedFd,
}

/// What a start did about leftovers.
#[derive(Debug)]
pub enum Removal {
    /// This service alone held the work folder, and this pass ran.
    Swept(Sweep),
    /// Another service holds the work folder, so nothing was removed.
    OtherServiceLive,
    /// The exclusive lock or the pass failed. Leftovers wait for a later start.
    Failed(std::io::Error),
}

/// Take the work folder's shared lock and, when no other service holds it,
/// remove the leftovers first. Call it before the first request. An error
/// means that this service holds no lock, so it must not start: its candidates
/// would be open to another service's sweep. A failure to remove leftovers is
/// not an error.
pub fn claim_and_sweep(work: &Path) -> std::io::Result<(Claim, Removal)> {
    claim_with(work, remove_leftovers)
}

fn claim_with(
    work: &Path,
    pass: impl FnOnce(&OwnedFd) -> std::io::Result<Sweep>,
) -> std::io::Result<(Claim, Removal)> {
    let dir = open_folder(CWD, work)?;
    let removal = match flock(&dir, FlockOperation::NonBlockingLockExclusive) {
        Ok(()) => pass(&dir).map_or_else(Removal::Failed, Removal::Swept),
        Err(Errno::WOULDBLOCK) => Removal::OtherServiceLive,
        // A filesystem can refuse an exclusive lock on a folder and still
        // give a shared one.
        Err(e) => Removal::Failed(e.into()),
    };
    // Another start can take the exclusive lock during this change. It holds
    // no request yet, and this call waits for it, so no request sees a sweep.
    flock(&dir, FlockOperation::LockShared)?;
    Ok((Claim { _work: dir }, removal))
}

/// What one pass over the work folder did.
#[derive(Debug, Default)]
pub struct Sweep {
    pub removed: Vec<OsString>,
    pub failed: Vec<(OsString, std::io::Error)>,
}

/// Remove the leftovers directly inside `folder`. One entry that cannot be
/// removed does not stop the others.
fn remove_leftovers(dir: &OwnedFd) -> std::io::Result<Sweep> {
    let me = rustix::process::geteuid().as_raw();
    let mut sweep = Sweep::default();
    for name in names(dir)? {
        let shown = OsStr::from_bytes(name.as_bytes()).to_owned();
        let stat = match statat(dir, &name, AtFlags::SYMLINK_NOFOLLOW) {
            Ok(stat) => stat,
            Err(Errno::NOENT) => continue,
            Err(e) => {
                sweep.failed.push((shown, e.into()));
                continue;
            }
        };
        if stat.st_uid != me || !is_leftover(name.as_bytes(), FileType::from_raw_mode(stat.st_mode))
        {
            continue;
        }
        match remove_entry(dir.as_fd(), &name) {
            Ok(()) => sweep.removed.push(shown),
            Err(e) => sweep.failed.push((shown, e.into())),
        }
    }
    Ok(sweep)
}

/// `tempfile`'s default name, which every temporary entry Toha makes in its
/// temporary folder has: `.tmp` and six letters or digits, a folder or a file.
/// Or Go's `os.MkdirTemp` name for the workspace service's copy-back folder
/// (`ReplaceFolder` in `backend/internal/throwaway/extract.go`):
/// `.menagerie-copy-` and a decimal 32-bit number, a folder only.
fn is_leftover(name: &[u8], kind: FileType) -> bool {
    if let Some(random) = name.strip_prefix(b".tmp") {
        return matches!(kind, FileType::Directory | FileType::RegularFile)
            && random.len() == 6
            && random.iter().all(u8::is_ascii_alphanumeric);
    }
    if let Some(random) = name.strip_prefix(b".menagerie-copy-") {
        return kind == FileType::Directory
            && (1..=10).contains(&random.len())
            && random.iter().all(u8::is_ascii_digit);
    }
    false
}

/// Remove the entry `name` of `dir` and, for a folder, everything beneath it,
/// read-only folders included, without following a symbolic link. It removes
/// what it can and returns the first failure.
fn remove_entry(dir: BorrowedFd<'_>, name: &CStr) -> Result<(), Errno> {
    let stat = match statat(dir, name, AtFlags::SYMLINK_NOFOLLOW) {
        Ok(stat) => stat,
        Err(Errno::NOENT) => return Ok(()),
        Err(e) => return Err(e),
    };
    if FileType::from_raw_mode(stat.st_mode) != FileType::Directory {
        return tolerate_missing(unlinkat(dir, name, AtFlags::empty()));
    }
    let folder = match open_folder(dir, name) {
        // A folder its owner may not read. `chmodat` always follows a link,
        // so it goes through a descriptor that holds this folder and no link.
        Err(Errno::ACCESS) => {
            let held = openat(
                dir,
                name,
                OFlags::PATH | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )?;
            let magic = format!("/proc/self/fd/{}", held.as_raw_fd());
            chmodat(CWD, magic.as_str(), Mode::RWXU, AtFlags::empty())?;
            open_folder(dir, name)?
        }
        Err(Errno::NOENT) => return Ok(()),
        other => other?,
    };
    let mut first = fchmod(&folder, Mode::RWXU).err();
    match names(&folder) {
        Ok(entries) => {
            for entry in entries {
                if let Err(e) = remove_entry(folder.as_fd(), &entry) {
                    first.get_or_insert(e);
                }
            }
        }
        Err(e) => {
            first.get_or_insert(e);
        }
    }
    drop(folder);
    if let Err(e) = tolerate_missing(unlinkat(dir, name, AtFlags::REMOVEDIR)) {
        first.get_or_insert(e);
    }
    first.map_or(Ok(()), Err)
}

fn tolerate_missing(result: Result<(), Errno>) -> Result<(), Errno> {
    match result {
        Err(Errno::NOENT) => Ok(()),
        other => other,
    }
}

fn open_folder(dir: impl AsFd, path: impl rustix::path::Arg) -> Result<OwnedFd, Errno> {
    openat(
        dir,
        path,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )
}

/// Every entry name of a folder, read before any is removed, so that a folder
/// holds one open descriptor at each depth.
fn names(folder: impl AsFd) -> Result<Vec<CString>, Errno> {
    let mut names = Vec::new();
    for entry in Dir::read_from(folder)? {
        let name = entry?.file_name().to_owned();
        if !matches!(name.as_bytes(), b"." | b"..") {
            names.push(name);
        }
    }
    Ok(names)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{PermissionsExt, symlink};

    fn sweep_of(folder: &Path) -> std::io::Result<Sweep> {
        remove_leftovers(&open_folder(CWD, folder)?)
    }

    fn swept(removal: Removal) -> Sweep {
        match removal {
            Removal::Swept(sweep) => sweep,
            other => panic!("{other:?}"),
        }
    }

    fn set_mode(path: &Path, mode: u32) {
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).unwrap();
    }

    /// The names the makers really produce, so that a change in either
    /// library's naming fails here and not silently in production.
    #[test]
    fn real_temporary_entries_are_recognised_and_removed() {
        let temp = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir_in(temp.path()).unwrap().keep();
        let file = tempfile::NamedTempFile::new_in(temp.path())
            .unwrap()
            .keep()
            .unwrap()
            .1;
        let sweep = sweep_of(temp.path()).unwrap();
        assert!(sweep.failed.is_empty(), "{:?}", sweep.failed);
        assert_eq!(sweep.removed.len(), 2, "{:?}", sweep.removed);
        assert!(!folder.exists() && !file.exists());
        assert!(temp.path().exists());
    }

    #[test]
    fn only_the_exact_shapes_of_the_right_kind_are_leftovers() {
        use FileType::{Directory as D, RegularFile as F, Symlink as L};
        for (name, kind, expected) in [
            (".tmpAb3xY9", D, true),
            (".tmpAb3xY9", F, true),
            (".tmpAb3xY9", L, false),
            (".tmpAb3xY9", FileType::Fifo, false),
            (".tmp", D, false),
            (".tmpAb3xY", D, false),
            (".tmpAb3xY90", D, false),
            (".tmp-b3xY9", D, false),
            (".tmpAb3.Y9", D, false),
            ("tmpAb3xY9a", D, false),
            (".menagerie-copy-1", D, true),
            (".menagerie-copy-4294967295", D, true),
            (".menagerie-copy-1", F, false),
            (".menagerie-copy-1", L, false),
            (".menagerie-copy-", D, false),
            (".menagerie-copy-12345678901", D, false),
            (".menagerie-copy-12a", D, false),
            (".menagerie-copy", D, false),
            ("notes.txt", F, false),
        ] {
            assert_eq!(
                is_leftover(name.as_bytes(), kind),
                expected,
                "{name} as {kind:?}"
            );
        }
    }

    #[test]
    fn a_leftover_goes_with_read_only_folders_and_links_are_not_followed() {
        let temp = tempfile::tempdir().unwrap();
        let outside = temp.path().join("outside");
        std::fs::create_dir(&outside).unwrap();
        std::fs::write(outside.join("kept.txt"), "kept").unwrap();
        let candidates = temp.path().join("candidates");
        let leftover = candidates.join(".tmpAb3xY9");
        std::fs::create_dir_all(leftover.join("read-only/inner")).unwrap();
        std::fs::create_dir_all(leftover.join("closed/inner")).unwrap();
        std::fs::write(leftover.join("read-only/inner/file"), "x").unwrap();
        std::fs::write(leftover.join("closed/inner/file"), "x").unwrap();
        symlink(&outside, leftover.join("link")).unwrap();
        set_mode(&leftover.join("read-only/inner"), 0o500);
        set_mode(&leftover.join("read-only"), 0o500);
        set_mode(&leftover.join("closed"), 0o000);
        // A link with a leftover's name is not this service's: it stays, and
        // what it names stays.
        symlink(&outside, candidates.join(".tmpLinked")).unwrap();

        let sweep = sweep_of(&candidates).unwrap();
        assert!(sweep.failed.is_empty(), "{:?}", sweep.failed);
        assert_eq!(sweep.removed, [OsString::from(".tmpAb3xY9")]);
        assert!(!leftover.exists());
        assert!(
            std::fs::symlink_metadata(candidates.join(".tmpLinked"))
                .unwrap()
                .is_symlink()
        );
        assert_eq!(
            std::fs::read_to_string(outside.join("kept.txt")).unwrap(),
            "kept"
        );
    }

    #[test]
    fn a_link_in_place_of_the_folder_is_refused() {
        let temp = tempfile::tempdir().unwrap();
        let outside = temp.path().join("outside");
        std::fs::create_dir(&outside).unwrap();
        std::fs::create_dir(outside.join(".tmpAb3xY9")).unwrap();
        let link = temp.path().join("candidates");
        symlink(&outside, &link).unwrap();
        assert!(sweep_of(&link).is_err());
        assert!(outside.join(".tmpAb3xY9").exists());
    }

    #[test]
    fn a_live_service_keeps_its_entries_and_an_ended_one_does_not() {
        let temp = tempfile::tempdir().unwrap();
        let work = prepare_work_folder(temp.path()).unwrap();
        let (first, removal) = claim_and_sweep(&work).unwrap();
        assert!(swept(removal).removed.is_empty());
        let live = work.join(".tmpAb3xY9");
        std::fs::create_dir(&live).unwrap();

        let (second, removal) = claim_and_sweep(&work).unwrap();
        assert!(matches!(removal, Removal::OtherServiceLive), "{removal:?}");
        assert!(live.exists());
        drop(first);
        let (third, removal) = claim_and_sweep(&work).unwrap();
        assert!(matches!(removal, Removal::OtherServiceLive), "{removal:?}");
        drop((second, third));

        let (_last, removal) = claim_and_sweep(&work).unwrap();
        assert_eq!(swept(removal).removed, [OsString::from(".tmpAb3xY9")]);
        assert!(!live.exists());
    }

    /// A service whose pass failed still starts, so it still holds its lock:
    /// another start takes nothing from it.
    #[test]
    fn a_failed_pass_keeps_the_lock() {
        let temp = tempfile::tempdir().unwrap();
        let work = prepare_work_folder(temp.path()).unwrap();
        let (_claim, removal) =
            claim_with(&work, |_| Err(std::io::Error::other("sample failure"))).unwrap();
        assert!(matches!(removal, Removal::Failed(_)), "{removal:?}");
        let live = work.join(".tmpAb3xY9");
        std::fs::create_dir(&live).unwrap();

        let (_second, removal) = claim_and_sweep(&work).unwrap();
        assert!(matches!(removal, Removal::OtherServiceLive), "{removal:?}");
        assert!(live.exists());
    }

    /// Only the work folder is in scope: another process that uses the
    /// candidate folder as its temporary folder keeps what it made there.
    #[test]
    fn the_candidate_folder_itself_is_never_swept() {
        let temp = tempfile::tempdir().unwrap();
        let other = tempfile::tempdir_in(temp.path()).unwrap();
        let work = prepare_work_folder(temp.path()).unwrap();
        let (_claim, removal) = claim_and_sweep(&work).unwrap();
        assert!(swept(removal).removed.is_empty());
        assert!(other.path().exists());
    }

    #[test]
    fn the_work_folder_is_private_and_a_link_in_its_place_is_refused() {
        let temp = tempfile::tempdir().unwrap();
        let work = prepare_work_folder(temp.path()).unwrap();
        assert_eq!(work, temp.path().join(WORK_FOLDER));
        set_mode(&work, 0o755);
        prepare_work_folder(temp.path()).unwrap();
        let mode = std::fs::symlink_metadata(&work)
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o700);

        let linked = tempfile::tempdir().unwrap();
        symlink(temp.path(), linked.path().join(WORK_FOLDER)).unwrap();
        assert!(prepare_work_folder(linked.path()).is_err());
    }

    /// One entry that cannot be removed is reported and the others still go.
    #[test]
    fn a_failure_is_reported_and_the_rest_are_removed() {
        if rustix::process::geteuid().is_root() {
            return; // Permission does not stop root.
        }
        let temp = tempfile::tempdir().unwrap();
        let stuck = temp.path().join(".menagerie-copy-1");
        let gone = temp.path().join(".menagerie-copy-2");
        std::fs::create_dir(&gone).unwrap();
        std::fs::create_dir(&stuck).unwrap();
        std::fs::write(stuck.join("file"), "x").unwrap();
        // The sweep makes each leftover writable but not the folder above it,
        // which then refuses to give up the two entries.
        set_mode(temp.path(), 0o500);
        let sweep = sweep_of(temp.path()).unwrap();
        set_mode(temp.path(), 0o700);
        assert_eq!(sweep.failed.len(), 2, "{sweep:?}");
        assert!(sweep.removed.is_empty(), "{sweep:?}");
        assert!(stuck.exists() && gone.exists());
        assert!(!stuck.join("file").exists(), "it removes what it can");
    }
}
