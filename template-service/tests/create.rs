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
