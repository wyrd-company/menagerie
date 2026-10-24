//! The service binary on its configured socket: an interview survives a
//! service restart because the session carries all of it.
mod common;
use common::*;
use serde_json::json;
use std::io::{BufRead, BufReader};
use std::process::{Child, Command, Stdio};

const BINARY: &str = env!("CARGO_BIN_EXE_menagerie-template-service");

/// Start the binary and wait for its own readiness line.
fn launch(world: &World) -> Child {
    let mut child = Command::new(BINARY)
        .arg("-config")
        .arg(&world.config)
        .arg("-data")
        .arg(&world.data)
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let mut line = String::new();
    BufReader::new(child.stderr.as_mut().unwrap())
        .read_line(&mut line)
        .unwrap();
    assert_eq!(
        line.trim(),
        format!("template service listening on {}", world.socket.display())
    );
    child
}

#[tokio::test(flavor = "multi_thread")]
async fn an_interview_continues_after_the_service_restarts() {
    let world = World::new();
    let mut first = launch(&world);

    let duplicate = Command::new(BINARY)
        .arg("-config")
        .arg(&world.config)
        .arg("-data")
        .arg(&world.data)
        .output()
        .unwrap();
    assert!(!duplicate.status.success());
    assert!(String::from_utf8_lossy(&duplicate.stderr).contains("another process is listening"));

    let selector = world.select("alpha", "sample-v1");
    let body = serde_json::to_vec(&selector).unwrap();
    let start = post_bytes(&world.socket, "/v1/interviews", body).await;
    assert_eq!(start.json["state"], "questions", "{}", start.json);
    first.kill().unwrap();
    first.wait().unwrap();
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
        serde_json::to_vec(&generate).unwrap(),
    )
    .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    assert_eq!(application(&folder)["commit"], world.v1.as_str());

    rustix::process::kill_process(
        rustix::process::Pid::from_child(&second),
        rustix::process::Signal::TERM,
    )
    .unwrap();
    assert!(second.wait().unwrap().success());
    assert!(
        !world.socket.exists(),
        "a terminated service removes its socket"
    );
}

#[test]
fn the_service_refuses_a_path_that_is_not_a_socket() {
    let world = World::new();
    write(&world.root, "run/template.sock", "not a socket");
    let output = Command::new(BINARY)
        .arg("-config")
        .arg(&world.config)
        .arg("-data")
        .arg(&world.data)
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("exists and is not a socket"));
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
