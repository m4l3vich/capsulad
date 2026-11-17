package profile

import "testing"

// TestAmlogicStockPanelMapIsComplete guards the panel table copied from the
// board's own MicroPython hardware layer: every one of the 30 controller
// channels must be assigned exactly once by the 28 segments plus the two colon
// dots, or a frame would light the wrong cell.
func TestAmlogicStockPanelMapIsComplete(t *testing.T) {
	led := AmlogicA113XStock.LED
	if led.SegmentChannelPrefix != "clock-led" {
		t.Fatalf("channel prefix = %q", led.SegmentChannelPrefix)
	}
	if want := led.SegmentDigits * led.SegmentsPerDigit; len(led.SegmentChannelMap) != want {
		t.Fatalf("map has %d entries, want %d", len(led.SegmentChannelMap), want)
	}
	seen := make([]bool, led.MultiCtrlChannels)
	for _, ch := range led.SegmentChannelMap {
		if ch < 0 || ch >= led.MultiCtrlChannels || seen[ch] {
			t.Fatalf("channel %d is out of range or duplicated", ch)
		}
		seen[ch] = true
	}
	for _, name := range []string{led.ColonTopLED, led.ColonBottomLED} {
		want := "clock-led16"
		if name == led.ColonTopLED {
			want = "clock-led17"
		}
		if name != want {
			t.Fatalf("colon device %q, want %q", name, want)
		}
	}
	for _, ch := range []int{15, 16} {
		if seen[ch] {
			t.Fatalf("colon channel %d is also mapped to a segment", ch)
		}
		seen[ch] = true
	}
	for ch, used := range seen {
		if !used {
			t.Fatalf("controller channel %d is unassigned", ch)
		}
	}
}

// TestAmlogicProfilesDoNotShareReservedCentre pins the board difference: the
// same KEY_VOICECOMMAND code is the neutral centre on the RK3308 and the
// assistant key on the Amlogic panel, so the override must be board data.
func TestAmlogicProfilesDoNotShareReservedCentre(t *testing.T) {
	for _, p := range []Profile{AmlogicA113XStock, AmlogicA113XAlpine} {
		if got := p.Input.KeyOverrides[0x246]; got != "play_pause" {
			t.Fatalf("%s override = %q, want play_pause", p.ID, got)
		}
	}
	if RK3308Rev4Stock.Input.KeyOverrides != nil {
		t.Fatalf("rockchip overrides = %v, want none", RK3308Rev4Stock.Input.KeyOverrides)
	}
}

// TestAmlogicAlpineIsMusl keeps the package-ABI gate honest: a glibc-linked
// native package must not be accepted on Alpine.
func TestAmlogicAlpineIsMusl(t *testing.T) {
	if got := AmlogicA113XAlpine.Architecture.ABIContains; got != "musl" {
		t.Fatalf("Alpine ABIContains = %q", got)
	}
	if got := AmlogicA113XStock.Architecture.ABIContains; got != "linux" {
		t.Fatalf("stock ABIContains = %q", got)
	}
}

// The Alpine ring is SPI: the profile must name the image's spidev node and
// clock, and the stock board must not claim one.
func TestAmlogicRingSelectionData(t *testing.T) {
	alpine := AmlogicA113XAlpine.LED
	if alpine.SPIDevPath != "/dev/spidev0.0" || alpine.SPISpeedHz != 3340000 || alpine.SPIBitsPerWord != 32 {
		t.Fatalf("alpine SPI ring = %q/%d/%d", alpine.SPIDevPath, alpine.SPISpeedHz, alpine.SPIBitsPerWord)
	}
	if alpine.SPIPrimeHz != 1000000 {
		t.Fatalf("alpine prime rate = %d", alpine.SPIPrimeHz)
	}
	if alpine.TopLEDsPath != "" {
		t.Fatalf("alpine must not claim /dev/topleds")
	}
	stock := AmlogicA113XStock.LED
	if stock.SPIDevPath != "" || stock.TopLEDsPath != "/dev/topleds" {
		t.Fatalf("stock ring paths = %q/%q", stock.TopLEDsPath, stock.SPIDevPath)
	}
}
