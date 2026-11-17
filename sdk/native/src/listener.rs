//! Daemon-mediated listeners.
//!
//! The application never binds a listening socket. `listener_open` asks the
//! daemon over the inherited control channel (`CAPSULAD_CONTROL_FD`), receives
//! the already-bound descriptor via `SCM_RIGHTS`, makes it non-blocking and
//! wraps it for Tokio. Policy (allowed ports, addresses, backlog, per-run
//! limits) belongs to the daemon; this layer only validates option shape.

use std::os::fd::{FromRawFd, OwnedFd};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use tokio::sync::{watch, Mutex};

use crate::control;
use crate::ffi::{SdkError, SdkResult, INTERNAL, INVALID_ARGUMENT};
use crate::registry::Registry;
use crate::stream::{io_op, read_len, StreamInner, STREAMS};
use crate::udp::{self, MAX_DATAGRAM};

pub static LISTENERS: Registry<ListenerInner> = Registry::new();

#[derive(Debug, Clone)]
pub struct ListenerOptions {
    pub protocol: String,
    pub bind: String,
    pub port: u16,
    pub backlog: u32,
    /// IPv4 multicast group to join for a receive-only UDP socket. The daemon
    /// restricts this to the PTP ports 319 and 320.
    pub join_group: String,
}

impl Default for ListenerOptions {
    fn default() -> Self {
        Self {
            protocol: "tcp".to_string(),
            bind: "127.0.0.1".to_string(),
            port: 0,
            backlog: 64,
            join_group: String::new(),
        }
    }
}

impl ListenerOptions {
    pub fn parse(bytes: &[u8]) -> SdkResult<Self> {
        let mut options = Self::default();
        if bytes.is_empty() {
            return Ok(options);
        }
        let value: Value = serde_json::from_slice(bytes).map_err(|err| {
            SdkError::new(
                INVALID_ARGUMENT,
                format!("listener options JSON is not valid: {err}"),
            )
        })?;
        let object = value.as_object().ok_or_else(|| {
            SdkError::new(INVALID_ARGUMENT, "listener options must be a JSON object")
        })?;
        for (key, value) in object {
            match key.as_str() {
                "protocol" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "listener protocol must be a string")
                    })?;
                    if text != "tcp" && text != "udp" {
                        return Err(SdkError::new(
                            INVALID_ARGUMENT,
                            format!("listener protocol {text:?} must be \"tcp\" or \"udp\""),
                        ));
                    }
                    options.protocol = text.to_string();
                }
                "bind" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "listener bind must be a string")
                    })?;
                    if text.is_empty() {
                        return Err(SdkError::new(INVALID_ARGUMENT, "listener bind is empty"));
                    }
                    options.bind = text.to_string();
                }
                "port" => {
                    let port = value.as_u64().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "listener port must be an integer")
                    })?;
                    options.port = u16::try_from(port).map_err(|_| {
                        SdkError::new(INVALID_ARGUMENT, "listener port must be 0..65535")
                    })?;
                }
                "backlog" => {
                    let backlog = value.as_u64().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "listener backlog must be an integer")
                    })?;
                    if !(1..=128).contains(&backlog) {
                        return Err(SdkError::new(
                            INVALID_ARGUMENT,
                            "listener backlog must be 1..128",
                        ));
                    }
                    options.backlog = backlog as u32;
                }
                "join_group" => {
                    // Parsed before validating against the protocol: the JSON
                    // object iterates in sorted key order, so "join_group"
                    // always precedes "protocol" and a check here would reject
                    // every valid multicast request.
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "listener join_group must be a string")
                    })?;
                    options.join_group = text.to_string();
                }
                other => {
                    return Err(SdkError::new(
                        INVALID_ARGUMENT,
                        format!("unknown listener option {other:?}"),
                    ))
                }
            }
        }
        if !options.join_group.is_empty() && options.protocol != "udp" {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "listener join_group requires protocol \"udp\"",
            ));
        }
        Ok(options)
    }

    fn request(&self) -> Value {
        json!({
            "op": "listen",
            "protocol": self.protocol,
            "bind": self.bind,
            "port": self.port,
            "backlog": self.backlog,
            "join_group": self.join_group,
        })
    }
}

enum ListenerSocket {
    Tcp(tokio::net::TcpListener),
    Udp(Arc<tokio::net::UdpSocket>),
}

pub struct ListenerInner {
    protocol: String,
    /// Non-empty for a multicast group receiver. Such a socket joined the group
    /// for receiving only: sending from it would let an app transmit from a
    /// root-bound privileged port, so core_api's listener_send refuses.
    join_group: String,
    /// Daemon allocation id, released with an explicit close request so the
    /// daemon's live-listener accounting follows the app's real lifetime.
    allocation: u64,
    info: Value,
    socket: ListenerSocket,
    closed: AtomicBool,
    closed_tx: watch::Sender<bool>,
    accept_gate: Mutex<()>,
    /// One logical UDP reader: concurrent recv calls are serialized.
    recv_gate: Mutex<()>,
}

impl ListenerInner {
    pub fn protocol(&self) -> &str {
        &self.protocol
    }

    pub fn is_closed(&self) -> bool {
        self.closed.load(Ordering::Acquire)
    }

    pub fn info_json(&self) -> Vec<u8> {
        let mut value = self.info.clone();
        if let Some(object) = value.as_object_mut() {
            object.insert(
                "closed".to_string(),
                Value::Bool(self.closed.load(Ordering::Acquire)),
            );
        }
        serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
    }

    /// Accepts one TCP connection and returns a stream handle plus peer JSON.
    pub async fn accept(&self, timeout_ms: u32) -> SdkResult<(u64, Vec<u8>)> {
        let _gate = self.accept_gate.lock().await;
        let ListenerSocket::Tcp(listener) = &self.socket else {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "listener_accept is only valid for a TCP listener",
            ));
        };
        let (stream, peer) = io_op(&self.closed_tx, timeout_ms, listener.accept()).await?;
        let handle = STREAMS.insert(StreamInner::from_tcp(stream, Some(peer)));
        Ok((handle, udp::peer_json(peer)))
    }

    pub async fn recv(&self, max_len: usize, timeout_ms: u32) -> SdkResult<(Vec<u8>, Vec<u8>)> {
        let _gate = self.recv_gate.lock().await;
        let ListenerSocket::Udp(socket) = &self.socket else {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "listener_recv is only valid for a UDP listener",
            ));
        };
        let len = read_len(max_len, MAX_DATAGRAM, MAX_DATAGRAM)?;
        let mut buf = vec![0u8; len];
        let (read, peer) = io_op(&self.closed_tx, timeout_ms, socket.recv_from(&mut buf)).await?;
        buf.truncate(read);
        Ok((buf, udp::peer_json(peer)))
    }

    pub async fn send(
        &self,
        peer: Option<std::net::SocketAddr>,
        data: &[u8],
        timeout_ms: u32,
    ) -> SdkResult<usize> {
        if !self.join_group.is_empty() {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "a multicast listener is receive-only; listener_send is refused",
            ));
        }
        let ListenerSocket::Udp(socket) = &self.socket else {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "listener_send is only valid for a UDP listener",
            ));
        };
        let Some(peer) = peer else {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "listener_send requires peer_json",
            ));
        };
        if data.len() > MAX_DATAGRAM {
            return Err(SdkError::new(
                crate::ffi::TOO_LARGE,
                format!(
                    "datagram is {} bytes; the cap is {MAX_DATAGRAM}",
                    data.len()
                ),
            ));
        }
        io_op(&self.closed_tx, timeout_ms, socket.send_to(data, peer)).await
    }

    pub async fn close(&self) {
        if self.closed.swap(true, Ordering::AcqRel) {
            return;
        }
        self.closed_tx.send_replace(true);
    }
}

fn extract_info(request: &ListenerOptions, reply: &Value) -> SdkResult<Value> {
    let source = reply.get("info").unwrap_or(reply);
    let protocol = source
        .get("protocol")
        .and_then(Value::as_str)
        .unwrap_or(&request.protocol);
    let address = source
        .get("address")
        .or_else(|| source.get("bind"))
        .and_then(Value::as_str)
        .unwrap_or(&request.bind);
    let port = source
        .get("port")
        .and_then(Value::as_u64)
        .ok_or_else(|| SdkError::new(INTERNAL, "control reply did not report a bound port"))?;
    if port > u64::from(u16::MAX) {
        return Err(SdkError::new(
            INTERNAL,
            "control reply reported an invalid port",
        ));
    }
    Ok(json!({
        "protocol": protocol,
        "address": address,
        "port": port,
    }))
}

/// Asks the daemon for one listener. Returns the handle and the bound JSON.
pub fn open(options_json: &[u8], timeout_ms: u32) -> SdkResult<(u64, Vec<u8>)> {
    let options = ListenerOptions::parse(options_json)?;
    let timeout = if timeout_ms == 0 {
        control::DEFAULT_TIMEOUT
    } else {
        Duration::from_millis(u64::from(timeout_ms))
    };
    let reply = control::request(&options.request(), timeout)?;
    let Some(fd) = reply.fd else {
        return Err(SdkError::new(
            INTERNAL,
            "control reply did not include a listener descriptor",
        ));
    };
    let info = match extract_info(&options, &reply.json) {
        Ok(info) => info,
        Err(err) => {
            control::close_received(fd);
            return Err(err);
        }
    };
    let allocation = reply
        .json
        .get("allocation")
        .and_then(Value::as_u64)
        .unwrap_or(0);
    // Adopt the descriptor immediately: every later failure closes it through
    // the owning std socket instead of leaking it.
    let owned = unsafe { OwnedFd::from_raw_fd(fd) };
    let inner = match options.protocol.as_str() {
        "tcp" => {
            let listener = std::net::TcpListener::from(owned);
            listener.set_nonblocking(true).map_err(|err| {
                SdkError::new(INTERNAL, format!("set listener non-blocking: {err}"))
            })?;
            let listener = crate::runtime::runtime()
                .block_on(async { tokio::net::TcpListener::from_std(listener) })
                .map_err(|err| SdkError::new(INTERNAL, format!("adopt TCP listener: {err}")))?;
            let (closed_tx, _) = watch::channel(false);
            ListenerInner {
                protocol: "tcp".to_string(),
                join_group: String::new(),
                allocation,
                info,
                socket: ListenerSocket::Tcp(listener),
                closed: AtomicBool::new(false),
                closed_tx,
                accept_gate: Mutex::new(()),
                recv_gate: Mutex::new(()),
            }
        }
        "udp" => {
            let socket = std::net::UdpSocket::from(owned);
            socket.set_nonblocking(true).map_err(|err| {
                SdkError::new(INTERNAL, format!("set listener non-blocking: {err}"))
            })?;
            let socket = crate::runtime::runtime()
                .block_on(async { tokio::net::UdpSocket::from_std(socket) })
                .map_err(|err| SdkError::new(INTERNAL, format!("adopt UDP listener: {err}")))?;
            let (closed_tx, _) = watch::channel(false);
            ListenerInner {
                protocol: "udp".to_string(),
                join_group: options.join_group.clone(),
                allocation,
                info,
                socket: ListenerSocket::Udp(Arc::new(socket)),
                closed: AtomicBool::new(false),
                closed_tx,
                accept_gate: Mutex::new(()),
                recv_gate: Mutex::new(()),
            }
        }
        _ => unreachable!("listener protocol was validated"),
    };
    let info_json = inner.info_json();
    let handle = LISTENERS.insert(Arc::new(inner));
    Ok((handle, info_json))
}

pub fn listener(handle: u64) -> SdkResult<Arc<ListenerInner>> {
    LISTENERS
        .get(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown listener handle"))
}

/// Releases a listener. Pending accept/recv/send return CANCELLED and the
/// daemon-owned socket is closed once no operation still references it.
pub fn close(handle: u64) -> SdkResult<()> {
    let inner = LISTENERS
        .remove(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown listener handle"))?;
    if inner.allocation != 0 {
        // Release the daemon's live-listener accounting. Best effort and
        // bounded: a daemon that is gone must not stop the socket from
        // closing, and the daemon treats an unknown allocation as a no-op.
        let request = serde_json::json!({
            "id": 0,
            "op": "close",
            "allocation": inner.allocation,
        });
        let _ = control::request(&request, Duration::from_millis(250));
    }
    if tokio::runtime::Handle::try_current().is_ok() {
        crate::runtime::runtime().spawn(async move { inner.close().await });
    } else {
        crate::runtime::runtime().block_on(inner.close());
    }
    Ok(())
}


#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn join_group_parses_before_protocol_validation() {
        // serde_json::Map iterates in sorted key order, so join_group is seen
        // before protocol; the parse must not depend on that order.
        let options = ListenerOptions::parse(
            br#"{"protocol":"udp","bind":"0.0.0.0","port":320,"join_group":"224.0.0.1"}"#,
        )
        .expect("a multicast listener request must parse");
        assert_eq!(options.protocol, "udp");
        assert_eq!(options.join_group, "224.0.0.1");
        assert_eq!(options.port, 320);
    }

    #[test]
    fn join_group_rejects_non_udp_and_keeps_the_default_protocol() {
        assert!(ListenerOptions::parse(
            br#"{"protocol":"tcp","port":320,"join_group":"224.0.0.1"}"#
        )
        .is_err());
        // No protocol key: the default is tcp, so a group join is refused.
        assert!(ListenerOptions::parse(br#"{"join_group":"224.0.0.1","port":320}"#).is_err());
    }

    #[test]
    fn join_group_empty_is_an_ordinary_listener() {
        let options = ListenerOptions::parse(br#"{"protocol":"udp","port":0}"#)
            .expect("ordinary listener");
        assert!(options.join_group.is_empty());
    }
}
