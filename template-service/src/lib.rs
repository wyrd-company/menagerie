//! Menagerie's template service. It embeds Toha and serves create interviews,
//! workspace generation and upgrades to the workspace service over a private
//! Unix socket. It keeps no state between requests: every request carries the
//! whole Toha session and is rebuilt from it.
// A request produces at most one `ServiceError`, which is serialized at once;
// its size does not matter on this path.
#![allow(clippy::result_large_err)]
pub mod api;
pub mod checks;
pub mod config;
pub mod create;
pub mod error;
pub mod host;
pub mod numbers;
pub mod reply;
pub mod revision;
pub mod server;
pub mod upgrade;

/// The settings and Toha host inputs every request reads.
#[derive(Debug, Clone)]
pub struct Service {
    pub settings: config::Settings,
    pub host: host::TohaHost,
}

impl Service {
    pub fn new(settings: config::Settings) -> Self {
        let host = host::TohaHost::new(&settings.data);
        Self { settings, host }
    }
}
