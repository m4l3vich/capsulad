//! TLS client construction and the socket type shared by HTTP and gRPC.
//!
//! There is no insecure mode: every client verifies the server against the
//! configured roots. Roots are the Mozilla bundle from `webpki-roots`
//! ("webpki"), that bundle plus the host's PEM store plus caller-supplied PEM
//! ("system"), or caller-supplied PEM only ("pem", for private PKI). TLS 1.2 is
//! the floor; ALPN is set to `h2` for gRPC.

use std::io;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;

use rustls::pki_types::{pem::PemObject, CertificateDer, ServerName};
use rustls::{ClientConfig, RootCertStore};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::net::TcpStream;
use tokio_rustls::TlsConnector;

use crate::ffi::{SdkError, SdkResult, INVALID_ARGUMENT, TIMEOUT, TRANSPORT};

/// System PEM bundles consulted by the default roots mode, in order.
const SYSTEM_PEM_FILES: &[&str] = &[
    "/etc/ssl/certs/ca-certificates.crt",
    "/etc/ssl/cert.pem",
    "/etc/pki/tls/certs/ca-bundle.crt",
];

const MAX_PEM_FILE: u64 = 4 * 1024 * 1024;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RootsMode {
    System,
    Webpki,
    /// Caller-supplied `ca_pem` only: no system bundle, no Mozilla roots.
    Pem,
}

#[derive(Debug, Clone)]
pub struct TlsOptions {
    pub roots: RootsMode,
    pub ca_pem: Option<Vec<u8>>,
}

impl Default for TlsOptions {
    fn default() -> Self {
        Self {
            roots: RootsMode::System,
            ca_pem: None,
        }
    }
}

/// Builds one verified TLS client configuration.
pub fn client_config(options: &TlsOptions, alpn_h2: bool) -> SdkResult<Arc<ClientConfig>> {
    let mut roots = RootCertStore::empty();
    if options.roots == RootsMode::System {
        for path in SYSTEM_PEM_FILES {
            let Some(bytes) = read_bounded(path) else {
                continue;
            };
            for cert in CertificateDer::pem_slice_iter(&bytes) {
                if let Ok(cert) = cert {
                    let _ = roots.add(cert);
                }
            }
        }
    }
    if options.roots != RootsMode::Pem {
        // Mozilla roots are always present in the default modes: for "system"
        // they are the fallback for hosts whose bundle is missing or sparse,
        // and for "webpki" they are the only source. "pem" is deliberately
        // narrower: a private-PKI client must not silently trust public CAs.
        roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
    }
    if let Some(pem) = options.ca_pem.as_deref() {
        let mut added = 0;
        for cert in CertificateDer::pem_slice_iter(pem) {
            let cert = cert.map_err(|err| {
                SdkError::new(INVALID_ARGUMENT, format!("ca_pem is not valid PEM: {err}"))
            })?;
            roots.add(cert).map_err(|err| {
                SdkError::new(INVALID_ARGUMENT, format!("ca_pem rejected: {err}"))
            })?;
            added += 1;
        }
        if added == 0 {
            return Err(SdkError::new(
                INVALID_ARGUMENT,
                "ca_pem contains no CERTIFICATE blocks",
            ));
        }
    } else if options.roots == RootsMode::Pem {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "roots \"pem\" requires ca_pem; no system or public roots are consulted",
        ));
    }
    if roots.is_empty() {
        return Err(SdkError::new(
            INVALID_ARGUMENT,
            "no TLS roots are available; install a system CA bundle or supply ca_pem",
        ));
    }

    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let mut config = ClientConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .map_err(|err| SdkError::new(INVALID_ARGUMENT, format!("TLS versions: {err}")))?
        .with_root_certificates(roots)
        .with_no_client_auth();
    if alpn_h2 {
        config.alpn_protocols = vec![b"h2".to_vec()];
    }
    Ok(Arc::new(config))
}

fn read_bounded(path: &str) -> Option<Vec<u8>> {
    let metadata = std::fs::metadata(path).ok()?;
    if !metadata.is_file() || metadata.len() > MAX_PEM_FILE {
        return None;
    }
    std::fs::read(path).ok()
}

/// Parses the SNI name, treating IP literals as IP addresses.
pub fn server_name(name: &str) -> SdkResult<ServerName<'static>> {
    ServerName::try_from(name.to_string()).map_err(|err| {
        SdkError::new(
            INVALID_ARGUMENT,
            format!("invalid TLS server name {name:?}: {err}"),
        )
    })
}

/// One TCP or TLS client stream. Both HTTP/1.1 and tonic's transport use this.
pub enum NetStream {
    Plain(TcpStream),
    Tls(Box<tokio_rustls::client::TlsStream<TcpStream>>),
}

impl AsyncRead for NetStream {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        match self.get_mut() {
            NetStream::Plain(stream) => Pin::new(stream).poll_read(cx, buf),
            NetStream::Tls(stream) => Pin::new(stream.as_mut()).poll_read(cx, buf),
        }
    }
}

impl AsyncWrite for NetStream {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        match self.get_mut() {
            NetStream::Plain(stream) => Pin::new(stream).poll_write(cx, buf),
            NetStream::Tls(stream) => Pin::new(stream.as_mut()).poll_write(cx, buf),
        }
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        match self.get_mut() {
            NetStream::Plain(stream) => Pin::new(stream).poll_flush(cx),
            NetStream::Tls(stream) => Pin::new(stream.as_mut()).poll_flush(cx),
        }
    }

    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        match self.get_mut() {
            NetStream::Plain(stream) => Pin::new(stream).poll_shutdown(cx),
            NetStream::Tls(stream) => Pin::new(stream.as_mut()).poll_shutdown(cx),
        }
    }
}

/// Connects one bounded TCP (and optional TLS) stream. DNS, TCP and handshake
/// share one deadline; any timeout is reported as `TIMEOUT`.
pub async fn connect(
    host: &str,
    port: u16,
    tls: Option<Arc<ClientConfig>>,
    name: Option<&str>,
    deadline: Duration,
) -> SdkResult<NetStream> {
    if deadline.is_zero() {
        return Err(SdkError::new(INVALID_ARGUMENT, "connect timeout is zero"));
    }
    let server_name = match (&tls, name) {
        (None, _) => None,
        (Some(_), Some(name)) => Some(server_name(name)?),
        (Some(_), None) => Some(server_name(host)?),
    };
    tokio::time::timeout(deadline, async move {
        let addresses = tokio::net::lookup_host((host, port))
            .await
            .map_err(|err| SdkError::new(TRANSPORT, format!("resolve {host}:{port}: {err}")))?;
        let mut last_error = None;
        let mut tcp = None;
        for address in addresses {
            match TcpStream::connect(address).await {
                Ok(stream) => {
                    tcp = Some(stream);
                    break;
                }
                Err(err) => last_error = Some(err),
            }
        }
        let tcp = tcp.ok_or_else(|| {
            SdkError::new(
                TRANSPORT,
                match last_error {
                    Some(err) => format!("connect {host}:{port}: {err}"),
                    None => format!("connect {host}:{port}: no addresses resolved"),
                },
            )
        })?;
        let _ = tcp.set_nodelay(true);
        match tls {
            None => Ok(NetStream::Plain(tcp)),
            Some(config) => {
                let Some(name) = server_name else {
                    return Err(SdkError::new(
                        crate::ffi::INTERNAL,
                        "missing TLS server name",
                    ));
                };
                let connector = TlsConnector::from(config);
                let stream = connector.connect(name, tcp).await.map_err(|err| {
                    SdkError::new(
                        TRANSPORT,
                        format!("TLS handshake with {host}:{port}: {err}"),
                    )
                })?;
                Ok(NetStream::Tls(Box::new(stream)))
            }
        }
    })
    .await
    .map_err(|_| {
        SdkError::new(
            TIMEOUT,
            format!("connect {host}:{port} timed out after {deadline:?}"),
        )
    })?
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn system_and_webpki_roots_are_available() {
        for roots in [RootsMode::System, RootsMode::Webpki] {
            let config = client_config(
                &TlsOptions {
                    roots,
                    ca_pem: None,
                },
                true,
            )
            .expect("roots must build");
            assert_eq!(config.alpn_protocols, vec![b"h2".to_vec()]);
        }
    }

    #[test]
    fn malformed_ca_pem_is_rejected() {
        let err = client_config(
            &TlsOptions {
                roots: RootsMode::Webpki,
                ca_pem: Some(b"not a certificate".to_vec()),
            },
            false,
        )
        .expect_err("malformed PEM must be rejected");
        assert_eq!(err.status, INVALID_ARGUMENT);
    }

    #[test]
    fn ip_and_dns_names_parse() {
        assert!(matches!(
            server_name("127.0.0.1").unwrap(),
            ServerName::IpAddress(_)
        ));
        assert!(matches!(
            server_name("localhost").unwrap(),
            ServerName::DnsName(_)
        ));
        assert!(server_name("").is_err());
    }
}
