//! UDS gRPC client state.

use std::collections::HashSet;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, RwLock};

use prost_reflect::DescriptorPool;
use tonic::transport::Channel;

use crate::dynamic::BASE_POOL;
use crate::ffi::{SdkError, SdkResult, CLOSED, INVALID_ARGUMENT};
use crate::registry::Registry;

pub static CLIENTS: Registry<ClientInner> = Registry::new();

pub struct ClientInner {
    pub socket: String,
    pub run_id: String,
    pub app_id: String,
    pub channel: Channel,
    pool: RwLock<DescriptorPool>,
    pub closed: AtomicBool,
    pub call_children: Mutex<HashSet<u64>>,
    pub bus_children: Mutex<HashSet<u64>>,
}

impl ClientInner {
    pub fn new(socket: String, run_id: String, app_id: String, channel: Channel) -> Arc<Self> {
        Arc::new(Self {
            socket,
            run_id,
            app_id,
            channel,
            pool: RwLock::new(BASE_POOL.clone()),
            closed: AtomicBool::new(false),
            call_children: Mutex::new(HashSet::new()),
            bus_children: Mutex::new(HashSet::new()),
        })
    }

    pub fn ensure_open(&self) -> SdkResult<()> {
        if self.closed.load(Ordering::Acquire) {
            return Err(SdkError::new(CLOSED, "client is closed"));
        }
        Ok(())
    }

    pub fn register_descriptors(&self, bytes: &[u8]) -> SdkResult<()> {
        self.ensure_open()?;
        if bytes.is_empty() {
            return Err(SdkError::new(INVALID_ARGUMENT, "empty FileDescriptorSet"));
        }
        let mut pool = self.pool.write().unwrap();
        crate::dynamic::register_descriptors(&mut pool, bytes)
    }

    pub fn with_pool<T>(&self, f: impl FnOnce(&DescriptorPool) -> T) -> T {
        let pool = self.pool.read().unwrap();
        f(&pool)
    }

    pub fn add_call_child(&self, id: u64) {
        self.call_children.lock().unwrap().insert(id);
    }

    pub fn remove_call_child(&self, id: u64) {
        self.call_children.lock().unwrap().remove(&id);
    }

    pub fn add_bus_child(&self, id: u64) {
        self.bus_children.lock().unwrap().insert(id);
    }

    pub fn remove_bus_child(&self, id: u64) {
        self.bus_children.lock().unwrap().remove(&id);
    }

    pub fn take_children(&self) -> (Vec<u64>, Vec<u64>) {
        let calls = self.call_children.lock().unwrap().drain().collect();
        let buses = self.bus_children.lock().unwrap().drain().collect();
        (calls, buses)
    }
}

pub fn connect(
    socket: &str,
    run_id: &str,
    app_id: &str,
    timeout: std::time::Duration,
) -> SdkResult<Arc<ClientInner>> {
    if run_id.is_empty() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "run_id is required for supervised app authentication",
        ));
    }
    let socket_path = if socket.is_empty() {
        "/run/capsulad/hal.sock".to_string()
    } else {
        socket.to_string()
    };
    let channel =
        crate::runtime::runtime().block_on(crate::runtime::connect_uds(&socket_path, timeout))?;
    Ok(ClientInner::new(
        socket_path,
        run_id.to_string(),
        app_id.to_string(),
        channel,
    ))
}
