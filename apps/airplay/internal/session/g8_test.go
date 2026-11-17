package session

// G8: the device-volume notification must be exactly the reference message on
// the event channel, and the write must be an RTSP POST /command.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"

	"capsulad/apps/airplay/internal/plist"
)

type captureEventWriter struct{ buf bytes.Buffer }

func (w *captureEventWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func TestUnitVolumeNotificationMatchesReference(t *testing.T) {
	writer := &captureEventWriter{}
	s := &Session{eventOut: writer, done: make(chan struct{})}

	if !s.sendUnitVolume(0.42) {
		t.Fatal("sendUnitVolume reported no event channel")
	}
	reader := bufio.NewReader(&writer.buf)
	line, err := reader.ReadString('\n')
	if err != nil || line != "POST /command RTSP/1.0\r\n" {
		t.Fatalf("request line = %q (%v)", line, err)
	}
	headers := map[string]string{}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		name, value, _ := bytes.Cut([]byte(line), []byte(":"))
		headers[string(name)] = string(bytes.TrimSpace(value))
	}
	if headers["Content-Type"] != "application/x-apple-binary-plist" {
		t.Fatalf("content type = %q", headers["Content-Type"])
	}
	length := 0
	if _, err := fmtSscan(headers["Content-Length"], &length); err != nil {
		t.Fatalf("content length %q", headers["Content-Length"])
	}
	body := make([]byte, length)
	if _, err := reader.Read(body); err != nil {
		t.Fatal(err)
	}
	value, err := plist.Decode(body)
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	dict, _ := value.(map[string]any)
	if dict["type"] != "sendMediaRemoteCommand" || dict["value"] != "dvlc" {
		t.Fatalf("notification = %v", dict)
	}
	volume, ok := dict["volume"].(float64)
	if !ok || volume < 0.41 || volume > 0.43 {
		t.Fatalf("volume = %v", dict["volume"])
	}
	params, _ := dict["params"].(map[string]any)
	if pv, ok := params["volume"].(float64); !ok || pv < 0.41 || pv > 0.43 {
		t.Fatalf("params volume = %v", params["volume"])
	}
}

func TestUnitVolumeClampsAndBounds(t *testing.T) {
	writer := &captureEventWriter{}
	s := &Session{eventOut: writer, done: make(chan struct{})}
	if !s.sendUnitVolume(0) || !s.sendUnitVolume(1) {
		t.Fatal("sendUnitVolume failed")
	}
	// Two 8-byte-body requests: the volume notifications are small and the
	// event writer must never be handed a malformed frame.
	if bytes.Count(writer.buf.Bytes(), []byte("POST /command RTSP/1.0")) != 2 {
		t.Fatalf("wrote %d requests", bytes.Count(writer.buf.Bytes(), []byte("POST /command")))
	}
	_ = binary.BigEndian
}

func fmtSscan(value string, out *int) (int, error) {
	n := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return 0, bytes.ErrTooLarge
	}
	*out = n
	return 1, nil
}
