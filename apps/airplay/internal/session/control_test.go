package session

// AP2 control plane: the sender's anchor (0xD7), our resend request (0xD5) and
// the sender's resend response (0xD6). The packets are built from the reference
// layout (Shairport Sync rtp.c), not from this package's encoder, so the test
// pins the wire format rather than a round trip.

import (
	"crypto/rand"
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/plist"
)

func encodeRTP(aead interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
}, sequence uint16, timestamp, ssrc uint32, plaintext []byte) []byte {
	header := make([]byte, 12)
	header[0] = 0x80
	header[1] = byte(streamTypeRealtime)
	binary.BigEndian.PutUint16(header[2:4], sequence)
	binary.BigEndian.PutUint32(header[4:8], timestamp)
	binary.BigEndian.PutUint32(header[8:12], ssrc)
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], uint64(sequence)+1)
	ciphertext := aead.Seal(nil, nonce, plaintext, header[4:12])
	packet := append(append(append([]byte{}, header...), ciphertext...), nonce[4:]...)
	return packet
}

func TestAP2ControlAnchorResendAndResponse(t *testing.T) {
	mgr, transport, _, _, sink, clientKey, pairingID := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	conn, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := newRTSPClient(conn)
	shared := pairVerify(t, client, pairingID, clientKey)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(conn, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("client channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})

	shk := make([]byte, 32)
	if _, err := rand.Read(shk); err != nil {
		t.Fatalf("shk: %v", err)
	}
	body, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeRealtime),
			"audioFormat": int64(1 << 11), // PCM 44.1 kHz stereo
			"spf":         int64(352),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err != nil || setup.status != 200 {
		t.Fatalf("SETUP = %v %v", setup, err)
	}
	decoded, err := plist.Decode(setup.body)
	if err != nil {
		t.Fatalf("decode SETUP: %v", err)
	}
	dict, _ := decoded.(map[string]any)
	streams, _ := dict["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("SETUP streams = %v", dict["streams"])
	}
	streamDict, _ := streams[0].(map[string]any)
	dataPort := int(plistUint(streamDict["dataPort"]))
	controlPort := int(plistUint(streamDict["controlPort"]))
	if dataPort == 0 || controlPort == 0 {
		t.Fatalf("SETUP ports data=%d control=%d", dataPort, controlPort)
	}
	sendAudio, err := transport.sendUDP(dataPort)
	if err != nil {
		t.Fatalf("audio socket: %v", err)
	}
	sendControl, err := transport.sendUDP(controlPort)
	if err != nil {
		t.Fatalf("control socket: %v", err)
	}

	// 0xD7 anchor: flags/sentinel, type 215, frame_1 at 4, master time at 8,
	// frame_2 at 16, clock id at 20 (all big-endian).
	anchor := make([]byte, 28)
	anchor[0] = 0x10
	anchor[1] = 0xd7
	frame1 := uint32(1000)
	frame2 := uint32(1000 + 77175)
	expectedAnchorFrame := frame1 - uint32(ap2RealTimeStreamLatencyFudge)
	binary.BigEndian.PutUint32(anchor[4:8], frame1)
	binary.BigEndian.PutUint64(anchor[8:16], 5_000_000_000)
	binary.BigEndian.PutUint32(anchor[16:20], frame2)
	binary.BigEndian.PutUint64(anchor[20:28], 0x1122334455667788)
	if err := sendControl(anchor); err != nil {
		t.Fatalf("anchor datagram: %v", err)
	}

	if response, err := client.do("RECORD", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("RECORD = %v %v", response, err)
	}

	aead, err := chacha20poly1305.New(shk)
	if err != nil {
		t.Fatalf("aead: %v", err)
	}
	samples := make([]byte, 352*2*2)
	for i := 0; i < 352*2; i++ {
		binary.LittleEndian.PutUint16(samples[i*2:], 800)
	}

	// Deliver the anchor to the stream once the stream exists, then check the
	// latency the sender declared (frame_2 - frame_1) was recorded.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if active := mgr.Active(); active != nil && len(active.activeStreams()) == 1 {
			stream := active.activeStreams()[0]
			stream.mu.Lock()
			latency := stream.latencyFrames
			anchorFrame := stream.anchorFrame
			stream.mu.Unlock()
			if latency == 77175 && anchorFrame == expectedAnchorFrame {
				break
			}
		}
		// The control socket ignores packets whose sentinel bit is clear in the
		// first two seconds, and our anchor has it set, so re-sending is safe.
		_ = sendControl(anchor)
		time.Sleep(20 * time.Millisecond)
	}
	stream := mgr.Active().activeStreams()[0]
	stream.mu.Lock()
	latency, anchorFrame := stream.latencyFrames, stream.anchorFrame
	stream.mu.Unlock()
	if latency != 77175 {
		t.Fatalf("stream latency = %d, want 77175 frames", latency)
	}
	if anchorFrame != expectedAnchorFrame {
		t.Fatalf("anchor frame = %d, want %d", anchorFrame, expectedAnchorFrame)
	}

	// Push 0, 1 and 3 so sequence 2 is missing, then expect one 0xD5 request.
	for _, sequence := range []uint16{0, 1, 3} {
		packet := encodeRTP(aead, sequence, uint32(sequence)*352, 0x01020304, samples)
		if err := sendAudio(packet); err != nil {
			t.Fatalf("audio %d: %v", sequence, err)
		}
	}
	var request []byte
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, sent := range transport.sentUDP(controlPort) {
			if len(sent) == 8 && sent[0] == 0x80 && sent[1] == 0xd5 {
				request = sent
			}
		}
		if request != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if request == nil {
		t.Fatal("no 0xD5 resend request was sent for the missing sequence")
	}
	if got := binary.BigEndian.Uint16(request[2:4]); got != 1 {
		t.Fatalf("resend request sequence = %d, want 1", got)
	}
	if got := binary.BigEndian.Uint16(request[4:6]); got != 2 {
		t.Fatalf("resend request first = %d, want 2", got)
	}
	if got := binary.BigEndian.Uint16(request[6:8]); got != 1 {
		t.Fatalf("resend request count = %d, want 1", got)
	}

	// The sender answers with 0xD6: a four-byte control header, then the whole
	// embedded RTP packet, header and payload type included.
	full := encodeRTP(aead, 2, 2*352, 0x01020304, samples)
	response := make([]byte, 4+len(full))
	response[0] = 0x10
	response[1] = 0xd6
	copy(response[4:], full)
	if err := sendControl(response); err != nil {
		t.Fatalf("resend response: %v", err)
	}

	// Advance the stream so the reorder window releases the buffered frames:
	// the receiver holds audio back by its target cushion.
	for sequence := uint16(4); sequence <= 10; sequence++ {
		packet := encodeRTP(aead, sequence, uint32(sequence)*352, 0x01020304, samples)
		if err := sendAudio(packet); err != nil {
			t.Fatalf("audio %d: %v", sequence, err)
		}
	}

	deadline = time.Now().Add(3 * time.Second)
	written := 0
	for time.Now().Before(deadline) {
		written, _ = sink.state()
		if written >= 3*len(samples) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if written < 3*len(samples) {
		t.Fatalf("sink received %d bytes, want the three frames including the resent one (%d)", written, 3*len(samples))
	}
	if response, err := client.do("TEARDOWN", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", response, err)
	}
}
