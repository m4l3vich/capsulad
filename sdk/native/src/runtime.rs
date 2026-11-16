//! Process-wide bounded Tokio runtime and UDS channel construction.

use std::sync::LazyLock;
use std::time::Duration;

use tonic::transport::{Channel, Endpoint};

use crate::ffi::{map_transport, SdkResult};

static RUNTIME: LazyLock<tokio::runtime::Runtime> = LazyLock::new(|| {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .max_blocking_threads(2)
        .thread_name("capsulad-native")
        .enable_all()
        .build()
        .expect("capsulad native: failed to build Tokio runtime")
});

/// One bounded runtime for the whole process. Created on first use, never per
/// client or per call.
pub fn runtime() -> &'static tokio::runtime::Runtime {
    &RUNTIME
}

pub const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

pub async fn connect_uds(path: &str, timeout: Duration) -> SdkResult<Channel> {
    if path.is_empty() {
        return Err(crate::ffi::SdkError::new(
            crate::ffi::INVALID_ARGUMENT,
            "socket path is empty",
        ));
    }
    let endpoint = Endpoint::from_shared(format!("unix://{path}"))
        .map_err(map_transport)?
        .connect_timeout(if timeout.is_zero() {
            DEFAULT_CONNECT_TIMEOUT
        } else {
            timeout
        });
    endpoint.connect().await.map_err(map_transport)
}
