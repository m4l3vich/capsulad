//! Outbound TCP/TLS streams for application protocols.
//!
//! One handle is one connection. `tcp_open` performs DNS, TCP and the optional
//! TLS handshake under a single deadline; TLS always verifies (no insecure
//! mode) and reuses [`crate::tls`]. The connection is split into independent
//! read and write halves (`tokio::io::split`), each behind its own mutex and
//! the read half behind a one-reader gate: a thread parked in
//! `stream_read(0xFFFFFFFF)` never blocks a concurrent `stream_write` or
//! `stream_shutdown(WRITE)`. Every wait races a broadcast close signal, which
//! is what makes `stream_close` unblock both halves; taking and dropping the
//! halves closes the underlying socket.

use std::future::Future;
use std::io;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serde_json::Value;
use tokio::io::{AsyncReadExt, AsyncWriteExt, ReadHalf, WriteHalf};
use tokio::sync::{watch, Mutex};

use crate::ffi::{
    SdkError, SdkResult, CANCELLED, CLOSED, END_OF_STREAM, INVALID_ARGUMENT, TIMEOUT, TOO_LARGE,
    TRANSPORT,
};
use crate::registry::Registry;
use crate::tls::{self, NetStream, RootsMode, TlsOptions};

pub const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(30);
pub const DEFAULT_READ_LEN: usize = 64 * 1024;
pub const MAX_READ_LEN: usize = 4 * 1024 * 1024;

pub const SHUTDOWN_WRITE: i32 = 0;
pub const SHUTDOWN_READ: i32 = 1;
pub const SHUTDOWN_BOTH: i32 = 2;

pub static STREAMS: Registry<StreamInner> = Registry::new();

/// Streams accept nothing but the documented option keys; a mistyped TLS
/// option must not silently weaken verification.
#[derive(Debug, Clone)]
pub struct StreamOptions {
    pub tls: bool,
    pub roots: RootsMode,
    pub ca_pem: Option<Vec<u8>>,
    pub server_name: Option<String>,
    pub nodelay: bool,
}

impl Default for StreamOptions {
    fn default() -> Self {
        Self {
            tls: false,
            roots: RootsMode::System,
            ca_pem: None,
            server_name: None,
            nodelay: true,
        }
    }
}

impl StreamOptions {
    pub fn parse(bytes: &[u8]) -> SdkResult<Self> {
        let mut options = Self::default();
        if bytes.is_empty() {
            return Ok(options);
        }
        let value: Value = serde_json::from_slice(bytes).map_err(|err| {
            SdkError::new(
                INVALID_ARGUMENT,
                format!("stream options JSON is not valid: {err}"),
            )
        })?;
        let object = value.as_object().ok_or_else(|| {
            SdkError::new(INVALID_ARGUMENT, "stream options must be a JSON object")
        })?;
        for (key, value) in object {
            match key.as_str() {
                "tls" => {
                    options.tls = value.as_bool().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "stream tls must be a boolean")
                    })?
                }
                "roots" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "stream roots must be a string")
                    })?;
                    options.roots = match text {
                        "system" => RootsMode::System,
                        "webpki" => RootsMode::Webpki,
                        "pem" => RootsMode::Pem,
                        other => {
                            return Err(SdkError::new(
                                INVALID_ARGUMENT,
                                format!(
                                    "unknown stream roots {other:?}; want system, webpki or pem"
                                ),
                            ))
                        }
                    };
                }
                "ca_pem" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "stream ca_pem must be a string")
                    })?;
                    if !text.is_empty() {
                        options.ca_pem = Some(text.as_bytes().to_vec());
                    }
                }
                "server_name" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "stream server_name must be a string")
                    })?;
                    if text.is_empty() {
                        return Err(SdkError::new(
                            INVALID_ARGUMENT,
                            "stream server_name must not be empty",
                        ));
                    }
                    options.server_name = Some(text.to_string());
                }
                "nodelay" => {
                    options.nodelay = value.as_bool().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "stream nodelay must be a boolean")
                    })?
                }
                other => {
                    return Err(SdkError::new(
                        INVALID_ARGUMENT,
                        format!("unknown stream option {other:?}"),
                    ))
                }
            }
        }
        Ok(options)
    }
}

pub struct StreamInner {
    host: String,
    port: u16,
    tls: bool,
    server_name: Option<String>,
    peer: Option<String>,
    /// Independent halves: a blocked read holds only `read`, so writes and
    /// `SHUTDOWN_WRITE` proceed while a reader is parked on an idle stream.
    read: Mutex<Option<ReadHalf<NetStream>>>,
    write: Mutex<Option<WriteHalf<NetStream>>>,
    read_gate: Mutex<()>,
    closed: AtomicBool,
    closed_tx: watch::Sender<bool>,
    read_stop_tx: watch::Sender<bool>,
    read_shutdown: AtomicBool,
    write_shutdown: AtomicBool,
}

impl StreamInner {
    pub fn new(
        host: String,
        port: u16,
        tls: bool,
        server_name: Option<String>,
        stream: NetStream,
    ) -> Arc<Self> {
        Self::from_stream(host, port, tls, server_name, None, stream)
    }

    /// Wraps an accepted TCP connection. Listener-owned sockets are already
    /// connected; the peer is recorded for diagnostics.
    pub fn from_tcp(
        stream: tokio::net::TcpStream,
        peer: Option<std::net::SocketAddr>,
    ) -> Arc<Self> {
        let _ = stream.set_nodelay(true);
        let host = peer
            .map(|address| address.ip().to_string())
            .unwrap_or_default();
        let port = peer.map(|address| address.port()).unwrap_or(0);
        Self::from_stream(
            host,
            port,
            false,
            None,
            peer.map(|address| address.to_string()),
            NetStream::Plain(stream),
        )
    }

    fn from_stream(
        host: String,
        port: u16,
        tls: bool,
        server_name: Option<String>,
        peer: Option<String>,
        stream: NetStream,
    ) -> Arc<Self> {
        // Both halves share the socket through a lock that is released on
        // every Pending poll, so a parked read cannot starve a write.
        let (read, write) = tokio::io::split(stream);
        let (closed_tx, _) = watch::channel(false);
        let (read_stop_tx, _) = watch::channel(false);
        Arc::new(Self {
            host,
            port,
            tls,
            server_name,
            peer,
            read: Mutex::new(Some(read)),
            write: Mutex::new(Some(write)),
            read_gate: Mutex::new(()),
            closed: AtomicBool::new(false),
            closed_tx,
            read_stop_tx,
            read_shutdown: AtomicBool::new(false),
            write_shutdown: AtomicBool::new(false),
        })
    }

    pub fn tls(&self) -> bool {
        self.tls
    }

    pub fn is_closed(&self) -> bool {
        self.closed.load(Ordering::Acquire)
    }

    /// Reads at most `max_len` bytes. A clean EOF is END_OF_STREAM.
    pub async fn read(&self, max_len: usize, timeout_ms: u32) -> SdkResult<Vec<u8>> {
        let max_len = read_len(max_len, DEFAULT_READ_LEN, MAX_READ_LEN)?;
        let _gate = self.read_gate.lock().await;
        if self.read_shutdown.load(Ordering::Acquire) {
            return Err(SdkError::new(
                END_OF_STREAM,
                "stream read side is shut down",
            ));
        }
        let mut guard = self.read.lock().await;
        if self.closed.load(Ordering::Acquire) {
            return Err(closed_error());
        }
        let Some(stream) = guard.as_mut() else {
            return Err(closed_error());
        };
        let mut buf = vec![0u8; max_len];
        let read = read_op(
            &self.closed_tx,
            &self.read_stop_tx,
            timeout_ms,
            stream.read(&mut buf),
        )
        .await?;
        if read == 0 {
            return Err(SdkError::new(END_OF_STREAM, "stream ended"));
        }
        buf.truncate(read);
        Ok(buf)
    }

    /// Writes once; the result may be a partial write.
    pub async fn write(&self, data: &[u8], timeout_ms: u32) -> SdkResult<usize> {
        if data.is_empty() {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "stream_write data is empty",
            ));
        }
        if self.write_shutdown.load(Ordering::Acquire) {
            return Err(SdkError::new(CLOSED, "stream write side is shut down"));
        }
        let mut guard = self.write.lock().await;
        if self.closed.load(Ordering::Acquire) {
            return Err(closed_error());
        }
        let Some(stream) = guard.as_mut() else {
            return Err(closed_error());
        };
        let written = io_op(&self.closed_tx, timeout_ms, stream.write(data)).await?;
        if written == 0 {
            return Err(SdkError::new(TRANSPORT, "stream write made no progress"));
        }
        Ok(written)
    }

    pub async fn shutdown(&self, how: i32) -> SdkResult<()> {
        if !matches!(how, SHUTDOWN_WRITE | SHUTDOWN_READ | SHUTDOWN_BOTH) {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "stream shutdown how must be WRITE (0), READ (1) or BOTH (2)",
            ));
        }
        if matches!(how, SHUTDOWN_READ | SHUTDOWN_BOTH) {
            self.read_shutdown.store(true, Ordering::Release);
            // Wake a read parked in select without closing the handle; it
            // observes END_OF_STREAM and releases the read half. This never
            // touches the write half.
            self.read_stop_tx.send_replace(true);
        }
        if matches!(how, SHUTDOWN_WRITE | SHUTDOWN_BOTH) {
            if self.write_shutdown.swap(true, Ordering::AcqRel) {
                return Ok(());
            }
            // Only the write half is locked: a reader parked on an idle
            // stream must not delay the FIN/close_notify.
            let mut guard = self.write.lock().await;
            if self.closed.load(Ordering::Acquire) {
                return Ok(());
            }
            if let Some(stream) = guard.as_mut() {
                // AsyncWrite::shutdown flushes TLS records and sends
                // close_notify before the TCP half-close. Reads continue on
                // the other half until the peer closes.
                let _ = tokio::time::timeout(
                    Duration::from_secs(5),
                    io_op(&self.closed_tx, u32::MAX, stream.shutdown()),
                )
                .await;
            }
        }
        Ok(())
    }

    /// Broadcasts closed, then takes both halves and shuts the write side
    /// down. A pending read/write observes the broadcast first, drops its
    /// future and releases its mutex, so this always completes.
    pub async fn close(&self) {
        if self.closed.swap(true, Ordering::AcqRel) {
            return;
        }
        self.closed_tx.send_replace(true);
        // Take the read half first so no reader can re-enter; the broadcast
        // has already woken any parked read.
        {
            let mut read = self.read.lock().await;
            read.take();
        }
        let mut write = self.write.lock().await;
        if let Some(mut stream) = write.take() {
            let _ = tokio::time::timeout(Duration::from_secs(5), stream.shutdown()).await;
        }
    }

    pub fn info_json(&self) -> Vec<u8> {
        let value = serde_json::json!({
            "kind": "stream",
            "host": self.host,
            "port": self.port,
            "tls": self.tls,
            "server_name": self.server_name.clone().unwrap_or_default(),
            "peer": self.peer.clone().unwrap_or_default(),
            "closed": self.closed.load(Ordering::Acquire),
            "read_shutdown": self.read_shutdown.load(Ordering::Acquire),
            "write_shutdown": self.write_shutdown.load(Ordering::Acquire),
        });
        serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
    }
}

pub fn open(host: &str, port: u16, options_json: &[u8], timeout_ms: u32) -> SdkResult<u64> {
    if host.is_empty() {
        return Err(SdkError::new(INVALID_ARGUMENT, "tcp_open host is empty"));
    }
    if host.len() > 255 {
        return Err(SdkError::new(INVALID_ARGUMENT, "tcp_open host is too long"));
    }
    if port == 0 {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "tcp_open port must be 1..65535",
        ));
    }
    let options = StreamOptions::parse(options_json)?;
    let tls_config = if options.tls {
        Some(tls::client_config(
            &TlsOptions {
                roots: options.roots,
                ca_pem: options.ca_pem.clone(),
            },
            false,
        )?)
    } else {
        None
    };
    let deadline = if timeout_ms == 0 {
        DEFAULT_CONNECT_TIMEOUT
    } else {
        Duration::from_millis(u64::from(timeout_ms))
    };
    let server_name = options
        .server_name
        .clone()
        .unwrap_or_else(|| host.to_string());
    let stream = crate::runtime::runtime().block_on(tls::connect(
        host,
        port,
        tls_config,
        options.server_name.as_deref(),
        deadline,
    ))?;
    if let Err(err) = set_nodelay(&stream, options.nodelay) {
        return Err(SdkError::new(TRANSPORT, format!("set TCP_NODELAY: {err}")));
    }
    let inner = StreamInner::new(
        host.to_string(),
        port,
        options.tls,
        if options.tls { Some(server_name) } else { None },
        stream,
    );
    Ok(STREAMS.insert(inner))
}

fn set_nodelay(stream: &NetStream, enabled: bool) -> io::Result<()> {
    match stream {
        NetStream::Plain(tcp) => tcp.set_nodelay(enabled),
        NetStream::Tls(tls) => tls.get_ref().0.set_nodelay(enabled),
    }
}

/// Closes and releases one stream handle. Safe to call from any application
/// thread; a pending read/write on that handle returns CANCELLED.
pub fn close(handle: u64) -> SdkResult<()> {
    let inner = STREAMS
        .remove(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown stream handle"))?;
    if tokio::runtime::Handle::try_current().is_ok() {
        crate::runtime::runtime().spawn(async move { inner.close().await });
    } else {
        crate::runtime::runtime().block_on(inner.close());
    }
    Ok(())
}

pub fn stream_handle(handle: u64) -> SdkResult<Arc<StreamInner>> {
    STREAMS
        .get(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown stream handle"))
}

/* ------------------------------------------------------------------ *
 * Shared timeout/close primitives (also used by UDP and listeners)
 * ------------------------------------------------------------------ */

pub(crate) fn closed_error() -> SdkError {
    SdkError::new(CLOSED, "handle is closed")
}

pub(crate) fn cancelled_error() -> SdkError {
    SdkError::new(CANCELLED, "operation cancelled by close")
}

pub(crate) fn read_stopped_error() -> SdkError {
    SdkError::new(END_OF_STREAM, "stream read side is shut down")
}

fn timeout_error(timeout_ms: u32) -> SdkError {
    if timeout_ms == 0 {
        SdkError::new(TIMEOUT, "operation timed out (non-blocking poll)")
    } else {
        SdkError::new(
            TIMEOUT,
            format!("operation timed out after {timeout_ms} ms"),
        )
    }
}

/// Applies the ABI timeout convention: 0 polls once, 0xFFFFFFFF waits
/// indefinitely, anything else bounds the future in milliseconds.
pub(crate) async fn bounded<F: Future>(timeout_ms: u32, future: F) -> SdkResult<F::Output> {
    if timeout_ms == u32::MAX {
        return Ok(future.await);
    }
    if timeout_ms == 0 {
        return tokio::select! {
            biased;
            value = future => Ok(value),
            _ = tokio::time::sleep(Duration::ZERO) => Err(timeout_error(0)),
        };
    }
    tokio::time::timeout(Duration::from_millis(u64::from(timeout_ms)), future)
        .await
        .map_err(|_| timeout_error(timeout_ms))
}

/// Runs one read, aborting with END_OF_STREAM when the read side is shut down
/// and CANCELLED when the whole handle is closed.
pub(crate) async fn read_op<T, F>(
    closed: &watch::Sender<bool>,
    read_stop: &watch::Sender<bool>,
    timeout_ms: u32,
    future: F,
) -> SdkResult<T>
where
    F: Future<Output = io::Result<T>>,
{
    let mut closed_rx = closed.subscribe();
    if *closed_rx.borrow() {
        return Err(cancelled_error());
    }
    let mut stop_rx = read_stop.subscribe();
    if *stop_rx.borrow() {
        return Err(read_stopped_error());
    }
    let mapped = async move {
        future
            .await
            .map_err(|err| SdkError::new(TRANSPORT, err.to_string()))
    };
    tokio::select! {
        biased;
        _ = closed_rx.changed() => Err(cancelled_error()),
        _ = stop_rx.changed() => Err(read_stopped_error()),
        result = bounded(timeout_ms, mapped) => result?,
    }
}

/// Runs one I/O future, aborting with CANCELLED when the handle is closed.
pub(crate) async fn io_op<T, F>(
    closed: &watch::Sender<bool>,
    timeout_ms: u32,
    future: F,
) -> SdkResult<T>
where
    F: Future<Output = io::Result<T>>,
{
    let mut receiver = closed.subscribe();
    if *receiver.borrow() {
        return Err(cancelled_error());
    }
    let mapped = async move {
        future
            .await
            .map_err(|err| SdkError::new(TRANSPORT, err.to_string()))
    };
    tokio::select! {
        biased;
        _ = receiver.changed() => Err(cancelled_error()),
        result = bounded(timeout_ms, mapped) => result?,
    }
}

pub(crate) fn read_len(requested: usize, default: usize, cap: usize) -> SdkResult<usize> {
    let len = if requested == 0 { default } else { requested };
    if len > cap {
        return Err(SdkError::new(
            TOO_LARGE,
            format!("read length {len} exceeds the {cap}-byte cap"),
        ));
    }
    Ok(len)
}
