package amlogic

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"capsulad/internal/backend"
	"capsulad/internal/backend/linuxdev"
)

// openPanel selects the clock-panel driver. The board can expose it three ways,
// and only the probed one is claimed:
//
//  1. per-segment LED class devices named by the profile pattern (a mainline
//     build, or a vendor driver that registers the IS31FL3236A/AW21036 that
//     way);
//  2. a kernel line display (linedisp message + map_seg7), which is what the
//     published Alpine tools drive;
//  3. the stock vendor char device, whose ABI is unrecovered: frames stay in
//     memory and the subsystem reports UNVERIFIED.
func openPanel(cfg Config, caps []backend.SubsystemStatus) (backend.SegmentDevice, []backend.SubsystemStatus) {
	if seg, count, ok := segmentFromChannels(cfg); ok {
		return seg, append(caps, backend.SubsystemStatus{
			Subsystem: "segment", BackendID: "amlogic.sysfs.segment.clockled", Status: backend.Partial,
			Detail: fmt.Sprintf("%d-channel LED class clock array mapped as %dx%d segments plus colon with the channel table published by the board's hardware layer; the stock brightness ceiling (153/255) is not applied", count, cfg.LEDSegmentDigits, cfg.LEDSegmentsPerDigit),
		})
	}
	if seg, count, ok := segmentFromLEDs(cfg); ok {
		return seg, append(caps, backend.SubsystemStatus{
			Subsystem: "segment", BackendID: "amlogic.sysfs.segment.led", Status: backend.Partial,
			Detail: fmt.Sprintf("%d per-segment LED class devices mapped as %dx%d segments plus colon; the a..g bit order follows the framework contract and needs one hardware check", count, cfg.LEDSegmentDigits, cfg.LEDSegmentsPerDigit),
		})
	}
	if seg, ok := openLineDisplay(cfg); ok {
		brightness := "no panel-level brightness attribute: brightness is applied as on/off, and 0 blanks the panel"
		if d, ok := seg.(*lineDisplay); ok && d.brightnessPath != "" {
			brightness = "panel brightness attribute " + d.brightnessPath
		}
		return seg, append(caps, backend.SubsystemStatus{
			Subsystem: "segment", BackendID: "amlogic.sysfs.linedisp", Status: backend.Partial,
			Detail: "kernel line display driven through message/map_seg7 and the colon LED class devices; glyph masks are reverse-looked-up in the device's own map_seg7, so an unexpected bit order blanks rather than misdrives the panel; " + brightness,
		})
	}
	if cfg.LEDMultiCtrlPath != "" && linuxdev.NodeExists(cfg.RootPrefix, cfg.LEDMultiCtrlPath) {
		// The vendor char device is present but no userspace path to it exists
		// here, so no segment resource is published: an app must not route a
		// frame into a no-op. The stock LED class array above is the supported
		// output path on this board.
		return nil, append(caps, backend.SubsystemStatus{
			Subsystem: "segment", BackendID: "amlogic.vendor.is31fl32xx", Status: backend.Unverified,
			Detail: fmt.Sprintf("clock panel char device %s present but not driven from this daemon (the stock frame ABI is a private ioctl); no segment resource is published", cfg.LEDMultiCtrlPath),
		})
	}
	return nil, append(caps, backend.SubsystemStatus{Subsystem: "segment", Status: backend.Unavailable, Detail: "no segment display detected"})
}

// segmentFromChannels builds the panel from a fixed LED class channel array
// (clock-led1..30 on the stock image) using the published cell/segment table.
func segmentFromChannels(cfg Config) (backend.SegmentDevice, int, bool) {
	if cfg.SegmentChannelPrefix == "" || len(cfg.SegmentChannelMap) < cfg.LEDSegmentDigits*cfg.LEDSegmentsPerDigit {
		return nil, 0, false
	}
	classDir := pathFor(cfg.RootPrefix, cfg.LEDSysfsClassDir)
	channel := func(index int) string {
		return filepath.Join(classDir, fmt.Sprintf("%s%d", cfg.SegmentChannelPrefix, index+1))
	}
	var dirs []string
	for cell := 0; cell < cfg.LEDSegmentDigits; cell++ {
		for seg := 0; seg < cfg.LEDSegmentsPerDigit; seg++ {
			index := cfg.SegmentChannelMap[cell*cfg.LEDSegmentsPerDigit+seg]
			dir := channel(index)
			if _, err := os.Stat(filepath.Join(dir, "brightness")); err != nil {
				return nil, 0, false
			}
			dirs = append(dirs, dir)
		}
	}
	colonEntries := 0
	if cfg.ColonTopLED != "" && cfg.ColonBottomLED != "" {
		top := filepath.Join(classDir, cfg.ColonTopLED)
		bottom := filepath.Join(classDir, cfg.ColonBottomLED)
		if _, err := os.Stat(filepath.Join(top, "brightness")); err == nil {
			if _, err := os.Stat(filepath.Join(bottom, "brightness")); err == nil {
				dirs = append(dirs, top, bottom)
				colonEntries = 2
			}
		}
	}
	if cfg.RootPrefix == "" {
		for _, dir := range dirs {
			linuxdev.ClaimLED(dir)
		}
	}
	return linuxdev.NewSysfsSegment(dirs, cfg.LEDSegmentDigits, cfg.LEDSegmentsPerDigit, colonEntries), len(dirs), true
}

// segmentFromLEDs builds the per-segment LED class map from the profile naming.
func segmentFromLEDs(cfg Config) (backend.SegmentDevice, int, bool) {
	if cfg.SegmentLEDPattern == "" || len(cfg.SegmentLetters) < cfg.LEDSegmentsPerDigit {
		return nil, 0, false
	}
	classDir := pathFor(cfg.RootPrefix, cfg.LEDSysfsClassDir)
	var dirs []string
	for digit := 1; digit <= cfg.LEDSegmentDigits; digit++ {
		for segment := 0; segment < cfg.LEDSegmentsPerDigit; segment++ {
			name := fmt.Sprintf(cfg.SegmentLEDPattern, digit, string(cfg.SegmentLetters[segment]))
			dir := filepath.Join(classDir, name)
			if _, err := os.Stat(filepath.Join(dir, "brightness")); err != nil {
				return nil, 0, false
			}
			dirs = append(dirs, dir)
		}
	}
	colonEntries := 0
	if cfg.ColonTopLED != "" && cfg.ColonBottomLED != "" {
		top := filepath.Join(classDir, cfg.ColonTopLED)
		bottom := filepath.Join(classDir, cfg.ColonBottomLED)
		if _, err := os.Stat(filepath.Join(top, "brightness")); err == nil {
			if _, err := os.Stat(filepath.Join(bottom, "brightness")); err == nil {
				dirs = append(dirs, top, bottom)
				colonEntries = 2
				if cfg.RootPrefix == "" {
					linuxdev.ClaimLED(top)
					linuxdev.ClaimLED(bottom)
				}
			}
		}
	}
	if cfg.RootPrefix == "" {
		for _, dir := range dirs {
			linuxdev.ClaimLED(dir)
		}
	}
	return linuxdev.NewSysfsSegment(dirs, cfg.LEDSegmentDigits, cfg.LEDSegmentsPerDigit, colonEntries), len(dirs), true
}

// lineDisplay drives the kernel line-display (seg7) interface. The framework
// renders segment masks, so each cell is mapped back to a character through the
// device's own 128-byte map_seg7 table; a mask the table does not contain shows
// a space instead of lighting the wrong segment.
type lineDisplay struct {
	mu             sync.Mutex
	messagePath    string
	brightnessPath string
	colonTop       string
	colonBottom    string
	glyphs         []byte
	digits         int
	lastMessage    []byte
	lastBrightness int
	lastColon      int
}

// lineDisplayCandidates are the paths the kernel line-display framework has
// been observed under on this board. They are tried in order.
var lineDisplayCandidates = []string{
	"sys/class/linedisp/linedisp.*/message",
	"sys/bus/platform/devices/*/linedisp.*/message",
	"sys/devices/platform/*/linedisp.*/message",
}

func openLineDisplay(cfg Config) (backend.SegmentDevice, bool) {
	var messagePath string
	for _, candidate := range lineDisplayCandidates {
		if matches := linuxdev.GlobSorted(linuxdev.Rooted(cfg.RootPrefix, candidate)); len(matches) > 0 {
			messagePath = matches[0]
			break
		}
	}
	if messagePath == "" {
		return nil, false
	}
	glyphs, err := os.ReadFile(filepath.Join(filepath.Dir(messagePath), "map_seg7"))
	if err != nil || len(glyphs) != 128 {
		return nil, false
	}
	d := &lineDisplay{
		messagePath:    messagePath,
		brightnessPath: probeBrightness(messagePath),
		glyphs:         glyphs,
		digits:         cfg.LEDSegmentDigits,
		lastBrightness: -1,
		lastColon:      -1,
	}
	classDir := pathFor(cfg.RootPrefix, cfg.LEDSysfsClassDir)
	if cfg.ColonTopLED != "" {
		if _, err := os.Stat(filepath.Join(classDir, cfg.ColonTopLED, "brightness")); err == nil {
			d.colonTop = filepath.Join(classDir, cfg.ColonTopLED)
		}
	}
	if cfg.ColonBottomLED != "" {
		if _, err := os.Stat(filepath.Join(classDir, cfg.ColonBottomLED, "brightness")); err == nil {
			d.colonBottom = filepath.Join(classDir, cfg.ColonBottomLED)
		}
	}
	if cfg.RootPrefix == "" {
		linuxdev.ClaimLED(d.colonTop)
		linuxdev.ClaimLED(d.colonBottom)
	}
	return d, true
}

// probeBrightness finds a panel-level brightness attribute. Upstream linedisp
// exposes only message/num_chars/scroll/map_* attributes, so the attribute may
// be absent (or live in the parent device dir when the linedisp is attached to a
// caller's device). An absent attribute is not invented and not written.
func probeBrightness(messagePath string) string {
	dir := filepath.Dir(messagePath)
	for _, candidate := range []string{
		filepath.Join(dir, "brightness"),
		filepath.Clean(filepath.Join(dir, "..", "brightness")),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func (d *lineDisplay) Caps() backend.SegmentCaps {
	return backend.SegmentCaps{
		DigitCount:       d.digits,
		SupportsColon:    d.colonTop != "" && d.colonBottom != "",
		SupportsDecimal:  false,
		SegmentsPerDigit: 7,
	}
}

func (d *lineDisplay) ResourceID() string { return "display.segment.front" }

// glyphFor returns the character whose segment bitmap equals mask. The map is
// indexed by ASCII code, so the printable range is searched first.
func (d *lineDisplay) glyphFor(mask uint16) byte {
	if mask&0x7f == 0 {
		return ' '
	}
	for code := 0x20; code < len(d.glyphs); code++ {
		if uint16(d.glyphs[code])&0x7f == mask&0x7f {
			return byte(code)
		}
	}
	return ' '
}

func (d *lineDisplay) WriteSegment(f backend.SegmentFrame) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	level := linuxdev.Brightness255(f.Brightness)
	if level != d.lastBrightness && d.brightnessPath != "" {
		// A probed attribute that disappears is a real failure, not a silent
		// fallback: the frame keeps going to message/colon, but the caller sees
		// the error instead of believing it dimmed the panel.
		if err := os.WriteFile(d.brightnessPath, []byte(strconv.Itoa(level)), 0o644); err != nil {
			return err
		}
		d.lastBrightness = level
	}
	colon := 0
	if f.Colon {
		colon = level
	}
	if colon != d.lastColon {
		for _, path := range []string{d.colonTop, d.colonBottom} {
			if path == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(path, "brightness"), []byte(strconv.Itoa(colon)), 0o644); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		d.lastColon = colon
	}
	message := make([]byte, d.digits)
	for cell := range message {
		message[cell] = ' '
	}
	// Without a panel-level brightness the display cannot dim; off must still
	// blank the glyphs instead of showing full-intensity digits.
	if level > 0 {
		for cell := range message {
			var mask uint16
			if cell < len(f.Cells) {
				mask = f.Cells[cell]
			}
			message[cell] = d.glyphFor(mask)
		}
	}
	if string(message) != string(d.lastMessage) {
		if err := os.WriteFile(d.messagePath, message, 0o644); err != nil {
			return err
		}
		d.lastMessage = message
	}
	return nil
}
