//! Unary smoke client.
//!
//! ```sh
//! CAPSULAD_LIBRARY=/path/libcapsulad.1.dylib \
//! CAPSULAD_SOCKET=/tmp/capsulad.sock CAPSULAD_RUN_ID=run-... \
//! cargo run --example smoke_unary -- capsulad.framework.v1.DeviceService GetInfo '{}'
//! ```
//!
//! Arguments: `[service] [method] [request-json]`. Environment fallbacks:
//! `CAPSULAD_SERVICE`, `CAPSULAD_METHOD`, `CAPSULAD_REQUEST`, `CAPSULAD_TIMEOUT_MS`.

use std::env;
use std::error::Error;
use std::time::Duration;

use capsulad::Library;

fn main() -> Result<(), Box<dyn Error>> {
    let library = Library::load()?;
    let client = library.connect_env()?;
    let args: Vec<String> = env::args().skip(1).collect();
    let service = args
        .first()
        .cloned()
        .or_else(|| env::var("CAPSULAD_SERVICE").ok())
        .unwrap_or_else(|| "capsulad.framework.v1.DeviceService".to_string());
    let method = args
        .get(1)
        .cloned()
        .or_else(|| env::var("CAPSULAD_METHOD").ok())
        .unwrap_or_else(|| "GetInfo".to_string());
    let request = args
        .get(2)
        .cloned()
        .or_else(|| env::var("CAPSULAD_REQUEST").ok())
        .unwrap_or_else(|| "{}".to_string());
    let timeout = env::var("CAPSULAD_TIMEOUT_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_millis);

    eprintln!("unary {service}/{method} via {}", library.path().display());
    let reply = client.unary_json(&service, &method, request.as_bytes(), timeout)?;
    println!("{}", String::from_utf8_lossy(&reply));
    Ok(())
}
