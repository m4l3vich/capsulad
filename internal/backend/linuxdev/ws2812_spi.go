package linuxdev

import (
	"sync"

	"capsulad/internal/backend"
)

// SPIRing drives a WS2812B chain from userspace through spidev. Each WS2812B bit
// is four SPI bits (0 -> 0b1000, 1 -> 0b1100, the table ringled emits) so a
// ~1.2 us symbol is emitted at
// the board's 3.34 MHz clock, and the frame ends with a low reset gap. The
// Amlogic Alpine image's own ringled helper uses the same spidev node and clock.
type SPIRing struct {
	mu       sync.Mutex
	dev      spiDevice
	pixels   int
	gamma    [256]uint8
	speedHz  int
	primeHz  int
	achieved int
	wordBits int
}

// spiDevice is the small kernel surface this driver needs; the real
// implementation is in ws2812_spi_linux.go, and the non-Linux stub refuses.
type spiDevice interface {
	Transfer(p []byte) error
	SetSpeed(hz int) error
	SpeedHz() (int, error)
	Close() error
}

// OpenSPIRing opens a spidev node and configures it for a WS2812B chain. When
// primeHz is non-zero every frame is preceded by a short transfer at that rate
// so the clock divider is reprogrammed (ringled's --prime mechanism; SetSpeed
// also drives spi_setup()).
func OpenSPIRing(path string, pixels, speedHz, bitsPerWord, primeHz int, gamma float64) (*SPIRing, error) {
	dev, err := openSPIDevice(path, speedHz, bitsPerWord)
	if err != nil {
		return nil, err
	}
	if pixels <= 0 {
		pixels = 12
	}
	achieved, _ := dev.SpeedHz()
	return &SPIRing{dev: dev, pixels: pixels, gamma: BuildGammaTable(gamma), speedHz: speedHz, primeHz: primeHz, achieved: achieved, wordBits: bitsPerWord}, nil
}

func (r *SPIRing) Caps() backend.RingCaps {
	return backend.RingCaps{PixelCount: r.pixels, SupportsRGB: true}
}

func (r *SPIRing) ResourceID() string { return "light.ring.main" }

func (r *SPIRing) WriteRing(f backend.RingFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primeHz > 0 {
		// ringled's primer "forces the divider to be reprogrammed"; running it
		// before every frame keeps the divider correct without rebinding the
		// controller.
		if err := r.dev.SetSpeed(r.primeHz); err != nil {
			return err
		}
		if err := r.dev.Transfer([]byte{0, 0, 0, 0}); err != nil {
			return err
		}
		if err := r.dev.SetSpeed(r.speedHz); err != nil {
			return err
		}
	}
	return r.dev.Transfer(encodeSPIWS2812(f.Pixels, f.Brightness, r.pixels, r.gamma, r.wordBits))
}

// ConfiguredSpeedHz reports the configured max_speed_hz read back at open. The
// achieved divider is not observable from userspace, so this is not a timing
// measurement.
func (r *SPIRing) ConfiguredSpeedHz() int { return r.achieved }

func (r *SPIRing) Close() error { return r.dev.Close() }

// encodeSPIWS2812 emits GRB per pixel, four SPI bits per WS2812B bit, then a
// reset gap. Logical pixel order is used as-is: the physical order on a given
// unit is a hardware check, not something this encoder claims to know.
func encodeSPIWS2812(pixels []backend.RGB, brightness float64, count int, gamma [256]uint8, wordBits int) []byte {
	level := ClampBrightness(brightness)
	channels := make([]byte, 0, count*3)
	for i := 0; i < count; i++ {
		var px backend.RGB
		if i < len(pixels) {
			px = pixels[i]
		}
		channels = append(channels,
			gamma[ScaleChannel(px.G, level)],
			gamma[ScaleChannel(px.R, level)],
			gamma[ScaleChannel(px.B, level)])
	}
	out := make([]byte, 0, len(channels)*4+64)
	var acc uint16
	bits := 0
	for _, v := range channels {
		for b := 7; b >= 0; b-- {
			if v&(1<<uint(b)) != 0 {
				acc = acc<<4 | 0b1100
			} else {
				acc = acc<<4 | 0b1000
			}
			bits += 4
			if bits == 8 {
				out = append(out, byte(acc))
				acc, bits = 0, 0
			}
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(8-bits)))
	}
	// ringled's default --reset 128: 128 zero bytes either side of the frame,
	// about 300 us each at 3.34 MHz and a whole number of 32-bit words.
	if wordBits == 0 || wordBits%8 != 0 {
		wordBits = 8
	}
	if wordBits > 8 {
		out = reverseWords(out, wordBits/8)
	}
	prefix := make([]byte, 128)
	out = append(out, make([]byte, 128)...)
	return append(prefix, out...)
}

// reverseWords reverses each memory word, because the SPI core clocks a
// multi-byte word MSB-first: on a little-endian host the last byte of the group
// reaches the wire first. ringled's own default (non---pio) buffer shows the
// same per-word reversal (its PIO stream 8C 88 88 88 appears as 88 88 88 8C).
func reverseWords(b []byte, width int) []byte {
	for i := 0; i+width <= len(b); i += width {
		for l, r := i, i+width-1; l < r; l, r = l+1, r-1 {
			b[l], b[r] = b[r], b[l]
		}
	}
	return b
}
