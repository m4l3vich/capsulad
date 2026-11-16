//! Server-streaming smoke client.
//!
//! ```sh
//! CAPSULAD_LIBRARY=/path/libcapsulad.1.dylib \
//! CAPSULAD_SOCKET=/tmp/capsulad.sock CAPSULAD_RUN_ID=run-... \
//! cargo run --example smoke_stream -- \
//!   capsulad.framework.v1.ApplicationService WatchRuns '{}'
//! ```
//!
//! Arguments: `[service] [method] [request-json]`. Each response is printed on
//! its own line until the daemon ends the stream.

use std::env;
use std::error::Error;
use std::time::Duration;

use capsulad::{CallKind, Library};

fn main() -> Result<(), Box<dyn Error>> {
    let library = Library::load()?;
    let client = library.connect_env()?;
    let args: Vec<String> = env::args().skip(1).collect();
    let service = args
        .first()
        .cloned()
        .or_else(|| env::var("CAPSULAD_SERVICE").ok())
        .unwrap_or_else(|| "capsulad.framework.v1.ApplicationService".to_string());
    let method = args
        .get(1)
        .cloned()
        .or_else(|| env::var("CAPSULAD_METHOD").ok())
        .unwrap_or_else(|| "WatchRuns".to_string());
    let request = args
        .get(2)
        .cloned()
        .or_else(|| env::var("CAPSULAD_REQUEST").ok())
        .unwrap_or_else(|| "{}".to_string());
    let recv_timeout = env::var("CAPSULAD_TIMEOUT_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_millis)
        .or(Some(Duration::from_secs(30)));

    eprintln!(
        "server stream {service}/{method} via {}",
        library.path().display()
    );
    let call = client.start_call(&service, &method, CallKind::ServerStream, None)?;
    call.send_json(request.as_bytes())?;
    call.half_close()?;
    loop {
        match call.recv_json(recv_timeout)? {
            Some(message) => println!("{}", String::from_utf8_lossy(&message)),
            None => break,
        }
    }
    Ok(())
}
