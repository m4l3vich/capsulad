//! Optional integration smoke tests for the appended v1 networking surface.
//!
//! They run only when `CAPSULAD_TEST_LIBRARY` names a staged `libcapsulad`
//! built with ABI 1.0; otherwise they return immediately so `cargo test` works
//! without the native artifact.

use std::env;
use std::io::{Read, Write};
use std::net::{TcpListener, UdpSocket as StdUdpSocket};
use std::thread;
use std::time::Duration;

use capsulad::{
    Library, Listener, ListenerOptions, PeerAddr, Shutdown, Stream, TcpOptions, UdpOptions,
    UdpSocket,
};

fn library() -> Option<Library> {
    let path = env::var_os("CAPSULAD_TEST_LIBRARY")?;
    Some(Library::open(path).expect("load CAPSULAD_TEST_LIBRARY"))
}

#[test]
fn tcp_loopback_echo_and_close_are_idempotent() {
    let Some(library) = library() else {
        eprintln!("skipping: CAPSULAD_TEST_LIBRARY is not set");
        return;
    };
    let server = TcpListener::bind("127.0.0.1:0").expect("bind loopback");
    let port = server.local_addr().expect("bound port").port();
    let server_task = thread::spawn(move || {
        let (mut conn, _) = server.accept().expect("accept");
        let mut buf = [0u8; 5];
        conn.read_exact(&mut buf).expect("server read");
        conn.write_all(&buf).expect("server write");
    });

    let mut stream = Stream::open(
        &library,
        "127.0.0.1",
        port,
        &TcpOptions::default(),
        Some(Duration::from_secs(5)),
    )
    .expect("open tcp");
    stream
        .write(b"hello", Some(Duration::from_secs(5)))
        .expect("write");
    let reply = stream
        .read(5, Some(Duration::from_secs(5)))
        .expect("read")
        .expect("data");
    assert_eq!(reply, b"hello");
    let info = stream.info().expect("info");
    assert_eq!(info.kind, "stream");
    assert_eq!(info.port, port);
    stream.shutdown(Shutdown::Write).expect("shutdown write");
    assert!(stream
        .read(5, Some(Duration::from_secs(5)))
        .expect("eof")
        .is_none());
    stream.close().expect("close");
    stream.close().expect("second close");
    server_task.join().expect("echo thread");
}

#[test]
fn udp_loopback_echo() {
    let Some(library) = library() else {
        eprintln!("skipping: CAPSULAD_TEST_LIBRARY is not set");
        return;
    };
    let server = StdUdpSocket::bind("127.0.0.1:0").expect("bind udp");
    let port = server.local_addr().expect("bound port").port();
    let server_task = thread::spawn(move || {
        let mut buf = [0u8; 16];
        let (n, peer) = server.recv_from(&mut buf).expect("server recv");
        server.send_to(&buf[..n], peer).expect("server send");
    });

    let mut socket = UdpSocket::open(
        &library,
        "",
        0,
        &UdpOptions {
            bind: Some("127.0.0.1".into()),
            port: None,
        },
        Some(Duration::from_secs(5)),
    )
    .expect("open udp");
    let peer = PeerAddr::new("127.0.0.1", port);
    assert_eq!(
        socket
            .send(Some(&peer), b"ping", Some(Duration::from_secs(5)))
            .expect("send"),
        4
    );
    let (data, from) = socket.recv(0, Some(Duration::from_secs(5))).expect("recv");
    assert_eq!(data, b"ping");
    assert_eq!(from.port, port);
    socket.close().expect("close");
    server_task.join().expect("echo thread");
}

#[test]
fn listener_without_a_control_channel_is_unsupported() {
    let Some(library) = library() else {
        eprintln!("skipping: CAPSULAD_TEST_LIBRARY is not set");
        return;
    };
    if env::var_os("CAPSULAD_CONTROL_FD").is_some() {
        eprintln!("skipping: a control channel is present");
        return;
    }
    match Listener::open(&library, &ListenerOptions::default(), None) {
        Ok(mut listener) => {
            let _ = listener.close();
            eprintln!("skipping: this environment has a usable control channel");
        }
        Err(err) => assert_eq!(err.status(), capsulad::STATUS_UNSUPPORTED),
    }
}
