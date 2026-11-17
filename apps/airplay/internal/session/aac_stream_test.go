//go:build ffmpeg

package session

// End-to-end proof that the AAC-LC flow upstream advertises actually plays:
// pair-verify, SETUP with the AAC-LC/44.1K/2 channel format mask, encrypted RTP
// carrying real AAC access units from an ffmpeg-encoded fixture, and PCM
// arriving at the framework sink.

import (
	"crypto/rand"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/plist"
)

// splitADTS returns the raw access units of an ADTS fixture.
func splitADTS(data []byte) ([][]byte, error) {
	var frames [][]byte
	for offset := 0; offset+7 <= len(data); {
		if data[offset] != 0xFF || data[offset+1]&0xF0 != 0xF0 {
			return nil, errBadADTS
		}
		headerLen := 7
		if data[offset+1]&0x01 == 0 {
			headerLen = 9
		}
		length := int(data[offset+3]&0x03)<<11 | int(data[offset+4])<<3 | int(data[offset+5]>>5)
		if length < headerLen || offset+length > len(data) {
			return nil, errBadADTS
		}
		frames = append(frames, data[offset+headerLen:offset+length])
		offset += length
	}
	return frames, nil
}

type adtsError struct{}

func (adtsError) Error() string { return "malformed ADTS fixture" }

var errBadADTS = adtsError{}

func TestAACStreamPlays(t *testing.T) {
	fixture, err := os.ReadFile("../aac/testdata/aac-lc-44100-stereo.adts")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	frames, err := splitADTS(fixture)
	if err != nil {
		t.Fatalf("split ADTS: %v", err)
	}

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
			"audioFormat": int64(1 << 22), // AAC-LC 44.1 kHz F24 stereo
			"spf":         int64(1024),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	if setup.status != 200 {
		t.Fatalf("SETUP status = %d, body %s", setup.status, setup.body)
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
	if dataPort == 0 {
		t.Fatalf("SETUP dataPort = %v", streamDict["dataPort"])
	}
	if response, err := client.do("RECORD", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("RECORD = %v %v", response, err)
	}

	sendAudio, err := transport.sendUDP(dataPort)
	if err != nil {
		t.Fatalf("audio socket: %v", err)
	}
	aead, err := chacha20poly1305.New(shk)
	if err != nil {
		t.Fatalf("aead: %v", err)
	}
	var timestamp uint32 = 500
	for seq, frame := range frames {
		header := make([]byte, 12)
		header[0] = 0x80
		header[1] = byte(streamTypeRealtime)
		binary.BigEndian.PutUint16(header[2:4], uint16(seq))
		binary.BigEndian.PutUint32(header[4:8], timestamp)
		binary.BigEndian.PutUint32(header[8:12], 0x55667788)
		nonce := make([]byte, 12)
		binary.LittleEndian.PutUint64(nonce[4:], uint64(seq)+1)
		ciphertext := aead.Seal(nil, nonce, frame, header[4:12])
		packet := append(append(append([]byte{}, header...), ciphertext...), nonce[4:]...)
		if err := sendAudio(packet); err != nil {
			t.Fatalf("audio datagram: %v", err)
		}
		timestamp += 1024
	}

	deadline := time.Now().Add(5 * time.Second)
	bytesWritten := 0
	for time.Now().Before(deadline) {
		bytesWritten, _ = sink.state()
		// 4096-sample AAC frames at 44.1 kHz: a few frames are enough proof.
		if bytesWritten >= 2048 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if bytesWritten < 2048 {
		t.Fatalf("the sink received %d bytes of AAC-derived PCM, want at least 2048", bytesWritten)
	}
	if bytesWritten%4 != 0 {
		t.Fatalf("PCM length %d is not a whole number of stereo s16 frames", bytesWritten)
	}
	if !mgr.board.Live() {
		t.Fatal("the player is not live while AAC audio plays")
	}
	if response, err := client.do("TEARDOWN", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", response, err)
	}
}
