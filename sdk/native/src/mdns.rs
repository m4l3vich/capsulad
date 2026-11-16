//! mDNS discovery helpers over the framework's `DiscoveryService`.
//!
//! Requests and responses are canonical protobuf JSON; field names are passed
//! straight through, never translated. Two unary calls (`Publish`, `Unpublish`)
//! go through the normal client unary path, while `Browse` starts a
//! server-streaming call and leaves the handle to the caller, who reads
//! `DiscoveredService` events with `call_recv_json`.
//!
//! The service descriptor is provided by the daemon agent; this module compiles
//! and is exercised through the same dynamic pool as every other service.

use serde_json::Value;

use crate::call::{self, OutboundCall};
use crate::client::{ClientInner, CLIENTS};
use crate::ffi::{
    SdkError, SdkResult, END_OF_STREAM, GRPC, INVALID_ARGUMENT, INVALID_MESSAGE, NOT_FOUND,
};

pub const SERVICE: &str = "capsulad.framework.v1.DiscoveryService";
pub const SERVER_STREAM: i32 = 1;

/// Canonical gRPC codes for the two refusals apps must distinguish.
const GRPC_INVALID_ARGUMENT: i32 = 3;
const GRPC_NOT_FOUND: i32 = 5;

fn client(handle: u64) -> SdkResult<std::sync::Arc<ClientInner>> {
    let client = CLIENTS
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown client handle"))?;
    client.ensure_open()?;
    if client.is_remote() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "mDNS requires a connected supervised client, not a remote gRPC channel",
        ));
    }
    Ok(client)
}

/// Validates that a request is a JSON object; the daemon owns field semantics.
fn request_object(bytes: &[u8]) -> SdkResult<Vec<u8>> {
    let value: Value = serde_json::from_slice(bytes).map_err(|err| {
        SdkError::new(
            INVALID_ARGUMENT,
            format!("mDNS request is not valid JSON: {err}"),
        )
    })?;
    if !value.is_object() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "mDNS request must be a JSON object",
        ));
    }
    Ok(bytes.to_vec())
}

/// Maps a remote gRPC refusal onto the ABI status apps can branch on. Every
/// other code stays `GRPC` (the precise code remains available through
/// `capsulad_call_status` for calls that expose a handle).
fn map_remote_status(mut err: SdkError) -> SdkError {
    if err.status == GRPC {
        match err.grpc_code {
            Some(GRPC_INVALID_ARGUMENT) => err.status = INVALID_ARGUMENT,
            Some(GRPC_NOT_FOUND) => err.status = NOT_FOUND,
            _ => {}
        }
    }
    err
}

fn unary(
    client: std::sync::Arc<ClientInner>,
    method: &str,
    request: Vec<u8>,
    timeout_ms: u32,
) -> SdkResult<Vec<u8>> {
    let call = call::start_direct(
        client,
        SERVICE,
        method,
        0,
        call::deadline_to_duration(timeout_ms),
    )?;
    let result = (|| -> SdkResult<Vec<u8>> {
        call.send_json(&request)?;
        call.half_close()?;
        let first = call.recv(u32::MAX, true)?;
        match call.recv(u32::MAX, true) {
            Err(err) if err.status == END_OF_STREAM => Ok(first),
            Err(err) => Err(err),
            Ok(_) => Err(SdkError::new(
                INVALID_MESSAGE,
                "mDNS unary call returned more than one response message",
            )),
        }
    })();
    call.release_ref();
    result.map_err(map_remote_status)
}

/// `Publish` -> response JSON (`PublishServiceResponse`).
pub fn publish(handle: u64, request_json: &[u8], timeout_ms: u32) -> SdkResult<Vec<u8>> {
    let request = request_object(request_json)?;
    unary(client(handle)?, "Publish", request, timeout_ms)
}

/// `Unpublish` by publication id. The daemon owns the response.
pub fn unpublish(handle: u64, publication_id: &str, timeout_ms: u32) -> SdkResult<()> {
    if publication_id.is_empty() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "mdns_unpublish publication_id is empty",
        ));
    }
    let request = serde_json::to_vec(&serde_json::json!({
        "publicationId": publication_id,
    }))
    .map_err(|err| SdkError::new(INVALID_ARGUMENT, format!("mDNS request JSON: {err}")))?;
    unary(client(handle)?, "Unpublish", request, timeout_ms)?;
    Ok(())
}

/// Starts the server-streaming `Browse` call, sends the request and returns the
/// regular call handle. Events are `DiscoveredService` canonical JSON.
pub fn browse_open(handle: u64, request_json: &[u8], timeout_ms: u32) -> SdkResult<u64> {
    let request = request_object(request_json)?;
    let c = client(handle)?;
    let call: std::sync::Arc<OutboundCall> = call::start_direct(
        c,
        SERVICE,
        "Browse",
        SERVER_STREAM,
        call::deadline_to_duration(timeout_ms),
    )?;
    let call_handle = call.handle.load(std::sync::atomic::Ordering::Acquire);
    let started = (|| -> SdkResult<()> {
        call.send_json(&request)?;
        call.half_close()
    })();
    if let Err(err) = started {
        call.release_ref();
        return Err(err);
    }
    Ok(call_handle)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn remote_status_mapping_distinguishes_refusals() {
        let err = map_remote_status(SdkError::grpc("unknown service type", 3));
        assert_eq!(err.status, INVALID_ARGUMENT);
        assert_eq!(err.grpc_code, Some(3));
        assert_eq!(err.message, "unknown service type");

        let err = map_remote_status(SdkError::grpc("no such publication", 5));
        assert_eq!(err.status, NOT_FOUND);
        assert_eq!(err.grpc_code, Some(5));

        let err = map_remote_status(SdkError::grpc("permission denied", 7));
        assert_eq!(err.status, GRPC);
        assert_eq!(err.grpc_code, Some(7));

        let err = map_remote_status(SdkError::new(INVALID_ARGUMENT, "local validation"));
        assert_eq!(err.status, INVALID_ARGUMENT);
        assert_eq!(err.grpc_code, None);
    }
}
