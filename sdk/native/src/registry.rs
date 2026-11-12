//! Integer-handle registries. Locks are always dropped before any I/O or
//! blocking wait; callers receive an `Arc` snapshot instead.

use std::collections::BTreeMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

pub struct Registry<T> {
    next: AtomicU64,
    items: Mutex<BTreeMap<u64, Arc<T>>>,
}

impl<T> Registry<T> {
    pub const fn new() -> Self {
        Self {
            next: AtomicU64::new(1),
            items: Mutex::new(BTreeMap::new()),
        }
    }

    pub fn insert(&self, item: Arc<T>) -> u64 {
        let id = self.next.fetch_add(1, Ordering::Relaxed);
        self.items.lock().unwrap().insert(id, item);
        id
    }

    pub fn get(&self, id: u64) -> Option<Arc<T>> {
        if id == 0 {
            return None;
        }
        self.items.lock().unwrap().get(&id).cloned()
    }

    pub fn remove(&self, id: u64) -> Option<Arc<T>> {
        if id == 0 {
            return None;
        }
        self.items.lock().unwrap().remove(&id)
    }

    pub fn contains(&self, id: u64) -> bool {
        id != 0 && self.items.lock().unwrap().contains_key(&id)
    }

    pub fn ids(&self) -> Vec<u64> {
        self.items.lock().unwrap().keys().copied().collect()
    }
}

impl<T> Default for Registry<T> {
    fn default() -> Self {
        Self::new()
    }
}
