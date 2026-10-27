//! Menagerie's template service. It embeds Toha and serves create interviews,
//! workspace generation and upgrades to the workspace service over a private
//! Unix socket. It keeps no state between requests: every request carries the
//! whole Toha session, signed by this service, and is rebuilt from it.
// A request produces at most one `ServiceError`, which is serialized at once;
// its size does not matter on this path.
#![allow(clippy::result_large_err)]
pub mod api;
pub mod checks;
pub mod config;
pub mod create;
pub mod credentials;
pub mod error;
pub mod hooks;
pub mod host;
pub mod numbers;
pub mod reply;
pub mod revision;
#[cfg(test)]
mod scripted;
pub mod seal;
pub mod server;
pub mod upgrade;

/// The settings, Toha host inputs and session key every request reads.
#[derive(Debug, Clone)]
pub struct Service {
    pub settings: config::Settings,
    pub host: host::TohaHost,
    pub key: seal::SessionKey,
}

impl Service {
    pub fn new(settings: config::Settings, key: seal::SessionKey) -> Self {
        let host = host::TohaHost::new(&settings.data);
        Self {
            settings,
            host,
            key,
        }
    }
}
