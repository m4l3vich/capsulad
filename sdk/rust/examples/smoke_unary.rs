//! Unary smoke client.
//!
//! ```sh
//! CAPSULA_LIBRARY=/path/libcapsula.1.dylib \
//! CAPSULA_SOCKET=/tmp/capsulad.sock CAPSULA_RUN_ID=run-... \
//! cargo run --example smoke_unary -- capsula.framework.v1.DeviceService GetInfo '{}'
//! ```
//!
//! Arguments: `[service] [method] [request-json]`. Environment fallbacks:
//! `CAPSULA_SERVICE`, `CAPSULA_METHOD`, `CAPSULA_REQUEST`, `CAPSULA_TIMEOUT_MS`.

use std::env;
use std::error::Error;
use std::time::Duration;

use capsula::Library;

fn main() -> Result<(), Box<dyn Error>> {
    let library = Library::load()?;
    let client = library.connect_env()?;
    let args: Vec<String> = env::args().skip(1).collect();
    let service = args
        .first()
        .cloned()
        .or_else(|| env::var("CAPSULA_SERVICE").ok())
        .unwrap_or_else(|| "capsula.framework.v1.DeviceService".to_string());
    let method = args
        .get(1)
        .cloned()
        .or_else(|| env::var("CAPSULA_METHOD").ok())
        .unwrap_or_else(|| "GetInfo".to_string());
    let request = args
        .get(2)
        .cloned()
        .or_else(|| env::var("CAPSULA_REQUEST").ok())
        .unwrap_or_else(|| "{}".to_string());
    let timeout = env::var("CAPSULA_TIMEOUT_MS")
        .ok()
        .and_then(|value| value.parse::<u64>().ok())
        .map(Duration::from_millis);

    eprintln!("unary {service}/{method} via {}", library.path().display());
    let reply = client.unary_json(&service, &method, request.as_bytes(), timeout)?;
    println!("{}", String::from_utf8_lossy(&reply));
    Ok(())
}
