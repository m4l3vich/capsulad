//! AppBus smoke client. Connects as the supervised app, prints lifecycle
//! events, and echoes inbound routed calls (raw protobuf) back to the caller.
//!
//! ```sh
//! CAPSULAD_LIBRARY=/path/libcapsulad.1.dylib \
//! CAPSULAD_SOCKET=/tmp/capsulad.sock CAPSULAD_RUN_ID=run-... \
//! cargo run --example smoke_bus
//! ```
//!
//! Runs for up to `CAPSULAD_TIMEOUT_MS` (default 30 s) or until the bus closes.

use std::env;
use std::error::Error;
use std::sync::{mpsc, Arc};
use std::time::Duration;

use capsulad::{BusEvent, BusHandler, CallInfo, InboundCall, Library};

struct Handler {
    closed: mpsc::Sender<()>,
}

impl BusHandler for Handler {
    fn on_ready(&self) {
        eprintln!("bus: ready");
    }

    fn on_disconnected(&self, status: i32, detail: &str) {
        eprintln!("bus: disconnected status={status} detail={detail}");
        let _ = self.closed.send(());
    }

    fn on_event(&self, event: BusEvent) {
        eprintln!(
            "bus: event kind={:?} json={}",
            event.kind,
            String::from_utf8_lossy(&event.json)
        );
    }

    fn on_call(&self, call: InboundCall<'_>, info: CallInfo) {
        eprintln!(
            "bus: call {} from {} {}/{}",
            info.call_id, info.caller_run_id, info.service, info.method
        );
        let mut request = Vec::new();
        loop {
            match call.recv_proto(Some(Duration::from_secs(5))) {
                Ok(Some(chunk)) => request.extend_from_slice(&chunk),
                Ok(None) => break,
                Err(err) => {
                    eprintln!("bus: call {} recv: {err}", info.call_id);
                    let _ = call.end(13, &err.to_string());
                    return;
                }
            }
        }
        if let Err(err) = call.send_proto(&request) {
            eprintln!("bus: call {} send: {err}", info.call_id);
            let _ = call.end(13, &err.to_string());
            return;
        }
        if let Err(err) = call.end(0, "") {
            eprintln!("bus: call {} end: {err}", info.call_id);
        }
    }

    fn on_log(&self, level: i32, message: &str) {
        eprintln!("bus: log level={level} {message}");
    }
}

fn main() -> Result<(), Box<dyn Error>> {
    let library = Library::load()?;
    let client = library.connect_env()?;
    let (closed_tx, closed_rx) = mpsc::channel();
    let handler = Arc::new(Handler { closed: closed_tx });
    let mut bus = client.bus(handler)?;
    bus.ready(Some(Duration::from_secs(5)))?;
    eprintln!("bus: open on {}", library.path().display());

    let wait = env::var("CAPSULAD_TIMEOUT_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_millis)
        .unwrap_or(Duration::from_secs(30));
    let _ = closed_rx.recv_timeout(wait);
    let _ = bus.close();
    Ok(())
}
