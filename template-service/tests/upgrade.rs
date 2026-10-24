//! Upgrades through the Unix socket, against Toha and two real template versions.
mod common;
use common::*;
use serde_json::{Value, json};

fn head(folder: &std::path::Path) -> (String, String) {
    (
        git(folder, &["rev-parse", "HEAD"]),
        git(folder, &["status", "--porcelain"]),
    )
}

#[tokio::test(flavor = "multi_thread")]
async fn an_upgrade_asks_only_the_new_question_and_keeps_local_edits() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    let local = read(&folder, "local.txt").replace("third: stable", "third: operator");
    write(&folder, "local.txt", local);
    write(
        &folder,
        "catalog.json",
        json!({"$schema": CATALOG, "title": "second"}).to_string(),
    );
    commit_all(&folder, "operator edits");

    let selector = world.select("alpha", "sample-v2");
    let start = service.post("/v1/upgrades", &selector).await;
    assert_eq!(start.status, 200, "{}", start.json);
    assert_eq!(start.json["state"], "questions");
    assert_eq!(
        questions(&start),
        ["extra"],
        "recorded answers were asked again"
    );
    assert_eq!(
        start.json["document"]["schema"]["required"],
        json!(["extra"])
    );
    assert_eq!(
        start.json["document"]["context"]["commit"],
        world.v2.as_str()
    );
    let admitted = &start.json["session"]["submissions"][0]["catalog"];
    assert_eq!(
        admitted["title"], "second",
        "the fresh document was not admitted"
    );

    // A later document change does not reach this session's admitted document.
    write(
        &folder,
        "catalog.json",
        json!({"$schema": CATALOG, "title": "third"}).to_string(),
    );
    commit_all(&folder, "later document");

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
    let again = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": done.json["session"],
            "submission": submission(&start, json!({}))}),
            ),
        )
        .await;
    assert_eq!((again.status, again.kind()), (409, "interview-complete"));

    let applied = service
        .post(
            "/v1/upgrades/apply",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(applied.status, 200, "{}", applied.json);
    assert_eq!(applied.json["outcome"], "written");
    assert_eq!(applied.json["recordPath"], ".toha.json");
    assert_eq!(applied.json["conflicts"], json!([]));
    assert_eq!(
        applied.json["hooks"],
        json!([{"id": null, "exitCode": 0, "success": true}])
    );
    let files = applied.json["files"].as_array().unwrap();
    assert!(
        files.contains(&json!({"path": "result.txt", "action": "merged"})),
        "{files:?}"
    );
    assert!(
        files.contains(&json!({"path": "local.txt", "action": "merged"})),
        "{files:?}"
    );

    let local = read(&folder, "local.txt");
    assert!(
        local.contains("third: operator"),
        "local edit lost: {local}"
    );
    assert!(
        local.contains("fifth: new"),
        "template change missing: {local}"
    );
    let result = read(&folder, "result.txt");
    assert!(
        result.contains("title: second\n"),
        "admitted document not kept: {result}"
    );
    assert!(result.contains("extra: added\n"), "{result}");
    let application = application(&folder);
    assert_eq!(application["commit"], world.v2.as_str());
    assert_eq!(application["answers"]["extra"], "added");
    assert_eq!(application["answers"]["label"], "first");
    assert!(
        application["answers"]
            .as_object()
            .unwrap()
            .contains_key("note")
            && application["answers"]["note"] == Value::Null,
        "the optional null was not replayed: {application}"
    );
    assert_eq!(application["answers"]["catalog"]["title"], "second");
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_recorded_answer_the_new_version_rejects_is_asked_with_its_error_and_nothing_is_written()
{
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    let before = head(&folder);
    let selector = world.select("alpha", "sample-v3");

    let start = service.post("/v1/upgrades", &selector).await;
    assert_eq!(start.json["state"], "questions", "{}", start.json);
    assert_eq!(questions(&start), ["label"]);
    assert!(
        start.json["document"]["errors"]["label"].is_array(),
        "{}",
        start.json
    );
    assert_eq!(
        head(&folder),
        before,
        "upgrade start wrote into the workspace"
    );

    let rejected = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, json!({"label": "still-letters"}))}),
            ),
        )
        .await;
    assert_eq!(rejected.json["state"], "questions");
    assert!(rejected.json["document"]["errors"]["label"].is_array());
    assert_eq!(
        rejected.session_bytes(),
        start.session_bytes(),
        "rejected batch changed the session"
    );
    let rebuilt = service
        .post(
            "/v1/upgrades/apply",
            &with(
                selector.clone(),
                json!({"session": rejected.json["session"]}),
            ),
        )
        .await;
    assert_eq!(
        (rebuilt.status, rebuilt.kind()),
        (409, "interview-not-complete")
    );
    assert_eq!(head(&folder), before);

    let done = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": rejected.json["session"],
            "submission": submission(&start, json!({"label": "123"}))}),
            ),
        )
        .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let applied = service
        .post(
            "/v1/upgrades/apply",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(applied.json["outcome"], "written", "{}", applied.json);
    assert_eq!(application(&folder)["answers"]["label"], "123");
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_stale_application_or_dirty_workspace_is_refused() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    let selector = world.select("alpha", "sample-v2");
    let first = service.post("/v1/upgrades", &selector).await;
    let second = service.post("/v1/upgrades", &selector).await;
    let done = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": second.json["session"],
            "submission": submission(&second, json!({"extra": "one"}))}),
            ),
        )
        .await;
    let applied = service
        .post(
            "/v1/upgrades/apply",
            &with(selector.clone(), json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(applied.json["outcome"], "written");
    commit_all(&folder, "upgraded");
    let before = head(&folder);

    let stale = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": first.json["session"],
            "submission": submission(&first, json!({"extra": "two"}))}),
            ),
        )
        .await;
    assert_eq!(
        (stale.status, stale.kind()),
        (409, "stale-application"),
        "{}",
        stale.json
    );
    assert_eq!(head(&folder), before);

    let third = service
        .post("/v1/upgrades", &world.select("alpha", "sample-v1"))
        .await;
    assert_eq!(third.json["state"], "completed", "{}", third.json);
    write(&folder, "result.txt", "uncommitted\n");
    let dirty = service.post("/v1/upgrades", &selector).await;
    assert_eq!((dirty.status, dirty.kind()), (409, "dirty-target"));
    let resumed = service
        .post(
            "/v1/upgrades/apply",
            &with(
                world.select("alpha", "sample-v1"),
                json!({"session": third.json["session"]}),
            ),
        )
        .await;
    assert_eq!((resumed.status, resumed.kind()), (409, "dirty-target"));
    assert_eq!(read(&folder, "result.txt"), "uncommitted\n");
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_invalid_document_file_refuses_the_upgrade_without_writes() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    write(
        &folder,
        "catalog.json",
        json!({"$schema": CATALOG, "title": 42}).to_string(),
    );
    commit_all(&folder, "invalid document");
    let before = head(&folder);
    let refused = service
        .post("/v1/upgrades", &world.select("alpha", "sample-v2"))
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (422, "document-invalid"),
        "{}",
        refused.json
    );
    assert_eq!(head(&folder), before);
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_upgrade_needs_an_existing_workspace_of_the_same_template() {
    let world = World::new();
    let service = Running::start(&world).await;
    let missing = service
        .post("/v1/upgrades", &world.select("alpha", "sample-v2"))
        .await;
    assert_eq!(
        (missing.status, missing.kind()),
        (404, "workspace-not-found")
    );
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    let before = head(&folder);
    let other = service
        .post("/v1/upgrades", &world.select("alpha", "flows"))
        .await;
    assert_eq!(
        (other.status, other.kind()),
        (422, "update-refused"),
        "{}",
        other.json
    );
    assert_eq!(head(&folder), before);
    service.stop().await;
}

/// Measured limit, not a contract: Toha reads a workspace document file itself
/// at upgrade start, so the service cannot refuse a number in it that Toha's
/// number model changes. This test fails when Toha starts keeping it exactly.
#[tokio::test(flavor = "multi_thread")]
async fn toha_converts_a_large_number_in_a_workspace_document_file() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    write(
        &folder,
        "catalog.json",
        format!(
            r#"{{"$schema":"{CATALOG}","title":"second","size":123456789012345678901234567890}}"#
        ),
    );
    commit_all(&folder, "large number");
    let start = service
        .post("/v1/upgrades", &world.select("alpha", "sample-v2"))
        .await;
    assert_eq!(start.json["state"], "questions", "{}", start.json);
    let text = String::from_utf8(start.session_bytes()).unwrap();
    assert!(text.contains("\"size\":1.2345678901234568e+29"), "{text}");
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_overlapping_local_edit_is_reported_as_conflicted() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    let local = read(&folder, "local.txt").replace("fifth: original", "fifth: operator");
    write(&folder, "local.txt", local);
    commit_all(&folder, "overlapping edit");
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
    let applied = service
        .post(
            "/v1/upgrades/apply",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(applied.status, 200, "{}", applied.json);
    assert_eq!(applied.json["outcome"], "conflicted");
    assert_eq!(applied.json["conflicts"], json!(["local.txt"]));
    assert!(
        applied.json["files"]
            .as_array()
            .unwrap()
            .contains(&json!({"path": "local.txt", "action": "conflicted"}))
    );
    assert_eq!(applied.json["recordPath"], ".toha.json");
    assert!(read(&folder, "local.txt").contains("<<<<<<<"));
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_upgrade_to_the_recorded_version_is_already_current() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    let before = head(&folder);
    let selector = world.select("alpha", "sample-v1");
    let start = service.post("/v1/upgrades", &selector).await;
    assert_eq!(start.json["state"], "completed", "{}", start.json);
    assert_eq!(start.json["document"], Value::Null);
    let applied = service
        .post(
            "/v1/upgrades/apply",
            &with(selector, json!({"session": start.json["session"]})),
        )
        .await;
    assert_eq!(
        applied.json["outcome"], "already-current",
        "{}",
        applied.json
    );
    assert_eq!(applied.json["recordPath"], Value::Null);
    assert_eq!(head(&folder), before);
    service.stop().await;
}
