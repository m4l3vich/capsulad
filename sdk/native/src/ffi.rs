//! FFI-safe primitives: status codes, owned buffers, thread-local errors.

use std::cell::RefCell;
use std::ffi::{c_char, CString};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::ptr;

pub const OK: i32 = 0;
pub const INVALID_ARGUMENT: i32 = 1;
pub const UNSUPPORTED_ABI: i32 = 2;
pub const NOT_CONNECTED: i32 = 3;
pub const CLOSED: i32 = 4;
pub const DESCRIPTOR_NOT_FOUND: i32 = 5;
pub const INVALID_MESSAGE: i32 = 6;
pub const TRANSPORT: i32 = 7;
pub const GRPC: i32 = 8;
pub const END_OF_STREAM: i32 = 9;
pub const TIMEOUT: i32 = 10;
pub const CANCELLED: i32 = 11;
pub const NOT_FOUND: i32 = 12;
pub const BUSY: i32 = 13;
pub const INTERNAL: i32 = 14;

/// Library-owned output buffer. Kept layout-identical to `capsula_buf`.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct Buf {
    pub data: *mut u8,
    pub len: usize,
}

impl Buf {
    pub fn empty() -> Self {
        Self {
            data: ptr::null_mut(),
            len: 0,
        }
    }
}

#[derive(Debug, Clone)]
pub struct SdkError {
    pub status: i32,
    pub message: String,
    pub grpc_code: Option<i32>,
}

impl SdkError {
    pub fn new(status: i32, message: impl Into<String>) -> Self {
        Self {
            status,
            message: message.into(),
            grpc_code: None,
        }
    }

    pub fn grpc(message: impl Into<String>, code: i32) -> Self {
        Self {
            status: GRPC,
            message: message.into(),
            grpc_code: Some(code),
        }
    }
}

pub type SdkResult<T> = Result<T, SdkError>;

pub fn map_grpc(status: tonic::Status) -> SdkError {
    SdkError::grpc(
        format!("gRPC {}: {}", status.code(), status.message()),
        status.code() as i32,
    )
}

pub fn map_transport(err: impl std::fmt::Display) -> SdkError {
    SdkError::new(TRANSPORT, err.to_string())
}

thread_local! {
    static LAST_ERROR: RefCell<CString> = RefCell::new(CString::new("").unwrap());
}

pub fn set_last_error(message: &str) {
    let cleaned: String = message.chars().filter(|c| *c != '\0').collect();
    LAST_ERROR.with(|slot| {
        *slot.borrow_mut() = CString::new(cleaned).unwrap_or_default();
    });
}

pub fn set_error(err: &SdkError) {
    set_last_error(&err.message);
}

pub fn last_error_ptr() -> *const c_char {
    LAST_ERROR.with(|slot| slot.borrow().as_ptr())
}

pub fn guard(f: impl FnOnce() -> i32) -> i32 {
    match catch_unwind(AssertUnwindSafe(f)) {
        Ok(status) => status,
        Err(_) => {
            set_last_error("capsula native: caught internal panic");
            INTERNAL
        }
    }
}

/// Borrow an input byte slice. NULL with len 0 is empty.
///
/// # Safety
/// `ptr` must point to at least `len` readable bytes when `len > 0`.
pub unsafe fn input_bytes<'a>(ptr: *const u8, len: usize) -> SdkResult<&'a [u8]> {
    if len == 0 {
        return Ok(&[]);
    }
    if ptr.is_null() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "null data pointer with non-zero length",
        ));
    }
    if len > isize::MAX as usize {
        return Err(SdkError::new(INVALID_ARGUMENT, "length out of range"));
    }
    Ok(std::slice::from_raw_parts(ptr, len))
}

/// Borrow an input UTF-8 string. NULL with len 0 is empty.
///
/// # Safety
/// `ptr` must point to at least `len` readable bytes when `len > 0`.
pub unsafe fn input_str<'a>(ptr: *const c_char, len: usize) -> SdkResult<&'a str> {
    if len == 0 {
        return Ok("");
    }
    if ptr.is_null() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "null string pointer with non-zero length",
        ));
    }
    let bytes = std::slice::from_raw_parts(ptr as *const u8, len);
    std::str::from_utf8(bytes)
        .map_err(|_| SdkError::new(INVALID_ARGUMENT, "input is not valid UTF-8"))
}

/// Copy bytes into a library-owned, optionally NUL-terminated buffer.
pub fn alloc_buf(bytes: Vec<u8>) -> Buf {
    if bytes.is_empty() {
        return Buf::empty();
    }
    let mut boxed = bytes.into_boxed_slice();
    let data = boxed.as_mut_ptr();
    let len = boxed.len();
    // Leak the allocation; capsula_buf_free reconstructs it.
    std::mem::forget(boxed);
    Buf { data, len }
}

pub fn alloc_json(bytes: Vec<u8>) -> Buf {
    // JSON/UTF-8 outputs get a trailing NUL for C convenience; `len` excludes it.
    let mut with_nul = bytes;
    with_nul.push(0);
    alloc_buf(with_nul)
}

pub fn alloc_string(value: impl Into<String>) -> Buf {
    alloc_json(value.into().into_bytes())
}

/// # Safety
/// `buf` must come from this library and not have been freed before.
pub unsafe fn free_buf(buf: Buf) -> SdkResult<()> {
    if buf.data.is_null() {
        return Ok(());
    }
    if buf.len == 0 || buf.len > isize::MAX as usize {
        return Err(SdkError::new(INVALID_ARGUMENT, "invalid buffer"));
    }
    // All buffers keep a trailing NUL byte; reconstruct exactly that slice.
    let total = buf.len + 1;
    let slice = ptr::slice_from_raw_parts_mut(buf.data, total);
    drop(Box::from_raw(slice));
    Ok(())
}

/// Mirrors `capsula_pcm_format` in the public header.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct PcmFormat {
    pub sample_format: i32,
    pub sample_rate_hz: u32,
    pub channels: u32,
    pub reserved: u32,
}

/// Mirrors `capsula_pcm_event` in the public header.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
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

/// # Safety
/// `out` must be a valid writable pointer.
pub unsafe fn write_out<T>(out: *mut T, value: T) -> SdkResult<()> {
    if out.is_null() {
        return Err(SdkError::new(INVALID_ARGUMENT, "null output pointer"));
    }
    ptr::write(out, value);
    Ok(())
}
