package session

// Cover art must reach the LED ring as the sender's palette, not as the dim
// neutral breath the receiver shows when it has no image. This drives the
// legacy RAOP path end to end over the in-memory transport: ANNOUNCE, SETUP,
// RECORD and a real PNG SET_PARAMETER, then inspects the frames the ring
// factory received.

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"
)

func TestCoverArtPaletteDrivesTheRing(t *testing.T) {
	mgr, transport, _, ring, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()
	announce(t, client)
	waitUntil(t, "the announced session", func() bool { return mgr.Active() != nil })

	setup, err := client.doHeaders("SETUP", "/1", nil,
		"Transport: RTP/AVP/TCP;unicast;interleaved=0-1")
	if err != nil || setup.status != 200 {
		t.Fatalf("SETUP = %v %v", setup, err)
	}
	record, err := client.do("RECORD", "/1", "", nil)
	if err != nil || record.status != 200 {
		t.Fatalf("RECORD = %v %v", record, err)
	}

	// A real PNG: left half red, right half blue. The palette is therefore
	// two colours in that order, and the 12-pixel gradient starts red and ends
	// blue.
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for y := range 96 {
		for x := range 96 {
			shade := color.RGBA{R: 255, A: 255}
			if x >= 48 {
				shade = color.RGBA{B: 255, A: 255}
			}
			img.SetRGBA(x, y, shade)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	response, err := client.do("SET_PARAMETER", "/1", "image/png", encoded.Bytes())
	if err != nil || response.status != 200 {
		t.Fatalf("SET_PARAMETER cover art = %v %v", response, err)
	}

	// Without audio the envelope leaves the ring at its floor (0.12), so the
	// palette's red and blue arrive as roughly 31 per channel. A neutral
	// breath would have equal channels across the whole ring.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frame := ring.lastFrame()
		if len(frame) == 36 &&
			frame[0] > 20 && frame[1] < 8 && frame[2] < 8 &&
			frame[33] < 8 && frame[34] < 8 && frame[35] > 20 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	frame := ring.lastFrame()
	if len(frame) != 36 {
		t.Fatalf("ring frame length = %d, want 36", len(frame))
	}
	if frame[0] <= 20 || frame[1] >= 8 || frame[2] >= 8 {
		t.Fatalf("pixel 0 = %v, want the cover art's red end", frame[0:3])
	}
	if frame[33] >= 8 || frame[34] >= 8 || frame[35] <= 20 {
		t.Fatalf("pixel 11 = %v, want the cover art's blue end", frame[33:36])
	}

	if response, err := client.do("TEARDOWN", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", response, err)
	}
	waitUntil(t, "the ring presentation to close", func() bool {
		_, closed := ring.snapshot()
		return closed
	})
}
