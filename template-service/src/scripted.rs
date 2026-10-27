//! A route on a Unix socket that answers every request with one scripted
//! status and body and keeps the requests, as the workspace service's hook
//! runner socket plays it in unit tests.
use serde_json::Value;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

pub struct Scripted {
    pub socket: PathBuf,
    requests: Arc<Mutex<Vec<Value>>>,
    _dir: tempfile::TempDir,
    stop: Option<tokio::sync::oneshot::Sender<()>>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl Scripted {
    pub fn start(route: &'static str, status: u16, reply: Vec<u8>) -> Self {
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
                    route,
                    axum::routing::post(move |body: axum::body::Bytes| {
                        kept.lock()
                            .unwrap()
                            .push(serde_json::from_slice(&body).unwrap());
                        let reply = reply.clone();
                        async move {
                            (
                                axum::http::StatusCode::from_u16(status).unwrap(),
                                [(axum::http::header::CONTENT_TYPE, "application/json")],
                                reply,
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

    pub fn requests(&self) -> Vec<Value> {
        self.requests.lock().unwrap().clone()
    }
}

impl Drop for Scripted {
    fn drop(&mut self) {
        let _ = self.stop.take().unwrap().send(());
        let _ = self.thread.take().unwrap().join();
    }
}
