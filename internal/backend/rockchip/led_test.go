package rockchip

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unsafe"

	"capsulad/internal/backend"
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

func TestEncodeWS2812Frame(t *testing.T) {
	pixels := []backend.RGB{
		{R: 255, G: 128, B: 64},
		{R: 200, G: 100, B: 50},
	}
	out := encodeWS2812Frame(pixels, 1, ws2812ChannelCount)
	if len(out) != ws2812ChannelCount {
		t.Fatalf("len = %d", len(out))
	}
	// Logical channel i lands at physical ws2812ChannelMap[i], after the
	// perceptual curve.
	want := map[int]byte{
		ws2812ChannelMap[0]: ws2812GammaTable[255],
		ws2812ChannelMap[1]: ws2812GammaTable[128],
		ws2812ChannelMap[2]: ws2812GammaTable[64],
		ws2812ChannelMap[3]: ws2812GammaTable[200],
		ws2812ChannelMap[4]: ws2812GammaTable[100],
		ws2812ChannelMap[5]: ws2812GammaTable[50],
	}
	for idx, value := range want {
		if out[idx] != value {
			t.Fatalf("channel %d = %d, want %d", idx, out[idx], value)
		}
	}
	for idx, value := range out {
		if _, expected := want[idx]; !expected && value != 0 {
			t.Fatalf("channel %d = %d, want black (unused pixels)", idx, value)
		}
	}
}

func TestEncodeWS2812FrameBrightness(t *testing.T) {
	pixel := []backend.RGB{{R: 255, G: 255, B: 255}}
	for _, tc := range []struct {
		name       string
		brightness float64
		want       byte
	}{
		{"negative", -1, 0},
		{"nan", math.NaN(), 0},
		{"above one", 2, ws2812GammaTable[255]},
		{"full", 1, ws2812GammaTable[255]},
		{"half", 0.5, ws2812GammaTable[128]},
		{"auto floor", 0.05, ws2812GammaTable[13]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := encodeWS2812Frame(pixel, tc.brightness, ws2812ChannelCount)
			if got := out[ws2812ChannelMap[0]]; got != tc.want {
				t.Fatalf("R = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestWS2812GammaCurve pins the perceptual behaviour rather than the table: the
// endpoints are fixed, the curve is monotonic, and mid tones are darker than a
// linear ramp would make them.
func TestWS2812GammaCurve(t *testing.T) {
	if ws2812GammaTable[0] != 0 || ws2812GammaTable[255] != 255 {
		t.Fatalf("gamma endpoints = %d..%d", ws2812GammaTable[0], ws2812GammaTable[255])
	}
	prev := byte(0)
	for i := 0; i < 256; i++ {
		if ws2812GammaTable[i] < prev {
			t.Fatalf("gamma table is not monotonic at %d", i)
		}
		prev = ws2812GammaTable[i]
	}
	if mid := ws2812GammaTable[128]; mid >= 128 || mid < 40 {
		t.Fatalf("gamma(128) = %d, want a mid tone well below the linear 128", mid)
	}
	if low := ws2812GammaTable[32]; low >= 32 {
		t.Fatalf("gamma(32) = %d, want below the linear 32", low)
	}
}

func TestEncodeMultiCtrlFrame(t *testing.T) {
	// Segment a of the first digit, segment g of the fourth, and both colon
	// dots, each of which must reach its calibrated channel.
	cells := []uint16{1 << 0, 0, 0, 1 << 6}
	records := encodeMultiCtrlFrame(cells, true, 0.5, multiCtrlCellCount)
	if len(records) != multiCtrlCellCount {
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
	floor := encodeMultiCtrlFrame(cells, true, 0.05, multiCtrlCellCount)
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
	records := encodeMultiCtrlFrame([]uint16{one, two, three, four}, true, 1, multiCtrlCellCount)
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
	seen := make([]bool, multiCtrlCellCount)
	for _, row := range multiCtrlChannelMap {
		for _, ch := range row {
			if ch < 0 || ch >= multiCtrlCellCount || seen[ch] {
				t.Fatalf("multiCtrlChannelMap is not a permutation at channel %d", ch)
			}
			seen[ch] = true
		}
	}
	for _, ch := range []int{multiCtrlColonTop, multiCtrlColonBottom} {
		if ch < 0 || ch >= multiCtrlCellCount || seen[ch] {
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

func TestWS2812ChannelMapIsPermutation(t *testing.T) {
	seen := make([]bool, len(ws2812ChannelMap))
	for _, v := range ws2812ChannelMap {
		if v < 0 || v >= len(ws2812ChannelMap) || seen[v] {
			t.Fatalf("ws2812ChannelMap is not a permutation at index %d", v)
		}
		seen[v] = true
	}
}

func TestNaturalLess(t *testing.T) {
	if !naturalLess("top-led2", "top-led10") {
		t.Fatal("top-led2 must sort before top-led10")
	}
	if naturalLess("top-led10", "top-led2") {
		t.Fatal("top-led10 must sort after top-led2")
	}
	if !naturalLess("clock-led9", "top-led1") {
		t.Fatal("different prefixes keep lexicographic order")
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
