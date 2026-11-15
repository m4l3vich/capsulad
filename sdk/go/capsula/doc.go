// Package capsula is the Go SDK for supervised Capsula applications.
//
// # Runtime model
//
// The package is a thin binding over the stable Capsula C ABI (v1). All
// transport (Unix-socket gRPC, protobuf reflection, framing, deadlines and
// buffer ownership) lives in the shared native engine libcapsula, which is
// loaded dynamically with dlopen at run time. No gRPC or protobuf dependency
// is linked into the application: the SDK depends only on the Go standard
// library and cgo.
//
// An app loads the library (CAPSULA_LIBRARY, or the platform SONAME), opens a
// Client, registers handlers, and calls Start. Start opens the single
// long-lived AppBus stream, identifies the app with the launcher-assigned run
// ID, signals READY, and blocks until the bus ends. Incoming custom calls,
// launch invocations, settings changes, actions and assistant activations are
// dispatched from that stream. Helpers for presentations, captures, input,
// audio and Bluetooth are thin wrappers over generic native framework
// operations, and CallJSON/Stream/OpenStream/OpenBidi provide an escape hatch
// for every other framework or app-provided method.
//
// # Payloads
//
// Payloads are plain Go values. The SDK marshals them with canonical protobuf
// JSON: lowerCamelCase field names, enum names, base64 bytes and 64-bit
// integers as strings. The native engine converts between that JSON and the
// framework's protobuf messages using descriptor reflection. Raw variants
// (Call, Stream.Send, ServerCall.Send) pass protobuf bytes through untouched.
// Binary stream frames (PCM audio) use the native binary fast path and are
// never base64-encoded: received bytes are copied exactly once into Go
// memory, and every native buffer is released before the call returns.
//
// Streams accept a context. Cancelling it cancels the native call and
// unblocks the reader without leaking a watcher goroutine. Call handles are
// released exactly once; Close methods are idempotent. Playback.Close is
// graceful: it half-closes the request side, waits for the framework to drain
// queued audio, and only then releases the handle. Use Playback.Abort for an
// immediate discard.
//
// # Build requirements
//
// cgo must be enabled (CGO_ENABLED=1) and a C compiler must be available for
// the target. On Linux the shim links only libdl (`-ldl`); libc and the cgo
// runtime's thread support are otherwise sufficient. Darwin needs no extra
// libraries. Cross-compiling a Linux/arm64 app therefore needs a matching C
// cross compiler, for example:
//
//	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
//	    CC=aarch64-linux-gnu-gcc go build ./...
//
// The vendored ABI declarations in abi.h are generated from the canonical
// native header by regenerate-abi.sh; the module does not read the repository
// at build time.
//
// A successfully loaded library is retained for the process lifetime. The
// engine owns a process-wide worker runtime whose threads may outlive any
// single client, so the SDK never dlcloses a working engine; Client.Close frees
// the client, bus and call handles only. A failed load or pre-init ABI
// negotiation is unloaded normally.
//
// There is no static transport fallback: if the shared library cannot be
// loaded, its ABI major is not 1, or its v1 table is incomplete, construction
// fails with a typed error (LibraryError or ABIError) before any framework
// state is touched.
package capsula
