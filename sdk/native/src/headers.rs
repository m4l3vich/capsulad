//! Caller-supplied header maps: strict JSON validation for HTTP and gRPC.
//!
//! Both surfaces take a JSON object whose values are strings. HTTP header
//! names are lowercased and hop-by-hop names are rejected; gRPC metadata names
//! are lowercased and restricted to the `grpc-metadata` token alphabet, with
//! printable-ASCII values. Binary (`-bin`) metadata is not part of this API.

use std::collections::BTreeMap;

use crate::ffi::{SdkError, SdkResult, INVALID_ARGUMENT};

pub const MAX_HEADER_VALUE: usize = 8 * 1024;
/// One request/call may carry at most this many caller header fields.
pub const MAX_HEADERS: usize = 128;

/// Parses a JSON string map. Empty input is an empty map. Unknown value types,
/// non-object roots and overlong values are rejected.
pub fn parse_string_map(bytes: &[u8], what: &str) -> SdkResult<Vec<(String, String)>> {
    if bytes.is_empty() {
        return Ok(Vec::new());
    }
    let value: serde_json::Value = serde_json::from_slice(bytes).map_err(|err| {
        SdkError::new(INVALID_ARGUMENT, format!("{what} JSON is not valid: {err}"))
    })?;
    let object = value.as_object().ok_or_else(|| {
        SdkError::new(
            INVALID_ARGUMENT,
            format!("{what} must be a JSON object of string values"),
        )
    })?;
    if object.len() > MAX_HEADERS {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            format!("{what} has more than {MAX_HEADERS} fields"),
        ));
    }
    let mut out = Vec::with_capacity(object.len());
    for (key, value) in object {
        let Some(text) = value.as_str() else {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("{what} value for {key:?} is not a string"),
            ));
        };
        if text.len() > MAX_HEADER_VALUE {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("{what} value for {key:?} exceeds {MAX_HEADER_VALUE} bytes"),
            ));
        }
        out.push((key.clone(), text.to_string()));
    }
    Ok(out)
}

/// HTTP request headers: names are lowercased, hop-by-hop/forbidden names and
/// control characters are rejected. Duplicate case variants collapse with the
/// last value winning.
pub fn validate_http(pairs: Vec<(String, String)>) -> SdkResult<Vec<(String, String)>> {
    let mut out = BTreeMap::new();
    for (name, value) in pairs {
        let lower = name.to_ascii_lowercase();
        if lower.is_empty() {
            return Err(SdkError::new(INVALID_ARGUMENT, "header name is empty"));
        }
        if matches!(
            lower.as_str(),
            "host"
                | "content-length"
                | "transfer-encoding"
                | "connection"
                | "keep-alive"
                | "proxy-connection"
                | "te"
                | "trailer"
                | "upgrade"
        ) {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("header {lower:?} is managed by the HTTP client and cannot be set"),
            ));
        }
        if !lower
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
        {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("header name {name:?} contains characters outside the HTTP token set"),
            ));
        }
        validate_http_value(&lower, &value)?;
        out.insert(lower, value);
    }
    Ok(out.into_iter().collect())
}

fn validate_http_value(name: &str, value: &str) -> SdkResult<()> {
    if value
        .bytes()
        .any(|b| b == b'\r' || b == b'\n' || b == 0 || (b < 0x20 && b != b'\t') || b == 0x7f)
    {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            format!("header {name:?} contains a control character or line break"),
        ));
    }
    Ok(())
}

/// HTTP/2 and gRPC protocol fields the transport owns. Accepting them as
/// caller metadata would let a caller forge framing (content type, encoding),
/// deadlines or connection identity, so they are rejected outright.
const RESERVED_GRPC_METADATA: &[&str] = &[
    "content-type",
    "te",
    "trailer",
    "grpc-timeout",
    "grpc-encoding",
    "grpc-accept-encoding",
    "user-agent",
    "host",
    // Set by the SDK from the client's run id.
    "capsulad-run-id",
];

/// gRPC metadata: lowercase keys from `[a-z0-9-_.]`, printable-ASCII values
/// without control characters. Binary metadata (`-bin` suffix) and protocol
/// fields reserved by HTTP/2 or gRPC are rejected.
pub fn validate_grpc(pairs: Vec<(String, String)>) -> SdkResult<Vec<(String, String)>> {
    let mut out = BTreeMap::new();
    for (name, value) in pairs {
        let lower = name.to_ascii_lowercase();
        if lower.is_empty() {
            return Err(SdkError::new(INVALID_ARGUMENT, "metadata name is empty"));
        }
        if lower.ends_with("-bin") {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("binary metadata {lower:?} is not supported"),
            ));
        }
        if !lower.bytes().all(|b| {
            b.is_ascii_lowercase() || b.is_ascii_digit() || matches!(b, b'-' | b'_' | b'.')
        }) {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("metadata name {name:?} is not a valid gRPC metadata key"),
            ));
        }
        if RESERVED_GRPC_METADATA.contains(&lower.as_str()) {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("metadata {lower:?} is reserved by HTTP/2 or gRPC"),
            ));
        }
        if value.bytes().any(|b| b < 0x20 || b > 0x7e) {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                format!("metadata value for {lower:?} is not printable ASCII"),
            ));
        }
        out.insert(lower, value);
    }
    Ok(out.into_iter().collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn http_rejects_hop_by_hop_and_control_bytes() {
        for bad in [
            "{\"Host\":\"x\"}",
            "{\"content-length\":\"3\"}",
            "{\"transfer-encoding\":\"chunked\"}",
            "{\"connection\":\"keep-alive\"}",
            "{\"x-a\":\"a\\r\\nb\"}",
        ] {
            let pairs = parse_string_map(bad.as_bytes(), "headers").expect("valid JSON");
            assert!(validate_http(pairs).is_err(), "{bad} must be rejected");
        }
        let pairs = parse_string_map(br#"{"X-Test":"a b"}"#, "headers").unwrap();
        let headers = validate_http(pairs).unwrap();
        assert_eq!(headers, vec![("x-test".to_string(), "a b".to_string())]);
    }

    #[test]
    fn grpc_metadata_is_lowercased_and_bounded() {
        let pairs = parse_string_map(br#"{"X-Session-Id":"abc"}"#, "metadata").unwrap();
        assert_eq!(
            validate_grpc(pairs).unwrap(),
            vec![("x-session-id".to_string(), "abc".to_string())]
        );
        for bad in [
            "{\"x-bin\":\"AA==\"}",
            "{\"capsulad-run-id\":\"r\"}",
            "{\"bad key\":\"v\"}",
            "{\"x-a\":\"caf\\u00e9\"}",
        ] {
            let pairs = parse_string_map(bad.as_bytes(), "metadata").expect("valid JSON");
            assert!(validate_grpc(pairs).is_err(), "{bad} must be rejected");
        }
    }

    #[test]
    fn grpc_rejects_protocol_owned_metadata_names() {
        for name in [
            "content-type",
            "Content-Type",
            "te",
            "trailer",
            "grpc-timeout",
            "grpc-encoding",
            "grpc-accept-encoding",
            "user-agent",
            "host",
        ] {
            let json = format!("{{\"{name}\":\"x\"}}");
            let pairs = parse_string_map(json.as_bytes(), "metadata").expect("valid JSON");
            let err = validate_grpc(pairs).expect_err("protocol metadata must be rejected");
            assert_eq!(err.status, INVALID_ARGUMENT, "{name}");
        }
    }

    #[test]
    fn non_object_and_short_values_are_rejected() {
        assert!(parse_string_map(b"[]", "metadata").is_err());
        assert!(parse_string_map(br#"{"x":1}"#, "metadata").is_err());
        assert!(parse_string_map(b"", "metadata").unwrap().is_empty());
    }
}
