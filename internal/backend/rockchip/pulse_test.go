package rockchip

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestVolumePercentages(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []int
	}{
		{
			name: "stereo unity",
			out:  "Volume: front-left: 65536 / 100% / 0.00 dB,   front-right: 65536 / 100% / 0.00 dB",
			want: []int{100, 100},
		},
		{
			name: "one channel lowered",
			out:  "Volume: front-left: 32768 /  50% / -18.06 dB,   front-right: 65536 / 100% / 0.00 dB",
			want: []int{50, 100},
		},
		{
			name: "mono",
			out:  "Volume: mono: 65536 / 100% / 0.00 dB",
			want: []int{100},
		},
		{
			name: "no percentages",
			out:  "Failure: No such entity",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumePercentages(tc.out); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("volumePercentages(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// fakePactl puts a scripted pactl first on PATH so the sink pin can be
// exercised without PulseAudio. The script body runs after the shebang.
func fakePactl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pactl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestPulseSinkPinWarnsThroughHook proves a failed sink pin reaches the
// daemon's log hook exactly once instead of the process's standard logger,
// and that a failed pin stays retryable.
func TestPulseSinkPinWarnsThroughHook(t *testing.T) {
	fakePactl(t, `case "$1" in
  get-sink-volume) echo "Volume: front-left: 32768 /  50% / -18.06 dB,   front-right: 32768 /  50% / -18.06 dB";;
esac
exit 0`)
	var warns, infos []string
	p := newPulseAudio(Config{
		AudioSink: "master",
		Warnf:     func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) },
		Infof:     func(format string, args ...any) { infos = append(infos, fmt.Sprintf(format, args...)) },
	})
	p.pinSinkUnity()
	p.warnOnce("a second warning is suppressed")
	if len(warns) != 1 {
		t.Fatalf("warnings = %q, want exactly one", warns)
	}
	if !strings.Contains(warns[0], "could not be pinned to unity") {
		t.Fatalf("warning %q does not report the failed pin", warns[0])
	}
	if len(infos) != 0 {
		t.Fatalf("informational logs = %q, want none", infos)
	}
	if p.pinDone {
		t.Fatal("pinDone set after a failed pin; a later volume change must retry")
	}
}

// TestPulseSinkPinSuccessReportsInfo covers the pinned path: one informational
// message, no warning, and the pin marked done.
func TestPulseSinkPinSuccessReportsInfo(t *testing.T) {
	fakePactl(t, `case "$1" in
  get-sink-volume) echo "Volume: front-left: 65536 / 100% / 0.00 dB,   front-right: 65536 / 100% / 0.00 dB";;
esac
exit 0`)
	var warns, infos []string
	p := newPulseAudio(Config{
		AudioSink: "master",
		Warnf:     func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) },
		Infof:     func(format string, args ...any) { infos = append(infos, fmt.Sprintf(format, args...)) },
	})
	p.pinSinkUnity()
	if len(warns) != 0 {
		t.Fatalf("warnings = %q, want none", warns)
	}
	if len(infos) != 1 || !strings.Contains(infos[0], "pinned at unity") {
		t.Fatalf("informational logs = %q, want one successful pin", infos)
	}
	if !p.pinDone {
		t.Fatal("pinDone not set after a verified unity sink")
	}
}

// TestPulseWarningsWithoutHook discards diagnostics when no hook is wired: a
// Config built without a daemon must not write to the process logger or panic.
func TestPulseWarningsWithoutHook(t *testing.T) {
	p := newPulseAudio(Config{AudioSink: "master"})
	p.warnOnce("discarded")
	p.infof("discarded %d", 1)
	if p.cfg.Warnf != nil || p.cfg.Infof != nil {
		t.Fatal("default config carries a log hook")
	}
}
