package session

// AirPlay 2 control plane.
//
// A type-96 (realtime) stream carries its audio as UDP datagrams to the
// stream's dataPort, and the session has ONE UDP control socket, echoed as
// controlPort in every stream descriptor. The control socket carries the
// sender's AP2 control packets, not RTCP:
//
//	0xD4  time announce (AP1 sync; ignored for AirPlay 2 playback decisions)
//	0xD5  resend request  — this receiver SENDS these on loss
//	0xD6  resend response — carries one complete RTP packet we must play
//	0xD7  anchor: the sender's frame-/latency information
//
// A receiver never builds 0xD6; it only consumes it. Resend requests go to the
// source address of the last datagram received on the control socket (the
// SETUP request's controlPort is informational only). References: Shairport
// Sync rtp.c `rtp_ap2_control_receiver`/`send_resend_request`, pyatv
// protocols/airplay/ap2_session.py.

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"capsulad/apps/airplay/internal/rtp"
)

const (
	// ap2ControlMinPacket is the smallest datagram worth parsing: the reference
	// requires 28 bytes before it looks at the type byte.
	ap2ControlMinPacket = 28
	// ap2ControlSentinelGrace ignores control traffic whose sentinel bit is
	// clear during the first moments of a session; those packets belong to an
	// earlier session on the same port.
	ap2ControlSentinelGrace = 2 * time.Second
	// resendDelay is how long a missing sequence number is observed before a
	// resend is requested; a reordered packet usually arrives sooner.
	resendDelay = 100 * time.Millisecond
	// resendInterval bounds how often one stream asks for a resend.
	resendInterval = 250 * time.Millisecond
	// resendBatch caps the count carried in one 0xD5 request.
	resendBatch = 8
)

// controlState is the session's AP2 control socket and the sender address
// learned from it.
type controlState struct {
	listener  UDPListener
	startedAt time.Time

	mu   sync.Mutex
	peer UDPAddr
	has  bool
}

// ensureAP2Control opens the session's UDP control socket once. All realtime
// stream descriptors report its port.
func (s *Session) ensureAP2Control() error {
	s.mu.Lock()
	if s.ap2Control != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	listener, err := s.m.transport.OpenUDPListener("0.0.0.0", 0, "")
	if err != nil {
		return fmt.Errorf("open the AP2 control socket: %w", err)
	}
	state := &controlState{listener: listener, startedAt: time.Now()}
	s.mu.Lock()
	if s.ap2Control != nil {
		s.mu.Unlock()
		_ = listener.Close()
		return nil
	}
	s.ap2Control = state
	s.mu.Unlock()
	go s.serveAP2Control(state)
	return nil
}

// ap2ControlPort reports the bound control port, or 0 before it exists.
func (s *Session) ap2ControlPort() int {
	s.mu.Lock()
	state := s.ap2Control
	s.mu.Unlock()
	if state == nil {
		return 0
	}
	_, port := state.listener.Addr()
	return port
}

func (s *Session) closeAP2Control() {
	s.mu.Lock()
	state := s.ap2Control
	s.ap2Control = nil
	s.mu.Unlock()
	if state != nil {
		_ = state.listener.Close()
	}
}

func (s *Session) serveAP2Control(state *controlState) {
	for {
		packet, peer, err := state.listener.Receive()
		if err != nil {
			return
		}
		if len(packet) < ap2ControlMinPacket {
			continue
		}
		state.mu.Lock()
		state.peer, state.has = peer, true
		state.mu.Unlock()
		if time.Since(state.startedAt) < ap2ControlSentinelGrace && packet[0]&0x10 == 0 {
			continue
		}
		control, ok := parseAP2Control(packet)
		if !ok {
			continue
		}
		switch control.Kind {
		case controlResendResponse:
			s.deliverResent(control.Payload)
		case controlAnchor:
			// The sender's anchor frame carries the stream latency; the frame
			// the master time refers to is AnchorFrameRef, and upstream anchors
			// on AnchorFrame minus its fudge factor. Type-103 playout is not
			// timed by 0xD7: it only accepts SETRATEANCHORTI with its timeline.
			for _, stream := range s.activeStreams() {
				if stream.buffered != nil {
					continue
				}
				stream.setAnchor(control.AnchorFrameRef, control.anchorPlayoutFrame(), control.RemoteTimeNS, control.LatencyFrames)
			}
		case controlTimeAnnounce:
			for _, stream := range s.activeStreams() {
				if stream.buffered != nil {
					continue
				}
				stream.setTimeAnnounce(control.RTPTimestamp, 0)
			}
		}
	}
}

// deliverResent feeds a 0xD6 payload, which is a complete RTP packet, through
// the same decrypt/queue path as the data channel.
func (s *Session) deliverResent(packet []byte) {
	parsed, err := rtp.Parse(packet)
	if err != nil {
		return
	}
	for _, stream := range s.activeStreams() {
		if stream.buffered != nil {
			// Type 103 is an ordered TCP stream with no resend channel.
			continue
		}
		if stream.ssrc == 0 || stream.ssrc == parsed.SSRC {
			stream.deliver(packet)
			return
		}
	}
}

// sendResend asks the sender for missing sequence numbers. It is a no-op until
// the sender has sent something on the control socket, because there is no
// address to send to.
func (s *Session) sendResend(first, count uint16) bool {
	s.mu.Lock()
	state := s.ap2Control
	s.mu.Unlock()
	if state == nil {
		return false
	}
	state.mu.Lock()
	peer, has := state.peer, state.has
	state.mu.Unlock()
	if !has {
		return false
	}
	request := buildResendRequest(1, first, count)
	if err := state.listener.Send(peer, request); err != nil {
		return false
	}
	return true
}

// resendWatch is one stream's loss-recovery loop. It deliberately waits before
// asking, so packets that were merely reordered are not requested.
func (st *audioStream) resendWatch() {
	seen := map[uint16]time.Time{}
	var last time.Time
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-st.s.done:
			return
		case <-ticker.C:
		}
		if st.isClosed() || st.buffer.Pending() == 0 {
			clear(seen)
			continue
		}
		missing := st.buffer.Missing()
		if len(missing) == 0 {
			clear(seen)
			continue
		}
		now := time.Now()
		firstSeen, ok := seen[missing[0]]
		if !ok {
			seen[missing[0]] = now
			continue
		}
		if now.Sub(firstSeen) < resendDelay || now.Sub(last) < resendInterval {
			continue
		}
		count := len(missing)
		if count > resendBatch {
			count = resendBatch
		}
		if st.s.sendResend(missing[0], uint16(count)) {
			last = now
			delete(seen, missing[0])
		}
	}
}

var errNoControlSocket = errors.New("the AP2 control socket is not open")
