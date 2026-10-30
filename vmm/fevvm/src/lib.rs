#![cfg(all(target_os = "macos", target_arch = "aarch64"))]

pub mod boot;
pub mod bus;
pub mod config;
pub mod console;
pub mod control;
pub mod devices;
pub mod exit;
pub mod fdt;
#[cfg(any(test, feature = "fuzzing"))]
pub mod fuzzing;
pub mod hops;
pub mod hvf;
pub mod irq;
pub mod layout;
pub mod lifeline;
pub mod machine;
pub mod memory;
pub mod pause;
pub mod psci;
pub mod space;
pub mod stage;
pub mod terminal;
pub mod trace;
pub mod virtio;
pub mod vmmctl;

use std::sync::OnceLock;
use std::time::Duration;

static LOG: OnceLock<console::ConsoleOut> = OnceLock::new();

pub fn log(args: std::fmt::Arguments) {
    let eol = if terminal::is_raw() { "\r\n" } else { "\n" };
    LOG.get_or_init(|| console::ConsoleOut::new(Box::new(std::io::stderr()), None))
        .write(format!("fevvm: {args}{eol}").as_bytes());
}

pub fn flush_log() {
    if let Some(log) = LOG.get() {
        log.flush(Duration::from_secs(2));
    }
}

#[macro_export]
macro_rules! vmm_log {
    ($($arg:tt)*) => {
        $crate::log(format_args!($($arg)*))
    };
}
