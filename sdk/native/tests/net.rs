//! Conformance for the appended ABI 1.0 networking surface: TCP/TLS streams, UDP,
//! daemon-mediated listeners (real `SOCK_SEQPACKET` control channel with
//! `SCM_RIGHTS`) and mDNS argument validation.
//!
//! Everything binds loopback only; no external network is touched.

use std::ffi::CStr;
use std::io::{Read, Write};
use std::net::{SocketAddr, TcpListener as StdTcpListener, TcpStream, UdpSocket as StdUdpSocket};
use std::os::fd::{AsRawFd, RawFd};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use capsulad::api::CapsuladApi;
use capsulad::ffi::{
    CANCELLED, END_OF_STREAM, INTERNAL, INVALID_ARGUMENT, NOT_FOUND, TIMEOUT, TRANSPORT,
    UNSUPPORTED,
};
use capsulad::stream::{self, SHUTDOWN_BOTH, SHUTDOWN_READ, SHUTDOWN_WRITE};
use capsulad::{control, listener, mdns, udp};
use rustls::pki_types::{pem::PemObject, CertificateDer, PrivateKeyDer};
use serde_json::{json, Value};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

const SERVER_PEM: &[u8] = include_bytes!("../testdata/server.pem");
const SERVER_KEY_PEM: &[u8] = include_bytes!("../testdata/server-key.pem");
const CA_PEM: &[u8] = include_bytes!("../testdata/ca.pem");

fn runtime() -> &'static tokio::runtime::Runtime {
    capsulad::runtime::runtime()
}

/// Environment mutation is process-wide; every control-channel test serializes
/// on this lock and restores the variable.
static ENV_LOCK: Mutex<()> = Mutex::new(());

/* ------------------------------------------------------------------ *
 * ABI identity
 * ------------------------------------------------------------------ */

#[test]
fn api_reports_v1_0_and_old_tables_still_load() {
    assert_eq!(capsulad::api::capsulad_abi_major(), 1);
    assert_eq!(capsulad::api::capsulad_abi_minor(), 0);
    let version = unsafe { CStr::from_ptr(capsulad::api::capsulad_version()) };
    assert_eq!(version.to_str().unwrap(), "1.0.0");

    let table = &capsulad::api::API;
    // Every appended entry is populated.
    assert!(table.tcp_open as usize != 0);
    assert!(table.stream_read as usize != 0);
    assert!(table.stream_write as usize != 0);
    assert!(table.stream_shutdown as usize != 0);
    assert!(table.stream_close as usize != 0);
    assert!(table.stream_info as usize != 0);
    assert!(table.udp_open as usize != 0);
    assert!(table.udp_send as usize != 0);
    assert!(table.udp_recv as usize != 0);
    assert!(table.udp_close as usize != 0);
    assert!(table.listener_open as usize != 0);
    assert!(table.listener_accept as usize != 0);
    assert!(table.listener_recv as usize != 0);
    assert!(table.listener_send as usize != 0);
    assert!(table.listener_info as usize != 0);
    assert!(table.listener_close as usize != 0);
    assert!(table.mdns_publish as usize != 0);
    assert!(table.mdns_unpublish as usize != 0);
    assert!(table.mdns_browse_open as usize != 0);

    // A caller whose table ends after `http_cancel` still copies cleanly.
    let v1_0_size = std::mem::offset_of!(CapsuladApi, tcp_open);
    let mut old = vec![0u64; v1_0_size.div_ceil(8)];
    let status =
        capsulad::api::capsulad_get_api(1, v1_0_size, old.as_mut_ptr() as *mut CapsuladApi);
    assert_eq!(status, 0);
    let copied = unsafe { &*(old.as_ptr() as *const CapsuladApi) };
    assert_eq!(copied.abi_minor, 0);
    assert!(copied.http_cancel as usize != 0);
}

/* ------------------------------------------------------------------ *
 * TCP streams
 * ------------------------------------------------------------------ */

fn spawn_one_shot_tcp(
    response: &'static [u8],
    shutdown_after: bool,
) -> (SocketAddr, std::thread::JoinHandle<Vec<u8>>) {
    let (addr_tx, addr_rx) = std::sync::mpsc::channel();
    let handle = std::thread::spawn(move || {
        let listener = StdTcpListener::bind("127.0.0.1:0").unwrap();
        addr_tx.send(listener.local_addr().unwrap()).unwrap();
        let (mut socket, _) = listener.accept().unwrap();
        let mut request = Vec::new();
        let mut chunk = [0u8; 1024];
        loop {
            match socket.read(&mut chunk) {
                Ok(0) => break,
                Ok(read) => {
                    request.extend_from_slice(&chunk[..read]);
                    if request.len() >= 4 {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
        let _ = socket.write_all(response);
        if shutdown_after {
            let _ = socket.shutdown(std::net::Shutdown::Write);
        }
        request
    });
    (addr_rx.recv().unwrap(), handle)
}

#[test]
fn tcp_round_trip_eof_and_shutdown() {
    let (addr, server) = spawn_one_shot_tcp(b"pong", true);
    let handle = stream::open("127.0.0.1", addr.port(), b"", 5_000).expect("open");
    let inner = stream::stream_handle(handle).unwrap();

    // Empty writes are invalid: a zero written count is only an error.
    assert_eq!(
        runtime()
            .block_on(inner.write(b"", 5_000))
            .unwrap_err()
            .status,
        INVALID_ARGUMENT
    );
    let written = runtime()
        .block_on(inner.write(b"ping", 5_000))
        .expect("write");
    assert_eq!(written, 4);
    assert_eq!(server.join().unwrap(), b"ping");

    assert_eq!(
        runtime().block_on(inner.read(0, 5_000)).expect("read"),
        b"pong"
    );
    let eof = runtime()
        .block_on(inner.read(0, 5_000))
        .expect_err("clean EOF");
    assert_eq!(eof.status, END_OF_STREAM);

    // A read shutdown is local and idempotent; a write shutdown refuses writes.
    runtime().block_on(inner.shutdown(SHUTDOWN_READ)).unwrap();
    assert_eq!(
        runtime().block_on(inner.read(0, 5_000)).unwrap_err().status,
        END_OF_STREAM
    );
    runtime().block_on(inner.shutdown(SHUTDOWN_WRITE)).unwrap();
    assert_eq!(
        runtime()
            .block_on(inner.write(b"x", 5_000))
            .unwrap_err()
            .status,
        capsulad::ffi::CLOSED
    );
    stream::close(handle).unwrap();
}

#[test]
fn c_abi_stream_functions_round_trip() {
    let (addr, server) = spawn_one_shot_tcp(b"pong", true);
    let host = b"127.0.0.1";
    let mut handle = 0u64;
    let status = unsafe {
        (capsulad::api::API.tcp_open)(
            host.as_ptr() as *const std::ffi::c_char,
            host.len(),
            addr.port(),
            std::ptr::null(),
            0,
            5_000,
            &mut handle,
        )
    };
    assert_eq!(status, 0);
    assert_ne!(handle, 0);

    let mut written = 0usize;
    let status =
        unsafe { (capsulad::api::API.stream_write)(handle, b"".as_ptr(), 0, 5_000, &mut written) };
    assert_eq!(status, INVALID_ARGUMENT);
    let status = unsafe {
        (capsulad::api::API.stream_write)(handle, b"ping".as_ptr(), 4, 5_000, &mut written)
    };
    assert_eq!(status, 0);
    assert_eq!(written, 4);
    assert_eq!(server.join().unwrap(), b"ping");

    let mut out = capsulad::ffi::Buf::default();
    let status = unsafe { (capsulad::api::API.stream_read)(handle, 0, 5_000, &mut out) };
    assert_eq!(status, 0);
    let data = unsafe { std::slice::from_raw_parts(out.data, out.len) }.to_vec();
    unsafe { capsulad::ffi::free_buf(out) }.unwrap();
    assert_eq!(data, b"pong");

    // A clean EOF reports END_OF_STREAM and writes the documented empty buffer
    // over whatever the caller passed in.
    let mut empty = capsulad::ffi::Buf {
        data: std::ptr::NonNull::<u8>::dangling().as_ptr(),
        len: 7,
    };
    let status = unsafe { (capsulad::api::API.stream_read)(handle, 0, 5_000, &mut empty) };
    assert_eq!(status, END_OF_STREAM);
    assert!(empty.data.is_null());
    assert_eq!(empty.len, 0);

    let mut info_buf = capsulad::ffi::Buf::default();
    let status = unsafe { (capsulad::api::API.stream_info)(handle, &mut info_buf) };
    assert_eq!(status, 0);
    let info: Value =
        serde_json::from_slice(unsafe { std::slice::from_raw_parts(info_buf.data, info_buf.len) })
            .unwrap();
    unsafe { capsulad::ffi::free_buf(info_buf) }.unwrap();
    assert_eq!(info["kind"], "stream");
    assert_eq!(info["port"], addr.port());

    assert_eq!(unsafe { (capsulad::api::API.stream_close)(handle) }, 0);
}

#[test]
fn tcp_timeout_leaves_stream_usable_and_close_unblocks() {
    let (addr_tx, addr_rx) = std::sync::mpsc::channel();
    let (release_tx, release_rx) = std::sync::mpsc::channel::<()>();
    let server = std::thread::spawn(move || {
        let listener = StdTcpListener::bind("127.0.0.1:0").unwrap();
        addr_tx.send(listener.local_addr().unwrap()).unwrap();
        let (mut socket, _) = listener.accept().unwrap();
        let _ = release_rx.recv();
        let _ = socket.write_all(b"late");
        let _ = socket.shutdown(std::net::Shutdown::Write);
    });
    let addr = addr_rx.recv().unwrap();
    let handle = stream::open("127.0.0.1", addr.port(), b"", 5_000).unwrap();
    let inner = stream::stream_handle(handle).unwrap();

    let timeout = runtime()
        .block_on(inner.read(0, 50))
        .expect_err("no data must time out");
    assert_eq!(timeout.status, TIMEOUT);

    // A waiting read is released by close from another thread.
    let closer = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(100));
        stream::close(handle).unwrap();
    });
    let cancelled = runtime()
        .block_on(inner.read(0, u32::MAX))
        .expect_err("close must unblock the reader");
    assert_eq!(cancelled.status, CANCELLED);
    closer.join().unwrap();
    let _ = release_tx.send(());
    server.join().unwrap();

    // The handle is gone from the registry after close.
    assert_eq!(
        stream::stream_handle(handle).err().unwrap().status,
        NOT_FOUND
    );
}

fn spawn_silent_tcp() -> (
    SocketAddr,
    std::sync::mpsc::Sender<()>,
    std::thread::JoinHandle<()>,
) {
    let (addr_tx, addr_rx) = std::sync::mpsc::channel();
    let (release_tx, release_rx) = std::sync::mpsc::channel::<()>();
    let server = std::thread::spawn(move || {
        let listener = StdTcpListener::bind("127.0.0.1:0").unwrap();
        addr_tx.send(listener.local_addr().unwrap()).unwrap();
        let (_socket, _) = listener.accept().unwrap();
        let _ = release_rx.recv();
    });
    (addr_rx.recv().unwrap(), release_tx, server)
}

fn assert_shutdown_unblocks_pending_read(how: i32) {
    let (addr, release, server) = spawn_silent_tcp();
    let handle = stream::open("127.0.0.1", addr.port(), b"", 5_000).unwrap();
    let inner = stream::stream_handle(handle).unwrap();
    let shutdown_inner = inner.clone();
    let shutdown = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(100));
        runtime().block_on(shutdown_inner.shutdown(how)).unwrap();
    });
    let err = runtime()
        .block_on(inner.read(0, u32::MAX))
        .expect_err("shutdown must unblock the reader");
    assert_eq!(err.status, END_OF_STREAM);
    shutdown.join().unwrap();
    stream::close(handle).unwrap();
    let _ = release.send(());
    server.join().unwrap();
}

#[test]
fn stream_shutdown_read_unblocks_pending_read() {
    assert_shutdown_unblocks_pending_read(SHUTDOWN_READ);
}

#[test]
fn stream_shutdown_both_unblocks_pending_read() {
    assert_shutdown_unblocks_pending_read(SHUTDOWN_BOTH);
}

/// Regression: a read parked on an idle stream must not hold the write path.
/// One thread blocks in `read(WAIT_FOREVER)`; the main thread writes and
/// half-closes under a watchdog, then the reader completes with the reply.
#[test]
fn stream_read_does_not_block_write_or_half_close() {
    let (addr_tx, addr_rx) = std::sync::mpsc::channel();
    let server = std::thread::spawn(move || {
        let listener = StdTcpListener::bind("127.0.0.1:0").unwrap();
        addr_tx.send(listener.local_addr().unwrap()).unwrap();
        let (mut socket, _) = listener.accept().unwrap();
        let mut buf = [0u8; 8];
        let read = socket.read(&mut buf).unwrap();
        assert_eq!(&buf[..read], b"ping");
        // The client half-close must arrive as a clean EOF.
        let mut rest = Vec::new();
        socket.read_to_end(&mut rest).unwrap();
        assert!(rest.is_empty(), "unexpected bytes after half-close");
        socket.write_all(b"pong").unwrap();
        let _ = socket.shutdown(std::net::Shutdown::Write);
    });
    let addr = addr_rx.recv().unwrap();
    let handle = stream::open("127.0.0.1", addr.port(), b"", 5_000).unwrap();
    let inner = stream::stream_handle(handle).unwrap();

    let reader_inner = inner.clone();
    let reader = std::thread::spawn(move || runtime().block_on(reader_inner.read(0, u32::MAX)));
    // Let the reader park inside read() before exercising the write half.
    std::thread::sleep(Duration::from_millis(100));

    let written = runtime().block_on(async {
        tokio::time::timeout(Duration::from_secs(2), inner.write(b"ping", 5_000)).await
    });
    let written = match written {
        Ok(result) => result.expect("write"),
        Err(_) => {
            stream::close(handle).ok();
            panic!("stream_write waited for the parked reader");
        }
    };
    assert_eq!(written, 4);

    let shutdown = runtime().block_on(async {
        tokio::time::timeout(Duration::from_secs(2), inner.shutdown(SHUTDOWN_WRITE)).await
    });
    if shutdown.is_err() {
        stream::close(handle).ok();
        panic!("stream_shutdown(WRITE) waited for the parked reader");
    }
    shutdown.unwrap().expect("write shutdown");

    assert_eq!(
        reader.join().unwrap().expect("read after peer reply"),
        b"pong"
    );
    stream::close(handle).unwrap();
    server.join().unwrap();
}

#[test]
fn tcp_open_validates_arguments() {
    let err = stream::open("", 80, b"", 1_000).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = stream::open("127.0.0.1", 0, b"", 1_000).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = stream::open("127.0.0.1", 80, br#"{"insecure":true}"#, 1_000).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = stream::open("127.0.0.1", 80, br#"{"roots":"none"}"#, 1_000).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
}

/* ------------------------------------------------------------------ *
 * TLS streams
 * ------------------------------------------------------------------ */

fn tls_server_config() -> Arc<rustls::ServerConfig> {
    let certs: Vec<CertificateDer<'static>> = CertificateDer::pem_slice_iter(SERVER_PEM)
        .collect::<Result<_, _>>()
        .unwrap();
    let key = PrivateKeyDer::from_pem_slice(SERVER_KEY_PEM).unwrap();
    Arc::new(
        rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .unwrap()
        .with_no_client_auth()
        .with_single_cert(certs, key)
        .unwrap(),
    )
}

fn spawn_tls_server() -> SocketAddr {
    let acceptor = tokio_rustls::TlsAcceptor::from(tls_server_config());
    let (addr_tx, addr_rx) = std::sync::mpsc::channel();
    runtime().spawn(async move {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        addr_tx.send(listener.local_addr().unwrap()).unwrap();
        let (tcp, _) = listener.accept().await.unwrap();
        let mut tls = acceptor.accept(tcp).await.unwrap();
        tls.write_all(b"secure").await.unwrap();
        tls.shutdown().await.unwrap();
    });
    addr_rx.recv().unwrap()
}

#[test]
fn tls_stream_verifies_the_embedded_ca() {
    let addr = spawn_tls_server();
    let ca = String::from_utf8(CA_PEM.to_vec()).unwrap();
    let options = serde_json::json!({ "tls": true, "roots": "pem", "ca_pem": ca }).to_string();
    let handle =
        stream::open("127.0.0.1", addr.port(), options.as_bytes(), 5_000).expect("verified TLS");
    let inner = stream::stream_handle(handle).unwrap();
    assert!(inner.tls());
    assert_eq!(
        runtime().block_on(inner.read(0, 5_000)).expect("read"),
        b"secure"
    );
    stream::close(handle).unwrap();

    // The same server without the test CA is untrusted: verification is not
    // optional and no insecure option exists.
    let err = stream::open(
        "127.0.0.1",
        addr.port(),
        br#"{"tls":true,"roots":"webpki"}"#,
        5_000,
    )
    .expect_err("unknown CA must be refused");
    assert_eq!(err.status, TRANSPORT);
}

#[test]
fn tls_half_close_keeps_read_side_usable() {
    // A TLS write half-close must send close_notify (so the peer reads EOF)
    // while the client keeps reading the peer's reply afterward.
    let acceptor = tokio_rustls::TlsAcceptor::from(tls_server_config());
    let (addr_tx, addr_rx) = std::sync::mpsc::channel();
    runtime().spawn(async move {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        addr_tx.send(listener.local_addr().unwrap()).unwrap();
        let (tcp, _) = listener.accept().await.unwrap();
        let mut tls = acceptor.accept(tcp).await.unwrap();
        let mut data = Vec::new();
        tls.read_to_end(&mut data).await.expect("close_notify");
        assert_eq!(data, b"ping");
        tls.write_all(b"pong").await.unwrap();
        tls.shutdown().await.unwrap();
    });
    let addr = addr_rx.recv().unwrap();
    let ca = String::from_utf8(CA_PEM.to_vec()).unwrap();
    let options = serde_json::json!({ "tls": true, "roots": "pem", "ca_pem": ca }).to_string();
    let handle =
        stream::open("127.0.0.1", addr.port(), options.as_bytes(), 5_000).expect("verified TLS");
    let inner = stream::stream_handle(handle).unwrap();
    assert_eq!(runtime().block_on(inner.write(b"ping", 5_000)).unwrap(), 4);
    runtime().block_on(inner.shutdown(SHUTDOWN_WRITE)).unwrap();
    assert_eq!(
        runtime().block_on(inner.read(0, 5_000)).expect("read"),
        b"pong"
    );
    stream::close(handle).unwrap();
}

/* ------------------------------------------------------------------ *
 * UDP
 * ------------------------------------------------------------------ */

fn udp_pair() -> (StdUdpSocket, SocketAddr) {
    let peer = StdUdpSocket::bind("127.0.0.1:0").unwrap();
    let addr = peer.local_addr().unwrap();
    (peer, addr)
}

#[test]
fn udp_unconnected_send_recv_and_timeout() {
    let (peer, peer_addr) = udp_pair();
    let handle = udp::open("", 0, br#"{"bind":"127.0.0.1","port":0}"#, 5_000).unwrap();
    let inner = udp::socket(handle).unwrap();

    let peer_json = serde_json::to_vec(&json!({
        "address": peer_addr.ip().to_string(),
        "port": peer_addr.port(),
    }))
    .unwrap();
    let sent = runtime()
        .block_on(inner.send(udp::parse_peer(&peer_json).unwrap(), b"hello", 5_000))
        .unwrap();
    assert_eq!(sent, 5);

    let mut buf = [0u8; 32];
    let (read, from) = peer.recv_from(&mut buf).unwrap();
    assert_eq!(&buf[..read], b"hello");
    peer.send_to(b"world", from).unwrap();

    let (data, source) = runtime().block_on(inner.recv(0, 5_000)).expect("recv");
    assert_eq!(data, b"world");
    assert_eq!(source, peer_addr);

    let timeout = runtime()
        .block_on(inner.recv(0, 30))
        .expect_err("empty socket must time out");
    assert_eq!(timeout.status, TIMEOUT);
    udp::close(handle).unwrap();
}

#[test]
fn udp_connected_send_recv_and_close_unblocks() {
    let (peer, peer_addr) = udp_pair();
    let handle = udp::open(
        "127.0.0.1",
        peer_addr.port(),
        br#"{"bind":"127.0.0.1","port":0}"#,
        5_000,
    )
    .unwrap();
    let inner = udp::socket(handle).unwrap();
    assert!(inner.connected());

    let sent = runtime()
        .block_on(inner.send(None, b"ping", 5_000))
        .unwrap();
    assert_eq!(sent, 4);
    let mut buf = [0u8; 32];
    let (read, from) = peer.recv_from(&mut buf).unwrap();
    assert_eq!(&buf[..read], b"ping");
    peer.send_to(b"pong", from).unwrap();
    let (data, _) = runtime().block_on(inner.recv(0, 5_000)).unwrap();
    assert_eq!(data, b"pong");

    let closer = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(100));
        udp::close(handle).unwrap();
    });
    let cancelled = runtime()
        .block_on(inner.recv(0, u32::MAX))
        .expect_err("close must unblock recv");
    assert_eq!(cancelled.status, CANCELLED);
    closer.join().unwrap();
}

#[test]
fn udp_rejects_bad_peer_and_oversized_datagrams() {
    let handle = udp::open("", 0, b"", 5_000).unwrap();
    let inner = udp::socket(handle).unwrap();
    let err = runtime()
        .block_on(inner.send(None, b"x", 5_000))
        .expect_err("unconnected without peer");
    assert_eq!(err.status, capsulad::ffi::NOT_CONNECTED);
    let err = udp::parse_peer(br#"{"address":"not-an-ip","port":53}"#).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = udp::parse_peer(br#"{"address":"127.0.0.1"}"#).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = runtime()
        .block_on(inner.recv(udp::MAX_DATAGRAM + 1, 10))
        .unwrap_err();
    assert_eq!(err.status, capsulad::ffi::TOO_LARGE);
    udp::close(handle).unwrap();
}

#[test]
fn udp_bind_port_must_be_ephemeral() {
    // A fixed inbound port would create an unpoliced listener outside the
    // daemon, so only ephemeral binds are accepted.
    for options in [
        br#"{"bind":"0.0.0.0","port":53}"#.as_slice(),
        br#"{"bind":"127.0.0.1","port":5353}"#.as_slice(),
        br#"{"bind":"127.0.0.1:53","port":0}"#.as_slice(),
    ] {
        let err = udp::open("", 0, options, 1_000).expect_err("fixed bind port");
        assert_eq!(err.status, INVALID_ARGUMENT);
        assert!(err.message.contains("listener_open"), "{}", err.message);
        assert!(err.message.contains("must be 0"), "{}", err.message);
    }
    let err = udp::UdpOptions::parse(br#"{"port":5353}"#).expect_err("fixed option port");
    assert_eq!(err.status, INVALID_ARGUMENT);
    assert!(err.message.contains("listener_open"), "{}", err.message);

    // The bind address remains selectable; only the port is constrained.
    let handle = udp::open("", 0, br#"{"bind":"127.0.0.1","port":0}"#, 1_000).unwrap();
    udp::close(handle).unwrap();
}

/* ------------------------------------------------------------------ *
 * Listener control channel (real SOCK_SEQPACKET + SCM_RIGHTS)
 * ------------------------------------------------------------------ */

fn socketpair() -> (RawFd, RawFd) {
    // SEQPACKET is the contract. macOS has no AF_UNIX SEQPACKET, so fall back
    // to DGRAM: both preserve the one-request/one-response message boundary.
    for kind in [libc::SOCK_SEQPACKET, libc::SOCK_DGRAM] {
        let mut fds = [0 as RawFd; 2];
        let rc = unsafe { libc::socketpair(libc::AF_UNIX, kind, 0, fds.as_mut_ptr()) };
        if rc == 0 {
            return (fds[0], fds[1]);
        }
    }
    panic!("socketpair: {}", std::io::Error::last_os_error());
}

fn send_with_fd(fd: RawFd, body: &[u8], file: Option<RawFd>) {
    let mut iov = libc::iovec {
        iov_base: body.as_ptr() as *mut libc::c_void,
        iov_len: body.len(),
    };
    let mut msg: libc::msghdr = unsafe { std::mem::zeroed() };
    msg.msg_iov = &mut iov;
    msg.msg_iovlen = 1;
    let mut control = [0u8; 128];
    if let Some(file) = file {
        msg.msg_control = control.as_mut_ptr() as *mut libc::c_void;
        msg.msg_controllen =
            unsafe { libc::CMSG_SPACE(std::mem::size_of::<libc::c_int>() as u32) as _ };
        let cmsg = unsafe { libc::CMSG_FIRSTHDR(&msg) };
        assert!(!cmsg.is_null());
        unsafe {
            (*cmsg).cmsg_level = libc::SOL_SOCKET;
            (*cmsg).cmsg_type = libc::SCM_RIGHTS;
            (*cmsg).cmsg_len = libc::CMSG_LEN(std::mem::size_of::<libc::c_int>() as u32) as _;
            std::ptr::write(libc::CMSG_DATA(cmsg) as *mut libc::c_int, file);
        }
    }
    let sent = unsafe { libc::sendmsg(fd, &msg, libc::MSG_NOSIGNAL) };
    assert_eq!(
        sent,
        body.len() as isize,
        "sendmsg: {}",
        std::io::Error::last_os_error()
    );
}

/// Fake daemon: reads one request, writes one response (optionally with an fd),
/// echoes the request's numeric `id` in the response, and returns the parsed
/// request.
fn fake_daemon(
    daemon_fd: RawFd,
    file: Option<RawFd>,
    response: Value,
) -> std::thread::JoinHandle<Value> {
    std::thread::spawn(move || {
        let request = read_request(daemon_fd);
        let mut response = response;
        let id = request
            .get("id")
            .cloned()
            .expect("control request must carry an id");
        assert!(id.is_u64(), "control request id must be numeric");
        response
            .as_object_mut()
            .expect("fake daemon response must be an object")
            .insert("id".to_string(), id);
        send_with_fd(daemon_fd, &serde_json::to_vec(&response).unwrap(), file);
        unsafe {
            libc::close(daemon_fd);
        }
        request
    })
}

/// Reads one control request message (requests carry no ancillary data).
fn read_request(fd: RawFd) -> Value {
    let mut buf = vec![0u8; 4096];
    let read = unsafe { libc::recv(fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len(), 0) };
    assert!(read > 0, "control recv failed");
    buf.truncate(read as usize);
    serde_json::from_slice(&buf).expect("request JSON")
}

/// Proves the peer's received duplicate of a descriptor was closed: the
/// socketpair peer reaches EOF. Used to catch fd leaks on error/stale paths.
fn assert_peer_closed(fd: RawFd) {
    for _ in 0..200 {
        let mut pfd = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        let rc = unsafe { libc::poll(&mut pfd, 1, 10) };
        assert!(rc >= 0, "poll: {}", std::io::Error::last_os_error());
        if rc > 0 && (pfd.revents & (libc::POLLIN | libc::POLLHUP)) != 0 {
            let mut byte = 0u8;
            let read = unsafe {
                libc::recv(
                    fd,
                    &mut byte as *mut u8 as *mut libc::c_void,
                    1,
                    libc::MSG_DONTWAIT,
                )
            };
            if read == 0 {
                return;
            }
            let err = std::io::Error::last_os_error();
            if err.kind() == std::io::ErrorKind::WouldBlock {
                continue;
            }
            // macOS reports a closed SEQPACKET peer as ECONNRESET instead of a
            // zero-length read; both mean the descriptor is gone.
            assert!(
                matches!(
                    err.raw_os_error(),
                    Some(libc::ECONNRESET) | Some(libc::ENOTCONN)
                ),
                "descriptor was leaked: peer still open ({err})"
            );
            return;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    panic!("received descriptor was not closed (peer never reached EOF)");
}

struct EnvGuard {
    _lock: std::sync::MutexGuard<'static, ()>,
}

impl EnvGuard {
    fn set(fd: RawFd) -> Self {
        let lock = ENV_LOCK.lock().unwrap_or_else(|err| err.into_inner());
        std::env::set_var(control::CONTROL_FD_ENV, fd.to_string());
        Self { _lock: lock }
    }

    fn clear() -> Self {
        let lock = ENV_LOCK.lock().unwrap_or_else(|err| err.into_inner());
        std::env::remove_var(control::CONTROL_FD_ENV);
        Self { _lock: lock }
    }
}

impl Drop for EnvGuard {
    fn drop(&mut self) {
        std::env::remove_var(control::CONTROL_FD_ENV);
    }
}

#[test]
fn listener_open_accept_over_real_control_channel() {
    let listener = StdTcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let (app_fd, daemon_fd) = socketpair();
    let daemon = fake_daemon(
        daemon_fd,
        Some(listener.as_raw_fd()),
        json!({
            "ok": true,
            "protocol": "tcp",
            "address": "127.0.0.1",
            "port": addr.port(),
        }),
    );

    let guard = EnvGuard::set(app_fd);
    let (handle, info) = listener::open(
        br#"{"protocol":"tcp","bind":"127.0.0.1","port":0,"backlog":16}"#,
        5_000,
    )
    .expect("listener open");
    let info: Value = serde_json::from_slice(&info).unwrap();
    assert_eq!(info["protocol"], "tcp");
    assert_eq!(info["address"], "127.0.0.1");
    assert_eq!(info["port"], addr.port());

    let request = daemon.join().unwrap();
    assert_eq!(request["op"], "listen");
    assert_eq!(request["protocol"], "tcp");
    assert_eq!(request["bind"], "127.0.0.1");
    assert_eq!(request["backlog"], 16);

    // An accept with no pending connection times out but leaves the listener
    // usable.
    let inner = listener::listener(handle).unwrap();
    let timeout = runtime()
        .block_on(inner.accept(50))
        .expect_err("no pending connection");
    assert_eq!(timeout.status, TIMEOUT);

    let client = std::thread::spawn(move || {
        let mut socket = TcpStream::connect(addr).unwrap();
        socket.write_all(b"hi").unwrap();
        let mut buf = [0u8; 2];
        socket.read_exact(&mut buf).unwrap();
        buf
    });
    let (stream_handle, peer_json) = runtime().block_on(inner.accept(5_000)).expect("accept");
    let peer: Value = serde_json::from_slice(&peer_json).unwrap();
    assert_eq!(peer["address"], "127.0.0.1");

    let stream_inner = stream::stream_handle(stream_handle).unwrap();
    assert_eq!(
        runtime().block_on(stream_inner.read(0, 5_000)).unwrap(),
        b"hi"
    );
    assert_eq!(
        runtime()
            .block_on(stream_inner.write(b"yo", 5_000))
            .unwrap(),
        2
    );
    assert_eq!(client.join().unwrap(), *b"yo");

    stream::close(stream_handle).unwrap();
    listener::close(handle).unwrap();
    drop(guard);
    unsafe {
        libc::close(app_fd);
    }
}

#[test]
fn listener_udp_recv_and_send_over_control_channel() {
    let socket = StdUdpSocket::bind("127.0.0.1:0").unwrap();
    let addr = socket.local_addr().unwrap();
    let (app_fd, daemon_fd) = socketpair();
    let daemon = fake_daemon(
        daemon_fd,
        Some(socket.as_raw_fd()),
        json!({
            "ok": true,
            "info": {"protocol": "udp", "address": "127.0.0.1", "port": addr.port()},
        }),
    );
    let guard = EnvGuard::set(app_fd);
    let (handle, _) = listener::open(br#"{"protocol":"udp","bind":"127.0.0.1","port":0}"#, 5_000)
        .expect("udp listener");
    assert_eq!(daemon.join().unwrap()["protocol"], "udp");

    let client = StdUdpSocket::bind("127.0.0.1:0").unwrap();
    let inner = listener::listener(handle).unwrap();
    let timeout = runtime()
        .block_on(inner.recv(0, 30))
        .expect_err("no datagram yet");
    assert_eq!(timeout.status, TIMEOUT);
    client.send_to(b"ping", addr).unwrap();
    let (data, peer_json) = runtime().block_on(inner.recv(0, 5_000)).expect("recv");
    assert_eq!(data, b"ping");
    let peer: Value = serde_json::from_slice(&peer_json).unwrap();
    let peer_addr: SocketAddr = format!(
        "{}:{}",
        peer["address"].as_str().unwrap(),
        peer["port"].as_u64().unwrap()
    )
    .parse()
    .unwrap();

    runtime()
        .block_on(inner.send(Some(peer_addr), b"pong", 5_000))
        .unwrap();
    let mut buf = [0u8; 32];
    let (read, _) = client.recv_from(&mut buf).unwrap();
    assert_eq!(&buf[..read], b"pong");

    let err = runtime()
        .block_on(inner.accept(100))
        .expect_err("udp listener cannot accept");
    assert_eq!(err.status, INVALID_ARGUMENT);

    listener::close(handle).unwrap();
    drop(guard);
    unsafe {
        libc::close(app_fd);
    }
}

#[test]
fn listener_open_without_control_channel_is_unsupported() {
    let _guard = EnvGuard::clear();
    let err = listener::open(br#"{"protocol":"tcp"}"#, 100).unwrap_err();
    assert_eq!(err.status, UNSUPPORTED);
    assert!(err.message.contains("control"), "{}", err.message);
}

#[test]
fn listener_open_with_invalid_fd_is_unsupported() {
    let _guard = EnvGuard::clear();
    std::env::set_var(control::CONTROL_FD_ENV, "999999");
    let err = listener::open(br#"{"protocol":"tcp"}"#, 100).unwrap_err();
    assert_eq!(err.status, UNSUPPORTED);
}

#[test]
fn listener_policy_error_is_surfaced() {
    let (app_fd, daemon_fd) = socketpair();
    let daemon = fake_daemon(
        daemon_fd,
        None,
        json!({"ok": false, "status": 1, "message": "port 5353 is reserved"}),
    );
    let guard = EnvGuard::set(app_fd);
    let err = listener::open(br#"{"protocol":"tcp","bind":"0.0.0.0","port":5353}"#, 5_000)
        .expect_err("policy error");
    assert_eq!(err.status, INVALID_ARGUMENT);
    assert!(err.message.contains("5353"), "{}", err.message);
    assert_eq!(daemon.join().unwrap()["op"], "listen");
    drop(guard);
    unsafe {
        libc::close(app_fd);
    }
}

#[test]
fn listener_error_without_status_is_invalid_argument() {
    let (app_fd, daemon_fd) = socketpair();
    let daemon = fake_daemon(
        daemon_fd,
        None,
        json!({"ok": false, "error": "port 5353 is reserved for mDNS"}),
    );
    let guard = EnvGuard::set(app_fd);
    let err = listener::open(br#"{"protocol":"tcp","bind":"0.0.0.0","port":5353}"#, 5_000)
        .expect_err("policy error");
    assert_eq!(err.status, INVALID_ARGUMENT);
    assert!(err.message.contains("5353"), "{}", err.message);
    daemon.join().unwrap();
    drop(guard);
    unsafe {
        libc::close(app_fd);
    }
}

#[test]
fn control_request_skips_stale_reply_and_closes_its_fd() {
    let (app_fd, daemon_fd) = socketpair();
    let (stale_a, stale_b) = socketpair();
    let matching = StdTcpListener::bind("127.0.0.1:0").unwrap();
    let daemon = std::thread::spawn(move || {
        // Request 1 times out in the caller; its reply is produced late with a
        // descriptor that must never reach request 2.
        let first = read_request(daemon_fd);
        std::thread::sleep(Duration::from_millis(200));
        let reply = json!({
            "ok": true,
            "port": 1111,
            "id": first["id"],
        });
        send_with_fd(
            daemon_fd,
            &serde_json::to_vec(&reply).unwrap(),
            Some(stale_a),
        );
        unsafe {
            libc::close(stale_a);
        }
        let second = read_request(daemon_fd);
        let reply = json!({"ok": true, "port": 4242, "id": second["id"]});
        send_with_fd(
            daemon_fd,
            &serde_json::to_vec(&reply).unwrap(),
            Some(matching.as_raw_fd()),
        );
        unsafe {
            libc::close(daemon_fd);
        }
        (first, second)
    });

    let guard = EnvGuard::set(app_fd);
    let timeout = control::request(&json!({"op": "listen"}), Duration::from_millis(50))
        .expect_err("first request must time out");
    assert_eq!(timeout.status, TIMEOUT);
    // Let the late reply land and queue before the next request is sent.
    std::thread::sleep(Duration::from_millis(250));

    let reply = control::request(&json!({"op": "listen"}), Duration::from_secs(5))
        .expect("second request must drain the stale reply and match its own");
    assert_eq!(reply.json["port"], 4242);
    let id = reply.json["id"].as_u64().expect("numeric id");
    control::close_received(reply.fd.expect("listener descriptor"));

    let (first, second) = daemon.join().unwrap();
    assert!(first["id"].is_u64(), "request carries a numeric id");
    assert_eq!(
        second["id"].as_u64().unwrap(),
        first["id"].as_u64().unwrap() + 1
    );
    assert_eq!(id, second["id"].as_u64().unwrap());
    // The stale reply's descriptor was closed instead of being returned.
    assert_peer_closed(stale_b);

    drop(guard);
    unsafe {
        libc::close(app_fd);
        libc::close(stale_b);
    }
}

#[test]
fn control_invalid_json_reply_closes_its_fd() {
    let (app_fd, daemon_fd) = socketpair();
    let (stale_a, stale_b) = socketpair();
    let daemon = std::thread::spawn(move || {
        let request = read_request(daemon_fd);
        assert!(request["id"].is_u64());
        send_with_fd(daemon_fd, b"not json", Some(stale_a));
        unsafe {
            libc::close(stale_a);
            libc::close(daemon_fd);
        }
    });

    let guard = EnvGuard::set(app_fd);
    let err = control::request(&json!({"op": "listen"}), Duration::from_secs(5))
        .expect_err("invalid JSON reply");
    assert_eq!(err.status, INTERNAL);
    assert!(err.message.contains("not valid JSON"), "{}", err.message);
    daemon.join().unwrap();
    assert_peer_closed(stale_b);

    drop(guard);
    unsafe {
        libc::close(app_fd);
        libc::close(stale_b);
    }
}

#[test]
fn listener_close_unblocks_accept() {
    let listener = StdTcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let (app_fd, daemon_fd) = socketpair();
    let daemon = fake_daemon(
        daemon_fd,
        Some(listener.as_raw_fd()),
        json!({
            "ok": true,
            "info": {"protocol": "tcp", "address": "127.0.0.1", "port": addr.port()},
        }),
    );
    let guard = EnvGuard::set(app_fd);
    let (handle, _) = listener::open(br#"{"protocol":"tcp","bind":"127.0.0.1","port":0}"#, 5_000)
        .expect("listener open");
    daemon.join().unwrap();

    let inner = listener::listener(handle).unwrap();
    let closer = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(100));
        listener::close(handle).unwrap();
    });
    let cancelled = runtime()
        .block_on(inner.accept(u32::MAX))
        .expect_err("close must unblock accept");
    assert_eq!(cancelled.status, CANCELLED);
    closer.join().unwrap();
    assert!(inner.is_closed());
    drop(guard);
    unsafe {
        libc::close(app_fd);
    }
}

#[test]
fn listener_options_are_validated() {
    let err = listener::ListenerOptions::parse(br#"{"protocol":"sctp"}"#).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = listener::ListenerOptions::parse(br#"{"backlog":0}"#).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = listener::ListenerOptions::parse(br#"{"backlog":129}"#).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = listener::ListenerOptions::parse(br#"{"port":70000}"#).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = listener::ListenerOptions::parse(br#"{"path":"/tmp/x"}"#).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT, "apps cannot choose a path");
    let defaults = listener::ListenerOptions::parse(b"").unwrap();
    assert_eq!(defaults.protocol, "tcp");
    assert_eq!(defaults.backlog, 64);
}

/* ------------------------------------------------------------------ *
 * mDNS argument validation
 * ------------------------------------------------------------------ */

#[test]
fn mdns_validates_handles_and_requests() {
    let err = mdns::publish(999_999, b"{}", 1_000).unwrap_err();
    assert_eq!(err.status, NOT_FOUND);
    let err = mdns::publish(999_999, b"not json", 1_000).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = mdns::publish(999_999, b"[]", 1_000).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = mdns::unpublish(999_999, "", 1_000).unwrap_err();
    assert_eq!(err.status, INVALID_ARGUMENT);
    let err = mdns::unpublish(999_999, "publication-1", 1_000).unwrap_err();
    assert_eq!(err.status, NOT_FOUND);
    let err = mdns::browse_open(999_999, b"{}", 1_000).unwrap_err();
    assert_eq!(err.status, NOT_FOUND);
}
