//! JSON over HTTP/1.1 on a Unix socket. Each request runs its Toha work on a
//! blocking worker, inside its own credential resolver scope, and returns
//! owned JSON.
use crate::Service;
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
            post(|s: State<Arc<Service>>, b: Bytes| call(s, b, crate::create::start)),
        )
        .route(
            INTERVIEW_ANSWERS,
            post(|s: State<Arc<Service>>, b: Bytes| call(s, b, crate::create::answer)),
        )
        .route(
            GENERATE,
            post(|s: State<Arc<Service>>, b: Bytes| call(s, b, crate::create::generate)),
        )
        .route(
            UPGRADES,
            post(|s: State<Arc<Service>>, b: Bytes| call(s, b, crate::upgrade::start)),
        )
        .route(
            UPGRADE_ANSWERS,
            post(|s: State<Arc<Service>>, b: Bytes| call(s, b, crate::upgrade::answer)),
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

async fn call<Req, Rep>(
    State(service): State<Arc<Service>>,
    body: Bytes,
    operation: fn(&Service, Req) -> Result<Rep, ServiceError>,
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
    operation: fn(&Service, Req) -> Result<Rep, ServiceError>,
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
    // future is dropped, which ends the resolver's credential requests.
    let (_alive, ended) = crate::credentials::lifetime();
    let kept = handed.clone();
    let result = tokio::task::spawn_blocking(move || {
        let resolver =
            crate::credentials::resolver(&service.settings.hook_runner_socket, kept, ended);
        toha::source::with_credential_resolver(resolver, || operation(&service, request))
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
