package websocket

import "crypto/rand"

// cryptoRandRead is the handshake key and frame-mask source. crypto/rand is the
// Go standard library's entropy source; it is not a network stack, and unlike
// the SDK's RandomBytes it needs no native engine, so the framing code is
// testable without the framework runtime.
func cryptoRandRead(p []byte) (int, error) { return rand.Read(p) }
