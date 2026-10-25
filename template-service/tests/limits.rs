//! A request nested far deeper than any JSON value is refused as JSON and the
//! service keeps serving. Runs the binary, so a stack overflow would end the
//! service process, not this test.
mod common;
use common::*;

#[tokio::test(flavor = "multi_thread")]
async fn a_deeply_nested_request_is_refused_and_the_service_keeps_serving() {
    let world = World::new();
    let mut service = launch(&world);
    for depth in [20_000, 128] {
        let body = format!(
            "{{\"unexpected\":{}0{}}}",
            "[".repeat(depth),
            "]".repeat(depth)
        );
        let refused = post_bytes(&world.socket, "/v1/interviews", body.into_bytes()).await;
        assert_eq!(
            (refused.status, refused.kind()),
            (400, "nesting-too-deep"),
            "{depth}"
        );
        assert_eq!(refused.content_type.as_deref(), Some("application/json"));
    }
    // The depth refusal comes first, even for a body that never closes.
    let unterminated = format!("{{\"unexpected\":{}", "[".repeat(20_000));
    let refused = post_bytes(&world.socket, "/v1/interviews", unterminated.into_bytes()).await;
    assert_eq!((refused.status, refused.kind()), (400, "nesting-too-deep"));
    let deepest = format!("{{\"unexpected\":{}0{}}}", "[".repeat(126), "]".repeat(126));
    let parsed = post_bytes(&world.socket, "/v1/interviews", deepest.into_bytes()).await;
    assert_eq!((parsed.status, parsed.kind()), (400, "malformed-request"));
    let start = post_bytes(
        &world.socket,
        "/v1/interviews",
        serde_json::to_vec(&world.select("alpha", "sample-v1")).unwrap(),
    )
    .await;
    assert_eq!(start.json["state"], "questions", "{}", start.json);
    assert!(
        service.0.try_wait().unwrap().is_none(),
        "the service exited"
    );
}
