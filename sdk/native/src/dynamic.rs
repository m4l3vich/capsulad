//! Protobuf reflection: embedded descriptors, JSON conversion, bus frames.

use std::sync::LazyLock;

use bytes::Bytes;
use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage, MessageDescriptor, MethodDescriptor, Value};
use prost_types::FileDescriptorSet;

use crate::ffi::{SdkError, SdkResult, DESCRIPTOR_NOT_FOUND, INVALID_ARGUMENT, INVALID_MESSAGE};

const EMBEDDED_FDS: &[u8] = include_bytes!("../descriptors/capsulad-framework.protoset");

pub static BASE_POOL: LazyLock<DescriptorPool> = LazyLock::new(|| {
    DescriptorPool::decode(EMBEDDED_FDS).expect("capsulad native: invalid embedded descriptor set")
});

pub fn base_message(name: &str) -> SdkResult<MessageDescriptor> {
    BASE_POOL.get_message_by_name(name).ok_or_else(|| {
        SdkError::new(
            DESCRIPTOR_NOT_FOUND,
            format!("embedded descriptor missing message {name}"),
        )
    })
}

pub fn message_by_name(pool: &DescriptorPool, name: &str) -> SdkResult<MessageDescriptor> {
    pool.get_message_by_name(name).ok_or_else(|| {
        SdkError::new(
            DESCRIPTOR_NOT_FOUND,
            format!("descriptor not registered for message {name}"),
        )
    })
}

pub fn resolve_method(
    pool: &DescriptorPool,
    service: &str,
    method: &str,
) -> SdkResult<MethodDescriptor> {
    let svc = pool
        .get_service_by_name(service)
        .ok_or_else(|| SdkError::new(DESCRIPTOR_NOT_FOUND, format!("unknown service {service}")))?;
    let found = svc.methods().find(|m| m.name() == method);
    found.ok_or_else(|| {
        SdkError::new(
            DESCRIPTOR_NOT_FOUND,
            format!("unknown method {service}/{method}"),
        )
    })
}

pub fn expected_kind(method: &MethodDescriptor) -> i32 {
    match (method.is_client_streaming(), method.is_server_streaming()) {
        (false, false) => 0,
        (false, true) => 1,
        (true, false) => 2,
        (true, true) => 3,
    }
}

pub fn json_to_dynamic(desc: &MessageDescriptor, json: &[u8]) -> SdkResult<DynamicMessage> {
    let mut de = serde_json::Deserializer::from_slice(json);
    let options = prost_reflect::DeserializeOptions::new().deny_unknown_fields(true);
    let msg = DynamicMessage::deserialize_with_options(desc.clone(), &mut de, &options)
        .map_err(|e| SdkError::new(INVALID_MESSAGE, format!("invalid JSON message: {e}")))?;
    de.end()
        .map_err(|e| SdkError::new(INVALID_MESSAGE, format!("trailing JSON data: {e}")))?;
    Ok(msg)
}

pub fn dynamic_to_json(msg: &DynamicMessage) -> SdkResult<Vec<u8>> {
    serde_json::to_vec(msg)
        .map_err(|e| SdkError::new(INVALID_MESSAGE, format!("cannot encode JSON: {e}")))
}

pub fn proto_to_json(desc: &MessageDescriptor, bytes: &[u8]) -> SdkResult<Vec<u8>> {
    let msg = DynamicMessage::decode(desc.clone(), bytes)
        .map_err(|e| SdkError::new(INVALID_MESSAGE, format!("invalid protobuf message: {e}")))?;
    dynamic_to_json(&msg)
}

pub fn json_to_proto(desc: &MessageDescriptor, json: &[u8]) -> SdkResult<Vec<u8>> {
    Ok(json_to_dynamic(desc, json)?.encode_to_vec())
}

/* ------------------------------------------------------------------ *
 * dynamic message helpers
 * ------------------------------------------------------------------ */

pub fn new_msg(name: &str) -> SdkResult<DynamicMessage> {
    Ok(DynamicMessage::new(base_message(name)?))
}

pub fn set_str(msg: &mut DynamicMessage, field: &str, value: impl Into<String>) {
    msg.set_field_by_name(field, Value::String(value.into()));
}

pub fn set_bytes(msg: &mut DynamicMessage, field: &str, value: Vec<u8>) {
    msg.set_field_by_name(field, Value::Bytes(Bytes::from(value)));
}

pub fn set_u64(msg: &mut DynamicMessage, field: &str, value: u64) {
    msg.set_field_by_name(field, Value::U64(value));
}

pub fn set_u32(msg: &mut DynamicMessage, field: &str, value: u32) {
    msg.set_field_by_name(field, Value::U32(value));
}

pub fn set_i32(msg: &mut DynamicMessage, field: &str, value: i32) {
    msg.set_field_by_name(field, Value::I32(value));
}

pub fn set_bool(msg: &mut DynamicMessage, field: &str, value: bool) {
    msg.set_field_by_name(field, Value::Bool(value));
}

pub fn set_enum(msg: &mut DynamicMessage, field: &str, value: i32) {
    msg.set_field_by_name(field, Value::EnumNumber(value));
}

pub fn set_message(msg: &mut DynamicMessage, field: &str, child: DynamicMessage) {
    msg.set_field_by_name(field, Value::Message(child));
}

pub fn get_str(msg: &DynamicMessage, field: &str) -> String {
    msg.get_field_by_name(field)
        .and_then(|v| v.as_str().map(ToOwned::to_owned))
        .unwrap_or_default()
}

pub fn get_u64(msg: &DynamicMessage, field: &str) -> u64 {
    msg.get_field_by_name(field)
        .and_then(|v| v.as_u64())
        .unwrap_or(0)
}

pub fn get_u32(msg: &DynamicMessage, field: &str) -> u32 {
    msg.get_field_by_name(field)
        .and_then(|v| v.as_u32())
        .unwrap_or(0)
}

pub fn get_i32(msg: &DynamicMessage, field: &str) -> i32 {
    msg.get_field_by_name(field)
        .and_then(|v| v.as_i32())
        .unwrap_or(0)
}

pub fn get_bool(msg: &DynamicMessage, field: &str) -> bool {
    msg.get_field_by_name(field)
        .and_then(|v| v.as_bool())
        .unwrap_or(false)
}

pub fn get_bytes(msg: &DynamicMessage, field: &str) -> Vec<u8> {
    msg.get_field_by_name(field)
        .and_then(|v| v.as_bytes().map(|b| b.to_vec()))
        .unwrap_or_default()
}

/// Returns a clone of the oneof/message field, only when it is actually set.
/// `get_field_by_name` returns the default value for unset fields, so presence
/// must be checked first or every oneof looks populated.
pub fn get_message(msg: &DynamicMessage, field: &str) -> Option<DynamicMessage> {
    if !msg.has_field_by_name(field) {
        return None;
    }
    msg.get_field_by_name(field)
        .and_then(|v| v.as_message().cloned())
}

/* ------------------------------------------------------------------ *
 * AppBus frames
 * ------------------------------------------------------------------ */

fn app_outbound() -> SdkResult<DynamicMessage> {
    new_msg("capsulad.framework.v1.AppOutbound")
}

pub fn app_hello(run_id: &str) -> SdkResult<Vec<u8>> {
    let mut hello = new_msg("capsulad.framework.v1.AppHello")?;
    set_str(&mut hello, "run_id", run_id);
    let mut out = app_outbound()?;
    set_message(&mut out, "hello", hello);
    Ok(out.encode_to_vec())
}

pub fn app_ready() -> SdkResult<Vec<u8>> {
    let mut out = app_outbound()?;
    set_message(
        &mut out,
        "ready",
        new_msg("capsulad.framework.v1.AppReady")?,
    );
    Ok(out.encode_to_vec())
}

pub fn app_call_reply_message(call_id: &str, payload: Vec<u8>) -> SdkResult<Vec<u8>> {
    let mut reply = new_msg("capsulad.framework.v1.AppCallReply")?;
    set_str(&mut reply, "call_id", call_id);
    set_bytes(&mut reply, "response_message", payload);
    let mut out = app_outbound()?;
    set_message(&mut out, "call_reply", reply);
    Ok(out.encode_to_vec())
}

pub fn app_call_reply_end(call_id: &str, code: i32, detail: &str) -> SdkResult<Vec<u8>> {
    let mut end = new_msg("capsulad.framework.v1.AppCallEnd")?;
    set_i32(&mut end, "grpc_status_code", code);
    set_str(&mut end, "detail", detail);
    let mut reply = new_msg("capsulad.framework.v1.AppCallReply")?;
    set_str(&mut reply, "call_id", call_id);
    set_message(&mut reply, "end", end);
    let mut out = app_outbound()?;
    set_message(&mut out, "call_reply", reply);
    Ok(out.encode_to_vec())
}

pub fn app_invocation_result(
    invocation_id: &str,
    accepted: bool,
    detail: &str,
) -> SdkResult<Vec<u8>> {
    let mut result = new_msg("capsulad.framework.v1.AppInvocationResult")?;
    set_str(&mut result, "invocation_id", invocation_id);
    set_bool(&mut result, "accepted", accepted);
    set_str(&mut result, "detail", detail);
    let mut out = app_outbound()?;
    set_message(&mut out, "invocation_result", result);
    Ok(out.encode_to_vec())
}

pub fn app_action_result(invocation_id: &str, succeeded: bool, detail: &str) -> SdkResult<Vec<u8>> {
    let mut result = new_msg("capsulad.framework.v1.AppActionResult")?;
    set_str(&mut result, "action_invocation_id", invocation_id);
    set_bool(&mut result, "succeeded", succeeded);
    set_str(&mut result, "detail", detail);
    let mut out = app_outbound()?;
    set_message(&mut out, "action_result", result);
    Ok(out.encode_to_vec())
}

/* ------------------------------------------------------------------ *
 * Routed call frames (AppBusService.Call payloads)
 * ------------------------------------------------------------------ */

fn routed_request() -> SdkResult<DynamicMessage> {
    new_msg("capsulad.framework.v1.RoutedCallRequest")
}

pub fn routed_start(
    target_app_id: &str,
    service: &str,
    rpc: &str,
    timeout_ms: u32,
) -> SdkResult<Vec<u8>> {
    let mut start = new_msg("capsulad.framework.v1.RoutedCallStart")?;
    set_str(&mut start, "target_app_id", target_app_id);
    set_str(&mut start, "grpc_service", service);
    set_str(&mut start, "rpc_name", rpc);
    set_u32(&mut start, "timeout_ms", timeout_ms);
    let mut req = routed_request()?;
    set_message(&mut req, "start", start);
    Ok(req.encode_to_vec())
}

pub fn routed_message(payload: Vec<u8>) -> SdkResult<Vec<u8>> {
    let mut req = routed_request()?;
    set_bytes(&mut req, "request_message", payload);
    Ok(req.encode_to_vec())
}

pub fn routed_half_close() -> SdkResult<Vec<u8>> {
    let mut req = routed_request()?;
    set_message(
        &mut req,
        "half_close",
        new_msg("capsulad.framework.v1.CallHalfClose")?,
    );
    Ok(req.encode_to_vec())
}

pub enum RoutedResponse {
    Message(Vec<u8>),
    End { code: i32, detail: String },
}

pub fn decode_routed_response(bytes: &[u8]) -> SdkResult<RoutedResponse> {
    let desc = base_message("capsulad.framework.v1.RoutedCallResponse")?;
    let msg = DynamicMessage::decode(desc, bytes)
        .map_err(|e| SdkError::new(INVALID_MESSAGE, format!("routed response decode: {e}")))?;
    if msg.has_field_by_name("response_message") {
        return Ok(RoutedResponse::Message(get_bytes(&msg, "response_message")));
    }
    if let Some(end) = get_message(&msg, "end") {
        return Ok(RoutedResponse::End {
            code: get_i32(&end, "grpc_status_code"),
            detail: get_str(&end, "detail"),
        });
    }
    Err(SdkError::new(
        INVALID_MESSAGE,
        "routed response has no message or end",
    ))
}

/* ------------------------------------------------------------------ *
 * Inbound AppBus frames
 * ------------------------------------------------------------------ */

pub enum InboundKind {
    Call {
        call_id: String,
        caller_run_id: String,
        service: String,
        rpc: String,
        deadline_monotonic_ns: u64,
    },
    CallData {
        call_id: String,
        payload: Vec<u8>,
    },
    CallHalfClose {
        call_id: String,
    },
    CallCancel {
        call_id: String,
    },
    Event {
        kind: i32,
        json: Vec<u8>,
    },
    Unknown(String),
}

pub fn decode_inbound(bytes: &[u8]) -> SdkResult<InboundKind> {
    let desc = base_message("capsulad.framework.v1.AppInbound")?;
    let msg = DynamicMessage::decode(desc, bytes)
        .map_err(|e| SdkError::new(INVALID_MESSAGE, format!("AppInbound decode: {e}")))?;

    if let Some(call) = get_message(&msg, "call") {
        return Ok(InboundKind::Call {
            call_id: get_str(&call, "call_id"),
            caller_run_id: get_str(&call, "caller_run_id"),
            service: get_str(&call, "grpc_service"),
            rpc: get_str(&call, "rpc_name"),
            deadline_monotonic_ns: get_u64(&call, "deadline_monotonic_ns"),
        });
    }
    if let Some(data) = get_message(&msg, "call_data") {
        return Ok(InboundKind::CallData {
            call_id: get_str(&data, "call_id"),
            payload: get_bytes(&data, "request_message"),
        });
    }
    if let Some(half) = get_message(&msg, "call_half_close") {
        return Ok(InboundKind::CallHalfClose {
            call_id: get_str(&half, "call_id"),
        });
    }
    if let Some(cancel) = get_message(&msg, "call_cancel") {
        return Ok(InboundKind::CallCancel {
            call_id: get_str(&cancel, "call_id"),
        });
    }

    let event = |kind: i32, msg: Option<DynamicMessage>| -> SdkResult<InboundKind> {
        let body = msg.ok_or_else(|| SdkError::new(INVALID_MESSAGE, "missing event payload"))?;
        Ok(InboundKind::Event {
            kind,
            json: dynamic_to_json(&body)?,
        })
    };

    if let Some(m) = get_message(&msg, "invocation") {
        return event(1, Some(m));
    }
    if let Some(m) = get_message(&msg, "settings_changed") {
        return event(2, Some(m));
    }
    if let Some(m) = get_message(&msg, "action") {
        return event(3, Some(m));
    }
    if let Some(m) = get_message(&msg, "activation") {
        return event(4, Some(m));
    }
    if let Some(m) = get_message(&msg, "touch_released") {
        return event(5, Some(m));
    }

    Ok(InboundKind::Unknown("unhandled AppInbound message".into()))
}

/* ------------------------------------------------------------------ *
 * descriptor registration
 * ------------------------------------------------------------------ */

pub fn register_descriptors(pool: &mut DescriptorPool, bytes: &[u8]) -> SdkResult<()> {
    let mut set = FileDescriptorSet::decode(bytes)
        .map_err(|e| SdkError::new(INVALID_ARGUMENT, format!("invalid FileDescriptorSet: {e}")))?;
    if set.file.is_empty() {
        return Err(SdkError::new(INVALID_ARGUMENT, "empty FileDescriptorSet"));
    }
    set.file.retain(|file| {
        let name = file.name.as_deref().unwrap_or("");
        name.is_empty() || pool.get_file_by_name(name).is_none()
    });
    if set.file.is_empty() {
        return Ok(());
    }
    pool.add_file_descriptor_set(set)
        .map_err(|e| SdkError::new(INVALID_ARGUMENT, format!("descriptor set rejected: {e}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn embedded_pool_covers_every_framework_service() {
        for service in [
            "capsulad.framework.v1.DeviceService",
            "capsulad.framework.v1.SettingsService",
            "capsulad.framework.v1.RoutingService",
            "capsulad.framework.v1.PresentationService",
            "capsulad.framework.v1.AudioService",
            "capsulad.framework.v1.InputService",
            "capsulad.framework.v1.AssistantService",
            "capsulad.framework.v1.ApplicationService",
            "capsulad.framework.v1.ApplicationConfigurationService",
            "capsulad.framework.v1.AppInterfaceService",
            "capsulad.framework.v1.AppBusService",
            "capsulad.framework.v1.SensorService",
            "capsulad.framework.v1.SystemService",
            "capsulad.framework.v1.DiagnosticsService",
            "capsulad.framework.v1.AssetService",
            "capsulad.framework.v1.BluetoothMediaService",
            "capsulad.contracts.media.v1.MediaPlayerService",
        ] {
            assert!(
                BASE_POOL.get_service_by_name(service).is_some(),
                "missing {service}"
            );
        }
    }

    #[test]
    fn unset_oneof_is_not_treated_as_present() {
        let mut out = new_msg("capsulad.framework.v1.AppInbound").unwrap();
        let mut invocation = new_msg("capsulad.framework.v1.AppLaunchInvocation").unwrap();
        set_str(&mut invocation, "invocation_id", "inv_test");
        set_message(&mut out, "invocation", invocation);
        let bytes = out.encode_to_vec();
        match decode_inbound(&bytes).unwrap() {
            InboundKind::Event { kind, .. } => assert_eq!(kind, 1),
            _ => panic!("invocation was not decoded as an event"),
        }
    }

    #[test]
    fn json_roundtrip_and_unknown_field_rejection() {
        let method =
            resolve_method(&BASE_POOL, "capsulad.framework.v1.DeviceService", "GetInfo").unwrap();
        let msg = json_to_dynamic(&method.input(), b"{}").unwrap();
        assert!(msg.get_field_by_name("nope").is_none());
        assert!(json_to_dynamic(&method.input(), br#"{"unknown":1}"#).is_err());
        let json = dynamic_to_json(&msg).unwrap();
        assert_eq!(json, b"{}");
    }
}
