//! Template hooks and generation failures, against Toha.
mod common;
use common::*;
use serde_json::json;

#[tokio::test(flavor = "multi_thread")]
async fn a_failing_hook_fails_generation_without_a_record() {
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "broken");
    let start = service.post("/v1/interviews", &selector).await;
    let first = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, json!({"label": "one"}))}),
            ),
        )
        .await;
    let done = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": first.json["session"],
            "submission": submission(&first, json!({"second": "two"}))}),
            ),
        )
        .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let folder = world.initialize("alpha");
    let failed = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(
        (failed.status, failed.kind()),
        (422, "hook-failed"),
        "{}",
        failed.json
    );
    assert!(
        !folder.join(".toha.json").exists(),
        "a failed generation wrote the record"
    );
    // Toha leaves the files written before the hook; the folder is not new now.
    assert_eq!(entries(&folder), [".git", "out.txt", "sub"]);
    let retry = service
        .post(
            "/v1/workspaces/generate",
            &with(
                world.select("alpha", "broken"),
                json!({"session": done.json["session"]}),
            ),
        )
        .await;
    assert_eq!((retry.status, retry.kind()), (409, "target-not-new"));
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unwritable_workspace_fails_with_an_io_error_and_no_record() {
    if menagerie_template_service::host::is_admin() {
        eprintln!("skipped: an administrator ignores folder permissions");
        return;
    }
    use std::os::unix::fs::PermissionsExt;
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "sample-v1");
    let start = service.post("/v1/interviews", &selector).await;
    let done = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, sample_answers("first"))}),
            ),
        )
        .await;
    let folder = world.initialize("alpha");
    std::fs::set_permissions(&folder, std::fs::Permissions::from_mode(0o555)).unwrap();
    let failed = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    std::fs::set_permissions(&folder, std::fs::Permissions::from_mode(0o755)).unwrap();
    assert_eq!(
        (failed.status, failed.kind()),
        (500, "io-failed"),
        "{}",
        failed.json
    );
    assert_eq!(entries(&folder), [".git"]);
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_hook_runs_in_its_target_relative_folder() {
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "flows");
    let start = service.post("/v1/interviews", &selector).await;
    let first = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, json!({"label": "one"}))}),
            ),
        )
        .await;
    let done = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": first.json["session"],
            "submission": submission(&first, json!({"second": "two"}))}),
            ),
        )
        .await;
    let folder = world.initialize("alpha");
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    assert_eq!(read(&folder, "sub/hook-marker.txt"), "ran\n");
    // The hook went to the workspace service with the request's grant, the
    // folder as configured and its target-relative folder.
    assert_eq!(
        world.hooks.calls(),
        [json!({"token": HOOK_TOKEN, "target": folder,
            "program": {"run": ["sh", "-c", "echo ran > hook-marker.txt"]}, "cwd": "sub",
            "templateRoot": world.hooks.calls()[0]["templateRoot"],
            "capture": {"stdout": false, "stderr": false}})]
    );
    assert!(
        read(&folder, "out.txt").ends_with(&format!("host: {}\n", hostname())),
        "the fixed environment was not captured"
    );
    assert!(!folder.join("hook-marker.txt").exists());
    service.stop().await;
}

/// A generate whose request carries no hook token, or one the workspace
/// service refuses, runs no hook and writes no applied record.
#[tokio::test(flavor = "multi_thread")]
async fn a_hook_without_a_granted_token_runs_nowhere() {
    for (token, needle) in [
        (json!(null), "no hook token"),
        (json!("not-granted"), "(hook-token-refused)"),
    ] {
        let world = World::new();
        let service = Running::start(&world).await;
        let selector = world.select("alpha", "flows");
        let start = service.post("/v1/interviews", &selector).await;
        let first = service
            .post(
                "/v1/interviews/answers",
                &with(
                    selector.clone(),
                    json!({"session": start.json["session"],
                "submission": submission(&start, json!({"label": "one"}))}),
                ),
            )
            .await;
        let done = service
            .post(
                "/v1/interviews/answers",
                &with(
                    selector.clone(),
                    json!({"session": first.json["session"],
                "submission": submission(&first, json!({"second": "two"}))}),
                ),
            )
            .await;
        let folder = world.initialize("alpha");
        let failed = service
            .post(
                "/v1/workspaces/generate",
                &with(
                    selector,
                    json!({"session": done.json["session"], "hookToken": token}),
                ),
            )
            .await;
        assert_eq!(
            (failed.status, failed.kind()),
            (500, "hook-io-failed"),
            "{}",
            failed.json
        );
        let message = failed.json["error"]["message"].as_str().unwrap();
        assert!(message.contains(needle), "{message}");
        assert!(!folder.join("sub/hook-marker.txt").exists());
        assert!(!folder.join(".toha.json").exists());
        assert!(world.hooks.calls().is_empty());
        service.stop().await;
    }
}
