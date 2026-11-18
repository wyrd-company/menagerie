//! Reading addresses and listing versions, the routes a registration and a
//! version check use before any template is approved.
mod common;
use common::*;
use serde_json::{Value, json};

/// A repository with release, pre-release, `v`-prefixed, annotated and
/// other tags, served by the test host.
fn tagged(world: &World) -> (String, Vec<(&'static str, String)>) {
    let repo = world.root.join("templates/tagged");
    std::fs::create_dir_all(&repo).unwrap();
    git(&repo, &["init", "-q", "-b", "main"]);
    let mut commits = vec![];
    for (tag, annotated) in [
        ("1.0.0", false),
        ("v1.0.0", false),
        ("v1.1.0", true),
        ("1.2.0-rc.1", false),
        ("1.2.0+build", false),
        ("v2", false),
        ("2.0", false),
        ("01.0.0", false),
        ("1.10.0", true),
        ("latest", false),
    ] {
        write(&repo, "template.yml", format!("name: tagged\n# {tag}\n"));
        let commit = commit_all(&repo, tag);
        if annotated {
            git(&repo, &["tag", "-a", "-m", tag, tag]);
        } else {
            git(&repo, &["tag", tag]);
        }
        commits.push((tag, commit));
    }
    (served(&repo), commits)
}

fn commit_of<'a>(commits: &'a [(&str, String)], tag: &str) -> &'a str {
    &commits.iter().find(|(t, _)| *t == tag).unwrap().1
}

#[tokio::test(flavor = "multi_thread")]
async fn an_address_is_read_without_reaching_the_network() {
    let world = World::new();
    let service = Running::start(&world).await;
    for (address, source, reference, base) in [
        (
            "https://github.com/example-org/kit.git@1.2.0#templates/kit",
            "gh:example-org/kit#templates/kit",
            json!("1.2.0"),
            "https://github.com/example-org/kit.git#templates/kit",
        ),
        (
            "https://sample-user@git.example.invalid/example-org/kit",
            "https://git.example.invalid/example-org/kit",
            Value::Null,
            "https://git.example.invalid/example-org/kit",
        ),
        (
            "gh:example-org/kit@v2.0.0",
            "gh:example-org/kit",
            json!("v2.0.0"),
            "gh:example-org/kit",
        ),
        (
            "git@git.example.invalid:example-org/kit.git@1.0.0",
            "ssh://git.example.invalid/example-org/kit",
            json!("1.0.0"),
            "git@git.example.invalid:example-org/kit.git",
        ),
    ] {
        let reply = service
            .post("/v1/templates/addresses", &json!({"address": address}))
            .await;
        assert_eq!(reply.status, 200, "{address}: {}", reply.json);
        assert_eq!(
            reply.json,
            json!({"source": source, "reference": reference, "base": base}),
            "{address}"
        );
    }
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn every_route_refuses_an_address_it_must_never_read_without_quoting_it() {
    let world = World::new();
    let service = Running::start(&world).await;
    let folder = world.root.join("templates/sample");
    let file = format!("file://{}", folder.display());
    let password = "https://sample-user:sample-password@git.example.invalid/example-org/kit";
    let refused = [
        folder.display().to_string(),
        "./kit".to_owned(),
        "sample-kit".to_owned(),
        file.clone(),
        format!("{file}@v1"),
        "git://git.example.invalid/example-org/kit".to_owned(),
        "http://git.example.invalid/example-org/kit".to_owned(),
        "ssh://-oProxyCommand=sample/example-org/kit".to_owned(),
        password.to_owned(),
        format!("{password}@1.0.0"),
    ];
    for route in ["/v1/templates/addresses", "/v1/templates/versions"] {
        for address in &refused {
            let reply = service.post(route, &json!({"address": address})).await;
            assert_eq!(
                (reply.status, reply.kind()),
                (400, "invalid-address"),
                "{route} {address}: {}",
                reply.json
            );
            let body = String::from_utf8_lossy(&reply.body);
            for quoted in [
                "sample-password",
                "example.invalid",
                "sample-kit",
                folder.to_str().unwrap(),
            ] {
                assert!(!body.contains(quoted), "{route} {address}: {body}");
            }
        }
        let extra = service
            .post(
                route,
                &json!({"address": "gh:example-org/kit", "ref": "1.0.0"}),
            )
            .await;
        assert_eq!(
            (extra.status, extra.kind()),
            (400, "malformed-request"),
            "{route}"
        );
    }
    // Nothing was fetched.
    assert!(!world.data.join("cache/sources").exists());
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn versions_are_the_release_tags_in_ascending_order() {
    let world = World::new();
    let service = Running::start(&world).await;
    let (address, commits) = tagged(&world);
    let reply = service
        .post("/v1/templates/versions", &json!({"address": address}))
        .await;
    assert_eq!(reply.status, 200, "{}", reply.json);
    assert_eq!(
        reply.json,
        json!({
            "source": address,
            "versions": [
                {"version": "1.0.0", "tag": "1.0.0", "commit": commit_of(&commits, "1.0.0")},
                {"version": "1.1.0", "tag": "v1.1.0", "commit": commit_of(&commits, "v1.1.0")},
                {"version": "1.10.0", "tag": "1.10.0", "commit": commit_of(&commits, "1.10.0")},
            ]
        })
    );
    // An address with a ref names a version, not a repository to list.
    let with_ref = service
        .post(
            "/v1/templates/versions",
            &json!({"address": format!("{address}@1.0.0")}),
        )
        .await;
    assert_eq!((with_ref.status, with_ref.kind()), (400, "invalid-address"));
    // Listing fetched no history into Toha's cache.
    assert!(!world.data.join("cache/sources").exists());
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_repository_with_no_tag_has_no_version_and_a_missing_one_is_not_found() {
    let world = World::new();
    let service = Running::start(&world).await;
    let flows = served(&world.root.join("templates/flows"));
    let untagged = service
        .post("/v1/templates/versions", &json!({"address": flows}))
        .await;
    assert_eq!(untagged.status, 200, "{}", untagged.json);
    assert_eq!(untagged.json["versions"], json!([]));
    let missing = format!("{}-missing", served(&world.root.join("templates")));
    let reply = service
        .post("/v1/templates/versions", &json!({"address": missing}))
        .await;
    assert_eq!(
        (reply.status, reply.kind()),
        (422, "template-not-found"),
        "{}",
        reply.json
    );
    assert!(!String::from_utf8_lossy(&reply.body).contains("missing"));
    service.stop().await;
}
