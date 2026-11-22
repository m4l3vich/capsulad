package install

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HealthPath is the daemon's unauthenticated readiness endpoint.
const HealthPath = "/healthz"

// PinnedTLSConfig trusts exactly the device's server certificate. There is no
// insecure mode: a wrong or mismatched certificate fails the handshake.
//
// The certificate is self-signed by `auth init` and may carry the device
// hostname and its LAN IP. When the address the user typed is not in the SANs
// but the certificate names a hostname, that name is verified instead: chain
// and name are both still checked against the pinned leaf.
func PinnedTLSConfig(certPEM []byte, address string) (*tls.Config, error) {
	certs, name, err := pinnedPool(certPEM, address)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    certs,
		ServerName: name,
	}, nil
}

func pinnedPool(certPEM []byte, address string) (*x509.CertPool, string, error) {
	pool := x509.NewCertPool()
	var leaf *x509.Certificate
	rest := certPEM
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, "", fmt.Errorf("parse server certificate: %w", err)
		}
		pool.AddCert(cert)
		if leaf == nil {
			leaf = cert
		}
	}
	if leaf == nil {
		return nil, "", fmt.Errorf("no PEM certificate in the device's server.crt")
	}
	host := hostOnly(address)
	if err := leaf.VerifyHostname(host); err == nil {
		return pool, host, nil
	}
	if len(leaf.DNSNames) > 0 {
		return pool, leaf.DNSNames[0], nil
	}
	if len(leaf.IPAddresses) > 0 {
		return pool, leaf.IPAddresses[0].String(), nil
	}
	return nil, "", fmt.Errorf("device certificate has no name matching %s", host)
}

// hostOnly strips an ADB or SSH port from the user-entered address.
func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(addr, "[]")
}

// HTTSPBase is the Web UI origin for an address.
func HTTPSBase(addr string, port int) string {
	if port == 0 {
		port = 8443
	}
	return fmt.Sprintf("https://%s", net.JoinHostPort(hostOnly(addr), fmt.Sprint(port)))
}

// WaitHealth polls GET <base>/healthz until it returns 200 or ctx expires. The
// base URL is normally https://<device>:8443; tests point it at an in-process
// TLS server.
func WaitHealth(ctx context.Context, certPEM []byte, base string, interval time.Duration, log func(string, ...any)) error {
	if interval <= 0 {
		interval = time.Second
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	tlsCfg, err := PinnedTLSConfig(certPEM, u.Host)
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   tlsCfg,
			DisableKeepAlives: true,
		},
		Timeout: 5 * time.Second,
		// /healthz must be served directly by the device, not by a redirect.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	healthURL := strings.TrimRight(base, "/") + HealthPath
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health endpoint returned HTTP %s", resp.Status)
		} else {
			lastErr = err
		}
		if log != nil {
			log("waiting for %s: %v", healthURL, lastErr)
		}
		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return fmt.Errorf("Web UI did not become healthy in time: %w", lastErr)
		case <-time.After(interval):
		}
	}
}

// CertFingerprint is the SHA-256 digest of the leaf certificate's DER bytes.
func CertFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.Raw)
	compact := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(compact); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(compact[i : i+2])
	}
	return b.String(), nil
}
