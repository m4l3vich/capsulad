//! Daemon-mediated TCP/UDP listeners.
//!
//! The daemon binds the socket and passes the descriptor over the inherited
//! `CAPSULAD_CONTROL_FD` channel; the app never creates a listening socket. A
//! runtime without that channel reports `UNSUPPORTED`.

use std::sync::Arc;
use std::time::Duration;

use serde_json::{Map, Value};

use crate::error::{self, Error, Result};
use crate::net::{parse_listener_info, parse_peer, ListenerInfoParts, PeerAddr, Stream};
use crate::sys::{self, Buf};
use crate::{status_error, take_buf, timeout_ms, wait_ms, Inner, Library};

/// Options for [`Listener::open`]. The zero value asks for TCP on
/// `127.0.0.1:0` with the native backlog default (64).
#[derive(Debug, Clone, Default)]
pub struct ListenerOptions {
    /// `"tcp"` or `"udp"`; `None` uses `"tcp"`.
    pub protocol: Option<String>,
    /// Local unicast bind address; `None` uses `127.0.0.1`.
    pub bind: Option<String>,
    /// `Some(0)` (or `None`) requests an ephemeral port; explicit ports must be
    /// 1024..65535 and 5353/the Web UI port are refused by the daemon.
    pub port: Option<u16>,
    /// TCP backlog, 1..128; `None` uses 64.
    pub backlog: Option<u32>,
}

impl ListenerOptions {
    fn encode(&self) -> Result<String> {
        let mut map = Map::new();
        if let Some(protocol) = &self.protocol {
            if !matches!(protocol.as_str(), "tcp" | "udp") {
                return Err(Error::local(
                    error::CODE_INVALID_ARGUMENT,
                    format!("protocol {protocol:?} must be \"tcp\" or \"udp\""),
                ));
            }
            map.insert("protocol".into(), Value::String(protocol.clone()));
        }
        if let Some(bind) = &self.bind {
            map.insert("bind".into(), Value::String(bind.clone()));
        }
        if let Some(port) = self.port {
            map.insert("port".into(), Value::from(port));
        }
        if let Some(backlog) = self.backlog {
            if !(1..=128).contains(&backlog) {
                return Err(Error::local(
                    error::CODE_INVALID_ARGUMENT,
                    "backlog must be 1..128",
                ));
            }
            map.insert("backlog".into(), Value::from(backlog));
        }
        Ok(Value::Object(map).to_string())
    }
}

/// The bound address reported by the daemon plus the local closed flag.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ListenerInfo {
    pub protocol: String,
    pub address: String,
    /// The actual bound port, so an ephemeral request is resolved here.
    pub port: u16,
    pub closed: bool,
}

/// One daemon-allocated listener. Dropping it closes the native handle.
pub struct Listener {
    inner: Arc<Inner>,
    handle: u64,
}

impl Listener {
    /// Asks the daemon for a bound listener. `timeout` bounds the control
    /// exchange (`None` uses the native default).
    pub fn open(
        library: &Library,
        options: &ListenerOptions,
        timeout: Option<Duration>,
    ) -> Result<Self> {
        let encoded = options.encode()?;
        let mut handle = 0u64;
        let mut info = Buf::default();
        let status = unsafe {
            (library.inner.api.listener_open)(
                encoded.as_ptr(),
                encoded.len(),
                timeout_ms(timeout),
                &mut handle,
                &mut info,
            )
        };
        if status != sys::STATUS_OK {
            let err = status_error(&library.inner, status, "listener_open");
            unsafe { take_buf(&library.inner, info) };
            return Err(err);
        }
        // The bound info is available through info(); release the open reply.
        unsafe { take_buf(&library.inner, info) };
        if handle == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "listener_open returned a zero handle",
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

    /// Accepts one TCP connection. A `TIMEOUT` error leaves the listener
    /// usable.
    pub fn accept(&self, timeout: Option<Duration>) -> Result<(Stream, PeerAddr)> {
        let mut stream = 0u64;
        let mut peer = Buf::default();
        let status = unsafe {
            (self.inner.api.listener_accept)(self.handle, wait_ms(timeout), &mut stream, &mut peer)
        };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "listener_accept");
            unsafe { take_buf(&self.inner, peer) };
            return Err(err);
        }
        let peer_bytes = unsafe { take_buf(&self.inner, peer) };
        let peer = match parse_peer(&peer_bytes) {
            Ok(peer) => peer,
            Err(err) => {
                if stream != 0 {
                    unsafe { (self.inner.api.stream_close)(stream) };
                }
                return Err(err);
            }
        };
        if stream == 0 {
            return Err(Error::local(
                sys::STATUS_INTERNAL,
                "listener_accept returned a zero stream handle",
            ));
        }
        Ok((Stream::from_handle(self.inner.clone(), stream), peer))
    }

    /// Receives one UDP datagram and its peer.
    pub fn recv(&self, max_len: usize, timeout: Option<Duration>) -> Result<(Vec<u8>, PeerAddr)> {
        let mut data = Buf::default();
        let mut peer = Buf::default();
        let status = unsafe {
            (self.inner.api.listener_recv)(
                self.handle,
                max_len,
                wait_ms(timeout),
                &mut data,
                &mut peer,
            )
        };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "listener_recv");
            unsafe {
                take_buf(&self.inner, data);
                take_buf(&self.inner, peer);
            }
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, data) };
        let peer_bytes = unsafe { take_buf(&self.inner, peer) };
        Ok((bytes, parse_peer(&peer_bytes)?))
    }

    /// Sends one datagram to `peer` (listeners have no connected peer).
    pub fn send(&self, peer: &PeerAddr, data: &[u8], timeout: Option<Duration>) -> Result<usize> {
        let peer_json = peer.to_json().to_string();
        let status = unsafe {
            (self.inner.api.listener_send)(
                self.handle,
                peer_json.as_ptr(),
                peer_json.len(),
                data.as_ptr(),
                data.len(),
                wait_ms(timeout),
            )
        };
        crate::check_status(&self.inner, status, "listener_send")?;
        Ok(data.len())
    }

    /// The bound address and closed flag.
    pub fn info(&self) -> Result<ListenerInfo> {
        let mut buf = Buf::default();
        let status = unsafe { (self.inner.api.listener_info)(self.handle, &mut buf) };
        if status != sys::STATUS_OK {
            let err = status_error(&self.inner, status, "listener_info");
            unsafe { take_buf(&self.inner, buf) };
            return Err(err);
        }
        let bytes = unsafe { take_buf(&self.inner, buf) };
        let ListenerInfoParts {
            protocol,
            address,
            port,
            closed,
        } = parse_listener_info(&bytes)?;
        Ok(ListenerInfo {
            protocol,
            address,
            port,
            closed,
        })
    }

    /// Releases the handle and unblocks pending accept/recv/send. Idempotent;
    /// also runs on drop.
    pub fn close(&mut self) -> Result<()> {
        if self.handle == 0 {
            return Ok(());
        }
        let handle = self.handle;
        self.handle = 0;
        let status = unsafe { (self.inner.api.listener_close)(handle) };
        crate::check_status(&self.inner, status, "listener_close")
    }
}

impl Drop for Listener {
    fn drop(&mut self) {
        let _ = self.close();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn encoded(options: &ListenerOptions) -> Value {
        serde_json::from_str(&options.encode().expect("options encode")).expect("valid JSON")
    }

    #[test]
    fn listener_options_omit_defaults() {
        assert_eq!(encoded(&ListenerOptions::default()), serde_json::json!({}));
        let value = encoded(&ListenerOptions {
            protocol: Some("udp".into()),
            bind: Some("0.0.0.0".into()),
            port: Some(0),
            backlog: Some(64),
        });
        assert_eq!(value["protocol"], "udp");
        assert_eq!(value["bind"], "0.0.0.0");
        assert_eq!(value["port"], 0);
        assert_eq!(value["backlog"], 64);
    }

    #[test]
    fn invalid_listener_options_are_rejected() {
        assert!(ListenerOptions {
            protocol: Some("sctp".into()),
            ..ListenerOptions::default()
        }
        .encode()
        .is_err());
        assert!(ListenerOptions {
            backlog: Some(129),
            ..ListenerOptions::default()
        }
        .encode()
        .is_err());
    }
}
