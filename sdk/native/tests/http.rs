//! HTTP client conformance over real loopback sockets.
//!
//! Every case exercises the production code path (`HttpClientInner::open` +
//! `http::request`) against a one-connection TCP server. The TLS case proves
//! certificate verification with an embedded CA fixture; there is no insecure
//! escape hatch to test.

use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use capsulad::ffi::{CANCELLED, INVALID_ARGUMENT, NOT_FOUND, TIMEOUT, TOO_LARGE, TRANSPORT};
use capsulad::http::{self, HttpClientInner, HttpRequest, HttpResponse, HTTP_CLIENTS};
use rustls::pki_types::{pem::PemObject, CertificateDer, PrivateKeyDer};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

const CA_PEM: &[u8] = include_bytes!("../testdata/ca.pem");
/// A valid CA that did not issue the test server certificate.
const OTHER_CA_PEM: &[u8] = include_bytes!("../testdata/other-ca.pem");
const SERVER_PEM: &[u8] = include_bytes!("../testdata/server.pem");
const SERVER_KEY_PEM: &[u8] = include_bytes!("../testdata/server-key.pem");

struct Server {
    addr: SocketAddr,
    requests: Arc<Mutex<Vec<Vec<u8>>>>,
}

impl Server {
    fn url(&self, path: &str) -> String {
        format!("http://{}{}", self.addr, path)
    }

    fn requests(&self) -> Vec<Vec<u8>> {
        self.requests.lock().unwrap().clone()
    }
}

async fn spawn_http<F>(handler: F) -> Server
where
    F: Fn(&[u8]) -> Vec<u8> + Send + Sync + 'static,
{
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let requests = Arc::new(Mutex::new(Vec::new()));
    let captured = requests.clone();
    let handler = Arc::new(handler);
    tokio::spawn(async move {
        loop {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            let handler = handler.clone();
            let captured = captured.clone();
            tokio::spawn(async move {
                let mut buf = Vec::new();
                let mut chunk = [0u8; 1024];
                loop {
                    let Ok(read) = stream.read(&mut chunk).await else {
                        return;
                    };
                    if read == 0 {
                        return;
                    }
                    buf.extend_from_slice(&chunk[..read]);
                    if buf.windows(4).any(|window| window == b"\r\n\r\n") {
                        break;
                    }
                    if buf.len() > 64 * 1024 {
                        return;
                    }
                }
                captured.lock().unwrap().push(buf.clone());
                let response = handler(&buf);
                let _ = stream.write_all(&response).await;
                let _ = stream.shutdown().await;
            });
        }
    });
    Server { addr, requests }
}

fn client(options: &str) -> Arc<HttpClientInner> {
    HttpClientInner::open(options.as_bytes()).expect("client options")
}

/// Globally unique request ids keep in-flight requests in different tests from
/// colliding if a test binary runs them concurrently.
fn request_id() -> u64 {
    static NEXT: AtomicU64 = AtomicU64::new(1);
    NEXT.fetch_add(1, Ordering::Relaxed)
}

async fn get(client: &Arc<HttpClientInner>, url: &str) -> capsulad::ffi::SdkResult<HttpResponse> {
    http::request(
        client.clone(),
        request_id(),
        HttpRequest {
            method: "GET".into(),
            url: url.into(),
            headers: Vec::new(),
            body: Vec::new(),
        },
        Some(Duration::from_secs(5)),
    )
    .await
}

#[tokio::test]
async fn content_length_and_last_header_value() {
    let server = spawn_http(|request| {
        assert!(request.starts_with(b"GET /hello?x=1 HTTP/1.1\r\n"));
        assert!(String::from_utf8_lossy(request).contains("Connection: close\r\n"));
        b"HTTP/1.1 200 OK\r\nContent-Length: 11\r\nX-Test: one\r\nX-Test: two\r\n\r\nhello world"
            .to_vec()
    })
    .await;
    let response = get(&client(r#"{"roots":"webpki"}"#), &server.url("/hello?x=1"))
        .await
        .unwrap();
    assert_eq!(response.status, 200);
    assert_eq!(response.body, b"hello world");
    let headers: serde_json::Value = serde_json::from_slice(&response.headers_json).unwrap();
    assert_eq!(headers["x-test"], "two");
    assert_eq!(headers["content-length"], "11");
    let request = &server.requests()[0];
    let text = String::from_utf8_lossy(request);
    assert!(
        text.contains("User-Agent: capsulad-native/1.0\r\n"),
        "{text}"
    );
    assert!(text.contains("Accept-Encoding: identity\r\n"), "{text}");
}

#[tokio::test]
async fn chunked_bodies_are_reassembled() {
    let server = spawn_http(|_| {
        b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n"
            .to_vec()
    })
    .await;
    let response = get(&client(r#"{"roots":"webpki"}"#), &server.url("/"))
        .await
        .unwrap();
    assert_eq!(response.body, b"hello world");
}

#[tokio::test]
async fn redirects_switch_to_get_and_strip_cross_origin_headers() {
    let target = spawn_http(|request| {
        assert!(request.starts_with(b"GET /final HTTP/1.1\r\n"));
        assert!(
            !String::from_utf8_lossy(request)
                .to_ascii_lowercase()
                .contains("authorization:"),
            "cross-origin redirect leaked the caller credential"
        );
        assert!(String::from_utf8_lossy(request).contains("user-agent: agent-x\r\n"));
        b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok".to_vec()
    })
    .await;
    let final_url = target.url("/final");
    let source = spawn_http(move |_| {
        format!("HTTP/1.1 302 Found\r\nLocation: {final_url}\r\nContent-Length: 0\r\n\r\n")
            .into_bytes()
    })
    .await;
    let response = http::request(
        client(r#"{"roots":"webpki"}"#),
        request_id(),
        HttpRequest {
            method: "POST".into(),
            url: source.url("/start"),
            headers: vec![
                ("Authorization".into(), "Bearer secret".into()),
                ("User-Agent".into(), "agent-x".into()),
                ("Content-Type".into(), "text/plain".into()),
            ],
            body: b"payload".to_vec(),
        },
        Some(Duration::from_secs(5)),
    )
    .await
    .unwrap();
    assert_eq!(response.body, b"ok");
    let first = String::from_utf8_lossy(&source.requests()[0]).to_string();
    assert!(first.starts_with("POST /start HTTP/1.1\r\n"));
    assert!(first.contains("Content-Length: 7\r\n"));
}

#[tokio::test]
async fn non_http_redirects_are_rejected() {
    let server = spawn_http(|_| {
        b"HTTP/1.1 302 Found\r\nLocation: ftp://example.com/x\r\nContent-Length: 0\r\n\r\n".to_vec()
    })
    .await;
    let err = get(&client(r#"{"roots":"webpki"}"#), &server.url("/"))
        .await
        .expect_err("a non-http Location must be rejected");
    assert_eq!(err.status, TRANSPORT);
}

#[tokio::test]
async fn same_origin_307_preserves_method_and_body() {
    let target = spawn_http(|request| {
        assert!(request.starts_with(b"PUT /final HTTP/1.1\r\n"));
        assert!(request.ends_with(b"\r\n\r\npayload"), "body was lost");
        b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok".to_vec()
    })
    .await;
    let final_url = target.url("/final");
    let source = spawn_http(move |_| {
        format!(
            "HTTP/1.1 307 Temporary Redirect\r\nLocation: {final_url}\r\nContent-Length: 0\r\n\r\n"
        )
        .into_bytes()
    })
    .await;
    let response = http::request(
        client(r#"{"roots":"webpki"}"#),
        request_id(),
        HttpRequest {
            method: "PUT".into(),
            url: source.url("/start"),
            headers: Vec::new(),
            body: b"payload".to_vec(),
        },
        Some(Duration::from_secs(5)),
    )
    .await
    .unwrap();
    assert_eq!(response.body, b"ok");
}

#[tokio::test]
async fn oversized_bodies_are_too_large() {
    let server = spawn_http(|_| b"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n".to_vec()).await;
    let err = get(
        &client(r#"{"roots":"webpki","max_body":16}"#),
        &server.url("/"),
    )
    .await
    .expect_err("body must be capped");
    assert_eq!(err.status, TOO_LARGE);

    let server = spawn_http(|_| {
        b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n20\r\n01234567890123456789012345678901\r\n0\r\n\r\n"
            .to_vec()
    })
    .await;
    let err = get(
        &client(r#"{"roots":"webpki","max_body":8}"#),
        &server.url("/"),
    )
    .await
    .expect_err("chunked body must be capped");
    assert_eq!(err.status, TOO_LARGE);
}

#[tokio::test]
async fn read_timeout_is_reported_as_timeout() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((mut stream, _)) = listener.accept().await {
            tokio::spawn(async move {
                let mut buf = [0u8; 1024];
                let _ = stream.read(&mut buf).await;
                tokio::time::sleep(Duration::from_millis(500)).await;
                let _ = stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                    .await;
            });
        }
    });
    let err = get(
        &client(r#"{"roots":"webpki","read_timeout_ms":100}"#),
        &format!("http://{addr}/"),
    )
    .await
    .expect_err("a stalled server must time out");
    assert_eq!(err.status, TIMEOUT);
}

#[tokio::test]
async fn invalid_urls_and_headers_are_invalid_argument() {
    let client = client(r#"{"roots":"webpki"}"#);
    for (url, headers) in [
        ("ftp://example.com/", Vec::new()),
        ("https://user@example.com/", Vec::new()),
        ("https://example.com/#x", Vec::new()),
        (
            "https://example.com/",
            vec![("Host".to_string(), "example.com".to_string())],
        ),
    ] {
        let err = http::request(
            client.clone(),
            request_id(),
            HttpRequest {
                method: "GET".into(),
                url: url.into(),
                headers,
                body: Vec::new(),
            },
            Some(Duration::from_secs(1)),
        )
        .await
        .expect_err("request must be rejected");
        assert_eq!(err.status, INVALID_ARGUMENT, "{url}");
    }
}

#[tokio::test]
async fn tls_verifies_the_embedded_test_ca() {
    let server = spawn_tls().await;
    let ca = String::from_utf8(CA_PEM.to_vec()).unwrap();
    let options = serde_json::json!({ "roots": "webpki", "ca_pem": ca }).to_string();
    let response = get(&client(&options), &format!("https://{}/tls", server.addr))
        .await
        .unwrap();
    assert_eq!(response.status, 200);
    assert_eq!(response.body, b"secure");

    // Without the test CA the same server is untrusted: verification is not
    // optional and there is no insecure switch.
    let err = get(
        &client(r#"{"roots":"webpki"}"#),
        &format!("https://{}/tls", server.addr),
    )
    .await
    .expect_err("unknown CA must be refused");
    assert_eq!(err.status, TRANSPORT);
}

struct TlsServer {
    addr: SocketAddr,
}

async fn spawn_tls() -> TlsServer {
    spawn_tls_handler(|_| b"HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nsecure".to_vec()).await
}

async fn spawn_tls_handler<F>(handler: F) -> TlsServer
where
    F: Fn(&[u8]) -> Vec<u8> + Send + Sync + 'static,
{
    let certs: Vec<CertificateDer<'static>> = CertificateDer::pem_slice_iter(SERVER_PEM)
        .collect::<Result<_, _>>()
        .unwrap();
    let key = PrivateKeyDer::from_pem_slice(SERVER_KEY_PEM).unwrap();
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(certs, key)
    .unwrap();
    let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(config));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let handler = Arc::new(handler);
    tokio::spawn(async move {
        while let Ok((stream, _)) = listener.accept().await {
            let acceptor = acceptor.clone();
            let handler = handler.clone();
            tokio::spawn(async move {
                let Ok(mut stream) = acceptor.accept(stream).await else {
                    return;
                };
                let mut buf = Vec::new();
                let mut chunk = [0u8; 1024];
                loop {
                    let Ok(read) = stream.read(&mut chunk).await else {
                        return;
                    };
                    if read == 0 {
                        return;
                    }
                    buf.extend_from_slice(&chunk[..read]);
                    if buf.windows(4).any(|window| window == b"\r\n\r\n") {
                        break;
                    }
                }
                let response = handler(&buf);
                let _ = stream.write_all(&response).await;
                let _ = stream.shutdown().await;
            });
        }
    });
    TlsServer { addr }
}

/// Counts accepted connections and answers each with a tiny 200.
struct PlainTarget {
    addr: SocketAddr,
    hits: Arc<AtomicUsize>,
}

async fn spawn_plain_target() -> PlainTarget {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let hits = Arc::new(AtomicUsize::new(0));
    let counter = hits.clone();
    tokio::spawn(async move {
        while let Ok((mut stream, _)) = listener.accept().await {
            counter.fetch_add(1, Ordering::SeqCst);
            tokio::spawn(async move {
                let mut buf = [0u8; 1024];
                let _ = stream.read(&mut buf).await;
                let _ = stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                    .await;
            });
        }
    });
    PlainTarget { addr, hits }
}

fn request_to(server: &Server, method: &str) -> HttpRequest {
    HttpRequest {
        method: method.into(),
        url: server.url("/"),
        headers: Vec::new(),
        body: Vec::new(),
    }
}

/// Serves the first connection slowly and every later one immediately. The
/// returned receiver fires when the first request head has been read, so the
/// test can cancel while that exchange is genuinely in flight.
struct SlowServer {
    addr: SocketAddr,
    first_seen: tokio::sync::mpsc::Receiver<()>,
}

async fn spawn_slow_then_fast() -> SlowServer {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (tx, rx) = tokio::sync::mpsc::channel(1);
    tokio::spawn(async move {
        let mut index = 0usize;
        loop {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            let first = index == 0;
            index += 1;
            let tx = tx.clone();
            tokio::spawn(async move {
                let mut buf = Vec::new();
                let mut chunk = [0u8; 1024];
                loop {
                    let Ok(read) = stream.read(&mut chunk).await else {
                        return;
                    };
                    if read == 0 {
                        return;
                    }
                    buf.extend_from_slice(&chunk[..read]);
                    if buf.windows(4).any(|window| window == b"\r\n\r\n") {
                        break;
                    }
                }
                if first {
                    let _ = tx.send(()).await;
                    tokio::time::sleep(Duration::from_secs(5)).await;
                }
                let _ = stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                    .await;
                let _ = stream.shutdown().await;
            });
        }
    });
    SlowServer {
        addr,
        first_seen: rx,
    }
}

fn slow_request(addr: SocketAddr) -> HttpRequest {
    HttpRequest {
        method: "GET".into(),
        url: format!("http://{addr}/slow"),
        headers: Vec::new(),
        body: Vec::new(),
    }
}

#[tokio::test]
async fn cancel_aborts_in_flight_and_the_client_stays_usable() {
    let mut server = spawn_slow_then_fast().await;
    let handle = http::open(br#"{"roots":"webpki","read_timeout_ms":15000}"#).unwrap();
    let client = HTTP_CLIENTS.get(handle).expect("client handle");
    let id = request_id();
    let task = tokio::spawn(http::request(
        client.clone(),
        id,
        slow_request(server.addr),
        Some(Duration::from_secs(30)),
    ));
    server.first_seen.recv().await.expect("first request seen");

    let started = Instant::now();
    http::cancel(handle, id).unwrap();
    let error = task
        .await
        .expect("request task")
        .expect_err("cancel must interrupt the exchange");
    assert_eq!(error.status, CANCELLED);
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "cancel took {:?}",
        started.elapsed()
    );

    // Cancelling one id only affects that request.
    let response = http::request(
        client,
        request_id(),
        slow_request(server.addr),
        Some(Duration::from_secs(10)),
    )
    .await
    .expect("a later request must still work");
    assert_eq!(response.status, 200);
    assert_eq!(response.body, b"ok");
    http::close(handle).unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn close_cancels_waits_and_invalidates_the_handle() {
    let mut server = spawn_slow_then_fast().await;
    let handle = http::open(br#"{"roots":"webpki","read_timeout_ms":15000}"#).unwrap();
    let client = HTTP_CLIENTS.get(handle).expect("client handle");
    let task = tokio::spawn(http::request(
        client,
        request_id(),
        slow_request(server.addr),
        Some(Duration::from_secs(30)),
    ));
    server.first_seen.recv().await.expect("first request seen");

    // The C ABI calls close from an application thread, not a runtime worker.
    let started = Instant::now();
    let closer = std::thread::spawn(move || http::close(handle));
    closer.join().expect("close thread").unwrap();
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "close took {:?}",
        started.elapsed()
    );
    let error = task
        .await
        .expect("request task")
        .expect_err("close must interrupt the exchange");
    assert_eq!(error.status, CANCELLED);
    assert_eq!(http::close(handle).unwrap_err().status, NOT_FOUND);
}

struct TwoSlowServer {
    addr: SocketAddr,
    first_seen: tokio::sync::mpsc::Receiver<()>,
    second_seen: tokio::sync::mpsc::Receiver<()>,
    release_first: Arc<tokio::sync::Notify>,
    release_second: Arc<tokio::sync::Notify>,
}

async fn spawn_two_slow() -> TwoSlowServer {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (first_tx, first_rx) = tokio::sync::mpsc::channel(1);
    let (second_tx, second_rx) = tokio::sync::mpsc::channel(1);
    let release_first = Arc::new(tokio::sync::Notify::new());
    let release_second = Arc::new(tokio::sync::Notify::new());
    let first_notify = release_first.clone();
    let second_notify = release_second.clone();
    tokio::spawn(async move {
        let mut index = 0usize;
        loop {
            let Ok((mut stream, _)) = listener.accept().await else {
                return;
            };
            let connection_index = index;
            index += 1;
            let first_tx = first_tx.clone();
            let second_tx = second_tx.clone();
            let release_first = first_notify.clone();
            let release_second = second_notify.clone();
            tokio::spawn(async move {
                let mut buf = Vec::new();
                let mut chunk = [0u8; 1024];
                loop {
                    let Ok(read) = stream.read(&mut chunk).await else {
                        return;
                    };
                    if read == 0 {
                        return;
                    }
                    buf.extend_from_slice(&chunk[..read]);
                    if buf.windows(4).any(|window| window == b"\r\n\r\n") {
                        break;
                    }
                }
                let body = if connection_index == 0 {
                    let _ = first_tx.send(()).await;
                    release_first.notified().await;
                    "A"
                } else {
                    let _ = second_tx.send(()).await;
                    release_second.notified().await;
                    "B"
                };
                let response = format!("HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\n{body}");
                let _ = stream.write_all(response.as_bytes()).await;
                let _ = stream.shutdown().await;
            });
        }
    });
    TwoSlowServer {
        addr,
        first_seen: first_rx,
        second_seen: second_rx,
        release_first,
        release_second,
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancelling_one_request_does_not_affect_another() {
    let mut server = spawn_two_slow().await;
    let handle = http::open(br#"{"roots":"webpki","read_timeout_ms":15000}"#).unwrap();
    let client = HTTP_CLIENTS.get(handle).expect("client handle");
    let id_a = request_id();
    let id_b = request_id();
    let a = tokio::spawn(http::request(
        client.clone(),
        id_a,
        slow_request(server.addr),
        Some(Duration::from_secs(30)),
    ));
    let b = tokio::spawn(http::request(
        client.clone(),
        id_b,
        slow_request(server.addr),
        Some(Duration::from_secs(30)),
    ));
    server.first_seen.recv().await.expect("first request seen");
    server
        .second_seen
        .recv()
        .await
        .expect("second request seen");

    http::cancel(handle, id_a).unwrap();
    // The two TCP connections are not labelled by request order, so release
    // both handlers: A is already cancelled, and B must still complete.
    server.release_first.notify_one();
    server.release_second.notify_one();
    let error = a
        .await
        .expect("first task")
        .expect_err("cancelled request must fail");
    assert_eq!(error.status, CANCELLED);

    // B was in flight the whole time and must still complete untouched.
    let response = b
        .await
        .expect("second task")
        .expect("the other request must survive");
    assert_eq!(response.status, 200);
    assert!(matches!(response.body.as_slice(), b"A" | b"B"));
    http::close(handle).unwrap();
}

#[tokio::test]
async fn duplicate_request_ids_are_rejected() {
    let mut server = spawn_slow_then_fast().await;
    let handle = http::open(br#"{"roots":"webpki","read_timeout_ms":15000}"#).unwrap();
    let client = HTTP_CLIENTS.get(handle).expect("client handle");
    let id = request_id();
    let task = tokio::spawn(http::request(
        client.clone(),
        id,
        slow_request(server.addr),
        Some(Duration::from_secs(30)),
    ));
    server.first_seen.recv().await.expect("first request seen");

    let error = http::request(
        client.clone(),
        id,
        slow_request(server.addr),
        Some(Duration::from_secs(5)),
    )
    .await
    .expect_err("a duplicate in-flight id must be rejected");
    assert_eq!(error.status, INVALID_ARGUMENT);

    http::cancel(handle, id).unwrap();
    let cancelled = task
        .await
        .expect("first task")
        .expect_err("the original request must still be cancellable");
    assert_eq!(cancelled.status, CANCELLED);
    http::close(handle).unwrap();
}

#[tokio::test]
async fn https_to_http_redirect_is_refused() {
    let cleartext = spawn_plain_target().await;
    let location = format!("http://{}/cleartext", cleartext.addr);
    let server = spawn_tls_handler(move |_| {
        format!(
            "HTTP/1.1 307 Temporary Redirect\r\nLocation: {location}\r\nContent-Length: 0\r\n\r\n"
        )
        .into_bytes()
    })
    .await;
    let ca = String::from_utf8(CA_PEM.to_vec()).unwrap();
    let options = serde_json::json!({ "roots": "pem", "ca_pem": ca }).to_string();
    let err = get(&client(&options), &format!("https://{}/start", server.addr))
        .await
        .expect_err("a verified https channel must not downgrade to http");
    assert_eq!(err.status, TRANSPORT);
    // The cleartext hop must never be dialled.
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert_eq!(cleartext.hits.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn head_stays_head_across_301_302_303() {
    for (code, reason) in [
        (301u16, "Moved Permanently"),
        (302, "Found"),
        (303, "See Other"),
    ] {
        let target = spawn_http(|_| b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n".to_vec()).await;
        let final_url = target.url("/final");
        let source = spawn_http(move |_| {
            format!(
                "HTTP/1.1 {code} {reason}\r\nLocation: {final_url}\r\nContent-Length: 0\r\n\r\n"
            )
            .into_bytes()
        })
        .await;
        let response = http::request(
            client(r#"{"roots":"webpki"}"#),
            request_id(),
            HttpRequest {
                method: "HEAD".into(),
                url: source.url("/start"),
                headers: vec![("Content-Type".into(), "text/plain".into())],
                body: b"payload".to_vec(),
            },
            Some(Duration::from_secs(5)),
        )
        .await
        .unwrap_or_else(|err| panic!("{code} redirect failed: {}", err.message));
        assert_eq!(response.status, 200);
        assert!(response.body.is_empty());

        let forwarded = target.requests();
        assert_eq!(forwarded.len(), 1, "{code} target was not reached");
        let head = String::from_utf8_lossy(&forwarded[0]);
        assert!(
            head.starts_with("HEAD /final HTTP/1.1\r\n"),
            "{code} turned HEAD into {head:?}"
        );
        assert!(
            head.contains("Content-Length: 0\r\n"),
            "{code} forwarded a body: {head:?}"
        );
        assert!(!head.contains("payload"), "{code} forwarded a body");
    }
}

#[tokio::test]
async fn interim_response_flood_is_rejected() {
    let flood = spawn_http(|_| {
        let mut response = Vec::new();
        for _ in 0..9 {
            response.extend_from_slice(b"HTTP/1.1 100 Continue\r\n\r\n");
        }
        response.extend_from_slice(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok");
        response
    })
    .await;
    let err = get(&client(r#"{"roots":"webpki"}"#), &flood.url("/"))
        .await
        .expect_err("a 1xx flood must be rejected");
    assert_eq!(err.status, TRANSPORT);

    let tolerated = spawn_http(|_| {
        let mut response = Vec::new();
        for _ in 0..8 {
            response.extend_from_slice(b"HTTP/1.1 103 Early Hints\r\n\r\n");
        }
        response.extend_from_slice(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok");
        response
    })
    .await;
    let response = get(&client(r#"{"roots":"webpki"}"#), &tolerated.url("/"))
        .await
        .expect("eight interim responses are within the limit");
    assert_eq!(response.body, b"ok");
}

#[tokio::test]
async fn endless_trailers_are_bounded() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((mut stream, _)) = listener.accept().await {
            tokio::spawn(async move {
                let mut buf = Vec::new();
                let mut chunk = [0u8; 1024];
                loop {
                    let Ok(read) = stream.read(&mut chunk).await else {
                        return;
                    };
                    if read == 0 {
                        return;
                    }
                    buf.extend_from_slice(&chunk[..read]);
                    if buf.windows(4).any(|window| window == b"\r\n\r\n") {
                        break;
                    }
                }
                if stream
                    .write_all(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n")
                    .await
                    .is_err()
                {
                    return;
                }
                // Never send the terminating blank line.
                for _ in 0..10_000 {
                    if stream
                        .write_all(b"X-Trailer: 0123456789abcdef\r\n")
                        .await
                        .is_err()
                    {
                        return;
                    }
                }
            });
        }
    });
    let started = Instant::now();
    let err = http::request(
        client(r#"{"roots":"webpki"}"#),
        request_id(),
        HttpRequest {
            method: "GET".into(),
            url: format!("http://{addr}/"),
            headers: Vec::new(),
            body: Vec::new(),
        },
        Some(Duration::from_secs(5)),
    )
    .await
    .expect_err("endless trailers must be bounded");
    assert_eq!(err.status, TOO_LARGE);
    assert!(
        started.elapsed() < Duration::from_secs(3),
        "trailer bound was not reached promptly: {:?}",
        started.elapsed()
    );
}

#[tokio::test]
async fn total_timeout_bounds_the_whole_exchange() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((mut stream, _)) = listener.accept().await {
            tokio::spawn(async move {
                let mut buf = [0u8; 1024];
                let _ = stream.read(&mut buf).await;
                tokio::time::sleep(Duration::from_secs(5)).await;
                let _ = stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                    .await;
            });
        }
    });
    let started = Instant::now();
    let err = http::request(
        client(r#"{"roots":"webpki","total_timeout_ms":200}"#),
        request_id(),
        HttpRequest {
            method: "GET".into(),
            url: format!("http://{addr}/"),
            headers: Vec::new(),
            body: Vec::new(),
        },
        None,
    )
    .await
    .expect_err("a stalled exchange must hit the total timeout");
    assert_eq!(err.status, TIMEOUT);
    assert!(
        started.elapsed() < Duration::from_secs(3),
        "total timeout took {:?}",
        started.elapsed()
    );
}

#[tokio::test]
async fn per_request_timeout_overrides_the_total_timeout() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((mut stream, _)) = listener.accept().await {
            tokio::spawn(async move {
                let mut buf = [0u8; 1024];
                let _ = stream.read(&mut buf).await;
                tokio::time::sleep(Duration::from_millis(400)).await;
                let _ = stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                    .await;
            });
        }
    });
    // The client's 200 ms total budget would fail this; the request's own 2 s
    // deadline wins.
    let response = http::request(
        client(r#"{"roots":"webpki","total_timeout_ms":200}"#),
        request_id(),
        HttpRequest {
            method: "GET".into(),
            url: format!("http://{addr}/"),
            headers: Vec::new(),
            body: Vec::new(),
        },
        Some(Duration::from_secs(2)),
    )
    .await
    .expect("the per-request timeout must win");
    assert_eq!(response.body, b"ok");
}

#[tokio::test]
async fn pem_roots_trust_only_the_supplied_ca() {
    let server = spawn_tls().await;

    let ca = String::from_utf8(CA_PEM.to_vec()).unwrap();
    let options = serde_json::json!({ "roots": "pem", "ca_pem": ca }).to_string();
    let response = get(&client(&options), &format!("https://{}/tls", server.addr))
        .await
        .expect("the supplied CA must verify the server");
    assert_eq!(response.body, b"secure");

    let other = String::from_utf8(OTHER_CA_PEM.to_vec()).unwrap();
    let options = serde_json::json!({ "roots": "pem", "ca_pem": other }).to_string();
    let err = get(&client(&options), &format!("https://{}/tls", server.addr))
        .await
        .expect_err("an unlisted CA must not verify the server");
    assert_eq!(err.status, TRANSPORT);

    // "pem" without anchors must not silently fall back to public roots.
    let err = HttpClientInner::open(br#"{"roots":"pem"}"#)
        .err()
        .expect("pem without ca_pem must not open");
    assert_eq!(err.status, INVALID_ARGUMENT);
}

#[tokio::test]
async fn cancel_before_registration_is_honoured() {
    let server = spawn_http(|_| b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok".to_vec()).await;
    let handle = http::open(br#"{"roots":"webpki"}"#).unwrap();
    let client = HTTP_CLIENTS.get(handle).expect("client handle");
    let id = request_id();
    // The cancellation watcher runs before the request registers: the id must
    // still be refused, promptly and without any network I/O.
    http::cancel(handle, id).unwrap();
    let started = Instant::now();
    let err = http::request(
        client.clone(),
        id,
        request_to(&server, "GET"),
        Some(Duration::from_secs(30)),
    )
    .await
    .expect_err("a pre-registration cancel must fail the request");
    assert_eq!(err.status, CANCELLED);
    assert!(
        started.elapsed() < Duration::from_secs(1),
        "cancel took {:?}",
        started.elapsed()
    );
    assert!(
        server.requests().is_empty(),
        "the cancelled request reached the network"
    );

    let response = http::request(
        client,
        request_id(),
        request_to(&server, "GET"),
        Some(Duration::from_secs(5)),
    )
    .await
    .expect("a later request must still succeed");
    assert_eq!(response.body, b"ok");
    http::close(handle).unwrap();
}

#[tokio::test]
async fn late_cancel_is_a_no_op_and_new_requests_proceed() {
    let server = spawn_http(|_| b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok".to_vec()).await;
    let handle = http::open(br#"{"roots":"webpki"}"#).unwrap();
    let client = HTTP_CLIENTS.get(handle).expect("client handle");
    let id = request_id();
    let first = http::request(
        client.clone(),
        id,
        request_to(&server, "GET"),
        Some(Duration::from_secs(5)),
    )
    .await
    .expect("the first request must complete");
    assert_eq!(first.status, 200);

    // Ids are unique for the client lifetime. A watcher firing after
    // completion must be a no-op for the finished id and must not disturb a
    // new, never-used id.
    http::cancel(handle, id).unwrap();
    let second = http::request(
        client,
        request_id(),
        request_to(&server, "GET"),
        Some(Duration::from_secs(5)),
    )
    .await
    .expect("a late cancel must not affect a new request");
    assert_eq!(second.status, 200);
    assert_eq!(server.requests().len(), 2);
    http::close(handle).unwrap();
}

#[tokio::test]
async fn response_header_control_bytes_are_a_transport_error() {
    for response in [
        b"HTTP/1.1 200 OK\r\nX-Test: a\nb\r\nContent-Length: 0\r\n\r\n".to_vec(),
        b"HTTP/1.1 200 OK\r\nX-Test: a\rb\r\nContent-Length: 0\r\n\r\n".to_vec(),
        b"HTTP/1.1 200 OK\r\nX-Test: a\0b\r\nContent-Length: 0\r\n\r\n".to_vec(),
    ] {
        let server = spawn_http(move |_| response.clone()).await;
        let err = get(&client(r#"{"roots":"webpki"}"#), &server.url("/"))
            .await
            .expect_err("a control byte in a header value must be rejected");
        assert_eq!(err.status, TRANSPORT);
    }
}
