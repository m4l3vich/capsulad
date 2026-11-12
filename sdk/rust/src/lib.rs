//! Safe Rust consumer for the Capsula shared engine (C ABI v1).
//!
//! The crate loads `libcapsula` dynamically (never linking transport code or a
//! native crate), negotiates the ABI table, and wraps every native allocation
//! in an RAII type. It is a thin binding: protobuf JSON/proto payloads are
//! opaque byte slices and the shared library owns the daemon transport.
//!
//! # Library resolution
//!
//! [`Library::load`] uses `CAPSULA_LIBRARY` when it is set (an absolute path,
//! normally `/capsula/lib/libcapsula.so.1` inside a sandbox), then
//! `CAPSULA_LIBRARY_DIR` joined with the platform soname, then the soname
//! itself (`libcapsula.so.1` on Linux, `libcapsula.1.dylib` on Darwin).
//!
//! # ABI negotiation
//!
//! The loader resolves `capsula_get_api`, zero-initializes a table, copies
//! `min(binding_size, library_size)` bytes, and requires major 1, a
//! `struct_size` covering every v1 field used here, and non-NULL required
//! function pointers. A newer minor may append fields.
//!
//! # Build and smoke tests
//!
//! ```sh
//! cargo build --manifest-path sdk/rust/Cargo.toml
//! cargo test --manifest-path sdk/rust/Cargo.toml
//! cargo build --manifest-path sdk/rust/Cargo.toml --examples
//! ```
//!
//! The `smoke_unary`, `smoke_stream` and `smoke_bus` examples connect to a
//! real daemon using `CAPSULA_SOCKET`/`CAPSULA_RUN_ID` (or
//! `Library::connect_env`). They exercise unary, server-streaming and AppBus
//! paths; no transport is linked into the Rust binary.
//!
//! ```no_run
//! use capsula::Library;
//!
//! let library = Library::load().expect("load libcapsula");
//! let client = library.connect_env().expect("connect");
//! let reply = client
//!     .unary_json(
//!         "capsula.framework.v1.DeviceService",
//!         "GetInfo",
//!         b"{}",
//!         Some(std::time::Duration::from_secs(2)),
//!     )
//!     .expect("unary call");
//! println!("{}", String::from_utf8_lossy(&reply));
//! ```

mod audio;
mod bus;
mod call;
mod client;
mod error;
mod sys;

pub use audio::{PcmEvent, PcmFormat, PcmKind, PlaybackCommand, SampleFormat};
pub use bus::{Bus, BusEvent, BusEventKind, BusHandler, CallInfo};
pub use call::{Call, CallKind, CallStatus, InboundCall};
pub use client::Client;
pub use error::{Error, Result};
pub use sys::DEFAULT_SOCKET;

use std::env;
use std::ffi::{c_char, CStr, OsStr};
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

/// Shared library state: the copied function table, the loader handle that
/// keeps it mapped, and the path used for diagnostics.
pub(crate) struct Inner {
    pub(crate) api: sys::Api,
    pub(crate) version: String,
    pub(crate) path: PathBuf,
}

/// A loaded Capsula shared library.
///
/// Cloning is cheap; every clone refers to the same mapped library. The
/// library is never unloaded while a [`Client`], [`Call`] or [`Bus`] exists.
#[derive(Clone)]
pub struct Library {
    pub(crate) inner: Arc<Inner>,
}

impl Library {
    /// Loads the library using the launcher contract:
    ///
    /// 1. `CAPSULA_LIBRARY` when set (must be an absolute path);
    /// 2. `CAPSULA_LIBRARY_DIR` joined with the platform soname;
    /// 3. the platform soname (`libcapsula.so.1` on Linux,
    ///    `libcapsula.1.dylib` on Darwin).
    pub fn load() -> Result<Self> {
        Self::open(resolve_library_path()?)
    }

    /// Loads an explicit shared library path.
    pub fn open(path: impl AsRef<OsStr>) -> Result<Self> {
        let path = PathBuf::from(path.as_ref());
        let lib = unsafe { libloading::Library::new(path.as_os_str()) }.map_err(|err| {
            Error::local(
                error::CODE_LOAD,
                format!("cannot load {}: {err}", path.display()),
            )
        })?;

        type SizeFn = unsafe extern "C" fn() -> usize;
        type MajorFn = unsafe extern "C" fn() -> u32;
        type MinorFn = unsafe extern "C" fn() -> u32;
        type VersionFn = unsafe extern "C" fn() -> *const c_char;

        let api_major = {
            let f: libloading::Symbol<MajorFn> =
                unsafe { lib.get(b"capsula_abi_major\0") }.map_err(symbol_error)?;
            unsafe { f() }
        };
        let _api_minor = {
            let f: libloading::Symbol<MinorFn> =
                unsafe { lib.get(b"capsula_abi_minor\0") }.map_err(symbol_error)?;
            unsafe { f() }
        };
        if api_major != sys::ABI_MAJOR {
            return Err(Error::local(
                error::CODE_ABI,
                format!(
                    "{} reports ABI major {api_major}, binding supports {}",
                    path.display(),
                    sys::ABI_MAJOR
                ),
            ));
        }

        let native_size = {
            let f: libloading::Symbol<SizeFn> =
                unsafe { lib.get(b"capsula_api_size\0") }.map_err(symbol_error)?;
            unsafe { f() }
        };
        if native_size < sys::TABLE_SIZE {
            return Err(Error::local(
                error::CODE_ABI,
                format!(
                    "{} reports a {native_size}-byte ABI table, binding requires {}",
                    path.display(),
                    sys::TABLE_SIZE
                ),
            ));
        }

        // Zero-initialize the caller table, then let the library copy
        // min(caller_size, library_size) bytes. `struct_size` reports the
        // library's full table so appended minor fields are detectable.
        type GetApiFn = unsafe extern "C" fn(u32, usize, *mut sys::Api) -> i32;
        let mut slot = std::mem::MaybeUninit::<sys::Api>::zeroed();
        let status = {
            let f: libloading::Symbol<GetApiFn> =
                unsafe { lib.get(b"capsula_get_api\0") }.map_err(symbol_error)?;
            unsafe { f(sys::ABI_MAJOR, sys::TABLE_SIZE, slot.as_mut_ptr()) }
        };
        if status != sys::STATUS_OK {
            return Err(Error::local(
                error::CODE_ABI,
                format!(
                    "{} refused capsula_get_api({}, {}): status {status}",
                    path.display(),
                    sys::ABI_MAJOR,
                    sys::TABLE_SIZE
                ),
            ));
        }
        let reported_size = unsafe { (*slot.as_ptr()).struct_size };
        if reported_size < sys::TABLE_SIZE {
            return Err(Error::local(
                error::CODE_ABI,
                format!(
                    "{} reported a {reported_size}-byte ABI table, binding requires {}",
                    path.display(),
                    sys::TABLE_SIZE
                ),
            ));
        }
        let api = unsafe { slot.assume_init() };
        api.negotiate()
            .and_then(|()| api.validate_non_null())
            .map_err(|msg| Error::local(error::CODE_ABI, format!("{}: {msg}", path.display())))?;

        let version = {
            let f: libloading::Symbol<VersionFn> =
                unsafe { lib.get(b"capsula_version\0") }.map_err(symbol_error)?;
            let ptr = unsafe { f() };
            if ptr.is_null() {
                String::new()
            } else {
                unsafe { CStr::from_ptr(ptr) }
                    .to_string_lossy()
                    .into_owned()
            }
        };

        // The shared engine owns a process-global Tokio runtime whose worker
        // threads outlive clients, calls and buses. Unloading a successfully
        // initialized library would leave those threads executing in unmapped
        // memory, so the mapped handle is deliberately retained for the
        // process lifetime (like a static link, but resolved at runtime).
        std::mem::forget(lib);
        Ok(Self {
            inner: Arc::new(Inner { api, version, path }),
        })
    }

    /// Library version string reported by `capsula_version`.
    pub fn version(&self) -> &str {
        &self.inner.version
    }

    pub fn abi_major(&self) -> u32 {
        self.inner.api.abi_major
    }

    pub fn abi_minor(&self) -> u32 {
        self.inner.api.abi_minor
    }

    /// Path the library was loaded from.
    pub fn path(&self) -> &Path {
        &self.inner.path
    }

    /// Opens a client for a supervised app identity.
    pub fn connect(&self, socket: impl AsRef<OsStr>, run_id: &str) -> Result<Client> {
        Client::connect(self, socket, run_id, None, None)
    }

    /// Opens a client using `CAPSULA_SOCKET`, `CAPSULA_RUN_ID` and
    /// `CAPSULA_APP_ID` as injected by the launcher.
    pub fn connect_env(&self) -> Result<Client> {
        Client::from_env(self)
    }
}

fn symbol_error(err: libloading::Error) -> Error {
    Error::local(error::CODE_LOAD, format!("missing ABI symbol: {err}"))
}

/// Platform soname used when no explicit path is configured.
pub fn default_soname() -> &'static str {
    #[cfg(target_os = "linux")]
    {
        "libcapsula.so.1"
    }
    #[cfg(target_os = "macos")]
    {
        "libcapsula.1.dylib"
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        "libcapsula.so.1"
    }
}

fn resolve_library_path() -> Result<PathBuf> {
    if let Some(value) = env::var_os("CAPSULA_LIBRARY") {
        if !value.is_empty() {
            let path = PathBuf::from(value);
            if !path.is_absolute() {
                return Err(Error::local(
                    error::CODE_INVALID_ARGUMENT,
                    format!(
                        "CAPSULA_LIBRARY must be an absolute path, got {}",
                        path.display()
                    ),
                ));
            }
            return Ok(path);
        }
    }
    if let Some(dir) = env::var_os("CAPSULA_LIBRARY_DIR") {
        if !dir.is_empty() {
            return Ok(PathBuf::from(dir).join(default_soname()));
        }
    }
    Ok(PathBuf::from(default_soname()))
}

/// Copies a native buffer and releases it. Used on both success and error
/// paths so a partially-filled output buffer never leaks.
pub(crate) unsafe fn take_buf(inner: &Inner, buf: sys::Buf) -> Vec<u8> {
    let out = if buf.data.is_null() || buf.len == 0 {
        Vec::new()
    } else {
        std::slice::from_raw_parts(buf.data, buf.len).to_vec()
    };
    let _ = (inner.api.buf_free)(buf);
    out
}

/// Copies `capsula_last_error` immediately (it is thread-local and valid only
/// until the next SDK call on this thread).
pub(crate) fn status_error(inner: &Inner, status: i32, context: &str) -> Error {
    let detail = unsafe {
        let ptr = (inner.api.last_error)();
        if ptr.is_null() {
            String::new()
        } else {
            CStr::from_ptr(ptr).to_string_lossy().into_owned()
        }
    };
    let message = if detail.is_empty() {
        context.to_string()
    } else {
        format!("{context}: {detail}")
    };
    Error::from_status(status, message)
}

pub(crate) fn check_status(inner: &Inner, status: i32, context: &str) -> Result<()> {
    if status == sys::STATUS_OK {
        Ok(())
    } else {
        Err(status_error(inner, status, context))
    }
}

/// `timeout_ms` for open/start calls: `None` and zero mean the native default
/// (or no deadline for calls).
pub(crate) fn timeout_ms(timeout: Option<Duration>) -> u32 {
    timeout
        .map(|d| d.as_millis().min(u32::MAX as u128) as u32)
        .unwrap_or(0)
}

/// Timeout for blocking receives: `None` means wait forever.
pub(crate) fn wait_ms(timeout: Option<Duration>) -> u32 {
    match timeout {
        None => sys::WAIT_FOREVER,
        Some(d) => d.as_millis().min(u32::MAX as u128) as u32,
    }
}

pub(crate) fn os_bytes(value: &OsStr) -> &[u8] {
    value.as_bytes()
}

pub(crate) fn str_bytes(value: &str) -> &[u8] {
    value.as_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn soname_matches_platform_contract() {
        #[cfg(target_os = "linux")]
        assert_eq!(default_soname(), "libcapsula.so.1");
        #[cfg(target_os = "macos")]
        assert_eq!(default_soname(), "libcapsula.1.dylib");
    }

    #[test]
    fn timeout_conversion_is_bounded() {
        assert_eq!(timeout_ms(None), 0);
        assert_eq!(timeout_ms(Some(Duration::from_millis(1500))), 1500);
        assert_eq!(timeout_ms(Some(Duration::from_secs(u64::MAX))), u32::MAX);
        assert_eq!(wait_ms(None), sys::WAIT_FOREVER);
        assert_eq!(wait_ms(Some(Duration::ZERO)), 0);
    }

    #[test]
    fn missing_library_is_an_error() {
        let err = match Library::open("/nonexistent/capsula/libcapsula.so.1") {
            Ok(_) => panic!("unexpectedly loaded a missing library"),
            Err(err) => err,
        };
        assert_eq!(err.status(), error::CODE_LOAD);
    }
}
