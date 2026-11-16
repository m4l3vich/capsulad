//! Self-contained declarations for the Capsulad shared-engine C ABI v1.
//!
//! The layout mirrors `sdk/native/include/capsulad/capsulad.h` exactly. Nothing
//! here links or includes native sources: the table is copied from the loaded
//! shared object and validated (major, size, struct_size) before use.
//!
//! Ownership: the library allocates every `Buf`; the caller copies the bytes
//! and calls `buf_free` exactly once. Handles are opaque `u64` values, never
//! pointers, and are freed through the matching close/release function.

#![allow(non_camel_case_types)]
// The complete v1 status/event vocabulary is part of the published ABI even
// where a wrapper path uses the matching enum instead of the raw constant.
#![allow(dead_code)]

use std::ffi::{c_char, c_void};

pub const ABI_MAJOR: u32 = 1;
pub const ABI_MINOR: u32 = 0;
pub const WAIT_FOREVER: u32 = 0xFFFF_FFFF;
pub const DEFAULT_SOCKET: &str = "/run/capsulad/hal.sock";

pub const STATUS_OK: i32 = 0;
pub const STATUS_INVALID_ARGUMENT: i32 = 1;
pub const STATUS_UNSUPPORTED_ABI: i32 = 2;
pub const STATUS_NOT_CONNECTED: i32 = 3;
pub const STATUS_CLOSED: i32 = 4;
pub const STATUS_DESCRIPTOR_NOT_FOUND: i32 = 5;
pub const STATUS_INVALID_MESSAGE: i32 = 6;
pub const STATUS_TRANSPORT: i32 = 7;
pub const STATUS_GRPC: i32 = 8;
pub const STATUS_END_OF_STREAM: i32 = 9;
pub const STATUS_TIMEOUT: i32 = 10;
pub const STATUS_CANCELLED: i32 = 11;
pub const STATUS_NOT_FOUND: i32 = 12;
pub const STATUS_BUSY: i32 = 13;
pub const STATUS_INTERNAL: i32 = 14;
pub const STATUS_TOO_LARGE: i32 = 15;

pub const EVENT_UNKNOWN: i32 = 0;
pub const EVENT_INVOCATION: i32 = 1;
pub const EVENT_SETTINGS_CHANGED: i32 = 2;
pub const EVENT_ACTION: i32 = 3;
pub const EVENT_ACTIVATION: i32 = 4;
pub const EVENT_TOUCH_RELEASED: i32 = 5;

pub const MEDIA_UNKNOWN: i32 = 0;
pub const MEDIA_STARTED: i32 = 1;
pub const MEDIA_CHUNK: i32 = 2;
pub const MEDIA_NOTICE: i32 = 3;

pub const PLAYBACK_UNSPECIFIED: i32 = 0;
pub const PLAYBACK_FLUSH: i32 = 1;
pub const PLAYBACK_PAUSE: i32 = 2;
pub const PLAYBACK_RESUME: i32 = 3;

pub const SAMPLE_UNSPECIFIED: i32 = 0;
pub const SAMPLE_PCM_S16LE: i32 = 1;
pub const SAMPLE_PCM_F32LE: i32 = 2;

#[repr(C)]
#[derive(Clone, Copy)]
pub struct Buf {
    pub data: *mut u8,
    pub len: usize,
}

impl Default for Buf {
    fn default() -> Self {
        Self {
            data: std::ptr::null_mut(),
            len: 0,
        }
    }
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
pub struct PcmFormat {
    pub sample_format: i32,
    pub sample_rate_hz: u32,
    pub channels: u32,
    pub reserved: u32,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub struct PcmEvent {
    pub kind: i32,
    pub discontinuity: u32,
    pub sequence: u64,
    pub first_frame: u64,
    pub monotonic_time_ns: u64,
    pub frame_count: u32,
    pub reserved: u32,
    pub dropped_frames_before: u64,
    pub pcm: Buf,
    pub json: Buf,
}

impl Default for PcmEvent {
    fn default() -> Self {
        Self {
            kind: MEDIA_UNKNOWN,
            discontinuity: 0,
            sequence: 0,
            first_frame: 0,
            monotonic_time_ns: 0,
            frame_count: 0,
            reserved: 0,
            dropped_frames_before: 0,
            pcm: Buf::default(),
            json: Buf::default(),
        }
    }
}

pub type ReadyFn = unsafe extern "C" fn(u64, *mut c_void);
pub type DisconnectedFn = unsafe extern "C" fn(u64, i32, *const c_char, *mut c_void);
pub type EventFn = unsafe extern "C" fn(u64, i32, *const u8, usize, *mut c_void);
pub type CallFn = unsafe extern "C" fn(u64, u64, *const u8, usize, *mut c_void);
pub type LogFn = unsafe extern "C" fn(i32, *const c_char, *mut c_void);

#[repr(C)]
pub struct BusCallbacksV1 {
    pub struct_size: usize,
    pub user_data: *mut c_void,
    pub on_ready: Option<ReadyFn>,
    pub on_disconnected: Option<DisconnectedFn>,
    pub on_event: Option<EventFn>,
    pub on_call: Option<CallFn>,
    pub on_log: Option<LogFn>,
}

impl BusCallbacksV1 {
    pub fn new(
        user_data: *mut c_void,
        on_ready: Option<ReadyFn>,
        on_disconnected: Option<DisconnectedFn>,
        on_event: Option<EventFn>,
        on_call: Option<CallFn>,
        on_log: Option<LogFn>,
    ) -> Self {
        Self {
            struct_size: std::mem::size_of::<Self>(),
            user_data,
            on_ready,
            on_disconnected,
            on_event,
            on_call,
            on_log,
        }
    }
}

pub type ClientConnectFn = unsafe extern "C" fn(
    *const c_char,
    usize,
    *const c_char,
    usize,
    *const c_char,
    usize,
    u32,
    *mut u64,
) -> i32;
pub type ClientCloseFn = unsafe extern "C" fn(u64) -> i32;
pub type RegisterDescriptorsFn = unsafe extern "C" fn(u64, *const u8, usize) -> i32;
pub type ClientInfoFn = unsafe extern "C" fn(u64, *mut Buf) -> i32;
pub type ClientUnaryFn = unsafe extern "C" fn(
    u64,
    *const c_char,
    usize,
    *const c_char,
    usize,
    *const u8,
    usize,
    u32,
    *mut Buf,
) -> i32;
pub type CallStartFn = unsafe extern "C" fn(
    u64,
    *const c_char,
    usize,
    *const c_char,
    usize,
    i32,
    u32,
    *mut u64,
) -> i32;
pub type CallStartRoutedFn = unsafe extern "C" fn(
    u64,
    *const c_char,
    usize,
    *const c_char,
    usize,
    *const c_char,
    usize,
    u32,
    *mut u64,
) -> i32;
pub type CallSendFn = unsafe extern "C" fn(u64, *const u8, usize) -> i32;
pub type CallHalfCloseFn = unsafe extern "C" fn(u64) -> i32;
pub type CallRecvFn = unsafe extern "C" fn(u64, u32, *mut Buf) -> i32;
pub type CallEndFn = unsafe extern "C" fn(u64, i32, *const c_char, usize) -> i32;
pub type CallHandleFn = unsafe extern "C" fn(u64) -> i32;
pub type CallInfoFn = unsafe extern "C" fn(u64, *mut Buf) -> i32;
pub type CallStatusFn = unsafe extern "C" fn(u64, *mut i32, *mut Buf) -> i32;
pub type BusOpenFn = unsafe extern "C" fn(u64, *const BusCallbacksV1, *mut u64) -> i32;
pub type BusCloseFn = unsafe extern "C" fn(u64) -> i32;
pub type BusReadyFn = unsafe extern "C" fn(u64, u32) -> i32;
pub type BusNotifyInvocationFn =
    unsafe extern "C" fn(u64, *const c_char, usize, i32, *const c_char, usize) -> i32;
pub type BusNotifyActionFn =
    unsafe extern "C" fn(u64, *const c_char, usize, i32, *const c_char, usize) -> i32;
pub type AudioCaptureOpenFn = unsafe extern "C" fn(
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
) -> i32;
pub type AudioCaptureRecvFn = unsafe extern "C" fn(u64, u32, *mut PcmEvent) -> i32;
pub type AudioPlaybackOpenFn = unsafe extern "C" fn(
    u64,
    *const c_char,
    usize,
    *const PcmFormat,
    u32,
    *const c_char,
    usize,
    *mut u64,
) -> i32;
pub type AudioPlaybackWriteFn =
    unsafe extern "C" fn(u64, *const u8, usize, u32, u64, i32, u64) -> i32;
pub type AudioPlaybackControlFn = unsafe extern "C" fn(u64, i32) -> i32;
pub type BufFreeFn = unsafe extern "C" fn(Buf) -> i32;
pub type LastErrorFn = unsafe extern "C" fn() -> *const c_char;
pub type HandleInfoFn = unsafe extern "C" fn(u64, *mut Buf) -> i32;

// Appended v1 surface.
pub type HttpOpenFn = unsafe extern "C" fn(*const u8, usize, u32, *mut u64) -> i32;
pub type HttpCloseFn = unsafe extern "C" fn(u64) -> i32;
pub type HttpRequestFn = unsafe extern "C" fn(
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
) -> i32;
pub type GrpcOpenFn =
    unsafe extern "C" fn(*const c_char, usize, *const u8, usize, u32, *mut u64) -> i32;
pub type CallStartMetaFn = unsafe extern "C" fn(
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
) -> i32;
pub type Base64Fn = unsafe extern "C" fn(*const u8, usize, i32, *mut Buf) -> i32;
pub type PrimitiveFn = unsafe extern "C" fn(*const u8, usize, *mut Buf) -> i32;
pub type RandomBytesFn = unsafe extern "C" fn(*mut u8, usize) -> i32;
pub type UuidV4Fn = unsafe extern "C" fn(*mut Buf) -> i32;
pub type UrlParseFn = unsafe extern "C" fn(*const c_char, usize, *mut Buf) -> i32;
pub type HttpCancelFn = unsafe extern "C" fn(u64, u64) -> i32;

/// Function table. Field order is frozen by ABI v1; future minors only append.
#[repr(C)]
#[derive(Clone, Copy)]
pub struct Api {
    pub abi_major: u32,
    pub abi_minor: u32,
    pub struct_size: usize,

    pub client_connect: ClientConnectFn,
    pub client_close: ClientCloseFn,
    pub client_register_descriptors: RegisterDescriptorsFn,
    pub client_info: ClientInfoFn,
    pub client_unary: ClientUnaryFn,
    pub client_unary_proto: ClientUnaryFn,

    pub call_start: CallStartFn,
    pub call_start_routed: CallStartRoutedFn,
    pub call_send_json: CallSendFn,
    pub call_send_proto: CallSendFn,
    pub call_half_close: CallHalfCloseFn,
    pub call_recv_json: CallRecvFn,
    pub call_recv_proto: CallRecvFn,
    pub call_end: CallEndFn,
    pub call_cancel: CallHandleFn,
    pub call_retain: CallHandleFn,
    pub call_release: CallHandleFn,
    pub call_info: CallInfoFn,
    pub call_status: CallStatusFn,

    pub bus_open: BusOpenFn,
    pub bus_close: BusCloseFn,
    pub bus_ready: BusReadyFn,
    pub bus_notify_invocation: BusNotifyInvocationFn,
    pub bus_notify_action: BusNotifyActionFn,

    pub audio_capture_open: AudioCaptureOpenFn,
    pub audio_capture_recv: AudioCaptureRecvFn,
    pub audio_playback_open: AudioPlaybackOpenFn,
    pub audio_playback_write: AudioPlaybackWriteFn,
    pub audio_playback_control: AudioPlaybackControlFn,

    pub buf_free: BufFreeFn,
    pub last_error: LastErrorFn,
    pub handle_info: HandleInfoFn,

    // Appended in v1: HTTP, remote gRPC and primitives.
    pub http_open: HttpOpenFn,
    pub http_close: HttpCloseFn,
    pub http_request: HttpRequestFn,
    pub grpc_open: GrpcOpenFn,
    pub call_start_meta: CallStartMetaFn,
    pub base64_encode: Base64Fn,
    pub base64_decode: Base64Fn,
    pub percent_encode: PrimitiveFn,
    pub percent_decode: PrimitiveFn,
    pub sha256: PrimitiveFn,
    pub random_bytes: RandomBytesFn,
    pub uuid_v4: UuidV4Fn,
    pub url_parse: UrlParseFn,
    pub http_cancel: HttpCancelFn,
}

/// Bytes this binding was compiled against. A native table must be at least
/// this large; a larger table from a newer minor is forward compatible.
pub const TABLE_SIZE: usize = std::mem::size_of::<Api>();

impl Api {
    /// Every function pointer this binding calls must be present in the
    /// returned table. `struct_size` already proved the bytes exist; a zero
    /// pointer still means the library cannot satisfy ABI v1.
    pub fn validate_non_null(&self) -> std::result::Result<(), String> {
        fn set<F: Copy>(field: F) -> bool {
            std::mem::size_of::<F>() == std::mem::size_of::<usize>()
                && unsafe { std::mem::transmute_copy::<F, usize>(&field) } != 0
        }
        let required: [(&str, bool); 46] = [
            ("client_connect", set(self.client_connect)),
            ("client_close", set(self.client_close)),
            (
                "client_register_descriptors",
                set(self.client_register_descriptors),
            ),
            ("client_info", set(self.client_info)),
            ("client_unary", set(self.client_unary)),
            ("client_unary_proto", set(self.client_unary_proto)),
            ("call_start", set(self.call_start)),
            ("call_start_routed", set(self.call_start_routed)),
            ("call_send_json", set(self.call_send_json)),
            ("call_send_proto", set(self.call_send_proto)),
            ("call_half_close", set(self.call_half_close)),
            ("call_recv_json", set(self.call_recv_json)),
            ("call_recv_proto", set(self.call_recv_proto)),
            ("call_end", set(self.call_end)),
            ("call_cancel", set(self.call_cancel)),
            ("call_retain", set(self.call_retain)),
            ("call_release", set(self.call_release)),
            ("call_info", set(self.call_info)),
            ("call_status", set(self.call_status)),
            ("bus_open", set(self.bus_open)),
            ("bus_close", set(self.bus_close)),
            ("bus_ready", set(self.bus_ready)),
            ("bus_notify_invocation", set(self.bus_notify_invocation)),
            ("bus_notify_action", set(self.bus_notify_action)),
            ("audio_capture_open", set(self.audio_capture_open)),
            ("audio_capture_recv", set(self.audio_capture_recv)),
            ("audio_playback_open", set(self.audio_playback_open)),
            ("audio_playback_write", set(self.audio_playback_write)),
            ("audio_playback_control", set(self.audio_playback_control)),
            ("buf_free", set(self.buf_free)),
            ("last_error", set(self.last_error)),
            ("handle_info", set(self.handle_info)),
            ("http_open", set(self.http_open)),
            ("http_close", set(self.http_close)),
            ("http_request", set(self.http_request)),
            ("grpc_open", set(self.grpc_open)),
            ("call_start_meta", set(self.call_start_meta)),
            ("base64_encode", set(self.base64_encode)),
            ("base64_decode", set(self.base64_decode)),
            ("percent_encode", set(self.percent_encode)),
            ("percent_decode", set(self.percent_decode)),
            ("sha256", set(self.sha256)),
            ("random_bytes", set(self.random_bytes)),
            ("uuid_v4", set(self.uuid_v4)),
            ("url_parse", set(self.url_parse)),
            ("http_cancel", set(self.http_cancel)),
        ];
        for (name, present) in required {
            if !present {
                return Err(format!("native ABI table has a null {name} pointer"));
            }
        }
        Ok(())
    }

    pub fn negotiate(&self) -> std::result::Result<(), String> {
        if self.abi_major != ABI_MAJOR {
            return Err(format!(
                "native ABI major {} is not supported (binding requires {})",
                self.abi_major, ABI_MAJOR
            ));
        }
        if self.struct_size < TABLE_SIZE {
            return Err(format!(
                "native ABI table is {} bytes, binding requires at least {}",
                self.struct_size, TABLE_SIZE
            ));
        }
        Ok(())
    }
}

/// Compile-time checks of the published C layout. These are independent of the
/// native repository so the published crate can be tested alone.
#[cfg(test)]
mod tests {
    use super::*;
    use std::mem::{align_of, size_of};
    use std::ptr::addr_of;

    fn offset<T, F>(base: *const T, field: *const F) -> usize {
        field as usize - base as usize
    }

    #[test]
    fn scalar_and_buffer_layout() {
        assert_eq!(size_of::<Buf>(), 16);
        assert_eq!(align_of::<Buf>(), 8);
        assert_eq!(size_of::<PcmFormat>(), 16);
        assert_eq!(size_of::<PcmEvent>(), 80);
        assert_eq!(align_of::<PcmEvent>(), 8);
        assert_eq!(size_of::<BusCallbacksV1>(), 56);
        assert_eq!(size_of::<u64>(), 8);
        assert_eq!(ABI_MAJOR, 1);
        assert_eq!(ABI_MINOR, 0);
        assert_eq!(WAIT_FOREVER, 0xFFFF_FFFF);
    }

    #[test]
    fn status_and_event_values_are_pinned() {
        assert_eq!(STATUS_OK, 0);
        assert_eq!(STATUS_INVALID_ARGUMENT, 1);
        assert_eq!(STATUS_UNSUPPORTED_ABI, 2);
        assert_eq!(STATUS_NOT_CONNECTED, 3);
        assert_eq!(STATUS_CLOSED, 4);
        assert_eq!(STATUS_DESCRIPTOR_NOT_FOUND, 5);
        assert_eq!(STATUS_INVALID_MESSAGE, 6);
        assert_eq!(STATUS_TRANSPORT, 7);
        assert_eq!(STATUS_GRPC, 8);
        assert_eq!(STATUS_END_OF_STREAM, 9);
        assert_eq!(STATUS_TIMEOUT, 10);
        assert_eq!(STATUS_CANCELLED, 11);
        assert_eq!(STATUS_NOT_FOUND, 12);
        assert_eq!(STATUS_BUSY, 13);
        assert_eq!(STATUS_INTERNAL, 14);
        assert_eq!(STATUS_TOO_LARGE, 15);

        assert_eq!(EVENT_UNKNOWN, 0);
        assert_eq!(EVENT_INVOCATION, 1);
        assert_eq!(EVENT_SETTINGS_CHANGED, 2);
        assert_eq!(EVENT_ACTION, 3);
        assert_eq!(EVENT_ACTIVATION, 4);
        assert_eq!(EVENT_TOUCH_RELEASED, 5);

        assert_eq!(MEDIA_UNKNOWN, 0);
        assert_eq!(MEDIA_STARTED, 1);
        assert_eq!(MEDIA_CHUNK, 2);
        assert_eq!(MEDIA_NOTICE, 3);

        assert_eq!(PLAYBACK_UNSPECIFIED, 0);
        assert_eq!(PLAYBACK_FLUSH, 1);
        assert_eq!(PLAYBACK_PAUSE, 2);
        assert_eq!(PLAYBACK_RESUME, 3);

        assert_eq!(SAMPLE_UNSPECIFIED, 0);
        assert_eq!(SAMPLE_PCM_S16LE, 1);
        assert_eq!(SAMPLE_PCM_F32LE, 2);
    }

    #[test]
    fn table_layout_is_pinned() {
        let base = std::mem::MaybeUninit::<Api>::uninit();
        let p = base.as_ptr();
        unsafe {
            assert_eq!(offset(p, addr_of!((*p).abi_major)), 0);
            assert_eq!(offset(p, addr_of!((*p).abi_minor)), 4);
            assert_eq!(offset(p, addr_of!((*p).struct_size)), 8);
            assert_eq!(offset(p, addr_of!((*p).client_connect)), 16);
            assert_eq!(offset(p, addr_of!((*p).client_close)), 24);
            assert_eq!(offset(p, addr_of!((*p).client_register_descriptors)), 32);
            assert_eq!(offset(p, addr_of!((*p).client_info)), 40);
            assert_eq!(offset(p, addr_of!((*p).client_unary)), 48);
            assert_eq!(offset(p, addr_of!((*p).client_unary_proto)), 56);
            assert_eq!(offset(p, addr_of!((*p).call_start)), 64);
            assert_eq!(offset(p, addr_of!((*p).call_start_routed)), 72);
            assert_eq!(offset(p, addr_of!((*p).call_send_json)), 80);
            assert_eq!(offset(p, addr_of!((*p).call_send_proto)), 88);
            assert_eq!(offset(p, addr_of!((*p).call_half_close)), 96);
            assert_eq!(offset(p, addr_of!((*p).call_recv_json)), 104);
            assert_eq!(offset(p, addr_of!((*p).call_recv_proto)), 112);
            assert_eq!(offset(p, addr_of!((*p).call_end)), 120);
            assert_eq!(offset(p, addr_of!((*p).call_cancel)), 128);
            assert_eq!(offset(p, addr_of!((*p).call_retain)), 136);
            assert_eq!(offset(p, addr_of!((*p).call_release)), 144);
            assert_eq!(offset(p, addr_of!((*p).call_info)), 152);
            assert_eq!(offset(p, addr_of!((*p).call_status)), 160);
            assert_eq!(offset(p, addr_of!((*p).bus_open)), 168);
            assert_eq!(offset(p, addr_of!((*p).bus_close)), 176);
            assert_eq!(offset(p, addr_of!((*p).bus_ready)), 184);
            assert_eq!(offset(p, addr_of!((*p).bus_notify_invocation)), 192);
            assert_eq!(offset(p, addr_of!((*p).bus_notify_action)), 200);
            assert_eq!(offset(p, addr_of!((*p).audio_capture_open)), 208);
            assert_eq!(offset(p, addr_of!((*p).audio_capture_recv)), 216);
            assert_eq!(offset(p, addr_of!((*p).audio_playback_open)), 224);
            assert_eq!(offset(p, addr_of!((*p).audio_playback_write)), 232);
            assert_eq!(offset(p, addr_of!((*p).audio_playback_control)), 240);
            assert_eq!(offset(p, addr_of!((*p).buf_free)), 248);
            assert_eq!(offset(p, addr_of!((*p).last_error)), 256);
            assert_eq!(offset(p, addr_of!((*p).handle_info)), 264);
            assert_eq!(offset(p, addr_of!((*p).http_open)), 272);
            assert_eq!(offset(p, addr_of!((*p).http_close)), 280);
            assert_eq!(offset(p, addr_of!((*p).http_request)), 288);
            assert_eq!(offset(p, addr_of!((*p).grpc_open)), 296);
            assert_eq!(offset(p, addr_of!((*p).call_start_meta)), 304);
            assert_eq!(offset(p, addr_of!((*p).base64_encode)), 312);
            assert_eq!(offset(p, addr_of!((*p).base64_decode)), 320);
            assert_eq!(offset(p, addr_of!((*p).percent_encode)), 328);
            assert_eq!(offset(p, addr_of!((*p).percent_decode)), 336);
            assert_eq!(offset(p, addr_of!((*p).sha256)), 344);
            assert_eq!(offset(p, addr_of!((*p).random_bytes)), 352);
            assert_eq!(offset(p, addr_of!((*p).uuid_v4)), 360);
            assert_eq!(offset(p, addr_of!((*p).url_parse)), 368);
            assert_eq!(offset(p, addr_of!((*p).http_cancel)), 376);
        }
        assert_eq!(TABLE_SIZE, 384);
    }
}
