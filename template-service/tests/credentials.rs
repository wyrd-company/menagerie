//! Private HTTPS templates through the workspace service's credential route.
//! A private Git host over TLS (`common::GitHost`) admits a fetch only with
//! `x-access-token` basic authentication and a token the route granted for
//! that repository.
mod common;
use common::*;
use serde_json::{Value, json};
use std::path::Path;
use std::process::Command;
use std::time::Duration;

/// Run `test` on a runtime started after the authority is trusted.
fn run(test: impl std::future::Future<Output = ()>) {
    run_with_workers(test, 512);
}

fn run_with_workers(test: impl std::future::Future<Output = ()>, workers: usize) {
    authority();
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .max_blocking_threads(workers)
        .enable_all()
        .build()
        .unwrap()
        .block_on(test);
}

/// Tokens the route grants; no reply may hold one.
const TOKEN_A: &str = "ghs_sample0token0a";
const TOKEN_B: &str = "ghs_sample0token0b";
/// A token that starts and ends with `g`.
const TOKEN_G: &str = "ghs_sample0overlap0token0g";

/// The route's answer for the GitHub App: a token for each URL in `grants`.
fn grants(grants: Vec<(String, &'static str)>) -> impl Fn(&str) -> (u16, Value) + Send + Sync {
    move |url| match grants.iter().find(|(u, _)| u == url) {
        Some((_, token)) => (
            200,
            json!({"username": "x-access-token", "password": token}),
        ),
        None => (
            404,
            json!({"reason": "the URL is not an https repository URL on the GitHub App's server"}),
        ),
    }
}

fn token_free(reply: &Reply) {
    let body = String::from_utf8_lossy(&reply.body);
    for token in [TOKEN_A, TOKEN_B, TOKEN_G] {
        assert!(!body.contains(token), "{body}");
    }
}

/// A world with the sample template served by `host` as
/// `example-org/kit-a.git` and `example-org/kit-b.git`, and as
/// `example-org/kit-c`, a name Toha's formal name keeps, in two versions.
fn world(host: &GitHost) -> World {
    let (a, b, c, next) = (
        format!("{}@v1", host.url("example-org/kit-a.git")),
        format!("{}@v1", host.url("example-org/kit-b.git")),
        format!("{}@v1", host.url("example-org/kit-c")),
        format!("{}@v2", host.url("example-org/kit-c")),
    );
    let world = World::with_templates(&[
        ("private-a", &a),
        ("private-b", &b),
        ("private-c", &c),
        ("private-c-next", &next),
    ]);
    host.publish("example-org/kit-c", &world.sample);
    host.publish("example-org/kit-a.git", &world.sample);
    host.publish("example-org/kit-b.git", &world.sample);
    world
}

#[test]
fn a_private_https_template_is_fetched_with_the_routes_token() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        let repo = host.url("example-org/kit-a.git");
        host.admit(TOKEN_A, "example-org/kit-a.git");
        world
            .hooks
            .answer_credentials(grants(vec![(repo.clone(), TOKEN_A)]));
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "private-a"))
            .await;
        assert_eq!(start.status, 200, "{}", start.json);
        token_free(&start);
        let urls = world.hooks.credential_urls();
        assert!(
            !urls.is_empty() && urls.iter().all(|u| *u == repo),
            "{urls:?}"
        );
        let seen = host.seen();
        assert_eq!(seen[0].user, None, "{seen:?}");
        assert!(
            seen.iter()
                .any(|s| s.user == Some(("x-access-token".to_owned(), TOKEN_A.to_owned()))),
            "{seen:?}"
        );
        // The interview's answers and the generate read the fetched
        // revision; the route answers them as well.
        let done = service
            .post(
                "/v1/interviews/answers",
                &with(
                    world.select("alpha", "private-a"),
                    json!({"session": start.json["session"],
                    "submission": submission(&start, json!({"label": "one",
                        "catalog": {"title": "Sample"}}))}),
                ),
            )
            .await;
        assert_eq!(done.json["state"], "completed", "{}", done.json);
        token_free(&done);
        world.initialize("alpha");
        let generated = service
            .post(
                "/v1/workspaces/generate",
                &with(
                    world.select("alpha", "private-a"),
                    json!({"session": done.json["session"]}),
                ),
            )
            .await;
        assert_eq!(generated.status, 200, "{}", generated.json);
        token_free(&generated);
        assert!(world.folder("alpha").join("result.txt").exists());
        service.stop().await;
    });
}

#[test]
fn a_url_the_route_does_not_serve_gets_no_credential() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "private-a"))
            .await;
        assert_eq!(
            (start.status, start.kind()),
            (422, "template-resolve-failed"),
            "{}",
            start.json
        );
        assert_eq!(
            world.hooks.credential_urls(),
            [host.url("example-org/kit-a.git")]
        );
        assert!(host.seen().iter().all(|s| s.user.is_none()));
        // No credential was handed out, so the reply keeps Toha's text.
        let message = start.json["error"]["message"].as_str().unwrap();
        assert!(message.starts_with("git fetch failed: "), "{message}");
        service.stop().await;
    });
}

#[test]
fn a_minting_failure_fails_the_fetch_with_the_routes_reason() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        world.hooks.answer_credentials(|_| {
            (
                502,
                json!({"reason": "the App is not installed for the repository"}),
            )
        });
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "private-a"))
            .await;
        assert_eq!(start.status, 422, "{}", start.json);
        let message = start.json["error"]["message"].as_str().unwrap();
        assert!(
            message.contains(
                "GitHub App credentials are unavailable: the App is not installed for the repository"
            ),
            "{message}"
        );
        assert!(host.seen().iter().all(|s| s.user.is_none()));
        service.stop().await;
    });
}

/// Two operations at once each answer their own challenge from their own
/// resolver scope: the host holds both challenges until both arrived, and
/// admits each token for its own repository only.
#[test]
fn concurrent_operations_each_fetch_with_their_own_token() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        host.admit(TOKEN_A, "example-org/kit-a.git");
        host.admit(TOKEN_B, "example-org/kit-b.git");
        host.hold_until(2);
        world.hooks.answer_credentials(grants(vec![
            (host.url("example-org/kit-a.git"), TOKEN_A),
            (host.url("example-org/kit-b.git"), TOKEN_B),
        ]));
        let service = Running::start(&world).await;
        let (a, b) = (
            world.select("alpha", "private-a"),
            world.select("beta", "private-b"),
        );
        let (a, b) = tokio::join!(
            service.post("/v1/interviews", &a),
            service.post("/v1/interviews", &b),
        );
        assert!(!host.gave_up(), "the two challenges did not overlap");
        for reply in [&a, &b] {
            assert_eq!(reply.status, 200, "{}", reply.json);
            token_free(reply);
        }
        let seen = host.seen();
        for (repo, token) in [
            ("/example-org/kit-a.git", TOKEN_A),
            ("/example-org/kit-b.git", TOKEN_B),
        ] {
            assert!(
                seen.iter()
                    .filter(|s| s.path.starts_with(repo))
                    .all(|s| s.user.as_ref().is_none_or(|(_, t)| t == token)),
                "{seen:?}"
            );
        }
        service.stop().await;
    });
}

/// The reply to a fetch that failed after the route handed out a token:
/// the fixed sentence for its kind, and nothing of the remote's answer.
const WITHHELD: &str = r#"{"error":{"kind":"template-resolve-failed","message":"the template could not be fetched; the Git host refused or failed the request"}}"#;

/// A failed fetch with a host that echoes its credential back, as `echo`
/// builds the echo, replies with `WITHHELD` exactly.
async fn echoed(token: &'static str, echo: fn(&str, &str) -> String) {
    let host = GitHost::start();
    let world = world(&host);
    host.admit(token, "example-org/kit-a.git");
    host.echo(echo);
    world
        .hooks
        .answer_credentials(grants(vec![(host.url("example-org/kit-a.git"), token)]));
    let service = Running::start(&world).await;
    let start = service
        .post("/v1/interviews", &world.select("alpha", "private-a"))
        .await;
    assert_eq!(start.status, 422);
    assert_eq!(String::from_utf8_lossy(&start.body), WITHHELD);
    // The host saw the credential, so the echo is the one it sent.
    assert!(
        host.seen()
            .iter()
            .any(|s| s.user == Some(("x-access-token".to_owned(), token.to_owned())))
    );
    service.stop().await;
}

/// A host that echoes the `Authorization` header gets a fixed reply.
#[test]
fn a_credential_a_remote_echoes_never_reaches_the_reply() {
    run(echoed(TOKEN_A, |authorization, _| authorization.to_owned()));
}

/// A token that starts and ends with the same byte, echoed as itself and
/// its overlapping rest, `T + T[1:]`: Toha redacts the first copy, which
/// leaves the rest, so no redaction recovers it. The reply is fixed.
#[test]
fn an_overlapping_echo_of_the_token_never_reaches_the_reply() {
    assert!(TOKEN_G.starts_with('g') && TOKEN_G.ends_with('g'));
    run(echoed(TOKEN_G, |_, token| {
        format!("{token}{}", &token[1..])
    }));
}

/// A success reply can carry the host's content: a host that saw the token
/// serves a template whose question prompt holds it. The reply boundary
/// removes it.
#[test]
fn a_token_in_served_template_content_is_scrubbed_from_a_success_reply() {
    run(async {
        let host = GitHost::start();
        let repo = host.url("example-org/echo.git");
        let world = World::with_templates(&[("echo", &format!("{repo}@v1"))]);
        let source = world.root.join("templates/echo");
        write(
            &source,
            "template.yml",
            format!(
                "name: echo\ninterview:\n  - {{id: label, type: text, prompt: 'Label {TOKEN_A} [{}]', required: true}}\n",
                {
                    let listed: Vec<String> = TOKEN_A.bytes().map(|b| b.to_string()).collect();
                    listed.join(", ")
                }
            ),
        );
        write(&source, "template/out.txt", "label: {{ label }}\n");
        git(&source, &["init", "-q", "-b", "main"]);
        commit_all(&source, "echo");
        git(&source, &["tag", "v1"]);
        host.publish("example-org/echo.git", &source);
        host.admit(TOKEN_A, "example-org/echo.git");
        world
            .hooks
            .answer_credentials(grants(vec![(repo, TOKEN_A)]));
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "echo"))
            .await;
        assert_eq!(start.status, 200, "{}", start.json);
        let body = String::from_utf8_lossy(&start.body);
        assert!(body.contains("Label <redacted> [<redacted>]"), "{body}");
        token_free(&start);
        service.stop().await;
    });
}

/// A user name in a template address does not opt out of the App: the
/// route is asked for the address without it, and the fetch uses the token.
#[test]
fn a_template_address_with_a_user_name_fetches_with_the_app_token() {
    run(async {
        let host = GitHost::start();
        let repo = host.url("example-org/kit-a.git");
        let address = format!(
            "{}@v1",
            repo.replacen("https://", "https://sample-user@", 1)
        );
        let world = World::with_templates(&[("named-user", &address)]);
        host.publish("example-org/kit-a.git", &world.sample);
        host.admit(TOKEN_A, "example-org/kit-a.git");
        world
            .hooks
            .answer_credentials(grants(vec![(repo.clone(), TOKEN_A)]));
        let service = Running::start(&world).await;
        let start = service
            .post("/v1/interviews", &world.select("alpha", "named-user"))
            .await;
        assert_eq!(start.status, 200, "{}", start.json);
        token_free(&start);
        let urls = world.hooks.credential_urls();
        assert!(
            !urls.is_empty() && urls.iter().all(|u| *u == repo),
            "{urls:?}"
        );
        assert!(
            host.seen()
                .iter()
                .any(|s| s.user == Some(("x-access-token".to_owned(), TOKEN_A.to_owned())))
        );
        service.stop().await;
    });
}

/// The threads of this process.
fn threads() -> usize {
    std::fs::read_dir("/proc/self/task").unwrap().count()
}

/// Wait up to ten seconds for `ok`.
async fn eventually(ok: impl Fn() -> bool) -> bool {
    for _ in 0..1000 {
        if ok() {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    ok()
}

/// A caller that goes away while the credential route holds its request,
/// as a cancelled job or one at its limit does, ends the request: the
/// route's connection closes, the worker returns, and repeats add no
/// thread.
#[test]
fn a_caller_going_away_ends_its_credential_request_and_worker() {
    // This assertion counts every OS thread in the process. Run just this
    // test in a child so parallel TLS fixtures and their separate runtimes
    // cannot change its baseline while the requests repeat.
    const ISOLATED: &str = "MENAGERIE_CREDENTIAL_THREADS_ISOLATED";
    if std::env::var_os(ISOLATED).is_none() {
        let output = Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "a_caller_going_away_ends_its_credential_request_and_worker",
                "--nocapture",
            ])
            .env(ISOLATED, "1")
            .output()
            .unwrap();
        assert!(
            output.status.success() && String::from_utf8_lossy(&output.stdout).contains("1 passed"),
            "{}\n{}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        return;
    }
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        world.hooks.hold_credentials(true);
        let service = Running::start(&world).await;
        let mut settled = None;
        for n in 1..=5 {
            let socket = service.socket.clone();
            let selector = world.select(&format!("w{n}"), "private-a");
            let caller = tokio::spawn(async move {
                post_bytes(
                    &socket,
                    "/v1/interviews",
                    body_of("/v1/interviews", &selector),
                )
                .await
            });
            assert!(
                eventually(|| world.hooks.held() == n).await,
                "request {n} did not reach the credential route"
            );
            caller.abort();
            assert!(
                eventually(|| world.hooks.released() == n).await,
                "the credential request of caller {n} stayed open"
            );
            match settled {
                // The first repeat starts the blocking pool's worker.
                None => settled = Some(threads()),
                Some(limit) => assert!(
                    eventually(|| threads() <= limit).await,
                    "caller {n} left a thread: {} > {limit}",
                    threads()
                ),
            }
        }
        // The service still answers, so no worker holds it.
        world.hooks.hold_credentials(false);
        world
            .hooks
            .answer_credentials(|_| (404, json!({"reason": "no GitHub App is configured"})));
        let start = service
            .post("/v1/interviews", &world.select("after", "private-a"))
            .await;
        assert_eq!(start.kind(), "template-resolve-failed", "{}", start.json);
        service.stop().await;
    });
}

/// Call `path` with `body` while the host stalls its last pack request, after
/// `others` others, go away, and prove the request worker returned while
/// the host still holds the connection. Then let the abandoned fetch finish.
async fn lose_the_caller_during_a_fetch(
    host: &GitHost,
    world: &World,
    service: &Running,
    path: &'static str,
    body: &Value,
    others: usize,
) {
    // With no admission cached, the operation asks the remote again.
    let cache = world.data.join("cache/sources");
    if cache.exists() {
        std::fs::remove_dir_all(cache).unwrap();
    }
    host.stall(others);
    let socket = service.socket.clone();
    let bytes = body_of(path, body);
    let caller = tokio::spawn(async move { post_bytes(&socket, path, bytes).await });
    assert!(
        eventually(|| host.stalls() > 0).await,
        "the operation never reached its last pack request"
    );
    caller.abort();
    assert!(caller.await.unwrap_err().is_cancelled());
    let returned =
        tokio::time::timeout(Duration::from_secs(2), tokio::task::spawn_blocking(|| ())).await;
    // Always release the host before asserting, including in mutation runs.
    host.resume();
    returned
        .expect("request worker did not return while Git was stalled")
        .unwrap();
    // Test-only: long enough for the writes an unguarded operation makes,
    // which removing each guard shows.
    tokio::time::sleep(Duration::from_secs(2)).await;
}

/// Every file of a folder outside `.git`, with its text.
fn tree(folder: &Path) -> Vec<(String, String)> {
    let mut files = vec![];
    for name in entries(folder) {
        if name != ".git" && folder.join(&name).is_file() {
            files.push((name.clone(), read(folder, &name)));
        }
    }
    files
}

/// A caller that goes away while the host holds its generate's fetch, as a
/// cancelled job or one at its limit does: nothing is written to the folder
/// when the fetch returns.
#[test]
fn a_fetch_that_returns_after_its_generate_lost_its_caller_writes_nothing() {
    run_with_workers(
        async {
            let host = GitHost::start();
            let world = world(&host);
            host.admit(TOKEN_A, "example-org/kit-a.git");
            world
                .hooks
                .answer_credentials(grants(vec![(host.url("example-org/kit-a.git"), TOKEN_A)]));
            let service = Running::start(&world).await;
            let selector = world.select("alpha", "private-a");
            let start = service.post("/v1/interviews", &selector).await;
            let done = service
                .post(
                    "/v1/interviews/answers",
                    &with(
                        selector.clone(),
                        json!({"session": start.json["session"],
                    "submission": submission(&start, json!({"label": "one",
                        "catalog": {"title": "Sample"}}))}),
                    ),
                )
                .await;
            assert_eq!(done.json["state"], "completed", "{}", done.json);
            let folder = world.initialize("alpha");
            let before = entries(&folder);
            let body = with(selector, json!({"session": done.json["session"]}));
            lose_the_caller_during_a_fetch(
                &host,
                &world,
                &service,
                "/v1/workspaces/generate",
                &body,
                1,
            )
            .await;
            assert_eq!(entries(&folder), before, "the generate wrote to the folder");
            service.stop().await;
        },
        1,
    );
}

/// The same for an upgrade's apply.
#[test]
fn a_fetch_that_returns_after_its_apply_lost_its_caller_writes_nothing() {
    run_with_workers(
        async {
            let host = GitHost::start();
            let world = world(&host);
            host.admit(TOKEN_A, "example-org/kit-c");
            world
                .hooks
                .answer_credentials(grants(vec![(host.url("example-org/kit-c"), TOKEN_A)]));
            let service = Running::start(&world).await;
            let folder = create(&world, &service, "alpha", "private-c").await;
            let selector = world.select("alpha", "private-c-next");
            let start = service.post("/v1/upgrades", &selector).await;
            assert_eq!(start.status, 200, "{}", start.json);
            let done = service
                .post(
                    "/v1/upgrades/answers",
                    &with(
                        selector.clone(),
                        json!({"session": start.json["session"],
                    "submission": submission(&start, json!({"extra": "added"}))}),
                    ),
                )
                .await;
            assert_eq!(done.json["state"], "completed", "{}", done.json);
            let before = tree(&folder);
            // The merge base is fetched by its commit, as GitHub allows.
            git(
                &host.root.join("example-org/kit-c"),
                &["config", "uploadpack.allowAnySHA1InWant", "true"],
            );
            let body = with(selector, json!({"session": done.json["session"]}));
            lose_the_caller_during_a_fetch(&host, &world, &service, "/v1/upgrades/apply", &body, 3)
                .await;
            assert_eq!(tree(&folder), before, "the apply wrote to the folder");
            service.stop().await;
        },
        1,
    );
}

#[test]
fn a_stalled_interview_fetch_returns_its_worker_when_the_caller_leaves() {
    run_with_workers(
        async {
            let host = GitHost::start();
            let world = world(&host);
            host.admit(TOKEN_A, "example-org/kit-a.git");
            world
                .hooks
                .answer_credentials(grants(vec![(host.url("example-org/kit-a.git"), TOKEN_A)]));
            let service = Running::start(&world).await;
            lose_the_caller_during_a_fetch(
                &host,
                &world,
                &service,
                "/v1/interviews",
                &world.select("alpha", "private-a"),
                0,
            )
            .await;
            assert!(!world.folder("alpha").exists());
            service.stop().await;
        },
        1,
    );
}

#[test]
fn a_template_address_with_a_password_is_refused_without_contacting_the_host() {
    run(async {
        let host = GitHost::start();
        let address = host.url("example-org/kit-a.git").replacen(
            "https://",
            "https://sample-user:sample-password@",
            1,
        );
        let world = World::with_templates(&[("password", &address)]);
        let service = Running::start(&world).await;
        let reply = service
            .post("/v1/interviews", &world.select("alpha", "password"))
            .await;
        assert_eq!(reply.status, 422, "{}", reply.json);
        assert!(!String::from_utf8_lossy(&reply.body).contains("sample-password"));
        assert!(host.seen().is_empty());
        assert!(world.hooks.credential_urls().is_empty());
        service.stop().await;
    });
}

#[test]
fn every_route_returns_its_worker_and_closes_a_stalled_schema_fetch() {
    run_with_workers(
        async {
            let host = GitHost::start();
            let schema = host.schema();
            let source = tempfile::tempdir().unwrap();
            write(
                source.path(),
                "template.yml",
                format!(
                    "name: schema\ninterview:\n  - {{id: catalog, type: document, prompt: Catalog, schema: '{schema}', input: catalog.json}}\n"
                ),
            );
            write(source.path(), "template/out.txt", "sample\n");
            git(source.path(), &["init", "-q", "-b", "main"]);
            let v1 = commit_all(source.path(), "v1");
            git(source.path(), &["tag", "v1"]);
            write(source.path(), "template/out.txt", "updated\n");
            let v2_template = read(source.path(), "template.yml")
                + "  - {id: extra, type: text, prompt: Extra, required: true}\n";
            write(source.path(), "template.yml", v2_template);
            commit_all(source.path(), "v2");
            git(source.path(), &["tag", "v2"]);
            let world = World::with_templates(&[
                ("schema-v1", &format!("{}@v1", served(source.path()))),
                ("schema-v2", &format!("{}@v2", served(source.path()))),
            ]);
            let service = Running::start(&world).await;
            let selector = world.select("alpha", "schema-v1");
            let start = service.post("/v1/interviews", &selector).await;
            assert_eq!(start.status, 200, "{}", start.json);
            assert_eq!(start.json["document"]["context"]["commit"], v1);
            let answering = with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, json!({"catalog": {}}))}),
            );
            let done = service.post("/v1/interviews/answers", &answering).await;
            assert_eq!(done.json["state"], "completed", "{}", done.json);
            let generate = with(selector.clone(), json!({"session": done.json["session"]}));

            async fn cancel_schema(
                host: &GitHost,
                world: &World,
                service: &Running,
                route: &'static str,
                request: Value,
            ) {
                host.hold_schema(true);
                let before = host.schema_stalls();
                let socket = service.socket.clone();
                let bytes = body_of(route, &request);
                let caller = tokio::spawn(async move { post_bytes(&socket, route, bytes).await });
                assert!(
                    eventually(|| host.schema_stalls() > before).await,
                    "{route} did not reach the schema endpoint"
                );
                caller.abort();
                assert!(caller.await.unwrap_err().is_cancelled());
                // The sole blocking worker must return and the schema
                // connection must close within the same two-second bound.
                tokio::time::timeout(Duration::from_secs(2), async {
                    tokio::task::spawn_blocking(|| ()).await.unwrap();
                    while host.schema_closed() <= before {
                        tokio::time::sleep(Duration::from_millis(10)).await;
                    }
                })
                .await
                .unwrap_or_else(|_| panic!("{route} kept its worker or schema connection"));
                host.hold_schema(false);
                // Another request still works on the same single-worker pool.
                let reply = service
                    .post("/v1/interviews", &world.select("healthy", "schema-v1"))
                    .await;
                assert_eq!(reply.status, 200, "{}", reply.json);
            }
            cancel_schema(&host, &world, &service, "/v1/interviews", selector.clone()).await;
            cancel_schema(&host, &world, &service, "/v1/interviews/answers", answering).await;
            let folder = world.initialize("alpha");
            cancel_schema(
                &host,
                &world,
                &service,
                "/v1/workspaces/generate",
                generate.clone(),
            )
            .await;
            assert_eq!(entries(&folder), vec![".git"]);
            let applied = service.post("/v1/workspaces/generate", &generate).await;
            assert_eq!(applied.json["outcome"], "written", "{}", applied.json);
            commit_all(&folder, "generated");
            write(&folder, "catalog.json", "{}");
            commit_all(&folder, "document input");
            let selector = world.select("alpha", "schema-v2");
            let start = service.post("/v1/upgrades", &selector).await;
            assert_eq!(start.json["state"], "questions", "{}", start.json);
            let answering = with(
                selector.clone(),
                json!({"session": start.json["session"],
            "submission": submission(&start, json!({"extra": "added"}))}),
            );
            let done = service.post("/v1/upgrades/answers", &answering).await;
            assert_eq!(done.json["state"], "completed", "{}", done.json);
            let apply = with(selector.clone(), json!({"session": done.json["session"]}));
            let before = tree(&folder);
            cancel_schema(&host, &world, &service, "/v1/upgrades", selector).await;
            cancel_schema(&host, &world, &service, "/v1/upgrades/answers", answering).await;
            cancel_schema(&host, &world, &service, "/v1/upgrades/apply", apply).await;
            assert_eq!(tree(&folder), before);
            service.stop().await;
        },
        1,
    );
}

/// Tag the private host's copy of `name` with the release tag `1.0.0` at
/// the sample's first version.
fn release(host: &GitHost, world: &World, name: &str) {
    git(&host.root.join(name), &["tag", "1.0.0", &world.v1]);
}

#[test]
fn a_private_repositorys_versions_are_listed_with_the_routes_token() {
    run(async {
        let host = GitHost::start();
        let world = world(&host);
        release(&host, &world, "example-org/kit-a.git");
        let repo = host.url("example-org/kit-a.git");
        host.admit(TOKEN_A, "example-org/kit-a.git");
        world
            .hooks
            .answer_credentials(grants(vec![(repo.clone(), TOKEN_A)]));
        let service = Running::start(&world).await;
        let reply = service
            .post("/v1/templates/versions", &json!({"address": repo}))
            .await;
        assert_eq!(reply.status, 200, "{}", reply.json);
        token_free(&reply);
        assert_eq!(
            reply.json["versions"],
            json!([{"version": "1.0.0", "tag": "1.0.0", "commit": world.v1}])
        );
        let urls = world.hooks.credential_urls();
        assert!(
            !urls.is_empty() && urls.iter().all(|u| *u == repo),
            "{urls:?}"
        );
        assert!(
            host.seen()
                .iter()
                .any(|s| s.user == Some(("x-access-token".to_owned(), TOKEN_A.to_owned()))),
            "{:?}",
            host.seen()
        );
        // Without a grant the host refuses the listing.
        world.hooks.answer_credentials(grants(vec![]));
        let refused = service
            .post("/v1/templates/versions", &json!({"address": repo}))
            .await;
        assert_eq!(
            (refused.status, refused.kind()),
            (422, "template-resolve-failed"),
            "{}",
            refused.json
        );
        service.stop().await;
    });
}

#[test]
fn a_stalled_version_listing_returns_its_worker_when_the_caller_leaves() {
    run_with_workers(
        async {
            let host = GitHost::start();
            let world = world(&host);
            release(&host, &world, "example-org/kit-a.git");
            host.admit(TOKEN_A, "example-org/kit-a.git");
            world
                .hooks
                .answer_credentials(grants(vec![(host.url("example-org/kit-a.git"), TOKEN_A)]));
            let service = Running::start(&world).await;
            lose_the_caller_during_a_fetch(
                &host,
                &world,
                &service,
                "/v1/templates/versions",
                &json!({"address": host.url("example-org/kit-a.git")}),
                0,
            )
            .await;
            service.stop().await;
        },
        1,
    );
}
