//! Bound HTTP/1.1 client on the shared engine.
//!
//! Verification is always on; the request body is returned exactly as sent
//! (no decompression). Headers are a caller-supplied string map.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{Map, Value};

use crate::error::{Error, Result};
use crate::sys::{self, Buf};
use crate::{positive_ms, status_error, take_buf, Inner, Library};

/// One request. `Timeout` bounds the whole exchange (all redirect hops); the
/// native per-connect/per-read timeouts still apply. `None` uses the client's
/// `HttpOptions::total_timeout` (native default 120 s). A set value is rounded
/// up to at least 1 ms, so a sub-millisecond (including zero) budget becomes a
/// 1 ms deadline instead of collapsing into the default.
#[derive(Debug, Clone, Default)]
pub struct HttpRequest {
    pub method: String,
    pub url: String,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
    pub timeout: Option<Duration>,
}

/// A completed exchange. Non-2xx statuses are successful results.
#[derive(Debug, Clone)]
pub struct HttpResponse {
    pub status_code: i32,
    pub headers: BTreeMap<String, String>,
    pub body: Vec<u8>,
}

/// Verification and bound options for [`HttpClient::open`].
#[derive(Debug, Clone, Default)]
pub struct HttpOptions {
    /// `"system"` (default), `"webpki"`, or `"pem"` (only `ca_pem` anchors are
    /// trusted: no system bundle and no public roots).
    pub roots: Option<String>,
    /// Additional PEM-encoded trust anchors.
    pub ca_pem: Option<Vec<u8>>,
    pub user_agent: Option<String>,
    /// Response body cap; `None` uses 32 MiB (hard cap 64 MiB).
    pub max_body: Option<u64>,
    /// Redirect budget; `Some(0)` disables redirects.
    pub follow_redirects: Option<u32>,
    pub connect_timeout: Option<Duration>,
    pub read_timeout: Option<Duration>,
    /// Whole-request budget covering every redirect hop. `None` uses the
    /// native default (120 s); `Some(Duration::ZERO)` also selects that
    /// default (the native option spells it `0`, never "unlimited").
    pub total_timeout: Option<Duration>,
}

impl HttpOptions {
    fn encode(&self) -> Result<String> {
        let mut map = Map::new();
        if let Some(roots) = &self.roots {
            map.insert("roots".into(), Value::String(roots.clone()));
        }
        if let Some(pem) = &self.ca_pem {
            let text = String::from_utf8(pem.clone()).map_err(|_| {
                Error::local(
                    crate::error::CODE_INVALID_ARGUMENT,
                    "ca_pem must be UTF-8 PEM text",
                )
            })?;
            map.insert("ca_pem".into(), Value::String(text));
        }
        if let Some(agent) = &self.user_agent {
            map.insert("user_agent".into(), Value::String(agent.clone()));
        }
        if let Some(max) = self.max_body {
            map.insert("max_body".into(), Value::from(max));
        }
        if let Some(count) = self.follow_redirects {
            map.insert("follow_redirects".into(), Value::from(count));
        }
        if let Some(timeout) = self.connect_timeout {
            map.insert(
                "connect_timeout_ms".into(),
                Value::from(positive_ms(timeout)?),
            );
        }
        if let Some(timeout) = self.read_timeout {
            map.insert("read_timeout_ms".into(), Value::from(positive_ms(timeout)?));
        }
        if let Some(timeout) = self.total_timeout {
            // 0 is the native spelling of "use the 120 s default"; a positive
            // sub-millisecond value must not collapse into it.
            let value = if timeout.is_zero() {
                0
            } else {
                positive_ms(timeout)?
            };
            map.insert("total_timeout_ms".into(), Value::from(value));
        }
        Ok(Value::Object(map).to_string())
    }
}

/// Encodes the per-request deadline: `None` keeps the client's total budget
/// (`0` on the wire), while a set value is rounded up to at least 1 ms so a
/// sub-millisecond budget never collapses into "no per-request timeout".
fn request_timeout_ms(timeout: Option<Duration>) -> u32 {
    match timeout {
        None => 0,
        Some(duration) => duration.as_millis().max(1).min(u32::MAX as u128) as u32,
    }
}

/// One native HTTP client handle. Dropping it closes the handle once: every
/// in-flight request is cancelled and, unless the drop runs on a Tokio runtime
/// thread, the native close waits until none can still touch the TLS/root
/// state. Inside a runtime it returns without blocking; requests hold `Arc`
/// snapshots, so that state stays valid and the handle is unusable either way.
pub struct HttpClient {
    pub(crate) inner: Arc<Inner>,
    handle: u64,
    next_id: AtomicU64,
}

impl HttpClient {
    /// Opens a client. TLS roots are read here; no network I/O happens.
    pub fn open(library: &Library, options: &HttpOptions) -> Result<Self> {
        let encoded = options.encode()?;
        let mut handle = 0u64;
        let status = unsafe {
            (library.inner.api.http_open)(encoded.as_ptr(), encoded.len(), 0, &mut handle)
        };
        crate::check_status(&library.inner, status, "http_open")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "http_open returned a zero handle",
            ));
        }
        Ok(Self {
            inner: library.inner.clone(),
            handle,
            next_id: AtomicU64::new(1),
        })
    }

    pub fn handle(&self) -> u64 {
        self.handle
    }

    /// Aborts the in-flight request with `request_id`. Other requests are
    /// unaffected. Cancellation is sticky across the registration race: an id
    /// whose request has not registered yet is remembered natively (bounded to
    /// 64 ids, oldest evicted) and that request fails with `CANCELLED` before
    /// any network I/O. An id that already finished is a successful no-op.
    pub fn cancel(&self, request_id: u64) -> Result<()> {
        let status = unsafe { (self.inner.api.http_cancel)(self.handle, request_id) };
        crate::check_status(&self.inner, status, "http_cancel")
    }

    /// Performs one request with a binding-chosen request id. Use
    /// [`HttpClient::request_with_id`] when another thread needs to cancel it.
    pub fn request(&self, request: &HttpRequest) -> Result<HttpResponse> {
        let request_id = self.next_id.fetch_add(1, Ordering::Relaxed);
        if request_id == 0 {
            return Err(Error::local(
                crate::error::CODE_INVALID_ARGUMENT,
                "http request id space exhausted",
            ));
        }
        self.request_with_id(request_id, request)
    }

    /// Performs one request whose id is caller-chosen. `request_id` must be
    /// non-zero and unique for the lifetime of the client: an id is never
    /// reused, even after its request returned. [`HttpClient::cancel`] with the
    /// same id aborts just this exchange.
    pub fn request_with_id(&self, request_id: u64, request: &HttpRequest) -> Result<HttpResponse> {
        if request_id == 0 {
            return Err(Error::local(
                crate::error::CODE_INVALID_ARGUMENT,
                "http request id must not be zero",
            ));
        }
        let method = request.method.as_bytes();
        let url = request.url.as_bytes();
        let mut header_map = Map::new();
        for (name, value) in &request.headers {
            header_map.insert(name.clone(), Value::String(value.clone()));
        }
        let headers = Value::Object(header_map).to_string();
        let body = &request.body;
        let mut status_code = 0i32;
        let mut out_headers = Buf::default();
        let mut out_body = Buf::default();
        let status = unsafe {
            (self.inner.api.http_request)(
                self.handle,
                request_id,
                method.as_ptr().cast(),
                method.len(),
                url.as_ptr().cast(),
                url.len(),
                headers.as_ptr(),
                headers.len(),
                body.as_ptr(),
                body.len(),
                request_timeout_ms(request.timeout),
                &mut status_code,
                &mut out_headers,
                &mut out_body,
            )
        };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "http_request");
            unsafe {
                take_buf(&self.inner, out_headers);
                take_buf(&self.inner, out_body);
            }
            return Err(err);
        }
        let header_bytes = unsafe { take_buf(&self.inner, out_headers) };
        let body = unsafe { take_buf(&self.inner, out_body) };
        let parsed: BTreeMap<String, String> = serde_json::from_slice(&header_bytes)
            .map_err(|err| Error::local(crate::error::CODE_INVALID_ARGUMENT, err.to_string()))?;
        Ok(HttpResponse {
            status_code,
            headers: parsed,
            body,
        })
    }
}

impl Drop for HttpClient {
    fn drop(&mut self) {
        if self.handle == 0 {
            return;
        }
        unsafe {
            (self.inner.api.http_close)(self.handle);
        }
        self.handle = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn encoded(options: &HttpOptions) -> Value {
        serde_json::from_str(&options.encode().expect("options encode")).expect("valid JSON")
    }

    #[test]
    fn sub_millisecond_timeouts_never_encode_as_zero() {
        let value = encoded(&HttpOptions {
            connect_timeout: Some(Duration::from_micros(100)),
            read_timeout: Some(Duration::from_nanos(1)),
            total_timeout: Some(Duration::from_micros(500)),
            ..HttpOptions::default()
        });
        assert_eq!(value["connect_timeout_ms"], 1);
        assert_eq!(value["read_timeout_ms"], 1);
        assert_eq!(value["total_timeout_ms"], 1);
    }

    #[test]
    fn zero_total_timeout_selects_the_native_default() {
        let value = encoded(&HttpOptions {
            total_timeout: Some(Duration::ZERO),
            ..HttpOptions::default()
        });
        assert_eq!(value["total_timeout_ms"], 0);
        let value = encoded(&HttpOptions {
            total_timeout: Some(Duration::from_secs(2)),
            ..HttpOptions::default()
        });
        assert_eq!(value["total_timeout_ms"], 2_000);
    }

    #[test]
    fn zero_connect_and_read_timeouts_are_rejected_locally() {
        assert!(HttpOptions {
            connect_timeout: Some(Duration::ZERO),
            ..HttpOptions::default()
        }
        .encode()
        .is_err());
        assert!(HttpOptions {
            read_timeout: Some(Duration::ZERO),
            ..HttpOptions::default()
        }
        .encode()
        .is_err());
    }

    #[test]
    fn request_timeouts_round_up_and_none_uses_the_default() {
        assert_eq!(request_timeout_ms(None), 0);
        assert_eq!(request_timeout_ms(Some(Duration::ZERO)), 1);
        assert_eq!(request_timeout_ms(Some(Duration::from_nanos(1))), 1);
        assert_eq!(request_timeout_ms(Some(Duration::from_micros(999))), 1);
        assert_eq!(request_timeout_ms(Some(Duration::from_millis(1))), 1);
        assert_eq!(request_timeout_ms(Some(Duration::from_secs(2))), 2_000);
        assert_eq!(
            request_timeout_ms(Some(Duration::from_secs(u64::MAX))),
            u32::MAX
        );
    }
}
