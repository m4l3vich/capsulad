package session

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"time"

	"capsulad/apps/airplay/internal/aac"
	"capsulad/apps/airplay/internal/airplayinfo"
	"capsulad/apps/airplay/internal/alac"
	"capsulad/apps/airplay/internal/artwork"
	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/plist"
	"capsulad/apps/airplay/internal/rtp"
	"capsulad/apps/airplay/internal/rtsp"
)

// audioStreamSpec describes one audio channel requested by a sender.
type audioStreamSpec struct {
	streamType         int
	codec              string // "alac", "aac" or "pcm"
	config             alac.Config
	aacProfile         aac.Profile
	sharedKey          []byte // AirPlay 2 `shk`, nil for an unencrypted stream
	aesKey             []byte // legacy RAOP key (from ANNOUNCE)
	aesIV              []byte
	samplesPerFrame    int
	interleavedData    int
	interleavedControl int
	useUDP             bool
}

// audioStream owns one sender audio channel: its listeners, its jitter buffer,
// its decoder and its framework playback stream.
type audioStream struct {
	s    *Session
	spec audioStreamSpec
	id   uint32

	dataListener    Listener
	controlListener Listener
	udpData         UDPListener
	udpControl      UDPListener
	decoder         frameDecoder
	// decryptMu serializes decryption: the stream decoder keeps a reusable
	// buffer, and a resent 0xD6 packet is decrypted on the control goroutine
	// while the data goroutine may be decrypting the next packet.
	decryptMu     sync.Mutex
	rtpDecoder    *rtp.RealtimeDecoder
	legacyDecoder *rtp.LegacyDecoder
	buffer        *rtp.Buffer
	buffered      *bufferedStream

	mu               sync.Mutex
	controlConn      Stream
	dataConn         Stream
	sink             Sink
	started          bool
	launched         bool
	paused           bool
	closed           bool
	failed           error
	pcm              []byte
	sequence         uint32
	ssrc             uint32
	anchor           time.Time
	anchorTS         uint32
	anchorFromSender bool
	anchorFrame      uint32
	anchorFrameRef   uint32
	anchorMasterNS   uint64
	latencyFrames    uint32
	lastPop          time.Time
	frames           uint64
	senderRTP        uint32
	hasSenderSR      bool
}

var nextStreamID = uint32(1)

func (s *Session) newAudioStream(spec audioStreamSpec) (*audioStream, error) {
	if spec.samplesPerFrame <= 0 {
		spec.samplesPerFrame = valueOrDefault(int(spec.config.FrameLength), 352)
	}
	if spec.config.SampleRate == 0 {
		spec.config.SampleRate = 44100
	}
	if spec.config.Channels == 0 {
		spec.config.Channels = 2
	}
	if spec.config.BitDepth == 0 {
		spec.config.BitDepth = 16
	}
	stream := &audioStream{
		s:    s,
		spec: spec,
		id:   nextStreamID,
	}
	nextStreamID++
	if spec.streamType == streamTypeBuffered {
		if spec.codec != "alac" && spec.codec != "aac" {
			return nil, fmt.Errorf("buffered codec %q is not supported", spec.codec)
		}
		if len(spec.sharedKey) != 32 {
			return nil, errors.New("buffered audio requires a 32-byte shared key")
		}
		stream.buffered = newBufferedStream(stream)
		// Type 103 has its own TCP framing; it must never match the legacy
		// RTSP interleaved channel zero used by a classic RAOP stream.
		stream.spec.interleavedData = -1
		stream.spec.interleavedControl = -1
	} else {
		stream.buffer = rtp.NewBuffer(spec.config.SampleRate, s.m.cfg.FrameTarget)
		switch spec.codec {
		case "alac":
			decoder, err := alac.NewDecoder(spec.config)
			if err != nil {
				return nil, fmt.Errorf("ALAC decoder: %w", err)
			}
			stream.decoder = alacFrameDecoder{decoder}
		case "aac":
			decoder, err := aac.New(aac.Config{
				SampleRate: int(spec.config.SampleRate),
				Channels:   int(spec.config.Channels),
				Profile:    spec.aacProfile,
			})
			if err != nil {
				return nil, fmt.Errorf("AAC decoder: %w", err)
			}
			stream.decoder = aacFrameDecoder{decoder}
		case "pcm":
		default:
			return nil, fmt.Errorf("codec %q is not supported", spec.codec)
		}
	}
	switch {
	case spec.useUDP || spec.streamType == streamTypeRealtime:
		// Realtime audio (AP2 type 96 and legacy RAOP over UDP) arrives as
		// datagrams on the returned dataPort. The AP2 control socket is the
		// session's, not the stream's.
		data, err := s.m.transport.OpenUDPListener("0.0.0.0", 0, "")
		if err != nil {
			return nil, fmt.Errorf("open the audio datagram listener: %w", err)
		}
		stream.udpData = data
		if len(spec.sharedKey) == 32 {
			decoder, err := rtp.NewRealtimeDecoder(spec.sharedKey)
			if err != nil {
				_ = data.Close()
				return nil, fmt.Errorf("realtime decoder: %w", err)
			}
			stream.rtpDecoder = decoder
		} else if len(spec.aesKey) == 16 {
			decoder, err := rtp.NewLegacyDecoder(spec.aesKey, spec.aesIV)
			if err != nil {
				_ = data.Close()
				return nil, fmt.Errorf("legacy decoder: %w", err)
			}
			stream.legacyDecoder = decoder
		}
		if spec.useUDP {
			// Legacy RAOP over UDP expects a receiver-chosen control port; AP2
			// uses the session-wide control socket instead.
			control, err := s.m.transport.OpenUDPListener("0.0.0.0", 0, "")
			if err != nil {
				_ = data.Close()
				return nil, fmt.Errorf("open the legacy control datagram listener: %w", err)
			}
			stream.udpControl = control
		}
	case spec.streamType == streamTypeBuffered:
		// Buffered audio (type 103) is a TCP stream whose packets carry a
		// 2-byte big-endian length that includes its own two bytes.
		data, err := s.m.transport.OpenTCPListener("0.0.0.0", 0)
		if err != nil {
			return nil, fmt.Errorf("open the buffered audio channel listener: %w", err)
		}
		stream.dataListener = data
	}
	// The listeners are open so the SETUP reply can name their ports, but no
	// reader or playback loop runs until the session commits ownership.
	return stream, nil
}

// stageStream holds a stream created by a successful SETUP until the session
// owns the receiver. A refused SETUP discards everything it staged.
func (s *Session) stageStream(stream *audioStream) {
	s.mu.Lock()
	s.staged = append(s.staged, stream)
	s.mu.Unlock()
}

func (s *Session) stagedStreams() []*audioStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*audioStream(nil), s.staged...)
}

// discardStagedStreams closes listeners created by SETUP that never committed.
// No reader or playback loop was started, so there is nothing to drain.
func (s *Session) discardStagedStreams() {
	s.mu.Lock()
	staged := s.staged
	s.staged = nil
	s.mu.Unlock()
	for _, stream := range staged {
		stream.stop()
	}
}

// commitStreams makes the SETUP streams live: they join the session's stream
// list and start their readers and playback loop. It runs once, after the
// success response and ownership commit.
func (s *Session) commitStreams() {
	if !s.m.owns(s) {
		// A takeover beat this commit: do not start audio for the previous
		// owner. The new owner's commit closes this session.
		s.discardStagedStreams()
		return
	}
	s.mu.Lock()
	staged := s.staged
	s.staged = nil
	s.streams = append(s.streams, staged...)
	s.mu.Unlock()
	for _, stream := range staged {
		stream.launch()
		s.m.logf("session from %s: stream %d type %d codec %s %d Hz %d ch", s.peer.Address, stream.id, stream.spec.streamType, stream.spec.codec, stream.spec.config.SampleRate, stream.spec.config.Channels)
	}
}

// launch starts the network readers and playback loop exactly once.
func (st *audioStream) launch() {
	st.mu.Lock()
	if st.launched || st.closed {
		st.mu.Unlock()
		return
	}
	st.launched = true
	data, control, listener := st.udpData, st.udpControl, st.dataListener
	st.mu.Unlock()
	if st.buffered != nil {
		// Buffered audio has one TCP data connection and an ordered queue; it
		// never enters the UDP jitter/resend path.
		go st.serveBufferedDataConnections(listener)
		go st.buffered.playLoop()
		return
	}
	if control != nil {
		go st.serveLegacyControl(control)
	}
	if data != nil {
		go st.serveUDP(data)
		go st.resendWatch()
	}
	go st.playLoop()
}

// descriptor is the SETUP response entry for this stream.
func (st *audioStream) descriptor() map[string]any {
	controlPort, dataPort := 0, 0
	switch {
	case st.dataListener != nil:
		_, dataPort = st.dataListener.Addr()
	case st.udpData != nil:
		_, dataPort = st.udpData.Addr()
	}
	controlPort = st.s.ap2ControlPort()
	bufferSize := 1 << 20
	descriptor := map[string]any{
		"type":            int64(st.spec.streamType),
		"controlPort":     int64(controlPort),
		"dataPort":        int64(dataPort),
		"audioBufferSize": int64(bufferSize),
	}
	if st.spec.streamType == streamTypeBuffered {
		// Type 103 identifies the stream by its TCP data port and negotiated
		// codec; the reference omits streamID for buffered streams, and the
		// advertised capacity is the real bounded queue, not a placeholder.
		descriptor["audioBufferSize"] = int64(bufferedAudioBufferSize)
		return descriptor
	}
	descriptor["streamID"] = int64(st.id)
	return descriptor
}

func (st *audioStream) udpPorts() (data, control int) {
	if st.udpData != nil {
		_, data = st.udpData.Addr()
	}
	if st.udpControl != nil {
		_, control = st.udpControl.Addr()
	}
	return data, control
}

func (st *audioStream) sampleRate() int {
	return int(valueOrDefault(int(st.spec.config.SampleRate), 44100))
}

// --- network readers -------------------------------------------------------

// serveLegacyControl drains the legacy RAOP control socket. Resend recovery
// needs a packet cache this receiver does not keep, so requests are counted and
// dropped rather than answered with the wrong bytes; the advertised control
// port is real and RTCP/time traffic has somewhere to go.
func (st *audioStream) serveLegacyControl(listener UDPListener) {
	for {
		packet, _, err := listener.Receive()
		if err != nil {
			return
		}
		if len(packet) >= 8 && packet[1]&0x7f == 0x55 {
			continue
		}
	}
}

func (st *audioStream) serveUDP(listener UDPListener) {
	for {
		packet, _, err := listener.Receive()
		if err != nil {
			return
		}
		st.deliver(packet)
	}
}

// deliver decrypts and queues one RTP packet.
func (st *audioStream) deliver(raw []byte) {
	if len(raw) == 0 || st.buffered != nil || st.buffer == nil || st.isClosed() {
		return
	}
	st.s.touch()
	st.s.markAudio()
	var packet *rtp.Packet
	var err error
	if len(st.spec.sharedKey) == 0 && len(st.spec.aesKey) == 0 {
		// An unencrypted stream has no tag or nonce, so its last packet may be
		// shorter than an encrypted one; the strict parser would drop it.
		packet, err = rtp.ParseUnencrypted(raw)
	} else {
		packet, err = rtp.Parse(raw)
	}
	if err != nil {
		return
	}
	st.mu.Lock()
	if st.ssrc == 0 {
		st.ssrc = packet.SSRC
	}
	st.mu.Unlock()
	st.decryptMu.Lock()
	plaintext, err := st.decrypt(packet)
	st.decryptMu.Unlock()
	if err != nil {
		if !st.isClosed() {
			st.s.m.logf("session %s: dropping undecryptable packet: %v", st.s.peer.Address, err)
		}
		return
	}
	st.buffer.Push(packet, plaintext)
}

// decrypt applies the stream's key material. An unencrypted stream (legacy
// RAOP with et=0, or PCM) has no tag and no nonce, so the packet's raw bytes
// after the header are the payload.
func (st *audioStream) decrypt(packet *rtp.Packet) ([]byte, error) {
	switch {
	case st.rtpDecoder != nil:
		// The decoder's key schedule is built once per stream. DecodeInto is
		// deliberately not used here: the jitter buffer keeps the returned
		// slice by reference until Pop, so a shared decode buffer would alias.
		return st.rtpDecoder.Decode(packet)
	case st.legacyDecoder != nil:
		return st.legacyDecoder.Decode(packet)
	default:
		// Unencrypted: rebuild the post-header bytes (Parse reserves the last
		// eight bytes as a nonce position).
		out := make([]byte, 0, len(packet.Payload)+len(packet.Nonce))
		out = append(out, packet.Payload...)
		out = append(out, packet.Nonce...)
		return out, nil
	}
}

// deliverControl folds one RTCP packet.
func (st *audioStream) deliverControl(raw []byte) {
	if len(raw) == 0 {
		return
	}
	if _, _, rtpTimestamp, ok := rtp.ParseSenderReport(raw); ok {
		st.mu.Lock()
		st.senderRTP = rtpTimestamp
		st.hasSenderSR = true
		st.mu.Unlock()
	}
}

func writeFramed(conn Stream, payload []byte) {
	if len(payload) == 0 || len(payload) > 65533 {
		return
	}
	frame := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(payload)+2))
	copy(frame[2:], payload)
	_, _ = conn.Write(frame)
}

// --- playout ---------------------------------------------------------------

func (st *audioStream) begin() {
	if st.buffered != nil {
		// A buffered stream is already accept-only; SETRATEANCHORTI, not a
		// RECORD, authorises playout. Do not publish PLAYING before PCM.
		return
	}
	st.mu.Lock()
	already := st.started
	st.started = true
	st.paused = false
	st.mu.Unlock()
	if already {
		return
	}
	st.s.startRing()
	if st.s.m.board != nil {
		st.s.m.board.SetPlayback(st.s.boardGen, media.StatePlaying)
	}
}

func (st *audioStream) pause() {
	if st.buffered != nil {
		st.buffered.pause()
		return
	}
	st.mu.Lock()
	st.paused = true
	sink := st.sink
	st.mu.Unlock()
	st.buffer.Flush()
	if sink != nil {
		_ = sink.Flush()
	}
}

func (st *audioStream) resume(rtpTime uint32) {
	if st.buffered != nil {
		// A metadata playback rate or legacy resume is not a buffered anchor.
		// Only SETRATEANCHORTI through bufferedAnchorPlay may authorise type-103
		// output; otherwise prefetched audio could start without timing.
		return
	}
	st.mu.Lock()
	st.paused = false
	ok := st.started
	st.started = true
	if rtpTime != 0 {
		// The sender names the RTP timestamp that is playing now, which is a
		// stronger anchor than the first packet's arrival: it maps the stream's
		// clock onto this receiver's clock directly.
		st.anchor = time.Now()
		st.anchorTS = rtpTime
		st.anchorFromSender = true
	} else {
		st.anchor = time.Time{}
		st.anchorFromSender = false
	}
	st.mu.Unlock()
	if !ok {
		st.begin()
	}
}

func (st *audioStream) flush() {
	if st.buffered != nil {
		// FLUSH has no buffered range: discard everything queued, require the
		// sender's next play anchor.
		st.buffered.flush(0, 0, true)
		return
	}
	st.buffer.Flush()
	st.mu.Lock()
	sink := st.sink
	st.mu.Unlock()
	if sink != nil {
		_ = sink.Flush()
	}
}

func (st *audioStream) stop() {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.closed = true
	sink := st.sink
	st.sink = nil
	buffered := st.buffered
	dataConn, controlConn := st.dataConn, st.controlConn
	st.mu.Unlock()
	if buffered != nil {
		buffered.stop()
	}
	if dataConn != nil {
		_ = dataConn.Close()
	}
	if controlConn != nil {
		_ = controlConn.Close()
	}
	if st.dataListener != nil {
		_ = st.dataListener.Close()
	}
	if st.udpData != nil {
		_ = st.udpData.Close()
	}
	if st.udpControl != nil {
		_ = st.udpControl.Close()
	}
	if sink != nil {
		// Discard queued audio: a stop is not a drain.
		_ = sink.Abort()
	}
	if st.decoder != nil {
		_ = st.decoder.Close()
	}
	if st.buffer != nil {
		st.buffer.Flush()
	}
}

func (st *audioStream) isClosed() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closed
}

func (st *audioStream) isPaused() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.paused
}

func (st *audioStream) noteError(err error) {
	st.mu.Lock()
	if st.failed == nil {
		st.failed = err
	}
	st.mu.Unlock()
}

// sinkFor returns the framework playback stream, opening it on first use.
func (st *audioStream) sinkFor() (Sink, error) {
	st.mu.Lock()
	if st.sink != nil {
		sink := st.sink
		st.mu.Unlock()
		return sink, nil
	}
	if st.closed {
		st.mu.Unlock()
		return nil, errors.New("stream closed")
	}
	st.mu.Unlock()
	if st.s.m.sinks == nil {
		return nil, errors.New("no playback sink available")
	}
	sink, err := st.s.m.sinks.OpenSink(st.sampleRate(), int(st.spec.config.Channels))
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		_ = sink.Abort()
		return nil, errors.New("stream closed")
	}
	st.sink = sink
	st.mu.Unlock()
	return sink, nil
}

func (st *audioStream) playLoop() {
	for {
		select {
		case <-st.s.done:
			return
		default:
		}
		if st.isClosed() {
			return
		}
		frame, timestamp, ok := st.buffer.Pop()
		if !ok {
			time.Sleep(3 * time.Millisecond)
			continue
		}
		if st.isPaused() || st.s.tvRate() == 0 {
			// A paused session discards what arrived while it was paused; the
			// sender restarts with a new anchor.
			continue
		}
		pcm, err := st.decode(frame)
		if err != nil || len(pcm) == 0 {
			if err != nil {
				st.s.m.logf("session %s: decode: %v", st.s.peer.Address, err)
			}
			continue
		}
		st.pace(timestamp)
		st.s.noteEnvelope(pcm)
		sink, err := st.sinkFor()
		if err != nil {
			// A stream closed by teardown is expected; do not report it as a
			// playback failure or log after the session has ended.
			if !st.isClosed() {
				st.s.m.logf("session %s: playback: %v", st.s.peer.Address, err)
			}
			st.noteError(err)
			return
		}
		if err := sink.Write(pcm); err != nil {
			if !st.isClosed() {
				st.s.m.logf("session %s: playback ended: %v", st.s.peer.Address, err)
			}
			st.noteError(err)
			return
		}
		st.mu.Lock()
		st.frames++
		st.lastPop = time.Now()
		st.mu.Unlock()
		if st.s.m.board != nil {
			st.s.m.board.SetPlayback(st.s.boardGen, media.StatePlaying)
		}
	}
}

// frameDecoder is one stream's audio codec.
type frameDecoder interface {
	Decode(frame []byte, dst []byte) ([]byte, error)
	Close() error
}

type alacFrameDecoder struct{ *alac.Decoder }

func (d alacFrameDecoder) Close() error { return nil }

type aacFrameDecoder struct{ *aac.Decoder }

func (d aacFrameDecoder) Close() error { return d.Decoder.Close() }

// decode turns one packet payload into interleaved S16LE PCM.
func (st *audioStream) decode(frame []byte) ([]byte, error) {
	if st.decoder == nil {
		// Raw PCM arrives as interleaved S16LE already.
		return frame, nil
	}
	out, err := st.decoder.Decode(frame, st.pcm[:0])
	if err != nil {
		return nil, err
	}
	st.pcm = out
	return out, nil
}

// pace holds the playout of a frame until its timestamp is due. Without a
// master clock (PTP) the first frame after an anchor defines the mapping from
// RTP timestamps to local time; the jitter target is the cushion.
func (st *audioStream) pace(timestamp uint32) {
	now := time.Now()
	st.mu.Lock()
	if st.anchor.IsZero() {
		st.anchor = now
		st.anchorTS = timestamp
		st.mu.Unlock()
		return
	}
	anchor, anchorTS, fromSender := st.anchor, st.anchorTS, st.anchorFromSender
	anchorFrame, anchorMaster, hasMaster := st.anchorFrame, st.anchorMasterNS, st.anchorMasterNS != 0
	st.mu.Unlock()

	// A 0xD7 anchor plus a disciplined PTP clock is the strongest mapping: the
	// sender told us which frame plays at which master-clock instant, and the
	// PTP offset converts that to local time (offset = master - local).
	if hasMaster && fromSender {
		if clock := st.s.m.ptpClock(); clock != nil {
			if offset, ok := clock.Offset(); ok {
				elapsed := time.Duration(float64(int64(timestamp-anchorFrame)) / float64(st.sampleRate()) * float64(time.Second))
				due := time.Unix(0, int64(anchorMaster)-offset).Add(elapsed)
				wait := time.Until(due)
				if wait > 0 && wait < 400*time.Millisecond {
					select {
					case <-st.s.done:
					case <-time.After(wait):
					}
				}
				return
			}
		}
	}

	elapsed := time.Duration(float64(int64(timestamp-anchorTS)) / float64(st.sampleRate()) * float64(time.Second))
	due := anchor.Add(elapsed)
	if !fromSender {
		// Without the sender's anchor, arrival time plus the cushion defines
		// playout; with it, the sender's own instant is the reference.
		due = due.Add(st.s.m.cfg.FrameTarget)
	}
	if now.After(due.Add(250 * time.Millisecond)) {
		// Too far behind: re-anchor instead of trying to catch up forever.
		st.mu.Lock()
		st.anchor = now
		st.anchorTS = timestamp
		st.mu.Unlock()
		return
	}
	if wait := time.Until(due); wait > 0 && wait < 400*time.Millisecond {
		select {
		case <-st.s.done:
		case <-time.After(wait):
		}
	}
}

// setAnchor records the sender's 0xD7 anchor: the RTP frame whose playout the
// sender timed, the master-clock (PTP) instant it applies to, and the stream's
// latency in frames. With a disciplined clock this is the authoritative
// playout mapping; without one the anchor is recorded and pacing stays on
// packet arrival.
func (st *audioStream) setAnchor(frameRef uint32, anchorFrame uint32, masterNS uint64, latencyFrames uint32) {
	if st.buffered != nil {
		// Type 103 is timed only by SETRATEANCHORTI with its network timeline.
		// A realtime 0xD7 anchor must not create a buffered play decision.
		return
	}
	st.mu.Lock()
	st.anchorFrameRef = frameRef
	st.anchorFrame = anchorFrame
	st.anchorMasterNS = masterNS
	st.latencyFrames = latencyFrames
	st.anchorFromSender = true
	st.anchor = time.Time{}
	st.mu.Unlock()
}

// setTimeAnnounce records the legacy 0xD4 announce's latency without a master
// clock; it is diagnostics-only for AirPlay 2.
func (st *audioStream) setTimeAnnounce(_ uint32, latencyFrames uint32) {
	st.mu.Lock()
	st.latencyFrames = latencyFrames
	st.mu.Unlock()
}

func (st *audioStream) framesPlayed() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.frames
}

// --- session-wide state ----------------------------------------------------

func (s *Session) tvRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rate
}

// noteEnvelope folds one decoded PCM block into the ring brightness envelope.
func (s *Session) noteEnvelope(pcm []byte) {
	s.mu.Lock()
	if s.envelopeState == nil {
		s.envelopeState = artwork.NewEnvelope()
	}
	envelope := s.envelopeState
	s.mu.Unlock()
	level := envelope.Update(pcm)
	s.mu.Lock()
	s.envelopeLevel = level
	s.mu.Unlock()
}

func (s *Session) markAudio() {
	s.audioSeen.Store(true)
}

// --- ring presentation -----------------------------------------------------

// startRing acquires the ring presentation for the duration of the session and
// keeps it up to date from the cover palette and the PCM envelope.
func (s *Session) startRing() {
	if !s.m.cfg.RingEnabled || s.m.rings == nil {
		return
	}
	select {
	case <-s.done:
		return
	default:
	}
	s.mu.Lock()
	if s.ring != nil || s.ringDone != nil {
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	s.ringDone = done
	s.mu.Unlock()
	ring, err := s.m.rings.OpenRing()
	if err != nil {
		s.m.logf("session %s: ring presentation unavailable: %v", s.peer.Address, err)
		s.mu.Lock()
		s.ringDone = nil
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	if s.ring != nil || s.ringDone != done {
		s.mu.Unlock()
		// Teardown released this acquisition while OpenRing was in flight.
		_ = ring.Close()
		return
	}
	s.ring = ring
	s.mu.Unlock()
	select {
	case <-s.done:
		// stopRing may already have run before the acquisition completed.
		s.mu.Lock()
		adopted := s.ring == ring
		if adopted {
			s.ring = nil
			s.ringDone = nil
		}
		s.mu.Unlock()
		if adopted {
			_ = ring.Close()
		}
		return
	default:
	}
	go s.ringLoop(done, ring)
}

func (s *Session) ringLoop(done chan struct{}, ring Ring) {
	ticker := time.NewTicker(60 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-done:
			return
		case <-ticker.C:
			if err := ring.Pixels(s.ringPixels()); err != nil {
				s.m.logf("session %s: ring frame: %v", s.peer.Address, err)
				return
			}
		}
	}
}

// ringPixels renders 12 RGB triples. With cover art the ring shows a gradient
// of the palette whose brightness follows the outgoing PCM envelope; without
// it, a dim neutral breath is shown, so the receiver is visibly alive and never
// invents artwork it does not have.
func (s *Session) ringPixels() []byte {
	s.mu.Lock()
	palette := append([]artwork.RGB(nil), s.palette...)
	brightness := s.envelopeLevel
	playing := s.rate > 0
	s.mu.Unlock()
	out := make([]byte, 36)
	if len(palette) == 0 {
		breath := 0.25 + 0.10*math.Sin(float64(time.Now().UnixMilli())/900)
		if !playing {
			breath *= 0.5
		}
		level := clamp01(breath)
		for i := range 12 {
			out[i*3+0] = uint8(255 * level)
			out[i*3+1] = uint8(255 * level)
			out[i*3+2] = uint8(255 * level)
		}
		return out
	}
	level := 0.12 + 0.88*clamp01(brightness)
	for i := range 12 {
		colour := samplePalette(palette, float64(i)/11)
		scaled := artwork.Scale(colour, level)
		out[i*3+0] = scaled.R
		out[i*3+1] = scaled.G
		out[i*3+2] = scaled.B
	}
	return out
}

func samplePalette(palette []artwork.RGB, position float64) artwork.RGB {
	if len(palette) == 1 {
		return palette[0]
	}
	scaled := position * float64(len(palette)-1)
	index := int(scaled)
	if index >= len(palette)-1 {
		return palette[len(palette)-1]
	}
	fraction := scaled - float64(index)
	return mix(palette[index], palette[index+1], fraction)
}

func mix(a, b artwork.RGB, t float64) artwork.RGB {
	return artwork.RGB{
		R: uint8(float64(a.R) + (float64(b.R)-float64(a.R))*t),
		G: uint8(float64(a.G) + (float64(b.G)-float64(a.G))*t),
		B: uint8(float64(a.B) + (float64(b.B)-float64(a.B))*t),
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func (s *Session) stopRing() {
	s.mu.Lock()
	done := s.ringDone
	ring := s.ring
	s.ringDone = nil
	s.ring = nil
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
	if ring != nil {
		if err := ring.Close(); err != nil {
			s.m.logf("session %s: releasing the ring: %v", s.peer.Address, err)
		}
	}
}

// --- event channel ---------------------------------------------------------

// acceptEventConnections accepts the sender's event channel connection. The
// channel is encrypted with the Events-* keys derived from the pair-verify
// shared secret, and it is the only path a receiver uses to send transport
// commands (play/pause/next/previous) back to the sender.
func (s *Session) acceptEventConnections(listener Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		s.eventMu.Lock()
		// Accept can return a peer after cleanup closed the listener. Never
		// publish that peer: teardown owns the final state and a late event
		// channel must not resurrect an update or reader.
		if s.eventListener != listener {
			s.eventMu.Unlock()
			_ = conn.Close()
			return
		}
		select {
		case <-s.done:
			s.eventMu.Unlock()
			_ = conn.Close()
			return
		default:
		}
		var rw io.ReadWriter = conn
		keys := s.evKeys
		var encrypted io.ReadWriter
		var err2 error
		if keys != nil {
			encrypted, err2 = hapcrypt.NewSession(conn, keys.write, keys.read)
		}
		if !s.encrypted {
			// Event channels exist only after pairing; without channel keys the
			// connection cannot carry authenticated traffic.
			s.eventMu.Unlock()
			_ = conn.Close()
			continue
		}
		if err2 != nil {
			s.eventMu.Unlock()
			_ = conn.Close()
			s.m.logf("session %s: event channel encryption: %v", s.peer.Address, err2)
			continue
		}
		rw = encrypted
		s.eventMu.Unlock()
		reader := bufio.NewReaderSize(rw, 8*1024)
		previous, published := s.replaceEventChannel(listener, conn, rw, reader)
		if !published {
			_ = conn.Close()
			return
		}
		if previous != nil {
			_ = previous.Close()
		}
		s.m.logf("session %s: event channel established", s.peer.Address)
		generation, published := s.publishEventChannel(rw, conn)
		if !published {
			continue
		}
		// The loop owns this connection's reader and the writer/generation it
		// was published with; it never resolves the session's current fields.
		go s.eventReadLoop(reader, rw, generation)
	}
}

// replaceEventChannel publishes a newly accepted reader/writer and returns the
// previous peer. eventApplyMu makes the swap atomic relative to a request body
// being applied: the body either completes before this assignment or is
// dropped after it. No socket I/O or stream operation runs under the lock.
func (s *Session) replaceEventChannel(listener Listener, conn Stream, rw io.Writer, reader *bufio.Reader) (Stream, bool) {
	s.eventApplyMu.Lock()
	defer s.eventApplyMu.Unlock()
	s.eventMu.Lock()
	if s.eventListener != listener {
		s.eventMu.Unlock()
		return nil, false
	}
	select {
	case <-s.done:
		s.eventMu.Unlock()
		return nil, false
	default:
	}
	previous := s.eventConn
	s.eventConn = conn
	s.eventOut = rw
	s.eventIn = reader
	s.eventReady = false
	s.eventMu.Unlock()
	return previous, true
}

// publishEventChannel writes updateInfo and only then makes the channel ready.
// A failed write may have advanced the HAP outbound nonce or left a partial
// block; that exact peer is retired and nothing is published, so the accept
// loop must not start its reader.
func (s *Session) publishEventChannel(rw io.Writer, conn Stream) (uint64, bool) {
	unit, ok := s.sendUpdateInfo()
	if !ok {
		s.retireEventWriter(rw, conn)
		return 0, false
	}
	// Seed before the channel becomes ready, so a ticker pass that observes it
	// cannot post a duplicate dvlc in the window between publish and seeding.
	s.noteSeedMaster(unit)
	s.eventMu.Lock()
	if s.eventOut != rw {
		// A replacement won the publish; leave it to its own updateInfo.
		s.eventMu.Unlock()
		return 0, false
	}
	s.eventReady = true
	s.eventGen++
	generation := s.eventGen
	s.eventMu.Unlock()
	// updateInfo has just told this channel the current master and seeded its
	// delivered state, so the pass this kick wakes has nothing to mirror; the
	// kick only establishes the channel generation for later local changes.
	select {
	case s.volumeKick <- struct{}{}:
	default:
	}
	return generation, true
}

func (s *Session) eventReadLoop(reader *bufio.Reader, out io.Writer, generation uint64) {
	if reader == nil {
		return
	}
	for {
		select {
		case <-s.done:
			return
		default:
		}
		prefix, err := reader.Peek(5)
		if err != nil {
			return
		}
		if string(prefix) == "RTSP/" {
			// A response to a command we sent; its CSeq is not needed.
			if err := skipResponse(reader); err != nil {
				return
			}
			continue
		}
		request, err := rtsp.ReadRequest(reader)
		if err != nil {
			return
		}
		// A request already read from a retired channel must not mutate the
		// replacement session (metadata, rate, group) either.
		if len(request.Body) > 0 {
			if !s.applyEventState(out, generation, func() {
				s.noteSenderPlist(request.Body)
			}) {
				return
			}
		}
		response := rtsp.NewResponse(request.CSeq(), 200, "")
		response.Set("Server", "AirTunes/"+s.m.cfg.SourceVersion)
		s.writeEventTo(out, generation, response)
	}
}

func skipResponse(reader *bufio.Reader) error {
	headers := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		headers++
		if headers > 64 {
			return errors.New("event response with too many headers")
		}
	}
	return nil
}

// eventChannelCurrent reports whether out is still the published writer at
// generation. It binds event requests and replies to the channel they were
// accepted on, not to whatever happened to replace it.
func (s *Session) eventChannelCurrent(out io.Writer, generation uint64) bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.eventOut == out && s.eventReady && s.eventGen == generation
}

// applyEventState runs apply only while out is still the published writer at
// generation. Replacement publication takes eventApplyMu, so a body either
// applies completely before the replacement or is dropped as stale. The lock
// covers only validation and the state mutation; no socket I/O or event write
// is performed here.
func (s *Session) applyEventState(out io.Writer, generation uint64, apply func()) bool {
	s.eventApplyMu.Lock()
	defer s.eventApplyMu.Unlock()
	select {
	case <-s.done:
		return false
	default:
	}
	if !s.eventChannelCurrent(out, generation) {
		return false
	}
	apply()
	return true
}

// writeEventTo writes the reply only if out is still the published writer at
// generation. A delayed reply from a retired reader is dropped instead of
// landing on the replacement channel.
func (s *Session) writeEventTo(out io.Writer, generation uint64, response *rtsp.Response) {
	s.eventWriteMu.Lock()
	s.eventMu.Lock()
	current, ready, currentGeneration := s.eventOut, s.eventReady, s.eventGen
	s.eventMu.Unlock()
	if out == nil || !ready || current != out || currentGeneration != generation {
		s.eventWriteMu.Unlock()
		return
	}
	if err := response.Write(out); err != nil {
		// HAP seals before the underlying write; a failed response leaves the
		// nonce advanced, so this peer must not be written again. Unpublish it
		// while the write lock still excludes every other event write.
		conn := s.unpublishEventWriter(out)
		s.eventWriteMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		return
	}
	s.eventWriteMu.Unlock()
}

// sendRemoteCommand posts one modern-media remote command to the sender. The
// command numbers are the sender's own (play 0, pause 1, next 4, previous 5).
func (s *Session) sendRemoteCommand(action string) (bool, string) {
	number, ok := map[string]string{"play": "0", "pause": "1", "next": "4", "previous": "5"}[action]
	if !ok {
		return false, "unsupported action"
	}
	commandID := make([]byte, 16)
	if _, err := rand.Read(commandID); err != nil {
		return false, "could not generate a command id"
	}
	body, err := plist.EncodeBinary(map[string]any{
		"type":                     "sendMediaRemoteCommand",
		"modernMediaRemoteCommand": number,
		"params": map[string]any{
			"kMRMediaRemoteOptionSendOptionsNumber":    int64(0),
			"kMRMediaRemoteOptionCommandID":            hex.EncodeToString(commandID),
			"kMRMediaRemoteOptionIsRedirectingCommand": true,
		},
	})
	if err != nil {
		return false, "could not encode the command"
	}
	written, err := s.writeEventRequest(eventCommandRequest(body), true)
	if err != nil {
		return false, "the event channel write failed: " + err.Error()
	}
	if !written {
		return false, "the sender has no event channel"
	}
	return true, ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func valueOrDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// sendUpdateInfo posts the reference "updateInfo" message the moment the event
// channel is established, so the sender sees the current name, feature set and
// TXT records without polling /info. It returns the master unit carried in the
// document so the caller can seed only this channel's delivered/display state;
// a failed write returns ok=false and seeds nothing.
func (s *Session) sendUpdateInfo() (float64, bool) {
	txt := airplayinfo.TXT(s.m.advertiseTXT())
	cfg := s.infoConfig()
	document, err := airplayinfo.UpdateInfo(cfg, txt)
	if err != nil {
		s.m.logf("session %s: updateInfo: %v", s.peer.Address, err)
		return 0, false
	}
	if _, ok := document["type"]; !ok {
		document = map[string]any{"type": "updateInfo", "value": document}
	}
	body, err := plist.EncodeBinary(document)
	if err != nil {
		s.m.logf("session %s: updateInfo encoding: %v", s.peer.Address, err)
		return 0, false
	}
	if !s.postEventRequest(body) {
		s.m.logf("session %s: updateInfo could not be posted", s.peer.Address)
		return 0, false
	}
	unit, valid := unitVolumeFromDB(cfg.VolumeDB)
	if !valid {
		// cfg.VolumeDB is always the safe -144 fallback when no level is
		// readable, which maps to 0; this is unreachable for finite values.
		unit = 0
	}
	return unit, true
}

// postEventRequest writes one RTSP POST /command request with a plist body on
// the sender's event channel and returns whether it was written. It is used for
// updateInfo (before the channel is ready) and the legacy volume helper; the
// watcher binds dvlc to a captured writer with writeEventRequestTo instead.
func (s *Session) postEventRequest(body []byte) bool {
	written, _ := s.writeEventRequest(eventCommandRequest(body), false)
	return written
}

// eventCommandRequest frames one plist body as the reference's POST /command.
func eventCommandRequest(body []byte) []byte {
	line := "POST /command RTSP/1.0\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\nCSeq: 1\r\n\r\n"
	return append([]byte(line), body...)
}

// writeEventRequest writes one raw request to the current event writer. The
// writer is captured under eventMu while eventWriteMu excludes other writers,
// so a failed write can retire exactly the peer that failed. requireReady is
// false only for the updateInfo that makes a new channel ready.
func (s *Session) writeEventRequest(raw []byte, requireReady bool) (bool, error) {
	select {
	case <-s.done:
		return false, nil
	default:
	}
	s.eventWriteMu.Lock()
	s.eventMu.Lock()
	out, ready := s.eventOut, s.eventReady
	s.eventMu.Unlock()
	if out == nil || (requireReady && !ready) {
		s.eventWriteMu.Unlock()
		return false, nil
	}
	if _, err := out.Write(raw); err != nil {
		conn := s.unpublishEventWriter(out)
		s.eventWriteMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		return false, err
	}
	s.eventWriteMu.Unlock()
	return true, nil
}

// writeEventRequestTo writes raw only to the event writer that was validated
// with this generation. A replacement channel is never used by an old
// notification, even if it is published between validation and the write.
func (s *Session) writeEventRequestTo(out io.Writer, generation uint64, raw []byte) (bool, error) {
	select {
	case <-s.done:
		return false, nil
	default:
	}
	s.eventWriteMu.Lock()
	s.eventMu.Lock()
	current, ready, currentGeneration := s.eventOut, s.eventReady, s.eventGen
	s.eventMu.Unlock()
	if !ready || current != out || currentGeneration != generation {
		s.eventWriteMu.Unlock()
		return false, nil
	}
	if _, err := out.Write(raw); err != nil {
		conn := s.unpublishEventWriter(out)
		s.eventWriteMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		return false, err
	}
	s.eventWriteMu.Unlock()
	return true, nil
}

// unpublishEventWriter atomically removes out from the published event state
// and returns its peer for closing, but only if it is still the current
// channel. A replacement keeps its own state. Callers that hold eventWriteMu
// keep every other event writer behind the lock until retirement is visible.
func (s *Session) unpublishEventWriter(out io.Writer) Stream {
	s.eventMu.Lock()
	if s.eventOut != out {
		s.eventMu.Unlock()
		return nil
	}
	conn := s.eventConn
	s.eventOut = nil
	s.eventIn = nil
	s.eventConn = nil
	s.eventReady = false
	s.eventGen++
	s.eventMu.Unlock()
	return conn
}

// retireEventWriter unpublishes and closes the event channel that owns out,
// but only if it is still the published channel. A replacement keeps running.
func (s *Session) retireEventWriter(out io.Writer, conn Stream) {
	c := s.unpublishEventWriter(out)
	if c == nil {
		c = conn
	}
	if c != nil {
		_ = c.Close()
	}
}

// volumeWatch mirrors receiver-side master changes to the sender as the
// reference's "dvlc" media-remote volume notification: one authoritative value
// per local change, never artificial intermediate steps. It never echoes a
// value the sender itself just set: a completed SET records the applied unit in
// this session's delivered state, so this pass sees no difference. A new event
// channel is seeded by its updateInfo (which reports the same master).
func (s *Session) volumeWatch() {
	if s.m.cfg.MasterVolumeContext == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-s.volumeKick:
		case <-ticker.C:
		}
		if !s.pollMasterVolume() {
			return
		}
	}
}

// pollMasterVolume runs one notification pass for volumeWatch. A missing
// channel does nothing. When the channel is ready, one fresh bounded master
// reading is compared with what this event channel has actually been told
// (seeded by updateInfo or a previous dvlc). A difference is a receiver-side
// change and is mirrored as one authoritative dvlc at the current master; no
// intermediate values are invented. A successful write records the delivered
// value and a one-shot echo token. It reports false when the watcher must stop.
func (s *Session) pollMasterVolume() bool {
	out, generation, ready := s.readyEventWriter()
	if !ready {
		return true
	}
	// A current armed token owns the channel until an echo consumes it or its
	// bounded delivery window expires. Deferring here lets the next regular
	// pass repair presentation without overwriting that token.
	if s.mirrorActive(out, generation) {
		return true
	}
	// Capture before the read: an owner SET or replacement seed accepted while
	// the read is parked makes this pass stale even if the read itself returns
	// an old sample.
	epoch := s.senderDeliveryEpoch()
	target, ok := s.m.readMasterVolume(s.masterVolumeContext())
	if !ok {
		return true
	}
	delivered, known := s.deliveredMaster()
	if known && unitsEqual(delivered, target) {
		return true
	}
	// The write helper revalidates this epoch under the event-writer order and
	// stages the echo token before any bytes can be observed.
	if !s.sendUnitVolumeIfCurrent(out, generation, target, epoch) {
		// A replaced channel has its own updateInfo seed; a failed peer is
		// retired. Never retry this notification on the same channel.
		return true
	}
	return true
}

// readyEventWriter captures the published, ready event writer and its
// generation. A notification must pass both to writeEventRequestTo; looking up
// the writer again at write time could route an old notification into a
// replacement channel before its updateInfo.
func (s *Session) readyEventWriter() (io.Writer, uint64, bool) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.eventOut == nil || !s.eventReady {
		return nil, s.eventGen, false
	}
	return s.eventOut, s.eventGen, true
}

// encodeUnitVolume builds the reference dvlc payload:
// {"type":"sendMediaRemoteCommand","value":"dvlc","volume":V,"params":{"volume":V}}.
func encodeUnitVolume(unit float64) ([]byte, error) {
	return plist.EncodeBinary(map[string]any{
		"type":   "sendMediaRemoteCommand",
		"value":  "dvlc",
		"volume": unit,
		"params": map[string]any{"volume": unit},
	})
}

// sendUnitVolume posts the reference volume notification on the current event
// channel. It is the unbounded test helper; the watcher uses sendUnitVolumeTo
// so every step is bound to one validated generation.
func (s *Session) sendUnitVolume(unit float64) bool {
	body, err := encodeUnitVolume(unit)
	if err != nil {
		return false
	}
	return s.postEventRequest(body)
}

// sendUnitVolumeTo posts one dvlc only to the writer captured for generation.
// It is the test helper; the watcher uses sendUnitVolumeIfCurrent, which also
// revalidates the sender epoch.
func (s *Session) sendUnitVolumeTo(out io.Writer, generation uint64, unit float64) bool {
	return s.sendUnitVolumeIfCurrent(out, generation, unit, s.senderDeliveryEpoch())
}

// sendUnitVolumeIfCurrent validates the exact writer and generation under the
// event-writer lock, revalidates the sender epoch that the pass captured before
// its read, and stages the generation-scoped echo token before writing any
// bytes. A SET arriving on the independent control socket during the write
// therefore sees the token and cannot be overwritten by the older value.
// Display state is recorded only if this generation is still current and no
// newer genuine SET/seed superseded the reservation. A failed write cancels
// only its own token.
func (s *Session) sendUnitVolumeIfCurrent(out io.Writer, generation uint64, unit float64, expectedEpoch uint64) bool {
	body, err := encodeUnitVolume(unit)
	if err != nil {
		return false
	}
	raw := eventCommandRequest(body)
	s.eventWriteMu.Lock()
	s.eventMu.Lock()
	current, ready, currentGen := s.eventOut, s.eventReady, s.eventGen
	s.eventMu.Unlock()
	if !ready || current != out || currentGen != generation {
		s.eventWriteMu.Unlock()
		return false
	}
	tokenID, displaySeq, ok := s.stageMirrorIfEpoch(expectedEpoch, unit, generation, out)
	if !ok {
		s.eventWriteMu.Unlock()
		return false
	}
	if _, err := out.Write(raw); err != nil {
		s.cancelMirror(tokenID)
		conn := s.unpublishEventWriter(out)
		s.eventWriteMu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		return false
	}
	s.completeMirror(tokenID, displaySeq, unit, out, generation)
	s.eventWriteMu.Unlock()
	return true
}
