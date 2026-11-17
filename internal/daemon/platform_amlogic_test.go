package daemon

import (
	"testing"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
)

// TestProfileOrDefaultSelectsAmlogic keeps the platform-to-profile mapping in
// one place: an embedder that only names a platform still gets the matching
// board data.
func TestProfileOrDefaultSelectsAmlogic(t *testing.T) {
	for _, tc := range []struct {
		platform string
		want     string
	}{
		{"rockchip-stock", "rk3308-capsula-mini-rev4-stock"},
		{"rockchip", "rk3308-capsula-mini-rev4-stock"},
		{"amlogic-stock", "amlogic-a113x-capsula-mini-stock"},
		{"amlogic-alpine", "amlogic-a113x-capsula-mini-alpine"},
		{"simulator", "simulator"},
		{"", "simulator"},
	} {
		if got := profileOrDefault(nil, tc.platform).ID; got != tc.want {
			t.Fatalf("profileOrDefault(%q) = %q, want %q", tc.platform, got, tc.want)
		}
	}
}

// TestPlatformProtoMapsAmlogic pins the wire enum each backend platform
// reports, so a daemon on the Amlogic board never claims the Rockchip target.
func TestPlatformProtoMapsAmlogic(t *testing.T) {
	for _, tc := range []struct {
		platform backend.Platform
		want     frameworkv1.TargetPlatform
	}{
		{backend.PlatformSimulator, frameworkv1.TargetPlatform_TARGET_PLATFORM_UNSPECIFIED},
		{backend.PlatformRockchipStock, frameworkv1.TargetPlatform_CAPSULA_MINI_ROCKCHIP_STOCK},
		{backend.PlatformAmlogicStock, frameworkv1.TargetPlatform_CAPSULA_MINI_AMLOGIC_STOCK},
		{backend.PlatformAmlogicAlpine, frameworkv1.TargetPlatform_CAPSULA_MINI_AMLOGIC_ALPINE},
	} {
		d := &Daemon{dev: fixedPlatformBackend{platform: tc.platform}}
		if got := d.platformProto(); got != tc.want {
			t.Fatalf("platformProto(%s) = %s, want %s", tc.platform, got, tc.want)
		}
	}
}
