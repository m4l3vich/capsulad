package profile

import (
	"reflect"
	"testing"
	"time"
)

// TestRK3308Rev4StockLiterals pins every field to the value the framework
// hard-coded before the profile existed. A change here is a hardware contract
// change, not a refactor.
func TestRK3308Rev4StockLiterals(t *testing.T) {
	want := Profile{
		ID:       "rk3308-capsula-mini-rev4-stock",
		Platform: "rockchip-stock",
		Model: Model{
			ModelFallback:      "Capsula Mini Rockchip (model probe unavailable)",
			SoC:                "Rockchip RK3308",
			SoCMatch:           "rk3308",
			KernelFallback:     "unknown",
			ImageModelFallback: "Capsula-mini (RK3308, from stock image metadata)",
		},
		Serial: Serial{
			Path:   "/proc/cpuinfo",
			Fields: []string{"Serial", "Serial Number"},
		},
		Audio: Audio{
			Sink:             "master",
			Source:           "speex",
			MicEnableGPIO:    21,
			PlaybackFormat:   "PCM_S16LE",
			PlaybackRateHz:   48000,
			PlaybackChannels: 2,
			CaptureFormat:    "PCM_S16LE",
			CaptureRateHz:    16000,
			CaptureChannels:  1,
		},
		LED: LED{
			TopLEDsPath:       "/dev/topleds",
			MultiCtrlPath:     "/dev/led_multi_ctrl",
			SysfsClassDir:     "/sys/class/leds",
			DefaultPath:       "/dev/led_multi_ctrl",
			WS2812Channels:    36,
			WS2812Pixels:      12,
			MultiCtrlChannels: 30,
			SegmentDigits:     4,
			SegmentsPerDigit:  7,
			Gamma:             2.2,
		},
		Input: Input{
			EventDir:         "/dev/input",
			EventPrefix:      "event",
			KeymapCompatible: "gpio-keys",
		},
		Watchdog: Watchdog{
			Path:           "/dev/watchdog",
			DefaultTimeout: 30 * time.Second,
			StatusID:       "rockchip.syswatchdog",
			Nowayout:       true,
		},
		Wifi: Wifi{
			StockConfigPaths:  []string{"/etc/wpa_supplicant.conf", "/etc/wpa_supplicant/wpa_supplicant.conf"},
			StockCtrlDirs:     []string{"/var/run/wpa_supplicant", "/run/wpa_supplicant"},
			RestoreConfigPath: "/etc/wpa_supplicant.conf",
			AdoptStock:        true,
		},
		Deploy: Deploy{
			InitScript:          "/etc/init.d/S99btctl.sh",
			BootHookName:        "S99btctl.sh",
			StagingReserveBytes: 26 << 20,
			DataReserveBytes:    8 << 20,
			ExtractionRatio:     2,
			BundleMaxFileBytes:  64 << 20,
			BundleMaxTotalBytes: 128 << 20,
			BundleMaxEntries:    8192,
			DataPartitionMiB:    78.5,
			DaemonBinaryMiB:     40,
		},
		Architecture: Architecture{
			Target:      "aarch64",
			Accepted:    []string{"aarch64", "arm64"},
			ABIContains: "linux",
			MaxGLIBC:    "2.33",
		},
		Memory: Memory{PhysicalMiB: 240, HeapLimitMB: 80, GCPercent: 50},
		Brightness: Brightness{
			Scale:         19.9375,
			Lambda:        0.05,
			Bias:          0.75,
			Floor:         5,
			Ceiling:       95,
			Deadband:      0.05,
			MinLux:        5,
			MaxLux:        32768,
			AutoFloor:     0.05,
			PollInterval:  time.Second,
			Tick:          100 * time.Millisecond,
			AnimationStep: 0.06,
		},
		VendorProcesses: []string{"sysmon", "voice-agent"},
	}
	if !reflect.DeepEqual(RK3308Rev4Stock, want) {
		t.Fatalf("RK3308Rev4Stock drifted from the documented literals:\n got %#v\nwant %#v", RK3308Rev4Stock, want)
	}
}

// TestSimulatorHasNoHardwarePaths guards the boundary: the simulator profile
// must not name device nodes, stock firmware files or a watchdog.
func TestSimulatorHasNoHardwarePaths(t *testing.T) {
	if Simulator.LED.TopLEDsPath != "" || Simulator.LED.MultiCtrlPath != "" ||
		Simulator.LED.SysfsClassDir != "" || Simulator.LED.DefaultPath != "" {
		t.Fatalf("simulator LED paths = %+v", Simulator.LED)
	}
	if Simulator.Input.EventDir != "" || Simulator.Input.KeymapCompatible != "" {
		t.Fatalf("simulator input paths = %+v", Simulator.Input)
	}
	if Simulator.Watchdog.Path != "" || Simulator.Watchdog.StatusID != "" || Simulator.Watchdog.DefaultTimeout != 0 {
		t.Fatalf("simulator watchdog = %+v", Simulator.Watchdog)
	}
	if Simulator.Wifi.StockConfigPaths != nil || Simulator.Wifi.StockCtrlDirs != nil || Simulator.Wifi.RestoreConfigPath != "" {
		t.Fatalf("simulator wifi paths = %+v", Simulator.Wifi)
	}
	if Simulator.Serial.Path != "" {
		t.Fatalf("simulator serial path = %q", Simulator.Serial.Path)
	}
	if len(Simulator.VendorProcesses) != 0 {
		t.Fatalf("simulator vendor processes = %v", Simulator.VendorProcesses)
	}
}
