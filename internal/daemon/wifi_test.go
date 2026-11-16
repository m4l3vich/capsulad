package daemon

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"capsulad/internal/bluetooth"
)

// ssidWithinWifiRule mirrors internal/wifi's SSID rule (1..32 UTF-8 bytes, no
// NUL/CR/LF). It is duplicated rather than weakened: the daemon must hand the
// Wi-Fi layer a name that rule accepts, otherwise the setup AP cannot start.
func ssidWithinWifiRule(ssid string) bool {
	if len(ssid) == 0 || len(ssid) > 32 || !utf8.ValidString(ssid) {
		return false
	}
	for _, r := range ssid {
		if r == 0 || r == '\n' || r == '\r' {
			return false
		}
	}
	return true
}

// TestWifiAPNameTruncatesLongAdapterAlias pins the boundary between the
// Bluetooth name (up to 248 bytes) and a Wi-Fi SSID (32 bytes): the setup AP
// always gets a valid SSID, shortened on a rune boundary, and the Bluetooth
// limit itself is not reduced.
func TestWifiAPNameTruncatesLongAdapterAlias(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		alias string
		want  string
	}{
		{name: "short alias unchanged", alias: "Kitchen Speaker", want: "Kitchen Speaker"},
		{name: "exactly 32 bytes", alias: strings.Repeat("a", 32), want: strings.Repeat("a", 32)},
		{name: "33 bytes", alias: strings.Repeat("a", 33), want: strings.Repeat("a", 32)},
		{name: "248 bytes", alias: strings.Repeat("b", bluetooth.MaxAdapterNameBytes), want: strings.Repeat("b", 32)},
		{
			name:  "rune ends exactly at the limit",
			alias: strings.Repeat("a", 30) + "Ж",
			want:  strings.Repeat("a", 30) + "Ж",
		},
		{
			name:  "multi-byte rune straddles the limit",
			alias: strings.Repeat("a", 31) + "Жз",
			want:  strings.Repeat("a", 31),
		},
		{
			name:  "emoji fills the limit exactly",
			alias: strings.Repeat("🎵", 40),
			want:  strings.Repeat("🎵", 8),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bt := bluetooth.NewMemory()
			if err := bt.SetAdapterName(ctx, tc.alias); err != nil {
				t.Fatalf("SetAdapterName(%d bytes): %v", len(tc.alias), err)
			}
			d := &Daemon{bt: bt}
			got, err := d.wifiAPName(ctx)
			if err != nil {
				t.Fatalf("wifiAPName: %v", err)
			}
			if got != tc.want {
				t.Fatalf("AP name = %q, want %q", got, tc.want)
			}
			if !ssidWithinWifiRule(got) {
				t.Fatalf("AP name %q is not a valid Wi-Fi SSID", got)
			}
		})
	}
}
