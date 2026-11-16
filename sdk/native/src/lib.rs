//! Capsulad native SDK: a stable C ABI over the framework's UDS gRPC engine,
//! plus the outbound networking apps would otherwise bundle.
//!
//! The library owns one bounded Tokio runtime, protobuf reflection for every
//! framework service, all four gRPC call shapes, the managed AppBus session,
//! a raw-PCM audio fast path, a verified HTTP/1.1 client, remote gRPC channels
//! (TLS/h2 or h2c) and small encoding/crypto/URL primitives. Handles crossing
//! the ABI are integers; no Rust objects are exposed to callers.

pub mod api;
pub mod audio;
pub mod bus;
pub mod call;
pub mod client;
pub mod control;
pub mod dynamic;
pub mod ffi;
pub mod grpc_remote;
pub mod headers;
pub mod http;
pub mod listener;
pub mod mdns;
pub mod primitives;
pub mod registry;
pub mod runtime;
pub mod stream;
pub mod threads;
pub mod tls;
pub mod udp;

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
