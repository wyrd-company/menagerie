//! The approved address decides which revision a session may use: a tag or
//! commit address fixes one revision, a branch address moves only when an
//! operation starts.
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

#[tokio::test(flavor = "multi_thread")]
async fn an_installed_branch_moves_when_a_create_starts_and_never_on_resume() {
    let world = World::new();
    world.install(&world.address(""));
    let service = Running::start(&world).await;
    let (selector, pending) = answered(&service, &world, "gamma", "sample-main").await;
    assert_eq!(pending["document"]["context"]["commit"], world.v1.as_str());

    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);

    let resumed = service
        .post("/v1/interviews/answers", &with(selector, json!({"session": pending["session"],
            "submission": {"template": pending["document"]["context"]["template"], "answers": sample_answers("first")}})))
        .await;
    assert_eq!(
        resumed.json["state"], "completed",
        "resume moved to the newer revision: {}",
        resumed.json
    );
    assert_eq!(resumed.json["session"]["commit"], world.v1.as_str());

    let fresh = service
        .post("/v1/interviews", &world.select("beta", "sample-main"))
        .await;
    assert_eq!(
        fresh.json["document"]["context"]["commit"],
        world.v2.as_str(),
        "{}",
        fresh.json
    );
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_installed_branch_moves_when_an_upgrade_starts() {
    let world = World::new();
    world.install(&world.address(""));
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-main").await;
    assert_eq!(application(&folder)["commit"], world.v1.as_str());

    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);

    let selector = world.select("alpha", "sample-main");
    let upgrade = service.post("/v1/upgrades", &selector).await;
    assert_eq!(upgrade.json["state"], "questions", "{}", upgrade.json);
    assert_eq!(questions(&upgrade), ["extra"]);
    assert_eq!(
        upgrade.json["document"]["context"]["commit"],
        world.v2.as_str()
    );
    let done = service
        .post(
            "/v1/upgrades/answers",
            &with(
                selector.clone(),
                json!({"session": upgrade.json["session"],
            "submission": submission(&upgrade, json!({"extra": "added"}))}),
            ),
        )
        .await;
    let applied = service
        .post(
            "/v1/upgrades/apply",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(applied.json["outcome"], "written", "{}", applied.json);
    assert_eq!(application(&folder)["commit"], world.v2.as_str());
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_installed_tag_stays_on_its_revision() {
    let world = World::new();
    world.install(&world.address("@v1"));
    let service = Running::start(&world).await;
    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);
    // Moving the tag upstream does not move the installed revision.
    git(&world.sample, &["tag", "-f", "v1", &world.v2]);
    let selector = world.select("alpha", "sample-v1");
    let start = service.post("/v1/interviews", &selector).await;
    assert_eq!(
        start.json["document"]["context"]["commit"],
        world.v1.as_str(),
        "{}",
        start.json
    );
    let envelope = submission(&start, sample_answers("first"));
    let mut moved = start.json["session"].clone();
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
        "{}",
        refused.json
    );
    let kept = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector,
                json!({"session": start.json["session"], "submission": envelope}),
            ),
        )
        .await;
    assert_eq!(kept.json["state"], "completed", "{}", kept.json);
    service.stop().await;
}

/// Toha's catalog updates only Git templates installed in the user layer;
/// other installations are used as they are.
#[tokio::test(flavor = "multi_thread")]
async fn installations_toha_does_not_update_are_used_as_they_are() {
    let world = World::new();
    world.install(&world.address(""));
    let user = world.data.join("data/templates.yml");
    let system = world.data.join("system/data/templates.yml");
    std::fs::create_dir_all(system.parent().unwrap()).unwrap();
    std::fs::rename(&user, &system).unwrap();
    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);
    let service = Running::start(&world).await;
    let start = service
        .post("/v1/interviews", &world.select("alpha", "sample-main"))
        .await;
    assert_eq!(start.status, 200, "{}", start.json);
    assert_eq!(
        start.json["document"]["context"]["commit"],
        world.v1.as_str()
    );

    world.install(&format!("{}/templates/flows", world.root.display()));
    let folder = service
        .post("/v1/interviews", &world.select("beta", "flows-folder"))
        .await;
    assert_eq!(folder.status, 200, "{}", folder.json);
    assert_eq!(folder.json["state"], "questions");
    service.stop().await;
}
