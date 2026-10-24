//! Host inputs the service gives Toha on every request: storage paths, host facts,
//! the fixed environment values and the clock.
use std::path::{Path, PathBuf};
use toha::context::{FixedEnvironment, FixedEnvironmentSource, HostFacts};
use toha::resolve::Paths;

/// Toha's storage for this service, all under one data folder. Resolution uses
/// the data folder as its working directory, so no workspace-local Toha
/// configuration is read.
#[derive(Debug, Clone)]
pub struct TohaHost {
    pub paths: Paths,
    pub cwd: PathBuf,
}

impl TohaHost {
    pub fn new(data: &Path) -> Self {
        Self {
            paths: Paths {
                system_config: data.join("system/config.yml"),
                user_config: data.join("config/config.yml"),
                local_config_override: None,
                system_data: data.join("system/data"),
                user_data: data.join("data"),
                cache: data.join("cache"),
                state: data.join("state"),
                home: data.join("home"),
            },
            cwd: data.to_owned(),
        }
    }

    /// Create the working directory Toha resolves against.
    pub fn prepare(&self) -> std::io::Result<()> {
        std::fs::create_dir_all(&self.cwd)
    }

    pub fn context(
        &self,
    ) -> Result<
        (toha::config::Config, toha::registry::Registry, Vec<String>),
        toha::resolve::ResolveError,
    > {
        toha::resolve::load_context(&self.paths, &self.cwd)
    }
}

/// The five fixed environment values, read from the service's process
/// environment and the native hostname, as Toha's command adapter reads them.
/// Toha calls it only under the stage grant the approved template carries.
#[derive(Debug, Default)]
pub struct ProcessEnvironment;

impl FixedEnvironmentSource for ProcessEnvironment {
    fn capture(&mut self) -> FixedEnvironment {
        FixedEnvironment::new(
            env_value("USER"),
            hostname(),
            env_value("EDITOR"),
            env_value("SHELL"),
            env_value("VISUAL"),
        )
    }
}

fn env_value(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|value| !value.is_empty())
}

fn hostname() -> Option<String> {
    rustix::system::uname()
        .nodename()
        .to_str()
        .ok()
        .map(str::to_owned)
        .filter(|value| !value.is_empty())
}

/// Effective administrative authority: effective UID zero.
pub fn is_admin() -> bool {
    rustix::process::geteuid().is_root()
}

pub fn host_facts() -> HostFacts {
    HostFacts::capture()
}

pub fn now() -> jiff::Zoned {
    jiff::Zoned::now()
}

/// A person answers Menagerie interviews through the web UI.
pub const INTERACTIVE: bool = true;
