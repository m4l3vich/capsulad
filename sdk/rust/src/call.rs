//! Call handle: unary, server/client streaming and bidirectional calls.

use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;

use crate::client::parse_json;
use crate::error::{Error, Result};
use crate::sys::{self, Buf};
use crate::{status_error, take_buf, wait_ms, Inner};

/// Declared gRPC call shape. The transport path is identical; the kind is
/// recorded in `call_info` and used by convenience helpers.
#[repr(i32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CallKind {
    Unary = 0,
    ServerStream = 1,
    ClientStream = 2,
    Bidi = 3,
}

/// Final or in-progress status of a call. `grpc_code == -1` while active.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CallStatus {
    pub grpc_code: i32,
    pub detail: String,
}

impl CallStatus {
    pub fn is_ok(&self) -> bool {
        self.grpc_code == 0
    }

    pub fn is_active(&self) -> bool {
        self.grpc_code < 0
    }
}

/// One gRPC call/stream. Dropping the last reference releases the native
/// handle; an unfinished outbound call is cancelled by that release.
pub struct Call {
    pub(crate) inner: Arc<Inner>,
    handle: u64,
    owns_ref: bool,
}

impl Call {
    pub(crate) fn from_handle(inner: Arc<Inner>, handle: u64) -> Self {
        Self {
            inner,
            handle,
            owns_ref: true,
        }
    }

    /// Borrowed view used inside `on_call`: the library owns the callback
    /// reference, so this value must not release it.
    pub(crate) fn borrowed(inner: Arc<Inner>, handle: u64) -> Self {
        Self {
            inner,
            handle,
            owns_ref: false,
        }
    }

    pub fn handle(&self) -> u64 {
        self.handle
    }

    /// Adds one native reference and returns an independently owned handle.
    /// Used inside `on_call` to keep working after the callback returns.
    pub fn retain(&self) -> Result<Self> {
        let status = unsafe { (self.inner.api.call_retain)(self.handle) };
        crate::check_status(&self.inner, status, "call_retain")?;
        Ok(Self {
            inner: self.inner.clone(),
            handle: self.handle,
            owns_ref: true,
        })
    }

    /// Sends one canonical protobuf-JSON request message.
    pub fn send_json(&self, json: &[u8]) -> Result<()> {
        let status =
            unsafe { (self.inner.api.call_send_json)(self.handle, json.as_ptr(), json.len()) };
        crate::check_status(&self.inner, status, "call_send_json")
    }

    /// Sends one serialized protobuf request message.
    pub fn send_proto(&self, proto: &[u8]) -> Result<()> {
        let status =
            unsafe { (self.inner.api.call_send_proto)(self.handle, proto.as_ptr(), proto.len()) };
        crate::check_status(&self.inner, status, "call_send_proto")
    }

    /// Closes the request direction.
    pub fn half_close(&self) -> Result<()> {
        let status = unsafe { (self.inner.api.call_half_close)(self.handle) };
        crate::check_status(&self.inner, status, "call_half_close")
    }

    /// Waits for one JSON response. `None` is a clean end of stream;
    /// `TIMEOUT` is returned as an error while the call remains open.
    pub fn recv_json(&self, timeout: Option<Duration>) -> Result<Option<Vec<u8>>> {
        self.recv(timeout, false)
    }

    /// Waits for one serialized protobuf response.
    pub fn recv_proto(&self, timeout: Option<Duration>) -> Result<Option<Vec<u8>>> {
        self.recv(timeout, true)
    }

    fn recv(&self, timeout: Option<Duration>, proto: bool) -> Result<Option<Vec<u8>>> {
        let mut out = Buf::default();
        let status = unsafe {
            let f = if proto {
                self.inner.api.call_recv_proto
            } else {
                self.inner.api.call_recv_json
            };
            f(self.handle, wait_ms(timeout), &mut out)
        };
        match status {
            sys::STATUS_OK => Ok(Some(unsafe { take_buf(&self.inner, out) })),
            sys::STATUS_END_OF_STREAM => {
                unsafe { take_buf(&self.inner, out) };
                Ok(None)
            }
            sys::STATUS_GRPC => {
                let err = self.grpc_error();
                unsafe { take_buf(&self.inner, out) };
                Err(err)
            }
            _ => {
                let err = status_error(
                    &self.inner,
                    status,
                    if proto {
                        "call_recv_proto"
                    } else {
                        "call_recv_json"
                    },
                );
                unsafe { take_buf(&self.inner, out) };
                Err(err)
            }
        }
    }

    /// Emits the final call frame. Inbound handlers must call this with
    /// `(0, "")` for success; returning without it ends with gRPC INTERNAL.
    pub fn end(&self, grpc_code: i32, detail: &str) -> Result<()> {
        let status = unsafe {
            (self.inner.api.call_end)(self.handle, grpc_code, detail.as_ptr().cast(), detail.len())
        };
        crate::check_status(&self.inner, status, "call_end")
    }

    /// Cancels the call. Safe to call from another thread while a receive is
    /// blocked; it unblocks the receive.
    pub fn cancel(&self) -> Result<()> {
        let status = unsafe { (self.inner.api.call_cancel)(self.handle) };
        crate::check_status(&self.inner, status, "call_cancel")
    }

    /// Call metadata JSON (`call_info`).
    pub fn info(&self) -> Result<Value> {
        let mut buf = Buf::default();
        let status = unsafe { (self.inner.api.call_info)(self.handle, &mut buf) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "call_info");
            unsafe { take_buf(&self.inner, buf) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, buf) };
        parse_json(&bytes, "call_info")
    }

    /// Final canonical gRPC status; `grpc_code == -1` while active.
    pub fn status(&self) -> Result<CallStatus> {
        let mut grpc_code = -1i32;
        let mut buf = Buf::default();
        let status = unsafe { (self.inner.api.call_status)(self.handle, &mut grpc_code, &mut buf) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "call_status");
            unsafe { take_buf(&self.inner, buf) };
            return Err(err);
        }
        let detail = unsafe { take_buf(&self.inner, buf) };
        Ok(CallStatus {
            grpc_code,
            detail: String::from_utf8_lossy(&detail).into_owned(),
        })
    }

    fn grpc_error(&self) -> Error {
        match self.status() {
            Ok(status) => Error::grpc(status.grpc_code, status.detail),
            Err(err) => err,
        }
    }
}

impl Drop for Call {
    fn drop(&mut self) {
        if self.handle != 0 && self.owns_ref {
            let _ = unsafe { (self.inner.api.call_release)(self.handle) };
            self.handle = 0;
        }
    }
}

/// Borrowed inbound call passed to [`crate::BusHandler::on_call`].
///
/// The library owns the callback reference: do not release it. Call
/// [`InboundCall::retain`] to obtain an independently owned [`Call`] for work
/// that outlives the callback. A successful handler must call
/// `call.end(0, "")`; returning without it ends with gRPC INTERNAL.
pub struct InboundCall<'a> {
    call: &'a Call,
}

impl<'a> InboundCall<'a> {
    pub(crate) fn new(call: &'a Call) -> Self {
        Self { call }
    }

    /// Retains the native handle for asynchronous work. The returned [`Call`]
    /// releases its reference when dropped.
    pub fn retain(&self) -> Result<Call> {
        self.call.retain()
    }
}

impl<'a> std::ops::Deref for InboundCall<'a> {
    type Target = Call;

    fn deref(&self) -> &Self::Target {
        self.call
    }
}
