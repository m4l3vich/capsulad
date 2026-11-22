package amlogic

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"capsulad/internal/backend"
	"capsulad/internal/backend/linuxdev"
	"capsulad/internal/backend/profile"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigForSelectsUserlandAudio(t *testing.T) {
	stock := ConfigFor(profile.AmlogicA113XStock)
	if stock.Platform != backend.PlatformAmlogicStock || stock.AudioBackend != "pulse" {
		t.Fatalf("stock = %s/%s", stock.Platform, stock.AudioBackend)
	}
	alpine := ConfigFor(profile.AmlogicA113XAlpine)
	if alpine.Platform != backend.PlatformAmlogicAlpine || alpine.AudioBackend != "alsa" {
		t.Fatalf("alpine = %s/%s", alpine.Platform, alpine.AudioBackend)
	}
	if alpine.PCMDevice != "default" || alpine.AudioCaptureFormat.RateHz != 16000 || alpine.AudioPlaybackFormat.RateHz != 48000 {
		t.Fatalf("alpine audio = %+v", alpine)
	}
	if stock.InputKeymapCompatible != "" {
		t.Fatalf("amlogic must not probe a gpio-keys node, got %q", stock.InputKeymapCompatible)
	}
	if stock.ModelFallback == "" || stock.SoCMatch != "amlogic" || stock.LEDWS2812Channels != 36 {
		t.Fatalf("stock config = %+v", stock)
	}
	if stock.BoardRevision != "Capsula-mini-6" || alpine.BoardRevision != "Capsula-mini-6" {
		t.Fatalf("board revisions = %q/%q, want Capsula-mini-6", stock.BoardRevision, alpine.BoardRevision)
	}
}

func TestConfigKeepsProfilesDistinct(t *testing.T) {
	stock := ConfigFor(profile.AmlogicA113XStock)
	if stock.SegmentLEDPattern != "" || stock.MixerMute != "" {
		t.Fatalf("stock must not claim an Alpine mixer/panel: %+v", stock)
	}
	alpine := ConfigFor(profile.AmlogicA113XAlpine)
	if alpine.SegmentLEDPattern != "clock:d%d:%s" || alpine.SegmentLetters != "abcdefg" {
		t.Fatalf("alpine panel naming = %q/%q", alpine.SegmentLEDPattern, alpine.SegmentLetters)
	}
}

func TestOpenRingPublishesNothingWhenUndrivable(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dev/topleds", "")
	device, caps := openRing(Config{RootPrefix: root, LEDTopPath: "/dev/topleds", LEDWS2812Channels: 36, LEDWS2812Pixels: 12}, nil)
	if device != nil {
		t.Fatalf("ring = %T, want no resource for a no-op sink", device)
	}
	if len(caps) != 1 || caps[0].Status != backend.Unverified || caps[0].Subsystem != "ring" {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestOpenRingUnavailableWithoutAnyNode(t *testing.T) {
	device, caps := openRing(Config{RootPrefix: t.TempDir(), LEDTopPath: "/dev/topleds"}, nil)
	if device != nil {
		t.Fatalf("ring = %T, want none", device)
	}
	if len(caps) != 1 || caps[0].Status != backend.Unavailable {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestOpenPanelFromSegmentLEDs(t *testing.T) {
	root := t.TempDir()
	letters := "abcdefg"
	for digit := 1; digit <= 4; digit++ {
		for i := 0; i < len(letters); i++ {
			write(t, root, filepath.Join("sys/class/leds", "clock:d"+string(rune('0'+digit))+":"+string(letters[i]), "brightness"), "0\n")
		}
	}
	write(t, root, "sys/class/leds/clock:colon:top/brightness", "0\n")
	write(t, root, "sys/class/leds/clock:colon:bottom/brightness", "0\n")

	cfg := Config{
		RootPrefix: root, LEDSysfsClassDir: "/sys/class/leds",
		LEDSegmentDigits: 4, LEDSegmentsPerDigit: 7,
		SegmentLEDPattern: "clock:d%d:%s", SegmentLetters: letters,
		ColonTopLED: "clock:colon:top", ColonBottomLED: "clock:colon:bottom",
	}
	device, caps := openPanel(cfg, nil)
	if device == nil {
		t.Fatalf("no panel: %+v", caps)
	}
	if caps[0].BackendID != "amlogic.sysfs.segment.led" || caps[0].Status != backend.Partial {
		t.Fatalf("caps = %+v", caps)
	}
	c := device.Caps()
	if c.DigitCount != 4 || !c.SupportsColon || c.SegmentsPerDigit != 7 {
		t.Fatalf("caps = %+v", c)
	}
	// Digit 1 segment a (bit 0) and the colon at full brightness.
	if err := device.WriteSegment(backend.SegmentFrame{Cells: []uint16{1}, Colon: true, Brightness: 1}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "sys/class/leds/clock:d1:a/brightness")); got != "255" {
		t.Fatalf("segment a = %q, want 255", got)
	}
	if got := readFile(t, filepath.Join(root, "sys/class/leds/clock:d1:b/brightness")); got != "0" {
		t.Fatalf("segment b = %q, want 0", got)
	}
	if got := readFile(t, filepath.Join(root, "sys/class/leds/clock:colon:top/brightness")); got != "255" {
		t.Fatalf("colon = %q, want 255", got)
	}
}

func TestOpenPanelFallsBackWhenOneSegmentIsMissing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "sys/class/leds/clock:d1:a/brightness", "0\n")
	cfg := Config{
		RootPrefix: root, LEDSysfsClassDir: "/sys/class/leds",
		LEDSegmentDigits: 4, LEDSegmentsPerDigit: 7,
		SegmentLEDPattern: "clock:d%d:%s", SegmentLetters: "abcdefg",
	}
	device, caps := openPanel(cfg, nil)
	if device != nil {
		t.Fatalf("partial panel accepted: %+v", caps)
	}
	if caps[0].Status != backend.Unavailable {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestOpenPanelLineDisplayUsesDeviceGlyphTable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sys/class/linedisp/linedisp.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	glyphs := make([]byte, 128)
	glyphs['0'] = 0b0111111 // a b c d e f
	glyphs['1'] = 0b0000110 // b c
	write(t, root, "sys/class/linedisp/linedisp.0/message", "    ")
	if err := os.WriteFile(filepath.Join(dir, "map_seg7"), glyphs, 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, root, "sys/class/linedisp/brightness", "0\n")

	cfg := Config{RootPrefix: root, LEDSysfsClassDir: "/sys/class/leds", LEDSegmentDigits: 4}
	device, caps := openPanel(cfg, nil)
	if device == nil {
		t.Fatalf("no line display: %+v", caps)
	}
	if caps[0].BackendID != "amlogic.sysfs.linedisp" {
		t.Fatalf("caps = %+v", caps)
	}
	// Cells: "1", an unknown mask, blank, and "0".
	if err := device.WriteSegment(backend.SegmentFrame{
		Cells: []uint16{0b0000110, 0b1111111, 0, 0b0111111}, Brightness: 0.5,
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "message")); got != "1  0" {
		t.Fatalf("message = %q, want \"1  0\"", got)
	}
	if got := readFile(t, filepath.Join(root, "sys/class/linedisp/brightness")); got != "128" {
		t.Fatalf("brightness = %q, want 128", got)
	}
}

func TestOpenPanelPublishesNothingForUnrecoveredVendorNode(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dev/is31fl32xx", "")
	device, caps := openPanel(Config{RootPrefix: root, LEDMultiCtrlPath: "/dev/is31fl32xx", LEDSegmentDigits: 4, LEDSegmentsPerDigit: 7}, nil)
	if device != nil {
		t.Fatalf("panel = %T, want no resource for a no-op sink", device)
	}
	if caps[0].Status != backend.Unverified || caps[0].BackendID != "amlogic.vendor.is31fl32xx" {
		t.Fatalf("caps = %+v", caps)
	}
}

// The stock image exposes the clock panel as clock-led1..30 with the channel
// table published for this board; that is a real output path and must publish a
// device.
func TestOpenPanelFromStockChannelArray(t *testing.T) {
	root := t.TempDir()
	for channel := 1; channel <= 30; channel++ {
		write(t, root, "sys/class/leds/"+channelName(channel)+"/brightness", "0\n")
	}
	cfg := ConfigFor(profile.AmlogicA113XStock)
	cfg.RootPrefix = root
	device, caps := openPanel(cfg, nil)
	if device == nil {
		t.Fatalf("no stock panel: %+v", caps)
	}
	if caps[0].BackendID != "amlogic.sysfs.segment.clockled" {
		t.Fatalf("caps = %+v", caps)
	}
	// Digit 0 segment a is channel index 1 -> clock-led2; colon top is
	// clock-led17 and bottom clock-led16.
	if err := device.WriteSegment(backend.SegmentFrame{Cells: []uint16{1}, Colon: true, Brightness: 1}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "sys/class/leds/clock-led2/brightness")); got != "255" {
		t.Fatalf("clock-led2 = %q, want 255", got)
	}
	for _, name := range []string{"clock-led17", "clock-led16"} {
		if got := readFile(t, filepath.Join(root, "sys/class/leds", name, "brightness")); got != "255" {
			t.Fatalf("%s = %q, want 255", name, got)
		}
	}
	if got := readFile(t, filepath.Join(root, "sys/class/leds/clock-led1/brightness")); got != "0" {
		t.Fatalf("unmapped channel = %q, want 0", got)
	}
}

func channelName(index int) string { return "clock-led" + strconv.Itoa(index) }

// The Amlogic panel is evdev, not a device-tree gpio-keys node: the default map
// plus the board override must name the four documented codes without any tree.
func TestAmlogicKeyMapWithoutDeviceTree(t *testing.T) {
	cfg := ConfigFor(profile.AmlogicA113XStock)
	cfg.RootPrefix = t.TempDir()
	keys := linuxdev.ProbeKeyMap(cfg.RootPrefix, cfg.InputKeymapCompatible)
	for code, name := range cfg.InputKeyOverrides {
		keys[code] = name
	}
	want := map[int]string{115: "volume_up", 114: "volume_down", 248: "mic_toggle", 0x246: "play_pause"}
	for code, name := range want {
		if keys[code] != name {
			t.Fatalf("code %d = %q, want %q", code, keys[code], name)
		}
	}
}

func TestNewReportsBoardCapabilities(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/device-tree/model", "VK Capsula Mini (A113X)\x00")
	write(t, root, "etc/os-release", "PRETTY_NAME=\"Alpine Linux v3.24\"\n")
	write(t, root, "sys/bus/iio/devices/iio:device0/name", "bh1730\n")
	write(t, root, "sys/bus/iio/devices/iio:device0/in_illuminance_input", "42\n")

	b, err := New(Config{RootPrefix: root, Platform: backend.PlatformAmlogicAlpine, AudioBackend: "alsa"})
	if err != nil {
		t.Fatal(err)
	}
	info := b.Info()
	if info.Platform != backend.PlatformAmlogicAlpine || info.SoC != "Amlogic A113X" || info.Firmware != "Alpine Linux v3.24" {
		t.Fatalf("info = %+v", info)
	}
	if b.Sensors() == nil {
		t.Fatal("no sensors")
	}
	var wakeword bool
	for _, cap := range b.Capabilities() {
		if cap.Subsystem == "wakeword" && cap.Status == backend.Unverified {
			wakeword = true
		}
		if cap.Subsystem == "audio" && cap.Status == backend.Available {
			t.Fatalf("offline audio must not be available: %+v", cap)
		}
	}
	if !wakeword {
		t.Fatalf("caps = %+v", b.Capabilities())
	}
}

func TestProbeInfoFallsBackWhenModelIsUnavailable(t *testing.T) {
	b, err := New(Config{RootPrefix: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	info := b.Info()
	if info.Model != "VK Capsula Mini Amlogic 113X (model probe unavailable)" {
		t.Fatalf("model = %q", info.Model)
	}
	if info.SoC != "Amlogic A113X" {
		t.Fatalf("SoC = %q", info.SoC)
	}
	if info.Platform != backend.PlatformAmlogicStock {
		t.Fatalf("platform = %s", info.Platform)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
