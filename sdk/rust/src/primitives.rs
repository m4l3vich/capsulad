//! Encoding, hashing and URL primitives on the shared engine.
//!
//! Thin wrappers: the native library owns the algorithms and the bounded
//! buffers, this module copies the result and frees it exactly once.

use crate::error::Result;
use crate::sys::{self, Buf};
use crate::{status_error, take_buf, Library};

/// Base64 alphabets and padding flags.
pub const BASE64_STANDARD: i32 = 0;
pub const BASE64_URL_SAFE: i32 = 1;
pub const BASE64_NO_PADDING: i32 = 2;

/// One parsed absolute http(s) URL. `port` is the effective TCP port.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Url {
    pub scheme: String,
    pub host: String,
    pub port: u16,
    pub path: String,
    pub query: String,
}

impl Library {
    pub fn base64_encode(&self, data: &[u8], flags: i32) -> Result<Vec<u8>> {
        let mut out = Buf::default();
        let status =
            unsafe { (self.inner.api.base64_encode)(data.as_ptr(), data.len(), flags, &mut out) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "base64_encode");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        Ok(unsafe { take_buf(&self.inner, out) })
    }

    pub fn base64_decode(&self, data: &[u8], flags: i32) -> Result<Vec<u8>> {
        let mut out = Buf::default();
        let status =
            unsafe { (self.inner.api.base64_decode)(data.as_ptr(), data.len(), flags, &mut out) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "base64_decode");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        Ok(unsafe { take_buf(&self.inner, out) })
    }

    pub fn percent_encode(&self, data: &[u8]) -> Result<Vec<u8>> {
        let mut out = Buf::default();
        let status =
            unsafe { (self.inner.api.percent_encode)(data.as_ptr(), data.len(), &mut out) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "percent_encode");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        Ok(unsafe { take_buf(&self.inner, out) })
    }

    pub fn percent_decode(&self, data: &[u8]) -> Result<Vec<u8>> {
        let mut out = Buf::default();
        let status =
            unsafe { (self.inner.api.percent_decode)(data.as_ptr(), data.len(), &mut out) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "percent_decode");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        Ok(unsafe { take_buf(&self.inner, out) })
    }

    pub fn sha256(&self, data: &[u8]) -> Result<Vec<u8>> {
        let mut out = Buf::default();
        let status = unsafe { (self.inner.api.sha256)(data.as_ptr(), data.len(), &mut out) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "sha256");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        Ok(unsafe { take_buf(&self.inner, out) })
    }

    pub fn random_bytes(&self, len: usize) -> Result<Vec<u8>> {
        let mut out = vec![0u8; len];
        if len == 0 {
            return Ok(out);
        }
        let status = unsafe { (self.inner.api.random_bytes)(out.as_mut_ptr(), out.len()) };
        if status != sys::STATUS_OK {
            return Err(status_error(&self.inner, status, "random_bytes"));
        }
        Ok(out)
    }

    pub fn uuid_v4(&self) -> Result<String> {
        let mut out = Buf::default();
        let status = unsafe { (self.inner.api.uuid_v4)(&mut out) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "uuid_v4");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, out) };
        String::from_utf8(bytes).map_err(|err| {
            crate::Error::local(crate::error::CODE_INVALID_ARGUMENT, err.to_string())
        })
    }

    pub fn parse_url(&self, raw: &str) -> Result<Url> {
        let mut out = Buf::default();
        let status =
            unsafe { (self.inner.api.url_parse)(raw.as_ptr().cast(), raw.len(), &mut out) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "url_parse");
            unsafe { take_buf(&self.inner, out) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, out) };
        let value: serde_json::Value = serde_json::from_slice(&bytes).map_err(|err| {
            crate::Error::local(crate::error::CODE_INVALID_ARGUMENT, err.to_string())
        })?;
        let text = |key: &str| -> Result<String> {
            value
                .get(key)
                .and_then(|value| value.as_str())
                .map(str::to_string)
                .ok_or_else(|| {
                    crate::Error::local(
                        crate::error::CODE_INVALID_ARGUMENT,
                        format!("url_parse result is missing {key}"),
                    )
                })
        };
        Ok(Url {
            scheme: text("scheme")?,
            host: text("host")?,
            port: value
                .get("port")
                .and_then(|value| value.as_u64())
                .filter(|port| *port <= u16::MAX as u64)
                .ok_or_else(|| {
                    crate::Error::local(
                        crate::error::CODE_INVALID_ARGUMENT,
                        "url_parse result has an invalid port",
                    )
                })? as u16,
            path: text("path")?,
            query: text("query")?,
        })
    }
}
