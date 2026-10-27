//! Shared fixtures: generic Git template repositories, a workspace root, the
//! service on a Unix socket, and a minimal HTTP/1.1 client for that socket.
//! Git subprocesses here only prepare temporary test repositories; the service
//! itself runs no Git command.
#![allow(dead_code)]
use http_body_util::{BodyExt, Full};
use hyper::body::Bytes;
pub use menagerie_template_service::checks::Operation;
use menagerie_template_service::seal::{self, Binding, SessionKey};
use menagerie_template_service::{Service, config::Settings, server};
use serde_json::{Value, json};
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Arc;

pub fn git(dir: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(args)
        .env("GIT_AUTHOR_NAME", "Example User")
        .env("GIT_AUTHOR_EMAIL", "user@example.invalid")
        .env("GIT_COMMITTER_NAME", "Example User")
        .env("GIT_COMMITTER_EMAIL", "user@example.invalid")
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", "/dev/null")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap().trim().to_owned()
}

pub fn write(root: &Path, path: &str, body: impl AsRef<[u8]>) {
    let path = root.join(path);
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, body).unwrap();
}

pub fn read(root: &Path, path: &str) -> String {
    std::fs::read_to_string(root.join(path)).unwrap()
}

pub fn commit_all(dir: &Path, message: &str) -> String {
    git(dir, &["add", "-A"]);
    git(dir, &["commit", "-q", "--allow-empty", "-m", message]);
    git(dir, &["rev-parse", "HEAD"])
}

pub const CATALOG: &str = "https://schemas.example.test/catalog/1";

fn sample_template(dir: &Path, version: u8) {
    let extra = if version == 2 {
        "  - {id: extra, type: text, prompt: Extra, required: true}\n"
    } else {
        ""
    };
    let label = if version == 3 {
        "  - {id: label, type: text, prompt: Label, required: true, validate: {regex: '^[0-9]+$'}}\n"
    } else {
        "  - {id: label, type: text, prompt: Label, required: true}\n"
    };
    write(
        dir,
        "template.yml",
        format!(
            "name: sample\nschemas: schemas\ninterview:\n{label}  - {{id: note, type: text, prompt: Note}}\n  - {{id: catalog, type: document, prompt: Catalog, schema: '{CATALOG}', input: catalog.json}}\n{extra}hooks:\n  - run: [sh, -c, 'echo ran > hook-marker.txt']\n"
        ),
    );
    write(
        dir,
        "schemas/catalog.json",
        json!({"$id": CATALOG, "type": "object", "required": ["title"], "properties": {
            "title": {"type": "string"}, "size": {"type": "integer"},
            "offset": {"type": "integer"}, "ratio": {"type": "number"}}})
        .to_string(),
    );
    let extra_line = if version == 2 {
        "extra: {{ extra }}\n"
    } else {
        ""
    };
    write(
        dir,
        "template/result.txt",
        format!(
            "label: {{{{ label }}}}\nnote: {{{{ note }}}}\ntitle: {{{{ catalog.title }}}}\nsize: {{{{ catalog.size }}}}\nratio: {{{{ catalog.ratio }}}}\nhost: {{{{ toha_env_hostname }}}}\n{extra_line}"
        ),
    );
    write(
        dir,
        "template/local.txt",
        format!(
            "first: template\nsecond: stable\nthird: stable\nfourth: stable\nfifth: {}\n",
            if version == 2 { "new" } else { "original" }
        ),
    );
}

/// A template whose interview can stop or abort, and whose hook can fail.
fn flows_template(dir: &Path, hook: &str) {
    write(
        dir,
        "template.yml",
        format!(
            "name: flows\ninterview:\n  - {{id: label, type: text, prompt: Label, required: true}}\n  - flow: stop\n    when: label == 'stop'\n  - flow: abort\n    when: label == 'cancel'\n  - {{id: second, type: text, prompt: Second, required: true}}\nhooks:\n  - run: [sh, -c, '{hook}']\n    cwd: sub\n"
        ),
    );
    write(
        dir,
        "template/out.txt",
        "label: {{ label }}\nsecond: {{ second }}\nhost: {{ toha_env_hostname }}\n",
    );
    write(dir, "template/sub/kept.txt", "kept\n");
}

/// Template repositories, a workspace root and the service settings.
pub struct World {
    /// The workspace service's hook runner, as this test plays it.
    pub hooks: HookHost,
    pub temp: tempfile::TempDir,
    pub root: PathBuf,
    pub workspaces: PathBuf,
    pub sample: PathBuf,
    pub v1: String,
    pub v2: String,
    pub v3: String,
    pub socket: PathBuf,
    pub data: PathBuf,
    pub config: PathBuf,
}

impl World {
    pub fn new() -> Self {
        let temp = tempfile::tempdir().unwrap();
        let root = temp.path().canonicalize().unwrap();
        let sample = root.join("templates/sample");
        std::fs::create_dir_all(&sample).unwrap();
        git(&sample, &["init", "-q", "-b", "main"]);
        sample_template(&sample, 1);
        let v1 = commit_all(&sample, "v1");
        git(&sample, &["tag", "v1"]);
        git(&sample, &["checkout", "-q", "-b", "next"]);
        sample_template(&sample, 2);
        let v2 = commit_all(&sample, "v2");
        git(&sample, &["tag", "v2"]);
        git(&sample, &["checkout", "-q", "-b", "strict", &v1]);
        sample_template(&sample, 3);
        let v3 = commit_all(&sample, "v3");
        git(&sample, &["tag", "v3"]);
        git(&sample, &["checkout", "-q", "main"]);
        for (name, hook) in [
            ("flows", "echo ran > hook-marker.txt"),
            ("broken", "exit 3"),
        ] {
            let dir = root.join("templates").join(name);
            std::fs::create_dir_all(&dir).unwrap();
            git(&dir, &["init", "-q", "-b", "main"]);
            flows_template(&dir, hook);
            commit_all(&dir, "flows");
        }
        // A different repository whose address folds to the same Toha cache
        // key as `sample@v1`.
        let collision = root.join("templates/sample_v1");
        std::fs::create_dir_all(&collision).unwrap();
        git(&collision, &["init", "-q", "-b", "main"]);
        sample_template(&collision, 1);
        write(&collision, "template/local.txt", "first: collision\n");
        commit_all(&collision, "collision");
        // A clone of `sample` sharing its commits, whose address also folds
        // to `sample@v1`'s cache key.
        let fork = root.join("templates_sample/v1");
        std::fs::create_dir_all(fork.parent().unwrap()).unwrap();
        git(
            &root,
            &[
                "clone",
                "-q",
                sample.to_str().unwrap(),
                fork.to_str().unwrap(),
            ],
        );
        // A clone of `sample` whose repository name ends in `.git`, which
        // Toha's formal name drops.
        let dotted = root.join("templates/dotted.git");
        git(
            &root,
            &[
                "clone",
                "-q",
                sample.to_str().unwrap(),
                dotted.to_str().unwrap(),
            ],
        );
        let workspaces = root.join("workspaces");
        std::fs::create_dir_all(&workspaces).unwrap();
        let data = root.join("data");
        let socket = root.join("run/template.sock");
        let hook_socket = root.join("run/hook-runner.sock");
        let config = root.join("config.yml");
        let s = sample.display();
        let t = root.join("templates");
        let t = t.display();
        write(
            &root,
            "config.yml",
            format!(
                "workspace-root: {}\nworkspace-repository: git@example.invalid:sample/backups\nwyrwood-directory: /run/sample\noidc: {{issuer: 'https://issuer.example.invalid', client-id: sample}}\ntemplate-service: {{socket: {}, hook-runner-socket: {}, candidate-folder: {}}}\ntemplates:\n  - {{id: sample-v1, name: Sample, address: 'file://{s}@v1'}}\n  - {{id: sample-v2, name: Sample, address: 'file://{s}@v2'}}\n  - {{id: sample-v3, name: Sample, address: 'file://{s}@v3'}}\n  - {{id: sample-main, name: Sample, address: 'file://{s}'}}\n  - {{id: sample-commit, name: Sample, address: 'file://{s}@{v1}'}}\n  - {{id: flows, name: Flows, address: 'file://{t}/flows'}}\n  - {{id: flows-folder, name: Flows, address: '{t}/flows'}}\n  - {{id: broken, name: Broken, address: 'file://{t}/broken'}}\n  - {{id: collision, name: Collision, address: 'file://{t}/sample_v1'}}\n  - {{id: fork, name: Fork, address: 'file://{f}'}}\n  - {{id: dotted-v1, name: Dotted, address: 'file://{t}/dotted.git@v1'}}\n  - {{id: dotted-v2, name: Dotted, address: 'file://{t}/dotted.git@v2'}}\n  - {{id: dotted-main, name: Dotted, address: 'file://{t}/dotted.git'}}\n",
                workspaces.display(),
                socket.display(),
                hook_socket.display(),
                root.join("candidates").display(),
                v1 = v1,
                f = fork.display(),
            ),
        );
        let hooks = HookHost::start(&hook_socket);
        Self {
            hooks,
            temp,
            root,
            workspaces,
            sample,
            v1,
            v2,
            v3,
            socket,
            data,
            config,
        }
    }

    pub fn settings(&self) -> Settings {
        Settings::load(&self.config, &self.data).unwrap()
    }

    /// Install an approved address into the service's Toha registry, as an
    /// operator does with Toha's catalog.
    pub fn install(&self, address: &str) {
        let host = menagerie_template_service::host::TohaHost::new(&self.data);
        host.prepare().unwrap();
        let catalog = toha::catalog::Catalog::load(host.paths.clone(), &host.cwd).unwrap();
        toha::catalog::add(&catalog, address, None, false, &host.cwd).unwrap();
    }

    /// Make every template repository unreachable, as an offline remote is.
    pub fn offline(&self) {
        std::fs::rename(
            self.root.join("templates"),
            self.root.join("templates.offline"),
        )
        .unwrap();
    }

    pub fn address(&self, reference: &str) -> String {
        format!("file://{}{reference}", self.sample.display())
    }

    pub fn folder(&self, workspace: &str) -> PathBuf {
        self.workspaces.join(workspace)
    }

    /// What the workspace service does before generation: create the folder and
    /// initialize an unborn Git repository.
    pub fn initialize(&self, workspace: &str) -> PathBuf {
        let folder = self.folder(workspace);
        std::fs::create_dir_all(&folder).unwrap();
        git(&folder, &["init", "-q", "-b", workspace]);
        folder
    }

    pub fn select(&self, workspace: &str, template: &str) -> Value {
        json!({"workspace": workspace, "target": self.folder(workspace), "templateId": template})
    }

    /// The service's session key, from its data folder.
    pub fn key(&self) -> SessionKey {
        SessionKey::load_or_create(&self.data).unwrap()
    }

    /// Sign `record` for the selector's workspace and template in this flow,
    /// as only a holder of the service's key can, so that a test reaches the
    /// checks behind the signature.
    pub fn signed(&self, operation: Operation, selector: &Value, record: Value) -> Value {
        let binding = Binding::new(
            operation,
            selector["workspace"].as_str().unwrap(),
            selector["templateId"].as_str().unwrap(),
        );
        seal::sealed(&self.key(), binding, record).unwrap()
    }

    /// A session whose record `edit` changed, signed again for the selector
    /// in this flow.
    pub fn resigned(
        &self,
        operation: Operation,
        selector: &Value,
        session: &Value,
        edit: impl FnOnce(&mut Value),
    ) -> Value {
        let mut record = session["record"].clone();
        edit(&mut record);
        self.signed(operation, selector, record)
    }
}

/// The service running in this process on the world's socket.
pub struct Running {
    pub socket: PathBuf,
    stop: Option<tokio::sync::oneshot::Sender<()>>,
    task: Option<tokio::task::JoinHandle<std::io::Result<()>>>,
}

impl Running {
    pub async fn start(world: &World) -> Self {
        let service = Arc::new(Service::new(
            world.settings(),
            menagerie_template_service::seal::SessionKey::load_or_create(&world.data).unwrap(),
        ));
        service.host.prepare().unwrap();
        let listener = server::bind(&world.socket).await.unwrap();
        let (stop, stopped) = tokio::sync::oneshot::channel::<()>();
        let task = tokio::spawn(server::serve(listener, service, async {
            let _ = stopped.await;
        }));
        Self {
            socket: world.socket.clone(),
            stop: Some(stop),
            task: Some(task),
        }
    }

    /// A client of a service started elsewhere, such as the binary.
    pub fn attach(world: &World) -> Self {
        Self {
            socket: world.socket.clone(),
            stop: None,
            task: None,
        }
    }

    pub async fn stop(mut self) {
        let _ = self.stop.take().unwrap().send(());
        self.task.take().unwrap().await.unwrap().unwrap();
    }

    pub async fn post(&self, path: &str, body: &Value) -> Reply {
        post_bytes(&self.socket, path, body_of(path, body)).await
    }
}

/// The token the workspace service grants a generate or apply in these tests.
pub const HOOK_TOKEN: &str = "0123456789abcdef";

/// A request body as the workspace service sends it: a generate or apply
/// carries the hook token unless the test gave `hookToken` itself.
pub fn body_of(path: &str, body: &Value) -> Vec<u8> {
    let mut body = body.clone();
    if matches!(path, "/v1/workspaces/generate" | "/v1/upgrades/apply")
        && let Some(members) = body.as_object_mut()
        && !members.contains_key("hookToken")
    {
        members.insert("hookToken".into(), json!(HOOK_TOKEN));
    }
    serde_json::to_vec(&body).unwrap()
}

/// The workspace service's hook runner for these tests: it accepts
/// `HOOK_TOKEN` only and runs each hook as a local process in its folder,
/// with only `PATH` in its environment, where Menagerie runs a throwaway
/// container. It records every request it accepts.
pub struct HookHost {
    pub socket: PathBuf,
    pub calls: Arc<std::sync::Mutex<Vec<Value>>>,
    stop: Option<tokio::sync::oneshot::Sender<()>>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl HookHost {
    pub fn start(socket: &Path) -> Self {
        std::fs::create_dir_all(socket.parent().unwrap()).unwrap();
        let calls = Arc::new(std::sync::Mutex::new(Vec::new()));
        let (stop, stopped) = tokio::sync::oneshot::channel::<()>();
        let (ready, listening) = std::sync::mpsc::channel();
        let path = socket.to_owned();
        let recorded = calls.clone();
        let thread = std::thread::spawn(move || {
            let runtime = tokio::runtime::Builder::new_multi_thread()
                .worker_threads(2)
                .enable_all()
                .build()
                .unwrap();
            runtime.block_on(async move {
                let listener = tokio::net::UnixListener::bind(&path).unwrap();
                ready.send(()).unwrap();
                let app = axum::Router::new().route(
                    menagerie_template_service::hooks::HOOK_PATH,
                    axum::routing::post(move |body: axum::body::Bytes| {
                        let recorded = recorded.clone();
                        async move {
                            tokio::task::spawn_blocking(move || run_hook(&recorded, &body))
                                .await
                                .unwrap()
                        }
                    }),
                );
                axum::serve(listener, app)
                    .with_graceful_shutdown(async {
                        let _ = stopped.await;
                    })
                    .await
                    .unwrap();
            });
        });
        listening.recv().unwrap();
        Self {
            socket: socket.to_owned(),
            calls,
            stop: Some(stop),
            thread: Some(thread),
        }
    }

    pub fn calls(&self) -> Vec<Value> {
        self.calls.lock().unwrap().clone()
    }
}

impl Drop for HookHost {
    fn drop(&mut self) {
        if let Some(stop) = self.stop.take() {
            let _ = stop.send(());
        }
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

fn run_hook(
    recorded: &std::sync::Mutex<Vec<Value>>,
    body: &[u8],
) -> (
    axum::http::StatusCode,
    [(axum::http::HeaderName, &'static str); 1],
    Vec<u8>,
) {
    use axum::http::StatusCode;
    use base64::Engine;
    let reply = |status: StatusCode, body: Value| {
        (
            status,
            [(axum::http::header::CONTENT_TYPE, "application/json")],
            serde_json::to_vec(&body).unwrap(),
        )
    };
    let request: Value = serde_json::from_slice(body).unwrap();
    if request["token"] != HOOK_TOKEN {
        return reply(
            StatusCode::FORBIDDEN,
            json!({"error": {"kind": "hook-token-refused", "message": "the hook token is unknown or revoked"}}),
        );
    }
    recorded.lock().unwrap().push(request.clone());
    let argv: Vec<String> = match (&request["program"]["run"], &request["program"]["script"]) {
        (Value::Array(argv), Value::Null) => argv
            .iter()
            .map(|v| v.as_str().unwrap().to_owned())
            .collect(),
        (Value::Null, script) => std::iter::once(script["path"].as_str().unwrap().to_owned())
            .chain(
                script["args"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|v| v.as_str().unwrap().to_owned()),
            )
            .collect(),
        _ => panic!("program {}", request["program"]),
    };
    let mut dir = PathBuf::from(request["target"].as_str().unwrap());
    if let Some(cwd) = request["cwd"].as_str() {
        dir.push(cwd);
    }
    let output = Command::new(&argv[0])
        .args(&argv[1..])
        .current_dir(dir)
        .env_clear()
        .env("PATH", std::env::var_os("PATH").unwrap())
        .output()
        .unwrap();
    let stream = |captured: &Value, bytes: &[u8]| {
        if captured.as_bool().unwrap() {
            json!(base64::engine::general_purpose::STANDARD.encode(bytes))
        } else {
            Value::Null
        }
    };
    let code = output.status.code().unwrap_or(-1);
    reply(
        StatusCode::OK,
        json!({
            "exitCode": code,
            "success": code == 0,
            "stdout": stream(&request["capture"]["stdout"], &output.stdout),
            "stderr": stream(&request["capture"]["stderr"], &output.stderr),
        }),
    )
}

#[derive(Debug)]
pub struct Reply {
    pub status: u16,
    pub content_type: Option<String>,
    pub body: Vec<u8>,
    pub json: Value,
}

impl Reply {
    pub fn kind(&self) -> &str {
        self.json["error"]["kind"].as_str().unwrap_or("")
    }
    /// The reply's session as the exact bytes the service sent.
    pub fn session_bytes(&self) -> Vec<u8> {
        serde_json::to_vec(&self.json["session"]).unwrap()
    }
    /// The Toha staged record inside the reply's signed session.
    pub fn record(&self) -> &Value {
        &self.json["session"]["record"]
    }
}

pub async fn post_bytes(socket: &Path, path: &str, body: Vec<u8>) -> Reply {
    request(socket, "POST", path, body).await
}

pub async fn request(socket: &Path, method: &str, path: &str, body: Vec<u8>) -> Reply {
    let stream = tokio::net::UnixStream::connect(socket).await.unwrap();
    let (mut sender, connection) =
        hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(stream))
            .await
            .unwrap();
    tokio::spawn(connection);
    let request = hyper::Request::builder()
        .method(method)
        .uri(path)
        .header("host", "template-service")
        .header("content-type", "application/json")
        .body(Full::new(Bytes::from(body)))
        .unwrap();
    let response = sender.send_request(request).await.unwrap();
    let status = response.status().as_u16();
    let content_type = response
        .headers()
        .get("content-type")
        .map(|v| v.to_str().unwrap().to_owned());
    let body = response
        .into_body()
        .collect()
        .await
        .unwrap()
        .to_bytes()
        .to_vec();
    let json = serde_json::from_slice(&body).unwrap_or(Value::Null);
    Reply {
        status,
        content_type,
        body,
        json,
    }
}

/// An answers envelope for the reply's batch.
pub fn submission(reply: &Reply, answers: Value) -> Value {
    json!({"template": reply.json["document"]["context"]["template"], "answers": answers})
}

pub fn with(mut selector: Value, members: Value) -> Value {
    for (key, value) in members.as_object().unwrap() {
        selector[key] = value.clone();
    }
    selector
}

/// The batch's question ids, sorted.
pub fn questions(reply: &Reply) -> Vec<String> {
    let mut ids: Vec<String> = reply.json["document"]["schema"]["properties"]
        .as_object()
        .map(|p| p.keys().cloned().collect())
        .unwrap_or_default();
    ids.sort();
    ids
}

/// The single application in a workspace's applied record.
pub fn application(folder: &Path) -> Value {
    let record: Value = serde_json::from_str(&read(folder, ".toha.json")).unwrap();
    record["applications"][0].clone()
}

/// Answers for the sample template's first batch.
pub fn sample_answers(label: &str) -> Value {
    json!({"label": label, "note": null, "catalog": {"$schema": CATALOG, "title": "first",
        "size": 18446744073709551615u64, "offset": -9223372036854775808i64}})
}

/// Run a whole create through the service as the workspace service does:
/// interview, create and initialize the folder, generate, then commit. The
/// workspace then gets the document question's input file, as an operator
/// keeps it, so an upgrade admits it instead of asking again.
pub async fn create(world: &World, service: &Running, workspace: &str, template: &str) -> PathBuf {
    let selector = world.select(workspace, template);
    let start = service.post("/v1/interviews", &selector).await;
    assert_eq!(start.status, 200, "{}", start.json);
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
    assert_eq!(done.json["state"], "completed", "{}", done.json);
    let folder = world.initialize(workspace);
    let generated = service
        .post(
            "/v1/workspaces/generate",
            &with(selector, json!({"session": done.json["session"]})),
        )
        .await;
    assert_eq!(generated.json["outcome"], "written", "{}", generated.json);
    commit_all(&folder, "generated");
    write(
        &folder,
        "catalog.json",
        sample_answers("first")["catalog"].to_string(),
    );
    commit_all(&folder, "document input");
    folder
}

/// The host name Toha captures as `toha_env_hostname`.
pub fn hostname() -> String {
    rustix::system::uname()
        .nodename()
        .to_str()
        .unwrap()
        .to_owned()
}

/// Every entry of a folder, sorted.
pub fn entries(folder: &Path) -> Vec<String> {
    let mut names: Vec<String> = std::fs::read_dir(folder)
        .unwrap()
        .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    names
}

pub const BINARY: &str = env!("CARGO_BIN_EXE_menagerie-template-service");

/// A started binary, stopped when dropped so a failed assertion leaks no process.
pub struct Process(pub Child);

impl Drop for Process {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

/// Start the binary and return its first stderr line: its readiness line, or
/// the reason it refused to start.
pub fn start(world: &World) -> (Process, String) {
    let mut child = Command::new(BINARY)
        .arg("-config")
        .arg(&world.config)
        .arg("-data")
        .arg(&world.data)
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let mut line = String::new();
    BufReader::new(child.stderr.as_mut().unwrap())
        .read_line(&mut line)
        .unwrap();
    (Process(child), line.trim().to_owned())
}

pub fn launch(world: &World) -> Process {
    let (process, line) = start(world);
    assert_eq!(
        line,
        format!("template service listening on {}", world.socket.display())
    );
    process
}
