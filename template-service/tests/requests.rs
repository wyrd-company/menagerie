//! Request checks every route makes before Toha replays or writes anything.
mod common;
use common::*;
use menagerie_template_service::server::ROUTES;
use serde_json::{Value, json};

#[tokio::test(flavor = "multi_thread")]
async fn selectors_name_an_approved_template_and_a_workspace_under_the_root() {
    let world = World::new();
    let service = Running::start(&world).await;
    for route in ROUTES {
        let unknown = service
            .post(route, &world.select("alpha", "unlisted"))
            .await;
        let expected = if route.ends_with("interviews") || route.ends_with("upgrades") {
            (404, "unknown-template")
        } else {
            (400, "malformed-request")
        };
        assert_eq!((unknown.status, unknown.kind()), expected, "{route}");
    }
    for name in ["", ".", "..", "a/b", "../alpha"] {
        let body = json!({"workspace": name, "target": world.workspaces.join(name), "templateId": "sample-v1"});
        let refused = service.post("/v1/interviews", &body).await;
        assert_eq!(
            (refused.status, refused.kind()),
            (400, "invalid-workspace"),
            "{name:?}"
        );
    }
    for target in [
        world.root.join("alpha"),
        world.workspaces.join("beta"),
        "alpha".into(),
    ] {
        let body = json!({"workspace": "alpha", "target": target, "templateId": "sample-v1"});
        let refused = service.post("/v1/interviews", &body).await;
        assert_eq!(
            (refused.status, refused.kind()),
            (400, "target-mismatch"),
            "{target:?}"
        );
    }
    let dotted = json!({"workspace": "alpha", "target": world.workspaces.join("./alpha/"), "templateId": "sample-v1"});
    assert_eq!(service.post("/v1/interviews", &dotted).await.status, 200);
    assert!(!world.folder("alpha").exists());
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_workspace_link_out_of_the_root_is_refused() {
    let world = World::new();
    let service = Running::start(&world).await;
    let outside = world.root.join("outside");
    std::fs::create_dir_all(&outside).unwrap();
    git(&outside, &["init", "-q"]);
    std::os::unix::fs::symlink(&outside, world.folder("alpha")).unwrap();
    std::os::unix::fs::symlink(world.folder("alpha"), world.folder("beta")).unwrap();
    for (workspace, route) in [
        ("alpha", "/v1/upgrades"),
        ("beta", "/v1/upgrades"),
        ("alpha", "/v1/interviews"),
    ] {
        let refused = service
            .post(route, &world.select(workspace, "sample-v1"))
            .await;
        assert_eq!(
            (refused.status, refused.kind()),
            (409, "target-outside-root"),
            "{workspace} {route}"
        );
    }
    assert_eq!(entries(&outside), [".git"]);
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_session_is_pinned_to_its_template_target_and_flow() {
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "sample-v1");
    let start = service.post("/v1/interviews", &selector).await;
    let session = start.json["session"].clone();
    let envelope = submission(&start, sample_answers("first"));
    let answer = |selector: Value, session: Value| {
        with(
            selector,
            json!({"session": session, "submission": envelope.clone()}),
        )
    };

    let other_template = service
        .post(
            "/v1/interviews/answers",
            &answer(world.select("alpha", "flows"), session.clone()),
        )
        .await;
    assert_eq!(
        (other_template.status, other_template.kind()),
        (409, "session-mismatch")
    );
    let other_version = service
        .post(
            "/v1/interviews/answers",
            &answer(world.select("alpha", "sample-v2"), session.clone()),
        )
        .await;
    assert_eq!(
        (other_version.status, other_version.kind()),
        (409, "session-mismatch")
    );
    let other_workspace = service
        .post(
            "/v1/interviews/answers",
            &answer(world.select("beta", "sample-v1"), session.clone()),
        )
        .await;
    assert_eq!(
        (other_workspace.status, other_workspace.kind()),
        (409, "session-mismatch")
    );
    let other_flow = service
        .post(
            "/v1/upgrades/answers",
            &answer(selector.clone(), session.clone()),
        )
        .await;
    assert_eq!(
        (other_flow.status, other_flow.kind()),
        (409, "session-mismatch")
    );
    let other_apply = service
        .post(
            "/v1/upgrades/apply",
            &with(selector.clone(), json!({"session": session.clone()})),
        )
        .await;
    assert_eq!(
        (other_apply.status, other_apply.kind()),
        (409, "session-mismatch")
    );

    let mut renamed = session.clone();
    renamed["template"] = json!(format!("file://{}/templates/flows", world.root.display()));
    let tampered = service
        .post("/v1/interviews/answers", &answer(selector.clone(), renamed))
        .await;
    assert_eq!(
        (tampered.status, tampered.kind()),
        (409, "session-mismatch")
    );
    let mut named = session.clone();
    named["named"] = json!(true);
    let tampered = service
        .post("/v1/interviews/answers", &answer(selector.clone(), named))
        .await;
    assert_eq!(
        (tampered.status, tampered.kind()),
        (409, "session-mismatch")
    );
    let mut moved = session.clone();
    moved["target"] = json!(world.folder("beta"));
    let tampered = service
        .post("/v1/interviews/answers", &answer(selector.clone(), moved))
        .await;
    assert_eq!(
        (tampered.status, tampered.kind()),
        (409, "session-mismatch")
    );
    let mut upgraded = session.clone();
    upgraded["base"] = json!("{}");
    let tampered = service
        .post(
            "/v1/interviews/answers",
            &answer(selector.clone(), upgraded),
        )
        .await;
    assert_eq!(
        (tampered.status, tampered.kind()),
        (409, "session-mismatch")
    );
    let mut baseline = session.clone();
    baseline["base"] = json!("@baseline");
    let tampered = service
        .post("/v1/upgrades/answers", &answer(selector.clone(), baseline))
        .await;
    assert_eq!(
        (tampered.status, tampered.kind()),
        (400, "malformed-session")
    );
    let mut recommitted = session.clone();
    recommitted["commit"] = json!("0".repeat(40));
    let tampered = service
        .post(
            "/v1/interviews/answers",
            &answer(selector.clone(), recommitted),
        )
        .await;
    assert_eq!(
        (tampered.status, tampered.kind()),
        (409, "session-mismatch"),
        "{}",
        tampered.json
    );
    // A moving address keeps the session's commit only when a start admitted it.
    let moving = world.select("alpha", "sample-main");
    let branch = service.post("/v1/interviews", &moving).await;
    let mut missing = branch.json["session"].clone();
    missing["commit"] = json!("0".repeat(40));
    let tampered = service
        .post(
            "/v1/interviews/answers",
            &with(
                moving,
                json!({"session": missing, "submission": submission(&branch, json!({}))}),
            ),
        )
        .await;
    assert_eq!(
        (tampered.status, tampered.kind()),
        (409, "session-mismatch"),
        "{}",
        tampered.json
    );

    let accepted = service
        .post("/v1/interviews/answers", &answer(selector, session))
        .await;
    assert_eq!(
        accepted.json["state"], "completed",
        "positive control failed: {}",
        accepted.json
    );
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn malformed_requests_sessions_and_envelopes_are_refused() {
    let world = World::new();
    let service = Running::start(&world).await;
    let selector = world.select("alpha", "sample-v1");
    let start = service.post("/v1/interviews", &selector).await;
    let session = start.json["session"].clone();

    let not_json = post_bytes(&world.socket, "/v1/interviews", b"{".to_vec()).await;
    assert_eq!(
        (not_json.status, not_json.kind()),
        (400, "malformed-request")
    );
    // Syntax is judged before numbers: a truncated body is malformed even
    // when it holds a number Toha could not keep.
    let truncated = post_bytes(
        &world.socket,
        "/v1/interviews",
        b"{\"workspace\": 1e400".to_vec(),
    )
    .await;
    assert_eq!(
        (truncated.status, truncated.kind()),
        (400, "malformed-request")
    );
    let extra = service
        .post(
            "/v1/interviews",
            &with(selector.clone(), json!({"unexpected": 1})),
        )
        .await;
    assert_eq!((extra.status, extra.kind()), (400, "malformed-request"));
    let missing = service
        .post(
            "/v1/interviews/answers",
            &with(selector.clone(), json!({"session": session})),
        )
        .await;
    assert_eq!((missing.status, missing.kind()), (400, "malformed-request"));

    for bad in [json!("text"), json!({}), json!({"template": "x"})] {
        let refused = service
            .post(
                "/v1/interviews/answers",
                &with(selector.clone(), json!({"session": bad, "submission": {}})),
            )
            .await;
        assert_eq!(
            (refused.status, refused.kind()),
            (400, "malformed-session"),
            "{bad}"
        );
    }
    let template = start.json["document"]["context"]["template"].clone();
    for (envelope, kind) in [
        (json!({"answers": {}}), "malformed-submission"),
        (
            json!({"template": template, "answers": {}, "extra": 1}),
            "malformed-submission",
        ),
        (
            json!({"template": template, "answers": []}),
            "malformed-submission",
        ),
        (
            json!({"template": template, "answers": {"Not An Id": 1}}),
            "malformed-submission",
        ),
        (
            json!({"template": "file:///elsewhere", "answers": {}}),
            "submission-template-mismatch",
        ),
    ] {
        let refused = service
            .post(
                "/v1/interviews/answers",
                &with(
                    selector.clone(),
                    json!({"session": session, "submission": envelope}),
                ),
            )
            .await;
        assert_eq!((refused.status, refused.kind()), (400, kind), "{envelope}");
    }

    let route = request(&world.socket, "POST", "/v1/elsewhere", b"{}".to_vec()).await;
    assert_eq!((route.status, route.kind()), (404, "not-found"));
    let method = request(&world.socket, "GET", "/v1/interviews", vec![]).await;
    assert_eq!((method.status, method.kind()), (405, "method-not-allowed"));
    assert_eq!(method.content_type.as_deref(), Some("application/json"));
    service.stop().await;
}

#[test]
fn owned_results_cross_the_worker_boundary() {
    fn send<T: Send + 'static>() {}
    send::<menagerie_template_service::api::InterviewReply>();
    send::<menagerie_template_service::api::ApplyReply>();
    send::<menagerie_template_service::error::ServiceError>();
    send::<menagerie_template_service::api::StartRequest>();
    send::<menagerie_template_service::api::AnswerRequest>();
    send::<menagerie_template_service::api::ApplyRequest>();
}
