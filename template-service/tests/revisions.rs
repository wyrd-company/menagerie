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

/// Edit the commit of a session reply and send it to an answer route.
fn edited(session: &Value, commit: &str) -> Value {
    let mut session = session.clone();
    session["commit"] = json!(commit);
    session
}

#[tokio::test(flavor = "multi_thread")]
async fn a_tag_replaced_by_a_branch_still_refuses_an_edited_commit() {
    let world = World::new();
    let service = Running::start(&world).await;
    let (selector, start) = answered(&service, &world, "alpha", "sample-v1").await;
    git(&world.sample, &["tag", "-d", "v1"]);
    git(&world.sample, &["branch", "v1", &world.v3]);
    let envelope = json!({"template": start["document"]["context"]["template"], "answers": sample_answers("first")});
    let refused = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": edited(&start["session"], &world.v2), "submission": envelope}),
            ),
        )
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (409, "session-mismatch"),
        "{}",
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
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let folder = world.initialize("alpha");
    let refused = service
        .post(
            "/v1/workspaces/generate",
            &with(
                selector,
                json!({"session": edited(&done.json["session"], &world.v2)}),
            ),
        )
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (409, "session-mismatch"),
        "{}",
        refused.json
    );
    assert_eq!(entries(&folder), [".git"]);
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_moved_tag_keeps_an_unchanged_session() {
    let world = World::new();
    let service = Running::start(&world).await;
    let (selector, start) = answered(&service, &world, "alpha", "sample-v1").await;
    git(&world.sample, &["tag", "-f", "v1", &world.v2]);
    let done = service
        .post("/v1/interviews/answers", &with(selector.clone(), json!({"session": start["session"],
            "submission": {"template": start["document"]["context"]["template"], "answers": sample_answers("first")}})))
        .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let folder = world.initialize("alpha");
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    assert_eq!(application(&folder)["commit"], world.v1.as_str());
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn admissions_of_other_addresses_do_not_count() {
    let world = World::new();
    let service = Running::start(&world).await;
    let other = service
        .post("/v1/interviews", &world.select("beta", "sample-v2"))
        .await;
    assert_eq!(
        other.json["document"]["context"]["commit"],
        world.v2.as_str()
    );
    let collided = service
        .post("/v1/interviews", &world.select("gamma", "collision"))
        .await;
    let foreign = collided.json["document"]["context"]["commit"]
        .as_str()
        .unwrap()
        .to_owned();
    assert_ne!(foreign, world.v1);
    let (selector, start) = answered(&service, &world, "alpha", "sample-v1").await;
    for commit in [world.v2.as_str(), foreign.as_str()] {
        let refused = service
            .post("/v1/interviews/answers", &with(selector.clone(), json!({"session": edited(&start["session"], commit),
                "submission": {"template": start["document"]["context"]["template"], "answers": sample_answers("first")}})))
            .await;
        assert_eq!(
            (refused.status, refused.kind()),
            (409, "session-mismatch"),
            "{commit}: {}",
            refused.json
        );
    }
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_session_commit_is_never_a_path() {
    let world = World::new();
    let service = Running::start(&world).await;
    let (selector, start) = answered(&service, &world, "alpha", "sample-v1").await;
    let upper = world.v1.to_ascii_uppercase();
    for commit in [
        "../../../../templates/sample",
        "..",
        "",
        upper.as_str(),
        &world.v1[..12],
    ] {
        let refused = service
            .post("/v1/interviews/answers", &with(selector.clone(), json!({"session": edited(&start["session"], commit),
                "submission": {"template": start["document"]["context"]["template"], "answers": sample_answers("first")}})))
            .await;
        assert_eq!(
            (refused.status, refused.kind()),
            (409, "session-mismatch"),
            "{commit:?}: {}",
            refused.json
        );
    }
    service.stop().await;
}

/// With the cache cleared, a session is admitted only when the approved
/// address resolves to exactly its commit now.
#[tokio::test(flavor = "multi_thread")]
async fn a_cleared_cache_admits_only_the_current_resolution() {
    let world = World::new();
    let service = Running::start(&world).await;
    let (selector, start) = answered(&service, &world, "alpha", "sample-v1").await;
    let (moving, branch) = answered(&service, &world, "beta", "sample-main").await;
    std::fs::remove_dir_all(world.data.join("cache/sources")).unwrap();
    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);
    let submit = |session: &Value, document: &Value| {
        json!({"session": session,
        "submission": {"template": document["context"]["template"], "answers": sample_answers("first")}})
    };
    let refused = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                submit(&edited(&start["session"], &world.v2), &start["document"]),
            ),
        )
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (409, "session-mismatch"),
        "{}",
        refused.json
    );
    let done = service
        .post(
            "/v1/interviews/answers",
            &with(selector, submit(&start["session"], &start["document"])),
        )
        .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let moved = service
        .post(
            "/v1/interviews/answers",
            &with(moving, submit(&branch["session"], &branch["document"])),
        )
        .await;
    assert_eq!(
        (moved.status, moved.kind()),
        (409, "session-mismatch"),
        "a moved branch admitted its old commit: {}",
        moved.json
    );
    service.stop().await;
}

/// A cache entry admits only the commit it holds: a copy of the v1 checkout
/// under v2's name admits nothing.
#[tokio::test(flavor = "multi_thread")]
async fn a_cache_entry_must_hold_its_commit() {
    let world = World::new();
    let service = Running::start(&world).await;
    let (selector, start) = answered(&service, &world, "alpha", "sample-v1").await;
    let key = std::fs::read_dir(world.data.join("cache/sources"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let copied = std::process::Command::new("cp")
        .arg("-a")
        .arg(key.join(&world.v1))
        .arg(key.join(&world.v2))
        .status()
        .unwrap();
    assert!(copied.success());
    world.offline();
    let refused = service
        .post("/v1/interviews/answers", &with(selector, json!({"session": edited(&start["session"], &world.v2),
            "submission": {"template": start["document"]["context"]["template"], "answers": sample_answers("first")}})))
        .await;
    assert_eq!(refused.status, 422, "{}", refused.json);
    assert_eq!(
        refused.kind(),
        "template-resolve-failed",
        "a forged entry was admitted: {}",
        refused.json
    );
    service.stop().await;
}

/// An upgrade started on an installed branch keeps its admitted revision
/// after a later start refreshes the installation, without its source.
#[tokio::test(flavor = "multi_thread")]
async fn an_installed_upgrade_session_keeps_its_admission_offline() {
    let world = World::new();
    world.install(&world.address(""));
    let service = Running::start(&world).await;
    let folder = create(&world, &service, "alpha", "sample-main").await;
    git(&world.sample, &["merge", "-q", "--ff-only", "next"]);
    let selector = world.select("alpha", "sample-main");
    let upgrade = service.post("/v1/upgrades", &selector).await;
    assert_eq!(
        upgrade.json["document"]["context"]["commit"],
        world.v2.as_str(),
        "{}",
        upgrade.json
    );
    write(&world.sample, "notes.txt", "later\n");
    let later = commit_all(&world.sample, "later");
    let moved = service
        .post("/v1/interviews", &world.select("beta", "sample-main"))
        .await;
    assert_eq!(moved.json["document"]["context"]["commit"], later.as_str());
    world.offline();
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
    assert_eq!(done.json["state"], "completed", "{}", done.json);
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

/// A fork sharing a commit and a cache key with another approved template
/// cannot be admitted from that template's cache entry.
#[tokio::test(flavor = "multi_thread")]
async fn a_fork_cannot_be_admitted_from_another_sources_cache_entry() {
    let world = World::new();
    let service = Running::start(&world).await;
    let (_, start) = answered(&service, &world, "alpha", "sample-v1").await;
    assert_eq!(start["document"]["context"]["commit"], world.v1.as_str());
    let fork = service
        .post("/v1/interviews", &world.select("beta", "fork"))
        .await;
    assert_eq!(
        (fork.status, fork.kind()),
        (409, "template-cache-conflict"),
        "{}",
        fork.json
    );
    service.stop().await;

    let fresh = World::new();
    let service = Running::start(&fresh).await;
    let fork = service
        .post("/v1/interviews", &fresh.select("beta", "fork"))
        .await;
    assert_eq!(fork.json["state"], "questions", "{}", fork.json);
    service.stop().await;
}
