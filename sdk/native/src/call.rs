//! Client-initiated calls and app-served inbound calls.

use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::mpsc::{Receiver, RecvTimeoutError, SyncSender, TrySendError};
use std::sync::{Arc, Mutex, Weak};
use std::time::Duration;

use bytes::{Buf, BufMut};
use http::uri::PathAndQuery;
use prost_reflect::MessageDescriptor;
use tonic::codec::{Codec, DecodeBuf, Decoder, EncodeBuf, Encoder};

use crate::client::ClientInner;
use crate::dynamic::{self, RoutedResponse};
use crate::ffi::{
    self, SdkError, SdkResult, BUSY, CANCELLED, CLOSED, DESCRIPTOR_NOT_FOUND, GRPC,
    INVALID_ARGUMENT, INVALID_MESSAGE, NOT_FOUND, OK, TRANSPORT,
};
use crate::registry::Registry;

pub static CALLS: Registry<CallInner> = Registry::new();

// EVENT_QUEUE bounds the response backlog between the async transport task and
// the consumer thread. It must be larger than the transport's flow-control
// window (roughly 100 audio chunks): a short scheduling stall in the consumer
// (or an app intentionally paused by an operator) must create backpressure,
// not fail a live audio call. 512 chunks is about 10 s of 20 ms microphone
// frames and stays a bounded, small allocation.
pub const EVENT_QUEUE: usize = 512;
pub const OUT_QUEUE: usize = 256;

/* ------------------------------------------------------------------ *
 * Raw gRPC codec: payload bytes stay opaque, no descriptor required.
 * ------------------------------------------------------------------ */

#[derive(Debug)]
pub struct RawMessage(pub Vec<u8>);

#[derive(Debug, Clone, Copy, Default)]
pub struct RawCodec;

pub struct RawEncoder;
pub struct RawDecoder;

impl Encoder for RawEncoder {
    type Item = RawMessage;
    type Error = tonic::Status;

    fn encode(&mut self, item: Self::Item, dst: &mut EncodeBuf<'_>) -> Result<(), Self::Error> {
        dst.put_slice(&item.0);
        Ok(())
    }
}

impl Decoder for RawDecoder {
    type Item = RawMessage;
    type Error = tonic::Status;

    fn decode(&mut self, src: &mut DecodeBuf<'_>) -> Result<Option<Self::Item>, Self::Error> {
        let len = src.remaining();
        Ok(Some(RawMessage(src.copy_to_bytes(len).to_vec())))
    }
}

impl Codec for RawCodec {
    type Encode = RawMessage;
    type Decode = RawMessage;
    type Encoder = RawEncoder;
    type Decoder = RawDecoder;

    fn encoder(&mut self) -> Self::Encoder {
        RawEncoder
    }

    fn decoder(&mut self) -> Self::Decoder {
        RawDecoder
    }
}

/* ------------------------------------------------------------------ *
 * Shared status
 * ------------------------------------------------------------------ */

#[derive(Debug, Clone)]
pub struct FinalStatus {
    pub status: i32,
    pub code: i32,
    pub detail: String,
}

impl FinalStatus {
    pub fn ok() -> Self {
        Self {
            status: OK,
            code: 0,
            detail: String::new(),
        }
    }

    pub fn error(status: i32, code: i32, detail: impl Into<String>) -> Self {
        Self {
            status,
            code,
            detail: detail.into(),
        }
    }

    pub fn to_error(&self) -> SdkError {
        if self.code == 0 {
            return SdkError::new(ffi::END_OF_STREAM, "end of stream");
        }
        SdkError {
            status: if self.status == OK { GRPC } else { self.status },
            message: if self.detail.is_empty() {
                format!("call ended with gRPC code {}", self.code)
            } else {
                self.detail.clone()
            },
            grpc_code: Some(self.code),
        }
    }
}

pub fn deadline_to_duration(timeout_ms: u32) -> Option<Duration> {
    if timeout_ms == 0 {
        None
    } else {
        Some(Duration::from_millis(u64::from(timeout_ms)))
    }
}

/* ------------------------------------------------------------------ *
 * Outbound (client-initiated) calls
 * ------------------------------------------------------------------ */

pub enum OutCommand {
    Message(Vec<u8>),
    HalfClose,
}

pub enum OutEvent {
    Message(Vec<u8>),
    End(FinalStatus),
}

#[derive(Clone)]
struct PlaybackState {
    channels: u32,
    bytes_per_sample: u32,
}

pub struct OutboundCall {
    pub handle: AtomicU64,
    pub client: Arc<ClientInner>,
    pub service: String,
    pub method: String,
    pub target_app_id: Option<String>,
    pub kind: i32,
    routed: bool,
    request_desc: Option<MessageDescriptor>,
    response_desc: Option<MessageDescriptor>,
    tx: tokio::sync::mpsc::Sender<OutCommand>,
    events: Mutex<Receiver<OutEvent>>,
    final_status: Mutex<Option<FinalStatus>>,
    finished: AtomicBool,
    half_closed: AtomicBool,
    task: Mutex<Option<tokio::task::JoinHandle<()>>>,
    refs: AtomicUsize,
    playback: Mutex<Option<PlaybackState>>,
    sequence: AtomicU64,
    timeout: Option<Duration>,
}

impl OutboundCall {
    pub fn new(
        client: Arc<ClientInner>,
        service: String,
        method: String,
        kind: i32,
        target_app_id: Option<String>,
        request_desc: Option<MessageDescriptor>,
        response_desc: Option<MessageDescriptor>,
        timeout: Option<Duration>,
    ) -> (
        Arc<Self>,
        tokio::sync::mpsc::Receiver<OutCommand>,
        SyncSender<OutEvent>,
    ) {
        let routed = target_app_id.is_some();
        let (tx, rx) = tokio::sync::mpsc::channel(OUT_QUEUE);
        let (event_tx, event_rx) = std::sync::mpsc::sync_channel(EVENT_QUEUE);
        let call = Arc::new(Self {
            handle: AtomicU64::new(0),
            client: client.clone(),
            service,
            method,
            target_app_id,
            kind,
            routed,
            request_desc,
            response_desc,
            tx,
            events: Mutex::new(event_rx),
            final_status: Mutex::new(None),
            finished: AtomicBool::new(false),
            half_closed: AtomicBool::new(false),
            task: Mutex::new(None),
            refs: AtomicUsize::new(1),
            playback: Mutex::new(None),
            sequence: AtomicU64::new(0),
            timeout,
        });
        (call, rx, event_tx)
    }

    /// Queue a fully encoded frame before the transport task starts. Used for
    /// the routed `RoutedCallStart` header so `call_start_routed` never blocks
    /// waiting for response headers.
    pub fn prequeue(&self, bytes: Vec<u8>) -> SdkResult<()> {
        self.queue(OutCommand::Message(bytes))
    }

    pub fn start_task(
        call: &Arc<Self>,
        rx: tokio::sync::mpsc::Receiver<OutCommand>,
        event_tx: SyncSender<OutEvent>,
    ) {
        let weak = Arc::downgrade(call);
        let runtime = crate::runtime::runtime();
        let handle = runtime.spawn(run_call(weak, rx, event_tx));
        *call.task.lock().unwrap() = Some(handle);
    }

    pub fn set_playback_format(&self, channels: u32, bytes_per_sample: u32) {
        *self.playback.lock().unwrap() = Some(PlaybackState {
            channels,
            bytes_per_sample,
        });
    }

    pub fn playback_format(&self) -> Option<(u32, u32)> {
        self.playback
            .lock()
            .unwrap()
            .as_ref()
            .map(|p| (p.channels, p.bytes_per_sample))
    }

    pub fn next_sequence(&self) -> u64 {
        self.sequence.fetch_add(1, Ordering::Relaxed)
    }

    pub fn mark_finished(&self, status: i32, code: i32, detail: impl Into<String>) -> bool {
        if self.finished.swap(true, Ordering::AcqRel) {
            return false;
        }
        *self.final_status.lock().unwrap() = Some(FinalStatus::error(status, code, detail));
        true
    }

    pub fn final_status(&self) -> Option<FinalStatus> {
        self.final_status.lock().unwrap().clone()
    }

    pub fn is_finished(&self) -> bool {
        self.finished.load(Ordering::Acquire)
    }

    fn queue(&self, command: OutCommand) -> SdkResult<()> {
        if self.is_finished() {
            return Err(SdkError::new(CLOSED, "call already finished"));
        }
        match self.tx.try_send(command) {
            Ok(()) => Ok(()),
            Err(tokio::sync::mpsc::error::TrySendError::Full(_)) => Err(SdkError::new(
                BUSY,
                "outbound call queue is full; retry after the stream drains",
            )),
            Err(tokio::sync::mpsc::error::TrySendError::Closed(_)) => {
                Err(SdkError::new(CLOSED, "call stream is closed"))
            }
        }
    }

    pub fn send_payload(&self, mut payload: Vec<u8>) -> SdkResult<()> {
        if self.routed {
            payload = dynamic::routed_message(payload)?;
        }
        self.queue(OutCommand::Message(payload))
    }

    pub fn send_json(&self, json: &[u8]) -> SdkResult<()> {
        let desc = self.request_desc.as_ref().ok_or_else(|| {
            SdkError::new(
                DESCRIPTOR_NOT_FOUND,
                format!(
                    "no request descriptor for {}/{}; register the app contract or send raw protobuf",
                    self.service, self.method
                ),
            )
        })?;
        let payload = dynamic::json_to_proto(desc, json)?;
        self.send_payload(payload)
    }

    pub fn half_close(&self) -> SdkResult<()> {
        if self.half_closed.swap(true, Ordering::AcqRel) {
            return Ok(());
        }
        if self.routed {
            self.queue(OutCommand::Message(dynamic::routed_half_close()?))?;
        }
        self.queue(OutCommand::HalfClose)
    }

    pub fn cancel(&self) -> SdkResult<()> {
        self.mark_finished(CANCELLED, 1, "call cancelled");
        if let Some(task) = self.task.lock().unwrap().take() {
            task.abort();
        }
        Ok(())
    }

    pub fn end(&self, grpc_code: i32, detail: &str) -> SdkResult<()> {
        let status = if grpc_code == 0 { OK } else { CANCELLED };
        self.mark_finished(status, grpc_code, detail);
        if let Some(task) = self.task.lock().unwrap().take() {
            task.abort();
        }
        Ok(())
    }

    pub fn recv(&self, timeout_ms: u32, as_json: bool) -> SdkResult<Vec<u8>> {
        if let Some(status) = self.final_status() {
            if status.status == CANCELLED {
                return Err(status.to_error());
            }
        }
        let timeout = if timeout_ms == u32::MAX {
            None
        } else {
            Some(Duration::from_millis(u64::from(timeout_ms)))
        };
        let event = {
            let receiver = self.events.lock().unwrap();
            match timeout {
                None => receiver.recv().map_err(|_| RecvTimeoutError::Disconnected),
                Some(duration) => receiver.recv_timeout(duration),
            }
        };
        match event {
            Ok(OutEvent::Message(bytes)) => {
                if let Some(status) = self.final_status() {
                    if status.status == CANCELLED {
                        return Err(status.to_error());
                    }
                }
                if as_json {
                    let desc = self.response_desc.as_ref().ok_or_else(|| {
                        SdkError::new(
                            DESCRIPTOR_NOT_FOUND,
                            format!(
                                "no response descriptor for {}/{}; register the app contract or use raw protobuf",
                                self.service, self.method
                            ),
                        )
                    })?;
                    dynamic::proto_to_json(desc, &bytes)
                } else {
                    Ok(bytes)
                }
            }
            Ok(OutEvent::End(status)) => Err(status.to_error()),
            Err(RecvTimeoutError::Timeout) => Err(SdkError::new(
                ffi::TIMEOUT,
                "recv timed out; call is still open",
            )),
            Err(RecvTimeoutError::Disconnected) => match self.final_status() {
                Some(status) => Err(status.to_error()),
                None => Err(SdkError::new(
                    ffi::INTERNAL,
                    "call stream ended without a status",
                )),
            },
        }
    }

    pub fn release_ref(self: &Arc<Self>) -> bool {
        let previous = self.refs.fetch_sub(1, Ordering::AcqRel);
        if previous > 1 {
            return false;
        }
        if previous == 0 {
            self.refs.store(0, Ordering::Release);
            return true;
        }
        if !self.is_finished() {
            let _ = self.cancel();
        }
        let id = self.handle.load(Ordering::Acquire);
        CALLS.remove(id);
        self.client.remove_call_child(id);
        true
    }

    pub fn retain_ref(&self) {
        self.refs.fetch_add(1, Ordering::AcqRel);
    }

    pub fn info_json(&self) -> Vec<u8> {
        let status = self.final_status();
        let value = serde_json::json!({
            "kind": self.kind,
            "direction": "outbound",
            "service": self.service,
            "method": self.method,
            "target_app_id": self.target_app_id.clone().unwrap_or_default(),
            "routed": self.routed,
            "call_id": "",
            "grpc_code": status.as_ref().map(|s| s.code).unwrap_or(-1),
            "ended": self.is_finished(),
        });
        serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
    }
}

async fn run_call(
    call: Weak<OutboundCall>,
    mut rx: tokio::sync::mpsc::Receiver<OutCommand>,
    event_tx: SyncSender<OutEvent>,
) {
    let Some(call) = call.upgrade() else {
        return;
    };
    let path_string = if call.routed {
        "/capsula.framework.v1.AppBusService/Call".to_string()
    } else {
        format!("/{}/{}", call.service, call.method)
    };
    let path: PathAndQuery = match path_string.parse() {
        Ok(path) => path,
        Err(err) => {
            finish_task(&call, &event_tx, INVALID_ARGUMENT, 3, err.to_string());
            return;
        }
    };

    let request_stream = async_stream::stream! {
        while let Some(command) = rx.recv().await {
            match command {
                OutCommand::Message(bytes) => yield RawMessage(bytes),
                OutCommand::HalfClose => break,
            }
        }
    };

    let mut request = tonic::Request::new(request_stream);
    if let Ok(value) = tonic::metadata::MetadataValue::try_from(call.client.run_id.as_str()) {
        request.metadata_mut().insert("capsula-run-id", value);
    }
    if let Some(timeout) = call.timeout {
        request.set_timeout(timeout);
    }

    let mut grpc = tonic::client::Grpc::new(call.client.channel.clone());
    if let Err(status) = grpc.ready().await {
        finish_task(&call, &event_tx, TRANSPORT, -1, status.to_string());
        return;
    }

    let response = grpc.streaming(request, path, RawCodec).await;
    let mut stream = match response {
        Ok(response) => response.into_inner(),
        Err(status) => {
            finish_task(
                &call,
                &event_tx,
                GRPC,
                status.code() as i32,
                status.message().to_string(),
            );
            return;
        }
    };

    loop {
        match stream.message().await {
            Ok(Some(raw)) => {
                if call.routed {
                    match dynamic::decode_routed_response(&raw.0) {
                        Ok(RoutedResponse::Message(payload)) => {
                            if !emit(&call, &event_tx, OutEvent::Message(payload)) {
                                return;
                            }
                        }
                        Ok(RoutedResponse::End { code, detail }) => {
                            let status = if code == 0 { OK } else { GRPC };
                            finish_task(&call, &event_tx, status, code, detail);
                            return;
                        }
                        Err(err) => {
                            finish_task(&call, &event_tx, INVALID_MESSAGE, -1, err.message);
                            return;
                        }
                    }
                } else if !emit(&call, &event_tx, OutEvent::Message(raw.0)) {
                    return;
                }
            }
            Ok(None) => {
                finish_task(&call, &event_tx, OK, 0, "");
                return;
            }
            Err(status) => {
                finish_task(
                    &call,
                    &event_tx,
                    GRPC,
                    status.code() as i32,
                    status.message().to_string(),
                );
                return;
            }
        }
    }
}

fn emit(call: &OutboundCall, tx: &SyncSender<OutEvent>, event: OutEvent) -> bool {
    match tx.try_send(event) {
        Ok(()) => true,
        Err(TrySendError::Full(_)) => {
            call.mark_finished(
                BUSY,
                13,
                "response consumer is too slow; call failed to keep memory bounded",
            );
            false
        }
        Err(TrySendError::Disconnected(_)) => false,
    }
}

fn finish_task(
    call: &OutboundCall,
    tx: &SyncSender<OutEvent>,
    status: i32,
    code: i32,
    detail: impl Into<String>,
) {
    let detail = detail.into();
    if call.mark_finished(status, code, detail.clone()) {
        let _ = tx.try_send(OutEvent::End(FinalStatus::error(status, code, detail)));
    }
}

/* ------------------------------------------------------------------ *
 * Inbound (app-served) calls
 * ------------------------------------------------------------------ */

pub enum InRequest {
    Message(Vec<u8>),
    End(FinalStatus),
}

type Cleanup = Box<dyn FnOnce() + Send>;

pub struct InboundCall {
    pub handle: AtomicU64,
    pub client: Arc<ClientInner>,
    pub call_id: String,
    pub service: String,
    pub method: String,
    request_desc: Option<MessageDescriptor>,
    response_desc: Option<MessageDescriptor>,
    out: tokio::sync::mpsc::Sender<Vec<u8>>,
    requests: Mutex<Receiver<InRequest>>,
    req_tx: SyncSender<InRequest>,
    ended: AtomicBool,
    final_status: Mutex<Option<FinalStatus>>,
    refs: AtomicUsize,
    cleanup: Mutex<Option<Cleanup>>,
}

impl InboundCall {
    pub fn new(
        client: Arc<ClientInner>,
        call_id: String,
        service: String,
        method: String,
        request_desc: Option<MessageDescriptor>,
        response_desc: Option<MessageDescriptor>,
        out: tokio::sync::mpsc::Sender<Vec<u8>>,
        cleanup: Cleanup,
    ) -> Arc<Self> {
        let (req_tx, req_rx) = std::sync::mpsc::sync_channel(32);
        Arc::new(Self {
            handle: AtomicU64::new(0),
            client,
            call_id,
            service,
            method,
            request_desc,
            response_desc,
            out,
            requests: Mutex::new(req_rx),
            req_tx,
            ended: AtomicBool::new(false),
            final_status: Mutex::new(None),
            refs: AtomicUsize::new(1),
            cleanup: Mutex::new(Some(cleanup)),
        })
    }

    pub fn push_data(&self, payload: Vec<u8>) -> SdkResult<()> {
        self.push(InRequest::Message(payload))
    }

    pub fn push_end(&self, status: FinalStatus) -> SdkResult<()> {
        let result = self.push(InRequest::End(status.clone()));
        if result.is_err() {
            *self.final_status.lock().unwrap() = Some(status);
        }
        result
    }

    fn push(&self, request: InRequest) -> SdkResult<()> {
        match self.req_tx.try_send(request) {
            Ok(()) => Ok(()),
            Err(TrySendError::Full(_)) => Err(SdkError::new(BUSY, "inbound request queue is full")),
            Err(TrySendError::Disconnected(_)) => {
                Err(SdkError::new(CLOSED, "inbound call is closed"))
            }
        }
    }

    pub fn recv(&self, timeout_ms: u32, as_json: bool) -> SdkResult<Vec<u8>> {
        if let Some(status) = self.final_status() {
            if status.status == CANCELLED {
                return Err(status.to_error());
            }
        }
        let timeout = if timeout_ms == u32::MAX {
            None
        } else {
            Some(Duration::from_millis(u64::from(timeout_ms)))
        };
        let receiver = self.requests.lock().unwrap();
        let event = loop {
            if let Some(status) = self.final_status() {
                if !matches!(status.status, OK) || status.code != 0 {
                    return Err(status.to_error());
                }
            }
            let result = match timeout {
                None => receiver.recv().map_err(|_| RecvTimeoutError::Disconnected),
                Some(duration) => receiver.recv_timeout(duration),
            };
            match result {
                Err(RecvTimeoutError::Timeout) => {
                    return Err(SdkError::new(
                        ffi::TIMEOUT,
                        "recv timed out; call is still open",
                    ))
                }
                other => break other,
            }
        };
        match event {
            Ok(InRequest::Message(bytes)) => {
                if as_json {
                    let desc = self.request_desc.as_ref().ok_or_else(|| {
                        SdkError::new(
                            DESCRIPTOR_NOT_FOUND,
                            format!(
                                "no request descriptor for {}/{}; register the app contract or use raw protobuf",
                                self.service, self.method
                            ),
                        )
                    })?;
                    dynamic::proto_to_json(desc, &bytes)
                } else {
                    Ok(bytes)
                }
            }
            Ok(InRequest::End(status)) => Err(status.to_error()),
            Err(RecvTimeoutError::Timeout) => Err(SdkError::new(
                ffi::TIMEOUT,
                "recv timed out; call is still open",
            )),
            Err(RecvTimeoutError::Disconnected) => match self.final_status() {
                Some(status) => Err(status.to_error()),
                None => Err(SdkError::new(
                    ffi::INTERNAL,
                    "inbound call closed without a status",
                )),
            },
        }
    }

    pub fn send_payload(&self, payload: Vec<u8>) -> SdkResult<()> {
        if self.ended.load(Ordering::Acquire) {
            return Err(SdkError::new(CLOSED, "call already ended"));
        }
        let frame = dynamic::app_call_reply_message(&self.call_id, payload)?;
        self.out.try_send(frame).map_err(|err| match err {
            tokio::sync::mpsc::error::TrySendError::Full(_) => {
                SdkError::new(BUSY, "AppBus outbound queue is full")
            }
            tokio::sync::mpsc::error::TrySendError::Closed(_) => {
                SdkError::new(CLOSED, "AppBus session is closed")
            }
        })
    }

    pub fn send_json(&self, json: &[u8]) -> SdkResult<()> {
        let desc = self.response_desc.as_ref().ok_or_else(|| {
            SdkError::new(
                DESCRIPTOR_NOT_FOUND,
                format!(
                    "no response descriptor for {}/{}; register the app contract or send raw protobuf",
                    self.service, self.method
                ),
            )
        })?;
        let payload = dynamic::json_to_proto(desc, json)?;
        self.send_payload(payload)
    }

    pub fn end(&self, grpc_code: i32, detail: &str) -> SdkResult<()> {
        if self.ended.swap(true, Ordering::AcqRel) {
            return Ok(());
        }
        let frame = dynamic::app_call_reply_end(&self.call_id, grpc_code, detail)?;
        let _ = self.out.try_send(frame);
        let status = if grpc_code == 0 { OK } else { GRPC };
        *self.final_status.lock().unwrap() = Some(FinalStatus::error(status, grpc_code, detail));
        Ok(())
    }

    pub fn cancel(&self) -> SdkResult<()> {
        self.end(1, "call cancelled")
    }

    pub fn final_status(&self) -> Option<FinalStatus> {
        self.final_status.lock().unwrap().clone()
    }

    pub fn is_ended(&self) -> bool {
        self.ended.load(Ordering::Acquire)
    }

    pub fn release_ref(self: &Arc<Self>) -> bool {
        let previous = self.refs.fetch_sub(1, Ordering::AcqRel);
        if previous > 1 {
            return false;
        }
        if previous == 0 {
            self.refs.store(0, Ordering::Release);
            return true;
        }
        if !self.is_ended() {
            let _ = self.end(1, "handler released the call without ending it");
        }
        let id = self.handle.load(Ordering::Acquire);
        CALLS.remove(id);
        self.client.remove_call_child(id);
        if let Some(cleanup) = self.cleanup.lock().unwrap().take() {
            cleanup();
        }
        true
    }

    pub fn retain_ref(&self) {
        self.refs.fetch_add(1, Ordering::AcqRel);
    }

    pub fn refs(&self) -> usize {
        self.refs.load(Ordering::Acquire)
    }

    pub fn info_json(&self) -> Vec<u8> {
        let status = self.final_status();
        let value = serde_json::json!({
            "kind": -1,
            "direction": "inbound",
            "service": self.service,
            "method": self.method,
            "target_app_id": "",
            "routed": true,
            "call_id": self.call_id,
            "grpc_code": status.as_ref().map(|s| s.code).unwrap_or(-1),
            "ended": self.is_ended(),
        });
        serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
    }
}

/* ------------------------------------------------------------------ *
 * Type-erased call handle
 * ------------------------------------------------------------------ */

pub enum CallInner {
    Out(Arc<OutboundCall>),
    In(Arc<InboundCall>),
}

impl CallInner {
    pub fn outbound(self: &Arc<Self>) -> Option<Arc<OutboundCall>> {
        match self.as_ref() {
            CallInner::Out(call) => Some(call.clone()),
            CallInner::In(_) => None,
        }
    }

    pub fn inbound(self: &Arc<Self>) -> Option<Arc<InboundCall>> {
        match self.as_ref() {
            CallInner::In(call) => Some(call.clone()),
            CallInner::Out(_) => None,
        }
    }

    pub fn cancel(&self) -> SdkResult<()> {
        match self {
            CallInner::Out(call) => call.cancel(),
            CallInner::In(call) => call.cancel(),
        }
    }

    pub fn release(self: &Arc<Self>) -> SdkResult<()> {
        match self.as_ref() {
            CallInner::Out(call) => {
                call.release_ref();
            }
            CallInner::In(call) => {
                call.release_ref();
            }
        }
        Ok(())
    }

    pub fn info_json(&self) -> Vec<u8> {
        match self {
            CallInner::Out(call) => call.info_json(),
            CallInner::In(call) => call.info_json(),
        }
    }

    pub fn status(&self) -> Option<FinalStatus> {
        match self {
            CallInner::Out(call) => call.final_status(),
            CallInner::In(call) => call.final_status(),
        }
    }
}

pub fn cancel_handle(handle: u64) -> SdkResult<()> {
    let call = CALLS
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown call handle"))?;
    call.cancel()
}

fn register_outbound(call: Arc<OutboundCall>) -> u64 {
    let handle = CALLS.insert(Arc::new(CallInner::Out(call.clone())));
    call.handle.store(handle, Ordering::Release);
    call.client.add_call_child(handle);
    handle
}

/// Start a direct framework/registered-service call. Unknown services or
/// methods are rejected here, before any stream exists.
pub fn start_direct(
    client: Arc<ClientInner>,
    service: &str,
    method: &str,
    kind: i32,
    timeout: Option<Duration>,
) -> SdkResult<Arc<OutboundCall>> {
    client.ensure_open()?;
    let method_desc =
        client.with_pool(
            |pool| match dynamic::resolve_method(pool, service, method) {
                Ok(desc) => Ok(desc),
                Err(err) => Err(err),
            },
        )?;
    if !(0..=3).contains(&kind) {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "call kind must be UNARY, SERVER_STREAM, CLIENT_STREAM or BIDI",
        ));
    }
    let expected = dynamic::expected_kind(&method_desc);
    if kind != expected {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            format!("{service}/{method} has streaming kind {expected}, caller requested {kind}"),
        ));
    }

    let (call, rx, events) = OutboundCall::new(
        client,
        service.to_string(),
        method.to_string(),
        kind,
        None,
        Some(method_desc.input()),
        Some(method_desc.output()),
        timeout,
    );
    register_outbound(call.clone());
    OutboundCall::start_task(&call, rx, events);
    Ok(call)
}

/// Start a routed `AppBusService.Call`. Raw protobuf works without
/// descriptors; JSON requires a registered app contract.
pub fn start_routed(
    client: Arc<ClientInner>,
    target_app_id: &str,
    service: &str,
    method: &str,
    timeout: Option<Duration>,
) -> SdkResult<Arc<OutboundCall>> {
    client.ensure_open()?;
    if target_app_id.is_empty() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "target_app_id is required for a routed call",
        ));
    }
    let descriptors = client.with_pool(|pool| dynamic::resolve_method(pool, service, method).ok());
    let (kind, request_desc, response_desc) = match descriptors {
        Some(desc) => (
            dynamic::expected_kind(&desc),
            Some(desc.input()),
            Some(desc.output()),
        ),
        None => (3, None, None),
    };

    let (call, rx, events) = OutboundCall::new(
        client,
        service.to_string(),
        method.to_string(),
        kind,
        Some(target_app_id.to_string()),
        request_desc,
        response_desc,
        timeout,
    );
    let timeout_ms = timeout
        .map(|duration| duration.as_millis().min(u32::MAX as u128) as u32)
        .unwrap_or(0);
    if let Err(err) = call.prequeue(dynamic::routed_start(
        target_app_id,
        service,
        method,
        timeout_ms,
    )?) {
        return Err(err);
    }
    register_outbound(call.clone());
    OutboundCall::start_task(&call, rx, events);
    Ok(call)
}
