//! Error type shared by every fallible operation.
//!
//! The shared library reports a `capsula_status`; gRPC failures additionally
//! carry the canonical gRPC code through `capsula_call_status`. Both are kept
//! so callers can branch on the transport outcome without parsing strings.

use std::fmt;

/// Failure inside the Rust binding before the native library was reached.
pub const CODE_LOAD: i32 = -1;
pub const CODE_ABI: i32 = -2;
pub const CODE_INVALID_ARGUMENT: i32 = -5;

/// An error returned by the Capsula shared engine or by this binding.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Error {
    status: i32,
    grpc_code: i32,
    message: String,
}

impl Error {
    /// Error carrying a native `capsula_status` and no gRPC code.
    pub fn from_status(status: i32, message: impl Into<String>) -> Self {
        Self {
            status,
            grpc_code: -1,
            message: message.into(),
        }
    }

    /// Error for a call that ended with a non-OK gRPC status.
    pub fn grpc(grpc_code: i32, detail: impl Into<String>) -> Self {
        Self {
            status: crate::sys::STATUS_GRPC,
            grpc_code,
            message: detail.into(),
        }
    }

    /// Binding-local error (loader, ABI check, invalid argument).
    pub fn local(status: i32, message: impl Into<String>) -> Self {
        Self::from_status(status, message)
    }

    /// Native `capsula_status` value (`0` is never stored).
    pub fn status(&self) -> i32 {
        self.status
    }

    /// Canonical gRPC code when the failure came from the daemon.
    pub fn grpc_code(&self) -> Option<i32> {
        (self.grpc_code >= 0).then_some(self.grpc_code)
    }

    /// gRPC code when present, otherwise the native status.
    pub fn code(&self) -> i32 {
        if self.grpc_code >= 0 {
            self.grpc_code
        } else {
            self.status
        }
    }

    pub fn message(&self) -> &str {
        &self.message
    }

    pub fn is_cancelled(&self) -> bool {
        self.status == crate::sys::STATUS_CANCELLED || self.grpc_code == 1
    }

    pub fn is_timeout(&self) -> bool {
        self.status == crate::sys::STATUS_TIMEOUT
    }

    pub fn is_end_of_stream(&self) -> bool {
        self.status == crate::sys::STATUS_END_OF_STREAM
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.grpc_code() {
            Some(code) => write!(f, "capsulad gRPC status {}: {}", code, self.message),
            None => write!(f, "capsulad status {}: {}", self.status, self.message),
        }
    }
}

impl std::error::Error for Error {}

impl From<std::ffi::NulError> for Error {
    fn from(err: std::ffi::NulError) -> Self {
        Error::local(CODE_INVALID_ARGUMENT, err.to_string())
    }
}

pub type Result<T, E = Error> = std::result::Result<T, E>;
