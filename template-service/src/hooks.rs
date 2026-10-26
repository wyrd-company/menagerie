//! Template hooks run by the workspace service. The service never starts a
//! hook process itself: each hook goes over the hook runner socket to the
//! workspace service, which runs it in a throwaway container in the
//! workspace folder and returns its exit code and captured streams.
use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use http_body_util::{BodyExt, Full};
use hyper::body::Bytes;
use hyper_util::rt::TokioIo;
use serde::{Deserialize, Serialize};
use std::path::Path;
use toha::hook::{HookError, HookOutcome, HookRunner};
use toha::plan::{PlannedHook, PlannedProgram};

/// The hook runner's one route.
pub const HOOK_PATH: &str = "/v1/hooks/run";

/// Toha's hook runner for one generate or apply request. `folder` is the
/// workspace folder as configured, the path the containers mount; `token`
/// is the request's hook grant.
pub struct CallbackRunner<'a> {
    pub socket: &'a Path,
    pub folder: &'a Path,
    pub token: Option<&'a str>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct HookRequest<'a> {
    token: &'a str,
    target: &'a Path,
    program: Program<'a>,
    cwd: Option<String>,
    template_root: &'a Path,
    capture: Capture,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
enum Program<'a> {
    Run(&'a [String]),
    Script { path: &'a Path, args: &'a [String] },
}

#[derive(Serialize)]
struct Capture {
    stdout: bool,
    stderr: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
struct HookReply {
    exit_code: i32,
    success: bool,
    stdout: Option<String>,
    stderr: Option<String>,
}

#[derive(Deserialize)]
struct Refusal {
    error: RefusalBody,
}

#[derive(Deserialize)]
struct RefusalBody {
    kind: String,
    message: String,
}

fn failure(message: impl Into<String>) -> HookError {
    HookError::Io(std::io::Error::other(message.into()))
}

impl HookRunner for CallbackRunner<'_> {
    /// No error built here carries a captured stream: a reply's streams are
    /// decoded only into the outcome.
    fn run(&self, hook: &PlannedHook, _target: &Path) -> Result<HookOutcome, HookError> {
        let Some(token) = self.token else {
            return Err(failure(
                "the request carries no hook token, so the workspace service runs none of its hooks",
            ));
        };
        let program = match &hook.program {
            PlannedProgram::Run(argv) => {
                if argv.first().is_none_or(|p| p.is_empty()) {
                    return Err(HookError::EmptyCommand);
                }
                Program::Run(argv)
            }
            PlannedProgram::Script { path, args } => Program::Script { path, args },
        };
        let request = HookRequest {
            token,
            target: self.folder,
            program,
            cwd: hook.cwd.as_ref().map(ToString::to_string),
            template_root: &hook.template_root,
            capture: Capture {
                stdout: hook.capture.stdout,
                stderr: hook.capture.stderr,
            },
        };
        let body = serde_json::to_vec(&request)
            .map_err(|_| failure("the hook's paths are not UTF-8, so it cannot be sent"))?;
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_io()
            .build()?;
        let (status, reply) = runtime.block_on(post(self.socket, body))?;
        if status != 200 {
            return Err(match serde_json::from_slice::<Refusal>(&reply) {
                Ok(r) => failure(format!(
                    "the workspace service refused the hook ({}): {}",
                    r.error.kind, r.error.message
                )),
                Err(_) => failure(format!(
                    "the workspace service's hook runner answered {status} without an error"
                )),
            });
        }
        let reply: HookReply = serde_json::from_slice(&reply)
            .map_err(|_| failure("the workspace service's hook runner gave a malformed reply"))?;
        let stdout = stream(reply.stdout, hook.capture.stdout, "stdout")?;
        let stderr = stream(reply.stderr, hook.capture.stderr, "stderr")?;
        Ok(HookOutcome {
            success: reply.success,
            code: Some(reply.exit_code),
            stdout,
            stderr,
        })
    }
}

/// A reply stream: present exactly when the hook captures it.
fn stream(value: Option<String>, captured: bool, name: &str) -> Result<Option<Vec<u8>>, HookError> {
    match (value, captured) {
        (Some(text), true) => STANDARD
            .decode(text)
            .map(Some)
            .map_err(|_| failure(format!("the hook runner's {name} is not base64"))),
        (None, false) => Ok(None),
        _ => Err(failure(format!(
            "the hook runner's {name} does not match the hook's capture"
        ))),
    }
}

/// POST `body` to the hook runner and read the whole reply. The reply
/// waits as long as the hook runs; the workspace service ends it when the
/// job's limit or its cancellation stops the hook.
async fn post(socket: &Path, body: Vec<u8>) -> std::io::Result<(u16, Bytes)> {
    let unavailable = |e: &dyn std::fmt::Display| {
        std::io::Error::other(format!("the workspace service's hook runner: {e}"))
    };
    let stream = tokio::net::UnixStream::connect(socket)
        .await
        .map_err(|e| unavailable(&e))?;
    let (mut sender, connection) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
        .await
        .map_err(|e| unavailable(&e))?;
    tokio::spawn(connection);
    let request = hyper::Request::post(HOOK_PATH)
        .header(hyper::header::HOST, "workspace-service")
        .header(hyper::header::CONTENT_TYPE, "application/json")
        .body(Full::new(Bytes::from(body)))
        .map_err(|e| unavailable(&e))?;
    let response = sender
        .send_request(request)
        .await
        .map_err(|e| unavailable(&e))?;
    let status = response.status().as_u16();
    let bytes = response
        .into_body()
        .collect()
        .await
        .map_err(|e| unavailable(&e))?
        .to_bytes();
    Ok((status, bytes))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};
    use std::sync::{Arc, Mutex};
    use toha::plan::TargetPath;
    use toha::template::Capture as Captured;

    /// A hook runner that answers every request with `status` and `reply`
    /// and keeps the requests.
    struct Scripted {
        socket: std::path::PathBuf,
        requests: Arc<Mutex<Vec<Value>>>,
        _dir: tempfile::TempDir,
        stop: Option<tokio::sync::oneshot::Sender<()>>,
        thread: Option<std::thread::JoinHandle<()>>,
    }

    impl Scripted {
        fn start(status: u16, reply: Value) -> Self {
            let dir = tempfile::tempdir().unwrap();
            let socket = dir.path().join("hook-runner.sock");
            let requests = Arc::new(Mutex::new(Vec::new()));
            let (stop, stopped) = tokio::sync::oneshot::channel::<()>();
            let (ready, listening) = std::sync::mpsc::channel();
            let (path, kept) = (socket.clone(), requests.clone());
            let thread = std::thread::spawn(move || {
                let runtime = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .unwrap();
                runtime.block_on(async move {
                    let listener = tokio::net::UnixListener::bind(&path).unwrap();
                    ready.send(()).unwrap();
                    let app = axum::Router::new().route(
                        HOOK_PATH,
                        axum::routing::post(move |body: axum::body::Bytes| {
                            kept.lock()
                                .unwrap()
                                .push(serde_json::from_slice(&body).unwrap());
                            let reply = reply.clone();
                            async move {
                                (
                                    axum::http::StatusCode::from_u16(status).unwrap(),
                                    [(axum::http::header::CONTENT_TYPE, "application/json")],
                                    serde_json::to_vec(&reply).unwrap(),
                                )
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
                socket,
                requests,
                _dir: dir,
                stop: Some(stop),
                thread: Some(thread),
            }
        }

        fn run(&self, hook: &PlannedHook, token: Option<&str>) -> Result<HookOutcome, HookError> {
            CallbackRunner {
                socket: &self.socket,
                folder: Path::new("/srv/spaces/alpha"),
                token,
            }
            .run(hook, Path::new("/real/spaces/alpha"))
        }

        fn requests(&self) -> Vec<Value> {
            self.requests.lock().unwrap().clone()
        }
    }

    impl Drop for Scripted {
        fn drop(&mut self) {
            let _ = self.stop.take().unwrap().send(());
            let _ = self.thread.take().unwrap().join();
        }
    }

    fn hook(program: PlannedProgram, capture: Captured) -> PlannedHook {
        PlannedHook {
            program,
            cwd: Some(TargetPath::parse("sub/dir").unwrap()),
            template_root: "/var/lib/sample/cache/t".into(),
            id: None,
            capture,
            allow_failure: false,
            parse_json: false,
            status_id: None,
        }
    }

    fn run_hook(capture: Captured) -> PlannedHook {
        hook(
            PlannedProgram::Run(vec!["sh".into(), "-c".into(), "exit 0".into()]),
            capture,
        )
    }

    const SECRET: &str = "c2VjcmV0LW91dHB1dA=="; // base64 of "secret-output"

    #[test]
    fn a_hook_is_sent_with_its_grant_folder_and_capture() {
        let runner = Scripted::start(
            200,
            json!({"exitCode": 3, "success": false, "stdout": SECRET, "stderr": null}),
        );
        let outcome = runner
            .run(
                &run_hook(Captured {
                    stdout: true,
                    stderr: false,
                }),
                Some("token-1"),
            )
            .unwrap();
        assert_eq!(
            (
                outcome.success,
                outcome.code,
                outcome.stdout.as_deref(),
                outcome.stderr
            ),
            (false, Some(3), Some(b"secret-output".as_slice()), None)
        );
        assert_eq!(
            runner.requests(),
            [json!({"token": "token-1", "target": "/srv/spaces/alpha",
                "program": {"run": ["sh", "-c", "exit 0"]}, "cwd": "sub/dir",
                "templateRoot": "/var/lib/sample/cache/t",
                "capture": {"stdout": true, "stderr": false}})]
        );
        let script = hook(
            PlannedProgram::Script {
                path: "/var/lib/sample/cache/t/hooks/run.sh".into(),
                args: vec!["a b".into()],
            },
            Captured::default(),
        );
        let runner = Scripted::start(
            200,
            json!({"exitCode": 0, "success": true, "stdout": null, "stderr": null}),
        );
        let outcome = runner.run(&script, Some("token-1")).unwrap();
        assert_eq!((outcome.success, outcome.code), (true, Some(0)));
        assert_eq!(
            runner.requests()[0]["program"],
            json!({"script": {"path": "/var/lib/sample/cache/t/hooks/run.sh", "args": ["a b"]}})
        );
    }

    #[test]
    fn without_a_token_no_hook_is_sent() {
        let runner = Scripted::start(
            200,
            json!({"exitCode": 0, "success": true, "stdout": null, "stderr": null}),
        );
        let error = runner
            .run(&run_hook(Captured::default()), None)
            .unwrap_err();
        assert!(error.to_string().contains("no hook token"), "{error}");
        assert!(runner.requests().is_empty());
    }

    #[test]
    fn a_refusal_names_its_kind_and_message() {
        let runner = Scripted::start(
            403,
            json!({"error": {"kind": "hook-token-refused", "message": "the hook token is unknown or revoked"}}),
        );
        let error = runner
            .run(&run_hook(Captured::default()), Some("token-1"))
            .unwrap_err();
        assert_eq!(
            error.to_string(),
            "the workspace service refused the hook (hook-token-refused): the hook token is unknown or revoked"
        );
        let runner = Scripted::start(502, json!(["secret reply body"]));
        let error = runner
            .run(&run_hook(Captured::default()), Some("token-1"))
            .unwrap_err();
        assert!(error.to_string().contains("answered 502"), "{error}");
        assert!(!error.to_string().contains("secret"), "{error}");
    }

    /// A reply that does not match the hook's capture, or is not a reply,
    /// fails the hook without the stream in the error.
    #[test]
    fn a_mismatched_or_malformed_reply_fails_without_its_streams() {
        for (capture, reply) in [
            (
                Captured::default(),
                json!({"exitCode": 0, "success": true, "stdout": SECRET, "stderr": null}),
            ),
            (
                Captured {
                    stdout: false,
                    stderr: true,
                },
                json!({"exitCode": 0, "success": true, "stdout": null, "stderr": null}),
            ),
            (
                Captured {
                    stdout: true,
                    stderr: false,
                },
                json!({"exitCode": 0, "success": true, "stdout": "not base64 secret-output", "stderr": null}),
            ),
            (
                Captured::default(),
                json!({"exitCode": 0, "success": true, "stdout": null, "stderr": null, "extra": SECRET}),
            ),
        ] {
            let runner = Scripted::start(200, reply.clone());
            let error = runner.run(&run_hook(capture), Some("token-1")).unwrap_err();
            let text = format!("{error} {error:?}");
            assert!(
                !text.contains("secret") && !text.contains(SECRET),
                "{reply}: {text}"
            );
        }
    }

    #[test]
    fn an_unreachable_hook_runner_fails_the_hook() {
        let dir = tempfile::tempdir().unwrap();
        let error = CallbackRunner {
            socket: &dir.path().join("absent.sock"),
            folder: Path::new("/srv/spaces/alpha"),
            token: Some("token-1"),
        }
        .run(
            &run_hook(Captured::default()),
            Path::new("/srv/spaces/alpha"),
        )
        .unwrap_err();
        assert!(
            error
                .to_string()
                .starts_with("the workspace service's hook runner: "),
            "{error}"
        );
    }
}
