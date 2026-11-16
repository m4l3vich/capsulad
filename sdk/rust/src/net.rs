//! Outbound TCP/TLS streams and UDP sockets on the shared engine.
//!
//! These are synchronous, blocking wrappers like the rest of the crate: every
//! receive takes `Option<Duration>` where `None` waits forever, and `Some`
//! bounds the wait in whole milliseconds. TLS is always verified; there is no
//! insecure mode.

use std::net::IpAddr;
use std::sync::Arc;
use std::time::Duration;

use serde_json::{Map, Value};

use crate::client::parse_json;
use crate::error::{self, Error, Result};
use crate::sys::{self, Buf};
use crate::{status_error, take_buf, timeout_ms, wait_ms, Inner, Library};

/// TLS and framing options for [`Stream::open`].
#[derive(Debug, Clone, Default)]
pub struct TcpOptions {
    /// Enable a verified TLS 1.2+ handshake.
    pub tls: bool,
    /// `"system"` (default), `"webpki"`, or `"pem"` (only `ca_pem` anchors are
    /// trusted: no system bundle and no public roots).
    pub roots: Option<String>,
    /// Additional PEM-encoded trust anchors.
    pub ca_pem: Option<Vec<u8>>,
    /// Overrides SNI and certificate verification; `None` uses the host.
    pub server_name: Option<String>,
    /// `Some(false)` disables TCP_NODELAY; `None` keeps the native default
    /// (enabled).
    pub nodelay: Option<bool>,
}

impl TcpOptions {
    fn encode(&self) -> Result<String> {
        let mut map = Map::new();
        if self.tls {
            map.insert("tls".into(), Value::Bool(true));
        }
        if let Some(roots) = &self.roots {
            if !matches!(roots.as_str(), "system" | "webpki" | "pem") {
                return Err(Error::local(
                    error::CODE_INVALID_ARGUMENT,
                    format!("roots {roots:?} must be \"system\", \"webpki\" or \"pem\""),
                ));
            }
            map.insert("roots".into(), Value::String(roots.clone()));
        }
        if let Some(pem) = &self.ca_pem {
            let text = String::from_utf8(pem.clone()).map_err(|_| {
                Error::local(
                    error::CODE_INVALID_ARGUMENT,
                    "ca_pem must be UTF-8 PEM text",
                )
            })?;
            map.insert("ca_pem".into(), Value::String(text));
        }
        if let Some(name) = &self.server_name {
            map.insert("server_name".into(), Value::String(name.clone()));
        }
        if let Some(nodelay) = self.nodelay {
            map.insert("nodelay".into(), Value::Bool(nodelay));
        }
        Ok(Value::Object(map).to_string())
    }
}

/// Which direction of a stream to half-close.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(i32)]
pub enum Shutdown {
    /// Send FIN / TLS close_notify; later writes fail.
    Write = 0,
    /// Make later reads report the end of stream.
    Read = 1,
    /// Both directions.
    Both = 2,
}

/// Diagnostics reported by `stream_info`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StreamInfo {
    pub kind: String,
    pub host: String,
    pub port: u16,
    pub tls: bool,
    pub server_name: String,
    pub peer: String,
    pub closed: bool,
    pub read_shutdown: bool,
    pub write_shutdown: bool,
}

/// One outbound TCP or TLS connection. Dropping it closes the native handle;
/// [`Stream::close`] releases it deterministically. A close from another thread
/// unblocks a pending read or write. Reads are serialized (one logical reader)
/// and so are writes (one logical writer): concurrent writes never interleave
/// bytes, though one write may accept fewer bytes than requested. The halves
/// are independent, so a parked read never delays a write or a write-side
/// shutdown.
pub struct Stream {
    inner: Arc<Inner>,
    handle: u64,
}

impl Stream {
    /// Wraps a stream handle returned by `listener_accept`.
    pub(crate) fn from_handle(inner: Arc<Inner>, handle: u64) -> Self {
        Self { inner, handle }
    }

    /// Performs DNS, TCP and the optional TLS handshake under `timeout`
    /// (`None` uses the native 30 s default).
    pub fn open(
        library: &Library,
        host: &str,
        port: u16,
        options: &TcpOptions,
        timeout: Option<Duration>,
    ) -> Result<Self> {
        if host.is_empty() {
            return Err(Error::local(
                error::CODE_INVALID_ARGUMENT,
                "host is required",
            ));
        }
        if port == 0 {
            return Err(Error::local(
                error::CODE_INVALID_ARGUMENT,
                "port must be 1..65535",
            ));
        }
        let encoded = options.encode()?;
        let mut handle = 0u64;
        let status = unsafe {
            (library.inner.api.tcp_open)(
                host.as_ptr().cast(),
                host.len(),
                port,
                encoded.as_ptr(),
                encoded.len(),
                timeout_ms(timeout),
                &mut handle,
            )
        };
        crate::check_status(&library.inner, status, "tcp_open")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "tcp_open returned a zero handle",
            ));
        }
        Ok(Self {
            inner: library.inner.clone(),
            handle,
        })
    }

    pub fn handle(&self) -> u64 {
        self.handle
    }

    /// Reads at most `max_len` bytes; `None` is a clean remote close and
    /// `None` for `timeout` waits forever. A `TIMEOUT` error leaves the stream
    /// usable.
    pub fn read(&self, max_len: usize, timeout: Option<Duration>) -> Result<Option<Vec<u8>>> {
        let mut out = Buf::default();
        let status = unsafe {
            (self.inner.api.stream_read)(self.handle, max_len, wait_ms(timeout), &mut out)
        };
        match status {
            sys::STATUS_OK => Ok(Some(unsafe { take_buf(&self.inner, out) })),
            sys::STATUS_END_OF_STREAM => {
                unsafe { take_buf(&self.inner, out) };
                Ok(None)
            }
            _ => {
                let err = status_error(&self.inner, status, "stream_read");
                unsafe { take_buf(&self.inner, out) };
                Err(err)
            }
        }
    }

    /// Writes one chunk. The returned count is in `1..=data.len()` (a partial
    /// write is possible). `None` for `timeout` waits forever.
    pub fn write(&self, data: &[u8], timeout: Option<Duration>) -> Result<usize> {
        let mut written = 0usize;
        let status = unsafe {
            (self.inner.api.stream_write)(
                self.handle,
                data.as_ptr(),
                data.len(),
                wait_ms(timeout),
                &mut written,
            )
        };
        crate::check_status(&self.inner, status, "stream_write")?;
        Ok(written)
    }

    /// Half-closes one direction.
    pub fn shutdown(&self, how: Shutdown) -> Result<()> {
        let status = unsafe { (self.inner.api.stream_shutdown)(self.handle, how as i32) };
        crate::check_status(&self.inner, status, "stream_shutdown")
    }

    /// Native diagnostics.
    pub fn info(&self) -> Result<StreamInfo> {
        let mut buf = Buf::default();
        let status = unsafe { (self.inner.api.stream_info)(self.handle, &mut buf) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "stream_info");
            unsafe { take_buf(&self.inner, buf) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, buf) };
        let value = parse_json(&bytes, "stream_info")?;
        Ok(StreamInfo {
            kind: string_field(&value, "kind"),
            host: string_field(&value, "host"),
            port: number_field(&value, "port") as u16,
            tls: bool_field(&value, "tls"),
            server_name: string_field(&value, "server_name"),
            peer: string_field(&value, "peer"),
            closed: bool_field(&value, "closed"),
            read_shutdown: bool_field(&value, "read_shutdown"),
            write_shutdown: bool_field(&value, "write_shutdown"),
        })
    }

    /// Releases the handle and unblocks a pending read/write with `CANCELLED`.
    /// Idempotent; also runs on drop.
    pub fn close(&mut self) -> Result<()> {
        if self.handle == 0 {
            return Ok(());
        }
        let handle = self.handle;
        self.handle = 0;
        let status = unsafe { (self.inner.api.stream_close)(handle) };
        crate::check_status(&self.inner, status, "stream_close")
    }
}

impl Drop for Stream {
    fn drop(&mut self) {
        let _ = self.close();
    }
}

/// One datagram peer (`{"address":"1.2.3.4","port":53}`). `address` must be
/// an IP literal: the library does not resolve peer names and rejects one
/// with `INVALID_ARGUMENT`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PeerAddr {
    pub address: String,
    pub port: u16,
}

impl PeerAddr {
    pub fn new(address: impl Into<String>, port: u16) -> Self {
        Self {
            address: address.into(),
            port,
        }
    }

    pub(crate) fn to_json(&self) -> Value {
        serde_json::json!({"address": self.address, "port": self.port})
    }

    fn from_value(value: &Value) -> Result<Self> {
        let address = value
            .get("address")
            .and_then(Value::as_str)
            .ok_or_else(|| {
                Error::local(
                    error::CODE_INVALID_ARGUMENT,
                    "library returned a peer without an address",
                )
            })?;
        let port = value.get("port").and_then(Value::as_u64).unwrap_or(0);
        let port = u16::try_from(port).map_err(|_| {
            Error::local(
                error::CODE_INVALID_ARGUMENT,
                "library returned an invalid peer port",
            )
        })?;
        Ok(Self {
            address: address.to_string(),
            port,
        })
    }

    /// Renders `[v6]:port` or `v4:port`.
    pub fn socket_addr(&self) -> Result<std::net::SocketAddr> {
        if let Ok(ip) = self.address.parse::<IpAddr>() {
            return Ok(std::net::SocketAddr::new(ip, self.port));
        }
        Err(Error::local(
            error::CODE_INVALID_ARGUMENT,
            format!("peer address {:?} is not an IP literal", self.address),
        ))
    }
}

/// Local bind options for [`UdpSocket::open`].
#[derive(Debug, Clone, Default)]
pub struct UdpOptions {
    /// Local address; `None` binds `0.0.0.0`. It must not embed a port: the
    /// bound port is always ephemeral.
    pub bind: Option<String>,
    /// Local port; `None` or `Some(0)` (an ephemeral port) is the only
    /// accepted value, because `udp_open` never binds a fixed local port. A
    /// fixed inbound UDP port belongs to [`crate::Listener::open`], which the
    /// daemon authorizes; a non-zero value is refused with
    /// `INVALID_ARGUMENT`.
    pub port: Option<u16>,
}

impl UdpOptions {
    fn encode(&self) -> Result<String> {
        let mut map = Map::new();
        if let Some(bind) = &self.bind {
            map.insert("bind".into(), Value::String(bind.clone()));
        }
        if let Some(port) = self.port {
            map.insert("port".into(), Value::from(port));
        }
        Ok(Value::Object(map).to_string())
    }
}

/// One UDP socket, connected or unconnected. Dropping it closes the native
/// handle; close unblocks a pending receive.
pub struct UdpSocket {
    inner: Arc<Inner>,
    handle: u64,
}

impl UdpSocket {
    /// Opens a socket. A non-empty `host` and non-zero `port` connect it to
    /// that peer (`host` may be a DNS name); empty/0 leaves it unconnected so
    /// every [`UdpSocket::send`] must name a peer, and those peers must be IP
    /// literals. `options.port` must be 0: the local port is always
    /// ephemeral.
    pub fn open(
        library: &Library,
        host: &str,
        port: u16,
        options: &UdpOptions,
        timeout: Option<Duration>,
    ) -> Result<Self> {
        if (host.is_empty()) != (port == 0) {
            return Err(Error::local(
                error::CODE_INVALID_ARGUMENT,
                "both host and port are required to connect, or neither",
            ));
        }
        let encoded = options.encode()?;
        let mut handle = 0u64;
        let status = unsafe {
            (library.inner.api.udp_open)(
                host.as_ptr().cast(),
                host.len(),
                port,
                encoded.as_ptr(),
                encoded.len(),
                timeout_ms(timeout),
                &mut handle,
            )
        };
        crate::check_status(&library.inner, status, "udp_open")?;
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "udp_open returned a zero handle",
            ));
        }
        Ok(Self {
            inner: library.inner.clone(),
            handle,
        })
    }

    pub fn handle(&self) -> u64 {
        self.handle
    }

    /// Sends one datagram. `peer` `None` uses the connected peer and fails with
    /// `NOT_CONNECTED` on an unconnected socket. Returns the bytes sent.
    pub fn send(
        &self,
        peer: Option<&PeerAddr>,
        data: &[u8],
        timeout: Option<Duration>,
    ) -> Result<usize> {
        let peer_json = peer.map(|value| value.to_json().to_string());
        let peer_bytes = peer_json.as_deref().unwrap_or("").as_bytes();
        let mut sent = 0usize;
        let status = unsafe {
            (self.inner.api.udp_send)(
                self.handle,
                peer_bytes.as_ptr(),
                peer_bytes.len(),
                data.as_ptr(),
                data.len(),
                wait_ms(timeout),
                &mut sent,
            )
        };
        crate::check_status(&self.inner, status, "udp_send")?;
        Ok(sent)
    }

    /// Receives one datagram. `max_len` 0 uses the native 65507-byte default;
    /// larger requests fail with `TOO_LARGE`.
    pub fn recv(&self, max_len: usize, timeout: Option<Duration>) -> Result<(Vec<u8>, PeerAddr)> {
        let mut data = Buf::default();
        let mut peer = Buf::default();
        let status = unsafe {
            (self.inner.api.udp_recv)(self.handle, max_len, wait_ms(timeout), &mut data, &mut peer)
        };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "udp_recv");
            unsafe {
                take_buf(&self.inner, data);
                take_buf(&self.inner, peer);
            }
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, data) };
        let peer_bytes = unsafe { take_buf(&self.inner, peer) };
        let peer = parse_peer(&peer_bytes)?;
        Ok((bytes, peer))
    }

    /// Releases the handle. Idempotent; also runs on drop.
    pub fn close(&mut self) -> Result<()> {
        if self.handle == 0 {
            return Ok(());
        }
        let handle = self.handle;
        self.handle = 0;
        let status = unsafe { (self.inner.api.udp_close)(handle) };
        crate::check_status(&self.inner, status, "udp_close")
    }
}

impl Drop for UdpSocket {
    fn drop(&mut self) {
        let _ = self.close();
    }
}

pub(crate) fn parse_peer(bytes: &[u8]) -> Result<PeerAddr> {
    if bytes.is_empty() {
        return Err(Error::local(
            sys::STATUS_INVALID_MESSAGE,
            "library returned an empty peer",
        ));
    }
    let value = parse_json(bytes, "peer")?;
    PeerAddr::from_value(&value)
}

pub(crate) fn parse_listener_info(bytes: &[u8]) -> Result<ListenerInfoParts> {
    let value = parse_json(bytes, "listener_info")?;
    let port = value.get("port").and_then(Value::as_u64).unwrap_or(0);
    let port = u16::try_from(port).map_err(|_| {
        Error::local(
            sys::STATUS_INTERNAL,
            "library returned an invalid bound port",
        )
    })?;
    Ok(ListenerInfoParts {
        protocol: string_field(&value, "protocol"),
        address: string_field(&value, "address"),
        port,
        closed: bool_field(&value, "closed"),
    })
}

/// Shared listener-info parsing result; the public struct lives in
/// [`crate::listener`].
pub(crate) struct ListenerInfoParts {
    pub protocol: String,
    pub address: String,
    pub port: u16,
    pub closed: bool,
}

pub(crate) fn string_field(value: &Value, key: &str) -> String {
    value
        .get(key)
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string()
}

pub(crate) fn bool_field(value: &Value, key: &str) -> bool {
    value.get(key).and_then(Value::as_bool).unwrap_or(false)
}

pub(crate) fn number_field(value: &Value, key: &str) -> u64 {
    value.get(key).and_then(Value::as_u64).unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn encoded(options: &TcpOptions) -> Value {
        serde_json::from_str(&options.encode().expect("options encode")).expect("valid JSON")
    }

    #[test]
    fn tcp_options_match_the_native_snake_case_contract() {
        let value = encoded(&TcpOptions {
            tls: true,
            roots: Some("pem".into()),
            ca_pem: Some(b"pem".to_vec()),
            server_name: Some("voice.test".into()),
            nodelay: Some(false),
        });
        assert_eq!(value["tls"], true);
        assert_eq!(value["roots"], "pem");
        assert_eq!(value["ca_pem"], "pem");
        assert_eq!(value["server_name"], "voice.test");
        assert_eq!(value["nodelay"], false);

        let value = encoded(&TcpOptions::default());
        assert_eq!(value, serde_json::json!({}));
        assert!(TcpOptions {
            roots: Some("bogus".into()),
            ..TcpOptions::default()
        }
        .encode()
        .is_err());
    }

    #[test]
    fn udp_options_omit_defaults() {
        let value: Value =
            serde_json::from_str(&UdpOptions::default().encode().expect("encode")).expect("json");
        assert_eq!(value, serde_json::json!({}));
        let value: Value = serde_json::from_str(
            &UdpOptions {
                bind: Some("127.0.0.1".into()),
                port: Some(0),
            }
            .encode()
            .expect("encode"),
        )
        .expect("json");
        assert_eq!(value["bind"], "127.0.0.1");
        assert_eq!(value["port"], 0);
    }

    #[test]
    fn peer_json_round_trips() {
        let peer = PeerAddr::new("192.0.2.1", 53);
        let encoded = peer.to_json().to_string();
        let decoded = PeerAddr::from_value(&serde_json::from_str(&encoded).expect("peer json"))
            .expect("peer");
        assert_eq!(decoded, peer);
        assert!(PeerAddr::new("example.test", 53).socket_addr().is_err());
        assert_eq!(
            PeerAddr::new("::1", 53)
                .socket_addr()
                .expect("v6")
                .to_string(),
            "[::1]:53"
        );
    }

    #[test]
    fn shutdown_values_are_pinned() {
        assert_eq!(Shutdown::Write as i32, 0);
        assert_eq!(Shutdown::Read as i32, 1);
        assert_eq!(Shutdown::Both as i32, 2);
    }
}
