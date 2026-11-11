# Implementation readiness — draft 0.5

**Verdict: enough to begin an implementation/simulator, not enough to freeze production v1.**

## Start now

1. Compile the `capsula.framework.v1` and optional media Protobuf definitions, generate initial Go/Rust clients, and implement daemon+mock devices.
2. Build a supervised read-only native app runner and single Unix-socket connection. Prove reverse-RPC and app-to-app calls with restart cleanup and bounded flow control.
3. Build one clock app and one assistant test app. Exercise first-frame display readiness, explicit release, stale/out-of-order capture cleanup and automatic return to clock.
4. Implement hardware backend capabilities behind an interface and probe the three **Mini** targets separately; never treat hardware variants as identical.
5. Add the HTTPS Web UI only behind mTLS with an explicit local/bootstrap CLI to enroll the first browser certificate and recover access.
6. Add native provenance evidence verification, showing both positive and failed/no-evidence states without a false safety guarantee.
7. Add QuickJS and MicroPython framework-owned runners, each in an isolated process, using their native transport bridge rather than requiring gRPC implementations in scripts.

## Must decide/test before production

| Issue | Why it matters | Pragmatic choice |
|---|---|---|
| mTLS bootstrap & recovery | A new user has no trusted browser certificate | Local CLI to generate trust root, enroll first client, rotate/revoke; recovery via physical/local console |
| Trust roots & provenance policy | Self-declared attestation can be forged | Verify shipped digest plus Sigstore identity/trust root; show unverified errors; never equate to safety |
| Script host API | QuickJS/MicroPython cannot be assumed to run standard gRPC SDKs | Framework-supplied host bridge with a small typed API, test streaming and errors |
| Read-only runtimes | Some interpreters and libraries expect `/tmp` | Force read-only imports/no caches; publish library compatibility list |
| Stock firmware coexistence | Vendor daemons may control devices | Use backend-specific bridges/ownership; probe on all three targets |
| Touch capability | Raw positions not guaranteed | Advertise coordinates only when confirmed by backend |
| Mic capture indicator | Must check *visible* output | Test watchdog with preemption, frame refresh and brightness-zero behavior |
| Built-in presets | Hardware geometry differs | Per-backend supported preset/face list, typed param validation |
| Device clock & persistent system state | Need good settings after reboot | Framework-owned atomic settings store and migration policy |
| Network listeners in sandboxes | Apps allowed outbound/inbound network | Clarify port conflicts and namespace network mode; disallow filesystem UDS listeners |

## Not needed to begin

An application-level permission/RBAC system, configurable rule engine, remote gRPC HAL, generalized filesystem storage API, large Capsula/Neo drivers, and universal native-plugin ABI.

## Acceptance tests for first vertical slice

- Clock displays by default, assistant submits first frame/effect and captures, clock frame continues updating invisibly, assistant explicitly releases, clock returns immediately.
- A foreground assistant crashes; all of its captures, routed calls, audio streams and conversations are released; supervisor backoff is observable.
- Partial touch capture keeps system volume/microphone toggle, full/raw capture suppresses system handlers and reports backend-limited raw readings.
- Two apps use the same `capsulad` Unix socket and make routed unary + streaming calls with no app IPC listener.
- QuickJS and MicroPython scripts can read settings, emit frames, log, and receive calls without ever writing to the filesystem.
- Web UI rejects missing/expired/revoked client certificates and has a tested local recovery path.
- Native package with genuine bundle verifies exact executable digest; forged/mismatched bundle is reported failed; no-evidence status is unambiguous.
