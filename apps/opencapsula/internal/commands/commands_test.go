package commands

import (
	"testing"

	"capsulad/sdk/capsulad"
)

func TestCapabilitiesAreTheImplementedActions(t *testing.T) {
	want := []string{
		"volume.get", "media.control", "media.status", "conversation.end",
	}
	if len(Capabilities) != len(want) {
		t.Fatalf("capabilities = %v", Capabilities)
	}
	for i := range want {
		if Capabilities[i] != want[i] {
			t.Fatalf("capabilities = %v", Capabilities)
		}
	}
}

func TestVolumePercent(t *testing.T) {
	cases := []struct {
		master float32
		want   int
	}{
		{0, 0}, {0.004, 0}, {0.006, 1}, {0.42, 42}, {0.999, 100},
		{1, 100}, {-1, 0}, {2, 100},
	}
	for _, tc := range cases {
		if got := VolumePercent(tc.master); got != tc.want {
			t.Errorf("VolumePercent(%v) = %d, want %d", tc.master, got, tc.want)
		}
	}
}

func TestBluetoothCommandMapping(t *testing.T) {
	cases := map[string]capsulad.BluetoothMediaCommand{
		"play":     capsulad.BluetoothMediaPlay,
		"pause":    capsulad.BluetoothMediaPause,
		"toggle":   capsulad.BluetoothMediaToggle,
		"next":     capsulad.BluetoothMediaNext,
		"previous": capsulad.BluetoothMediaPrevious,
	}
	for operation, want := range cases {
		got, err := BluetoothCommand(operation)
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", operation, got, err, want)
		}
	}
	for _, operation := range []string{"stop", "", "rewind"} {
		if _, err := BluetoothCommand(operation); err == nil {
			t.Errorf("%q accepted", operation)
		}
	}
}

func TestMediaDataReportsAbsenceHonestly(t *testing.T) {
	data := MediaData(capsulad.BluetoothMediaState{Present: false, Detail: "no source"})
	if data["present"] != false || data["playing"] != false ||
		data["source"] != "bluetooth" || data["detail"] != "no source" {
		t.Fatalf("data = %+v", data)
	}
	playing := MediaData(capsulad.BluetoothMediaState{
		Present: true, Playing: true, Title: "Song", DirectionKnown: true, DirectionPlaying: false,
	})
	if playing["playing"] != false {
		t.Fatalf("direction should win over source status: %+v", playing)
	}
}
