//! Tracks library callback threads so close operations can refuse to join
//! themselves and return a defined BUSY error.

use std::collections::HashSet;
use std::sync::{Mutex, OnceLock};
use std::thread::ThreadId;

fn threads() -> &'static Mutex<HashSet<ThreadId>> {
    static THREADS: OnceLock<Mutex<HashSet<ThreadId>>> = OnceLock::new();
    THREADS.get_or_init(|| Mutex::new(HashSet::new()))
}

pub fn enter() {
    threads()
        .lock()
        .unwrap()
        .insert(std::thread::current().id());
}

pub fn leave() {
    threads()
        .lock()
        .unwrap()
        .remove(&std::thread::current().id());
}

pub fn is_callback_thread() -> bool {
    threads()
        .lock()
        .unwrap()
        .contains(&std::thread::current().id())
}
