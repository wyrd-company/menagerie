//! The approved address decides which revision a session may use: a tag or
//! commit address fixes one revision.
mod common;
use common::*;
use serde_json::{Value, json};

async fn answered(
    service: &Running,
    world: &World,
    workspace: &str,
    template: &str,
) -> (Value, Value) {
    let selector = world.select(workspace, template);
    let start = service.post("/v1/interviews", &selector).await;
    assert_eq!(start.json["state"], "questions", "{}", start.json);
    (selector, start.json)
}

#[tokio::test(flavor = "multi_thread")]
async fn a_fixed_address_refuses_a_session_on_another_existing_revision() {
    let world = World::new();
    let service = Running::start(&world).await;
    for template in ["sample-v1", "sample-commit"] {
        let (selector, start) = answered(&service, &world, "alpha", template).await;
        let envelope = json!({"template": start["document"]["context"]["template"], "answers": sample_answers("first")});
        let mut moved = start["session"].clone();
        moved["commit"] = json!(world.v2);
        let refused = service
            .post(
                "/v1/interviews/answers",
                &with(
                    selector.clone(),
                    json!({"session": moved, "submission": envelope}),
                ),
            )
            .await;
        assert_eq!(
            (refused.status, refused.kind()),
            (409, "session-mismatch"),
            "{template}: {}",
            refused.json
        );

        let done = service
            .post(
                "/v1/interviews/answers",
                &with(
                    selector.clone(),
                    json!({"session": start["session"], "submission": envelope}),
                ),
            )
            .await;
        assert_eq!(done.json["state"], "completed", "{template}: {}", done.json);
        let folder = world.initialize("alpha");
        let mut moved = done.json["session"].clone();
        moved["commit"] = json!(world.v2);
        let refused = service
            .post(
                "/v1/workspaces/generate",
                &with(selector.clone(), json!({"session": moved})),
            )
            .await;
        assert_eq!(
            (refused.status, refused.kind()),
            (409, "session-mismatch"),
            "{template}: {}",
            refused.json
        );
        assert_eq!(
            entries(&folder),
            [".git"],
            "{template}: a refused session wrote files"
        );
        std::fs::remove_dir_all(&folder).unwrap();
    }
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_fixed_upgrade_address_refuses_a_session_on_another_revision() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-v1").await;
    let selector = world.select("alpha", "sample-v2");
    let start = service.post("/v1/upgrades", &selector).await;
    let mut moved = start.json["session"].clone();
    moved["commit"] = json!(world.v3);
    let before = git(&folder, &["rev-parse", "HEAD"]);
    let refused = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": moved,
            "submission": submission(&start, json!({"extra": "added"}))}),
            ),
        )
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (409, "session-mismatch"),
        "{}",
        refused.json
    );
    let refused = service
        .post(
            "/v1/upgrades/apply",
            &with(selector, json!({"session": moved})),
        )
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (409, "session-mismatch"),
        "{}",
        refused.json
    );
    assert_eq!(git(&folder, &["rev-parse", "HEAD"]), before);
    assert_eq!(git(&folder, &["status", "--porcelain"]), "");
    service.stop().await;
}
