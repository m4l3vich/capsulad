//! Routed-call smoke client. Relays one raw protobuf message to another
//! supervised app over `AppBusService/Call` and prints the reply.
//!
//! ```sh
//! CAPSULA_LIBRARY=/path/libcapsula.1.dylib \
//! CAPSULA_SOCKET=/tmp/capsulad.sock CAPSULA_RUN_ID=run-... \
//! cargo run --example smoke_routed -- org.example.ring-controller \
//!   org.example.ring.v1.RingControllerService GetState ''
//! ```
//!
//! Arguments: `[target-app-id] [service] [method] [request-bytes]`.

use std::env;
use std::error::Error;
use std::time::Duration;

use capsula::Library;

fn main() -> Result<(), Box<dyn Error>> {
    let library = Library::load()?;
    let client = library.connect_env()?;
    let args: Vec<String> = env::args().skip(1).collect();
    let target = args
        .first()
        .cloned()
        .or_else(|| env::var("CAPSULA_TARGET_APP").ok())
        .unwrap_or_else(|| "org.example.ring-controller".to_string());
    let service = args
        .get(1)
        .cloned()
        .or_else(|| env::var("CAPSULA_SERVICE").ok())
        .unwrap_or_else(|| "org.example.ring.v1.RingControllerService".to_string());
    let method = args
        .get(2)
        .cloned()
        .or_else(|| env::var("CAPSULA_METHOD").ok())
        .unwrap_or_else(|| "GetState".to_string());
    let request = args
        .get(3)
        .cloned()
        .or_else(|| env::var("CAPSULA_REQUEST").ok())
        .unwrap_or_default();
    let timeout = env::var("CAPSULA_TIMEOUT_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_millis)
        .or(Some(Duration::from_secs(10)));

    eprintln!("routed {target} {service}/{method}");
    let call = client.start_routed(&target, &service, &method, None)?;
    call.send_proto(request.as_bytes())?;
    call.half_close()?;
    loop {
        match call.recv_proto(timeout)? {
            Some(message) => println!("{}", String::from_utf8_lossy(&message)),
            None => break,
        }
    }
    Ok(())
}
