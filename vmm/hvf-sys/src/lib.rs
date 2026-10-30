#![cfg(all(target_os = "macos", target_arch = "aarch64"))]
#![allow(non_camel_case_types, non_upper_case_globals, non_snake_case)]
#![allow(clippy::all)]

mod bindings;
pub use bindings::*;

#[link(name = "Hypervisor", kind = "framework")]
extern "C" {}

extern "C" {
    pub fn os_release(object: *mut std::ffi::c_void);
}
