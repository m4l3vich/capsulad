package linuxdev

import (
	"bytes"
	"testing"

	"capsulad/internal/backend"
)

// Deterministic waveform fixture, byte-for-byte what ringled emits:
// `--dump --reset 0 --pio --count 1 raw 800000` prints
// 88 88 88 88 | 8c 88 88 88 | 88 88 88 88. ringled's default brightness is
// 0x7f/255, so R=0x80 scales to 0x40; with MSB-first bits and 0b1100/0b1000
// symbols 0x40 is 0x8C followed by 0x88s, and GRB order puts it in the middle.
func TestEncodeSPIWS2812Fixture(t *testing.T) {
	linear := BuildGammaTable(1.0)
	out := encodeSPIWS2812([]backend.RGB{{R: 0x80}}, 0.5, 1, linear, 8)
	want := make([]byte, 0, 128+12+128)
	want = append(want, make([]byte, 128)...)
	want = append(want, 0x88, 0x88, 0x88, 0x88) // G = 0x00
	want = append(want, 0x8C, 0x88, 0x88, 0x88) // R = 0x40 after 0.5 brightness
	want = append(want, 0x88, 0x88, 0x88, 0x88) // B = 0x00
	want = append(want, make([]byte, 128)...)
	if !bytes.Equal(out, want) {
		t.Fatalf("waveform drift:\n got %x\nwant %x", out, want)
	}
	if len(out)%4 != 0 {
		t.Fatalf("frame is not a whole number of 32-bit words: %d bytes", len(out))
	}
}

// With 32 bits per word the SPI core clocks the last byte of each group first,
// so the buffer is the PIO stream with each 4-byte group reversed.
func TestEncodeSPIWS2812WordOrder(t *testing.T) {
	linear := BuildGammaTable(1.0)
	pio := encodeSPIWS2812([]backend.RGB{{R: 0x80}}, 1, 1, linear, 8)
	wide := encodeSPIWS2812([]backend.RGB{{R: 0x80}}, 1, 1, linear, 32)
	if len(pio) != len(wide) {
		t.Fatalf("lengths differ: %d vs %d", len(pio), len(wide))
	}
	for base := 128; base < 140; base += 4 {
		want := []byte{pio[base+3], pio[base+2], pio[base+1], pio[base]}
		if !bytes.Equal(wide[base:base+4], want) {
			t.Fatalf("word at %d = %x, want %x", base, wide[base:base+4], want)
		}
	}
}

// wireOrder emulates the SPI core clocking a buffer out MSB-first at a given
// word size: on a little-endian host the last byte of each word reaches the wire
// first. The 8-bit buffer is the reference wire sequence.
func wireOrder(buf []byte, wordBits int) []byte {
	out := append([]byte(nil), buf...)
	width := wordBits / 8
	if width > 1 {
		for i := 0; i+width <= len(out); i += width {
			for l, r := i, i+width-1; l < r; l, r = l+1, r-1 {
				out[l], out[r] = out[r], out[l]
			}
		}
	}
	return out
}

// The 32-bit buffer, after kernel wire conversion, must be exactly the 8-bit
// wire stream. This pins the convention rather than the intended byte order:
// ringled's own --dump agrees (its --pio stream 8C 88 88 88 appears as
// 88 88 88 8C in the default 32-bit buffer).
func TestSPIWireOrderMatchesPIO(t *testing.T) {
	linear := BuildGammaTable(1.0)
	pixels := []backend.RGB{{R: 0x80, G: 0x01, B: 0x7f}, {R: 0x00, G: 0xff, B: 0x10}}
	pio := encodeSPIWS2812(pixels, 1, 2, linear, 8)
	wide := encodeSPIWS2812(pixels, 1, 2, linear, 32)
	if got := wireOrder(wide, 32); !bytes.Equal(got, pio) {
		t.Fatalf("32-bit buffer does not clock out as the 8-bit stream:\n got %x\nwant %x", got, pio)
	}
}
