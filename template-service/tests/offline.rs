//! Admitted sessions continue on every resume route after a restart of the
//! service binary while their template repositories are unreachable.
mod common;
use common::*;
use serde_json::{Value, json};

async fn post(world: &World, path: &str, body: &Value) -> Reply {
    post_bytes(&world.socket, path, serde_json::to_vec(body).unwrap()).await
}

#[tokio::test(flavor = "multi_thread")]
async fn every_resume_route_works_offline_after_a_restart() {
    let world = World::new();
    world.install(&world.address(""));
    let service = launch(&world);
    // Online: a created workspace, a pending upgrade, a pending create, and a
    // pending create on an installed branch that then moves.
    let running = Running::attach(&world);
    let folder = create(&world, &running, "alpha", "sample-v1").await;
    let upgrade_selector = world.select("alpha", "sample-v2");
    let upgrade = post(&world, "/v1/upgrades", &upgrade_selector).await;
    assert_eq!(upgrade.json["state"], "questions", "{}", upgrade.json);
    let create_selector = world.select("beta", "sample-v1");
    let pending = post(&world, "/v1/interviews", &create_selector).await;
    let branch_selector = world.select("gamma", "sample-main");
    let branch = post(&world, "/v1/interviews", &branch_selector).await;
    assert_eq!(
        branch.json["document"]["context"]["commit"],
        world.v1.as_str()
    );
    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);
    let moved = post(
        &world,
        "/v1/interviews",
        &world.select("delta", "sample-main"),
    )
    .await;
    assert_eq!(
        moved.json["document"]["context"]["commit"],
        world.v2.as_str()
    );
    drop(service);

    world.offline();
    let _service = launch(&world);
    for (selector, reply) in [(&create_selector, &pending), (&branch_selector, &branch)] {
        let done = post(
            &world,
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": reply.json["session"],
            "submission": submission(reply, sample_answers("first"))}),
            ),
        )
        .await;
        assert_eq!(done.json["state"], "completed", "{}", done.json);
        let workspace = selector["workspace"].as_str().unwrap();
        let target = world.initialize(workspace);
        let generated = post(
            &world,
            "/v1/workspaces/generate",
            &with(selector.clone(), json!({"session": done.json["session"]})),
        )
        .await;
        assert_eq!(
            generated.json["outcome"], "written",
            "{workspace}: {}",
            generated.json
        );
        assert_eq!(application(&target)["commit"], world.v1.as_str());
    }
    let done = post(
        &world,
        "/v1/upgrades/answers",
        &with(
            upgrade_selector.clone(),
            json!({"session": upgrade.json["session"],
        "submission": submission(&upgrade, json!({"extra": "added"}))}),
        ),
    )
    .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let applied = post(
        &world,
        "/v1/upgrades/apply",
        &with(upgrade_selector, json!({"session": done.json["session"]})),
    )
    .await;
    assert_eq!(applied.json["outcome"], "written", "{}", applied.json);
    assert_eq!(application(&folder)["commit"], world.v2.as_str());
}
