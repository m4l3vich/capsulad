package linuxdev

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"capsulad/internal/backend"
)

// ClaimLED disables a kernel trigger before the sysfs driver writes
// brightness. The stock boot scripts do the same; capsulad may start earlier or
// the script may be skipped, and a device-tree default `timer` trigger ignores
// brightness writes.
func ClaimLED(dir string) {
	trigger := filepath.Join(dir, "trigger")
	if _, err := os.Stat(trigger); err == nil {
		_ = os.WriteFile(trigger, []byte("none"), 0o644)
	}
}

// ProbeSysfsLEDs enumerates /sys/class/leds. Devices exposing multi_intensity
// are RGB pixels; the rest are monochrome cells. skipPrefixes drops device
// families that must not be treated as display cells (a board names its ring
// channels there). Ordering is natural by channel number, which matches the
// firmwares' ledN declaration.
func ProbeSysfsLEDs(prefix, classDir string, ringCap, monoCap int, skipPrefixes []string) (ring, mono []string) {
	entries, err := os.ReadDir(filepath.Join(prefix, classDir))
	if err != nil {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool { return NaturalLess(names[i], names[j]) })
	skipped := func(name string) bool {
		for _, p := range skipPrefixes {
			if p != "" && len(name) >= len(p) && name[:len(p)] == p {
				return true
			}
		}
		return false
	}
	for _, name := range names {
		dir := filepath.Join(prefix, classDir, name)
		if _, err := os.Stat(filepath.Join(dir, "multi_intensity")); err == nil {
			ring = append(ring, dir)
			continue
		}
		if skipped(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "brightness")); err == nil {
			mono = append(mono, dir)
		}
	}
	if ringCap > 0 && len(ring) > ringCap {
		ring = ring[:ringCap]
	}
	if monoCap > 0 && len(mono) > monoCap {
		mono = mono[:monoCap]
	}
	return ring, mono
}

// SysfsRing drives a ring made of RGB LED class devices.
type SysfsRing struct {
	mu   sync.Mutex
	leds []string
}

// NewSysfsRing wraps one multi_intensity LED class device per pixel.
func NewSysfsRing(leds []string) *SysfsRing { return &SysfsRing{leds: leds} }

func (r *SysfsRing) Caps() backend.RingCaps {
	return backend.RingCaps{PixelCount: len(r.leds), SupportsRGB: true}
}

func (r *SysfsRing) ResourceID() string { return "light.ring.main" }

func (r *SysfsRing) WriteRing(f backend.RingFrame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, path := range r.leds {
		var px backend.RGB
		if i < len(f.Pixels) {
			px = f.Pixels[i]
		}
		lvl := f.Brightness
		if lvl < 0 || lvl > 1 {
			lvl = 1
		}
		val := fmt.Sprintf("%d %d %d", int(float64(px.R)*lvl), int(float64(px.G)*lvl), int(float64(px.B)*lvl))
		if err := os.WriteFile(filepath.Join(path, "multi_intensity"), []byte(val), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// SysfsSegment drives a monochrome LED class device per segment.
type SysfsSegment struct {
	mu               sync.Mutex
	leds             []string
	digits           int
	segmentsPerDigit int
	colonEntries     int
}

// NewSysfsSegment maps led index cell*segmentsPerDigit+segment to one LED class
// device. colonEntries is how many colon devices trail the digit cells (0 or 2,
// and only meaningful when they are present); it is not a total channel count.
func NewSysfsSegment(leds []string, digits, segmentsPerDigit, colonEntries int) *SysfsSegment {
	return &SysfsSegment{leds: leds, digits: digits, segmentsPerDigit: segmentsPerDigit, colonEntries: colonEntries}
}

func (s *SysfsSegment) Caps() backend.SegmentCaps {
	hasColon := s.colonEntries == 2 && len(s.leds) >= s.digits*s.segmentsPerDigit+2
	return backend.SegmentCaps{
		DigitCount:       s.digits,
		SupportsColon:    hasColon,
		SupportsDecimal:  false,
		SegmentsPerDigit: s.segmentsPerDigit,
	}
}

func (s *SysfsSegment) ResourceID() string { return "display.segment.front" }

func (s *SysfsSegment) WriteSegment(f backend.SegmentFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for cell := range s.digits {
		var mask uint16
		if cell < len(f.Cells) {
			mask = f.Cells[cell]
		}
		for seg := range s.segmentsPerDigit {
			idx := cell*s.segmentsPerDigit + seg
			if idx >= len(s.leds) {
				break
			}
			on := mask&(1<<uint(seg)) != 0
			v := "0"
			if on {
				v = strconv.Itoa(Brightness255(f.Brightness))
			}
			if err := os.WriteFile(filepath.Join(s.leds[idx], "brightness"), []byte(v), 0o644); err != nil {
				return err
			}
		}
	}
	if s.colonEntries == 2 && len(s.leds) >= s.digits*s.segmentsPerDigit+2 {
		v := "0"
		if f.Colon {
			v = strconv.Itoa(Brightness255(f.Brightness))
		}
		for _, idx := range []int{s.digits * s.segmentsPerDigit, s.digits*s.segmentsPerDigit + 1} {
			if idx >= len(s.leds) {
				continue
			}
			_ = os.WriteFile(filepath.Join(s.leds[idx], "brightness"), []byte(v), 0o644)
		}
	}
	return nil
}
