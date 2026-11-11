package daemon

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
)

func TestRecordingLabel(t *testing.T) {
	tests := []struct {
		in     string
		stored string
		speech bool
	}{
		{"word", "word", true},
		{"speech", "background", true},
		{"phrase", "background", true},
		{"room", "background", false},
		{"background", "background", false},
		{"", "word", true},
	}
	for _, tt := range tests {
		stored, speech, err := recordingLabel(tt.in)
		if err != nil {
			t.Fatalf("recordingLabel(%q): %v", tt.in, err)
		}
		if stored != tt.stored || speech != tt.speech {
			t.Fatalf("recordingLabel(%q) = %q, %v; want %q, %v", tt.in, stored, speech, tt.stored, tt.speech)
		}
	}
	if _, _, err := recordingLabel("nonsense"); err == nil {
		t.Fatal("unknown label was accepted")
	}
}

// TestCountdownCuePCM checks the audible countdown spans three seconds with
// two short tones and one longer tone, matching the browser CUE_MS offset.
func TestCountdownCuePCM(t *testing.T) {
	format := backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
	pcm := countdownCuePCM(format)
	const wantFrames = 48000 * 3000 / 1000
	if got := len(pcm) / (format.Channels * 2); got != wantFrames {
		t.Fatalf("cue has %d frames, want %d", got, wantFrames)
	}

	var runs [][2]int
	const blockFrames = 480 // 10 ms
	start := -1
	for b := range wantFrames / blockFrames {
		peak := 0
		for frame := b * blockFrames; frame < (b+1)*blockFrames; frame++ {
			v := int(int16(binary.LittleEndian.Uint16(pcm[frame*format.Channels*2:])))
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		loud := peak > 500
		if loud && start < 0 {
			start = b
		}
		if !loud && start >= 0 {
			runs = append(runs, [2]int{start, b - start})
			start = -1
		}
	}
	if start >= 0 {
		runs = append(runs, [2]int{start, wantFrames/blockFrames - start})
	}
	if len(runs) != 3 {
		t.Fatalf("cue has %d audible tones, want 3: %v", len(runs), runs)
	}
	if runs[0][1] >= runs[2][1] || runs[1][1] >= runs[2][1] {
		t.Fatalf("last tone is not the longest: %v", runs)
	}
	if gap := runs[1][0] - (runs[0][0] + runs[0][1]); gap < 84 || gap > 92 {
		t.Fatalf("gap between tones is %d blocks, want about 88", gap)
	}
}

func TestPlayRecordingCueDrains(t *testing.T) {
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
	d := &Daemon{ctx: ctx, audio: manager, dev: sim, logs: logs}
	errCh := make(chan error, 1)
	go func() { errCh <- d.playRecordingCue(ctx) }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("recording cue did not finish playing")
	}
}

func TestRecordingIndicatorSelectsAndReleasesRing(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	pres := presentations.NewManager(clock, logs, nil)
	router := routing.New(clock, logs, pres)
	const ringID = "light.ring.main"
	router.SetResourceClass(ringID, "ring")
	d := &Daemon{
		clock: clock, logs: logs, pres: pres, router: router,
		resources: map[string]*frameworkv1.ResourceInfo{
			ringID: {ResourceId: ringID, Kind: frameworkv1.ResourceKind_RESOURCE_LED_RING},
		},
	}
	d.beginRecordingIndicator()
	sel := router.Current(ringID)
	if sel.SelectedCaptureID == "" || sel.SelectedPresentation == "" {
		t.Fatalf("recording indicator was not selected: %+v", sel)
	}
	p, ok := pres.Get(sel.SelectedPresentation)
	if !ok || !p.Visible() {
		t.Fatal("recording indicator presentation is not visible")
	}
	d.endRecordingIndicator()
	if sel := router.Current(ringID); sel.SelectedCaptureID != "" {
		t.Fatalf("recording indicator capture survived release: %+v", sel)
	}
}

// cuePeak plays the countdown at the given master volume and returns the
// largest absolute sample the mixer wrote to the virtual sink.
func cuePeak(t *testing.T, volume float64) int {
	t.Helper()
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
	manager.SetMasterVolume(volume)
	d := &Daemon{ctx: ctx, audio: manager, dev: sim, logs: logs}
	if err := d.playRecordingCue(ctx); err != nil {
		t.Fatal(err)
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
	return peak
}

func TestRecordingCueRespectsMasterVolume(t *testing.T) {
	low := cuePeak(t, 0.25)
	high := cuePeak(t, 1.0)
	if low == 0 || high == 0 {
		t.Fatalf("cue produced no audio (low=%d high=%d)", low, high)
	}
	ratio := float64(high) / float64(low)
	if ratio < 3.5 || ratio > 4.5 {
		t.Fatalf("cue does not track master volume: low=%d high=%d ratio=%.2f, want about 4", low, high, ratio)
	}
}
