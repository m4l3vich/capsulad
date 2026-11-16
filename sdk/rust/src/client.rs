//! Client handle and unary/stream entry points.

use std::ffi::OsStr;
use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;

use crate::bus::{Bus, BusHandler};
use crate::call::{Call, CallKind};
use crate::error::{self, Error, Result};
use crate::grpc::RemoteOptions;
use crate::sys::{self, Buf};
use crate::{
    check_status, os_bytes, status_error, str_bytes, take_buf, timeout_ms, Inner, Library,
};

/// One UDS gRPC channel to `capsulad`, authenticated by the launcher-assigned
/// run ID.
pub struct Client {
    pub(crate) inner: Arc<Inner>,
    handle: u64,
    run_id: String,
    app_id: Option<String>,
}

impl Client {
    /// Opens a client. `run_id` is mandatory and is sent as the
    /// `capsulad-run-id` metadata on every RPC. `timeout` of `None` (or zero)
    /// uses the native 10-second connect default.
    pub fn connect(
        library: &Library,
        socket: impl AsRef<OsStr>,
        run_id: &str,
        app_id: Option<&str>,
        timeout: Option<Duration>,
    ) -> Result<Self> {
        if run_id.is_empty() {
            return Err(Error::local(
                error::CODE_INVALID_ARGUMENT,
                "run_id is required",
            ));
        }
        let socket = socket.as_ref();
        let socket = os_bytes(socket);
        let app_id_bytes = app_id.map(str_bytes).unwrap_or(&[]);
        let mut handle = 0u64;
        let status = unsafe {
            (library.inner.api.client_connect)(
                socket.as_ptr().cast(),
                socket.len(),
                run_id.as_ptr().cast(),
                run_id.len(),
                app_id_bytes.as_ptr().cast(),
                app_id_bytes.len(),
                timeout_ms(timeout),
                &mut handle,
            )
        };
        check_status(&library.inner, status, "client_connect")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "client_connect returned a zero handle",
            ));
        }
        Ok(Self {
            inner: library.inner.clone(),
            handle,
            run_id: run_id.to_string(),
            app_id: app_id.map(|value| value.to_string()),
        })
    }

    /// Uses `CAPSULAD_SOCKET`, `CAPSULAD_RUN_ID` and `CAPSULAD_APP_ID` from the
    /// supervised environment.
    pub fn from_env(library: &Library) -> Result<Self> {
        let socket = std::env::var_os("CAPSULAD_SOCKET")
            .filter(|value| !value.is_empty())
            .unwrap_or_else(|| std::ffi::OsString::from(sys::DEFAULT_SOCKET));
        let run_id = std::env::var("CAPSULAD_RUN_ID").map_err(|_| {
            Error::local(
                error::CODE_INVALID_ARGUMENT,
                "CAPSULAD_RUN_ID is not set; start the app through capsulad",
            )
        })?;
        let app_id = std::env::var("CAPSULAD_APP_ID").ok();
        Self::connect(library, socket, &run_id, app_id.as_deref(), None)
    }

    pub fn handle(&self) -> u64 {
        self.handle
    }

    pub fn run_id(&self) -> &str {
        &self.run_id
    }

    pub fn app_id(&self) -> Option<&str> {
        self.app_id.as_deref()
    }

    /// Opens a remote gRPC channel to an origin (`https://host[:port]` or
    /// `http://host[:port]`). https verifies TLS with ALPN h2; http uses h2c
    /// prior knowledge. The returned client has no run id and no app bus.
    pub fn open_remote(
        library: &Library,
        url: &str,
        options: &RemoteOptions,
        timeout: Option<Duration>,
    ) -> Result<Self> {
        let encoded = options.encode()?;
        let mut handle = 0u64;
        let status = unsafe {
            (library.inner.api.grpc_open)(
                url.as_ptr().cast(),
                url.len(),
                encoded.as_ptr(),
                encoded.len(),
                timeout_ms(timeout),
                &mut handle,
            )
        };
        check_status(&library.inner, status, "grpc_open")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "grpc_open returned a zero handle",
            ));
        }
        Ok(Self {
            inner: library.inner.clone(),
            handle,
            run_id: String::new(),
            app_id: None,
        })
    }

    /// Starts a call with caller-supplied gRPC metadata. Names are lowercased
    /// and validated by the native library; invalid metadata is
    /// `INVALID_ARGUMENT`.
    pub fn start_call_meta(
        &self,
        service: &str,
        method: &str,
        kind: CallKind,
        headers: &[(&str, &str)],
        timeout: Option<Duration>,
    ) -> Result<Call> {
        let service = str_bytes(service);
        let method = str_bytes(method);
        let mut map = serde_json::Map::new();
        for (name, value) in headers {
            map.insert(
                (*name).to_string(),
                serde_json::Value::String((*value).to_string()),
            );
        }
        let encoded = serde_json::Value::Object(map).to_string();
        let mut handle = 0u64;
        let status = unsafe {
            (self.inner.api.call_start_meta)(
                self.handle,
                service.as_ptr().cast(),
                service.len(),
                method.as_ptr().cast(),
                method.len(),
                kind as i32,
                timeout_ms(timeout),
                encoded.as_ptr(),
                encoded.len(),
                &mut handle,
            )
        };
        check_status(&self.inner, status, "call_start_meta")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "call_start_meta returned a zero handle",
            ));
        }
        Ok(Call::from_handle(self.inner.clone(), handle))
    }

    /// Registers a serialized `google.protobuf.FileDescriptorSet` for custom
    /// JSON contracts. Framework descriptors are already embedded.
    pub fn register_descriptors(&self, file_descriptor_set: &[u8]) -> Result<()> {
        let status = unsafe {
            (self.inner.api.client_register_descriptors)(
                self.handle,
                file_descriptor_set.as_ptr(),
                file_descriptor_set.len(),
            )
        };
        check_status(&self.inner, status, "client_register_descriptors")
    }

    /// Client JSON information (`client_info`).
    pub fn info(&self) -> Result<Value> {
        let mut buf = Buf::default();
        let status = unsafe { (self.inner.api.client_info)(self.handle, &mut buf) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "client_info");
            unsafe { take_buf(&self.inner, buf) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, buf) };
        parse_json(&bytes, "client_info")
    }

    /// Unary JSON call using canonical protobuf JSON mapping.
    pub fn unary_json(
        &self,
        service: &str,
        method: &str,
        request_json: &[u8],
        timeout: Option<Duration>,
    ) -> Result<Vec<u8>> {
        self.unary(service, method, request_json, timeout, false)
    }

    /// Unary call with a serialized protobuf request and response.
    pub fn unary_proto(
        &self,
        service: &str,
        method: &str,
        request_proto: &[u8],
        timeout: Option<Duration>,
    ) -> Result<Vec<u8>> {
        self.unary(service, method, request_proto, timeout, true)
    }

    fn unary(
        &self,
        service: &str,
        method: &str,
        request: &[u8],
        timeout: Option<Duration>,
        proto: bool,
    ) -> Result<Vec<u8>> {
        let service = str_bytes(service);
        let method = str_bytes(method);
        let mut out = Buf::default();
        let status = unsafe {
            let f = if proto {
                self.inner.api.client_unary_proto
            } else {
                self.inner.api.client_unary
            };
            f(
                self.handle,
                service.as_ptr().cast(),
                service.len(),
                method.as_ptr().cast(),
                method.len(),
                request.as_ptr(),
                request.len(),
                timeout_ms(timeout),
                &mut out,
            )
        };
        if status != sys::STATUS_OK {
            let err = status_error(
                &self.inner,
                status,
                if proto {
                    "client_unary_proto"
                } else {
                    "client_unary"
                },
            );
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        Ok(unsafe { take_buf(&self.inner, out) })
    }

    /// Starts a direct framework call. The wire path is
    /// `/<service>/<method>` for every kind.
    pub fn start_call(
        &self,
        service: &str,
        method: &str,
        kind: CallKind,
        timeout: Option<Duration>,
    ) -> Result<Call> {
        let service = str_bytes(service);
        let method = str_bytes(method);
        let mut handle = 0u64;
        let status = unsafe {
            (self.inner.api.call_start)(
                self.handle,
                service.as_ptr().cast(),
                service.len(),
                method.as_ptr().cast(),
                method.len(),
                kind as i32,
                timeout_ms(timeout),
                &mut handle,
            )
        };
        check_status(&self.inner, status, "call_start")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "call_start returned a zero handle",
            ));
        }
        Ok(Call::from_handle(self.inner.clone(), handle))
    }

    /// Starts a routed call through `AppBusService/Call` to another app.
    pub fn start_routed(
        &self,
        target_app_id: &str,
        service: &str,
        method: &str,
        timeout: Option<Duration>,
    ) -> Result<Call> {
        let target = str_bytes(target_app_id);
        let service = str_bytes(service);
        let method = str_bytes(method);
        let mut handle = 0u64;
        let status = unsafe {
            (self.inner.api.call_start_routed)(
                self.handle,
                target.as_ptr().cast(),
                target.len(),
                service.as_ptr().cast(),
                service.len(),
                method.as_ptr().cast(),
                method.len(),
                timeout_ms(timeout),
                &mut handle,
            )
        };
        check_status(&self.inner, status, "call_start_routed")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "call_start_routed returned a zero handle",
            ));
        }
        Ok(Call::from_handle(self.inner.clone(), handle))
    }

    /// Opens the managed AppBus session with the supplied callbacks.
    pub fn bus(&self, handler: Arc<dyn BusHandler>) -> Result<Bus> {
        Bus::open(self, handler)
    }
}

impl Drop for Client {
    fn drop(&mut self) {
        if self.handle == 0 {
            return;
        }
        let handle = self.handle;
        self.handle = 0;
        let status = unsafe { (self.inner.api.client_close)(handle) };
        if status != sys::STATUS_BUSY {
            return;
        }
        // BUSY means this drop ran on one of the client's callback threads.
        // Defer the close to a non-callback thread instead of leaking the
        // client or freeing callback state while the bus is still active.
        let inner = self.inner.clone();
        std::thread::spawn(move || loop {
            let status = unsafe { (inner.api.client_close)(handle) };
            if status != sys::STATUS_BUSY {
                break;
            }
            std::thread::sleep(Duration::from_millis(10));
        });
    }
}

pub(crate) fn parse_json(bytes: &[u8], context: &str) -> Result<Value> {
    serde_json::from_slice(bytes).map_err(|err| {
        Error::local(
            error::CODE_INVALID_ARGUMENT,
            format!("{context}: library returned invalid JSON: {err}"),
        )
    })
}
