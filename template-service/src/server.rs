//! JSON over HTTP/1.1 on a Unix socket. Each request runs its Toha work on a
//! blocking worker, inside its own stop signal and credential resolver
//! scopes, and returns owned JSON.
use crate::Service;
use crate::credentials::Ended;
use crate::error::ServiceError;
use axum::Router;
use axum::body::Bytes;
use axum::extract::{DefaultBodyLimit, State};
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use serde::Serialize;
use serde::de::DeserializeOwned;
use std::os::unix::fs::FileTypeExt;
use std::path::Path;
use std::sync::Arc;
use tokio::net::{UnixListener, UnixStream};

pub const INTERVIEWS: &str = "/v1/interviews";
pub const INTERVIEW_ANSWERS: &str = "/v1/interviews/answers";
pub const GENERATE: &str = "/v1/workspaces/generate";
pub const UPGRADES: &str = "/v1/upgrades";
pub const UPGRADE_ANSWERS: &str = "/v1/upgrades/answers";
pub const UPGRADE_APPLY: &str = "/v1/upgrades/apply";

/// Every route, in documentation order.
pub const ROUTES: [&str; 6] = [
    INTERVIEWS,
    INTERVIEW_ANSWERS,
    GENERATE,
    UPGRADES,
    UPGRADE_ANSWERS,
    UPGRADE_APPLY,
];

pub fn router(service: Arc<Service>) -> Router {
    Router::new()
        .route(
            INTERVIEWS,
            post(|s: State<Arc<Service>>, b: Bytes| {
                call(s, b, |s, r, _| crate::create::start(s, r))
            }),
        )
        .route(
            INTERVIEW_ANSWERS,
            post(|s: State<Arc<Service>>, b: Bytes| {
                call(s, b, |s, r, _| crate::create::answer(s, r))
            }),
        )
        .route(
            GENERATE,
            post(|s: State<Arc<Service>>, b: Bytes| call(s, b, crate::create::generate)),
        )
        .route(
            UPGRADES,
            post(|s: State<Arc<Service>>, b: Bytes| {
                call(s, b, |s, r, _| crate::upgrade::start(s, r))
            }),
        )
        .route(
            UPGRADE_ANSWERS,
            post(|s: State<Arc<Service>>, b: Bytes| {
                call(s, b, |s, r, _| crate::upgrade::answer(s, r))
            }),
        )
        .route(
            UPGRADE_APPLY,
            post(|s: State<Arc<Service>>, b: Bytes| call(s, b, crate::upgrade::apply)),
        )
        .fallback(|| async { error(&ServiceError::new(404, "not-found", "no such route")) })
        .method_not_allowed_fallback(|| async {
            error(&ServiceError::new(
                405,
                "method-not-allowed",
                "routes accept POST only",
            ))
        })
        .layer(DefaultBodyLimit::disable())
        .with_state(service)
}

/// One route's work. It gets its caller's `Ended` signal, which the routes
/// that write to a folder also check before they start to write.
type Operation<Req, Rep> = fn(&Service, Req, &Ended) -> Result<Rep, ServiceError>;

async fn call<Req, Rep>(
    State(service): State<Arc<Service>>,
    body: Bytes,
    operation: Operation<Req, Rep>,
) -> Response
where
    Req: DeserializeOwned + Send + 'static,
    Rep: Serialize + Send + 'static,
{
    // The one way out of a route: every status and body, refusals and
    // internal errors included, loses each written form of the credentials
    // the operation's resolver handed out, whatever text carried them.
    let handed = crate::credentials::Handed::default();
    let (status, body) = run(service, body, operation, handed.clone()).await;
    json(status, handed.scrub(body))
}

async fn run<Req, Rep>(
    service: Arc<Service>,
    body: Bytes,
    operation: Operation<Req, Rep>,
    handed: crate::credentials::Handed,
) -> (StatusCode, Vec<u8>)
where
    Req: DeserializeOwned + Send + 'static,
    Rep: Serialize + Send + 'static,
{
    // The bounded scan runs first, so a body nested too deep is refused as
    // such even when it is also malformed; syntax is then judged before any
    // number refusal.
    let scanned = match crate::numbers::scan(&body) {
        Ok(scanned) => scanned,
        Err(refusal) => return refused(&refusal),
    };
    if let Err(e) = serde_json::from_slice::<serde::de::IgnoredAny>(&body) {
        return refused(&ServiceError::bad_request(
            "malformed-request",
            e.to_string(),
        ));
    }
    if let Err(refusal) = scanned.check() {
        return refused(&refusal);
    }
    let request: Req = match serde_json::from_slice(&body) {
        Ok(request) => request,
        Err(e) => {
            return refused(&ServiceError::bad_request(
                "malformed-request",
                e.to_string(),
            ));
        }
    };
    // Every Toha fetch the operation makes, on this worker, asks the
    // workspace service's credential route when an HTTPS remote challenges.
    // `_alive` lives as long as this future: when the caller goes away the
    // future is dropped, which fires Toha's stop signal synchronously and
    // ends the resolver's credential requests.
    let (_alive, ended) = crate::credentials::lifetime();
    let kept = handed.clone();
    let result = tokio::task::spawn_blocking(move || {
        let resolver =
            crate::credentials::resolver(&service.settings.hook_runner_socket, kept, ended.clone());
        ended.scope(|| {
            toha::source::with_credential_resolver(resolver, || {
                operation(&service, request, &ended)
            })
        })
    })
    .await;
    let reply = match result {
        Ok(Ok(reply)) => serde_json::to_vec(&reply)
            .map_err(|e| ServiceError::internal(format!("reply does not serialize: {e}"))),
        Ok(Err(refusal)) => Err(refusal),
        Err(join) => Err(ServiceError::internal(format!(
            "request worker failed: {join}"
        ))),
    };
    match reply {
        Ok(bytes) => (StatusCode::OK, bytes),
        // A failure after the resolver handed out a credential carries no
        // text from Toha, gix or the remote.
        Err(error) => refused(&handed.withhold(error)),
    }
}

fn refused(error: &ServiceError) -> (StatusCode, Vec<u8>) {
    let status = StatusCode::from_u16(error.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    (status, error.body())
}

fn json(status: StatusCode, body: Vec<u8>) -> Response {
    (status, [(header::CONTENT_TYPE, "application/json")], body).into_response()
}

fn error(error: &ServiceError) -> Response {
    let (status, body) = refused(error);
    json(status, body)
}

#[derive(Debug, thiserror::Error)]
pub enum BindError {
    #[error("{path}: {source}")]
    Io {
        path: std::path::PathBuf,
        source: std::io::Error,
    },
    #[error("{0}: another process is listening on this socket")]
    InUse(std::path::PathBuf),
    #[error("{0}: exists and is not a socket")]
    NotSocket(std::path::PathBuf),
}

/// Listen on `path`, creating its folder. A socket left by an ended process is
/// replaced; a live socket or any other file is refused.
pub async fn bind(path: &Path) -> Result<UnixListener, BindError> {
    let io = |source| BindError::Io {
        path: path.to_owned(),
        source,
    };
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).map_err(io)?;
    }
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.file_type().is_socket() => {
            if UnixStream::connect(path).await.is_ok() {
                return Err(BindError::InUse(path.to_owned()));
            }
            std::fs::remove_file(path).map_err(io)?;
        }
        Ok(_) => return Err(BindError::NotSocket(path.to_owned())),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(io(e)),
    }
    UnixListener::bind(path).map_err(io)
}

/// Serve until `shutdown` resolves.
pub async fn serve(
    listener: UnixListener,
    service: Arc<Service>,
    shutdown: impl std::future::Future<Output = ()> + Send + 'static,
) -> std::io::Result<()> {
    axum::serve(listener, router(service))
        .with_graceful_shutdown(shutdown)
        .await
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;
    use std::time::{Duration, Instant};

    // Use the real request worker and Toha file loop, pausing only in the
    // reporting callback after the first file. Dropping the request must
    // stop the next file even though no hook or network call intervenes.
    fn files(service: &Service, root: PathBuf, ended: &Ended) -> Result<(), ServiceError> {
        let template = toha::Template::load(&root.join("source"))?;
        let (config, _, _) = service.host.context()?;
        let interview = toha::interview::configured_defaults(
            "sample",
            &template,
            &config.presets,
            &config.template_defaults,
        )
        .unwrap()
        .start(&template, crate::host::now())
        .unwrap();
        let toha::Interview::Complete(done) = interview else {
            panic!("empty interview")
        };
        let target = toha::staging::canonical_target(&root.join("target")).unwrap();
        let plan = toha::Plan::build(&template, &done, &target).unwrap();
        let result = plan
            .apply_reporting(
                &target,
                toha::ApplyOptions::default(),
                &toha::hook::RecordingRunner::default(),
                &mut |_| {
                    std::fs::write(root.join("first-written"), "").unwrap();
                    let deadline = Instant::now() + Duration::from_secs(5);
                    while ended.check().is_ok() {
                        assert!(Instant::now() < deadline, "request never ended");
                        std::thread::sleep(Duration::from_millis(1));
                    }
                },
            )
            .map_err(ServiceError::from);
        std::fs::write(
            root.join("result"),
            match &result {
                Ok(_) => "written",
                Err(e) => e.kind,
            },
        )
        .unwrap();
        result.map(|_| ())
    }

    #[test]
    fn dropping_a_request_between_files_stops_the_loop_and_returns_its_worker() {
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .max_blocking_threads(1)
            .enable_all()
            .build()
            .unwrap()
            .block_on(async {
                let temp = tempfile::tempdir().unwrap();
                let root = temp.path().canonicalize().unwrap();
                std::fs::create_dir_all(root.join("source/template")).unwrap();
                std::fs::create_dir(root.join("target")).unwrap();
                std::fs::write(
                    root.join("source/template.yml"),
                    "name: sample\ninterview: []\n",
                )
                .unwrap();
                for name in ["a.txt", "b.txt", "c.txt"] {
                    std::fs::write(root.join("source/template").join(name), name).unwrap();
                }
                let settings = crate::config::Settings::parse(
                    &format!("workspace-root: {}", root.join("target").display()),
                    &root.join("data"),
                )
                .unwrap();
                let key = crate::seal::SessionKey::load_or_create(&settings.data).unwrap();
                let service = Arc::new(Service::new(settings, key));
                service.host.prepare().unwrap();
                let body = serde_json::to_vec(&root).unwrap();
                let caller = tokio::spawn(run(service, body.into(), files, Default::default()));
                tokio::time::timeout(Duration::from_secs(5), async {
                    while !root.join("first-written").exists() {
                        tokio::time::sleep(Duration::from_millis(1)).await;
                    }
                })
                .await
                .expect("first write never arrived");
                caller.abort();
                assert!(caller.await.unwrap_err().is_cancelled());
                tokio::time::timeout(Duration::from_secs(2), tokio::task::spawn_blocking(|| ()))
                    .await
                    .expect("request worker stayed blocked")
                    .unwrap();
                assert_eq!(
                    std::fs::read_to_string(root.join("result")).unwrap(),
                    "request-ended"
                );
                assert_eq!(
                    std::fs::read_dir(root.join("target")).unwrap().count(),
                    1,
                    "the request wrote another file after it ended"
                );
            });
    }
}
