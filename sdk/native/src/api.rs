//! Exported C ABI v1: bounded function table and entry points.

use std::ffi::{c_char, c_void};
use std::mem::size_of;
use std::ptr;
use std::sync::Arc;

use crate::audio;
use crate::bus::{self, Callbacks};
use crate::call::{self, CallInner, CALLS};
use crate::client::{ClientInner, CLIENTS};
use crate::ffi::{
    self, alloc_buf, alloc_json, alloc_string, guard, input_bytes, input_str, set_error, write_out,
    Buf, PcmEvent, PcmFormat, SdkError, SdkResult, BUSY, INVALID_ARGUMENT, INVALID_MESSAGE,
    NOT_FOUND, OK,
};

pub const ABI_MAJOR: u32 = 1;
pub const ABI_MINOR: u32 = 0;
pub const VERSION: &str = "1.0.0";

#[repr(C)]
pub struct CapsuladApi {
    pub abi_major: u32,
    pub abi_minor: u32,
    pub struct_size: usize,

    pub client_connect: unsafe extern "C" fn(
        *const c_char,
        usize,
        *const c_char,
        usize,
        *const c_char,
        usize,
        u32,
        *mut u64,
    ) -> i32,
    pub client_close: unsafe extern "C" fn(u64) -> i32,
    pub client_register_descriptors: unsafe extern "C" fn(u64, *const u8, usize) -> i32,
    pub client_info: unsafe extern "C" fn(u64, *mut Buf) -> i32,
    pub client_unary: unsafe extern "C" fn(
        u64,
        *const c_char,
        usize,
        *const c_char,
        usize,
        *const u8,
        usize,
        u32,
        *mut Buf,
    ) -> i32,
    pub client_unary_proto: unsafe extern "C" fn(
        u64,
        *const c_char,
        usize,
        *const c_char,
        usize,
        *const u8,
        usize,
        u32,
        *mut Buf,
    ) -> i32,

    pub call_start: unsafe extern "C" fn(
        u64,
        *const c_char,
        usize,
        *const c_char,
        usize,
        i32,
        u32,
        *mut u64,
    ) -> i32,
    pub call_start_routed: unsafe extern "C" fn(
        u64,
        *const c_char,
        usize,
        *const c_char,
        usize,
        *const c_char,
        usize,
        u32,
        *mut u64,
    ) -> i32,
    pub call_send_json: unsafe extern "C" fn(u64, *const u8, usize) -> i32,
    pub call_send_proto: unsafe extern "C" fn(u64, *const u8, usize) -> i32,
    pub call_half_close: unsafe extern "C" fn(u64) -> i32,
    pub call_recv_json: unsafe extern "C" fn(u64, u32, *mut Buf) -> i32,
    pub call_recv_proto: unsafe extern "C" fn(u64, u32, *mut Buf) -> i32,
    pub call_end: unsafe extern "C" fn(u64, i32, *const c_char, usize) -> i32,
    pub call_cancel: unsafe extern "C" fn(u64) -> i32,
    pub call_retain: unsafe extern "C" fn(u64) -> i32,
    pub call_release: unsafe extern "C" fn(u64) -> i32,
    pub call_info: unsafe extern "C" fn(u64, *mut Buf) -> i32,
    pub call_status: unsafe extern "C" fn(u64, *mut i32, *mut Buf) -> i32,

    pub bus_open: unsafe extern "C" fn(u64, *const BusCallbacksV1, *mut u64) -> i32,
    pub bus_close: unsafe extern "C" fn(u64) -> i32,
    pub bus_ready: unsafe extern "C" fn(u64, u32) -> i32,
    pub bus_notify_invocation:
        unsafe extern "C" fn(u64, *const c_char, usize, i32, *const c_char, usize) -> i32,
    pub bus_notify_action:
        unsafe extern "C" fn(u64, *const c_char, usize, i32, *const c_char, usize) -> i32,

    pub audio_capture_open: unsafe extern "C" fn(
        u64,
        *const c_char,
        usize,
        *const PcmFormat,
        u32,
        *const c_char,
        usize,
        *const c_char,
        usize,
        *mut u64,
    ) -> i32,
    pub audio_capture_recv: unsafe extern "C" fn(u64, u32, *mut PcmEvent) -> i32,
    pub audio_playback_open: unsafe extern "C" fn(
        u64,
        *const c_char,
        usize,
        *const PcmFormat,
        u32,
        *const c_char,
        usize,
        *mut u64,
    ) -> i32,
    pub audio_playback_write:
        unsafe extern "C" fn(u64, *const u8, usize, u32, u64, i32, u64) -> i32,
    pub audio_playback_control: unsafe extern "C" fn(u64, i32) -> i32,

    pub buf_free: unsafe extern "C" fn(Buf) -> i32,
    pub last_error: unsafe extern "C" fn() -> *const c_char,
    pub handle_info: unsafe extern "C" fn(u64, *mut Buf) -> i32,

    // --- appended in ABI v1 (never reordered) -------------------------------
    pub http_open: unsafe extern "C" fn(*const u8, usize, u32, *mut u64) -> i32,
    pub http_close: unsafe extern "C" fn(u64) -> i32,
    pub http_request: unsafe extern "C" fn(
        u64,
        u64,
        *const c_char,
        usize,
        *const c_char,
        usize,
        *const u8,
        usize,
        *const u8,
        usize,
        u32,
        *mut i32,
        *mut Buf,
        *mut Buf,
    ) -> i32,
    pub grpc_open:
        unsafe extern "C" fn(*const c_char, usize, *const u8, usize, u32, *mut u64) -> i32,
    pub call_start_meta: unsafe extern "C" fn(
        u64,
        *const c_char,
        usize,
        *const c_char,
        usize,
        i32,
        u32,
        *const u8,
        usize,
        *mut u64,
    ) -> i32,
    pub base64_encode: unsafe extern "C" fn(*const u8, usize, i32, *mut Buf) -> i32,
    pub base64_decode: unsafe extern "C" fn(*const u8, usize, i32, *mut Buf) -> i32,
    pub percent_encode: unsafe extern "C" fn(*const u8, usize, *mut Buf) -> i32,
    pub percent_decode: unsafe extern "C" fn(*const u8, usize, *mut Buf) -> i32,
    pub sha256: unsafe extern "C" fn(*const u8, usize, *mut Buf) -> i32,
    pub random_bytes: unsafe extern "C" fn(*mut u8, usize) -> i32,
    pub uuid_v4: unsafe extern "C" fn(*mut Buf) -> i32,
    pub url_parse: unsafe extern "C" fn(*const c_char, usize, *mut Buf) -> i32,
    pub http_cancel: unsafe extern "C" fn(u64, u64) -> i32,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct BusCallbacksV1 {
    pub struct_size: usize,
    pub user_data: *mut c_void,
    pub on_ready: Option<bus::ReadyFn>,
    pub on_disconnected: Option<bus::DisconnectedFn>,
    pub on_event: Option<bus::EventFn>,
    pub on_call: Option<bus::CallFn>,
    pub on_log: Option<bus::LogFn>,
}

fn outcome(result: SdkResult<()>) -> i32 {
    match result {
        Ok(()) => OK,
        Err(err) => {
            set_error(&err);
            err.status
        }
    }
}

fn run(f: impl FnOnce() -> SdkResult<()>) -> i32 {
    guard(|| outcome(f()))
}

fn client(handle: u64) -> SdkResult<Arc<ClientInner>> {
    CLIENTS
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown client handle"))
}

fn call_inner(handle: u64) -> SdkResult<Arc<CallInner>> {
    CALLS
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown call handle"))
}

fn client_info_json(client: &ClientInner) -> Vec<u8> {
    let value = serde_json::json!({
        "socket": client.socket,
        "run_id": client.run_id,
        "app_id": client.app_id,
        "remote_url": client.remote_url.clone().unwrap_or_default(),
        "abi_major": ABI_MAJOR,
        "abi_minor": ABI_MINOR,
    });
    serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
}

/* ------------------------------------------------------------------ *
 * Client
 * ------------------------------------------------------------------ */

unsafe extern "C" fn c_client_connect(
    socket: *const c_char,
    socket_len: usize,
    run_id: *const c_char,
    run_id_len: usize,
    app_id: *const c_char,
    app_id_len: usize,
    timeout_ms: u32,
    out_client: *mut u64,
) -> i32 {
    run(|| {
        let socket = input_str(socket, socket_len)?;
        let run_id = input_str(run_id, run_id_len)?;
        let app_id = input_str(app_id, app_id_len)?;
        if out_client.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_client"));
        }
        let timeout = if timeout_ms == 0 {
            crate::runtime::DEFAULT_CONNECT_TIMEOUT
        } else {
            std::time::Duration::from_millis(u64::from(timeout_ms))
        };
        let client = crate::client::connect(socket, run_id, app_id, timeout)?;
        let handle = CLIENTS.insert(client);
        write_out(out_client, handle)?;
        Ok(())
    })
}

unsafe extern "C" fn c_client_close(handle: u64) -> i32 {
    run(|| {
        if crate::threads::is_callback_thread() {
            return Err(SdkError::new(
                BUSY,
                "client_close cannot be called from an SDK callback thread",
            ));
        }
        let Some(client) = CLIENTS.get(handle) else {
            return Err(SdkError::new(NOT_FOUND, "unknown client handle"));
        };
        client
            .closed
            .store(true, std::sync::atomic::Ordering::Release);
        let (calls, buses) = bus::cancel_all_for_client(&client);
        for bus_handle in buses {
            let _ = bus::close_handle(bus_handle);
        }
        for call_handle in calls {
            let _ = call::cancel_handle(call_handle);
            if let Some(inner) = CALLS.get(call_handle) {
                let _ = inner.release();
            }
        }
        CLIENTS.remove(handle);
        Ok(())
    })
}

unsafe extern "C" fn c_client_register_descriptors(
    handle: u64,
    bytes: *const u8,
    len: usize,
) -> i32 {
    run(|| {
        let c = client(handle)?;
        let bytes = input_bytes(bytes, len)?;
        c.register_descriptors(bytes)
    })
}

unsafe extern "C" fn c_client_info(handle: u64, out: *mut Buf) -> i32 {
    run(|| {
        let c = client(handle)?;
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        write_out(out, alloc_json(client_info_json(&c)))?;
        Ok(())
    })
}

fn unary_common(
    handle: u64,
    service: &str,
    method: &str,
    request: Vec<u8>,
    json: bool,
    timeout_ms: u32,
) -> SdkResult<Vec<u8>> {
    let c = client(handle)?;
    c.ensure_open()?;
    let call = call::start_direct(
        c,
        service,
        method,
        0,
        call::deadline_to_duration(timeout_ms),
    )?;
    let result = (|| -> SdkResult<Vec<u8>> {
        if json {
            call.send_json(&request)?;
        } else {
            call.send_payload(request)?;
        }
        call.half_close()?;
        let first = call.recv(u32::MAX, json)?;
        match call.recv(u32::MAX, json) {
            Err(err) if err.status == ffi::END_OF_STREAM => Ok(first),
            Err(err) => Err(err),
            Ok(_) => Err(SdkError::new(
                INVALID_MESSAGE,
                "unary call returned more than one response message",
            )),
        }
    })();
    call.release_ref();
    result
}

unsafe extern "C" fn c_client_unary(
    handle: u64,
    service: *const c_char,
    service_len: usize,
    method: *const c_char,
    method_len: usize,
    request: *const u8,
    request_len: usize,
    timeout_ms: u32,
    out: *mut Buf,
) -> i32 {
    run(|| {
        let service = input_str(service, service_len)?;
        let method = input_str(method, method_len)?;
        let request = input_bytes(request, request_len)?.to_vec();
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let bytes = unary_common(handle, service, method, request, true, timeout_ms)?;
        write_out(out, alloc_json(bytes))?;
        Ok(())
    })
}

unsafe extern "C" fn c_client_unary_proto(
    handle: u64,
    service: *const c_char,
    service_len: usize,
    method: *const c_char,
    method_len: usize,
    request: *const u8,
    request_len: usize,
    timeout_ms: u32,
    out: *mut Buf,
) -> i32 {
    run(|| {
        let service = input_str(service, service_len)?;
        let method = input_str(method, method_len)?;
        let request = input_bytes(request, request_len)?.to_vec();
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let bytes = unary_common(handle, service, method, request, false, timeout_ms)?;
        write_out(out, alloc_buf(bytes))?;
        Ok(())
    })
}

/* ------------------------------------------------------------------ *
 * Calls
 * ------------------------------------------------------------------ */

unsafe extern "C" fn c_call_start(
    handle: u64,
    service: *const c_char,
    service_len: usize,
    method: *const c_char,
    method_len: usize,
    kind: i32,
    timeout_ms: u32,
    out_call: *mut u64,
) -> i32 {
    run(|| {
        let c = client(handle)?;
        let service = input_str(service, service_len)?;
        let method = input_str(method, method_len)?;
        if out_call.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_call"));
        }
        let call = call::start_direct(
            c,
            service,
            method,
            kind,
            call::deadline_to_duration(timeout_ms),
        )?;
        write_out(
            out_call,
            call.handle.load(std::sync::atomic::Ordering::Acquire),
        )?;
        Ok(())
    })
}

unsafe extern "C" fn c_call_start_routed(
    handle: u64,
    target_app_id: *const c_char,
    target_app_id_len: usize,
    service: *const c_char,
    service_len: usize,
    method: *const c_char,
    method_len: usize,
    timeout_ms: u32,
    out_call: *mut u64,
) -> i32 {
    run(|| {
        let c = client(handle)?;
        let target = input_str(target_app_id, target_app_id_len)?;
        let service = input_str(service, service_len)?;
        let method = input_str(method, method_len)?;
        if out_call.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_call"));
        }
        let call = call::start_routed(
            c,
            target,
            service,
            method,
            call::deadline_to_duration(timeout_ms),
        )?;
        write_out(
            out_call,
            call.handle.load(std::sync::atomic::Ordering::Acquire),
        )?;
        Ok(())
    })
}

unsafe extern "C" fn c_call_send_json(call: u64, json: *const u8, len: usize) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        let bytes = input_bytes(json, len)?;
        match inner.as_ref() {
            CallInner::Out(call) => call.send_json(bytes),
            CallInner::In(call) => call.send_json(bytes),
        }
    })
}

unsafe extern "C" fn c_call_send_proto(call: u64, proto: *const u8, len: usize) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        let bytes = input_bytes(proto, len)?.to_vec();
        match inner.as_ref() {
            CallInner::Out(call) => call.send_payload(bytes),
            CallInner::In(call) => call.send_payload(bytes),
        }
    })
}

unsafe extern "C" fn c_call_half_close(call: u64) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        match inner.as_ref() {
            CallInner::Out(call) => call.half_close(),
            CallInner::In(_) => Err(SdkError::new(
                INVALID_ARGUMENT,
                "half_close is only valid on client-initiated calls",
            )),
        }
    })
}

fn recv_common(call: u64, timeout_ms: u32, json: bool) -> SdkResult<Vec<u8>> {
    let inner = call_inner(call)?;
    match inner.as_ref() {
        CallInner::Out(call) => call.recv(timeout_ms, json),
        CallInner::In(call) => call.recv(timeout_ms, json),
    }
}

unsafe extern "C" fn c_call_recv_json(call: u64, timeout_ms: u32, out: *mut Buf) -> i32 {
    run(|| {
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let bytes = recv_common(call, timeout_ms, true)?;
        write_out(out, alloc_json(bytes))?;
        Ok(())
    })
}

unsafe extern "C" fn c_call_recv_proto(call: u64, timeout_ms: u32, out: *mut Buf) -> i32 {
    run(|| {
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let bytes = recv_common(call, timeout_ms, false)?;
        write_out(out, alloc_buf(bytes))?;
        Ok(())
    })
}

unsafe extern "C" fn c_call_end(
    call: u64,
    grpc_code: i32,
    detail: *const c_char,
    detail_len: usize,
) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        let detail = input_str(detail, detail_len)?;
        match inner.as_ref() {
            CallInner::Out(call) => call.end(grpc_code, detail),
            CallInner::In(call) => call.end(grpc_code, detail),
        }
    })
}

unsafe extern "C" fn c_call_cancel(call: u64) -> i32 {
    run(|| call_inner(call)?.cancel())
}

unsafe extern "C" fn c_call_retain(call: u64) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        match inner.as_ref() {
            CallInner::Out(call) => {
                call.retain_ref();
            }
            CallInner::In(call) => call.retain_ref(),
        }
        Ok(())
    })
}

unsafe extern "C" fn c_call_release(call: u64) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        inner.release()
    })
}

unsafe extern "C" fn c_call_info(call: u64, out: *mut Buf) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        write_out(out, alloc_json(inner.info_json()))?;
        Ok(())
    })
}

unsafe extern "C" fn c_call_status(call: u64, out_code: *mut i32, out_detail: *mut Buf) -> i32 {
    run(|| {
        let inner = call_inner(call)?;
        let status = inner.status();
        if !out_code.is_null() {
            ptr::write(
                out_code,
                status.as_ref().map(|status| status.code).unwrap_or(-1),
            );
        }
        if !out_detail.is_null() {
            let detail = status
                .as_ref()
                .map(|status| status.detail.clone())
                .unwrap_or_default();
            write_out(out_detail, alloc_json(detail.into_bytes()))?;
        }
        Ok(())
    })
}

/* ------------------------------------------------------------------ *
 * AppBus
 * ------------------------------------------------------------------ */

unsafe fn parse_callbacks(ptr_: *const BusCallbacksV1) -> SdkResult<Callbacks> {
    if ptr_.is_null() {
        return Err(SdkError::new(INVALID_ARGUMENT, "null bus callbacks"));
    }
    let declared = ptr::addr_of!((*ptr_).struct_size).read();
    let prefix = std::mem::offset_of!(BusCallbacksV1, user_data) + size_of::<*mut c_void>();
    if declared < prefix {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "bus callbacks struct_size is smaller than the required prefix",
        ));
    }
    let mut local: BusCallbacksV1 = std::mem::zeroed();
    let copy_len = declared.min(size_of::<BusCallbacksV1>());
    ptr::copy_nonoverlapping(
        ptr_ as *const u8,
        &mut local as *mut BusCallbacksV1 as *mut u8,
        copy_len,
    );
    Ok(Callbacks {
        user_data: local.user_data as usize,
        on_ready: local.on_ready,
        on_disconnected: local.on_disconnected,
        on_event: local.on_event,
        on_call: local.on_call,
        on_log: local.on_log,
    })
}

unsafe extern "C" fn c_bus_open(
    client_handle: u64,
    callbacks: *const BusCallbacksV1,
    out_bus: *mut u64,
) -> i32 {
    run(|| {
        if out_bus.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_bus"));
        }
        let callbacks = parse_callbacks(callbacks)?;
        let handle = bus::open_for_handle(client_handle, callbacks)?;
        write_out(out_bus, handle)?;
        Ok(())
    })
}

unsafe extern "C" fn c_bus_close(bus: u64) -> i32 {
    run(|| bus::close_handle(bus))
}

unsafe extern "C" fn c_bus_ready(bus: u64, timeout_ms: u32) -> i32 {
    run(|| bus::ready_handle(bus, timeout_ms))
}

unsafe extern "C" fn c_bus_notify_invocation(
    bus: u64,
    invocation_id: *const c_char,
    invocation_id_len: usize,
    accepted: i32,
    detail: *const c_char,
    detail_len: usize,
) -> i32 {
    run(|| {
        let invocation_id = input_str(invocation_id, invocation_id_len)?;
        let detail = input_str(detail, detail_len)?;
        bus::notify_invocation(bus, invocation_id, accepted != 0, detail)
    })
}

unsafe extern "C" fn c_bus_notify_action(
    bus: u64,
    invocation_id: *const c_char,
    invocation_id_len: usize,
    succeeded: i32,
    detail: *const c_char,
    detail_len: usize,
) -> i32 {
    run(|| {
        let invocation_id = input_str(invocation_id, invocation_id_len)?;
        let detail = input_str(detail, detail_len)?;
        bus::notify_action(bus, invocation_id, succeeded != 0, detail)
    })
}

/* ------------------------------------------------------------------ *
 * Audio
 * ------------------------------------------------------------------ */

unsafe fn read_format(format: *const PcmFormat) -> SdkResult<PcmFormat> {
    if format.is_null() {
        return Err(SdkError::new(INVALID_ARGUMENT, "null audio format"));
    }
    Ok(ptr::read(format))
}

unsafe extern "C" fn c_audio_capture_open(
    client_handle: u64,
    endpoint_id: *const c_char,
    endpoint_id_len: usize,
    format: *const PcmFormat,
    packet_ms: u32,
    activation_id: *const c_char,
    activation_id_len: usize,
    conversation_id: *const c_char,
    conversation_id_len: usize,
    out_call: *mut u64,
) -> i32 {
    run(|| {
        let c = client(client_handle)?;
        let endpoint_id = input_str(endpoint_id, endpoint_id_len)?;
        let activation_id = input_str(activation_id, activation_id_len)?;
        let conversation_id = input_str(conversation_id, conversation_id_len)?;
        let format = read_format(format)?;
        if out_call.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_call"));
        }
        let call = audio::capture_open(
            c,
            endpoint_id,
            &format,
            packet_ms,
            activation_id,
            conversation_id,
        )?;
        write_out(
            out_call,
            call.handle.load(std::sync::atomic::Ordering::Acquire),
        )?;
        Ok(())
    })
}

unsafe extern "C" fn c_audio_capture_recv(call: u64, timeout_ms: u32, out: *mut PcmEvent) -> i32 {
    run(|| {
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output event"));
        }
        let event = audio::capture_recv(call, timeout_ms)?;
        write_out(out, event)?;
        Ok(())
    })
}

unsafe extern "C" fn c_audio_playback_open(
    client_handle: u64,
    endpoint_id: *const c_char,
    endpoint_id_len: usize,
    format: *const PcmFormat,
    target_latency_ms: u32,
    conversation_id: *const c_char,
    conversation_id_len: usize,
    out_call: *mut u64,
) -> i32 {
    run(|| {
        let c = client(client_handle)?;
        let endpoint_id = input_str(endpoint_id, endpoint_id_len)?;
        let conversation_id = input_str(conversation_id, conversation_id_len)?;
        let format = read_format(format)?;
        if out_call.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_call"));
        }
        let call =
            audio::playback_open(c, endpoint_id, &format, target_latency_ms, conversation_id)?;
        write_out(
            out_call,
            call.handle.load(std::sync::atomic::Ordering::Acquire),
        )?;
        Ok(())
    })
}

unsafe extern "C" fn c_audio_playback_write(
    call: u64,
    pcm: *const u8,
    pcm_len: usize,
    frame_count: u32,
    monotonic_time_ns: u64,
    discontinuity: i32,
    dropped_frames_before: u64,
) -> i32 {
    run(|| {
        let bytes = input_bytes(pcm, pcm_len)?;
        audio::playback_write(
            call,
            bytes,
            frame_count,
            monotonic_time_ns,
            discontinuity != 0,
            dropped_frames_before,
        )
    })
}

unsafe extern "C" fn c_audio_playback_control(call: u64, command: i32) -> i32 {
    run(|| audio::playback_control(call, command))
}

/* ------------------------------------------------------------------ *
 * HTTP client
 * ------------------------------------------------------------------ */

unsafe extern "C" fn c_http_open(
    options_json: *const u8,
    options_len: usize,
    timeout_ms: u32,
    out_http: *mut u64,
) -> i32 {
    run(|| {
        let options = input_bytes(options_json, options_len)?;
        if out_http.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_http"));
        }
        // Opening reads CA files only; no network is touched, so timeout_ms is
        // accepted for symmetry with the other open calls and ignored.
        let _ = timeout_ms;
        let handle = crate::http::open(options)?;
        write_out(out_http, handle)
    })
}

unsafe extern "C" fn c_http_close(handle: u64) -> i32 {
    run(|| crate::http::close(handle))
}

unsafe extern "C" fn c_http_cancel(handle: u64, request_id: u64) -> i32 {
    run(|| crate::http::cancel(handle, request_id))
}

#[allow(clippy::too_many_arguments)]
unsafe extern "C" fn c_http_request(
    handle: u64,
    request_id: u64,
    method: *const c_char,
    method_len: usize,
    url: *const c_char,
    url_len: usize,
    headers_json: *const u8,
    headers_len: usize,
    body: *const u8,
    body_len: usize,
    timeout_ms: u32,
    out_status_code: *mut i32,
    out_headers_json: *mut Buf,
    out_body: *mut Buf,
) -> i32 {
    run(|| {
        let client = crate::http::HTTP_CLIENTS
            .get(handle)
            .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown http handle"))?;
        if out_status_code.is_null() || out_headers_json.is_null() || out_body.is_null() {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "http request needs non-null status/header/body outputs",
            ));
        }
        let method = input_str(method, method_len)?.to_string();
        let url = input_str(url, url_len)?.to_string();
        let headers =
            crate::headers::parse_string_map(input_bytes(headers_json, headers_len)?, "headers")?;
        let body = input_bytes(body, body_len)?.to_vec();
        let timeout = call::deadline_to_duration(timeout_ms);
        let response = crate::runtime::runtime().block_on(crate::http::request(
            client,
            request_id,
            crate::http::HttpRequest {
                method,
                url,
                headers,
                body,
            },
            timeout,
        ))?;
        write_out(out_status_code, response.status)?;
        write_out(out_headers_json, alloc_json(response.headers_json))?;
        write_out(out_body, alloc_buf(response.body))?;
        Ok(())
    })
}

/* ------------------------------------------------------------------ *
 * Remote gRPC
 * ------------------------------------------------------------------ */

unsafe extern "C" fn c_grpc_open(
    url: *const c_char,
    url_len: usize,
    options_json: *const u8,
    options_len: usize,
    timeout_ms: u32,
    out_client: *mut u64,
) -> i32 {
    run(|| {
        let url = input_str(url, url_len)?;
        let options_bytes = input_bytes(options_json, options_len)?;
        if out_client.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_client"));
        }
        let mut options = crate::grpc_remote::GrpcOptions::parse(options_bytes)?;
        if timeout_ms != 0 {
            options.connect_timeout = std::time::Duration::from_millis(u64::from(timeout_ms));
        }
        let origin = crate::primitives::parse_origin(url)?;
        let channel =
            crate::runtime::runtime().block_on(crate::grpc_remote::connect(url, &options))?;
        let client = ClientInner::new_remote(origin.origin(), channel);
        let handle = CLIENTS.insert(client);
        write_out(out_client, handle)
    })
}

#[allow(clippy::too_many_arguments)]
unsafe extern "C" fn c_call_start_meta(
    handle: u64,
    service: *const c_char,
    service_len: usize,
    method: *const c_char,
    method_len: usize,
    kind: i32,
    timeout_ms: u32,
    headers_json: *const u8,
    headers_len: usize,
    out_call: *mut u64,
) -> i32 {
    run(|| {
        let c = client(handle)?;
        let service = input_str(service, service_len)?;
        let method = input_str(method, method_len)?;
        if out_call.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null out_call"));
        }
        let headers = crate::headers::validate_grpc(crate::headers::parse_string_map(
            input_bytes(headers_json, headers_len)?,
            "metadata",
        )?)?;
        let call = call::start_meta(
            c,
            service,
            method,
            kind,
            call::deadline_to_duration(timeout_ms),
            headers,
        )?;
        write_out(
            out_call,
            call.handle.load(std::sync::atomic::Ordering::Acquire),
        )?;
        Ok(())
    })
}

/* ------------------------------------------------------------------ *
 * Primitives
 * ------------------------------------------------------------------ */

unsafe extern "C" fn c_base64_encode(
    data: *const u8,
    len: usize,
    flags: i32,
    out: *mut Buf,
) -> i32 {
    run(|| {
        let bytes = input_bytes(data, len)?;
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let encoded = crate::primitives::base64_encode(bytes, flags)?;
        write_out(out, alloc_json(encoded))
    })
}

unsafe extern "C" fn c_base64_decode(
    data: *const u8,
    len: usize,
    flags: i32,
    out: *mut Buf,
) -> i32 {
    run(|| {
        let bytes = input_bytes(data, len)?;
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let decoded = crate::primitives::base64_decode(bytes, flags)?;
        write_out(out, alloc_buf(decoded))
    })
}

unsafe extern "C" fn c_percent_encode(data: *const u8, len: usize, out: *mut Buf) -> i32 {
    run(|| {
        let bytes = input_bytes(data, len)?;
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        write_out(out, alloc_json(crate::primitives::percent_encode(bytes)))
    })
}

unsafe extern "C" fn c_percent_decode(data: *const u8, len: usize, out: *mut Buf) -> i32 {
    run(|| {
        let bytes = input_bytes(data, len)?;
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let decoded = crate::primitives::percent_decode(bytes)?;
        write_out(out, alloc_buf(decoded))
    })
}

unsafe extern "C" fn c_sha256(data: *const u8, len: usize, out: *mut Buf) -> i32 {
    run(|| {
        let bytes = input_bytes(data, len)?;
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        write_out(out, alloc_buf(crate::primitives::sha256(bytes)))
    })
}

unsafe extern "C" fn c_random_bytes(out: *mut u8, len: usize) -> i32 {
    run(|| {
        if len == 0 {
            return Ok(());
        }
        if out.is_null() {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "null random output pointer with non-zero length",
            ));
        }
        let slice = std::slice::from_raw_parts_mut(out, len);
        crate::primitives::random_bytes(slice)
    })
}

unsafe extern "C" fn c_uuid_v4(out: *mut Buf) -> i32 {
    run(|| {
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let uuid = crate::primitives::uuid_v4()?;
        write_out(out, alloc_string(uuid))
    })
}

unsafe extern "C" fn c_url_parse(url: *const c_char, len: usize, out_json: *mut Buf) -> i32 {
    run(|| {
        let url = input_str(url, len)?;
        if out_json.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        let parsed = crate::primitives::parse_url(url)?;
        let bytes = serde_json::to_vec(&parsed.to_json())
            .map_err(|err| SdkError::new(ffi::INTERNAL, err.to_string()))?;
        write_out(out_json, alloc_json(bytes))
    })
}

/* ------------------------------------------------------------------ *
 * Misc
 * ------------------------------------------------------------------ */

unsafe extern "C" fn c_buf_free(buf: Buf) -> i32 {
    run(|| unsafe { ffi::free_buf(buf) })
}

unsafe extern "C" fn c_last_error() -> *const c_char {
    ffi::last_error_ptr()
}

unsafe extern "C" fn c_handle_info(handle: u64, out: *mut Buf) -> i32 {
    run(|| {
        if out.is_null() {
            return Err(SdkError::new(INVALID_ARGUMENT, "null output buffer"));
        }
        if let Some(client) = CLIENTS.get(handle) {
            write_out(out, alloc_json(client_info_json(&client)))?;
            return Ok(());
        }
        if let Some(call) = CALLS.get(handle) {
            write_out(out, alloc_json(call.info_json()))?;
            return Ok(());
        }
        if let Some(bus) = bus::bus_info(handle) {
            write_out(out, alloc_json(bus))?;
            return Ok(());
        }
        if let Some(http) = crate::http::HTTP_CLIENTS.get(handle) {
            write_out(out, alloc_json(http.info_json()))?;
            return Ok(());
        }
        Err(SdkError::new(NOT_FOUND, "unknown handle"))
    })
}

/* ------------------------------------------------------------------ *
 * Static table and exported symbols
 * ------------------------------------------------------------------ */

pub static API: CapsuladApi = CapsuladApi {
    abi_major: ABI_MAJOR,
    abi_minor: ABI_MINOR,
    struct_size: size_of::<CapsuladApi>(),
    client_connect: c_client_connect,
    client_close: c_client_close,
    client_register_descriptors: c_client_register_descriptors,
    client_info: c_client_info,
    client_unary: c_client_unary,
    client_unary_proto: c_client_unary_proto,
    call_start: c_call_start,
    call_start_routed: c_call_start_routed,
    call_send_json: c_call_send_json,
    call_send_proto: c_call_send_proto,
    call_half_close: c_call_half_close,
    call_recv_json: c_call_recv_json,
    call_recv_proto: c_call_recv_proto,
    call_end: c_call_end,
    call_cancel: c_call_cancel,
    call_retain: c_call_retain,
    call_release: c_call_release,
    call_info: c_call_info,
    call_status: c_call_status,
    bus_open: c_bus_open,
    bus_close: c_bus_close,
    bus_ready: c_bus_ready,
    bus_notify_invocation: c_bus_notify_invocation,
    bus_notify_action: c_bus_notify_action,
    audio_capture_open: c_audio_capture_open,
    audio_capture_recv: c_audio_capture_recv,
    audio_playback_open: c_audio_playback_open,
    audio_playback_write: c_audio_playback_write,
    audio_playback_control: c_audio_playback_control,
    buf_free: c_buf_free,
    last_error: c_last_error,
    handle_info: c_handle_info,
    http_open: c_http_open,
    http_close: c_http_close,
    http_request: c_http_request,
    grpc_open: c_grpc_open,
    call_start_meta: c_call_start_meta,
    base64_encode: c_base64_encode,
    base64_decode: c_base64_decode,
    percent_encode: c_percent_encode,
    percent_decode: c_percent_decode,
    sha256: c_sha256,
    random_bytes: c_random_bytes,
    uuid_v4: c_uuid_v4,
    url_parse: c_url_parse,
    http_cancel: c_http_cancel,
};

#[no_mangle]
pub extern "C" fn capsulad_api_size() -> usize {
    size_of::<CapsuladApi>()
}

#[no_mangle]
pub extern "C" fn capsulad_abi_major() -> u32 {
    ABI_MAJOR
}

#[no_mangle]
pub extern "C" fn capsulad_abi_minor() -> u32 {
    ABI_MINOR
}

#[no_mangle]
pub extern "C" fn capsulad_version() -> *const c_char {
    c"1.0.0".as_ptr()
}

#[no_mangle]
pub extern "C" fn capsulad_api_v1() -> *const CapsuladApi {
    &API
}

const API_MIN_SIZE: usize = std::mem::offset_of!(CapsuladApi, client_connect);

#[no_mangle]
pub extern "C" fn capsulad_get_api(
    abi_major: u32,
    table_size: usize,
    out: *mut CapsuladApi,
) -> i32 {
    if abi_major != ABI_MAJOR {
        set_error(&SdkError::new(
            ffi::UNSUPPORTED_ABI,
            format!("unsupported ABI major {abi_major}; this library is v{ABI_MAJOR}"),
        ));
        return ffi::UNSUPPORTED_ABI;
    }
    if out.is_null() || table_size < API_MIN_SIZE {
        set_error(&SdkError::new(
            INVALID_ARGUMENT,
            format!("capsulad_get_api needs a non-null table of at least {API_MIN_SIZE} bytes"),
        ));
        return INVALID_ARGUMENT;
    }
    let copy_len = table_size.min(size_of::<CapsuladApi>());
    unsafe {
        ptr::copy_nonoverlapping(
            &API as *const CapsuladApi as *const u8,
            out as *mut u8,
            copy_len,
        );
    }
    OK
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn api_prefix_is_stable_and_bounded() {
        assert_eq!(API_MIN_SIZE, 4 + 4 + size_of::<usize>());
        assert!(API.struct_size > API_MIN_SIZE);
        assert_eq!(capsulad_abi_major(), 1);
        assert_eq!(capsulad_abi_minor(), 0);
        assert!(!capsulad_api_v1().is_null());
    }

    #[repr(align(8))]
    struct Aligned([u8; 64]);

    #[test]
    fn small_prefix_is_accepted_without_touching_tail() {
        let mut backing = Aligned([0xABu8; 64]);
        let out = backing.0.as_mut_ptr() as *mut CapsuladApi;
        let status = capsulad_get_api(1, API_MIN_SIZE, out);
        assert_eq!(status, OK);
        // Prefix was written; bytes beyond the requested prefix are untouched.
        assert_eq!(backing.0[API_MIN_SIZE..], [0xABu8; 64][API_MIN_SIZE..]);
        unsafe {
            assert_eq!((*out).abi_major, 1);
            assert_eq!((*out).abi_minor, 0);
            assert_eq!((*out).struct_size, size_of::<CapsuladApi>());
        }
    }

    #[test]
    fn unsupported_major_writes_nothing() {
        let mut backing = Aligned([0u8; 64]);
        let out = backing.0.as_mut_ptr() as *mut CapsuladApi;
        let status = capsulad_get_api(2, 64, out);
        assert_eq!(status, ffi::UNSUPPORTED_ABI);
        assert_eq!(backing.0, [0u8; 64]);
    }
}
