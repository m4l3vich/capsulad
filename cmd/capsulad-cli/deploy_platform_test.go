package main

import (
	"strings"
	"testing"

	"capsulad/internal/backend/profile"
)

// TestSelectDeployProfileSwitchesTheContract pins the per-target deployment
// contract: the boot service, the hook name and the space floors change, while
// the bundle limits stay the same because the bundle format is identical.
func TestSelectDeployProfileSwitchesTheContract(t *testing.T) {
	defer selectDeployProfile("rockchip-stock")

	for _, tc := range []struct {
		platform string
		init     string
		hook     string
	}{
		{"rockchip-stock", profile.RK3308Rev4Stock.Deploy.InitScript, profile.RK3308Rev4Stock.Deploy.BootHookName},
		{"rockchip", profile.RK3308Rev4Stock.Deploy.InitScript, profile.RK3308Rev4Stock.Deploy.BootHookName},
		{"amlogic-stock", "/etc/init.d/S99sysmon.sh", "S99sysmon.sh"},
		{"amlogic-alpine", "/etc/init.d/capsulad", "capsulad"},
	} {
		if _, err := selectDeployProfile(tc.platform); err != nil {
			t.Fatalf("%s: %v", tc.platform, err)
		}
		if defaultInitScript != tc.init || bootHookName != tc.hook {
			t.Fatalf("%s: init=%q hook=%q", tc.platform, defaultInitScript, bootHookName)
		}
		if bundleLimits.MaxFileBytes != 64<<20 || bundleLimits.MaxTotalBytes != 128<<20 || bundleLimits.MaxEntries != 8192 {
			t.Fatalf("%s: bundle limits = %+v", tc.platform, bundleLimits)
		}
		if extractionRatio != 2 || stagingReserve != 26<<20 || dataReserve != 8<<20 {
			t.Fatalf("%s: floors = %d/%d/%d", tc.platform, extractionRatio, stagingReserve, dataReserve)
		}
	}
}

func TestSelectDeployProfileRejectsUnknownTarget(t *testing.T) {
	if _, err := selectDeployProfile("amlogic"); err == nil || !strings.Contains(err.Error(), "-platform") {
		t.Fatalf("unknown target error = %v", err)
	}
}

func TestPlatformFlagReadsBothForms(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "rockchip-stock"},
		{[]string{"bundle", "-platform", "amlogic-stock"}, "amlogic-stock"},
		{[]string{"-platform=amlogic-alpine", "bundle"}, "amlogic-alpine"},
		{[]string{"bundle", "--platform=amlogic-stock", "-dry-run"}, "amlogic-stock"},
	} {
		if got := platformFlag(tc.args); got != tc.want {
			t.Fatalf("platformFlag(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
