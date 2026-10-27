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
async fn a_session_value_is_signed_for_its_record_workspace_template_and_flow() {
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
    let refused = |reply: &Reply, case: &str| {
        assert_eq!(
            (reply.status, reply.kind()),
            (400, "session-tampered"),
            "{case}: {}",
            reply.json
        );
    };

    // Any change to any member of the record is refused.
    let record = start.record().as_object().unwrap().clone();
    assert!(record.len() > 3, "{record:?}");
    for (member, value) in &record {
        let altered = match value {
            Value::Null => json!("altered"),
            _ => Value::Null,
        };
        let mut edited = session.clone();
        edited["record"][member] = altered;
        let reply = service
            .post("/v1/interviews/answers", &answer(selector.clone(), edited))
            .await;
        refused(&reply, member);
    }
    let mut extra = session.clone();
    extra["record"]["added"] = json!(1);
    let reply = service
        .post("/v1/interviews/answers", &answer(selector.clone(), extra))
        .await;
    refused(&reply, "added member");
    let mut deeper = session.clone();
    deeper["record"]["context"] = json!({"altered": true});
    let reply = service
        .post("/v1/interviews/answers", &answer(selector.clone(), deeper))
        .await;
    refused(&reply, "replaced context");

    // An unsigned record, a stripped or foreign signature is refused.
    let mut unsigned = session.clone();
    unsigned.as_object_mut().unwrap().remove("signature");
    let foreign = menagerie_template_service::seal::sealed(
        &menagerie_template_service::seal::SessionKey::new(&[1; 32]),
        menagerie_template_service::seal::Binding::new(Operation::Create, "alpha", "sample-v1"),
        start.record().clone(),
    )
    .unwrap();
    for (case, value) in [
        ("bare record", start.record().clone()),
        ("no signature", unsigned),
        ("foreign key", foreign),
    ] {
        let reply = service
            .post("/v1/interviews/answers", &answer(selector.clone(), value))
            .await;
        refused(&reply, case);
    }

    // The value cannot be replayed into another workspace, template or flow.
    for (case, route, selector) in [
        (
            "other workspace",
            "/v1/interviews/answers",
            world.select("beta", "sample-v1"),
        ),
        (
            "other version",
            "/v1/interviews/answers",
            world.select("alpha", "sample-v2"),
        ),
        (
            "other template",
            "/v1/interviews/answers",
            world.select("alpha", "flows"),
        ),
        ("upgrade answer", "/v1/upgrades/answers", selector.clone()),
    ] {
        let reply = service
            .post(route, &answer(selector, session.clone()))
            .await;
        refused(&reply, case);
    }
    let reply = service
        .post(
            "/v1/upgrades/apply",
            &with(selector.clone(), json!({"session": session.clone()})),
        )
        .await;
    refused(&reply, "upgrade apply");

    let accepted = service
        .post("/v1/interviews/answers", &answer(selector.clone(), session))
        .await;
    assert_eq!(
        accepted.json["state"], "completed",
        "positive control failed: {}",
        accepted.json
    );
    let folder = world.initialize("alpha");
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": accepted.json["session"]})),
        )
        .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    assert!(folder.join(".toha.json").is_file());
    service.stop().await;
}

/// Behind the signature, a session is still checked against its approved
/// template, target and flow: a value signed with the service's key for
/// another binding is refused as a mismatch.
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
    let record = start.record().clone();
    let mismatch = |reply: &Reply, case: &str| {
        assert_eq!(
            (reply.status, reply.kind()),
            (409, "session-mismatch"),
            "{case}: {}",
            reply.json
        );
    };

    for (case, route, operation, selector) in [
        (
            "other template",
            "/v1/interviews/answers",
            Operation::Create,
            world.select("alpha", "flows"),
        ),
        (
            "other version",
            "/v1/interviews/answers",
            Operation::Create,
            world.select("alpha", "sample-v2"),
        ),
        (
            "other workspace",
            "/v1/interviews/answers",
            Operation::Create,
            world.select("beta", "sample-v1"),
        ),
        (
            "other flow",
            "/v1/upgrades/answers",
            Operation::Upgrade,
            selector.clone(),
        ),
    ] {
        let signed = world.signed(operation, &selector, record.clone());
        let reply = service.post(route, &answer(selector, signed)).await;
        mismatch(&reply, case);
    }
    let signed = world.signed(Operation::Upgrade, &selector, record.clone());
    let reply = service
        .post(
            "/v1/upgrades/apply",
            &with(selector.clone(), json!({"session": signed})),
        )
        .await;
    mismatch(&reply, "other apply");

    let edit = |edit: &dyn Fn(&mut Value)| {
        world.resigned(Operation::Create, &selector, &session, |record| {
            edit(record)
        })
    };
    let renamed = edit(&|r| {
        r["template"] = json!(format!("file://{}/templates/flows", world.root.display()))
    });
    let named = edit(&|r| r["named"] = json!(true));
    let moved = edit(&|r| r["target"] = json!(world.folder("beta")));
    let upgraded = edit(&|r| r["base"] = json!("{}"));
    let recommitted = edit(&|r| r["commit"] = json!("0".repeat(40)));
    for (case, value) in [
        ("renamed", renamed),
        ("named", named),
        ("moved", moved),
        ("upgraded", upgraded),
        ("recommitted", recommitted),
    ] {
        let reply = service
            .post("/v1/interviews/answers", &answer(selector.clone(), value))
            .await;
        mismatch(&reply, case);
    }
    let baseline = world.resigned(Operation::Upgrade, &selector, &session, |r| {
        r["base"] = json!("@baseline")
    });
    let reply = service
        .post("/v1/upgrades/answers", &answer(selector.clone(), baseline))
        .await;
    assert_eq!(
        (reply.status, reply.kind()),
        (400, "malformed-session"),
        "{}",
        reply.json
    );
    // A moving address keeps the session's commit only when a start admitted it.
    let moving = world.select("alpha", "sample-main");
    let branch = service.post("/v1/interviews", &moving).await;
    let missing = world.resigned(Operation::Create, &moving, &branch.json["session"], |r| {
        r["commit"] = json!("0".repeat(40))
    });
    let reply = service
        .post(
            "/v1/interviews/answers",
            &with(
                moving,
                json!({"session": missing, "submission": submission(&branch, json!({}))}),
            ),
        )
        .await;
    mismatch(&reply, "missing admission");

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
            (400, "session-tampered"),
            "{bad}"
        );
        // Signed by the service, the same value is not a staged record.
        let signed = world.signed(Operation::Create, &selector, bad.clone());
        let refused = service
            .post(
                "/v1/interviews/answers",
                &with(
                    selector.clone(),
                    json!({"session": signed, "submission": {}}),
                ),
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
