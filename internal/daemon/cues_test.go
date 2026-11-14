package daemon

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/settings"
	"capsulad/internal/sounds"
)

func TestVolumeCellsRightAligned(t *testing.T) {
	if got := volumeCells(5); got[3] != volumeDigits[5] || got[2] != 0 || got[1] != 0 || got[0] != 0 {
		t.Fatalf("5%% = %v", got)
	}
	if got := volumeCells(35); got[2] != volumeDigits[3] || got[3] != volumeDigits[5] {
		t.Fatalf("35%% = %v", got)
	}
	if got := volumeCells(100); got[1] != volumeDigits[1] || got[2] != volumeDigits[0] || got[3] != volumeDigits[0] {
		t.Fatalf("100%% = %v", got)
	}
	if got := volumeCells(150); got[1] != volumeDigits[1] {
		t.Fatalf("150%% not clamped: %v", got)
	}
}

// The microphone indication must wait for the mixer to consume and drain the
// cue, and an inaudible configuration must be refused.
func TestMicrophoneCueIsConsumedBeforeCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	defer sim.Close()
	manager := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	d := &Daemon{ctx: ctx, audio: manager, dev: sim, logs: logs}

	manager.SetMasterVolume(0.5)
	if err := d.audibleMicIndication("test.app"); err != nil {
		t.Fatalf("microphone cue: %v", err)
	}
	raw := sim.Audio().(interface{ LastOutput() []byte }).LastOutput()
	peak := 0
	for i := 0; i+1 < len(raw); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(raw[i:])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	if peak == 0 {
		t.Fatal("the drained microphone cue produced no digital output")
	}

	manager.SetMasterVolume(0)
	if err := d.audibleMicIndication("test.app"); err == nil {
		t.Fatal("zero master volume was accepted as an audible indication")
	}
	manager.SetMasterVolume(0.5)
	manager.SetMuted(true)
	if err := d.audibleMicIndication("test.app"); err == nil {
		t.Fatal("a muted microphone was accepted as an audible indication")
	}
}

// TestStartupCueFollowsMarkerAndSetting covers the installation contract: the
// first boot on a data directory always plays the cue and records the marker;
// every later boot follows the off-by-default setting.
func TestStartupCueFollowsMarkerAndSetting(t *testing.T) {
	dir := t.TempDir()
	store, err := settings.OpenUser(filepath.Join(dir, "settings.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{DataDir: dir}, user: store}

	due, installation, err := d.startupCueDue()
	if err != nil {
		t.Fatal(err)
	}
	if !due || !installation {
		t.Fatalf("fresh data dir: due=%v installation=%v, want both true", due, installation)
	}

	if err := os.WriteFile(filepath.Join(dir, startupSoundMarker), []byte("played\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	due, installation, err = d.startupCueDue()
	if err != nil {
		t.Fatal(err)
	}
	if due || installation {
		t.Fatalf("same boot after the marker: due=%v installation=%v, want both false", due, installation)
	}

	snap := store.Get()
	patch := snap.Values
	patch.StartupSoundEnabled = true
	if _, err := store.Update(snap.Revision, patch, []string{"startup_sound_enabled"}); err != nil {
		t.Fatal(err)
	}
	due, installation, err = d.startupCueDue()
	if err != nil {
		t.Fatal(err)
	}
	if !due || installation {
		t.Fatalf("enabled later boot: due=%v installation=%v, want due only", due, installation)
	}
}

// TestUISoundReachesSimulatorOutput plays an embedded cue through the real
// mixer and asserts the digital output, so a decode, format or trim mistake
// fails here rather than only opening a stream that carries silence.
func TestUISoundReachesSimulatorOutput(t *testing.T) {
	ctx := context.Background()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	defer sim.Close()
	manager := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	if err := manager.Start(); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.SetMasterVolume(1)
	d := &Daemon{ctx: ctx, audio: manager, dev: sim, logs: logs}

	sink := sim.Audio().(interface{ LastOutput() []byte })
	d.playUISound(sounds.MicrophoneLive())
	waitFor(t, 2*time.Second, "cue output", func() bool {
		raw := sink.LastOutput()
		for i := 0; i+1 < len(raw); i += 2 {
			if binary.LittleEndian.Uint16(raw[i:]) != 0 {
				return true
			}
		}
		return false
	})
	peak := 0
	for raw, i := sink.LastOutput(), 0; i+1 < len(raw); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(raw[i:])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	// info.wav peaks at 23170 before the -8 dB UI trim, at unity master gain.
	if peak < 6000 || peak > 13000 {
		t.Fatalf("cue peak %d, want the trimmed authored level", peak)
	}
	waitFor(t, 2*time.Second, "cue stream closed", func() bool {
		return len(manager.ActiveStreams()) == 0
	})
}
