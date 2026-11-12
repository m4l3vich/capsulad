//! Managed AppBus session and callback dispatch.

use std::ffi::{c_void, CStr};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;

use crate::call::{Call, InboundCall};
use crate::client::parse_json;
use crate::error::{Error, Result};
use crate::sys::{self, Buf, BusCallbacksV1};
use crate::{status_error, take_buf, wait_ms, Client, Inner};

/// AppBus lifecycle event kinds delivered to [`BusHandler::on_event`].
#[repr(i32)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BusEventKind {
    Unknown = 0,
    Invocation = 1,
    SettingsChanged = 2,
    Action = 3,
    Activation = 4,
    TouchReleased = 5,
}

impl BusEventKind {
    pub fn from_raw(value: i32) -> Self {
        match value {
            sys::EVENT_INVOCATION => Self::Invocation,
            sys::EVENT_SETTINGS_CHANGED => Self::SettingsChanged,
            sys::EVENT_ACTION => Self::Action,
            sys::EVENT_ACTIVATION => Self::Activation,
            sys::EVENT_TOUCH_RELEASED => Self::TouchReleased,
            _ => Self::Unknown,
        }
    }
}

/// One event from `on_event`. `json` is canonical protobuf JSON for the
/// message named by `kind`.
#[derive(Clone, Debug)]
pub struct BusEvent {
    pub kind: BusEventKind,
    pub raw_kind: i32,
    pub json: Vec<u8>,
}

impl BusEvent {
    pub fn value(&self) -> Option<Value> {
        serde_json::from_slice(&self.json).ok()
    }
}

/// Metadata for an inbound routed call (`on_call`).
#[derive(Clone, Debug)]
pub struct CallInfo {
    pub call_id: String,
    pub caller_run_id: String,
    pub service: String,
    pub method: String,
    pub deadline_monotonic_ns: u64,
    pub raw: Vec<u8>,
}

impl CallInfo {
    fn from_bytes(bytes: Vec<u8>) -> Self {
        let value = serde_json::from_slice::<Value>(&bytes).ok();
        let get = |key: &str| -> String {
            value
                .as_ref()
                .and_then(|v| v.get(key))
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string()
        };
        let deadline = value
            .as_ref()
            .and_then(|v| v.get("deadline_monotonic_ns"))
            .and_then(|v| match v {
                Value::String(text) => text.parse::<u64>().ok(),
                Value::Number(number) => number.as_u64(),
                _ => None,
            })
            .unwrap_or(0);
        Self {
            call_id: get("call_id"),
            caller_run_id: get("caller_run_id"),
            service: get("grpc_service"),
            method: get("rpc_name"),
            deadline_monotonic_ns: deadline,
            raw: bytes,
        }
    }
}

/// Application callbacks. Methods run on library threads and must not panic
/// across the FFI boundary; the binding catches panics, but state may be
/// partially updated.
///
/// `on_call` receives a borrowed [`InboundCall`]. The library owns the
/// callback reference: do not release it. Call `call.end(0, "")` on success;
/// returning without ending the call makes the daemon observe INTERNAL.
pub trait BusHandler: Send + Sync + 'static {
    fn on_ready(&self) {}
    fn on_disconnected(&self, _status: i32, _detail: &str) {}
    fn on_event(&self, _event: BusEvent) {}
    fn on_call(&self, call: InboundCall<'_>, info: CallInfo);
    fn on_log(&self, _level: i32, _message: &str) {}
}

struct HandlerSlot {
    handler: Arc<dyn BusHandler>,
    inner: Arc<Inner>,
}

unsafe fn slot<'a>(user_data: *mut c_void) -> &'a HandlerSlot {
    &*(user_data as *const HandlerSlot)
}

unsafe extern "C" fn on_ready_cb(_bus: u64, user_data: *mut c_void) {
    if user_data.is_null() {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| {
        let slot = slot(user_data);
        slot.handler.on_ready();
    }));
}

unsafe extern "C" fn on_disconnected_cb(
    _bus: u64,
    status: i32,
    detail: *const std::ffi::c_char,
    user_data: *mut c_void,
) {
    if user_data.is_null() {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| {
        let detail = if detail.is_null() {
            String::new()
        } else {
            CStr::from_ptr(detail).to_string_lossy().into_owned()
        };
        let slot = slot(user_data);
        slot.handler.on_disconnected(status, &detail);
    }));
}

unsafe extern "C" fn on_event_cb(
    _bus: u64,
    kind: i32,
    json: *const u8,
    len: usize,
    user_data: *mut c_void,
) {
    if user_data.is_null() {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| {
        let json = if json.is_null() || len == 0 {
            Vec::new()
        } else {
            std::slice::from_raw_parts(json, len).to_vec()
        };
        let event = BusEvent {
            kind: BusEventKind::from_raw(kind),
            raw_kind: kind,
            json,
        };
        let slot = slot(user_data);
        slot.handler.on_event(event);
    }));
}

unsafe extern "C" fn on_call_cb(
    _bus: u64,
    call_handle: u64,
    info_json: *const u8,
    info_len: usize,
    user_data: *mut c_void,
) {
    if user_data.is_null() || call_handle == 0 {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| {
        let raw = if info_json.is_null() || info_len == 0 {
            Vec::new()
        } else {
            std::slice::from_raw_parts(info_json, info_len).to_vec()
        };
        let info = CallInfo::from_bytes(raw);
        let slot = slot(user_data);
        let inner = slot.handler_inner();
        // The library owns the callback reference; this borrowed handle must
        // not release it. The library drops it when the callback returns.
        let borrowed = Call::borrowed(inner, call_handle);
        let inbound = InboundCall::new(&borrowed);
        slot.handler.on_call(inbound, info);
    }));
}

unsafe extern "C" fn on_log_cb(
    level: i32,
    message: *const std::ffi::c_char,
    user_data: *mut c_void,
) {
    if user_data.is_null() {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| {
        let message = if message.is_null() {
            String::new()
        } else {
            CStr::from_ptr(message).to_string_lossy().into_owned()
        };
        let slot = slot(user_data);
        slot.handler.on_log(level, &message);
    }));
}

impl HandlerSlot {
    fn handler_inner(&self) -> Arc<Inner> {
        self.inner.clone()
    }
}

/// One managed `AppBusService/Connect` session.
pub struct Bus {
    inner: Arc<Inner>,
    handle: u64,
    slot: *mut HandlerSlot,
}

unsafe impl Send for Bus {}
unsafe impl Sync for Bus {}

impl Bus {
    pub(crate) fn open(client: &Client, handler: Arc<dyn BusHandler>) -> Result<Self> {
        let slot = Box::into_raw(Box::new(HandlerSlot {
            handler,
            inner: client.inner.clone(),
        }));
        let callbacks = BusCallbacksV1::new(
            slot as *mut c_void,
            Some(on_ready_cb),
            Some(on_disconnected_cb),
            Some(on_event_cb),
            Some(on_call_cb),
            Some(on_log_cb),
        );
        let mut handle = 0u64;
        let status =
            unsafe { (client.inner.api.bus_open)(client.handle(), &callbacks, &mut handle) };
        if status != sys::STATUS_OK {
            let err = status_error(&client.inner, status, "bus_open");
            unsafe { drop(Box::from_raw(slot)) };
            return Err(err);
        }
        if handle == 0 {
            unsafe { drop(Box::from_raw(slot)) };
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "bus_open returned a zero handle",
            ));
        }
        Ok(Self {
            inner: client.inner.clone(),
            handle,
            slot,
        })
    }

    pub fn handle(&self) -> u64 {
        self.handle
    }

    /// Waits until the Connect stream and the Hello/Ready frames are handed to
    /// the transport. `None` waits forever.
    pub fn ready(&self, timeout: Option<Duration>) -> Result<()> {
        let status = unsafe { (self.inner.api.bus_ready)(self.handle, wait_ms(timeout)) };
        crate::check_status(&self.inner, status, "bus_ready")
    }

    /// Reports a launch invocation outcome.
    pub fn notify_invocation(
        &self,
        invocation_id: &str,
        accepted: bool,
        detail: &str,
    ) -> Result<()> {
        let status = unsafe {
            (self.inner.api.bus_notify_invocation)(
                self.handle,
                invocation_id.as_ptr().cast(),
                invocation_id.len(),
                accepted as i32,
                detail.as_ptr().cast(),
                detail.len(),
            )
        };
        crate::check_status(&self.inner, status, "bus_notify_invocation")
    }

    /// Reports an action button outcome.
    pub fn notify_action(
        &self,
        action_invocation_id: &str,
        succeeded: bool,
        detail: &str,
    ) -> Result<()> {
        let status = unsafe {
            (self.inner.api.bus_notify_action)(
                self.handle,
                action_invocation_id.as_ptr().cast(),
                action_invocation_id.len(),
                succeeded as i32,
                detail.as_ptr().cast(),
                detail.len(),
            )
        };
        crate::check_status(&self.inner, status, "bus_notify_action")
    }

    /// Bus metadata JSON (`handle_info`).
    pub fn info(&self) -> Result<Value> {
        let mut buf = Buf::default();
        let status = unsafe { (self.inner.api.handle_info)(self.handle, &mut buf) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "handle_info");
            unsafe { take_buf(&self.inner, buf) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, buf) };
        parse_json(&bytes, "handle_info")
    }

    /// Intentionally closes the session. Idempotent. May return `BUSY` when
    /// called from one of this bus's callback threads.
    pub fn close(&mut self) -> Result<()> {
        if self.handle == 0 {
            return Ok(());
        }
        let status = unsafe { (self.inner.api.bus_close)(self.handle) };
        if status == sys::STATUS_OK
            || status == sys::STATUS_CLOSED
            || status == sys::STATUS_NOT_FOUND
        {
            self.handle = 0;
            self.free_slot();
            return Ok(());
        }
        Err(status_error(&self.inner, status, "bus_close"))
    }
}

impl Drop for Bus {
    fn drop(&mut self) {
        let handle = self.handle;
        self.handle = 0;
        if handle != 0 {
            let status = unsafe { (self.inner.api.bus_close)(handle) };
            if status == sys::STATUS_OK
                || status == sys::STATUS_CLOSED
                || status == sys::STATUS_NOT_FOUND
            {
                self.free_slot();
                return;
            }
            if status != sys::STATUS_BUSY {
                // Unknown failure: keep the callback slot alive rather than
                // freeing user data that native code might still reference.
                self.slot = std::ptr::null_mut();
                return;
            }
            // BUSY means this drop ran on one of the bus's callback threads.
            // Move both the close and the user-data release to a non-callback
            // thread; the slot stays alive until bus_close joins callbacks.
            let inner = self.inner.clone();
            let slot = self.take_slot();
            std::thread::spawn(move || {
                loop {
                    let status = unsafe { (inner.api.bus_close)(handle) };
                    if status != sys::STATUS_BUSY {
                        break;
                    }
                    std::thread::sleep(Duration::from_millis(10));
                }
                drop(slot);
            });
            return;
        }
        self.free_slot();
    }
}

impl Bus {
    fn take_slot(&mut self) -> Option<Box<HandlerSlot>> {
        if self.slot.is_null() {
            None
        } else {
            let slot = unsafe { Box::from_raw(self.slot) };
            self.slot = std::ptr::null_mut();
            Some(slot)
        }
    }

    fn free_slot(&mut self) {
        if !self.slot.is_null() {
            unsafe { drop(Box::from_raw(self.slot)) };
            self.slot = std::ptr::null_mut();
        }
    }
}
