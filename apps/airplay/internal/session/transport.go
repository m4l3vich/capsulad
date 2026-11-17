// Package session implements the AirPlay receiver: the RTSP control channel,
// HAP pairing, the realtime/buffered audio streams and their lifecycle.
//
// This file declares the transport seam. Production code binds it to the
// capsulad native SDK (daemon-mediated listeners only — the app never creates
// or binds a socket itself); tests bind it to plain Go listeners on loopback.
package session

// Peer identifies the remote end of a stream or datagram.
type Peer struct {
	Address string
	Port    int
}

// Stream is a bidirectional byte stream: an accepted RTSP connection or a
// data-channel connection (realtime/buffered audio, control).
type Stream interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	// Close releases the stream and unblocks a pending Read or Write.
	Close() error
	// Peer returns the remote address, for logging and RTCP.
	Peer() Peer
}

// Listener is a daemon-allocated listening socket. Accept blocks until a
// connection arrives or the listener is closed.
type Listener interface {
	Accept() (Stream, error)
	// Addr reports the address and port the daemon actually bound.
	Addr() (address string, port int)
	Close() error
}

// UDPAddr is a datagram peer.
type UDPAddr struct {
	Address string
	Port    int
}

// UDPListener is a daemon-allocated UDP socket: either an ephemeral reply
// socket opened with Connect, or a fixed inbound port. Group is set when the
// socket joined a multicast group (PTP).
type UDPListener interface {
	Receive() ([]byte, UDPAddr, error)
	Send(peer UDPAddr, data []byte) error
	Addr() (address string, port int)
	Group() string
	Close() error
}

// Transport opens the sockets the daemon mediates on the app's behalf.
//
// joinGroup is empty for ordinary listeners. A non-empty group requests a
// multicast receive socket for the group address; the daemon decides whether
// that is permitted, and an unsupported request fails rather than silently
// binding something else.
type Transport interface {
	OpenTCPListener(address string, port int) (Listener, error)
	OpenUDPListener(address string, port int, joinGroup string) (UDPListener, error)
}

// Ring is the ring presentation the receiver drives while a session is live.
type Ring interface {
	// Pixels writes one frame; the slice is 12 RGB triples, pixel 0 at the top.
	Pixels(pixels []byte) error
	Close() error
}

// Sink is the framework playback stream.
type Sink interface {
	// Write queues interleaved 16-bit little-endian PCM. It blocks when the
	// framework queue is full, which is the backpressure the receiver wants.
	Write(pcm []byte) error
	// Flush discards queued audio without closing the stream.
	Flush() error
	// Close half-closes and drains what is queued.
	Close() error
	// Abort discards queued audio immediately.
	Abort() error
}

// Clock supplies the master clock when PTP is available. Sessions run without
// it (arrival-time playout) when OpenClock returns nil.
type Clock interface {
	// Offset reports (master - local) nanoseconds and whether it is disciplined.
	Offset() (int64, bool)
}
