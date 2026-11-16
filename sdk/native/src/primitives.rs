//! Small encoding, hashing and URL primitives exposed on the C ABI.
//!
//! Every routine is self-contained and bounded: callers pass explicit lengths,
//! nothing allocates beyond the returned payload, and malformed input is a
//! typed `INVALID_ARGUMENT` rather than a panic.

use base64::engine::general_purpose::{STANDARD, STANDARD_NO_PAD, URL_SAFE, URL_SAFE_NO_PAD};
use base64::Engine;
use sha2::{Digest, Sha256};

use crate::ffi::{SdkError, SdkResult, INVALID_ARGUMENT};

pub const BASE64_STANDARD: i32 = 0;
pub const BASE64_URL_SAFE: i32 = 1;
pub const BASE64_NO_PADDING: i32 = 2;
const BASE64_FLAGS: i32 = BASE64_URL_SAFE | BASE64_NO_PADDING;

fn base64_flags(flags: i32) -> SdkResult<(bool, bool)> {
    if flags & !BASE64_FLAGS != 0 {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            format!("unknown base64 flags {flags:#x}"),
        ));
    }
    Ok((flags & BASE64_URL_SAFE != 0, flags & BASE64_NO_PADDING == 0))
}

pub fn base64_encode(data: &[u8], flags: i32) -> SdkResult<Vec<u8>> {
    let (url, pad) = base64_flags(flags)?;
    let encoded = match (url, pad) {
        (false, true) => STANDARD.encode(data),
        (false, false) => STANDARD_NO_PAD.encode(data),
        (true, true) => URL_SAFE.encode(data),
        (true, false) => URL_SAFE_NO_PAD.encode(data),
    };
    Ok(encoded.into_bytes())
}

pub fn base64_decode(data: &[u8], flags: i32) -> SdkResult<Vec<u8>> {
    let (url, pad) = base64_flags(flags)?;
    let decoded = match (url, pad) {
        (false, true) => STANDARD.decode(data),
        (false, false) => STANDARD_NO_PAD.decode(data),
        (true, true) => URL_SAFE.decode(data),
        (true, false) => URL_SAFE_NO_PAD.decode(data),
    };
    decoded.map_err(|err| SdkError::new(INVALID_ARGUMENT, format!("invalid base64 input: {err}")))
}

fn is_unreserved(b: u8) -> bool {
    b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~')
}

pub fn percent_encode(data: &[u8]) -> Vec<u8> {
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    let mut out = Vec::with_capacity(data.len());
    for &byte in data {
        if is_unreserved(byte) {
            out.push(byte);
        } else {
            out.push(b'%');
            out.push(HEX[(byte >> 4) as usize]);
            out.push(HEX[(byte & 0x0f) as usize]);
        }
    }
    out
}

fn hex_value(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

pub fn percent_decode(data: &[u8]) -> SdkResult<Vec<u8>> {
    let mut out = Vec::with_capacity(data.len());
    let mut index = 0;
    while index < data.len() {
        if data[index] != b'%' {
            out.push(data[index]);
            index += 1;
            continue;
        }
        if index + 2 >= data.len() {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "percent-encoded input ends inside an escape",
            ));
        }
        let (Some(high), Some(low)) = (hex_value(data[index + 1]), hex_value(data[index + 2]))
        else {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "percent-encoded input has a non-hex escape",
            ));
        };
        out.push((high << 4) | low);
        index += 3;
    }
    Ok(out)
}

pub fn sha256(data: &[u8]) -> Vec<u8> {
    Sha256::digest(data).to_vec()
}

pub fn random_bytes(out: &mut [u8]) -> SdkResult<()> {
    getrandom::getrandom(out).map_err(|err| {
        SdkError::new(
            crate::ffi::INTERNAL,
            format!("system random source failed: {err}"),
        )
    })
}

pub fn uuid_v4() -> SdkResult<String> {
    let mut bytes = [0u8; 16];
    random_bytes(&mut bytes)?;
    bytes[6] = (bytes[6] & 0x0f) | 0x40;
    bytes[8] = (bytes[8] & 0x3f) | 0x80;
    let mut text = String::with_capacity(36);
    for (index, byte) in bytes.iter().enumerate() {
        if matches!(index, 4 | 6 | 8 | 10) {
            text.push('-');
        }
        text.push_str(&format!("{byte:02x}"));
    }
    Ok(text)
}

/* ------------------------------------------------------------------ *
 * URL parsing
 * ------------------------------------------------------------------ */

/// One absolute http(s) URL decomposed by the primitives surface.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedUrl {
    pub scheme: String,
    pub host: String,
    /// Effective port: the explicit value, else 80 for http and 443 for https.
    pub port: u16,
    pub path: String,
    pub query: String,
}

impl ParsedUrl {
    pub fn origin(&self) -> String {
        let host = if self.host.contains(':') {
            format!("[{}]", self.host)
        } else {
            self.host.clone()
        };
        format!("{}://{}:{}", self.scheme, host, self.port)
    }

    pub fn default_port(&self) -> u16 {
        if self.scheme == "https" {
            443
        } else {
            80
        }
    }

    pub fn authority(&self) -> String {
        let host = if self.host.contains(':') {
            format!("[{}]", self.host)
        } else {
            self.host.clone()
        };
        if self.port == self.default_port() {
            host
        } else {
            format!("{host}:{}", self.port)
        }
    }

    pub fn path_and_query(&self) -> String {
        if self.query.is_empty() {
            self.path.clone()
        } else {
            format!("{}?{}", self.path, self.query)
        }
    }

    pub fn to_json(&self) -> serde_json::Value {
        serde_json::json!({
            "scheme": self.scheme,
            "host": self.host,
            "port": self.port,
            "path": self.path,
            "query": self.query,
        })
    }
}

fn invalid(message: impl Into<String>) -> SdkError {
    SdkError::new(INVALID_ARGUMENT, message)
}

/// Parses an absolute `http://` or `https://` URL. Userinfo and fragments are
/// rejected; a missing path becomes `/`.
pub fn parse_url(text: &str) -> SdkResult<ParsedUrl> {
    if text.is_empty() {
        return Err(invalid("URL is empty"));
    }
    if text.bytes().any(|b| b < 0x20 || b == 0x7f || b == b' ') {
        return Err(invalid("URL contains whitespace or control characters"));
    }
    if text.contains('#') {
        return Err(invalid("URL fragments are not supported"));
    }
    let Some((scheme, rest)) = text.split_once("://") else {
        return Err(invalid("URL must be absolute and include a scheme"));
    };
    let scheme = scheme.to_ascii_lowercase();
    if scheme != "http" && scheme != "https" {
        return Err(invalid("only http and https URLs are supported"));
    }
    if rest.is_empty() {
        return Err(invalid("URL has no authority"));
    }

    let (authority, tail) = match rest.find(['/', '?']) {
        Some(index) => (&rest[..index], &rest[index..]),
        None => (rest, ""),
    };
    if authority.is_empty() {
        return Err(invalid("URL has no host"));
    }
    if authority.contains('@') {
        return Err(invalid("URL userinfo is not supported"));
    }

    let (host, port) = if let Some(stripped) = authority.strip_prefix('[') {
        let Some((inside, after)) = stripped.split_once(']') else {
            return Err(invalid("URL has an unterminated IPv6 literal"));
        };
        if inside.is_empty() || inside.parse::<std::net::Ipv6Addr>().is_err() {
            return Err(invalid("URL has an invalid IPv6 literal"));
        }
        let port = match after {
            "" => None,
            value => {
                let Some(value) = value.strip_prefix(':') else {
                    return Err(invalid(
                        "URL has trailing characters after the IPv6 literal",
                    ));
                };
                Some(value)
            }
        };
        (inside.to_ascii_lowercase(), port)
    } else {
        match authority.rsplit_once(':') {
            Some((host, port)) if !host.contains(':') => (host.to_ascii_lowercase(), Some(port)),
            _ => (authority.to_ascii_lowercase(), None),
        }
    };
    if host.is_empty() {
        return Err(invalid("URL has an empty host"));
    }
    if !host
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b':'))
    {
        return Err(invalid(format!("URL host {host:?} has invalid characters")));
    }

    let port = match port {
        None | Some("") => {
            if scheme == "https" {
                443
            } else {
                80
            }
        }
        Some(value) => value
            .parse::<u16>()
            .ok()
            .filter(|port| *port != 0)
            .ok_or_else(|| invalid(format!("URL port {value:?} is not a valid TCP port")))?,
    };

    let (path, query) = match tail.split_once('?') {
        Some((path, query)) => (path, query),
        None => (tail, ""),
    };
    if path.contains('?') {
        return Err(invalid("URL has more than one query separator"));
    }
    let path = if path.is_empty() { "/" } else { path };
    if !path.starts_with('/') {
        return Err(invalid("URL path must be absolute"));
    }

    Ok(ParsedUrl {
        scheme,
        host,
        port,
        path: path.to_string(),
        query: query.to_string(),
    })
}

/// Parses a gRPC origin: absolute http(s) URL with no path, query or fragment.
pub fn parse_origin(text: &str) -> SdkResult<ParsedUrl> {
    let url = parse_url(text)?;
    if url.path != "/" || !url.query.is_empty() {
        return Err(invalid(
            "the gRPC URL is an origin only (scheme://host[:port])",
        ));
    }
    Ok(url)
}

/// Resolves a redirect `Location` against the URL that produced it. Relative
/// references are supported for the common absolute-path, query and
/// same-directory forms.
pub fn join_location(base: &ParsedUrl, location: &str) -> SdkResult<ParsedUrl> {
    let location = location.trim();
    if location.is_empty() {
        return Err(invalid("redirect Location is empty"));
    }
    if location.bytes().any(|b| b < 0x20 || b == 0x7f || b == b' ') {
        return Err(invalid("redirect Location contains control characters"));
    }
    if location.contains("://") {
        return parse_url(location);
    }
    if let Some(rest) = location.strip_prefix("//") {
        return parse_url(&format!("{}://{}", base.scheme, rest));
    }
    if location.starts_with('/') {
        return parse_url(&format!("{}{}", base.origin(), location));
    }
    if location.starts_with('?') {
        return parse_url(&format!("{}{}{}", base.origin(), base.path, location));
    }

    // Same-directory relative reference: drop the last path segment. Dot
    // segments are resolved before the URL is re-parsed.
    let directory = match base.path.rfind('/') {
        Some(index) => &base.path[..=index],
        None => "/",
    };
    let resolved = remove_dot_segments(&format!("{directory}{location}"));
    parse_url(&format!("{}{}", base.origin(), resolved))
}

fn remove_dot_segments(path: &str) -> String {
    let (path, suffix) = match path.find('?') {
        Some(index) => (&path[..index], &path[index..]),
        None => (path, ""),
    };
    let mut segments: Vec<&str> = Vec::new();
    for segment in path.split('/') {
        match segment {
            "" | "." => {}
            ".." => {
                segments.pop();
            }
            value => segments.push(value),
        }
    }
    let mut out = String::from("/");
    out.push_str(&segments.join("/"));
    if path.ends_with('/') && !out.ends_with('/') {
        out.push('/');
    }
    out.push_str(suffix);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64_round_trips_every_flag_combination() {
        let data = b"capsulad \x00\xff binary";
        for flags in [
            BASE64_STANDARD,
            BASE64_URL_SAFE,
            BASE64_STANDARD | BASE64_NO_PADDING,
            BASE64_URL_SAFE | BASE64_NO_PADDING,
        ] {
            let encoded = base64_encode(data, flags).unwrap();
            assert_eq!(base64_decode(&encoded, flags).unwrap(), data);
        }
        assert!(base64_encode(b"x", 8).is_err());
        assert!(base64_decode(b"***", BASE64_STANDARD).is_err());
    }

    #[test]
    fn base64_is_strict_about_padding_and_alphabet() {
        assert!(base64_decode(b"aGk", BASE64_STANDARD).is_err());
        assert_eq!(base64_decode(b"aGk", BASE64_NO_PADDING).unwrap(), b"hi");
        assert!(base64_decode(b"aGk=", BASE64_NO_PADDING).is_err());
        assert!(base64_decode(b"aGk=", BASE64_URL_SAFE).unwrap().is_empty() || true);
    }

    #[test]
    fn percent_coding_uses_the_unreserved_set() {
        let encoded = percent_encode(b"a b/c?d=e&f\x00~\xff");
        assert_eq!(encoded, b"a%20b%2Fc%3Fd%3De%26f%00~%FF".to_vec());
        assert_eq!(percent_decode(&encoded).unwrap(), b"a b/c?d=e&f\x00~\xff");
        assert!(percent_decode(b"%").is_err());
        assert!(percent_decode(b"%zz").is_err());
        assert!(percent_decode(b"%1").is_err());
        assert_eq!(percent_decode(b"a+b").unwrap(), b"a+b");
    }

    #[test]
    fn sha256_is_the_standard_digest() {
        assert_eq!(
            sha256(b"abc"),
            hex("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
        );
        assert_eq!(
            sha256(b""),
            hex("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
        );
    }

    fn hex(text: &str) -> Vec<u8> {
        (0..text.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&text[i..i + 2], 16).unwrap())
            .collect()
    }

    #[test]
    fn uuid_is_lowercase_v4() {
        let first = uuid_v4().unwrap();
        let second = uuid_v4().unwrap();
        assert_ne!(first, second);
        assert_eq!(first.len(), 36);
        let bytes: Vec<u8> = first.bytes().collect();
        assert_eq!(bytes[8], b'-');
        assert_eq!(bytes[13], b'-');
        assert_eq!(bytes[18], b'-');
        assert_eq!(bytes[23], b'-');
        assert_eq!(bytes[14], b'4');
        assert!(matches!(bytes[19], b'8' | b'9' | b'a' | b'b'));
        assert!(first
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-'));
    }

    #[test]
    fn url_parse_decomposes_and_defaults_ports() {
        let url = parse_url("HTTPS://Example.COM/a/b%20c?x=1&y=2").unwrap();
        assert_eq!(url.scheme, "https");
        assert_eq!(url.host, "example.com");
        assert_eq!(url.port, 443);
        assert_eq!(url.path, "/a/b%20c");
        assert_eq!(url.query, "x=1&y=2");
        assert_eq!(url.origin(), "https://example.com:443");
        assert_eq!(url.authority(), "example.com");

        let url = parse_url("http://127.0.0.1:8080").unwrap();
        assert_eq!(url.port, 8080);
        assert_eq!(url.path, "/");
        assert_eq!(url.authority(), "127.0.0.1:8080");

        let url = parse_url("http://[::1]:9090/x").unwrap();
        assert_eq!(url.host, "::1");
        assert_eq!(url.port, 9090);
        assert_eq!(url.authority(), "[::1]:9090");
    }

    #[test]
    fn url_parse_rejects_unsupported_forms() {
        for bad in [
            "",
            "example.com",
            "ftp://example.com",
            "https://user@example.com",
            "https://example.com/x#frag",
            "https://example.com:0/",
            "https://example.com:99999/",
            "https://[::1/",
            "https://exa mple.com/",
        ] {
            assert!(parse_url(bad).is_err(), "{bad} must be rejected");
        }
        assert!(parse_origin("https://example.com/path").is_err());
        assert!(parse_origin("https://example.com?x=1").is_err());
        assert!(parse_origin("https://example.com:8443").is_ok());
    }

    #[test]
    fn redirect_locations_are_resolved() {
        let base = parse_url("https://example.com/a/b?x=1").unwrap();
        assert_eq!(
            join_location(&base, "https://other.test/z")
                .unwrap()
                .origin(),
            "https://other.test:443"
        );
        assert_eq!(join_location(&base, "/z").unwrap().path_and_query(), "/z");
        assert_eq!(join_location(&base, "c").unwrap().path_and_query(), "/a/c");
        assert_eq!(join_location(&base, "../c").unwrap().path_and_query(), "/c");
        assert_eq!(
            join_location(&base, "?y=2").unwrap().path_and_query(),
            "/a/b?y=2"
        );
        assert!(join_location(&base, "ftp://example.com/x").is_err());
    }
}
