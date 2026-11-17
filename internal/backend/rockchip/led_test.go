package rockchip

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unsafe"
)

func writeDTFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeDTNode(t *testing.T, dir, name string, compatible string, ledLabels int) {
	t.Helper()
	node := filepath.Join(dir, name)
	if err := os.MkdirAll(node, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDTFile(t, node, "compatible", []byte(compatible+"\x00"))
	for i := 1; i <= ledLabels; i++ {
		led := filepath.Join(node, "led"+strconv.Itoa(i))
		if err := os.MkdirAll(led, 0o755); err != nil {
			t.Fatal(err)
		}
		writeDTFile(t, led, "label", []byte("led"+strconv.Itoa(i)+"\x00"))
	}
}

func TestProbeLEDDeviceTree(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "proc/device-tree")
	spi := filepath.Join(base, "spi@ff120000")
	i2c := filepath.Join(base, "i2c@ff060000")
	if err := os.MkdirAll(spi, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(i2c, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDTNode(t, spi, "ws2812b@00", "rockchip,rk3308-spi\x00ws2812b\x00", 36)
	writeDTFile(t, filepath.Join(spi, "ws2812b@00"), "device-name", []byte("topleds\x00"))
	writeDTNode(t, i2c, "led-controller@3c", "issi,is31fl3236\x00", 30)

	dt := probeLEDDeviceTree(root)
	if dt.Ring == nil || dt.Ring.Channels != 36 || dt.Ring.DeviceName != "topleds" {
		t.Fatalf("ring node = %+v", dt.Ring)
	}
	if dt.Segment == nil || dt.Segment.Cells != 30 {
		t.Fatalf("segment node = %+v", dt.Segment)
	}
}

func TestProbeLEDDeviceTreeAbsent(t *testing.T) {
	dt := probeLEDDeviceTree(t.TempDir())
	if dt.Ring != nil || dt.Segment != nil {
		t.Fatalf("unexpected nodes: %+v", dt)
	}
}

func TestEncodeMultiCtrlFrame(t *testing.T) {
	// Segment a of the first digit, segment g of the fourth, and both colon
	// dots, each of which must reach its calibrated channel.
	cells := []uint16{1 << 0, 0, 0, 1 << 6}
	records := encodeMultiCtrlFrame(cells, true, 0.5, DefaultConfig().LEDMultiCtrlChannels, DefaultConfig().LEDSegmentDigits, DefaultConfig().LEDSegmentsPerDigit)
	if len(records) != DefaultConfig().LEDMultiCtrlChannels {
		t.Fatalf("len = %d", len(records))
	}
	if got := unsafe.Sizeof(records[0]); got != 20 {
		t.Fatalf("record size = %d, want 20", got)
	}
	want := map[int]uint32{
		multiCtrlChannelMap[0][0]: 128,
		multiCtrlChannelMap[3][6]: 128,
		multiCtrlColonTop:         128,
		multiCtrlColonBottom:      128,
	}
	for i, record := range records {
		if got := record.Brightness; got != want[i] {
			t.Fatalf("record %d brightness = %d, want %d", i, got, want[i])
		}
	}
	for i, record := range records {
		if record.Enable != 1 {
			t.Fatalf("record %d enable = %d, want 1", i, record.Enable)
		}
	}
	// The auto-darkness floor must survive 8-bit quantization: 5 % is 13/255,
	// not zero, so an enabled automatic display stays visibly lit.
	floor := encodeMultiCtrlFrame(cells, true, 0.05, DefaultConfig().LEDMultiCtrlChannels, DefaultConfig().LEDSegmentDigits, DefaultConfig().LEDSegmentsPerDigit)
	for i, record := range floor {
		if want[i] != 0 && record.Brightness == 0 {
			t.Fatalf("channel %d quantized the 5%% floor to zero", i)
		}
	}
}

// TestEncodeMultiCtrlFrameClockPattern pins the whole chain: the standard glyph
// masks the clock app emits, mapped through the measured channel table, must
// light exactly the channels recorded by cmd/ledmap for 12:34.
func TestEncodeMultiCtrlFrameClockPattern(t *testing.T) {
	const (
		one   = 0b0000110 // b c
		two   = 0b1011011 // a b d e g
		three = 0b1001111 // a b c d g
		four  = 0b1100110 // b c f g
	)
	records := encodeMultiCtrlFrame([]uint16{one, two, three, four}, true, 1, DefaultConfig().LEDMultiCtrlChannels, DefaultConfig().LEDSegmentDigits, DefaultConfig().LEDSegmentsPerDigit)
	want := map[int]bool{
		28: true, 21: true, // digit 1: b, c
		26: true, 25: true, 18: true, 23: true, 20: true, // digit 2: a b d e g
		0: true, 2: true, 7: true, 5: true, 3: true, // digit 3: a b c d g
		12: true, 11: true, 8: true, 9: true, // digit 4: b c f g
		multiCtrlColonTop: true, multiCtrlColonBottom: true,
	}
	for ch, record := range records {
		lit := record.Brightness != 0
		if lit != want[ch] {
			t.Fatalf("channel %d lit=%v, want %v", ch, lit, want[ch])
		}
	}
}

// The display's record order is measured, not derived; this guards the table
// against edits that would silently drive the wrong segment.
func TestMultiCtrlChannelMapIsPermutation(t *testing.T) {
	seen := make([]bool, DefaultConfig().LEDMultiCtrlChannels)
	for _, row := range multiCtrlChannelMap {
		for _, ch := range row {
			if ch < 0 || ch >= DefaultConfig().LEDMultiCtrlChannels || seen[ch] {
				t.Fatalf("multiCtrlChannelMap is not a permutation at channel %d", ch)
			}
			seen[ch] = true
		}
	}
	for _, ch := range []int{multiCtrlColonTop, multiCtrlColonBottom} {
		if ch < 0 || ch >= DefaultConfig().LEDMultiCtrlChannels || seen[ch] {
			t.Fatalf("colon channel %d is out of range or duplicated", ch)
		}
		seen[ch] = true
	}
	for ch, used := range seen {
		if !used {
			t.Fatalf("channel %d is not assigned to any segment", ch)
		}
	}
}

func TestNewSelectsVendorGeometryWithoutDevices(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "proc/device-tree")
	spi := filepath.Join(base, "spi@ff120000")
	i2c := filepath.Join(base, "i2c@ff060000")
	for _, dir := range []string{spi, i2c} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeDTNode(t, spi, "ws2812b@00", "ws2812b\x00", 36)
	writeDTNode(t, i2c, "led-controller@3c", "issi,is31fl3236\x00", 30)

	b, err := New(Config{RootPrefix: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.ring.(*unverifiedRing); !ok {
		t.Fatalf("ring = %T, want *unverifiedRing without a live device", b.ring)
	}
	if _, ok := b.seg.(*unverifiedSegment); !ok {
		t.Fatalf("segment = %T, want *unverifiedSegment without a live device", b.seg)
	}
	if got := b.ring.Caps().PixelCount; got != 12 {
		t.Fatalf("ring pixels = %d, want 12", got)
	}
}
