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

/// A repository with a minimal template under `kit`, tagged `1.0.0`,
/// changed by `shape` before its commit.
fn kit(world: &World, name: &str, shape: impl FnOnce(&std::path::Path)) -> String {
    let repo = world.root.join("templates").join(name);
    std::fs::create_dir_all(&repo).unwrap();
    git(&repo, &["init", "-q", "-b", "main"]);
    write(
        &repo,
        "kit/template.yml",
        "name: kit\ninterview:\n  - {id: label, type: text, prompt: Label}\nhooks:\n  - run: [sh, -c, 'echo ran > hook-marker.txt']\n",
    );
    write(&repo, "kit/template/readme.txt", "{{ label }}\n");
    shape(&repo);
    commit_all(&repo, "kit");
    git(&repo, &["tag", "1.0.0"]);
    format!("{}#kit", served(&repo))
}

#[tokio::test(flavor = "multi_thread")]
async fn a_check_reads_a_version_and_its_definition_without_running_it() {
    let world = World::new();
    let service = Running::start(&world).await;
    let with = kit(&world, "with", |repo| {
        write(repo, "kit/template/.devcontainer/devcontainer.json", "{}\n");
    });
    let without = kit(&world, "without", |_| {});
    let commit = git(&world.root.join("templates/with"), &["rev-parse", "1.0.0"]);
    for (address, definition) in [(&with, true), (&without, false)] {
        let reply = service
            .post(
                "/v1/templates/checks",
                &json!({"address": address, "ref": "1.0.0"}),
            )
            .await;
        assert_eq!(reply.status, 200, "{}", reply.json);
        assert_eq!(reply.json["source"], json!(address), "{address}");
        assert_eq!(reply.json["definition"], json!(definition), "{address}");
    }
    let reply = service
        .post(
            "/v1/templates/checks",
            &json!({"address": with, "ref": "1.0.0"}),
        )
        .await;
    assert_eq!(reply.json["commit"], json!(commit));
    // No hook ran and nothing reached the hook runner.
    assert!(world.hooks.calls().is_empty());
    assert!(
        !world
            .root
            .join("templates/with/kit/hook-marker.txt")
            .exists()
    );
    // A missing ref is the fetch's refusal; a ref on the address is refused.
    let missing = service
        .post(
            "/v1/templates/checks",
            &json!({"address": with, "ref": "9.9.9"}),
        )
        .await;
    assert_eq!(missing.status, 422, "{}", missing.json);
    let doubled = service
        .post(
            "/v1/templates/checks",
            &json!({"address": with.replace("#kit", "@1.0.0#kit"), "ref": "1.0.0"}),
        )
        .await;
    assert_eq!((doubled.status, doubled.kind()), (400, "invalid-address"));
    // A folder the repository does not hold is no template.
    let absent = service
        .post(
            "/v1/templates/checks",
            &json!({"address": with.replace("#kit", "#absent"), "ref": "1.0.0"}),
        )
        .await;
    assert_eq!(
        (absent.status, absent.kind()),
        (422, "template-invalid"),
        "{}",
        absent.json
    );
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_check_refuses_a_manifest_toha_cannot_load() {
    let world = World::new();
    let service = Running::start(&world).await;
    let broken = kit(&world, "broken-manifest", |repo| {
        write(
            repo,
            "kit/template.yml",
            "name: kit\ninterview: {not: a list}\n",
        );
    });
    let reply = service
        .post(
            "/v1/templates/checks",
            &json!({"address": broken, "ref": "1.0.0"}),
        )
        .await;
    assert_eq!(
        (reply.status, reply.kind()),
        (422, "template-invalid"),
        "{}",
        reply.json
    );
    service.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_check_refuses_a_folder_or_link_that_leaves_the_checkout() {
    let world = World::new();
    let service = Running::start(&world).await;
    let key = world.data.join(menagerie_template_service::seal::KEY_FILE);
    assert!(key.exists());
    let linked = kit(&world, "linked", |repo| {
        std::os::unix::fs::symlink(&key, repo.join("kit/template/key.txt")).unwrap();
    });
    let escaping = kit(&world, "escaping", |repo| {
        std::os::unix::fs::symlink("../../../../..", repo.join("away")).unwrap();
    });
    let escaping = escaping.replace("#kit", "#away");
    let beside = kit(&world, "beside", |repo| {
        std::fs::create_dir_all(repo.join("kit/scripts")).unwrap();
        std::os::unix::fs::symlink(&key, repo.join("kit/scripts/key")).unwrap();
    });
    for address in [linked, escaping, beside] {
        let reply = service
            .post(
                "/v1/templates/checks",
                &json!({"address": address, "ref": "1.0.0"}),
            )
            .await;
        assert_eq!(
            (reply.status, reply.kind()),
            (422, "unsafe-template-link"),
            "{address}: {}",
            reply.json
        );
        assert!(!String::from_utf8_lossy(&reply.body).contains(key.to_str().unwrap()));
    }
    service.stop().await;
}

/// A kit with a clean `1.0.0` and a `2.0.0` whose link beside the content
/// folder, as a hook script could be, leads to the service's session key:
/// Toha refuses a link in the content folder itself when it loads the
/// template. The world names `kit-1` and `kit-2`.
fn linked_kit() -> (World, World, std::path::PathBuf) {
    let holder = World::new();
    let key = holder.data.join(menagerie_template_service::seal::KEY_FILE);
    let address = kit(&holder, "upgraded", |_| {});
    let repo = holder.root.join("templates/upgraded");
    std::fs::create_dir_all(repo.join("kit/scripts")).unwrap();
    std::os::unix::fs::symlink(&key, repo.join("kit/scripts/key")).unwrap();
    commit_all(&repo, "linked");
    git(&repo, &["tag", "2.0.0"]);
    let at = |tag: &str| address.replace("#kit", &format!("@{tag}#kit"));
    let world = World::with_templates(&[("kit-1", &at("1.0.0")), ("kit-2", &at("2.0.0"))]);
    (holder, world, repo)
}

/// `session` moved to `kit-2`'s version and signed for it, as only a holder
/// of the service's key can, so that a test reaches the checks behind the
/// signature.
fn moved_to_the_linked_version(
    world: &World,
    repo: &std::path::Path,
    operation: Operation,
    session: &Value,
) -> (Value, Value) {
    let selector = world.select("alpha", "kit-2");
    let commit = git(repo, &["rev-parse", "2.0.0"]);
    let session = world.resigned(operation, &selector, session, |record| {
        let template = record["template"]
            .as_str()
            .unwrap()
            .replace("@1.0.0", "@2.0.0");
        record["template"] = json!(template);
        record["commit"] = json!(commit);
    });
    (selector, session)
}

#[tokio::test(flavor = "multi_thread")]
async fn every_route_that_reads_a_template_refuses_a_link_that_leaves_the_checkout() {
    let (_holder, world, repo) = linked_kit();
    let service = Running::start(&world).await;
    // A start fetches the version and refuses it before Toha loads it.
    let refused = service
        .post("/v1/interviews", &world.select("alpha", "kit-2"))
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (422, "unsafe-template-link"),
        "{}",
        refused.json
    );
    // A create session, moved behind its signature to the linked version,
    // is refused by generate before anything is written.
    let selector = world.select("alpha", "kit-1");
    let start = service.post("/v1/interviews", &selector).await;
    let done = service
        .post(
            "/v1/interviews/answers",
            &with(
                selector.clone(),
                json!({"session": start.json["session"],
                    "submission": submission(&start, json!({"label": "one"}))}),
            ),
        )
        .await;
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let folder = world.initialize("alpha");
    let (linked, session) =
        moved_to_the_linked_version(&world, &repo, Operation::Create, &done.json["session"]);
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(linked, json!({"session": session})),
        )
        .await;
    assert_eq!(
        (generated.status, generated.kind()),
        (422, "unsafe-template-link"),
        "{}",
        generated.json
    );
    assert_eq!(entries(&folder), [".git"]);
    assert!(world.hooks.calls().is_empty());

    // The clean version generates, and an upgrade session moved to the
    // linked version is refused by apply with the workspace unchanged.
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(selector.clone(), json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    commit_all(&folder, "generated");
    let refused = service
        .post("/v1/upgrades", &world.select("alpha", "kit-2"))
        .await;
    assert_eq!(
        (refused.status, refused.kind()),
        (422, "unsafe-template-link"),
        "{}",
        refused.json
    );
    let upgrade = service.post("/v1/upgrades", &selector).await;
    assert_eq!(upgrade.json["state"], "completed", "{}", upgrade.json);
    let before = std::fs::read_to_string(folder.join(".toha.json")).unwrap();
    let (linked, session) =
        moved_to_the_linked_version(&world, &repo, Operation::Upgrade, &upgrade.json["session"]);
    let applied = service
        .post(
            "/v1/upgrades/apply",
            &with(linked, json!({"session": session})),
        )
        .await;
    assert_eq!(
        (applied.status, applied.kind()),
        (422, "unsafe-template-link"),
        "{}",
        applied.json
    );
    assert_eq!(
        std::fs::read_to_string(folder.join(".toha.json")).unwrap(),
        before
    );
    assert!(!folder.join("scripts").exists());
    service.stop().await;
}
