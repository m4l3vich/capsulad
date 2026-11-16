//! Bounded HTTP/1.1 client: one request per call, verified TLS, no bundled
//! HTTP stack on the application side.
//!
//! The wire behaviour is deliberately small and documented:
//!
//! * one request per call, `Connection: close`, HTTP/1.1;
//! * no automatic decompression — `Accept-Encoding: identity` is added unless
//!   the caller sets its own value, and the body is returned exactly as sent;
//! * response headers are lowercased with the last value winning; header
//!   values containing control characters other than HTAB are rejected;
//! * a non-2xx status is a successful call with `status_code` set;
//! * redirects follow the 301/302/303 (HEAD stays HEAD, everything else
//!   becomes a bodiless GET) and 307/308 (unchanged) rules, bounded by
//!   `follow_redirects`; an `https` → `http` hop is refused with `TRANSPORT`;
//! * the whole exchange, redirects included, is bounded by `total_timeout`
//!   unless the request supplies its own `timeout`;
//! * at most eight interim 1xx responses are tolerated, and chunked trailers
//!   are bounded by both field count and total bytes.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::io::Write as _;
use std::sync::{Arc, Condvar, Mutex};
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::ffi::{SdkError, SdkResult, CANCELLED, INVALID_ARGUMENT, TIMEOUT, TOO_LARGE, TRANSPORT};
use crate::headers;
use crate::primitives::{join_location, parse_url, ParsedUrl};
use crate::registry::Registry;
use crate::tls::{self, NetStream, RootsMode, TlsOptions};

pub const DEFAULT_MAX_BODY: usize = 32 * 1024 * 1024;
pub const HARD_MAX_BODY: usize = 64 * 1024 * 1024;
const MAX_HEAD: usize = 64 * 1024;
/// Interim (1xx) responses tolerated before the final status. Servers use at
/// most a few (`100 Continue`, `103 Early Hints`); more is a protocol flood.
const MAX_INTERIM: u32 = 8;
/// Trailer budget: the number of fields and their total encoded size. Both are
/// capped so a chunked response cannot stream trailers forever.
const MAX_TRAILER_LINES: usize = 64;
const MAX_REDIRECTS: u32 = 4;
const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(15);
const DEFAULT_READ_TIMEOUT: Duration = Duration::from_secs(30);
/// Default for the whole exchange, including redirect hops.
const DEFAULT_TOTAL_TIMEOUT: Duration = Duration::from_secs(120);
/// Ids cancelled before `begin` registered them. Bounded: the oldest record is
/// dropped first, so cancelling arbitrarily many unknown ids cannot grow memory.
const MAX_EARLY_CANCELS: usize = 64;
/// Recently completed ids. A cancel that loses the completion race is a no-op
/// instead of occupying an early-cancel slot: ids are unique for the lifetime
/// of a client, so a tombstone would be meaningless and could evict a live
/// pre-registration record.
const MAX_RECENT_COMPLETIONS: usize = 256;
const DEFAULT_USER_AGENT: &str = "capsulad-native/1.0";

pub static HTTP_CLIENTS: Registry<HttpClientInner> = Registry::new();

#[derive(Debug, Clone)]
pub struct HttpOptions {
    pub roots: RootsMode,
    pub ca_pem: Option<Vec<u8>>,
    pub user_agent: String,
    pub max_body: usize,
    pub follow_redirects: u32,
    pub connect_timeout: Duration,
    pub read_timeout: Duration,
    /// Whole-request budget, covering every redirect hop. A per-request
    /// `timeout_ms` overrides it.
    pub total_timeout: Duration,
}

impl Default for HttpOptions {
    fn default() -> Self {
        Self {
            roots: RootsMode::System,
            ca_pem: None,
            user_agent: DEFAULT_USER_AGENT.to_string(),
            max_body: DEFAULT_MAX_BODY,
            follow_redirects: MAX_REDIRECTS,
            connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            read_timeout: DEFAULT_READ_TIMEOUT,
            total_timeout: DEFAULT_TOTAL_TIMEOUT,
        }
    }
}

impl HttpOptions {
    /// Parses the snake_case options object. Unknown keys are rejected so a
    /// misspelled security option cannot be silently ignored.
    pub fn parse(bytes: &[u8]) -> SdkResult<Self> {
        let mut options = Self::default();
        if bytes.is_empty() {
            return Ok(options);
        }
        let value: serde_json::Value = serde_json::from_slice(bytes).map_err(|err| {
            SdkError::new(
                INVALID_ARGUMENT,
                format!("http options JSON is not valid: {err}"),
            )
        })?;
        let object = value
            .as_object()
            .ok_or_else(|| SdkError::new(INVALID_ARGUMENT, "http options must be a JSON object"))?;
        for (key, value) in object {
            match key.as_str() {
                "roots" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "http roots must be a string")
                    })?;
                    options.roots = match text {
                        "system" => RootsMode::System,
                        "webpki" => RootsMode::Webpki,
                        "pem" => RootsMode::Pem,
                        other => {
                            return Err(SdkError::new(
                                INVALID_ARGUMENT,
                                format!("unknown http roots {other:?}; want system, webpki or pem"),
                            ))
                        }
                    };
                }
                "ca_pem" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "http ca_pem must be a string")
                    })?;
                    if !text.is_empty() {
                        options.ca_pem = Some(text.as_bytes().to_vec());
                    }
                }
                "user_agent" => {
                    let text = value.as_str().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "http user_agent must be a string")
                    })?;
                    if text.is_empty()
                        || text
                            .bytes()
                            .any(|b| b == b'\r' || b == b'\n' || b < 0x20 || b == 0x7f)
                    {
                        return Err(SdkError::new(
                            INVALID_ARGUMENT,
                            "http user_agent is empty or contains control characters",
                        ));
                    }
                    options.user_agent = text.to_string();
                }
                "max_body" => {
                    let number = value.as_u64().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "http max_body must be an integer")
                    })?;
                    if number == 0 || number > HARD_MAX_BODY as u64 {
                        return Err(SdkError::new(
                            INVALID_ARGUMENT,
                            format!("http max_body must be between 1 and {HARD_MAX_BODY} bytes"),
                        ));
                    }
                    options.max_body = number as usize;
                }
                "follow_redirects" => {
                    let number = value.as_u64().ok_or_else(|| {
                        SdkError::new(INVALID_ARGUMENT, "http follow_redirects must be an integer")
                    })?;
                    if number > MAX_REDIRECTS as u64 {
                        return Err(SdkError::new(
                            INVALID_ARGUMENT,
                            format!("http follow_redirects is capped at {MAX_REDIRECTS}"),
                        ));
                    }
                    options.follow_redirects = number as u32;
                }
                "connect_timeout_ms" => {
                    options.connect_timeout = duration_ms(value, "connect_timeout_ms")?;
                }
                "read_timeout_ms" => {
                    options.read_timeout = duration_ms(value, "read_timeout_ms")?;
                }
                "total_timeout_ms" => {
                    // 0 is the documented spelling of "use the default"; it is
                    // never "unlimited".
                    let number = value
                        .as_u64()
                        .filter(|value| *value <= u64::from(u32::MAX))
                        .ok_or_else(|| {
                            SdkError::new(
                                INVALID_ARGUMENT,
                                format!(
                                    "http total_timeout_ms must be an integer between 0 and {}",
                                    u32::MAX
                                ),
                            )
                        })?;
                    options.total_timeout = if number == 0 {
                        DEFAULT_TOTAL_TIMEOUT
                    } else {
                        Duration::from_millis(number)
                    };
                }
                other => {
                    return Err(SdkError::new(
                        INVALID_ARGUMENT,
                        format!("unknown http option {other:?}"),
                    ))
                }
            }
        }
        Ok(options)
    }
}

fn duration_ms(value: &serde_json::Value, name: &str) -> SdkResult<Duration> {
    let number = value
        .as_u64()
        .filter(|value| *value > 0 && *value <= u64::from(u32::MAX))
        .ok_or_else(|| {
            SdkError::new(
                INVALID_ARGUMENT,
                format!("http {name} must be an integer between 1 and {}", u32::MAX),
            )
        })?;
    Ok(Duration::from_millis(number))
}

pub struct HttpClientInner {
    pub options: HttpOptions,
    tls: Arc<rustls::ClientConfig>,
    active: Mutex<ActiveState>,
    idle: Condvar,
}

#[derive(Default)]
struct ActiveState {
    closed: bool,
    requests: HashMap<u64, tokio::sync::watch::Sender<()>>,
    /// Ids cancelled before `begin` saw them. Consumed by the matching
    /// `begin`, which then refuses to register a doomed exchange.
    early_cancels: VecDeque<u64>,
    /// Ids whose registration already ended. A cancel that arrives after
    /// completion is a no-op instead of arming a tombstone for a later reuse.
    completed: VecDeque<u64>,
}

/// Appends to a bounded FIFO, evicting the oldest entry at the cap.
fn push_bounded(queue: &mut VecDeque<u64>, value: u64, limit: usize) {
    if queue.len() == limit {
        queue.pop_front();
    }
    queue.push_back(value);
}

impl HttpClientInner {
    pub fn open(options_json: &[u8]) -> SdkResult<Arc<Self>> {
        let options = HttpOptions::parse(options_json)?;
        let tls = tls::client_config(
            &TlsOptions {
                roots: options.roots,
                ca_pem: options.ca_pem.clone(),
            },
            false,
        )?;
        Ok(Arc::new(Self {
            options,
            tls,
            active: Mutex::new(ActiveState::default()),
            idle: Condvar::new(),
        }))
    }

    /// Aborts one in-flight request by its caller-supplied id.
    ///
    /// Cancellation is sticky across the registration race: an id that is not
    /// in flight yet is remembered (bounded, oldest evicted at 64) and the
    /// matching `begin` then returns `CANCELLED` instead of registering a
    /// doomed exchange. An id whose registration already ended is a no-op, so
    /// a cancellation watcher cannot evict a live pre-registration record or
    /// affect a new request. Ids are unique for the client lifetime, so this
    /// never needs to reason about reuse; id 0 is never a request and is a
    /// no-op here.
    pub fn cancel(&self, request_id: u64) {
        if request_id == 0 {
            return;
        }
        let mut state = self.active.lock().unwrap_or_else(|err| err.into_inner());
        if let Some(sender) = state.requests.get(&request_id) {
            sender.send_replace(());
            return;
        }
        if state.completed.contains(&request_id) {
            return;
        }
        if !state.early_cancels.contains(&request_id) {
            push_bounded(&mut state.early_cancels, request_id, MAX_EARLY_CANCELS);
        }
    }

    /// Cancels every in-flight request and, when called from an application
    /// thread, waits until none can be touching the TLS and root state. After
    /// this returns the client is closed and refuses new requests.
    ///
    /// A call from a Tokio runtime thread returns without blocking: on a
    /// current-thread runtime that thread may be the only one able to drop the
    /// in-flight future. Requests hold Arc snapshots of the TLS/root state, so
    /// it stays valid, and the handle is already removed and closed.
    pub fn shutdown(&self) {
        {
            let mut state = self.active.lock().unwrap_or_else(|err| err.into_inner());
            state.closed = true;
            for sender in state.requests.values() {
                sender.send_replace(());
            }
        }
        // Never block a Tokio worker: on a current-thread runtime that worker
        // may be the only one able to drop the in-flight future. Requests hold
        // Arc snapshots of the TLS/root state, so it stays valid, and the
        // handle is already removed and closed. The C ABI calls this from an
        // application thread, where it waits for the in-flight requests.
        if tokio::runtime::Handle::try_current().is_ok() {
            return;
        }
        let mut state = self.active.lock().unwrap_or_else(|err| err.into_inner());
        while !state.requests.is_empty() {
            state = self.idle.wait(state).unwrap_or_else(|err| err.into_inner());
        }
    }

    fn begin(
        self: &Arc<Self>,
        request_id: u64,
    ) -> SdkResult<(ActiveGuard, tokio::sync::watch::Receiver<()>)> {
        let mut state = self.active.lock().unwrap_or_else(|err| err.into_inner());
        if state.closed {
            return Err(SdkError::new(CANCELLED, "http client is closed"));
        }
        if request_id == 0 {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "http request id must not be zero",
            ));
        }
        // A cancel that arrived before this registration is honoured here:
        // consume the record and fail without registering, so the request never
        // reaches the network.
        if let Some(index) = state.early_cancels.iter().position(|id| *id == request_id) {
            state.early_cancels.remove(index);
            return Err(SdkError::new(
                CANCELLED,
                "http request was cancelled before it started",
            ));
        }
        if state.requests.contains_key(&request_id) {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("http request id {request_id} is already in flight"),
            ));
        }
        // Register under the same lock shutdown() takes, so a concurrent close
        // is never missed: either begin() sees `closed`, or close signals the
        // freshly registered sender.
        let (sender, receiver) = tokio::sync::watch::channel(());
        state.requests.insert(request_id, sender);
        Ok((
            ActiveGuard {
                client: self.clone(),
                request_id,
            },
            receiver,
        ))
    }

    pub fn info_json(&self) -> Vec<u8> {
        let value = serde_json::json!({
            "kind": "http",
            "roots": match self.options.roots {
                RootsMode::System => "system",
                RootsMode::Webpki => "webpki",
                RootsMode::Pem => "pem",
            },
            "user_agent": self.options.user_agent,
            "max_body": self.options.max_body,
            "follow_redirects": self.options.follow_redirects,
            "connect_timeout_ms": self.options.connect_timeout.as_millis() as u64,
            "read_timeout_ms": self.options.read_timeout.as_millis() as u64,
            "total_timeout_ms": self.options.total_timeout.as_millis() as u64,
        });
        serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
    }
}

pub fn open(options_json: &[u8]) -> SdkResult<u64> {
    let client = HttpClientInner::open(options_json)?;
    Ok(HTTP_CLIENTS.insert(client))
}

/// Closes one HTTP client. Every in-flight request is cancelled. The call then
/// waits until none can still touch the TLS/root state, unless it runs on a
/// Tokio runtime thread, where it returns without blocking (requests hold Arc
/// snapshots, so that state stays valid) and the handle is already unusable.
pub fn close(handle: u64) -> SdkResult<()> {
    let client = HTTP_CLIENTS
        .remove(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown http handle"))?;
    client.shutdown();
    Ok(())
}

/// Aborts one in-flight request on `handle`; see [`HttpClientInner::cancel`] for
/// the sticky, bounded pre-registration behaviour. Unknown handles are
/// `NOT_FOUND`; a matching id that is already finished is a successful no-op.
pub fn cancel(handle: u64, request_id: u64) -> SdkResult<()> {
    HTTP_CLIENTS
        .get(handle)
        .ok_or_else(|| SdkError::new(crate::ffi::NOT_FOUND, "unknown http handle"))?
        .cancel(request_id);
    Ok(())
}

/// Keeps a request registered until its future is dropped, including the
/// early-return/error paths.
struct ActiveGuard {
    client: Arc<HttpClientInner>,
    request_id: u64,
}

impl Drop for ActiveGuard {
    fn drop(&mut self) {
        let mut state = self
            .client
            .active
            .lock()
            .unwrap_or_else(|err| err.into_inner());
        state.requests.remove(&self.request_id);
        push_bounded(
            &mut state.completed,
            self.request_id,
            MAX_RECENT_COMPLETIONS,
        );
        if state.requests.is_empty() {
            self.client.idle.notify_all();
        }
    }
}

pub struct HttpRequest {
    pub method: String,
    pub url: String,
    pub headers: Vec<(String, String)>,
    pub body: Vec<u8>,
}

#[derive(Debug)]
pub struct HttpResponse {
    pub status: i32,
    pub headers_json: Vec<u8>,
    pub body: Vec<u8>,
}

pub async fn request(
    client: Arc<HttpClientInner>,
    request_id: u64,
    req: HttpRequest,
    timeout: Option<Duration>,
) -> SdkResult<HttpResponse> {
    // The caller's per-request deadline wins; otherwise the client's total
    // budget (default 120 s) bounds the whole exchange, including redirects.
    let limit = timeout.unwrap_or(client.options.total_timeout);
    let (_active, mut cancelled) = client.begin(request_id)?;
    let work = async move {
        let future = request_inner(client, req);
        tokio::time::timeout(limit, future).await.map_err(|_| {
            SdkError::new(TIMEOUT, format!("http request timed out after {limit:?}"))
        })?
    };
    tokio::select! {
        biased;
        _ = async {
            // A closed sender (never in practice) is also treated as cancelled.
            let _ = cancelled.changed().await;
        } => Err(SdkError::new(CANCELLED, "http request cancelled")),
        result = work => result,
    }
}

async fn request_inner(client: Arc<HttpClientInner>, req: HttpRequest) -> SdkResult<HttpResponse> {
    let mut method = req.method.trim().to_string();
    if method.is_empty() || http::Method::from_bytes(method.as_bytes()).is_err() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            format!("invalid HTTP method {:?}", req.method),
        ));
    }
    let mut url = parse_url(&req.url)?;
    let mut caller_headers = headers::validate_http(req.headers)?;
    let mut body = req.body;
    let mut redirects = 0u32;
    loop {
        let response = exchange(&client, &url, &method, &caller_headers, &body).await?;
        let status = response.status;
        if !matches!(status, 301 | 302 | 303 | 307 | 308) || client.options.follow_redirects == 0 {
            return Ok(response.into_public());
        }
        let Some(location) = header_last(&response.headers, "location").map(str::to_string) else {
            return Ok(response.into_public());
        };
        if redirects >= client.options.follow_redirects {
            return Err(SdkError::new(
                TRANSPORT,
                format!(
                    "too many redirects (limit {})",
                    client.options.follow_redirects
                ),
            ));
        }
        // A malformed or non-http(s) Location is a server-side protocol
        // failure, not a caller argument error.
        let next = join_location(&url, &location).map_err(|err| {
            SdkError::new(
                TRANSPORT,
                format!("invalid redirect Location {location:?}: {}", err.message),
            )
        })?;
        // Never follow a redirect that downgrades a verified TLS channel to
        // cleartext: the target would be fetched without transport
        // authentication. Fail closed instead of following.
        if url.scheme == "https" && next.scheme == "http" {
            return Err(SdkError::new(
                TRANSPORT,
                format!("refusing https->http redirect to {location:?}"),
            ));
        }
        if next.scheme != url.scheme || next.host != url.host || next.port != url.port {
            // Cross-origin: credentials and application headers do not travel.
            caller_headers.retain(|(name, _)| name == "user-agent");
        }
        if matches!(status, 301 | 302 | 303) {
            caller_headers.retain(|(name, _)| name != "content-type");
            // RFC 9110: 303 means "GET for everything but HEAD"; 301/302 keep
            // HEAD as HEAD. Either way the redirect request carries no body.
            if !method.eq_ignore_ascii_case("HEAD") {
                method = "GET".to_string();
            }
            body.clear();
        }
        redirects += 1;
        url = next;
    }
}

struct RawResponse {
    status: u16,
    headers: Vec<(String, String)>,
    body: Vec<u8>,
}

impl RawResponse {
    fn into_public(self) -> HttpResponse {
        let map: BTreeMap<String, String> = self.headers.into_iter().collect();
        HttpResponse {
            status: i32::from(self.status),
            headers_json: serde_json::to_vec(&map).unwrap_or_else(|_| b"{}".to_vec()),
            body: self.body,
        }
    }
}

fn authority(url: &ParsedUrl) -> String {
    let host = if url.host.contains(':') {
        format!("[{}]", url.host)
    } else {
        url.host.clone()
    };
    let default = if url.scheme == "https" { 443 } else { 80 };
    if url.port == default {
        host
    } else {
        format!("{host}:{}", url.port)
    }
}

async fn exchange(
    client: &HttpClientInner,
    url: &ParsedUrl,
    method: &str,
    caller_headers: &[(String, String)],
    body: &[u8],
) -> SdkResult<RawResponse> {
    let tls_config = if url.scheme == "https" {
        Some(client.tls.clone())
    } else {
        None
    };
    let stream = tls::connect(
        &url.host,
        url.port,
        tls_config,
        Some(&url.host),
        client.options.connect_timeout,
    )
    .await?;
    let mut conn = Conn {
        stream,
        buf: Vec::new(),
        read_timeout: client.options.read_timeout,
    };

    let mut head = Vec::with_capacity(512);
    write!(head, "{} {} HTTP/1.1\r\n", method, url.path_and_query())
        .map_err(|err| SdkError::new(TRANSPORT, err.to_string()))?;
    write!(head, "Host: {}\r\n", authority(url))
        .map_err(|err| SdkError::new(TRANSPORT, err.to_string()))?;

    let mut caller_has_agent = false;
    let mut caller_has_encoding = false;
    for (name, _) in caller_headers {
        caller_has_agent |= name == "user-agent";
        caller_has_encoding |= name == "accept-encoding";
    }
    if !caller_has_agent {
        write!(head, "User-Agent: {}\r\n", client.options.user_agent)
            .map_err(|err| SdkError::new(TRANSPORT, err.to_string()))?;
    }
    if !caller_has_encoding {
        head.extend_from_slice(b"Accept-Encoding: identity\r\n");
    }
    head.extend_from_slice(b"Connection: close\r\n");
    write!(head, "Content-Length: {}\r\n", body.len())
        .map_err(|err| SdkError::new(TRANSPORT, err.to_string()))?;
    for (name, value) in caller_headers {
        write!(head, "{name}: {value}\r\n")
            .map_err(|err| SdkError::new(TRANSPORT, err.to_string()))?;
    }
    head.extend_from_slice(b"\r\n");
    head.extend_from_slice(body);

    conn.write_all(&head).await?;

    let mut interim = 0u32;
    let (status, response_headers) = loop {
        let (status, headers) = conn.read_head().await?;
        if status >= 200 {
            break (status, headers);
        }
        interim += 1;
        if interim > MAX_INTERIM {
            return Err(SdkError::new(
                TRANSPORT,
                format!("more than {MAX_INTERIM} interim 1xx responses"),
            ));
        }
    };

    let no_body = method.eq_ignore_ascii_case("HEAD")
        || status == 204
        || status == 304
        || (100..200).contains(&status);
    let mut raw_body = Vec::new();
    if !no_body {
        let max = client.options.max_body;
        if let Some(encoding) = header_last(&response_headers, "transfer-encoding") {
            if !encoding
                .split(',')
                .last()
                .map(|value| value.trim().eq_ignore_ascii_case("chunked"))
                .unwrap_or(false)
            {
                return Err(SdkError::new(
                    TRANSPORT,
                    format!("unsupported transfer-encoding {encoding:?}"),
                ));
            }
            raw_body = conn.read_chunked(max).await?;
        } else if let Some(length) = header_last(&response_headers, "content-length") {
            let length: usize = length.parse().map_err(|_| {
                SdkError::new(TRANSPORT, format!("invalid content-length {length:?}"))
            })?;
            if length > max {
                return Err(SdkError::new(
                    TOO_LARGE,
                    format!("response body of {length} bytes exceeds max_body {max}"),
                ));
            }
            raw_body = conn.take(length).await?;
        } else {
            raw_body = conn.read_to_end(max).await?;
        }
    }
    let mut headers: Vec<(String, String)> = response_headers
        .into_iter()
        .map(|(name, value)| (name, value))
        .collect();
    headers.shrink_to_fit();
    Ok(RawResponse {
        status,
        headers,
        body: raw_body,
    })
}

fn header_last<'a>(headers: &'a [(String, String)], name: &str) -> Option<&'a str> {
    headers
        .iter()
        .rev()
        .find(|(key, _)| key.eq_ignore_ascii_case(name))
        .map(|(_, value)| value.as_str())
}

struct Conn {
    stream: NetStream,
    buf: Vec<u8>,
    read_timeout: Duration,
}

impl Conn {
    async fn fill(&mut self) -> SdkResult<usize> {
        let mut chunk = [0u8; 8192];
        let read = tokio::time::timeout(self.read_timeout, self.stream.read(&mut chunk))
            .await
            .map_err(|_| {
                SdkError::new(
                    TIMEOUT,
                    format!("http read timed out after {:?}", self.read_timeout),
                )
            })?
            .map_err(|err| SdkError::new(TRANSPORT, format!("http read: {err}")))?;
        self.buf.extend_from_slice(&chunk[..read]);
        Ok(read)
    }

    async fn write_all(&mut self, bytes: &[u8]) -> SdkResult<()> {
        tokio::time::timeout(self.read_timeout, self.stream.write_all(bytes))
            .await
            .map_err(|_| {
                SdkError::new(
                    TIMEOUT,
                    format!("http write timed out after {:?}", self.read_timeout),
                )
            })?
            .map_err(|err| SdkError::new(TRANSPORT, format!("http write: {err}")))
    }

    async fn take(&mut self, len: usize) -> SdkResult<Vec<u8>> {
        while self.buf.len() < len {
            if self.fill().await? == 0 {
                return Err(SdkError::new(
                    TRANSPORT,
                    "connection closed before the response body was complete",
                ));
            }
        }
        let rest = self.buf.split_off(len);
        Ok(std::mem::replace(&mut self.buf, rest))
    }

    async fn read_line(&mut self, limit: usize) -> SdkResult<Vec<u8>> {
        loop {
            if let Some(index) = find(&self.buf, b"\r\n") {
                let mut line: Vec<u8> = self.buf.drain(..index + 2).collect();
                line.truncate(index);
                return Ok(line);
            }
            if self.buf.len() > limit {
                return Err(SdkError::new(
                    TOO_LARGE,
                    "http chunk header exceeds the size limit",
                ));
            }
            if self.fill().await? == 0 {
                return Err(SdkError::new(TRANSPORT, "unexpected end of chunked body"));
            }
        }
    }

    async fn read_head(&mut self) -> SdkResult<(u16, Vec<(String, String)>)> {
        loop {
            if let Some(index) = find(&self.buf, b"\r\n\r\n") {
                let head: Vec<u8> = self.buf.drain(..index + 4).collect();
                return parse_head(&head[..index]);
            }
            if self.buf.len() > MAX_HEAD {
                return Err(SdkError::new(
                    TOO_LARGE,
                    format!("response status line and headers exceed {MAX_HEAD} bytes"),
                ));
            }
            if self.fill().await? == 0 {
                return Err(SdkError::new(
                    TRANSPORT,
                    "connection closed before response headers",
                ));
            }
        }
    }

    async fn read_chunked(&mut self, max: usize) -> SdkResult<Vec<u8>> {
        let mut out = Vec::new();
        loop {
            let line = self.read_line(8192).await?;
            let text = std::str::from_utf8(&line)
                .map_err(|_| SdkError::new(TRANSPORT, "non-ASCII chunk size"))?;
            let size_text = text.split(';').next().unwrap_or("").trim();
            let size = usize::from_str_radix(size_text, 16).map_err(|_| {
                SdkError::new(TRANSPORT, format!("invalid chunk size {size_text:?}"))
            })?;
            if size == 0 {
                // Consume optional trailers up to the blank line, inside a
                // bounded budget: a server must not be able to stream trailers
                // forever or exceed one header block.
                let mut trailer_bytes = 0usize;
                let mut trailer_lines = 0usize;
                loop {
                    let trailer = self.read_line(MAX_HEAD).await?;
                    if trailer.is_empty() {
                        break;
                    }
                    trailer_lines += 1;
                    trailer_bytes = trailer_bytes.saturating_add(trailer.len() + 2);
                    if trailer_lines > MAX_TRAILER_LINES || trailer_bytes > MAX_HEAD {
                        return Err(SdkError::new(
                            TOO_LARGE,
                            format!(
                                "response trailers exceed {MAX_TRAILER_LINES} lines or {MAX_HEAD} bytes"
                            ),
                        ));
                    }
                }
                return Ok(out);
            }
            if out.len().saturating_add(size) > max {
                return Err(SdkError::new(
                    TOO_LARGE,
                    format!("response body exceeds max_body {max}"),
                ));
            }
            out.extend_from_slice(&self.take(size).await?);
            let crlf = self.take(2).await?;
            if crlf != b"\r\n" {
                return Err(SdkError::new(TRANSPORT, "chunk is not CRLF terminated"));
            }
        }
    }

    async fn read_to_end(&mut self, max: usize) -> SdkResult<Vec<u8>> {
        loop {
            if self.buf.len() > max {
                return Err(SdkError::new(
                    TOO_LARGE,
                    format!("response body exceeds max_body {max}"),
                ));
            }
            if self.fill().await? == 0 {
                return Ok(std::mem::take(&mut self.buf));
            }
        }
    }
}

fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

fn parse_head(head: &[u8]) -> SdkResult<(u16, Vec<(String, String)>)> {
    // The whole block must be ASCII. Obs-text and Unicode line separators have
    // no place in a field block, and admitting them would let a peer smuggle
    // C1 controls past a downstream consumer.
    if !head.is_ascii() {
        return Err(SdkError::new(TRANSPORT, "response headers are not ASCII"));
    }
    let text = std::str::from_utf8(head)
        .map_err(|_| SdkError::new(TRANSPORT, "response headers are not ASCII"))?;
    let mut lines = text.split("\r\n");
    let status_line = lines
        .next()
        .ok_or_else(|| SdkError::new(TRANSPORT, "empty response"))?;
    let mut parts = status_line.splitn(3, ' ');
    let version = parts.next().unwrap_or("");
    if !(version.starts_with("HTTP/1.") || version.starts_with("HTTP/2")) {
        return Err(SdkError::new(
            TRANSPORT,
            format!("unsupported HTTP response version {version:?}"),
        ));
    }
    let status: u16 = parts
        .next()
        .and_then(|value| value.parse().ok())
        .filter(|value| (100..=599).contains(value))
        .ok_or_else(|| SdkError::new(TRANSPORT, "invalid HTTP status line"))?;
    let mut headers = Vec::new();
    for line in lines {
        if line.is_empty() {
            continue;
        }
        if line.starts_with(' ') || line.starts_with('\t') {
            return Err(SdkError::new(
                TRANSPORT,
                "folded HTTP response headers are not supported",
            ));
        }
        let Some((name, value)) = line.split_once(':') else {
            return Err(SdkError::new(
                TRANSPORT,
                format!("malformed response header {line:?}"),
            ));
        };
        let name = http::HeaderName::from_bytes(name.as_bytes()).map_err(|_| {
            SdkError::new(
                TRANSPORT,
                format!("malformed response header name {name:?}"),
            )
        })?;
        let value = value.trim_matches([' ', '\t']);
        // RFC 9110 field-vchar plus HTAB; everything else is a protocol
        // failure. A lone CR/LF/NUL would otherwise survive into the JSON
        // header map and could be replayed as a second header.
        if value
            .bytes()
            .any(|b| !(b == b'\t' || (0x20..=0x7e).contains(&b)))
        {
            return Err(SdkError::new(
                TRANSPORT,
                format!("response header {name:?} contains a non-ASCII or control byte"),
            ));
        }
        headers.push((name.as_str().to_string(), value.to_string()));
    }
    Ok((status, headers))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn options_reject_unknown_and_out_of_range_values() {
        assert!(HttpOptions::parse(br#"{"insecure":true}"#).is_err());
        assert!(HttpOptions::parse(br#"{"max_body":0}"#).is_err());
        assert!(HttpOptions::parse(br#"{"max_body":67108865}"#).is_err());
        assert!(HttpOptions::parse(br#"{"follow_redirects":5}"#).is_err());
        assert!(HttpOptions::parse(br#"{"roots":"nope"}"#).is_err());
        assert!(HttpOptions::parse(br#"{"total_timeout_ms":-1}"#).is_err());
        assert!(HttpOptions::parse(br#"{"total_timeout_ms":4294967296}"#).is_err());
        let options = HttpOptions::parse(
            br#"{"roots":"webpki","user_agent":"t","max_body":1024,"follow_redirects":0}"#,
        )
        .unwrap();
        assert_eq!(options.roots, RootsMode::Webpki);
        assert_eq!(options.max_body, 1024);
        assert_eq!(options.follow_redirects, 0);
        assert!(HttpOptions::parse(b"").is_ok());
    }

    #[test]
    fn total_timeout_zero_means_the_default_not_unlimited() {
        let options = HttpOptions::parse(br#"{"total_timeout_ms":0}"#).unwrap();
        assert_eq!(options.total_timeout, Duration::from_secs(120));
        let options = HttpOptions::parse(br#"{"total_timeout_ms":250}"#).unwrap();
        assert_eq!(options.total_timeout, Duration::from_millis(250));
        assert_eq!(
            HttpOptions::default().total_timeout,
            Duration::from_secs(120)
        );
    }

    #[test]
    fn pem_roots_require_supplied_anchors() {
        assert!(HttpOptions::parse(br#"{"roots":"pem"}"#).is_ok());
        // Building the client must fail: "pem" never falls back to the system
        // bundle or the Mozilla roots.
        let err = HttpClientInner::open(br#"{"roots":"pem"}"#)
            .err()
            .expect("pem without ca_pem must not build");
        assert_eq!(err.status, INVALID_ARGUMENT);
        let options = HttpOptions::parse(br#"{"roots":"pem","ca_pem":"x"}"#).unwrap();
        assert_eq!(options.roots, RootsMode::Pem);
    }

    #[test]
    fn cancel_before_begin_is_sticky_and_consumed_once() {
        let client = HttpClientInner::open(br#"{"roots":"webpki"}"#).unwrap();
        client.cancel(7);
        let err = client
            .begin(7)
            .err()
            .expect("a pre-registration cancel must refuse the exchange");
        assert_eq!(err.status, CANCELLED);
        // The record is consumed by the refused registration.
        assert!(!client.active.lock().unwrap().early_cancels.contains(&7));
        // A different, never-used id is unaffected.
        assert!(client.begin(8).is_ok());
    }

    #[test]
    fn late_cancel_for_a_completed_id_is_a_no_op() {
        let client = HttpClientInner::open(br#"{"roots":"webpki"}"#).unwrap();
        drop(client.begin(9).expect("registration"));
        // The id is completed: the late cancel records no tombstone and does
        // not consume an early-cancel slot.
        client.cancel(9);
        assert!(!client.active.lock().unwrap().early_cancels.contains(&9));
        // A new, never-used id is unaffected.
        assert!(client.begin(10).is_ok());
    }

    #[test]
    fn zero_request_id_cancel_is_a_no_op() {
        let client = HttpClientInner::open(br#"{"roots":"webpki"}"#).unwrap();
        client.cancel(0);
        assert!(client.active.lock().unwrap().early_cancels.is_empty());
    }

    #[test]
    fn early_cancel_records_are_bounded() {
        let client = HttpClientInner::open(br#"{"roots":"webpki"}"#).unwrap();
        for id in 1..=1000 {
            client.cancel(id);
        }
        let state = client.active.lock().unwrap();
        assert_eq!(state.early_cancels.len(), MAX_EARLY_CANCELS);
        assert!(!state.early_cancels.contains(&1));
        assert!(state.early_cancels.contains(&1000));
    }

    #[test]
    fn response_header_control_bytes_are_rejected() {
        for head in [
            // Lone CR/LF/NUL and DEL inside a value.
            b"HTTP/1.1 200 OK\r\nX-Test: a\nb".to_vec(),
            b"HTTP/1.1 200 OK\r\nX-Test: a\rb".to_vec(),
            b"HTTP/1.1 200 OK\r\nX-Test: a\0b".to_vec(),
            b"HTTP/1.1 200 OK\r\nX-Test: a\x7fb".to_vec(),
            // Raw C1 control 0x85 and its UTF-8 form U+0085.
            b"HTTP/1.1 200 OK\r\nX-Test: a\x85b".to_vec(),
            b"HTTP/1.1 200 OK\r\nX-Test: a\xc2\x85b".to_vec(),
            // Unicode line separator U+2028.
            b"HTTP/1.1 200 OK\r\nX-Test: a\xe2\x80\xa8b".to_vec(),
            // Non-ASCII value bytes are obs-text, not a header.
            b"HTTP/1.1 200 OK\r\nX-Test: caf\xc3\xa9".to_vec(),
            // Control byte in a name.
            b"HTTP/1.1 200 OK\r\nX-\x01Test: a".to_vec(),
            // Names must be RFC 7230 tokens: no spaces, separators or
            // whitespace before the colon.
            b"HTTP/1.1 200 OK\r\nX y: a".to_vec(),
            b"HTTP/1.1 200 OK\r\nX(y): a".to_vec(),
            b"HTTP/1.1 200 OK\r\nX-Test : a".to_vec(),
            b"HTTP/1.1 200 OK\r\nX-\xc3\xa9: a".to_vec(),
        ] {
            let err = parse_head(&head).expect_err("control bytes must be rejected");
            assert_eq!(err.status, TRANSPORT, "{}", String::from_utf8_lossy(&head));
        }
        // HTAB inside a value is legal and survives trimming at the edges.
        let (status, headers) =
            parse_head(b"HTTP/1.1 200 OK\r\nX-Test: a\tb").expect("HTAB is allowed");
        assert_eq!(status, 200);
        assert_eq!(headers, vec![("x-test".to_string(), "a\tb".to_string())]);
        // Token punctuation and case normalization keep working.
        let (_, headers) =
            parse_head(b"HTTP/1.1 200 OK\r\nX-Weird_T.1: v").expect("token name is allowed");
        assert_eq!(headers, vec![("x-weird_t.1".to_string(), "v".to_string())]);
    }
}
