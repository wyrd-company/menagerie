//! Numbers through the session and the applied record, against Toha.
mod common;
use common::*;
use serde_json::json;

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
    let catalog = &done.record()["submissions"][0]["catalog"];
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
        json!(["/session/record/submissions/0/catalog/size"])
    );
    assert_eq!(
        entries(&folder),
        [".git"],
        "refusal wrote into the workspace"
    );
    service.stop().await;
}
