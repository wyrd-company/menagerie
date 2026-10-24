//! Create interviews and generation through the Unix socket, against Toha.
mod common;
use common::*;
use serde_json::{Value, json};

#[tokio::test(flavor = "multi_thread")]
async fn create_generates_into_the_initialized_workspace_and_writes_the_record() {
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "sample-v1");

    let start = service.post("/v1/interviews", &selector).await;
    assert_eq!(start.status, 200);
    assert_eq!(start.content_type.as_deref(), Some("application/json"));
    assert_eq!(start.json["state"], "questions");
    assert_eq!(questions(&start), ["catalog", "label", "note"]);
    assert_eq!(start.json["document"]["status"], "questions");
    assert_eq!(
        start.json["document"]["context"]["commit"],
        world.v1.as_str()
    );
    assert!(start.json["document"].get("errors").is_none());
    assert!(!world.folder("alpha").exists(), "start created the folder");

    let rejected = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
                "submission": submission(&start, json!({"label": 42}))}),
            ),
        )
        .await;
    assert_eq!(rejected.status, 200);
    assert_eq!(rejected.json["state"], "questions");
    assert!(
        rejected.json["document"]["errors"]["label"].is_array(),
        "{}",
        rejected.json
    );
    assert_eq!(
        rejected.session_bytes(),
        start.session_bytes(),
        "rejected batch changed the session"
    );

    let done = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": rejected.json["session"],
                "submission": submission(&start, sample_answers("first"))}),
            ),
        )
        .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    assert_eq!(done.json["document"], Value::Null);

    let again = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": done.json["session"],
                "submission": submission(&start, json!({}))}),
            ),
        )
        .await;
    assert_eq!((again.status, again.kind()), (409, "interview-complete"));

    let early = service
        .post(
            "/v1/workspaces/generate",
            &with(selector.clone(), json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!((early.status, early.kind()), (409, "target-not-new"));
    assert!(!world.folder("alpha").exists());

    let folder = world.initialize("alpha");
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(generated.status, 200, "{}", generated.json);
    assert_eq!(generated.json["outcome"], "written");
    assert_eq!(generated.json["recordPath"], ".toha.json");
    assert_eq!(
        generated.json["files"],
        json!([{"path": "local.txt", "action": "create"}, {"path": "result.txt", "action": "create"}])
    );
    assert_eq!(
        generated.json["hooks"],
        json!([{"id": null, "exitCode": 0, "success": true}])
    );
    assert_eq!(generated.json["conflicts"], json!([]));

    assert_eq!(
        read(&folder, "hook-marker.txt"),
        "ran\n",
        "hook did not run in the target"
    );
    assert!(read(&folder, "result.txt").starts_with("label: first\nnote: \ntitle: first\n"));
    let application = application(&folder);
    assert_eq!(application["commit"], world.v1.as_str());
    assert_eq!(application["answers"]["note"], Value::Null);
    let record = read(&folder, ".toha.json");
    assert!(
        record.contains("\"size\": 18446744073709551615"),
        "{record}"
    );
    assert!(
        record.contains("\"offset\": -9223372036854775808"),
        "{record}"
    );
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn equivalent_numbers_keep_their_value_through_the_session_and_record() {
    let world = World::new();
    let service = Running::start(&world).await;
    let start = service
        .post("/v1/interviews", &world.select("alpha", "sample-v1"))
        .await;
    let session = serde_json::to_string(&start.json["session"]).unwrap();
    let template = start.json["document"]["context"]["template"]
        .as_str()
        .unwrap();
    let target = world.folder("alpha");
    let body = |size: &str, ratio: &str| {
        format!(
            r#"{{"workspace":"alpha","target":"{}","templateId":"sample-v1","session":{session},"submission":{{"template":"{template}","answers":{{"label":"first","note":null,"catalog":{{"$schema":"{CATALOG}","title":"first","size":{size},"ratio":{ratio},"offset":-0}}}}}}}}"#,
            target.display()
        )
    };

    let refused = post_bytes(
        &world.socket,
        "/v1/interviews/answers",
        body("123456789012345678901234567890", "1.0").into_bytes(),
    )
    .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (400, "number-not-representable")
    );
    assert_eq!(
        refused.json["error"]["locations"],
        json!(["/submission/answers/catalog/size"])
    );
    assert_eq!(refused.json["error"]["questions"], json!(["catalog"]));
    let tiny = post_bytes(
        &world.socket,
        "/v1/interviews/answers",
        body("1", "1e-400").into_bytes(),
    )
    .await;
    assert_eq!(
        tiny.json["error"]["locations"],
        json!(["/submission/answers/catalog/ratio"])
    );

    let done = post_bytes(
        &world.socket,
        "/v1/interviews/answers",
        body("1e2", "1.50").into_bytes(),
    )
    .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let catalog = &done.json["session"]["submissions"][0]["catalog"];
    assert_eq!(catalog["size"].as_f64(), Some(100.0));
    assert_eq!(catalog["ratio"].as_f64(), Some(1.5));

    // The returned session crosses the next request byte for byte.
    let reply = service
        .post(
            "/v1/interviews/answers",
            &with(
                world.select("alpha", "sample-v1"),
                json!({
            "session": done.json["session"], "submission": submission(&start, json!({}))}),
            ),
        )
        .await;
    assert_eq!(reply.kind(), "interview-complete");
    let folder = world.initialize("alpha");
    let generated = post_bytes(
        &world.socket,
        "/v1/workspaces/generate",
        format!(
            r#"{{"workspace":"alpha","target":"{}","templateId":"sample-v1","session":{}}}"#,
            target.display(),
            String::from_utf8(done.session_bytes()).unwrap()
        )
        .into_bytes(),
    )
    .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    assert_eq!(
        application(&folder)["answers"]["catalog"]["size"].as_f64(),
        Some(100.0)
    );
    assert!(read(&folder, "result.txt").contains("size: 100.0\nratio: 1.5\n"));
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_tampered_session_number_is_refused_before_any_write() {
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
    let session = String::from_utf8(done.session_bytes())
        .unwrap()
        .replace("18446744073709551615", "18446744073709551616");
    let body = format!(
        r#"{{"workspace":"alpha","target":"{}","templateId":"sample-v1","session":{session}}}"#,
        folder.display()
    );
    let refused = post_bytes(&world.socket, "/v1/workspaces/generate", body.into_bytes()).await;
    assert_eq!(
        (refused.status, refused.kind()),
        (400, "number-not-representable")
    );
    assert_eq!(
        refused.json["error"]["locations"],
        json!(["/session/submissions/0/catalog/size"])
    );
    assert_eq!(
        entries(&folder),
        [".git"],
        "refusal wrote into the workspace"
    );
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_interview_keeps_its_revision_and_a_new_one_resolves_again() {
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "sample-main");
    let start = service.post("/v1/interviews", &selector).await;
    assert_eq!(
        start.json["document"]["context"]["commit"],
        world.v1.as_str()
    );

    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);
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
    assert_eq!(
        done.json["state"], "completed",
        "resume asked the newer revision: {}",
        done.json
    );
    let folder = world.initialize("alpha");
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(generated.json["outcome"], "written");
    assert_eq!(application(&folder)["commit"], world.v1.as_str());

    let fresh = service
        .post("/v1/interviews", &world.select("beta", "sample-main"))
        .await;
    assert_eq!(
        fresh.json["document"]["context"]["commit"],
        world.v2.as_str()
    );
    assert_eq!(questions(&fresh), ["catalog", "extra", "label", "note"]);
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_flow_stop_or_abort_ends_without_writes() {
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "flows");
    let start = service.post("/v1/interviews", &selector).await;
    let folder = world.initialize("alpha");

    let stopped = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, json!({"label": "stop"}))}),
            ),
        )
        .await;
    assert_eq!(stopped.json["state"], "ended", "{}", stopped.json);
    assert_eq!(stopped.json["document"]["status"], "ended");
    assert_eq!(stopped.json["document"]["kind"], "stop");
    let applied = service
        .post(
            "/v1/workspaces/generate",
            &with(
                selector.clone(),
                json!({"session": stopped.json["session"]}),
            ),
        )
        .await;
    assert_eq!((applied.status, applied.kind()), (409, "interview-ended"));

    let aborted = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, json!({"label": "cancel"}))}),
            ),
        )
        .await;
    assert_eq!(aborted.json["state"], "ended");
    assert_eq!(aborted.json["document"]["kind"], "abort");
    assert_eq!(
        aborted.session_bytes(),
        start.session_bytes(),
        "an abort recorded its batch"
    );
    let applied = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": aborted.json["session"]})),
        )
        .await;
    assert_eq!(
        (applied.status, applied.kind()),
        (409, "interview-not-complete")
    );
    assert_eq!(entries(&folder), [".git"]);
    service.stop().await;
}

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
async fn generation_refuses_a_folder_that_is_not_a_fresh_repository() {
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
    let body = with(selector.clone(), json!({"session": done.json["session"]}));
    let generate = || service.post("/v1/workspaces/generate", &body);

    let folder = world.folder("alpha");
    std::fs::create_dir_all(&folder).unwrap();
    let not_git = generate().await;
    assert_eq!((not_git.status, not_git.kind()), (409, "not-git"));
    assert!(entries(&folder).is_empty());

    git(&folder, &["init", "-q"]);
    write(&folder, "existing.txt", "kept\n");
    let occupied = generate().await;
    assert_eq!((occupied.status, occupied.kind()), (409, "target-not-new"));
    assert!(
        occupied.json["error"]["message"]
            .as_str()
            .unwrap()
            .contains("existing.txt")
    );
    assert_eq!(entries(&folder), [".git", "existing.txt"]);

    let exists = service.post("/v1/interviews", &selector).await;
    assert_eq!((exists.status, exists.kind()), (409, "workspace-exists"));
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
    assert!(!folder.join("hook-marker.txt").exists());
    service.stop().await;
}
