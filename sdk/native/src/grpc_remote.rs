//! Remote gRPC channels: HTTP/2 over TLS (ALPN `h2`) or cleartext prior
//! knowledge (`h2c`) to an origin outside the daemon's Unix socket.
//!
//! The returned `tonic` channel is installed in a normal `ClientInner`, so the
//! existing call machinery (queues, deadlines, `RawCodec`, metadata) is reused
//! unchanged. Remote clients have no run id and no app bus.

use std::future::Future;
use std::io;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;

use http::Uri;
use tonic::transport::{Channel, Endpoint};
use tower::Service;

use crate::ffi::{SdkError, SdkResult, INVALID_ARGUMENT, TIMEOUT, TRANSPORT};
use crate::primitives::{parse_origin, ParsedUrl};
use crate::tls::{self, NetStream, RootsMode, TlsOptions};

pub const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Debug, Clone)]
pub struct GrpcOptions {
    pub roots: RootsMode,
    pub ca_pem: Option<Vec<u8>>,
    pub server_name: Option<String>,
    pub connect_timeout: Duration,
}

impl Default for GrpcOptions {
    fn default() -> Self {
        Self {
            roots: RootsMode::System,
            ca_pem: None,
            server_name: None,
            connect_timeout: DEFAULT_CONNECT_TIMEOUT,
        }
    }
}

impl GrpcOptions {
    pub fn parse(bytes: &[u8]) -> SdkResult<Self> {
        let mut options = Self::default();
        if bytes.is_empty() {
            return Ok(options);
        }
        let value: serde_json::Value = serde_json::from_slice(bytes).map_err(|err| {
            SdkError::new(
                INVALID_ARGUMENT,
                format!("gRPC options JSON is not valid: {err}"),
            )
        })?;
        let object = value
            .as_object()
            .ok_or_else(|| SdkError::new(INVALID_ARGUMENT, "gRPC options must be a JSON object"))?;
        for (key, value) in object {
            match key.as_str() {
                "roots" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "gRPC roots must be a string")
                    })?;
                    options.roots = match text {
                        "system" => RootsMode::System,
                        "webpki" => RootsMode::Webpki,
                        "pem" => RootsMode::Pem,
                        other => {
                            return Err(SdkError::new(
                                INVALID_ARGUMENT,
                                format!("unknown gRPC roots {other:?}; want system, webpki or pem"),
                            ))
                        }
                    };
                }
                "ca_pem" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "gRPC ca_pem must be a string")
                    })?;
                    if !text.is_empty() {
                        options.ca_pem = Some(text.as_bytes().to_vec());
                    }
                }
                "server_name" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "gRPC server_name must be a string")
                    })?;
                    if text.is_empty() {
                        return Err(SdkError::new(INVALID_ARGUMENT, "gRPC server_name is empty"));
                    }
                    options.server_name = Some(text.to_string());
                }
                "connect_timeout_ms" => {
                    let number = value
                        .as_u64()
                        .filter(|value| *value > 0 && *value <= u64::from(u32::MAX))
                        .ok_or_else(|| {
                            SdkError::new(
                                INVALID_ARGUMENT,
                                "gRPC connect_timeout_ms must be a positive integer",
                            )
                        })?;
                    options.connect_timeout = Duration::from_millis(number);
                }
                other => {
                    return Err(SdkError::new(
                        INVALID_ARGUMENT,
                        format!("unknown gRPC option {other:?}"),
                    ))
                }
            }
        }
        Ok(options)
    }
}

/// Connects one remote channel. `url` is an origin only.
pub async fn connect(url: &str, options: &GrpcOptions) -> SdkResult<Channel> {
    let origin = parse_origin(url)?;
    let tls_config = if origin.scheme == "https" {
        Some(tls::client_config(
            &TlsOptions {
                roots: options.roots,
                ca_pem: options.ca_pem.clone(),
            },
            true,
        )?)
    } else {
        None
    };
    let name = options
        .server_name
        .clone()
        .unwrap_or_else(|| origin.host.clone());
    if tls_config.is_some() {
        tls::server_name(&name)?;
    }
    let connector = Connector {
        origin: origin.clone(),
        tls: tls_config,
        name,
        timeout: options.connect_timeout,
    };
    let uri: Uri = origin
        .origin()
        .parse()
        .map_err(|err| SdkError::new(INVALID_ARGUMENT, format!("gRPC origin URI: {err}")))?;
    let endpoint = Endpoint::from_shared(uri.to_string())
        .map_err(|err| SdkError::new(INVALID_ARGUMENT, format!("gRPC endpoint: {err}")))?
        .connect_timeout(options.connect_timeout);
    let channel = tokio::time::timeout(
        options.connect_timeout,
        endpoint.connect_with_connector(connector),
    )
    .await
    .map_err(|_| {
        SdkError::new(
            TIMEOUT,
            format!(
                "gRPC connect to {} timed out after {:?}",
                origin.origin(),
                options.connect_timeout
            ),
        )
    })?
    .map_err(|err| SdkError::new(TRANSPORT, format!("gRPC connect: {err}")))?;
    Ok(channel)
}

#[derive(Clone)]
struct Connector {
    origin: ParsedUrl,
    tls: Option<Arc<rustls::ClientConfig>>,
    name: String,
    timeout: Duration,
}

impl Service<Uri> for Connector {
    type Response = hyper_util::rt::TokioIo<NetStream>;
    type Error = io::Error;
    type Future = Pin<Box<dyn Future<Output = Result<Self::Response, io::Error>> + Send>>;

    fn poll_ready(&mut self, _cx: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, _uri: Uri) -> Self::Future {
        let origin = self.origin.clone();
        let tls = self.tls.clone();
        let name = self.name.clone();
        let timeout = self.timeout;
        Box::pin(async move {
            let stream = tls::connect(&origin.host, origin.port, tls, Some(&name), timeout)
                .await
                .map_err(|err| io::Error::new(io::ErrorKind::Other, err.message))?;
            Ok(hyper_util::rt::TokioIo::new(stream))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn options_reject_unknown_keys() {
        assert!(GrpcOptions::parse(br#"{"insecure":true}"#).is_err());
        assert!(GrpcOptions::parse(br#"{"server_name":""}"#).is_err());
        let options =
            GrpcOptions::parse(br#"{"roots":"webpki","server_name":"x","connect_timeout_ms":500}"#)
                .unwrap();
        assert_eq!(options.roots, RootsMode::Webpki);
        assert_eq!(options.server_name.as_deref(), Some("x"));
        assert_eq!(options.connect_timeout, Duration::from_millis(500));
    }

    #[test]
    fn connection_failure_is_a_transport_error() {
        let options = GrpcOptions {
            roots: RootsMode::Webpki,
            connect_timeout: Duration::from_millis(250),
            ..GrpcOptions::default()
        };
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        match runtime.block_on(connect("https://127.0.0.1:1", &options)) {
            Ok(_) => panic!("a closed loopback port must not connect"),
            Err(err) => assert!(
                err.status == TRANSPORT || err.status == TIMEOUT,
                "unexpected status {}: {}",
                err.status,
                err.message
            ),
        }
    }

    #[test]
    fn origin_only_urls_are_enforced_before_connecting() {
        let options = GrpcOptions::default();
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        assert!(runtime
            .block_on(connect("https://example.com/x", &options))
            .is_err());
        assert!(runtime
            .block_on(connect("ftp://example.com", &options))
            .is_err());
    }
}
