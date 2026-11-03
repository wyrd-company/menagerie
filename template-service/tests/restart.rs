//! The service binary on its configured socket: an interview survives a
//! service restart because the session carries all of it.
mod common;
use common::*;
use serde_json::json;
use std::process::Command;

#[tokio::test(flavor = "multi_thread")]
async fn an_interview_continues_after_the_service_restarts() {
    let world = World::new();
    let mut first = launch(&world);
    // The service makes its candidate folder, private to its user.
    use std::os::unix::fs::PermissionsExt;
    let candidates = std::fs::symlink_metadata(world.root.join("candidates")).unwrap();
    assert!(candidates.is_dir());
    assert_eq!(candidates.permissions().mode() & 0o777, 0o700);

    let (mut duplicate, line) = start(&world);
    assert!(line.contains("another process is listening"), "{line}");
    assert!(!duplicate.0.wait().unwrap().success());

    let selector = world.select("alpha", "sample-v1");
    let body = serde_json::to_vec(&selector).unwrap();
    let start = post_bytes(&world.socket, "/v1/interviews", body).await;
    assert_eq!(start.json["state"], "questions", "{}", start.json);
    first.0.kill().unwrap();
    first.0.wait().unwrap();
    assert!(world.socket.exists(), "a killed service leaves its socket");

    let mut second = launch(&world);
    let answer = with(
        selector.clone(),
        json!({"session": start.json["session"], "submission": submission(&start, sample_answers("first"))}),
    );
    let done = post_bytes(
        &world.socket,
        "/v1/interviews/answers",
        serde_json::to_vec(&answer).unwrap(),
    )
    .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let folder = world.initialize("alpha");
    let generate = with(selector, json!({"session": done.json["session"]}));
    let generated = post_bytes(
        &world.socket,
        "/v1/workspaces/generate",
        body_of("/v1/workspaces/generate", &generate),
    )
    .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    assert_eq!(application(&folder)["commit"], world.v1.as_str());

    rustix::process::kill_process(
        rustix::process::Pid::from_child(&second.0),
        rustix::process::Signal::TERM,
    )
    .unwrap();
    assert!(second.0.wait().unwrap().success());
    assert!(
        !world.socket.exists(),
        "a terminated service removes its socket"
    );
}

#[test]
fn the_service_refuses_a_path_that_is_not_a_socket() {
    let world = World::new();
    write(&world.root, "run/template.sock", "not a socket");
    let (mut refused, line) = start(&world);
    assert!(line.contains("exists and is not a socket"), "{line}");
    assert!(!refused.0.wait().unwrap().success());
    assert_eq!(read(&world.root, "run/template.sock"), "not a socket");
}

#[test]
fn the_service_reports_bad_arguments_and_configuration() {
    let usage = Command::new(BINARY).arg("-unknown").output().unwrap();
    assert_eq!(usage.status.code(), Some(2));
    assert!(String::from_utf8_lossy(&usage.stderr).contains("usage:"));
    let world = World::new();
    write(&world.root, "bad.yml", "templates: []\n");
    let bad = Command::new(BINARY)
        .arg("-config")
        .arg(world.root.join("bad.yml"))
        .output()
        .unwrap();
    assert_eq!(bad.status.code(), Some(2));
    assert!(String::from_utf8_lossy(&bad.stderr).contains("workspace-root is required"));
}

/// Wait until the upgrade's hook ran in its candidate and return the
/// candidate.
async fn held_candidate(world: &World) -> std::path::PathBuf {
    for _ in 0..3000 {
        let target = world
            .hooks
            .calls()
            .last()
            .and_then(|call| call["target"].as_str().map(std::path::PathBuf::from));
        if let Some(target) = target
            && target.starts_with(world.root.join("candidates"))
            && target.join("hook-marker.txt").exists()
        {
            return target;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    panic!("no upgrade hook ran: {:?}", world.hooks.calls());
}

#[tokio::test(flavor = "multi_thread")]
async fn a_candidate_left_by_a_killed_upgrade_is_gone_after_a_restart() {
    let world = World::new();
    let mut first = launch(&world);
    let service = Running::attach(&world);
    create(&world, &service, "alpha", "sample-v1").await;
    let selector = world.select("alpha", "sample-v2");
    let start = service.post("/v1/upgrades", &selector).await;
    let done = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
                "submission": submission(&start, json!({"extra": "added"}))}),
            ),
        )
        .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);

    world.hooks.hold_hooks(true);
    let socket = world.socket.clone();
    let apply = body_of(
        "/v1/upgrades/apply",
        &with(selector, json!({"session": done.json["session"]})),
    );
    let applying = tokio::spawn(async move {
        // The service is killed before it answers: this client's read panics
        // in its own task, and the test ignores the result.
        let _ = post_bytes(&socket, "/v1/upgrades/apply", apply).await;
    });
    let candidate = held_candidate(&world).await;
    let candidates = world.root.join("candidates");
    let work = candidates.join(".menagerie-work");
    assert_eq!(candidate.parent().unwrap(), work);

    // A second start while the service lives is refused and takes nothing
    // from the service that holds the socket.
    let (mut duplicate, line) = common::start(&world);
    assert!(line.contains("another process is listening"), "{line}");
    assert!(!duplicate.0.wait().unwrap().success());
    assert!(candidate.join("hook-marker.txt").exists());

    first.0.kill().unwrap();
    first.0.wait().unwrap();
    world.hooks.hold_hooks(false);
    let _ = applying.await;
    assert!(candidate.exists(), "a killed service leaves its candidate");

    write(&work, ".menagerie-copy-1234567/file", "copy");
    let unrelated = [
        "notes.txt",
        ".tmp",
        ".tmpkeep-me-please",
        ".menagerie-copy-notes/kept.txt",
        "folder/kept.txt",
    ];
    for name in unrelated {
        write(&work, name, "kept");
    }
    // Directly in the candidate folder a real temporary name stays too: it
    // can belong to another process.
    write(&candidates, ".tmpAb3xY9/kept.txt", "kept");

    let _second = launch(&world);
    assert!(!candidate.exists(), "the candidate stayed");
    assert_eq!(
        entries(&work),
        [
            ".menagerie-copy-notes",
            ".tmp",
            ".tmpkeep-me-please",
            "folder",
            "notes.txt"
        ]
    );
    for name in [unrelated[0], unrelated[3], unrelated[4]] {
        assert_eq!(read(&work, name), "kept");
    }
    assert_eq!(read(&candidates, ".tmpAb3xY9/kept.txt"), "kept");
}
