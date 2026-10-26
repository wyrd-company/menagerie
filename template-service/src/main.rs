use menagerie_template_service::{Service, config, seal, server};
use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::Arc;

const USAGE: &str = "usage: menagerie-template-service [-config FILE] [-data FOLDER]";

fn arguments() -> Result<(PathBuf, PathBuf), String> {
    let mut config = PathBuf::from(config::DEFAULT_CONFIG);
    let mut data = PathBuf::from(config::DEFAULT_DATA);
    let mut args = std::env::args_os().skip(1);
    while let Some(flag) = args.next() {
        let slot = match flag.to_str() {
            Some("-config" | "--config") => &mut config,
            Some("-data" | "--data") => &mut data,
            _ => return Err(format!("unknown argument {flag:?}\n{USAGE}")),
        };
        *slot = args
            .next()
            .map(PathBuf::from)
            .ok_or_else(|| format!("{flag:?} needs a value\n{USAGE}"))?;
    }
    Ok((config, data))
}

#[tokio::main]
async fn main() -> ExitCode {
    let (config_path, data) = match arguments() {
        Ok(values) => values,
        Err(message) => {
            eprintln!("{message}");
            return ExitCode::from(2);
        }
    };
    let settings = match config::Settings::load(&config_path, &data) {
        Ok(settings) => settings,
        Err(e) => {
            eprintln!("template service: {e}");
            return ExitCode::from(2);
        }
    };
    let key = match seal::SessionKey::load_or_create(&settings.data) {
        Ok(key) => key,
        Err(e) => {
            eprintln!("template service: {}: {e}", settings.data.display());
            return ExitCode::FAILURE;
        }
    };
    let service = Service::new(settings, key);
    if let Err(e) = service.host.prepare() {
        eprintln!("template service: {}: {e}", service.settings.data.display());
        return ExitCode::FAILURE;
    }
    let socket = service.settings.socket.clone();
    let listener = match server::bind(&socket).await {
        Ok(listener) => listener,
        Err(e) => {
            eprintln!("template service: {e}");
            return ExitCode::FAILURE;
        }
    };
    eprintln!("template service listening on {}", socket.display());
    let served = server::serve(listener, Arc::new(service), shutdown()).await;
    let _ = std::fs::remove_file(&socket);
    match served {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("template service: {e}");
            ExitCode::FAILURE
        }
    }
}

async fn shutdown() {
    use tokio::signal::unix::{SignalKind, signal};
    let mut terminate = signal(SignalKind::terminate()).expect("terminate signal handler");
    tokio::select! {
        _ = terminate.recv() => {}
        _ = tokio::signal::ctrl_c() => {}
    }
}
