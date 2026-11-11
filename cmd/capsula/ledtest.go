package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/backend/rockchip"
)

// ledFrame is the vendor voice-agent frame format: a duration and one
// brightness value per display channel.
type ledFrame struct {
	Duration   float64 `json:"duration"`
	Brightness []int   `json:"brightness"`
}

// runLEDTest drives the IS31FL3236 display outside the daemon so the physical
// channel order can be calibrated. It is a bring-up tool: it writes raw
// per-channel brightness and never goes through routing or presets.
func runLEDTest(args []string, jsonOut bool) error {
	fs := flag.NewFlagSet("platform ledtest", flag.ContinueOnError)
	ring := fs.Bool("ring", false, "drive the WS2812B ring instead of the segment display")
	path := fs.String("path", "", "vendor device (default /dev/led_multi_ctrl, or /dev/topleds with -ring)")
	color := fs.String("color", "255,0,0", "ring color as R,G,B")
	level := fs.Float64("level", 1, "ring brightness multiplier, 0-1")
	channels := fs.String("channels", "", "channels to light, e.g. 0-6,28,29 (blank clears)")
	value := fs.Int("value", 200, "brightness for -channels and -sweep, 0-255")
	bg := fs.Int("bg", 0, "dim baseline brightness for every other channel, so the display grid stays visible")
	hold := fs.Duration("hold", 4*time.Second, "how long a static pattern stays on")
	sweep := fs.Bool("sweep", false, "light one channel at a time from -from to -to")
	from := fs.Int("from", 0, "first channel for -sweep")
	to := fs.Int("to", 29, "last channel for -sweep")
	step := fs.Duration("step", 1500*time.Millisecond, "dwell per channel for -sweep")
	gap := fs.Duration("gap", 500*time.Millisecond, "dark gap per channel for -sweep")
	jsonPath := fs.String("json", "", "replay a vendor *_digits.json animation")
	mapping := fs.String("map", "identity", "JSON order mapping: identity, vendor, inverse")
	frame := fs.Int("frame", -1, "with -json, show only this frame index")
	loop := fs.Bool("loop", false, "with -json, replay until interrupted")
	speed := fs.Float64("speed", 1, "with -json, playback rate multiplier")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *path == "" {
		if *ring {
			*path = "/dev/topleds"
		} else {
			*path = "/dev/led_multi_ctrl"
		}
	}
	rgb, err := parseRGB(*color)
	if err != nil {
		return err
	}
	count := 30
	if *ring {
		count = 12
	}
	clearRing := func() error { return rockchip.WriteRingPixels(*path, nil, 1) }
	clear := func() {
		var err error
		if *ring {
			err = clearRing()
		} else {
			err = rockchip.WriteRawMultiCtrl(*path, make([]byte, count))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "capsula: clear: %v\n", err)
		}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		clear()
		os.Exit(1)
	}()

	write := func(values []byte) error {
		if jsonOut {
			return nil
		}
		if *ring {
			pixels := make([]backend.RGB, len(values))
			for i, v := range values {
				pixels[i] = backend.RGB{R: v, G: v, B: v}
			}
			return rockchip.WriteRingPixels(*path, pixels, *level)
		}
		return rockchip.WriteRawMultiCtrl(*path, values)
	}

	switch {
	case *jsonPath != "":
		raw, err := os.ReadFile(*jsonPath)
		if err != nil {
			return err
		}
		var frames []ledFrame
		if err := json.Unmarshal(raw, &frames); err != nil {
			return fmt.Errorf("parse %s: %w", *jsonPath, err)
		}
		if *frame >= 0 {
			if *frame >= len(frames) {
				return fmt.Errorf("-frame %d out of range (0-%d)", *frame, len(frames)-1)
			}
			frames = frames[*frame : *frame+1]
		}
		perm := rockchip.VendorMap()
		for pass := 0; ; pass++ {
			for i, fr := range frames {
				out := applyLEDMap(fr.Brightness, *mapping, perm)
				if err := write(out); err != nil {
					return err
				}
				if jsonOut {
					fmt.Printf("frame %d %v\n", i, out)
				}
				time.Sleep(time.Duration(float64(fr.Duration) / *speed * float64(time.Second)))
			}
			if !*loop || len(frames) == 0 {
				break
			}
		}
	case *sweep && *ring:
		if *from < 0 || *to >= count || *from > *to {
			return fmt.Errorf("-from/%d -to/%d out of range 0-%d", *from, *to, count-1)
		}
		for pass := 0; ; pass++ {
			for px := *from; px <= *to; px++ {
				pixels := make([]backend.RGB, count)
				pixels[px] = rgb
				fmt.Printf("pass %d pixel %2d\n", pass, px)
				if err := rockchip.WriteRingPixels(*path, pixels, *level); err != nil {
					return err
				}
				time.Sleep(*step)
				if err := clearRing(); err != nil {
					return err
				}
				time.Sleep(*gap)
			}
			if !*loop {
				break
			}
		}
	case *sweep:
		for pass := 0; ; pass++ {
			for ch := *from; ch <= *to; ch++ {
				buf := make([]byte, count)
				for i := range buf {
					buf[i] = clampByte(*bg)
				}
				buf[ch] = byte(*value)
				fmt.Printf("pass %d channel %2d\n", pass, ch)
				if err := write(buf); err != nil {
					return err
				}
				time.Sleep(*step)
				restoreBaseline(*bg, count, write)
				time.Sleep(*gap)
			}
			if !*loop {
				break
			}
		}
	default:
		if *ring {
			// Pixels listed in -channels light with -color; the rest stay dark.
			pixels := make([]backend.RGB, count)
			list, err := parseChannels(*channels, count)
			if err != nil {
				return err
			}
			for _, px := range list {
				pixels[px] = rgb
			}
			fmt.Printf("pixels %v color %s on %s\n", list, *color, *path)
			if jsonOut {
				return nil
			}
			if err := rockchip.WriteRingPixels(*path, pixels, *level); err != nil {
				return err
			}
			if *hold > 0 {
				time.Sleep(*hold)
				return clearRing()
			}
			return nil
		}
		buf := make([]byte, count)
		for i := range buf {
			buf[i] = clampByte(*bg)
		}
		if *channels != "" {
			list, err := parseChannels(*channels, count)
			if err != nil {
				return err
			}
			for _, ch := range list {
				buf[ch] = byte(*value)
			}
			fmt.Printf("channels %v on %s at %d\n", list, *path, *value)
		}
		if err := write(buf); err != nil {
			return err
		}
		// hold <= 0 leaves the pattern on screen for an external stepper.
		if *hold > 0 {
			time.Sleep(*hold)
			clear()
		}
	}
	return nil
}

// restoreBaseline returns to the dim grid between sweep steps instead of going
// fully dark, so the operator keeps positional context.
func restoreBaseline(bg, count int, write func([]byte) error) {
	buf := make([]byte, count)
	for i := range buf {
		buf[i] = clampByte(bg)
	}
	_ = write(buf)
}

// applyLEDMap converts vendor JSON frame order into driver record order.
func applyLEDMap(values []int, mode string, perm []int) []byte {
	out := make([]byte, len(values))
	switch mode {
	case "vendor":
		for i, v := range values {
			if i < len(perm) && perm[i] < len(out) {
				out[perm[i]] = clampByte(v)
			}
		}
	case "inverse":
		for i := range out {
			if i < len(perm) && perm[i] < len(values) {
				out[i] = clampByte(values[perm[i]])
			}
		}
	default: // identity
		for i, v := range values {
			if i < len(out) {
				out[i] = clampByte(v)
			}
		}
	}
	return out
}

// parseRGB parses "r,g,b" with 0-255 components.
func parseRGB(spec string) (backend.RGB, error) {
	parts := strings.Split(spec, ",")
	if len(parts) != 3 {
		return backend.RGB{}, fmt.Errorf("expected R,G,B, got %q", spec)
	}
	var out [3]int
	for i, part := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || v < 0 || v > 255 {
			return backend.RGB{}, fmt.Errorf("bad color component %q", part)
		}
		out[i] = v
	}
	return backend.RGB{R: uint8(out[0]), G: uint8(out[1]), B: uint8(out[2])}, nil
}

func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// parseChannels expands "0-6,28,29" into explicit indices.
func parseChannels(spec string, count int) ([]int, error) {
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err := strconv.Atoi(strings.TrimSpace(lo))
			if err != nil {
				return nil, fmt.Errorf("bad channel range %q", part)
			}
			b, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("bad channel range %q", part)
			}
			if a > b {
				a, b = b, a
			}
			for i := a; i <= b; i++ {
				if i < 0 || i >= count {
					return nil, fmt.Errorf("channel %d out of range 0-%d", i, count-1)
				}
				out = append(out, i)
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("bad channel %q", part)
		}
		if n < 0 || n >= count {
			return nil, fmt.Errorf("channel %d out of range 0-%d", n, count-1)
		}
		out = append(out, n)
	}
	return out, nil
}
