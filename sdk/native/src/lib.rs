//! Capsula native SDK: a stable C ABI over the framework's UDS gRPC engine.
//!
//! The library owns one bounded Tokio runtime, protobuf reflection for every
//! framework service, all four gRPC call shapes, the managed AppBus session,
//! and a raw-PCM audio fast path. Handles crossing the ABI are integers; no
//! Rust objects are exposed to callers.

pub mod api;
pub mod audio;
pub mod bus;
pub mod call;
pub mod client;
pub mod dynamic;
pub mod ffi;
pub mod registry;
pub mod runtime;
pub mod threads;

#[cfg(unix)]
pub fn runtime_monotonic_ns() -> u64 {
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    let rc = unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut ts) };
    if rc != 0 {
        return 0;
    }
    (ts.tv_sec as u64)
        .saturating_mul(1_000_000_000)
        .saturating_add(ts.tv_nsec as u64)
}

#[cfg(not(unix))]
pub fn runtime_monotonic_ns() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_nanos() as u64)
        .unwrap_or(0)
}
