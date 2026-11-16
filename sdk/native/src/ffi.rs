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
/// A bounded buffer (request body, response body or header block) exceeded its
/// configured cap. Appended to ABI v1 without changing the existing values.
pub const TOO_LARGE: i32 = 15;
/// The runtime lacks the capability (no control channel for listeners, no mDNS
/// responder). Appended to the v1 table in this pre-release tree.
pub const UNSUPPORTED: i32 = 16;

/// Library-owned output buffer. Kept layout-identical to `capsulad_buf`.
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
            set_last_error("capsulad native: caught internal panic");
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

/// Copy bytes into a library-owned, NUL-terminated buffer.
///
/// Allocation invariant (the only one `free_buf` relies on):
///
/// * an empty payload returns [`Buf::empty`] — `{data = NULL, len = 0}` — and
///   owns no allocation;
/// * every other buffer owns **exactly `len + 1` bytes**, byte `0..len` is the
///   caller-visible payload and byte `len` is a single NUL sentinel. `Buf.len`
///   always equals `bytes.len()`, so the sentinel is never counted in `len`.
///
/// For JSON/UTF-8 outputs the sentinel is the C string terminator promised by
/// `capsulad.h` (`data[len] == 0`); for raw PCM the sentinel is padding outside
/// the payload and `len` stays the exact sample byte count.
pub fn alloc_buf(bytes: Vec<u8>) -> Buf {
    if bytes.is_empty() {
        return Buf::empty();
    }
    let len = bytes.len();
    let mut payload = bytes;
    payload.push(0);
    let mut boxed = payload.into_boxed_slice();
    debug_assert_eq!(boxed.len(), len + 1);
    let data = boxed.as_mut_ptr();
    // Leak the allocation; `free_buf` reconstructs `len + 1` bytes from it.
    std::mem::forget(boxed);
    Buf { data, len }
}

/// Same allocation shape as [`alloc_buf`]: JSON/UTF-8 payload plus the NUL
/// sentinel at `data[len]`, with `len` excluding it.
pub fn alloc_json(bytes: Vec<u8>) -> Buf {
    alloc_buf(bytes)
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
    if buf.len == 0 {
        // The library never emits {non-NULL, 0}; the documented empty buffer is
        // {NULL, 0}. Reject instead of inventing a one-byte allocation.
        return Err(SdkError::new(INVALID_ARGUMENT, "invalid buffer"));
    }
    // Reconstruct exactly the allocation `alloc_buf` made: `len + 1` bytes.
    // The addition is checked, so no input can describe an allocation larger
    // than the one that exists, and the total is inside the `isize::MAX`
    // allocation-size limit Rust guarantees.
    let total = buf.len.checked_add(1).filter(|n| *n <= isize::MAX as usize);
    let Some(total) = total else {
        return Err(SdkError::new(INVALID_ARGUMENT, "invalid buffer"));
    };
    let slice = ptr::slice_from_raw_parts_mut(buf.data, total);
    drop(Box::from_raw(slice));
    Ok(())
}

/// Mirrors `capsulad_pcm_format` in the public header.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default)]
pub struct PcmFormat {
    pub sample_format: i32,
    pub sample_rate_hz: u32,
    pub channels: u32,
    pub reserved: u32,
}

/// Mirrors `capsulad_pcm_event` in the public header.
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

#[cfg(test)]
mod tests {
    use super::*;

    fn payload_at(buf: Buf) -> Vec<u8> {
        assert!(!buf.data.is_null(), "non-empty buffer must own memory");
        assert!(buf.len > 0);
        let bytes = unsafe { std::slice::from_raw_parts(buf.data, buf.len) }.to_vec();
        // The sentinel lives inside the allocation: reading it must be in-bounds.
        let sentinel = unsafe { *buf.data.add(buf.len) };
        unsafe { free_buf(buf) }.expect("free must succeed");
        assert_eq!(sentinel, 0, "data[len] must be the NUL sentinel");
        bytes
    }

    #[test]
    fn json_buffer_excludes_nul_from_len() {
        let payload = br#"{"method":"hal.Status","ok":true}"#.to_vec();
        let buf = alloc_json(payload.clone());
        assert_eq!(buf.len, payload.len());
        assert_eq!(payload_at(buf), payload);
    }

    #[test]
    fn string_buffer_excludes_nul_from_len() {
        let value = "capsulad \u{1F680} hello";
        let buf = alloc_string(value);
        assert_eq!(buf.len, value.len());
        assert_eq!(payload_at(buf), value.as_bytes().to_vec());
    }

    #[test]
    fn raw_pcm_keeps_exact_length() {
        // Little-endian s16 samples including embedded NUL bytes.
        let pcm: Vec<u8> = (0u16..64).map(|s| s.to_le_bytes()[0]).collect();
        let buf = alloc_buf(pcm.clone());
        assert_eq!(buf.len, pcm.len(), "raw payload length must be exact");
        assert_eq!(payload_at(buf), pcm);
    }

    #[test]
    fn empty_buffers_are_null_and_zero() {
        for buf in [alloc_buf(Vec::new()), alloc_json(Vec::new())] {
            assert!(buf.data.is_null());
            assert_eq!(buf.len, 0);
            unsafe { free_buf(buf) }.expect("freeing an empty buffer is a no-op");
        }
        let empty = Buf::empty();
        assert!(empty.data.is_null());
        assert_eq!(empty.len, 0);
    }

    #[test]
    fn free_rejects_impossible_lengths_without_dereferencing() {
        let dangling = std::ptr::NonNull::<u8>::dangling().as_ptr();
        for len in [0, usize::MAX, isize::MAX as usize] {
            let buf = Buf {
                data: dangling,
                len,
            };
            let err = unsafe { free_buf(buf) }.expect_err("impossible length must be rejected");
            assert_eq!(err.status, INVALID_ARGUMENT);
        }
    }
}
