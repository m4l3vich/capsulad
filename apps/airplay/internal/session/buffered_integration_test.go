package session

// Type-103 integration: an authenticated control channel stages a buffered
// stream through SETUP, audio is held until a valid anchor, then real
// ALAC/AAC frames reach the framework sink. The anchor operations use both
// observed wire spellings: the reference's SETRATEANCHORTI and the iPhone field
// client's SETRATEANCHORTIME. Pause, immediate flush, invalid setup and
// teardown are exercised over daemon-style loopback listeners.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"capsulad/apps/airplay/internal/aac"
	"capsulad/apps/airplay/internal/alac"
	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/plist"
	"capsulad/apps/airplay/internal/rtsp"
)

func readBufferedAACFrames(t *testing.T) [][]byte {
	t.Helper()
	data, err := os.ReadFile("../aac/testdata/aac-lc-44100-stereo.adts")
	if err != nil {
		t.Fatalf("AAC fixture: %v", err)
	}
	var frames [][]byte
	for offset := 0; offset+7 <= len(data); {
		if data[offset] != 0xFF || data[offset+1]&0xF0 != 0xF0 {
			t.Fatalf("AAC fixture has a bad ADTS sync at %d", offset)
		}
		headerLen := 7
		if data[offset+1]&0x01 == 0 {
			headerLen = 9
		}
		length := int(data[offset+3]&0x03)<<11 | int(data[offset+4])<<3 | int(data[offset+5]>>5)
		if length < headerLen || offset+length > len(data) {
			t.Fatalf("AAC fixture frame at %d overruns the file", offset)
		}
		frames = append(frames, data[offset+headerLen:offset+length])
		offset += length
	}
	if len(frames) < 3 {
		t.Fatalf("AAC fixture has %d frames, need at least 3", len(frames))
	}
	return frames
}

func bufferedSetupBody(t *testing.T, format uint64, shk []byte) []byte {
	t.Helper()
	body, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeBuffered),
			"audioFormat": int64(format),
			"spf":         int64(4096),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode type-103 SETUP: %v", err)
	}
	return body
}

func sendBufferedPacket(conn Stream, key []byte, seq uint32, timestamp, ssrc uint32, payload []byte) error {
	header := make([]byte, 12)
	// Buffered packets use the marker bit plus a 23-bit sequence number as the
	// first big-endian word, unlike realtime RTP's 16-bit sequence field.
	binary.BigEndian.PutUint32(header[0:4], 0x80000000|(seq&0x7fffff))
	binary.BigEndian.PutUint32(header[4:8], timestamp)
	binary.BigEndian.PutUint32(header[8:12], ssrc)
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], uint64(seq)+1)
	ciphertext := aead.Seal(nil, nonce, payload, header[4:12])
	packet := append(append(append([]byte{}, header...), ciphertext...), nonce[4:]...)
	frame := make([]byte, 2+len(packet))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(frame)))
	copy(frame[2:], packet)
	if deadlineWriter, ok := conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := deadlineWriter.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
	}
	for len(frame) > 0 {
		written, err := conn.Write(frame)
		if err != nil {
			return err
		}
		if written <= 0 {
			return fmt.Errorf("buffered packet write returned no bytes")
		}
		frame = frame[written:]
	}
	return nil
}

func waitForBuffered(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func dialTCPStream(t *testing.T, port int) *realStream {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial loopback port %d: %v", port, err)
	}
	return &realStream{conn: conn, peer: Peer{Address: "127.0.0.1", Port: 50000}}
}

func (s *realStream) SetWriteDeadline(deadline time.Time) error {
	return s.conn.SetWriteDeadline(deadline)
}

type recordingSink struct {
	mu      sync.Mutex
	writes  [][]byte
	flushes int
	closed  bool
	aborted bool
}

func (s *recordingSink) Write(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.aborted {
		return fmt.Errorf("recording sink is closed")
	}
	s.writes = append(s.writes, append([]byte(nil), pcm...))
	return nil
}

func (s *recordingSink) Flush() error {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) Abort() error {
	s.mu.Lock()
	s.aborted = true
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) snapshot() (writes [][]byte, aborted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, write := range s.writes {
		writes = append(writes, append([]byte(nil), write...))
	}
	return writes, s.aborted
}

type recordingSinks struct{ sink *recordingSink }

func (r recordingSinks) OpenSink(int, int) (Sink, error) { return r.sink, nil }

func expectedBufferedPCM(t *testing.T, frame []byte) []byte {
	t.Helper()
	decoder, err := alac.NewDecoder(alac.Config{
		FrameLength: 352, SampleRate: 48000, Channels: 2, BitDepth: 24,
	})
	if err != nil {
		t.Fatalf("expected PCM decoder: %v", err)
	}
	pcm, err := decoder.Decode(frame, nil)
	if err != nil {
		t.Fatalf("expected PCM decode: %v", err)
	}
	return append([]byte(nil), pcm...)
}

type reopeningSinks struct {
	mu  sync.Mutex
	all []*fakeSink
}

func (r *reopeningSinks) OpenSink(int, int) (Sink, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sink := &fakeSink{}
	r.all = append(r.all, sink)
	return sink, nil
}

func (r *reopeningSinks) state() (pcm int, aborted bool) {
	r.mu.Lock()
	all := append([]*fakeSink(nil), r.all...)
	r.mu.Unlock()
	for _, sink := range all {
		bytes, closed := sink.state()
		pcm += bytes
		aborted = aborted || closed
	}
	return pcm, aborted
}

func (r *reopeningSinks) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.all)
}

func (r *reopeningSinks) allAborted() bool {
	r.mu.Lock()
	all := append([]*fakeSink(nil), r.all...)
	r.mu.Unlock()
	if len(all) == 0 {
		return false
	}
	for _, sink := range all {
		if _, aborted := sink.state(); !aborted {
			return false
		}
	}
	return true
}

func (r *reopeningSinks) nonZeroPCM() bool {
	r.mu.Lock()
	all := append([]*fakeSink(nil), r.all...)
	r.mu.Unlock()
	for _, sink := range all {
		sink.mu.Lock()
		for _, b := range sink.pcm {
			if b != 0 {
				sink.mu.Unlock()
				return true
			}
		}
		sink.mu.Unlock()
	}
	return false
}

func (r *reopeningSinks) lastPCM() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.all) == 0 {
		return nil
	}
	sink := r.all[len(r.all)-1]
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]byte(nil), sink.pcm...)
}

func bufferedHasSequence(stream *audioStream, sequence uint32) bool {
	if stream == nil || stream.buffered == nil {
		return false
	}
	stream.buffered.mu.Lock()
	defer stream.buffered.mu.Unlock()
	for i := stream.buffered.head; i < len(stream.buffered.packets); i++ {
		if stream.buffered.packets[i].sequence == sequence {
			return true
		}
	}
	return false
}

func pcmHasSignal(pcm []byte) bool {
	for _, b := range pcm {
		if b != 0 {
			return true
		}
	}
	return false
}

func runBufferedType103Flow(t *testing.T, format uint64, spf int, ssrc uint32, frames [][]byte) {
	t.Helper()
	transport := &realTransport{}
	t.Cleanup(transport.Close)
	sinks := &reopeningSinks{}
	board := media.NewBoard()
	master := 0.05
	var masterMu sync.Mutex
	var masterWrites []float64
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Buffered Loopback", Port: 0,
		FrameTarget: 20 * time.Millisecond, RingEnabled: false, Logf: t.Logf,
		MasterVolumeContext: func(context.Context) (float64, bool) {
			masterMu.Lock()
			defer masterMu.Unlock()
			return master, true
		},
		SetMasterVolume: func(_ context.Context, level float64) (float64, error) {
			masterMu.Lock()
			master = level
			masterWrites = append(masterWrites, level)
			masterMu.Unlock()
			return level, nil
		},
	}, transport, &fakeAdvertiser{}, fakeRings{ring: &fakeRing{}}, sinks,
		board, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	port := mgr.advertisedPort()
	if port == 0 {
		t.Fatal("manager has no RTSP port")
	}

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("controller key: %v", err)
	}
	const controllerID = "buffered-test-controller"
	mgr.Store().SetControllerLTPK(controllerID, public)
	controlConn := dialTCPStream(t, port)
	defer controlConn.Close()
	client := newRTSPClient(controlConn)
	shared := pairVerify(t, client, controllerID, private)
	keys := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(controlConn, keys.Inbound, keys.Outbound)
	if err != nil {
		t.Fatalf("encrypted control channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})

	shk := make([]byte, 32)
	if _, err := rand.Read(shk); err != nil {
		t.Fatalf("shk: %v", err)
	}
	body, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeBuffered),
			"audioFormat": int64(format),
			"spf":         int64(spf),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err != nil || setup.status != 200 {
		t.Fatalf("type-103 SETUP = %v %v", setup, err)
	}
	decoded, err := plist.Decode(setup.body)
	if err != nil {
		t.Fatalf("decode SETUP: %v", err)
	}
	setupDict, _ := decoded.(map[string]any)
	streams, _ := setupDict["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("SETUP streams = %v", setupDict["streams"])
	}
	streamDict, _ := streams[0].(map[string]any)
	dataPort := int(plistUint(streamDict["dataPort"]))
	if dataPort == 0 || int(plistUint(streamDict["controlPort"])) == 0 {
		t.Fatalf("SETUP descriptor has no ports: %v", streamDict)
	}
	if got := plistUint(streamDict["audioBufferSize"]); got != 8<<20 {
		t.Fatalf("SETUP audioBufferSize = %d, want the real 8 MiB queue", got)
	}
	if _, present := streamDict["streamID"]; present {
		t.Fatalf("type-103 SETUP descriptor carries streamID: %v", streamDict)
	}

	dataConn := dialTCPStream(t, dataPort)
	defer dataConn.Close()
	const timestampBase uint32 = 90_000
	if err := sendBufferedPacket(dataConn, shk, 0, timestampBase, ssrc, frames[0]); err != nil {
		t.Fatalf("pre-anchor packet: %v", err)
	}
	// A staged stream may buffer, but no anchor means no audible output.
	time.Sleep(150 * time.Millisecond)
	if bytes, _ := sinks.state(); bytes != 0 {
		t.Fatalf("buffered audio played %d bytes before a valid anchor", bytes)
	}

	owner := mgr.Active()
	if owner == nil {
		t.Fatal("the authenticated setup did not promote the session")
	}
	// The promoted HAP owner changes the one persisted master. The sender's
	// volume must not scale the PCM this app writes, so the exact decoded
	// fixture asserted below stays byte-for-byte intact at a mute position.
	if response, err := client.do("SET_PARAMETER", "/1", "text/parameters", []byte("volume: -144\r\n")); err != nil || response.status != 200 {
		t.Fatalf("SET_PARAMETER volume = %v %v", response, err)
	}
	masterMu.Lock()
	wrote, gotMaster := append([]float64(nil), masterWrites...), master
	masterMu.Unlock()
	if len(wrote) != 1 || wrote[0] != 0 || gotMaster != 0 {
		t.Fatalf("buffered sender volume wrote %v (master %v), want one mute", wrote, gotMaster)
	}
	liveStreams := owner.activeStreams()
	if len(liveStreams) != 1 || liveStreams[0].buffered == nil {
		t.Fatalf("live buffered streams = %d, want one", len(liveStreams))
	}
	live := liveStreams[0]
	incomplete, err := plist.EncodeBinary(map[string]any{
		"rtpTime": int64(timestampBase),
		"rate":    int64(1),
	})
	if err != nil {
		t.Fatalf("encode incomplete anchor: %v", err)
	}
	if refused, err := client.do("SETRATEANCHORTI", "/1", "application/x-apple-binary-plist", incomplete); err != nil || refused.status != 400 {
		t.Fatalf("incomplete buffered anchor (SETRATEANCHORTI) = %v %v, want 400 from the shared handler", refused, err)
	}
	if bytes, _ := sinks.state(); bytes != 0 {
		t.Fatalf("incomplete anchor produced %d bytes of audio", bytes)
	}
	networkSecs := time.Now().Unix()
	anchorBody, err := plist.EncodeBinary(map[string]any{
		"networkTimeSecs":       int64(networkSecs),
		"networkTimeFrac":       int64(0),
		"networkTimeTimelineID": int64(1),
		"rtpTime":               int64(timestampBase),
		"rate":                  int64(1),
	})
	if err != nil {
		t.Fatalf("encode anchor: %v", err)
	}
	anchor, err := client.do("SETRATEANCHORTIME", "/1", "application/x-apple-binary-plist", anchorBody)
	if err != nil || anchor.status != 200 {
		t.Fatalf("SETRATEANCHORTIME = %v %v, want the shared anchor handler to start playout", anchor, err)
	}
	limit := len(frames)
	if limit > 9 {
		limit = 9
	}
	for i := 1; i < limit; i++ {
		if err := sendBufferedPacket(dataConn, shk, uint32(i), timestampBase+uint32(i*spf), ssrc, frames[i]); err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
	}
	waitForBuffered(t, "decoded PCM at the sink", func() bool {
		written, _ := sinks.state()
		return written > 0
	})
	if !sinks.nonZeroPCM() {
		t.Fatal("the sink received only zero-valued PCM")
	}

	// A plaintext candidate cannot take over with an otherwise well-formed
	// type-103 SETUP.
	plain := dialTCPStream(t, port)
	plainClient := newRTSPClient(plain)
	plainBody, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeBuffered),
			"audioFormat": int64(format),
			"spf":         int64(spf),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode plaintext SETUP: %v", err)
	}
	refused, err := plainClient.do("SETUP", "/1", "application/x-apple-binary-plist", plainBody)
	if err != nil || refused.status == 200 {
		t.Fatalf("plaintext type-103 SETUP = %v %v, want a refusal", refused, err)
	}
	plain.Close()
	if mgr.Active() != owner {
		t.Fatal("a refused plaintext SETUP replaced the live sender")
	}

	pauseBody, err := plist.EncodeBinary(map[string]any{"rate": int64(0)})
	if err != nil {
		t.Fatalf("encode pause: %v", err)
	}
	endpointTimestamp := timestampBase + 11*uint32(spf)
	pause, err := client.do("SETRATEANCHORTIME", "/1", "application/x-apple-binary-plist", pauseBody)
	if err != nil || pause.status != 200 {
		t.Fatalf("SETRATEANCHORTIME pause = %v %v", pause, err)
	}
	if !sinks.allAborted() {
		t.Fatal("pause did not abort the current playback sink")
	}
	if err := sendBufferedPacket(dataConn, shk, 10, endpointTimestamp, ssrc, frames[1]); err != nil {
		t.Fatalf("packet while paused: %v", err)
	}
	waitForBuffered(t, "stale paused record admission", func() bool {
		return bufferedHasSequence(live, 10)
	})
	time.Sleep(150 * time.Millisecond)
	pausedBytes, _ := sinks.state()
	time.Sleep(100 * time.Millisecond)
	if after, _ := sinks.state(); after != pausedBytes {
		t.Fatalf("paused buffered stream wrote %d bytes after pause", after-pausedBytes)
	}

	// Immediate FLUSHBUFFERED stops playout and discards sequences below its
	// endpoint; the endpoint itself is half-open. The paused seq10 and endpoint
	// seq11 share a timestamp, so queue admission and exact PCM distinguish an
	// off-by-one rather than a timestamp difference.
	flushBody, err := plist.EncodeBinary(map[string]any{
		"flushUntilSeq": int64(11),
		"flushUntilTS":  int64(endpointTimestamp),
	})
	if err != nil {
		t.Fatalf("encode immediate flush: %v", err)
	}
	beforeFlush, _ := sinks.state()
	flush, err := client.do("FLUSHBUFFERED", "/1", "application/x-apple-binary-plist", flushBody)
	if err != nil || flush.status != 200 {
		t.Fatalf("FLUSHBUFFERED immediate = %v %v", flush, err)
	}
	if bufferedHasSequence(live, 10) {
		t.Fatal("immediate flush retained the obsolete seq10 below its endpoint")
	}
	time.Sleep(150 * time.Millisecond)
	if after, _ := sinks.state(); after != beforeFlush {
		t.Fatalf("immediate flush left %d obsolete bytes playable", after-beforeFlush)
	}

	// A fresh anchor at the endpoint timestamp authorises the endpoint record
	// itself. Sending sequence 12 would not distinguish an off-by-one range.
	resumeTimestamp := endpointTimestamp
	resumeBody, err := plist.EncodeBinary(map[string]any{
		"networkTimeSecs":       int64(networkSecs + 1),
		"networkTimeFrac":       int64(0),
		"networkTimeTimelineID": int64(1),
		"rtpTime":               int64(resumeTimestamp),
		"rate":                  int64(1),
	})
	if err != nil {
		t.Fatalf("encode resume anchor: %v", err)
	}
	resume, err := client.do("SETRATEANCHORTIME", "/1", "application/x-apple-binary-plist", resumeBody)
	if err != nil || resume.status != 200 {
		t.Fatalf("SETRATEANCHORTIME resume anchor = %v %v", resume, err)
	}
	if err := sendBufferedPacket(dataConn, shk, 11, resumeTimestamp, ssrc, frames[2]); err != nil {
		t.Fatalf("endpoint packet: %v", err)
	}
	var expectedEndpoint []byte
	if format == bufferedFormatALAC48S24Stereo {
		expectedEndpoint = expectedBufferedPCM(t, frames[2])
	}
	waitForBuffered(t, "endpoint PCM after a fresh anchor", func() bool {
		after, _ := sinks.state()
		last := sinks.lastPCM()
		if expectedEndpoint != nil {
			return after > beforeFlush && sinks.count() >= 2 && len(last) >= len(expectedEndpoint)
		}
		return after > beforeFlush && sinks.count() >= 2 && len(last) > 0
	})
	if got := sinks.lastPCM(); expectedEndpoint != nil {
		if !bytes.Equal(got, expectedEndpoint) {
			t.Fatalf("endpoint sink PCM = %d bytes, want exact ALAC frame after flushUntilSeq=11", len(got))
		}
	} else if !pcmHasSignal(got) {
		t.Fatal("AAC endpoint produced no non-zero PCM through the fresh sink")
	}
	if sinks.count() < 2 {
		t.Fatal("resume did not open a fresh playback sink after the old sink was aborted")
	}
	if sinks.allAborted() {
		t.Fatal("resume did not leave a live playback sink generation")
	}

	teardown, err := client.do("TEARDOWN", "/1", "", nil)
	if err != nil || teardown.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", teardown, err)
	}
	waitForBuffered(t, "session release", func() bool { return !mgr.board.Live() })
	if !sinks.allAborted() {
		t.Fatal("teardown did not abort every playback sink generation")
	}
}

func TestBufferedType103PlaysAndControls(t *testing.T) {
	t.Run("ALAC 48k 24-bit stereo", func(t *testing.T) {
		runBufferedType103Flow(t, bufferedFormatALAC48S24Stereo, 352, 0x15000000, bufferedALAC352Frames(4))
	})
	t.Run("AAC-LC 44.1k stereo", func(t *testing.T) {
		if !aac.Available() {
			t.Skip("FFmpeg AAC decoder is not linked in this build")
		}
		runBufferedType103Flow(t, bufferedFormatAAC44F24Stereo, 1024, 0x16000000, readBufferedAACFrames(t))
	})
}

func TestBufferedDeferredFlushDropsOnlyRange(t *testing.T) {
	transport := newFakeTransport()
	sink := &recordingSink{}
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Deferred Range Test",
		FrameTarget: 20 * time.Millisecond, RingEnabled: false, Logf: t.Logf,
	}, transport, &fakeAdvertiser{}, fakeRings{ring: &fakeRing{}}, recordingSinks{sink: sink},
		media.NewBoard(), nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	session := &Session{m: mgr, peer: Peer{Address: "receiver.test"}, rate: 1, done: make(chan struct{})}
	stream, err := session.newAudioStream(audioStreamSpec{
		streamType: streamTypeBuffered,
		codec:      "alac",
		config: alac.Config{
			FrameLength: 352, SampleRate: 48000, Channels: 2, BitDepth: 24,
		},
		sharedKey:       make([]byte, 32),
		samplesPerFrame: 352,
	})
	if err != nil {
		t.Fatalf("new buffered stream: %v", err)
	}
	defer stream.stop()
	session.streams = []*audioStream{stream}
	session.setBufferedAnchor([]*audioStream{stream}, 1, 1000, 0)
	stream.launch()
	dataPort := 0
	_, dataPort = stream.dataListener.Addr()
	dataConn, err := transport.dial(dataPort)
	if err != nil {
		stream.stop()
		t.Fatalf("dial buffered data port: %v", err)
	}
	defer dataConn.Close()

	frames := bufferedALAC352Frames(3)
	const (
		inRangeSequence  uint32 = 3
		endpointSequence uint32 = 4
		anchorTimestamp  uint32 = 1000
	)
	// A is due immediately. The SSRC_NONE control block resets timestamp
	// expectation without producing PCM, so B can be due 200 ms later without
	// being mistaken for a timestamp discontinuity. After A is written B is
	// popped and held in pace() while deferred [3,4) is registered. C is the
	// preserved endpoint and follows B by one ALAC block.
	timestamps := []uint32{anchorTimestamp, anchorTimestamp + 9600, anchorTimestamp + 9600 + 352}
	for i, timestamp := range timestamps {
		sequence := uint32(i + 1)
		if i > 0 {
			sequence++
		}
		if err := sendBufferedPacket(dataConn, stream.spec.sharedKey, sequence, timestamp, bufferedSSRCALAC48, frames[i]); err != nil {
			stream.stop()
			t.Fatalf("send range frame %d: %v", sequence, err)
		}
		if i == 0 {
			if err := sendBufferedPacket(dataConn, stream.spec.sharedKey, 2, anchorTimestamp+352, bufferedSSRCNone, nil); err != nil {
				stream.stop()
				t.Fatalf("send SSRC_NONE control block: %v", err)
			}
		}
	}
	waitForBuffered(t, "in-range frame to be pacing", func() bool {
		stream.buffered.mu.Lock()
		pacing := stream.buffered.pacing && stream.buffered.pacingSequence == inRangeSequence
		stream.buffered.mu.Unlock()
		writes, _ := sink.snapshot()
		return pacing && len(writes) == 1
	})

	body, err := plist.EncodeBinary(map[string]any{
		"flushFromSeq":  int64(inRangeSequence),
		"flushFromTS":   int64(timestamps[1]),
		"flushUntilSeq": int64(endpointSequence),
		"flushUntilTS":  int64(timestamps[2]),
	})
	if err != nil {
		stream.stop()
		t.Fatalf("encode deferred range: %v", err)
	}
	response, _, err := session.handleFlushBuffered(&rtsp.Request{Method: "FLUSHBUFFERED", Body: body})
	if err != nil {
		stream.stop()
		t.Fatalf("deferred flush: %v", err)
	}
	if response == nil || response.Status != 200 {
		stream.stop()
		t.Fatalf("deferred flush response = %v", response)
	}

	waitForBuffered(t, "pre-range and endpoint PCM", func() bool {
		writes, _ := sink.snapshot()
		return len(writes) == 2
	})
	writes, _ := sink.snapshot()
	if len(writes) != 2 {
		stream.stop()
		t.Fatalf("sink received %d writes, want pre-range and endpoint only", len(writes))
	}
	if !bytes.Equal(writes[0], expectedBufferedPCM(t, frames[0])) {
		stream.stop()
		t.Fatal("pre-range PCM does not match the frame before the flush range")
	}
	if !bytes.Equal(writes[1], expectedBufferedPCM(t, frames[2])) {
		stream.stop()
		t.Fatal("endpoint PCM was not preserved or an in-range frame was written")
	}
	// Exact write count and bytes above distinguish the in-range frame from
	// the before/end frames rather than relying on elapsed output length.
	teardown, _, err := session.handleTeardown(&rtsp.Request{Method: "TEARDOWN"})
	if err != nil {
		stream.stop()
		t.Fatalf("teardown: %v", err)
	}
	if teardown == nil || teardown.Status != 200 {
		stream.stop()
		t.Fatalf("teardown response = %v", teardown)
	}
	if _, aborted := sink.snapshot(); !aborted {
		stream.stop()
		t.Fatal("teardown did not abort the recording sink")
	}
}

func TestCompactBufferedFlushUsesUnsignedSequence(t *testing.T) {
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Compact Flush Test",
		FrameTarget: 20 * time.Millisecond, Logf: t.Logf,
	}, newFakeTransport(), &fakeAdvertiser{}, fakeRings{ring: &fakeRing{}}, fakeSinks{sink: &fakeSink{}},
		media.NewBoard(), nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	session := &Session{m: mgr, done: make(chan struct{}), rate: 1}
	stream, err := session.newAudioStream(audioStreamSpec{
		streamType: streamTypeBuffered,
		codec:      "alac",
		config: alac.Config{
			FrameLength: 352, SampleRate: 48000, Channels: 2, BitDepth: 24,
		},
		sharedKey:       make([]byte, 32),
		samplesPerFrame: 352,
	})
	if err != nil {
		t.Fatalf("new buffered stream: %v", err)
	}
	defer stream.stop()
	session.streams = []*audioStream{stream}
	body, err := plist.EncodeBinary(map[string]any{
		"flushUntilSeq": int64(200),
		"flushUntilTS":  int64(0),
	})
	if err != nil {
		t.Fatalf("encode compact flush: %v", err)
	}
	response, _, err := session.handleFlushBuffered(&rtsp.Request{Method: "FLUSHBUFFERED", Body: body})
	if err != nil {
		t.Fatalf("compact flush: %v", err)
	}
	if response == nil || response.Status != 200 {
		t.Fatalf("compact flush response = %v", response)
	}
	if got := stream.buffered.immediateUntil; got != 200 {
		t.Fatalf("compact flush endpoint = %#x, want 0xc8", got)
	}

	// A UID is not an integer. It must be refused without changing the live
	// endpoint established above.
	uidFlush, err := plist.EncodeBinary(map[string]any{
		"flushUntilSeq": plist.UID(201),
		"flushUntilTS":  int64(0),
	})
	if err != nil {
		t.Fatalf("encode UID flush: %v", err)
	}
	if response, _, err = session.handleFlushBuffered(&rtsp.Request{Method: "FLUSHBUFFERED", Body: uidFlush}); err != nil || response == nil || response.Status != 400 {
		t.Fatalf("UID FLUSHBUFFERED = %v %v, want 400", response, err)
	}
	if got := stream.buffered.immediateUntil; got != 200 {
		t.Fatalf("UID flush changed endpoint to %#x", got)
	}

	// 2^64+200 must be rejected as an out-of-range wide integer, not truncated
	// to 200 or accepted as a numeric flush endpoint.
	wideFlush := singleEntryBinary("flushUntilSeq", wideIntegerObject(1, 200))
	if response, _, err = session.handleFlushBuffered(&rtsp.Request{Method: "FLUSHBUFFERED", Body: wideFlush}); err != nil || response == nil || response.Status != 400 {
		t.Fatalf("wide FLUSHBUFFERED = %v %v, want 400", response, err)
	}
	if got := stream.buffered.immediateUntil; got != 200 {
		t.Fatalf("wide flush changed endpoint to %#x", got)
	}

	// The same wide integer used as an anchor rate must not mutate play state.
	wideRate := singleEntryBinary("rate", wideIntegerObject(1, 1)) // 2^64+1
	if response, _, err = session.handleSetRateAnchorTime(&rtsp.Request{Method: "SETRATEANCHORTI", Body: wideRate}); err != nil || response == nil || response.Status != 400 {
		t.Fatalf("wide anchor rate = %v %v, want 400", response, err)
	}
	if stream.buffered.playRequested || stream.buffered.anchorSet {
		t.Fatal("wide anchor rate mutated buffered play state")
	}
}

func TestBufferedPlistUnsignedBitPatterns(t *testing.T) {
	if got, ok := plistUint64Bits(int64(-1)); !ok || got != ^uint64(0) {
		t.Fatalf("unsigned 64-bit bit pattern = %#x, ok=%v; want all bits set", got, ok)
	}
	if got, ok := plistUint64Bits(int64(-2)); !ok || got != ^uint64(0)-1 {
		t.Fatalf("unsigned binary fraction bit pattern = %#x, ok=%v; want %#x", got, ok, ^uint64(0)-1)
	}
	if got, ok := plistUint32Bits(int64(-1)); ok || got != 0 {
		t.Fatalf("negative 32-bit field = %#x, ok=%v; want rejection", got, ok)
	}
}

func wideIntegerObject(high, low uint64) []byte {
	object := make([]byte, 17)
	object[0] = 0x14
	binary.BigEndian.PutUint64(object[1:9], high)
	binary.BigEndian.PutUint64(object[9:17], low)
	return object
}

// singleEntryBinary builds a complete binary plist dictionary with one short
// key and an arbitrary value object. It is used only to inject malformed
// integers the normal encoder cannot produce.
func singleEntryBinary(key string, value []byte) []byte {
	objects := [][]byte{
		{0xd1, 1, 2}, // dict, one pair: key ref 1, value ref 2
		append([]byte{0x50 | byte(len(key))}, key...),
		value,
	}
	b := []byte("bplist00")
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = len(b)
		b = append(b, object...)
	}
	offsetTableOffset := len(b)
	for _, offset := range offsets {
		b = append(b, byte(offset))
	}
	append64 := func(v uint64) {
		for i := 7; i >= 0; i-- {
			b = append(b, byte(v>>uint(8*i)))
		}
	}
	b = append(b, make([]byte, 6)...)
	b = append(b, 1, 1) // offsetIntSize, objectRefSize
	append64(uint64(len(objects)))
	append64(0) // top object
	append64(uint64(offsetTableOffset))
	return b
}

func TestBufferedSetupRequiresAuthenticationAndKey(t *testing.T) {
	transport := &realTransport{}
	t.Cleanup(transport.Close)
	sink := &fakeSink{}
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Buffered Validation", Port: 0,
		FrameTarget: 20 * time.Millisecond, Logf: t.Logf,
	}, transport, &fakeAdvertiser{}, fakeRings{ring: &fakeRing{}}, fakeSinks{sink: sink},
		media.NewBoard(), nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	port := mgr.advertisedPort()

	// Plaintext: refuse before the AP2 control socket or data listener exists.
	conn := dialTCPStream(t, port)
	client := newRTSPClient(conn)
	shk := make([]byte, 32)
	if _, err := rand.Read(shk); err != nil {
		t.Fatalf("shk: %v", err)
	}
	response, err := client.do("SETUP", "/1", "application/x-apple-binary-plist",
		bufferedSetupBody(t, bufferedFormatALAC48S24Stereo, shk))
	if err != nil || response.status == 200 {
		t.Fatalf("plaintext SETUP = %v %v, want refusal", response, err)
	}
	if mgr.Active() != nil {
		t.Fatal("a plaintext type-103 SETUP promoted a session")
	}
	if len(transport.all) != 1 {
		t.Fatalf("refused SETUP created %d listeners, want only the RTSP control listener", len(transport.all)-1)
	}
	if bytes, _ := sink.state(); bytes != 0 {
		t.Fatalf("refused SETUP produced %d bytes of audio", bytes)
	}
	conn.Close()

	// Authenticated, but the fixed shk length is mandatory.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("controller key: %v", err)
	}
	const controllerID = "validation-controller"
	mgr.Store().SetControllerLTPK(controllerID, public)
	controlConn := dialTCPStream(t, port)
	defer controlConn.Close()
	secureClient := newRTSPClient(controlConn)
	shared := pairVerify(t, secureClient, controllerID, private)
	keys := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(controlConn, keys.Inbound, keys.Outbound)
	if err != nil {
		t.Fatalf("encrypted control channel: %v", err)
	}
	secureClient.setStream(&rwStream{rw: encrypted})
	response, err = secureClient.do("SETUP", "/1", "application/x-apple-binary-plist",
		bufferedSetupBody(t, bufferedFormatALAC48S24Stereo, shk[:31]))
	if err != nil || response.status == 200 {
		t.Fatalf("short-shk SETUP = %v %v, want refusal", response, err)
	}
	if response, err = secureClient.do("SETUP", "/1", "application/x-apple-binary-plist",
		bufferedSetupBody(t, 1<<18, shk)); err != nil || response.status == 200 {
		t.Fatalf("unsupported buffered format SETUP = %v %v, want refusal", response, err)
	}
	partialFlush, err := plist.EncodeBinary(map[string]any{
		"flushFromSeq":  int64(4),
		"flushUntilSeq": int64(5),
		"flushUntilTS":  int64(6),
	})
	if err != nil {
		t.Fatalf("encode partial flush: %v", err)
	}
	if response, err = secureClient.do("FLUSHBUFFERED", "/1", "application/x-apple-binary-plist", partialFlush); err != nil || response.status == 200 {
		t.Fatalf("partial FLUSHBUFFERED = %v %v, want refusal", response, err)
	}
	untypedFlush, err := plist.EncodeBinary(map[string]any{
		"flushFromSeq":  "four",
		"flushFromTS":   int64(4),
		"flushUntilSeq": int64(5),
		"flushUntilTS":  int64(6),
	})
	if err != nil {
		t.Fatalf("encode untyped flush: %v", err)
	}
	if response, err = secureClient.do("FLUSHBUFFERED", "/1", "application/x-apple-binary-plist", untypedFlush); err != nil || response.status == 200 {
		t.Fatalf("untyped FLUSHBUFFERED = %v %v, want refusal", response, err)
	}
	// The first descriptor is valid; the second is not. Validation must run
	// before either descriptor opens a listener.
	mixed, err := plist.EncodeBinary(map[string]any{
		"streams": []any{
			map[string]any{"type": int64(streamTypeBuffered), "audioFormat": int64(bufferedFormatALAC48S24Stereo), "spf": int64(4096), "shk": shk},
			map[string]any{"type": int64(streamTypeBuffered), "audioFormat": int64(1 << 18), "spf": int64(4096), "shk": shk},
		},
	})
	if err != nil {
		t.Fatalf("encode mixed SETUP: %v", err)
	}
	before := len(transport.all)
	response, err = secureClient.do("SETUP", "/1", "application/x-apple-binary-plist", mixed)
	if err != nil || response.status == 200 {
		t.Fatalf("mixed SETUP = %v %v, want refusal", response, err)
	}
	if after := len(transport.all); after != before {
		t.Fatalf("refused mixed SETUP opened %d listeners", after-before)
	}
	// The pair-verify channel is authenticated but remains a candidate until
	// an accepted media request; every SETUP above was refused.
	waitForBuffered(t, "the refused candidate", func() bool { return pendingCount(mgr) == 1 })
	if mgr.Active() != nil {
		t.Fatal("refused SETUPs started a media session")
	}
	if got := len(transport.all); got != 1 {
		t.Fatalf("refused SETUPs left %d listeners, want only the RTSP listener", got)
	}
	if bytes, _ := sink.state(); bytes != 0 {
		t.Fatalf("invalid SETUPs produced %d bytes of audio", bytes)
	}
}

// timelineClock proves the buffered path asks for the anchor's own timeline
// and never falls back to the aggregate realtime offset.
type timelineClock struct {
	offsets map[uint64]int64
	asked   []uint64
}

func (c *timelineClock) Offset() (int64, bool) {
	// Deliberately wrong for the buffered anchor: using this would fail.
	return 3_000_000_000, true
}

func (c *timelineClock) OffsetForClockID(id uint64) (int64, bool) {
	c.asked = append(c.asked, id)
	offset, ok := c.offsets[id]
	return offset, ok
}

func TestBufferedAnchorUsesOnlyItsPTPTimeline(t *testing.T) {
	const (
		timelineA uint64 = 0x0102030405060708
		timelineB uint64 = 0x1112131415161718
	)
	clock := &timelineClock{offsets: map[uint64]int64{
		timelineA: 1_000_000_000,
		timelineB: 3_000_000_000,
	}}
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Timeline Test",
		FrameTarget: 20 * time.Millisecond, Logf: t.Logf,
	}, newFakeTransport(), &fakeAdvertiser{}, fakeRings{ring: &fakeRing{}}, fakeSinks{sink: &fakeSink{}},
		media.NewBoard(), nil, func() Clock { return clock }, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	session := &Session{m: mgr, done: make(chan struct{}), rate: 1}
	stream, err := session.newAudioStream(audioStreamSpec{
		streamType: streamTypeBuffered,
		codec:      "alac",
		config: alac.Config{
			FrameLength: 352, SampleRate: 48000, Channels: 2, BitDepth: 24,
		},
		sharedKey:       make([]byte, 32),
		samplesPerFrame: 352,
	})
	if err != nil {
		t.Fatalf("new buffered stream: %v", err)
	}
	defer stream.stop()

	format, ok := formatForSSRC(bufferedSSRCALAC48)
	if !ok {
		t.Fatal("buffered ALAC format is not recognised")
	}
	const networkNS = uint64(5_000_000_000)
	session.setBufferedAnchor([]*audioStream{stream}, timelineA, 100, networkNS)
	if !stream.buffered.resolveAnchorOnce(format) {
		t.Fatal("timeline A anchor did not resolve")
	}
	if got, want := stream.buffered.anchorLocal.UnixNano(), int64(networkNS-1_000_000_000); got != want {
		t.Fatalf("timeline A local anchor = %d, want %d (aggregate offset was not used)", got, want)
	}

	session.setBufferedAnchor([]*audioStream{stream}, timelineB, 200, networkNS)
	if !stream.buffered.resolveAnchorOnce(format) {
		t.Fatal("timeline B anchor did not resolve")
	}
	if got, want := stream.buffered.anchorLocal.UnixNano(), int64(networkNS-3_000_000_000); got != want {
		t.Fatalf("timeline B local anchor = %d, want %d", got, want)
	}
	if len(clock.asked) != 2 || clock.asked[0] != timelineA || clock.asked[1] != timelineB {
		t.Fatalf("timeline lookups = %#x, want [%#x %#x]", clock.asked, timelineA, timelineB)
	}
}

func TestBufferedFeatureOverridesRemainTruthful(t *testing.T) {
	advertiser := &fakeAdvertiser{}
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Feature Override",
		// Keep bit 40 and an unrelated codec bit, but clear both buffered
		// codecs: the override cannot leave a buffered claim with no format.
		Features: FeatureAirPlayAudio | FeatureBufferedAudio | FeatureReceiveAudioPCM,
		Logf:     t.Logf,
	}, newFakeTransport(), advertiser, fakeRings{ring: &fakeRing{}}, fakeSinks{sink: &fakeSink{}},
		media.NewBoard(), nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := mgr.advertisedFeatures(); got&FeatureBufferedAudio != 0 {
		t.Fatalf("feature override kept bit 40 with no buffered codec: %#x", got)
	}
	// The override is the whole mask: a set that omits the FairPlay handshake
	// bit must not inherit it from the default.
	if got := mgr.advertisedFeatures(); got&FeatureFairPlayHandshake != 0 {
		t.Fatalf("feature override gained bit 14 without asking: %#x", got)
	}
	if got := bufferedStreamFormats(mgr.advertisedFeatures()); got != 0 {
		t.Fatalf("feature override reports buffered formats %#x, want none", got)
	}
	if got := mgr.advertiseTXT()["features"]; got == "" {
		t.Fatal("advertisement has no features record")
	}

	stock, _, _, _, _, _, _ := newTestManager(t)
	features := stock.advertisedFeatures()
	formats := bufferedStreamFormats(features)
	if features&FeatureBufferedAudio == 0 {
		t.Fatal("stock receiver does not advertise buffered audio")
	}
	if formats&bufferedFormatALAC48S24Stereo == 0 {
		t.Fatal("stock receiver does not advertise buffered ALAC")
	}
	if features&FeatureFairPlayHandshake == 0 {
		t.Fatal("stock receiver does not advertise feature bit 14 for the implemented /fp-setup handshake")
	}
	if aac.Available() {
		if features&FeatureReceiveAudioAACLC == 0 || formats&(bufferedFormatAAC44F24Stereo|bufferedFormatAAC48F24Stereo) == 0 {
			t.Fatal("FFmpeg build does not advertise its buffered AAC-LC formats")
		}
	} else {
		if features&FeatureReceiveAudioAACLC != 0 || formats&(bufferedFormatAAC44F24Stereo|bufferedFormatAAC48F24Stereo) != 0 {
			t.Fatal("non-FFmpeg build claims AAC-LC it cannot decode")
		}
	}
}

// type103Rig is one authenticated type-103 session over the real loopback
// transport: control channel, staged stream, data connection and the shared
// key the sender encrypts records with.
type type103Rig struct {
	mgr    *Manager
	board  *media.Board
	owner  *Session
	client *rtspClient
	data   *realStream
	shk    []byte
	stream *audioStream
}

func startType103Rig(t *testing.T, rings RingFactory, sinks SinkFactory) *type103Rig {
	t.Helper()
	transport := &realTransport{}
	t.Cleanup(transport.Close)
	board := media.NewBoard()
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Type-103 State Test", Port: 0,
		FrameTarget: 100 * time.Millisecond, RingEnabled: true, Logf: t.Logf,
	}, transport, &fakeAdvertiser{}, rings, sinks, board, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	port := mgr.advertisedPort()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("controller key: %v", err)
	}
	const controllerID = "type103-state-controller"
	mgr.Store().SetControllerLTPK(controllerID, public)
	control := dialTCPStream(t, port)
	t.Cleanup(func() { _ = control.Close() })
	client := newRTSPClient(control)
	shared := pairVerify(t, client, controllerID, private)
	keys := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(control, keys.Inbound, keys.Outbound)
	if err != nil {
		t.Fatalf("encrypted control channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})

	shk := make([]byte, 32)
	if _, err := rand.Read(shk); err != nil {
		t.Fatalf("shk: %v", err)
	}
	body, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeBuffered),
			"audioFormat": int64(bufferedFormatALAC48S24Stereo),
			"spf":         int64(352),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err != nil || setup.status != 200 {
		t.Fatalf("type-103 SETUP = %v %v", setup, err)
	}
	decoded, err := plist.Decode(setup.body)
	if err != nil {
		t.Fatalf("decode SETUP: %v", err)
	}
	setupDict, _ := decoded.(map[string]any)
	streams, _ := setupDict["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("SETUP streams = %v", setupDict["streams"])
	}
	streamDict, _ := streams[0].(map[string]any)
	dataPort := int(plistUint(streamDict["dataPort"]))
	if dataPort == 0 {
		t.Fatalf("SETUP descriptor has no dataPort: %v", streamDict)
	}
	data := dialTCPStream(t, dataPort)
	t.Cleanup(func() { _ = data.Close() })

	waitUntil(t, "the type-103 owner", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()
	waitUntil(t, "the launched type-103 stream", func() bool {
		return len(owner.activeStreams()) == 1 && owner.activeStreams()[0].buffered != nil
	})
	// A new buffered data connection resets the anchor and queue when it is
	// accepted. Wait for that generation before a test sends its first record
	// and anchor, or the accept could discard both.
	buffered := owner.activeStreams()[0].buffered
	waitUntil(t, "the type-103 data connection", func() bool {
		buffered.mu.Lock()
		defer buffered.mu.Unlock()
		return buffered.connID != 0
	})
	return &type103Rig{
		mgr: mgr, board: board, owner: owner, client: client,
		data: data, shk: shk, stream: owner.activeStreams()[0],
	}
}

func (r *type103Rig) waitQueued(t *testing.T, want int) {
	t.Helper()
	waitForBuffered(t, "the buffered record to be admitted", func() bool {
		r.stream.buffered.mu.Lock()
		defer r.stream.buffered.mu.Unlock()
		return r.stream.buffered.pendingLocked() >= want
	})
}

func (r *type103Rig) playAnchor(t *testing.T, timestamp uint32) {
	t.Helper()
	body, err := plist.EncodeBinary(map[string]any{
		"networkTimeSecs":       time.Now().Unix(),
		"networkTimeFrac":       int64(0),
		"networkTimeTimelineID": int64(1),
		"rtpTime":               int64(timestamp),
		"rate":                  int64(1),
	})
	if err != nil {
		t.Fatalf("encode anchor: %v", err)
	}
	response, err := r.client.do("SETRATEANCHORTIME", "/1", "application/x-apple-binary-plist", body)
	if err != nil || response.status != 200 {
		t.Fatalf("SETRATEANCHORTIME = %v %v", response, err)
	}
}

func sendPlaybackState(t *testing.T, client *rtspClient, state int64) {
	t.Helper()
	body, err := plist.EncodeBinary(map[string]any{
		"type":   "updateMRPlaybackState",
		"params": map[string]any{"mrPlaybackState": state},
	})
	if err != nil {
		t.Fatalf("encode playback state: %v", err)
	}
	response, err := client.do("POST", "/command", "application/x-apple-binary-plist", body)
	if err != nil || response.status != 200 {
		t.Fatalf("updateMRPlaybackState %d = %v %v", state, response, err)
	}
}

func playbackState(board *media.Board) string {
	state, _ := board.Snapshot()["playbackState"].(string)
	return state
}

// TestBufferedType103PlaybackStatePrecedence covers both directions of the
// display rule: a sender's "playing" report cannot paint first until decoded
// PCM reaches the sink, and PCM already in flight cannot repaint over an
// explicit paused or stopped report. The transport is untouched by the report.
func TestBufferedType103PlaybackStatePrecedence(t *testing.T) {
	sinks := &reopeningSinks{}
	rig := startType103Rig(t, fakeRings{ring: &fakeRing{}}, sinks)
	board := rig.board
	if got := playbackState(board); got != string(media.StateBuffering) {
		t.Fatalf("fresh type-103 state = %q, want buffering", got)
	}

	// The sender reports playing before any PCM: the player is still filling
	// its buffer, so it must not claim playback that has not happened.
	sendPlaybackState(t, rig.client, 1)
	if got := playbackState(board); got == string(media.StatePlaying) {
		t.Fatal("a pre-PCM playing report repainted the buffered player as playing")
	}

	frames := bufferedALAC352Frames(4)
	const base uint32 = 90_000
	if err := sendBufferedPacket(rig.data, rig.shk, 0, base, bufferedSSRCALAC48, frames[0]); err != nil {
		t.Fatalf("first packet: %v", err)
	}
	rig.waitQueued(t, 1)
	rig.playAnchor(t, base)
	waitForBuffered(t, "the first decoded PCM", func() bool {
		written, _ := sinks.state()
		return written > 0
	})
	waitForBuffered(t, "the player to report playing after PCM", func() bool {
		return playbackState(board) == string(media.StatePlaying)
	})

	// An explicit pause freezes the display even though the transport and the
	// already-anchored queue continue to deliver PCM.
	sendPlaybackState(t, rig.client, 2)
	waitForBuffered(t, "the paused report", func() bool {
		return playbackState(board) == string(media.StatePaused)
	})
	before, _ := sinks.state()
	if err := sendBufferedPacket(rig.data, rig.shk, 1, base+352, bufferedSSRCALAC48, frames[1]); err != nil {
		t.Fatalf("packet after pause report: %v", err)
	}
	waitForBuffered(t, "PCM after the pause report", func() bool {
		written, _ := sinks.state()
		return written > before
	})
	if got := playbackState(board); got != string(media.StatePaused) {
		t.Fatalf("PCM overwrote the explicit paused report with %q", got)
	}

	// The same rule applies to stopped, and a later explicit playing report is
	// authoritative again once real PCM has already been observed.
	sendPlaybackState(t, rig.client, 3)
	waitForBuffered(t, "the stopped report", func() bool {
		return playbackState(board) == string(media.StateStopped)
	})
	before, _ = sinks.state()
	if err := sendBufferedPacket(rig.data, rig.shk, 2, base+704, bufferedSSRCALAC48, frames[2]); err != nil {
		t.Fatalf("packet after stop report: %v", err)
	}
	waitForBuffered(t, "PCM after the stop report", func() bool {
		written, _ := sinks.state()
		return written > before
	})
	if got := playbackState(board); got != string(media.StateStopped) {
		t.Fatalf("PCM overwrote the explicit stopped report with %q", got)
	}
	sendPlaybackState(t, rig.client, 1)
	waitForBuffered(t, "the explicit playing report", func() bool {
		return playbackState(board) == string(media.StatePlaying)
	})
}

// gatedRing blocks Close until the test releases it, so teardown order is
// observable rather than timing-dependent.
type gatedRing struct {
	mu           sync.Mutex
	frames       int
	closed       bool
	closeStarted chan struct{}
	closeRelease chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
}

func newGatedRing() *gatedRing {
	return &gatedRing{closeStarted: make(chan struct{}), closeRelease: make(chan struct{})}
}

func (r *gatedRing) Pixels(pixels []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(pixels) != 36 {
		return fmt.Errorf("ring frame is %d bytes", len(pixels))
	}
	r.frames++
	return nil
}

func (r *gatedRing) Close() error {
	r.startOnce.Do(func() { close(r.closeStarted) })
	<-r.closeRelease
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}

func (r *gatedRing) release() { r.releaseOnce.Do(func() { close(r.closeRelease) }) }

func (r *gatedRing) wasClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

type gatedRings struct{ ring *gatedRing }

func (g gatedRings) OpenRing(context.Context) (Ring, error) { return g.ring, nil }

// TestTeardownStopsAudioBeforeRingRelease pins the takeover invariant: a
// blocked routing-capture release must not keep a superseded sender's audio
// sink alive. The sink is aborted before the ring release is even entered.
func TestTeardownStopsAudioBeforeRingRelease(t *testing.T) {
	ring := newGatedRing()
	sink := &fakeSink{}
	rig := startType103Rig(t, gatedRings{ring: ring}, fakeSinks{sink: sink})
	t.Cleanup(ring.release)

	frames := bufferedALAC352Frames(2)
	const base uint32 = 120_000
	if err := sendBufferedPacket(rig.data, rig.shk, 0, base, bufferedSSRCALAC48, frames[0]); err != nil {
		t.Fatalf("first packet: %v", err)
	}
	rig.waitQueued(t, 1)
	rig.playAnchor(t, base)
	waitForBuffered(t, "the sink to receive PCM", func() bool {
		written, _ := sink.state()
		return written > 0
	})
	waitUntil(t, "the acquired ring presentation", func() bool {
		rig.owner.mu.Lock()
		defer rig.owner.mu.Unlock()
		return rig.owner.ring != nil
	})

	// A second sender's ANNOUNCE takes the session over.
	nextConn := dialTCPStream(t, rig.mgr.advertisedPort())
	defer nextConn.Close()
	nextClient := newRTSPClient(nextConn)
	announce(t, nextClient)
	waitUntil(t, "the successor to own the session", func() bool {
		active := rig.mgr.Active()
		return active != nil && active != rig.owner
	})
	select {
	case <-ring.closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown never reached the ring release")
	}
	if _, aborted := sink.state(); !aborted {
		t.Fatal("the superseded audio sink was still live while the ring release was blocked")
	}
	if ring.wasClosed() {
		t.Fatal("the ring reported closed before the harness released it")
	}
}

func (r *type103Rig) sendRate(t *testing.T, rate int64) {
	t.Helper()
	body, err := plist.EncodeBinary(map[string]any{"rate": rate})
	if err != nil {
		t.Fatalf("encode rate %d: %v", rate, err)
	}
	response, err := r.client.do("SETRATEANCHORTIME", "/1", "application/x-apple-binary-plist", body)
	if err != nil || response.status != 200 {
		t.Fatalf("SETRATEANCHORTIME rate %d = %v %v", rate, response, err)
	}
}

func (r *type103Rig) immediateFlush() error {
	body, err := plist.EncodeBinary(map[string]any{})
	if err != nil {
		return fmt.Errorf("encode immediate flush: %w", err)
	}
	response, err := r.client.do("FLUSHBUFFERED", "/1", "application/x-apple-binary-plist", body)
	if err != nil {
		return err
	}
	if response.status != 200 {
		return fmt.Errorf("FLUSHBUFFERED immediate = %d", response.status)
	}
	return nil
}

func (r *type103Rig) sendImmediateFlush(t *testing.T) {
	t.Helper()
	if err := r.immediateFlush(); err != nil {
		t.Fatalf("FLUSHBUFFERED immediate: %v", err)
	}
}

// startPlayingType103 brings one type-103 stream to PLAYING through a real
// decoded PCM write and returns the frame/base used for its first record.
func startPlayingType103(t *testing.T) (*type103Rig, *reopeningSinks, []byte, uint32) {
	t.Helper()
	sinks := &reopeningSinks{}
	rig := startType103Rig(t, fakeRings{ring: &fakeRing{}}, sinks)
	frames := bufferedALAC352Frames(2)
	const base uint32 = 150_000
	if err := sendBufferedPacket(rig.data, rig.shk, 0, base, bufferedSSRCALAC48, frames[0]); err != nil {
		t.Fatalf("first packet: %v", err)
	}
	rig.waitQueued(t, 1)
	rig.playAnchor(t, base)
	waitForBuffered(t, "the first decoded PCM", func() bool {
		written, _ := sinks.state()
		return written > 0
	})
	waitForBuffered(t, "the player to report playing", func() bool {
		return playbackState(rig.board) == string(media.StatePlaying)
	})
	return rig, sinks, frames[1], base
}

// TestBufferedType103ResumeWithoutFreshPCMStaysBuffering covers both transport
// boundaries. A successful write before the boundary belongs to the retired
// generation, so an explicit Playing report after a pause or immediate flush
// shows buffering until a fresh anchor and PCM arrive.
func TestBufferedType103ResumeWithoutFreshPCMStaysBuffering(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		rig, sinks, frame, base := startPlayingType103(t)
		rig.sendRate(t, 0)
		waitForBuffered(t, "the transport pause", func() bool {
			return playbackState(rig.board) == string(media.StatePaused)
		})
		sendPlaybackState(t, rig.client, 1)
		waitForBuffered(t, "buffering after the report", func() bool {
			return playbackState(rig.board) == string(media.StateBuffering)
		})
		if got := playbackState(rig.board); got == string(media.StatePlaying) {
			t.Fatal("an explicit playing report repainted a paused generation with no PCM")
		}

		before, _ := sinks.state()
		next := base + 352
		if err := sendBufferedPacket(rig.data, rig.shk, 1, next, bufferedSSRCALAC48, frame); err != nil {
			t.Fatalf("post-pause packet: %v", err)
		}
		rig.waitQueued(t, 1)
		rig.playAnchor(t, next)
		waitForBuffered(t, "fresh PCM after the pause", func() bool {
			written, _ := sinks.state()
			return written > before
		})
		waitForBuffered(t, "playing after fresh PCM", func() bool {
			return playbackState(rig.board) == string(media.StatePlaying)
		})
	})

	t.Run("flush", func(t *testing.T) {
		rig, sinks, frame, base := startPlayingType103(t)
		rig.sendImmediateFlush(t)
		waitForBuffered(t, "the immediate flush pause", func() bool {
			return playbackState(rig.board) == string(media.StatePaused)
		})
		sendPlaybackState(t, rig.client, 1)
		waitForBuffered(t, "buffering after the report", func() bool {
			return playbackState(rig.board) == string(media.StateBuffering)
		})
		if got := playbackState(rig.board); got == string(media.StatePlaying) {
			t.Fatal("an explicit playing report repainted a flushed generation with no PCM")
		}

		before, _ := sinks.state()
		next := base + 352
		if err := sendBufferedPacket(rig.data, rig.shk, 1, next, bufferedSSRCALAC48, frame); err != nil {
			t.Fatalf("post-flush packet: %v", err)
		}
		rig.waitQueued(t, 1)
		rig.playAnchor(t, next)
		waitForBuffered(t, "fresh PCM after the flush", func() bool {
			written, _ := sinks.state()
			return written > before
		})
		waitForBuffered(t, "playing after fresh PCM", func() bool {
			return playbackState(rig.board) == string(media.StatePlaying)
		})
	})
}

// stageAdditionalType103 stages one more type-103 stream on the active session
// and connects its data channel without sending PCM.
func stageAdditionalType103(t *testing.T, rig *type103Rig) (*audioStream, *realStream) {
	t.Helper()
	shk := make([]byte, 32)
	if _, err := rand.Read(shk); err != nil {
		t.Fatalf("second shk: %v", err)
	}
	body, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeBuffered),
			"audioFormat": int64(bufferedFormatALAC48S24Stereo),
			"spf":         int64(352),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode second SETUP: %v", err)
	}
	setup, err := rig.client.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err != nil || setup.status != 200 {
		t.Fatalf("second type-103 SETUP = %v %v", setup, err)
	}
	decoded, err := plist.Decode(setup.body)
	if err != nil {
		t.Fatalf("decode second SETUP: %v", err)
	}
	dict, _ := decoded.(map[string]any)
	streams, _ := dict["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("second SETUP streams = %v", dict["streams"])
	}
	streamDict, _ := streams[0].(map[string]any)
	dataPort := int(plistUint(streamDict["dataPort"]))
	if dataPort == 0 {
		t.Fatalf("second SETUP descriptor has no dataPort: %v", streamDict)
	}
	data := dialTCPStream(t, dataPort)
	t.Cleanup(func() { _ = data.Close() })
	waitUntil(t, "the second type-103 stream", func() bool {
		return len(rig.owner.activeStreams()) == 2
	})
	live := rig.owner.activeStreams()[1]
	if live.buffered == nil {
		t.Fatal("the second stream is not buffered")
	}
	waitUntil(t, "the second data connection", func() bool {
		live.buffered.mu.Lock()
		defer live.buffered.mu.Unlock()
		return live.buffered.connID != 0
	})
	return live, data
}

// TestBufferedSecondStreamConnectionPreservesPlayingEvidence proves the PCM
// evidence is per stream. Opening the second type-103 data channel must not
// erase the first stream's current evidence, so an explicit playing report
// stays playing instead of falling back to buffering.
func TestBufferedSecondStreamConnectionPreservesPlayingEvidence(t *testing.T) {
	rig, _, _, _ := startPlayingType103(t)
	if got := playbackState(rig.board); got != string(media.StatePlaying) {
		t.Fatalf("first stream state = %q, want playing", got)
	}
	stageAdditionalType103(t, rig)
	sendPlaybackState(t, rig.client, 1)
	if got := playbackState(rig.board); got != string(media.StatePlaying) {
		t.Fatalf("second data channel cleared the first stream's evidence: %q", got)
	}
}

func playbackStates(events <-chan map[string]any) []string {
	var states []string
	for {
		select {
		case event := <-events:
			updated, _ := event["updated"].(map[string]any)
			if state, ok := updated["playbackState"].(string); ok {
				states = append(states, state)
			}
		default:
			return states
		}
	}
}

func assertNoPlayingAfterPause(t *testing.T, states []string) {
	t.Helper()
	paused := false
	for _, state := range states {
		switch {
		case state == string(media.StatePaused):
			paused = true
		case state == string(media.StatePlaying) && paused:
			t.Fatalf("playing republished after pause: %v", states)
		}
	}
	if !paused {
		t.Fatalf("flush never published paused: %v", states)
	}
}

// TestBufferedImmediateFlushRetiresBeforePause pins the WatchPlayers order.
// FLUSHBUFFERED must retire every affected generation before it publishes the
// pause, so a completed stale write cannot produce PAUSED→PLAYING→PAUSED. The
// no-PCM case must still publish the pause.
func TestBufferedImmediateFlushRetiresBeforePause(t *testing.T) {
	t.Run("stale completion", func(t *testing.T) {
		rig, sinks, frame, base := startPlayingType103(t)
		_, events, unsubscribe := rig.board.Subscribe()
		defer unsubscribe()

		stream := rig.stream.buffered
		stream.mu.Lock()
		before := stream.generation
		stream.mu.Unlock()
		beforeBytes, _ := sinks.state()
		// Hold publication so the flush must retire the generation before its
		// pause can become visible; the in-flight completion is queued behind
		// the same lock.
		rig.owner.publishMu.Lock()
		locked := true
		defer func() {
			if locked {
				rig.owner.publishMu.Unlock()
			}
		}()
		if err := sendBufferedPacket(rig.data, rig.shk, 1, base+352, bufferedSSRCALAC48, frame); err != nil {
			t.Fatalf("second packet: %v", err)
		}
		if err := waitForBufferedState(t, "the in-flight write", func() bool {
			written, _ := sinks.state()
			stream.mu.Lock()
			writing := stream.writing
			stream.mu.Unlock()
			return written > beforeBytes && writing
		}); err != nil {
			t.Fatal(err)
		}
		flushDone := make(chan error, 1)
		go func() { flushDone <- rig.immediateFlush() }()
		if err := waitForBufferedState(t, "the flush generation retirement", func() bool {
			stream.mu.Lock()
			defer stream.mu.Unlock()
			return stream.generation != before
		}); err != nil {
			t.Fatal(err)
		}
		rig.owner.publishMu.Unlock()
		locked = false
		if err := <-flushDone; err != nil {
			t.Fatalf("immediate flush: %v", err)
		}
		waitForBuffered(t, "the flushed pause", func() bool {
			return playbackState(rig.board) == string(media.StatePaused)
		})
		time.Sleep(50 * time.Millisecond)
		assertNoPlayingAfterPause(t, playbackStates(events))
	})

	t.Run("no PCM", func(t *testing.T) {
		sinks := &reopeningSinks{}
		rig := startType103Rig(t, fakeRings{ring: &fakeRing{}}, sinks)
		_, events, unsubscribe := rig.board.Subscribe()
		defer unsubscribe()
		rig.sendImmediateFlush(t)
		waitForBuffered(t, "the no-PCM pause", func() bool {
			return playbackState(rig.board) == string(media.StatePaused)
		})
		assertNoPlayingAfterPause(t, playbackStates(events))
	})
}

func waitForBufferedState(t *testing.T, what string, condition func() bool) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", what)
}

// TestBufferedStaleWriteCompletionCannotRepaintPaused drives a real transport
// pause after PCM has played, then delivers the completion of the packet whose
// generation the pause retired. The stale completion must be rejected and the
// board must remain paused.
func TestBufferedStaleWriteCompletionCannotRepaintPaused(t *testing.T) {
	rig, _, _, _ := startPlayingType103(t)
	stream := rig.stream.buffered
	stream.mu.Lock()
	staleGeneration := stream.generation
	staleSink := stream.sink
	stream.mu.Unlock()
	if staleSink == nil {
		t.Fatal("the playing stream has no sink")
	}

	rig.sendRate(t, 0)
	waitForBuffered(t, "the transport pause", func() bool {
		return playbackState(rig.board) == string(media.StatePaused)
	})
	rig.owner.mu.Lock()
	ready := rig.owner.bufferedPCMReadyLocked()
	rig.owner.mu.Unlock()
	if ready {
		t.Fatal("the pause left the previous generation's PCM evidence live")
	}
	first, committed, err := stream.commitWrite(staleGeneration, 0, staleSink, nil)
	if err != nil {
		t.Fatalf("stale completion: %v", err)
	}
	if first || committed {
		t.Fatal("a write from the retired generation committed after the pause")
	}
	rig.owner.mu.Lock()
	ready = rig.owner.bufferedPCMReadyLocked()
	rig.owner.mu.Unlock()
	if ready {
		t.Fatal("the stale completion restored the previous generation's PCM evidence")
	}
	if got := playbackState(rig.board); got != string(media.StatePaused) {
		t.Fatalf("stale completion repainted the board as %q", got)
	}
}
