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

	mu               sync.Mutex
	controlConn      Stream
	dataConn         Stream
	sink             Sink
	started          bool
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
		s:      s,
		spec:   spec,
		id:     nextStreamID,
		buffer: rtp.NewBuffer(spec.config.SampleRate, s.m.cfg.FrameTarget),
	}
	nextStreamID++
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
				return nil, fmt.Errorf("realtime decoder: %w", err)
			}
			stream.rtpDecoder = decoder
		} else if len(spec.aesKey) == 16 {
			decoder, err := rtp.NewLegacyDecoder(spec.aesKey, spec.aesIV)
			if err != nil {
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
			go stream.serveLegacyControl(control)
		}
		go stream.serveUDP(data)
		go stream.resendWatch()
	case spec.streamType == streamTypeBuffered:
		// Buffered audio (type 103) is a TCP stream whose packets carry a
		// 2-byte big-endian length that includes its own two bytes.
		data, err := s.m.transport.OpenTCPListener("0.0.0.0", 0)
		if err != nil {
			return nil, fmt.Errorf("open the buffered audio channel listener: %w", err)
		}
		stream.dataListener = data
		go stream.serveDataConnections(data)
	}
	s.mu.Lock()
	s.streams = append(s.streams, stream)
	s.mu.Unlock()
	go stream.playLoop()
	s.m.logf("session from %s: stream %d type %d codec %s %d Hz %d ch", s.peer.Address, stream.id, spec.streamType, spec.codec, spec.config.SampleRate, spec.config.Channels)
	return stream, nil
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
	return map[string]any{
		"type":            int64(st.spec.streamType),
		"controlPort":     int64(controlPort),
		"dataPort":        int64(dataPort),
		"audioBufferSize": int64(1 << 20),
		"streamID":        int64(st.id),
	}
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

func (st *audioStream) serveDataConnections(listener Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			st.noteError(fmt.Errorf("audio channel: %w", err))
			return
		}
		st.mu.Lock()
		previous := st.dataConn
		st.dataConn = conn
		st.mu.Unlock()
		if previous != nil {
			_ = previous.Close()
		}
		st.s.m.logf("session %s: audio channel from %s", st.s.peer.Address, conn.Peer().Address)
		go st.readData(conn)
	}
}

func (st *audioStream) readData(conn Stream) {
	for {
		packet, err := readFramed(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !st.isClosed() {
				st.s.m.logf("session %s: audio channel ended: %v", st.s.peer.Address, err)
			}
			return
		}
		st.deliver(packet)
	}
}

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

// readFramed reads one AirPlay TCP audio/control packet: a big-endian uint16
// that counts the whole frame including its own two bytes, then the payload.
func readFramed(r io.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	total := int(binary.BigEndian.Uint16(header[:]))
	if total < 2 || total > 65535 {
		return nil, fmt.Errorf("malformed audio frame length %d", total)
	}
	payload := make([]byte, total-2)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// deliver decrypts and queues one RTP packet.
func (st *audioStream) deliver(raw []byte) {
	if len(raw) == 0 || st.isClosed() {
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
	dataConn, controlConn := st.dataConn, st.controlConn
	st.mu.Unlock()
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
	st.buffer.Flush()
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
		media.ScalePCM16(pcm, st.s.gain())
		st.s.noteEnvelope(pcm)
		sink, err := st.sinkFor()
		if err != nil {
			st.s.m.logf("session %s: playback: %v", st.s.peer.Address, err)
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
	s.ring = ring
	s.mu.Unlock()
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
		previous := s.eventConn
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
		s.eventConn = conn
		s.eventOut = rw
		s.eventIn = bufio.NewReaderSize(rw, 8*1024)
		s.eventMu.Unlock()
		if previous != nil {
			_ = previous.Close()
		}
		s.m.logf("session %s: event channel established", s.peer.Address)
		s.sendUpdateInfo()
		go s.eventReadLoop(conn)
	}
}

func (s *Session) eventReadLoop(conn Stream) {
	s.eventMu.Lock()
	reader := s.eventIn
	s.eventMu.Unlock()
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
		if len(request.Body) > 0 {
			s.noteSenderPlist(request.Body)
		}
		response := rtsp.NewResponse(request.CSeq(), 200, "")
		response.Set("Server", "AirTunes/"+s.m.cfg.SourceVersion)
		s.writeEvent(response)
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

func (s *Session) writeEvent(response *rtsp.Response) {
	s.eventMu.Lock()
	out := s.eventOut
	s.eventMu.Unlock()
	if out == nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = response.Write(out)
}

// sendRemoteCommand posts one modern-media remote command to the sender. The
// command numbers are the sender's own (play 0, pause 1, next 4, previous 5).
func (s *Session) sendRemoteCommand(action string) (bool, string) {
	number, ok := map[string]string{"play": "0", "pause": "1", "next": "4", "previous": "5"}[action]
	if !ok {
		return false, "unsupported action"
	}
	s.eventMu.Lock()
	out := s.eventOut
	s.eventMu.Unlock()
	if out == nil {
		return false, "the sender has no event channel"
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
	line := fmt.Sprintf("POST /command RTSP/1.0\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: %d\r\nCSeq: 1\r\n\r\n", len(body))
	s.writeMu.Lock()
	_, err = out.Write(append([]byte(line), body...))
	s.writeMu.Unlock()
	if err != nil {
		return false, "the event channel write failed: " + err.Error()
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
// TXT records without polling /info.
func (s *Session) sendUpdateInfo() {
	txt := airplayinfo.TXT(s.m.advertiseTXT(true))
	document, err := airplayinfo.UpdateInfo(s.infoConfig(), txt)
	if err != nil {
		s.m.logf("session %s: updateInfo: %v", s.peer.Address, err)
		return
	}
	if _, ok := document["type"]; !ok {
		document = map[string]any{"type": "updateInfo", "value": document}
	}
	body, err := plist.EncodeBinary(document)
	if err != nil {
		s.m.logf("session %s: updateInfo encoding: %v", s.peer.Address, err)
		return
	}
	if !s.postEventRequest(body) {
		s.m.logf("session %s: updateInfo could not be posted (no event channel)", s.peer.Address)
	}
}

// postEventRequest writes one RTSP POST /command request with a plist body on
// the sender's event channel and returns whether it was written.
func (s *Session) postEventRequest(body []byte) bool {
	s.eventMu.Lock()
	out := s.eventOut
	s.eventMu.Unlock()
	if out == nil {
		return false
	}
	line := "POST /command RTSP/1.0\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\nCSeq: 1\r\n\r\n"
	s.writeMu.Lock()
	_, err := out.Write(append([]byte(line), body...))
	s.writeMu.Unlock()
	return err == nil
}

// volumeWatch forwards the device's master level to the sender as the
// reference's "dvlc" media-remote volume notification, ramped so a large jump
// does not disturb the sender's own slider. It only reads the mixer: a
// supervised app cannot write the master, so the device level stays the
// ceiling.
func (s *Session) volumeWatch() {
	read := s.m.cfg.MasterVolume
	if read == nil {
		return
	}
	last := -1.0
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		level, ok := read()
		if !ok || level < 0 || level > 1 {
			continue
		}
		if last >= 0 && math.Abs(level-last) < 0.01 {
			continue
		}
		last = level
		if !s.hasEventChannel() {
			continue
		}
		// Ramp in the reference's 0.09 steps with a short pause, ending exactly
		// on the target level. The step count is bounded (0..1 in 0.09 steps is
		// 12) so a sender that stops reading cannot pin this goroutine, and
		// every step re-checks the channel and cancellation.
		present := last
		for steps := 0; steps <= 24; steps++ {
			step := present
			if math.Abs(level-present) < 0.1 {
				step = level
			} else if level > present {
				step = present + 0.09
			} else {
				step = present - 0.09
			}
			if step < 0 {
				step = 0
			}
			if step > 1 {
				step = 1
			}
			select {
			case <-s.done:
				return
			default:
			}
			if !s.sendUnitVolume(step) {
				return
			}
			present = step
			if step == level {
				break
			}
			select {
			case <-s.done:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

func (s *Session) hasEventChannel() bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.eventOut != nil
}

// sendUnitVolume posts the reference volume notification:
// {"type":"sendMediaRemoteCommand","value":"dvlc","volume":V,"params":{"volume":V}}.
func (s *Session) sendUnitVolume(unit float64) bool {
	body, err := plist.EncodeBinary(map[string]any{
		"type":   "sendMediaRemoteCommand",
		"value":  "dvlc",
		"volume": unit,
		"params": map[string]any{"volume": unit},
	})
	if err != nil {
		return false
	}
	return s.postEventRequest(body)
}
