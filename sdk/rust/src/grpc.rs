//! Remote gRPC channels: an origin plus verification options turned into a
//! normal [`crate::Client`] handle.
//!
//! The channel speaks HTTP/2 over TLS (ALPN `h2`) for `https` origins and h2c
//! prior knowledge for `http`. Remote clients have no run id and no app bus;
//! calls reuse the existing [`crate::Call`] machinery.

use std::time::Duration;

use serde_json::{Map, Value};

use crate::error::Result;
use crate::positive_ms;

/// Verification and connect options for [`crate::Client::open_remote`].
#[derive(Debug, Clone, Default)]
pub struct RemoteOptions {
    /// `"system"` (default), `"webpki"`, or `"pem"` (only `ca_pem` anchors are
    /// trusted: no system bundle and no public roots).
    pub roots: Option<String>,
    /// Additional PEM-encoded trust anchors.
    pub ca_pem: Option<Vec<u8>>,
    /// TLS verification name; `None` uses the origin host (IP literals are
    /// verified as IP addresses).
    pub server_name: Option<String>,
    /// Bounds DNS, TCP, TLS and the HTTP/2 handshake. `None` uses 10 s.
    pub connect_timeout: Option<Duration>,
}

impl RemoteOptions {
    pub(crate) fn encode(&self) -> Result<String> {
        let mut map = Map::new();
        if let Some(roots) = &self.roots {
            map.insert("roots".into(), Value::String(roots.clone()));
        }
        if let Some(pem) = &self.ca_pem {
            let text = String::from_utf8(pem.clone()).map_err(|_| {
                crate::Error::local(
                    crate::error::CODE_INVALID_ARGUMENT,
                    "ca_pem must be UTF-8 PEM text",
                )
            })?;
            map.insert("ca_pem".into(), Value::String(text));
        }
        if let Some(name) = &self.server_name {
            map.insert("server_name".into(), Value::String(name.clone()));
        }
        if let Some(timeout) = self.connect_timeout {
            map.insert(
                "connect_timeout_ms".into(),
                Value::from(positive_ms(timeout)?),
            );
        }
        Ok(Value::Object(map).to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn encoded(options: &RemoteOptions) -> Value {
        serde_json::from_str(&options.encode().expect("options encode")).expect("valid JSON")
    }

    #[test]
    fn zero_connect_timeout_is_omitted() {
        let value = encoded(&RemoteOptions {
            connect_timeout: None,
            ..RemoteOptions::default()
        });
        assert!(
            value.get("connect_timeout_ms").is_none(),
            "None must omit the key so the native 10 s default applies"
        );
    }

    #[test]
    fn sub_millisecond_connect_timeout_rounds_up() {
        let value = encoded(&RemoteOptions {
            connect_timeout: Some(Duration::from_micros(100)),
            ..RemoteOptions::default()
        });
        assert_eq!(value["connect_timeout_ms"], 1);
        let value = encoded(&RemoteOptions {
            connect_timeout: Some(Duration::from_secs(2)),
            ..RemoteOptions::default()
        });
        assert_eq!(value["connect_timeout_ms"], 2_000);
    }

    #[test]
    fn zero_duration_connect_timeout_is_rejected() {
        assert!(RemoteOptions {
            connect_timeout: Some(Duration::ZERO),
            ..RemoteOptions::default()
        }
        .encode()
        .is_err());
    }
}
