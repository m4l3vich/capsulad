//! UDP sockets: connected or unconnected, datagram-bounded.
//!
//! `udp_open` binds `{bind,port}` (default `0.0.0.0:0`) and, when a non-empty
//! host and non-zero port are given, connects. The bind port must be 0:
//! outbound sockets only ever use an ephemeral local port, because a fixed
//! inbound port would bypass the daemon's listener policy. A fixed inbound
//! UDP port belongs to `listener_open`, which the daemon authorizes. Both
//! shapes use `send_to` for an explicit peer; the connected form may omit it.
//! Datagrams are capped at the protocol maximum 65507, timeouts follow the ABI
//! convention (0 polls, 0xFFFFFFFF waits), and close unblocks a pending
//! receive.

use std::net::{IpAddr, SocketAddr};
use std::str::FromStr;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;
use tokio::net::UdpSocket;
use tokio::sync::{watch, Mutex};

use crate::ffi::{SdkError, SdkResult, INVALID_ARGUMENT, NOT_CONNECTED, TOO_LARGE};
use crate::registry::Registry;
use crate::stream::{bounded, io_op, read_len};

pub const MAX_DATAGRAM: usize = 65507;
pub const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(30);

pub static UDP_SOCKETS: Registry<UdpInner> = Registry::new();

#[derive(Debug, Clone)]
pub struct UdpOptions {
    pub bind: String,
    pub port: u16,
}

impl Default for UdpOptions {
    fn default() -> Self {
        Self {
            bind: "0.0.0.0".to_string(),
            port: 0,
        }
    }
}

impl UdpOptions {
    pub fn parse(bytes: &[u8]) -> SdkResult<Self> {
        let mut options = Self::default();
        if bytes.is_empty() {
            return Ok(options);
        }
        let value: Value = serde_json::from_slice(bytes).map_err(|err| {
            SdkError::new(
                INVALID_ARGUMENT,
                format!("udp options JSON is not valid: {err}"),
            )
        })?;
        let object = value
            .as_object()
            .ok_or_else(|| SdkError::new(INVALID_ARGUMENT, "udp options must be a JSON object"))?;
        for (key, value) in object {
            match key.as_str() {
                "bind" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "udp bind must be a string")
                    })?;
                    if text.is_empty() {
                        return Err(SdkError::new(INVALID_ARGUMENT, "udp bind is empty"));
                    }
                    options.bind = text.to_string();
                }
                "port" => {
                    let port = value.as_u64().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "udp port must be an integer")
                    })?;
                    options.port = u16::try_from(port).map_err(|_| {
                        SdkError::new(INVALID_ARGUMENT, "udp port must be 0..65535")
                    })?;
                }
                other => {
                    return Err(SdkError::new(
                        INVALID_ARGUMENT,
                        format!("unknown udp option {other:?}"),
                    ))
                }
            }
        }
        ephemeral_bind_port(options.port)?;
        Ok(options)
    }
}

/// Outbound sockets bind an ephemeral local port only. A fixed inbound port
/// bypasses daemon policy, so it is refused with a pointer at the mediated
/// listener path instead.
fn ephemeral_bind_port(port: u16) -> SdkResult<()> {
    if port == 0 {
        return Ok(());
    }
    Err(SdkError::new(
        INVALID_ARGUMENT,
        format!(
            "udp bind port must be 0 (ephemeral), not {port}: the SDK does not open fixed inbound UDP ports; use listener_open with {{\"protocol\":\"udp\",\"port\":{port}}} so the daemon applies its policy"
        ),
    ))
}

pub struct UdpInner {
    socket: Arc<UdpSocket>,
    connected: bool,
    peer: Option<SocketAddr>,
    bound: SocketAddr,
    closed: AtomicBool,
    closed_tx: watch::Sender<bool>,
    /// One logical reader: concurrent recv calls are serialized.
    recv_gate: Mutex<()>,
}

impl UdpInner {
    fn new(socket: UdpSocket, connected: bool, peer: Option<SocketAddr>) -> SdkResult<Arc<Self>> {
        let bound = socket
            .local_addr()
            .map_err(|err| SdkError::new(crate::ffi::TRANSPORT, err.to_string()))?;
        let (closed_tx, _) = watch::channel(false);
        Ok(Arc::new(Self {
            socket: Arc::new(socket),
            connected,
            peer,
            bound,
            closed: AtomicBool::new(false),
            closed_tx,
            recv_gate: Mutex::new(()),
        }))
    }

    pub fn connected(&self) -> bool {
        self.connected
    }

    pub fn is_closed(&self) -> bool {
        self.closed.load(Ordering::Acquire)
    }

    pub async fn send(
        &self,
        peer: Option<SocketAddr>,
        data: &[u8],
        timeout_ms: u32,
    ) -> SdkResult<usize> {
        if data.len() > MAX_DATAGRAM {
            return Err(SdkError::new(
                TOO_LARGE,
                format!(
                    "datagram is {} bytes; the cap is {MAX_DATAGRAM}",
                    data.len()
                ),
            ));
        }
        let target = match peer.or(self.peer) {
            Some(target) => target,
            None => {
                return Err(SdkError::new(
                    NOT_CONNECTED,
                    "udp socket is unconnected and no peer_json was supplied",
                ))
            }
        };
        // A connected UDP socket rejects sendto(2) with EISCONN on BSD/macOS;
        // use send(2) whenever the target is the connected peer.
        if self.peer == Some(target) {
            io_op(&self.closed_tx, timeout_ms, self.socket.send(data)).await
        } else {
            io_op(
                &self.closed_tx,
                timeout_ms,
                self.socket.send_to(data, target),
            )
            .await
        }
    }

    pub async fn recv(&self, max_len: usize, timeout_ms: u32) -> SdkResult<(Vec<u8>, SocketAddr)> {
        let _gate = self.recv_gate.lock().await;
        let len = read_len(max_len, MAX_DATAGRAM, MAX_DATAGRAM)?;
        let mut buf = vec![0u8; len];
        let (read, peer) =
            io_op(&self.closed_tx, timeout_ms, self.socket.recv_from(&mut buf)).await?;
        buf.truncate(read);
        Ok((buf, peer))
    }

    pub async fn close(&self) {
        if self.closed.swap(true, Ordering::AcqRel) {
            return;
        }
        self.closed_tx.send_replace(true);
    }

    pub fn info_json(&self) -> Vec<u8> {
        let value = serde_json::json!({
            "kind": "udp",
            "bind": self.bound.ip().to_string(),
            "port": self.bound.port(),
            "connected": self.connected,
            "peer": self.peer.map(peer_json_value).unwrap_or_else(|| serde_json::json!({})),
            "closed": self.closed.load(Ordering::Acquire),
        });
        serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
    }
}

fn peer_json_value(address: SocketAddr) -> Value {
    serde_json::json!({
        "address": address.ip().to_string(),
        "port": address.port(),
    })
}

pub fn peer_json(address: SocketAddr) -> Vec<u8> {
    serde_json::to_vec(&peer_json_value(address)).unwrap_or_else(|_| b"{}".to_vec())
}

/// Parses the optional peer object `{"address":"1.2.3.4","port":53}`.
pub fn parse_peer(bytes: &[u8]) -> SdkResult<Option<SocketAddr>> {
    if bytes.is_empty() {
        return Ok(None);
    }
    let value: Value = serde_json::from_slice(bytes).map_err(|err| {
        SdkError::new(
            INVALID_ARGUMENT,
            format!("udp peer JSON is not valid: {err}"),
        )
    })?;
    let object = value
        .as_object()
        .ok_or_else(|| SdkError::new(INVALID_ARGUMENT, "udp peer must be a JSON object"))?;
    let mut address = None;
    let mut port = None;
    for (key, value) in object {
        match key.as_str() {
            "address" => {
                let text = value.as_str().ok_or_else(|| {
                    SdkError::new(INVALID_ARGUMENT, "udp peer address must be a string")
                })?;
                let ip = IpAddr::from_str(text).map_err(|_| {
                    SdkError::new(
                        INVALID_ARGUMENT,
                        format!("udp peer address {text:?} is not an IP literal"),
                    )
                })?;
                address = Some(ip);
            }
            "port" => {
                let number = value.as_u64().ok_or_else(|| {
                    SdkError::new(INVALID_ARGUMENT, "udp peer port must be an integer")
                })?;
                if number == 0 || number > u64::from(u16::MAX) {
                    return Err(SdkError::new(
                        INVALID_ARGUMENT,
                        "udp peer port must be 1..65535",
                    ));
                }
                port = Some(number as u16);
            }
            other => {
                return Err(SdkError::new(
                    INVALID_ARGUMENT,
                    format!("unknown udp peer field {other:?}"),
                ))
            }
        }
    }
    match (address, port) {
        (Some(address), Some(port)) => Ok(Some(SocketAddr::new(address, port))),
        _ => Err(SdkError::new(
            INVALID_ARGUMENT,
            "udp peer needs both address and port",
        )),
    }
}

pub fn open(host: &str, port: u16, options_json: &[u8], timeout_ms: u32) -> SdkResult<u64> {
    if host.is_empty() != (port == 0) {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "udp_open needs both host and port, or neither, to choose connected or unconnected",
        ));
    }
    if host.len() > 255 {
        return Err(SdkError::new(INVALID_ARGUMENT, "udp_open host is too long"));
    }
    let options = UdpOptions::parse(options_json)?;
    let timeout = if timeout_ms == 0 {
        DEFAULT_CONNECT_TIMEOUT
    } else {
        Duration::from_millis(u64::from(timeout_ms))
    };
    let bind_address = resolve_bind(&options.bind, options.port)?;
    // A bind literal may embed its own port (`127.0.0.1:5353`), which would
    // slip past the option check; enforce the ephemeral rule on the resolved
    // address too.
    ephemeral_bind_port(bind_address.port())?;
    let budget = timeout.as_millis().min(u128::from(u32::MAX - 1)) as u32;
    let inner = crate::runtime::runtime().block_on(bounded(budget, async {
        let socket = UdpSocket::bind(bind_address).await.map_err(|err| {
            SdkError::new(
                crate::ffi::TRANSPORT,
                format!("bind udp {}: {err}", bind_address),
            )
        })?;
        if host.is_empty() {
            return UdpInner::new(socket, false, None);
        }
        let target = tokio::net::lookup_host((host, port))
            .await
            .map_err(|err| {
                SdkError::new(
                    crate::ffi::TRANSPORT,
                    format!("resolve udp peer {host}:{port}: {err}"),
                )
            })?
            .next()
            .ok_or_else(|| {
                SdkError::new(
                    crate::ffi::TRANSPORT,
                    format!("resolve udp peer {host}:{port}: no addresses"),
                )
            })?;
        socket.connect(target).await.map_err(|err| {
            SdkError::new(
                crate::ffi::TRANSPORT,
                format!("connect udp {host}:{port}: {err}"),
            )
        })?;
        UdpInner::new(socket, true, Some(target))
    }))??;
    let handle = UDP_SOCKETS.insert(inner);
    Ok(handle)
}

fn resolve_bind(bind: &str, port: u16) -> SdkResult<SocketAddr> {
    // Numeric addresses are the normal case; a hostname given as bind is
    // resolved synchronously through the same resolver the runtime uses.
    if let Ok(ip) = IpAddr::from_str(bind) {
        return Ok(SocketAddr::new(ip, port));
    }
    if let Ok(address) = SocketAddr::from_str(bind) {
        return Ok(address);
    }
    let mut addresses = std::net::ToSocketAddrs::to_socket_addrs(&(bind, port)).map_err(|err| {
        SdkError::new(
            INVALID_ARGUMENT,
            format!("udp bind {bind:?} cannot be resolved: {err}"),
        )
    })?;
    addresses.next().ok_or_else(|| {
        SdkError::new(
            INVALID_ARGUMENT,
            format!("udp bind {bind:?} resolved to no address"),
        )
    })
}

pub fn socket(handle: u64) -> SdkResult<Arc<UdpInner>> {
    UDP_SOCKETS
        .get(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown udp handle"))
}

/// Releases a UDP handle. A pending recv/send returns CANCELLED.
pub fn close(handle: u64) -> SdkResult<()> {
    let inner = UDP_SOCKETS
        .remove(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown udp handle"))?;
    if tokio::runtime::Handle::try_current().is_ok() {
        crate::runtime::runtime().spawn(async move { inner.close().await });
    } else {
        crate::runtime::runtime().block_on(inner.close());
    }
    Ok(())
}
