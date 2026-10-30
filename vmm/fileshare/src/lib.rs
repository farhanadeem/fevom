#![cfg(target_os = "macos")]

pub mod abi;
pub mod errno;
pub mod fake;
pub mod feed;
pub mod fs;
pub mod fsevents;
pub mod iov;
pub mod passthrough;
pub mod rosetta;
pub mod server;
pub mod watcher;

#[cfg(any(test, feature = "testing"))]
pub mod client;
#[cfg(any(test, feature = "testing"))]
pub mod fuzzing;

pub use errno::Errno;
pub use fs::Filesystem;
pub use server::{Request, Server};
