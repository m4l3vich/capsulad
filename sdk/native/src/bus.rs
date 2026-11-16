//! Managed AppBus session: Connect lifecycle, inbound events, bounded call
//! workers, and routed-call replies.

use std::collections::{HashMap, VecDeque};
use std::ffi::{c_char, c_void, CString};
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex, Weak};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use tokio::sync::{OwnedSemaphorePermit, Semaphore};

use crate::call::{CallInner, FinalStatus, InboundCall, CALLS};
use crate::client::ClientInner;
use crate::dynamic::{self, InboundKind};
use crate::ffi::{
    self, SdkError, SdkResult, BUSY, CANCELLED, CLOSED, GRPC, INTERNAL, INVALID_ARGUMENT,
    NOT_FOUND, OK, TRANSPORT,
};
use crate::registry::Registry;

pub static BUSES: Registry<BusInner> = Registry::new();

pub const WORKER_THREADS: usize = 4;
pub const JOB_QUEUE: usize = 32;
pub const LIVE_CALLS: usize = 64;
const BUS_OUT_QUEUE: usize = 256;
const DISPATCH_QUEUE: usize = 256;

pub type ReadyFn = unsafe extern "C" fn(u64, *mut c_void);
pub type DisconnectedFn = unsafe extern "C" fn(u64, i32, *const c_char, *mut c_void);
pub type EventFn = unsafe extern "C" fn(u64, i32, *const u8, usize, *mut c_void);
pub type CallFn = unsafe extern "C" fn(u64, u64, *const u8, usize, *mut c_void);
pub type LogFn = unsafe extern "C" fn(i32, *const c_char, *mut c_void);

#[derive(Clone, Copy, Default)]
pub struct Callbacks {
    pub user_data: usize,
    pub on_ready: Option<ReadyFn>,
    pub on_disconnected: Option<DisconnectedFn>,
    pub on_event: Option<EventFn>,
    pub on_call: Option<CallFn>,
    pub on_log: Option<LogFn>,
}

enum Dispatch {
    Started,
    Inbound(Vec<u8>, OwnedSemaphorePermit),
    Disconnected(FinalStatus),
    Close,
}

#[derive(Clone)]
enum ConnState {
    Connecting,
    Ready,
    Failed(SdkError),
    Closed,
}

struct JobQueue {
    queue: Mutex<VecDeque<(u64, Vec<u8>)>>,
    cv: Condvar,
    closed: AtomicBool,
}

impl JobQueue {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            queue: Mutex::new(VecDeque::new()),
            cv: Condvar::new(),
            closed: AtomicBool::new(false),
        })
    }

    fn push(&self, id: u64, info: Vec<u8>) -> bool {
        let mut queue = self.queue.lock().unwrap();
        if self.closed.load(Ordering::Acquire) || queue.len() >= JOB_QUEUE {
            return false;
        }
        queue.push_back((id, info));
        self.cv.notify_one();
        true
    }

    fn pop(&self) -> Option<(u64, Vec<u8>)> {
        let mut queue = self.queue.lock().unwrap();
        loop {
            if let Some(id) = queue.pop_front() {
                return Some(id);
            }
            if self.closed.load(Ordering::Acquire) {
                return None;
            }
            queue = self.cv.wait(queue).unwrap();
        }
    }

    fn close(&self) {
        self.closed.store(true, Ordering::Release);
        self.cv.notify_all();
    }
}

pub struct BusInner {
    pub handle: AtomicU64,
    pub client: Arc<ClientInner>,
    callbacks: Callbacks,
    out_tx: Mutex<Option<tokio::sync::mpsc::Sender<Vec<u8>>>>,
    dispatch_tx: tokio::sync::mpsc::UnboundedSender<Dispatch>,
    state: Mutex<ConnState>,
    state_cv: Condvar,
    disconnect_fired: AtomicBool,
    closing: AtomicBool,
    task: Mutex<Option<tokio::task::JoinHandle<()>>>,
    dispatch_join: Mutex<Option<JoinHandle<()>>>,
    workers: Mutex<Vec<JoinHandle<()>>>,
    jobs: Arc<JobQueue>,
    calls: Mutex<HashMap<String, Weak<InboundCall>>>,
    live_calls: AtomicUsize,
}

/* ------------------------------------------------------------------ *
 * Session open
 * ------------------------------------------------------------------ */

pub fn open(client: Arc<ClientInner>, callbacks: Callbacks) -> SdkResult<u64> {
    client.ensure_open()?;

    let (out_tx, out_rx) = tokio::sync::mpsc::channel::<Vec<u8>>(BUS_OUT_QUEUE);
    let (dispatch_tx, dispatch_rx) = tokio::sync::mpsc::unbounded_channel::<Dispatch>();
    let dispatch_permits = Arc::new(Semaphore::new(DISPATCH_QUEUE));

    let bus = Arc::new(BusInner {
        handle: AtomicU64::new(0),
        client: client.clone(),
        callbacks,
        out_tx: Mutex::new(Some(out_tx.clone())),
        dispatch_tx: dispatch_tx.clone(),
        state: Mutex::new(ConnState::Connecting),
        state_cv: Condvar::new(),
        disconnect_fired: AtomicBool::new(false),
        closing: AtomicBool::new(false),
        task: Mutex::new(None),
        dispatch_join: Mutex::new(None),
        workers: Mutex::new(Vec::new()),
        jobs: JobQueue::new(),
        calls: Mutex::new(HashMap::new()),
        live_calls: AtomicUsize::new(0),
    });

    let id = BUSES.insert(bus.clone());
    bus.handle.store(id, Ordering::Release);
    client.add_bus_child(id);

    // Hello and READY are queued before the transport task exists, so the
    // stream can never wait for a first message from the caller.
    out_tx
        .try_send(dynamic::app_hello(&client.run_id)?)
        .map_err(|_| SdkError::new(INTERNAL, "cannot queue AppHello"))?;
    out_tx
        .try_send(dynamic::app_ready()?)
        .map_err(|_| SdkError::new(INTERNAL, "cannot queue AppReady"))?;

    let dispatch_bus = bus.clone();
    let dispatch_join = std::thread::Builder::new()
        .name("capsulad-bus-dispatch".into())
        .spawn(move || dispatch_loop(dispatch_bus, dispatch_rx))
        .map_err(|e| SdkError::new(INTERNAL, format!("cannot start bus dispatch: {e}")))?;
    *bus.dispatch_join.lock().unwrap() = Some(dispatch_join);

    let weak = Arc::downgrade(&bus);
    let channel = client.channel.clone();
    let run_id = client.run_id.clone();
    let _ = weak;
    let task = crate::runtime::runtime().spawn(bus_transport(
        channel,
        run_id,
        out_rx,
        dispatch_tx,
        dispatch_permits,
    ));
    *bus.task.lock().unwrap() = Some(task);

    for index in 0..WORKER_THREADS {
        let weak = Arc::downgrade(&bus);
        let jobs = bus.jobs.clone();
        let join = std::thread::Builder::new()
            .name(format!("capsulad-bus-worker-{index}"))
            .spawn(move || worker_loop(weak, jobs))
            .map_err(|e| SdkError::new(INTERNAL, format!("cannot start bus worker: {e}")))?;
        bus.workers.lock().unwrap().push(join);
    }

    Ok(id)
}

/* ------------------------------------------------------------------ *
 * Transport task
 * ------------------------------------------------------------------ */

async fn bus_transport(
    channel: tonic::transport::Channel,
    run_id: String,
    mut out_rx: tokio::sync::mpsc::Receiver<Vec<u8>>,
    dispatch: tokio::sync::mpsc::UnboundedSender<Dispatch>,
    permits: Arc<Semaphore>,
) {
    let request_stream = async_stream::stream! {
        while let Some(bytes) = out_rx.recv().await {
            yield crate::call::RawMessage(bytes);
        }
    };
    let mut request = tonic::Request::new(request_stream);
    if let Ok(value) = tonic::metadata::MetadataValue::try_from(run_id.as_str()) {
        request.metadata_mut().insert("capsulad-run-id", value);
    }
    let path: http::uri::PathAndQuery = match "/capsulad.framework.v1.AppBusService/Connect".parse()
    {
        Ok(path) => path,
        Err(err) => {
            let _ = dispatch.send(Dispatch::Disconnected(FinalStatus::error(
                INTERNAL,
                13,
                err.to_string(),
            )));
            return;
        }
    };

    let mut grpc = tonic::client::Grpc::new(channel);
    if let Err(status) = grpc.ready().await {
        let _ = dispatch.send(Dispatch::Disconnected(FinalStatus::error(
            TRANSPORT,
            -1,
            status.to_string(),
        )));
        return;
    }

    let response = match grpc.streaming(request, path, crate::call::RawCodec).await {
        Ok(response) => response,
        Err(status) => {
            let _ = dispatch.send(Dispatch::Disconnected(FinalStatus::error(
                GRPC,
                status.code() as i32,
                status.message().to_string(),
            )));
            return;
        }
    };

    if dispatch.send(Dispatch::Started).is_err() {
        return;
    }
    let mut stream = response.into_inner();
    loop {
        match stream.message().await {
            Ok(Some(raw)) => {
                let Ok(permit) = Arc::clone(&permits).acquire_owned().await else {
                    return;
                };
                if dispatch.send(Dispatch::Inbound(raw.0, permit)).is_err() {
                    return;
                }
            }
            Ok(None) => {
                let _ = dispatch.send(Dispatch::Disconnected(FinalStatus::error(
                    OK,
                    0,
                    "bus stream closed by daemon",
                )));
                return;
            }
            Err(status) => {
                let _ = dispatch.send(Dispatch::Disconnected(FinalStatus::error(
                    GRPC,
                    status.code() as i32,
                    status.message().to_string(),
                )));
                return;
            }
        }
    }
}

/* ------------------------------------------------------------------ *
 * Dispatch
 * ------------------------------------------------------------------ */

fn dispatch_loop(bus: Arc<BusInner>, mut rx: tokio::sync::mpsc::UnboundedReceiver<Dispatch>) {
    crate::threads::enter();
    while let Some(event) = rx.blocking_recv() {
        match event {
            Dispatch::Started => bus.handle_started(),
            Dispatch::Inbound(bytes, permit) => {
                bus.handle_inbound(&bytes);
                drop(permit);
            }
            Dispatch::Disconnected(status) => {
                bus.finish_disconnect(status);
                break;
            }
            Dispatch::Close => {
                bus.finish_disconnect(FinalStatus::error(OK, 0, "bus closed by client"));
                break;
            }
        }
    }
    if !bus.disconnect_fired.load(Ordering::Acquire) {
        bus.fire_disconnect(&FinalStatus::error(OK, 0, "bus session closed"));
    }
    bus.set_state(ConnState::Closed);
    crate::threads::leave();
}

impl BusInner {
    fn set_state(&self, state: ConnState) {
        *self.state.lock().unwrap() = state;
        self.state_cv.notify_all();
    }

    fn handle_started(&self) {
        if self.closing.load(Ordering::Acquire) {
            return;
        }
        self.fire_ready();
        // Readiness becomes visible only after on_ready has returned.
        self.set_state(ConnState::Ready);
    }

    fn handle_inbound(self: &Arc<Self>, bytes: &[u8]) {
        if self.closing.load(Ordering::Acquire) {
            return;
        }
        match dynamic::decode_inbound(bytes) {
            Ok(InboundKind::Call {
                call_id,
                caller_run_id,
                service,
                rpc,
                deadline_monotonic_ns,
            }) => self.begin_call(call_id, caller_run_id, service, rpc, deadline_monotonic_ns),
            Ok(InboundKind::CallData { call_id, payload }) => {
                if let Some(call) = self.lookup_call(&call_id) {
                    if let Err(err) = call.push_data(payload) {
                        let _ = call.end(8, &err.message);
                    }
                } else {
                    self.log(1, &format!("call data for unknown call {call_id}"));
                }
            }
            Ok(InboundKind::CallHalfClose { call_id }) => {
                if let Some(call) = self.lookup_call(&call_id) {
                    let _ = call.push_end(FinalStatus::ok());
                }
            }
            Ok(InboundKind::CallCancel { call_id }) => {
                if let Some(call) = self.lookup_call(&call_id) {
                    let _ = call.push_end(FinalStatus::error(
                        CANCELLED,
                        1,
                        "caller cancelled the call",
                    ));
                }
            }
            Ok(InboundKind::Event { kind, json }) => self.emit_event(kind, &json),
            Ok(InboundKind::Unknown(detail)) => self.log(1, &detail),
            Err(err) => self.log(2, &err.message),
        }
    }

    fn lookup_call(&self, call_id: &str) -> Option<Arc<InboundCall>> {
        self.calls
            .lock()
            .unwrap()
            .get(call_id)
            .and_then(|weak| weak.upgrade())
    }

    fn begin_call(
        self: &Arc<Self>,
        call_id: String,
        caller_run_id: String,
        service: String,
        rpc: String,
        deadline_monotonic_ns: u64,
    ) {
        if self.live_calls.load(Ordering::Acquire) >= LIVE_CALLS {
            self.reject_call(&call_id, 8, "too many live inbound calls on this bus");
            return;
        }

        let (request_desc, response_desc) =
            self.client
                .with_pool(|pool| match dynamic::resolve_method(pool, &service, &rpc) {
                    Ok(method) => (Some(method.input()), Some(method.output())),
                    Err(_) => (None, None),
                });

        let Some(out) = self.out_tx.lock().unwrap().clone() else {
            return;
        };

        let weak = Arc::downgrade(self);
        let cleanup_call_id = call_id.clone();
        let cleanup: Box<dyn FnOnce() + Send> = Box::new(move || {
            if let Some(bus) = weak.upgrade() {
                bus.remove_call(&cleanup_call_id);
            }
        });

        let call = InboundCall::new(
            self.client.clone(),
            call_id.clone(),
            service,
            rpc,
            request_desc,
            response_desc,
            out,
            cleanup,
        );
        let id = CALLS.insert(Arc::new(CallInner::In(call.clone())));
        call.handle.store(id, Ordering::Release);
        self.client.add_call_child(id);
        self.calls
            .lock()
            .unwrap()
            .insert(call_id, Arc::downgrade(&call));
        self.live_calls.fetch_add(1, Ordering::AcqRel);

        let info = serde_json::json!({
            "call_id": call.call_id,
            "caller_run_id": caller_run_id,
            "grpc_service": call.service,
            "rpc_name": call.method,
            "deadline_monotonic_ns": deadline_monotonic_ns,
        });
        let info = serde_json::to_vec(&info).unwrap_or_default();
        if !self.jobs.push(id, info) {
            let _ = call.end(8, "inbound call queue is full");
            call.release_ref();
        }
    }

    fn reject_call(&self, call_id: &str, code: i32, detail: &str) {
        if let Ok(frame) = dynamic::app_call_reply_end(call_id, code, detail) {
            if let Some(out) = self.out_tx.lock().unwrap().as_ref() {
                let _ = out.try_send(frame);
            }
        }
    }

    fn remove_call(&self, call_id: &str) {
        self.calls.lock().unwrap().remove(call_id);
        let previous = self.live_calls.load(Ordering::Acquire);
        if previous > 0 {
            self.live_calls.fetch_sub(1, Ordering::AcqRel);
        }
    }

    fn fire_ready(&self) {
        if let Some(callback) = self.callbacks.on_ready {
            let handle = self.handle.load(Ordering::Acquire);
            let user = self.callbacks.user_data as *mut c_void;
            let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
                callback(handle, user);
            }));
        }
    }

    fn fire_disconnect(&self, status: &FinalStatus) {
        if self.disconnect_fired.swap(true, Ordering::AcqRel) {
            return;
        }
        if let Some(callback) = self.callbacks.on_disconnected {
            let handle = self.handle.load(Ordering::Acquire);
            let user = self.callbacks.user_data as *mut c_void;
            let detail = CString::new(status.detail.replace('\0', "")).unwrap_or_default();
            let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
                callback(handle, status.status, detail.as_ptr(), user);
            }));
        }
    }

    fn finish_disconnect(&self, status: FinalStatus) {
        if status.status == OK {
            self.set_state(ConnState::Closed);
        } else {
            self.set_state(ConnState::Failed(SdkError {
                status: status.status,
                message: status.detail.clone(),
                grpc_code: Some(status.code),
            }));
        }
        self.fire_disconnect(&status);
    }

    fn emit_event(&self, kind: i32, json: &[u8]) {
        let Some(callback) = self.callbacks.on_event else {
            return;
        };
        let handle = self.handle.load(Ordering::Acquire);
        let user = self.callbacks.user_data as *mut c_void;
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
            callback(handle, kind, json.as_ptr(), json.len(), user);
        }));
    }

    fn emit_call(&self, call: u64, info: &[u8]) {
        let Some(callback) = self.callbacks.on_call else {
            return;
        };
        let handle = self.handle.load(Ordering::Acquire);
        let user = self.callbacks.user_data as *mut c_void;
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
            callback(handle, call, info.as_ptr(), info.len(), user);
        }));
    }

    fn log(&self, level: i32, message: &str) {
        if let Some(callback) = self.callbacks.on_log {
            let user = self.callbacks.user_data as *mut c_void;
            let message = CString::new(message.replace('\0', "")).unwrap_or_default();
            let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| unsafe {
                callback(level, message.as_ptr(), user);
            }));
        }
    }

    pub fn notify_invocation(
        &self,
        invocation_id: &str,
        accepted: bool,
        detail: &str,
    ) -> SdkResult<()> {
        self.send_control(dynamic::app_invocation_result(
            invocation_id,
            accepted,
            detail,
        )?)
    }

    pub fn notify_action(
        &self,
        invocation_id: &str,
        succeeded: bool,
        detail: &str,
    ) -> SdkResult<()> {
        self.send_control(dynamic::app_action_result(
            invocation_id,
            succeeded,
            detail,
        )?)
    }

    fn send_control(&self, frame: Vec<u8>) -> SdkResult<()> {
        let guard = self.out_tx.lock().unwrap();
        let out = guard
            .as_ref()
            .ok_or_else(|| SdkError::new(CLOSED, "AppBus session is closed"))?;
        out.try_send(frame).map_err(|err| match err {
            tokio::sync::mpsc::error::TrySendError::Full(_) => {
                SdkError::new(BUSY, "AppBus outbound queue is full")
            }
            tokio::sync::mpsc::error::TrySendError::Closed(_) => {
                SdkError::new(CLOSED, "AppBus session is closed")
            }
        })
    }

    pub fn wait_ready(&self, timeout_ms: u32) -> SdkResult<()> {
        let deadline = if timeout_ms == 0 || timeout_ms == u32::MAX {
            None
        } else {
            Some(Instant::now() + Duration::from_millis(u64::from(timeout_ms)))
        };
        let mut state = self.state.lock().unwrap();
        loop {
            match &*state {
                ConnState::Ready => return Ok(()),
                ConnState::Failed(err) => return Err(err.clone()),
                ConnState::Closed => return Err(SdkError::new(CLOSED, "bus session is closed")),
                ConnState::Connecting => {}
            }
            state = match deadline {
                None => self.state_cv.wait(state).unwrap(),
                Some(deadline) => {
                    let now = Instant::now();
                    if now >= deadline {
                        return Err(SdkError::new(
                            ffi::TIMEOUT,
                            "bus did not become ready before the timeout",
                        ));
                    }
                    self.state_cv.wait_timeout(state, deadline - now).unwrap().0
                }
            };
        }
    }

    pub fn info_json(&self) -> Vec<u8> {
        let state = match &*self.state.lock().unwrap() {
            ConnState::Connecting => "connecting",
            ConnState::Ready => "ready",
            ConnState::Failed(_) => "failed",
            ConnState::Closed => "closed",
        };
        let value = serde_json::json!({
            "kind": "bus",
            "run_id": self.client.run_id,
            "app_id": self.client.app_id,
            "state": state,
            "live_calls": self.live_calls.load(Ordering::Acquire),
        });
        serde_json::to_vec(&value).unwrap_or_else(|_| b"{}".to_vec())
    }

    pub fn close(self: &Arc<Self>) -> SdkResult<()> {
        if self.closing.swap(true, Ordering::AcqRel) {
            return Ok(());
        }

        // Wake every handler before joining its worker.
        let pending: Vec<Arc<InboundCall>> = self
            .calls
            .lock()
            .unwrap()
            .values()
            .filter_map(|weak| weak.upgrade())
            .collect();
        for call in pending {
            let _ = call.push_end(FinalStatus::error(CANCELLED, 1, "bus closed by client"));
        }

        if let Some(task) = self.task.lock().unwrap().take() {
            task.abort();
        }
        self.out_tx.lock().unwrap().take();
        self.jobs.close();

        // Deliver the intentional-close callback before returning.
        let _ = self.dispatch_tx.send(Dispatch::Close);
        if let Some(join) = self.dispatch_join.lock().unwrap().take() {
            let _ = join.join();
        }
        for join in self.workers.lock().unwrap().drain(..) {
            let _ = join.join();
        }

        let id = self.handle.load(Ordering::Acquire);
        BUSES.remove(id);
        self.client.remove_bus_child(id);
        Ok(())
    }
}

/* ------------------------------------------------------------------ *
 * Bounded workers
 * ------------------------------------------------------------------ */

fn worker_loop(bus: Weak<BusInner>, jobs: Arc<JobQueue>) {
    crate::threads::enter();
    while let Some((handle, info)) = jobs.pop() {
        let Some(bus) = bus.upgrade() else {
            break;
        };
        let Some(call) = CALLS.get(handle).and_then(|inner| inner.inbound()) else {
            continue;
        };
        let has_handler = bus.callbacks.on_call.is_some();
        bus.emit_call(handle, &info);

        // A callback that retained the call may finish asynchronously.
        if call.refs() == 1 && !call.is_ended() {
            if has_handler {
                let _ = call.end(13, "handler returned without ending the call");
            } else {
                let _ = call.end(0, "");
            }
        }
        call.release_ref();
    }
    crate::threads::leave();
}

/* ------------------------------------------------------------------ *
 * Public operations
 * ------------------------------------------------------------------ */

pub fn open_for_handle(client: u64, callbacks: Callbacks) -> SdkResult<u64> {
    if crate::threads::is_callback_thread() {
        return Err(SdkError::new(
            BUSY,
            "bus_open cannot be called from an SDK callback thread",
        ));
    }
    let client = crate::client::CLIENTS
        .get(client)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown client handle"))?;
    if client.is_remote() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "the app bus is only available on a supervised client, not a remote gRPC channel",
        ));
    }
    open(client, callbacks)
}

pub fn close_handle(handle: u64) -> SdkResult<()> {
    if handle == 0 {
        return Err(SdkError::new(INVALID_ARGUMENT, "invalid bus handle"));
    }
    if crate::threads::is_callback_thread() {
        return Err(SdkError::new(
            BUSY,
            "bus_close cannot be called from an SDK callback thread",
        ));
    }
    let bus = BUSES
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown bus handle"))?;
    bus.close()
}

pub fn ready_handle(handle: u64, timeout_ms: u32) -> SdkResult<()> {
    if crate::threads::is_callback_thread() {
        return Err(SdkError::new(
            BUSY,
            "bus_ready cannot be called from an SDK callback thread",
        ));
    }
    let bus = BUSES
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown bus handle"))?;
    bus.wait_ready(timeout_ms)
}

pub fn notify_invocation(
    handle: u64,
    invocation_id: &str,
    accepted: bool,
    detail: &str,
) -> SdkResult<()> {
    let bus = BUSES
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown bus handle"))?;
    bus.notify_invocation(invocation_id, accepted, detail)
}

pub fn notify_action(
    handle: u64,
    invocation_id: &str,
    succeeded: bool,
    detail: &str,
) -> SdkResult<()> {
    let bus = BUSES
        .get(handle)
        .ok_or_else(|| SdkError::new(NOT_FOUND, "unknown bus handle"))?;
    bus.notify_action(invocation_id, succeeded, detail)
}

pub fn bus_info(handle: u64) -> Option<Vec<u8>> {
    BUSES.get(handle).map(|bus| bus.info_json())
}

pub fn cancel_all_for_client(client: &Arc<ClientInner>) -> (Vec<u64>, Vec<u64>) {
    client.take_children()
}
