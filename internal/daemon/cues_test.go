package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
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

// startupRingOwnership reads the introduction's current owner.
func startupRingOwnership(d *Daemon) (*presentations.Presentation, string) {
	d.startupMu.Lock()
	defer d.startupMu.Unlock()
	return d.startupPres, d.startupCapture
}

// TestStartupRingGeometryFollowsRingCapabilities derives the run from the
// advertised geometry instead of a fixed pixel list, and pins the blue-teal
// fade-in contract of the rendered effect.
func TestStartupRingGeometryFollowsRingCapabilities(t *testing.T) {
	for _, pixels := range []int{12, 6, 4, 1, 39, 48} {
		params, ok := startupRingParams(pixels)
		if !ok {
			t.Fatalf("%d-pixel ring: no startup parameters", pixels)
		}
		want := pixels / 3
		if want < 1 {
			want = 1
		}
		if want > startupRingMaxRun {
			want = startupRingMaxRun
		}
		if count, _ := params["count"].(int64); int(count) != want {
			t.Fatalf("%d-pixel ring: count = %v, want %d", pixels, params["count"], want)
		}
		if start, _ := params["start"].(int64); start != 0 {
			t.Fatalf("%d-pixel ring: start = %v, want the top pixel 0", pixels, params["start"])
		}
	}
	if _, ok := startupRingParams(0); ok {
		t.Fatal("a ring with no pixels accepted the introduction")
	}

	params, _ := startupRingParams(12)
	dark, err := presets.RingFrame(startupRingPreset, params, 12, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, px := range dark {
		if px != (backend.RGB{}) {
			t.Fatalf("t=0 pixel %d = %v, want the fade-in to start dark", i, px)
		}
	}
	lit, err := presets.RingFrame(startupRingPreset, params, 12, startupRingFadeIn)
	if err != nil {
		t.Fatal(err)
	}
	if lit[0].B <= lit[0].G || lit[0].G <= lit[0].R {
		t.Fatalf("top pixel %v is not blue-teal", lit[0])
	}
	for i := 4; i < len(lit); i++ {
		if lit[i] != (backend.RGB{}) {
			t.Fatalf("pixel %d = %v outside the derived four-pixel run", i, lit[i])
		}
	}
}

// TestStartupRingFadesAndRestoresBaseline drives the introduction on the
// simulator backend and asserts the observable frames: a full blue-teal run on
// the top of the ring, intermediate levels on both sides of the peak (a fade,
// not a snap), and the selected ring baseline selected again afterwards
// instead of a forced black frame.
func TestStartupRingFadesAndRestoresBaseline(t *testing.T) {
	base := shortTempDir(t)
	marker := filepath.Join(base, startupSoundMarker)
	if err := os.WriteFile(marker, []byte("played\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	ringID := d.ringResourceID()
	if ringID == "" {
		t.Fatal("no ring resource in the simulator profile")
	}
	waitForAppSelection(t, d, ringID, ringAppID)
	sim := d.dev.(*backend.Simulator)
	pixelCount := sim.Ring().Caps().PixelCount

	d.showStartupRing()
	pres, capture := startupRingOwnership(d)
	if pres == nil || capture == "" {
		t.Fatal("the startup ring did not take ring ownership")
	}
	defer d.stopStartupRing("test cleanup")
	waitFor(t, time.Second, "startup ring selected", func() bool {
		return d.router.Current(ringID).SelectedCaptureID == capture
	})

	var (
		levels    []int
		peak      int
		peakFrame backend.RingFrame
	)
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if d.router.Current(ringID).SelectedCaptureID != capture {
			break
		}
		frame := sim.RingSnapshot()
		if len(frame.Pixels) == pixelCount {
			// Blue is the largest channel of the blue-teal run, so it tracks
			// the indicator intensity across the fade.
			level := int(frame.Pixels[0].B)
			levels = append(levels, level)
			if level > peak {
				peak, peakFrame = level, frame
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if peak == 0 {
		t.Fatal("the introduction never lit the ring")
	}
	if peak < 200 {
		t.Fatalf("peak blue level %d, want a full-intensity frame", peak)
	}
	top := peakFrame.Pixels[0]
	if top.B <= top.G || top.G <= top.R {
		t.Fatalf("peak frame %v is not blue-teal", top)
	}
	run := pixelCount / 3
	if run < 1 {
		run = 1
	}
	for i, px := range peakFrame.Pixels {
		if i < run && px == (backend.RGB{}) {
			t.Fatalf("peak frame left derived pixel %d dark", i)
		}
		if i >= run && px != (backend.RGB{}) {
			t.Fatalf("peak frame lit pixel %d outside the derived run", i)
		}
	}

	peakIndex := 0
	for i, level := range levels {
		if level == peak {
			peakIndex = i
		}
	}
	var after []int
	for _, level := range levels[peakIndex:] {
		if level > 0 && level < peak-10 {
			after = append(after, level)
		}
	}
	distinct := map[int]bool{}
	for _, level := range after {
		distinct[level] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("fade-out levels after the peak = %v, want at least two intermediate steps", after)
	}
	fellBelowQuarter := false
	for _, level := range levels {
		if level > 0 && level < peak/4 {
			fellBelowQuarter = true
		}
	}
	if !fellBelowQuarter {
		t.Fatalf("levels %v never approached dark, so the introduction has no fade", levels)
	}

	// The release must return the selected baseline, not paint a black frame.
	waitFor(t, 3*time.Second, "startup ring release", func() bool {
		sel := d.router.Current(ringID)
		return sel.SelectedCaptureID == "" && sel.SelectedAppID == ringAppID
	})
	if _, capture := startupRingOwnership(d); capture != "" {
		t.Fatalf("startup capture %q outlived its introduction", capture)
	}
	if _, ok := d.pres.Get(pres.ID); ok {
		t.Fatalf("startup presentation %s leaked", pres.ID)
	}
	waitFor(t, 4*time.Second, "selected ring baseline frames", func() bool {
		for _, px := range sim.RingSnapshot().Pixels {
			if px != (backend.RGB{}) {
				return true
			}
		}
		return false
	})
}

// TestStartupRingFollowsStartupCueDecision pins the trigger: the installation
// boot shows it once with the cue, and a later boot shows it only while
// startup_sound_enabled is on.
func TestStartupRingFollowsStartupCueDecision(t *testing.T) {
	t.Run("installation boot", func(t *testing.T) {
		d := startEmptyDaemon(t, shortTempDir(t))
		waitFor(t, 5*time.Second, "installation startup ring", func() bool {
			_, capture := startupRingOwnership(d)
			return capture != ""
		})
		d.stopStartupRing("test cleanup")
	})

	t.Run("explicit no ring provider keeps a blank baseline", func(t *testing.T) {
		base := shortTempDir(t)
		values := settings.DefaultUserSettings()
		values.DefaultAppByResource = map[string]string{
			"display.segment.front": clockAppID,
			"light.ring.main":       "",
		}
		data, err := json.Marshal(struct {
			Revision uint64                `json:"revision"`
			Values   settings.UserSettings `json:"values"`
		}{Revision: 1, Values: values})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "settings.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		d := startEmptyDaemon(t, base)
		ringID := d.ringResourceID()
		waitForAppSelection(t, d, "display.segment.front", clockAppID)
		// Force the installation cue decision after boot recorded the marker.
		// The transient indication still uses the ring, exactly like the muted
		// microphone and volume indications; what must survive is the explicit
		// blank baseline once it releases.
		marker := filepath.Join(base, startupSoundMarker)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if due, _, err := d.startupCueDue(); err != nil || !due {
			t.Fatalf("startup cue due = %v (%v), want an installation-boot decision", due, err)
		}
		d.maybePlayStartupCue()
		waitFor(t, 2*time.Second, "introduction over the blank baseline", func() bool {
			_, capture := startupRingOwnership(d)
			return capture != ""
		})
		waitFor(t, 4*time.Second, "introduction released", func() bool {
			sel := d.router.Current(ringID)
			return sel.SelectedCaptureID == "" && sel.SelectedPresentation == ""
		})
		sim := d.dev.(*backend.Simulator)
		waitFor(t, 2*time.Second, "explicit blank baseline restored", func() bool {
			for _, px := range sim.RingSnapshot().Pixels {
				if px != (backend.RGB{}) {
					return false
				}
			}
			return true
		})
	})

	t.Run("later boot follows the setting", func(t *testing.T) {
		base := shortTempDir(t)
		marker := filepath.Join(base, startupSoundMarker)
		if err := os.WriteFile(marker, []byte("played\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		d := startEmptyDaemon(t, base)
		ringID := d.ringResourceID()
		waitForAppSelection(t, d, ringID, ringAppID)

		d.maybePlayStartupCue()
		time.Sleep(150 * time.Millisecond)
		if _, capture := startupRingOwnership(d); capture != "" {
			t.Fatal("a disabled startup sound showed the ring introduction")
		}

		if err := d.writeUserSettings([]string{"startup_sound_enabled"}, func(v *settings.UserSettings) {
			v.StartupSoundEnabled = true
		}); err != nil {
			t.Fatal(err)
		}
		d.maybePlayStartupCue()
		waitFor(t, 2*time.Second, "startup ring for the enabled cue", func() bool {
			_, capture := startupRingOwnership(d)
			return capture != ""
		})
		d.stopStartupRing("test cleanup")
	})
}

// TestStartupRingStaleFadeCannotTouchNewerOwner is the ownership regression: a
// replaced introduction and a cancelled introduction followed by a newer system
// indication both invalidate the fade generation, so no late step may release
// or blank the current owner.
func TestStartupRingStaleFadeCannotTouchNewerOwner(t *testing.T) {
	base := shortTempDir(t)
	marker := filepath.Join(base, startupSoundMarker)
	if err := os.WriteFile(marker, []byte("played\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := startEmptyDaemon(t, base)
	ringID := d.ringResourceID()
	waitForAppSelection(t, d, ringID, ringAppID)
	sim := d.dev.(*backend.Simulator)

	// Replacing the introduction while it runs: the first fade must not release
	// the second run.
	d.showStartupRing()
	_, first := startupRingOwnership(d)
	d.showStartupRing()
	_, second := startupRingOwnership(d)
	if first == "" || second == "" || first == second {
		t.Fatalf("replacement captures = %q, %q; want two distinct owners", first, second)
	}
	time.Sleep(startupRingHold + 2*startupRingStep)
	if _, capture := startupRingOwnership(d); capture != second {
		t.Fatalf("a stale fade released the current introduction: capture %q, want %q", capture, second)
	}
	waitFor(t, 2*time.Second, "replacement introduction selected", func() bool {
		return d.router.Current(ringID).SelectedCaptureID == second
	})

	// A newer system indication owns the ring after the introduction was
	// cancelled; the stale fade must neither blank nor release it.
	d.stopStartupRing("cancelled")
	d.startMicIndicator()
	d.indicatorMu.Lock()
	mic := d.micCaptureID
	d.indicatorMu.Unlock()
	if mic == "" {
		t.Fatal("the muted indication did not take ownership")
	}
	waitFor(t, 2*time.Second, "muted indication selected", func() bool {
		return d.router.Current(ringID).SelectedCaptureID == mic
	})
	time.Sleep(startupRingHold + 2*startupRingStep)
	frame := sim.RingSnapshot()
	d.indicatorMu.Lock()
	still := d.micCaptureID
	d.releaseMicIndicatorLocked("test cleanup")
	d.indicatorMu.Unlock()
	if still != mic {
		t.Fatal("a stale startup fade released the newer indication")
	}
	if len(frame.Pixels) == 0 || frame.Pixels[0] != (backend.RGB{}) {
		t.Fatalf("stale startup pixels still lit on the ring: %v", frame.Pixels)
	}
	if frame.Pixels[5].R == 0 {
		t.Fatalf("the newer indication frame was overwritten: %v", frame.Pixels)
	}
}

// slowBootManifest is a non-provider background app for the ordering test. It
// is spawned by startBootApps, which is where the test blocks the boot.
const slowBootManifest = `
id = "test.slowboot"
name = "Slow boot fixture"
version = "0.1.0"
background = true
start_on_boot = true
single_instance = true

[entrypoint]
runtime = "native"
path = "slowboot"
architecture = "aarch64"
abi = "linux-musl"
`

// TestStartupRingRendersBeforeSlowBootApps pins the physical-writer ordering:
// with the boot-app spawn deliberately blocked, the introduction's first frames
// must still reach the ring from the near-dark start of its fade-in. Before the
// output loop was started ahead of the introduction, this blocked boot spent
// the whole ~1.9 s lifetime before the first tick, so the device missed the
// fade-in or the entire introduction while the jingle played.
func TestStartupRingRendersBeforeSlowBootApps(t *testing.T) {
	base := shortTempDir(t)
	// Explicitly no providers keeps applySettings from launching anything
	// before the introduction; the installed boot app is the only Launch on
	// this boot, and the test holds pkgMu to block it after the introduction
	// has been created.
	values := settings.DefaultUserSettings()
	values.DefaultAppByResource = map[string]string{}
	values.DefaultAssistantAppID = ""
	data, err := json.Marshal(struct {
		Revision uint64                `json:"revision"`
		Values   settings.UserSettings `json:"values"`
	}{Revision: 1, Values: values})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	apps := filepath.Join(base, "apps")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	installApp(t, apps, "test.slowboot", "slowboot", buildProbe(t), slowBootManifest)

	cfg := DefaultConfig()
	cfg.DataDir = base
	cfg.RunDir = filepath.Join(base, "run")
	cfg.SocketPath = filepath.Join(cfg.RunDir, "hal.sock")
	cfg.AppsDir = apps
	cfg.AssetsDir = filepath.Join(base, "assets")
	cfg.WebUIAddr = ""
	cfg.SandboxMode = "none"
	cfg.NativeLibDir = nativeLibDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var unlocked sync.Once
	unlock := func() { unlocked.Do(d.pkgMu.Unlock) }
	t.Cleanup(func() {
		unlock()
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("daemon shutdown timed out")
		}
	})
	d.pkgMu.Lock()
	go func() { done <- d.Run() }()
	waitFor(t, 5*time.Second, "daemon socket", func() bool {
		_, err := os.Stat(cfg.SocketPath)
		return err == nil
	})

	ringID := d.ringResourceID()
	sim := d.dev.(*backend.Simulator)
	pixelCount := sim.Ring().Caps().PixelCount
	waitFor(t, 5*time.Second, "installation introduction while boot apps are blocked", func() bool {
		_, capture := startupRingOwnership(d)
		return capture != ""
	})

	sawFrame, minBlue := false, 255
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		sel := d.router.Current(ringID)
		if sel.SelectedAppID == routing.SystemAppID && sel.SelectedCaptureID != "" {
			if frame := sim.RingSnapshot(); len(frame.Pixels) == pixelCount {
				if blue := int(frame.Pixels[0].B); blue > 0 {
					sawFrame = true
					if blue < minBlue {
						minBlue = blue
					}
				}
			}
		}
		if sawFrame && minBlue <= 24 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawFrame {
		t.Fatal("the introduction produced no physical frame while boot app startup was blocked")
	}
	if minBlue > 60 {
		t.Fatalf("first introduction frames were already at blue=%d; the fade-in was clipped", minBlue)
	}
	waitFor(t, 2*time.Second, "full introduction frame while boot app startup is blocked", func() bool {
		frame := sim.RingSnapshot()
		return len(frame.Pixels) == pixelCount && frame.Pixels[0].B >= 200
	})

	unlock()
	waitFor(t, 4*time.Second, "introduction released after the boot continues", func() bool {
		_, capture := startupRingOwnership(d)
		return capture == ""
	})
}

// TestStartupCueMarkerConsumesTheIntroduction pins the once contract end to
// end: the installation marker is written only after the cue reached the
// mixer, and a later decision with the off-by-default setting shows neither the
// jingle nor the ring. The retry side (no marker after a failed audio open) is
// the same decision function and is covered by TestStartupCueFollowsMarkerAndSetting.
func TestStartupCueMarkerConsumesTheIntroduction(t *testing.T) {
	base := shortTempDir(t)
	d := startEmptyDaemon(t, base)
	marker := filepath.Join(base, startupSoundMarker)
	waitFor(t, 5*time.Second, "installation marker after the cue reached the mixer", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	if d.user.Get().Values.StartupSoundEnabled {
		t.Fatal("this test expects the off-by-default startup sound setting")
	}
	d.stopStartupRing("test cleanup")
	waitFor(t, 3*time.Second, "introduction released", func() bool {
		_, capture := startupRingOwnership(d)
		return capture == ""
	})

	d.maybePlayStartupCue()
	time.Sleep(200 * time.Millisecond)
	if _, capture := startupRingOwnership(d); capture != "" {
		t.Fatal("the recorded installation marker did not consume the introduction")
	}
}

// TestStartupRingReleasedOnShutdown proves the terminal path for an active
// introduction: shutdown releases the capture through the router, destroys the
// presentation, and leaves no selection behind.
func TestStartupRingReleasedOnShutdown(t *testing.T) {
	d, cancel, _ := newTestDaemonPrepared(t, func(d *Daemon) {
		marker := filepath.Join(d.cfg.DataDir, startupSoundMarker)
		if err := os.WriteFile(marker, []byte("started\n"), 0o644); err != nil {
			t.Error(err)
		}
	})
	ringID := d.ringResourceID()
	waitForAppSelection(t, d, ringID, ringAppID)
	d.showStartupRing()
	pres, capture := startupRingOwnership(d)
	if pres == nil || capture == "" {
		t.Fatal("the startup ring did not take ownership")
	}

	cancel()
	waitFor(t, 10*time.Second, "startup ring released on shutdown", func() bool {
		p, c := startupRingOwnership(d)
		return p == nil && c == ""
	})
	if _, ok := d.pres.Get(pres.ID); ok {
		t.Fatalf("startup presentation %s survived shutdown", pres.ID)
	}
	if sel := d.router.Current(ringID); sel.SelectedCaptureID == capture {
		t.Fatalf("startup capture %s is still selected after shutdown", capture)
	}
}
