# Web UI TLS certificate bootstrap — draft 0.5

- `capsulad` gRPC HAL and application bus remain **Unix-domain-socket only**.
- The browser-facing Web UI is an **HTTPS** service with client-certificate verification required by default. **No Web UI API keys.**
- App-owned TCP/UDP network listeners remain separate: they are not automatically authenticated or protected by Web UI mTLS. Their developers must design any needed authentication for those public endpoints.
- A client cert is an administrative access credential to the UI; it is **not** a permission grant to an installed app.
- Provision the first browser certificate with a CLI available from the device's local console / an authenticated local maintenance session. Do not make a remote unauthenticated enrollment URL.
- The CLI should: initialize a trust root, issue server certificate with valid SANs for selected hostname/IP, issue/import client certificate and private key, list/revoke client certs, renew server cert, show trusted CA fingerprint, and support lost-client recovery. An enrollment UX can later abstract this, but should not weaken bootstrap.
- Server verifies client chain, validity dates and revocation state. Browser must verify server identity; do not tell users to ignore certificate errors.
- Continue to use CSRF defenses, secure cookies and appropriate Origin checks for state-changing web actions even with mTLS.
- Expose only non-secret `SystemService.GetWebUiSecurity` status over the local framework connection. Private keys and issued client credential material never enter the app-facing Protobuf API.

## Open implementation decisions

Certificate format/issuance (PEM/P12), CA key storage, renewal defaults, CRL/allowlist approach, hostname/SAN configuration, proxy termination policy, first-run mobile browser support, and recovery UX. All need tests before a network-visible Web UI ships.
