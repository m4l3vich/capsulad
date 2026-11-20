package daemon

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/settings"
)

// The AirPlay volume fixture uses the generic probe binary under the real
// receiver ID. It connects and reports READY, which is all the authorization
// path needs; the sender/session policy is the app's responsibility.
const airPlayTestManifest = `
id = "org.capsulad.airplay"
name = "AirPlay receiver fixture"
version = "0.1.0"
background = false
restart_on_failure = false

[entrypoint]
runtime = "native"
path = "airplay"
architecture = "aarch64"
abi = "linux-musl"
`

func airPlayVolumeContext(runID string, pid int, withCreds bool) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("capsulad-run-id", runID))
	if withCreds {
		ctx = grpcpeer.WithCreds(ctx, grpcpeer.Creds{PID: pid, OK: true})
	}
	return ctx
}

func setMasterVolumeRPC(d *Daemon, ctx context.Context, volume float32) (*frameworkv1.AudioState, error) {
	return (&audioService{d: d}).SetMasterVolume(ctx,
		&frameworkv1.SetMasterVolumeRequest{Volume: volume})
}

func airPlayVolumeDaemon(t *testing.T) *Daemon {
	t.Helper()
	probe := buildProbe(t)
	d, _, _ := newTestDaemonPrepared(t, func(d *Daemon) {
		// The installation cue is a mixer source of its own; without the
		// marker it can overlap the digital-output measurement.
		if err := os.WriteFile(filepath.Join(d.cfg.DataDir, startupSoundMarker),
			[]byte("test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}, appInstall{
		id: airPlayReceiverAppID, binaryName: "airplay", binaryPath: probe,
		manifest: []byte(airPlayTestManifest),
	})
	waitReady(t, d, clockAppID, 10*time.Second)
	return d
}

// TestAirPlayMasterVolumeAuthorization exercises the daemon gate through the
// public AudioService handler. The accepted call is a live READY AirPlay run
// whose socket PID matches; the persisted setting and the real simulator mixer
// output are both checked. Every other identity is denied before any write.
func TestAirPlayMasterVolumeAuthorization(t *testing.T) {
	d := airPlayVolumeDaemon(t)

	// A live run that has not completed APP_READY is not an authorization
	// source even when its app ID and PID match. A native app is a singleton,
	// so the not-ready fixture must be the app's only run: exercise it before
	// the ready receiver starts. Start a real child that never connects to the
	// bus and leave it parked in Starting.
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "airplay-not-ready")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notReadyManifest := &manifest.Manifest{
		ID: airPlayReceiverAppID, Name: "AirPlay not ready", Version: "0.1.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeNative, Path: filepath.Base(script)},
	}
	notReadyManifest.Dir = scriptDir
	beforeNotReady := d.user.Get().Values.MasterVolume
	starting, spawned, err := d.sup.StartIfAbsent(notReadyManifest, "test-airplay-not-ready")
	if err != nil || !spawned {
		t.Fatalf("start not-ready fixture: spawned=%v err=%v", spawned, err)
	}
	if _, err := setMasterVolumeRPC(d, airPlayVolumeContext(starting.RunID, starting.PID, true), 0.5); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("not-ready AirPlay run code = %v, want PermissionDenied", status.Code(err))
	}
	if got := d.user.Get().Values.MasterVolume; got != beforeNotReady {
		t.Fatalf("not-ready AirPlay run changed durable master from %v to %v", beforeNotReady, got)
	}
	if !d.sup.Stop(starting.RunID) {
		t.Fatal("stop not-ready fixture")
	}
	waitFor(t, 5*time.Second, "not-ready fixture removed", func() bool {
		_, found := d.sup.Get(starting.RunID)
		return !found
	})

	airRunID, _, spawned, err := d.Launch(airPlayReceiverAppID, map[string]string{})
	if err != nil || !spawned {
		t.Fatalf("launch AirPlay fixture: spawned=%v err=%v", spawned, err)
	}
	waitReady(t, d, airPlayReceiverAppID, 10*time.Second)
	airRun, ok := d.sup.Get(airRunID)
	if !ok {
		t.Fatal("AirPlay fixture run disappeared after ready")
	}

	otherRunID, _, spawned, err := d.Launch("test.probe", map[string]string{"mode": "serve"})
	if err != nil || !spawned {
		t.Fatalf("launch other app: spawned=%v err=%v", spawned, err)
	}
	waitReady(t, d, "test.probe", 10*time.Second)
	otherRun, ok := d.sup.Get(otherRunID)
	if !ok {
		t.Fatal("other app run disappeared")
	}

	const level = 0.25
	state, err := setMasterVolumeRPC(d, airPlayVolumeContext(airRun.RunID, airRun.PID, true), level)
	if err != nil {
		t.Fatalf("ready AirPlay volume accepted: %v", err)
	}
	if state.GetMasterVolume() != level {
		t.Fatalf("returned master = %v, want %v", state.GetMasterVolume(), level)
	}
	if got := d.user.Get().Values.MasterVolume; got != level {
		t.Fatalf("persisted master = %v, want %v", got, level)
	}
	if got := d.audio.MasterVolume(); got != level {
		t.Fatalf("mixer master = %v, want %v", got, level)
	}

	// Digital proof: a full-scale stream from the built-in clock app goes
	// through the same mixer and must peak at the new master, not at the
	// previous device level.
	tail, ok := d.dev.Audio().(outputTail)
	if !ok {
		t.Fatal("simulator audio has no LastOutput")
	}
	client := newBuiltinAppClient(d, clockAppID)
	defer client.Close()
	stream := openVolumeStream(t, client, d.audio.PlaybackFormat())
	sendAndDrain(t, stream, fullScaleS16(24000))
	time.Sleep(50 * time.Millisecond)
	peak := outputPeak(tail.LastOutput())
	wantPeak := s16Code(audio.MasterVolumeGain(level))
	if diff := peak - wantPeak; diff < -2 || diff > 2 {
		t.Fatalf("master %v sink peak %d, want tapered gain %d ±2", level, peak, wantPeak)
	}

	before := d.user.Get().Values.MasterVolume
	deny := func(name string, ctx context.Context) {
		t.Helper()
		if _, err := setMasterVolumeRPC(d, ctx, 0.5); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s: code = %v, want PermissionDenied", name, status.Code(err))
		}
		if got := d.user.Get().Values.MasterVolume; got != before {
			t.Fatalf("%s changed persisted master from %v to %v", name, before, got)
		}
		if got := d.audio.MasterVolume(); got != before {
			t.Fatalf("%s changed mixer master from %v to %v", name, before, got)
		}
	}

	deny("other app", airPlayVolumeContext(otherRun.RunID, otherRun.PID, true))
	deny("mismatched peer", airPlayVolumeContext(airRun.RunID, otherRun.PID, true))
	deny("unknown run", airPlayVolumeContext("run_unknown_airplay", airRun.PID, true))
	deny("no peer credentials", airPlayVolumeContext(airRun.RunID, airRun.PID, false))

	// A request queued behind the canonical write lock must observe its own
	// expired context after it acquires the lock, not persist a late level.
	expiredCtx, cancelExpired := context.WithCancel(
		airPlayVolumeContext(airRun.RunID, airRun.PID, true))
	expiredDone := make(chan error, 1)
	d.volumeMu.Lock()
	go func() {
		_, err := setMasterVolumeRPC(d, expiredCtx, 0.9)
		expiredDone <- err
	}()
	select {
	case err := <-expiredDone:
		d.volumeMu.Unlock()
		cancelExpired()
		t.Fatalf("expired writer completed while volumeMu was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancelExpired()
	d.volumeMu.Unlock()
	if err := <-expiredDone; status.Code(err) != codes.Canceled {
		t.Fatalf("expired queued writer code = %v, want Canceled", status.Code(err))
	}
	if got := d.user.Get().Values.MasterVolume; got != before {
		t.Fatalf("expired queued writer changed durable master from %v to %v", before, got)
	}
	if got := d.audio.MasterVolume(); got != before {
		t.Fatalf("expired queued writer changed mixer master from %v to %v", before, got)
	}

	// A queued writer must not persist after the AirPlay owner is stopped
	// while it waits. Stop the run under the held write lock, then let the
	// writer acquire it and observe that the READY peer identity is gone.
	beforeStale := d.user.Get().Values.MasterVolume
	takeoverDone := make(chan error, 1)
	d.volumeMu.Lock()
	go func() {
		_, err := setMasterVolumeRPC(d,
			airPlayVolumeContext(airRun.RunID, airRun.PID, true), 0.95)
		takeoverDone <- err
	}()
	select {
	case err := <-takeoverDone:
		d.volumeMu.Unlock()
		t.Fatalf("takeover writer completed while volumeMu was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if !d.sup.Stop(airRun.RunID) {
		d.volumeMu.Unlock()
		t.Fatal("stop AirPlay fixture")
	}
	d.volumeMu.Unlock()
	if err := <-takeoverDone; status.Code(err) != codes.PermissionDenied {
		t.Fatalf("queued writer after takeover code = %v, want PermissionDenied", status.Code(err))
	}
	if got := d.user.Get().Values.MasterVolume; got != beforeStale {
		t.Fatalf("queued writer after takeover changed durable master from %v to %v", beforeStale, got)
	}
	if got := d.audio.MasterVolume(); got != beforeStale {
		t.Fatalf("queued writer after takeover changed mixer master from %v to %v", beforeStale, got)
	}
	waitFor(t, 5*time.Second, "AirPlay fixture removed", func() bool {
		return len(d.sup.List(airPlayReceiverAppID)) == 0
	})
	if _, err := setMasterVolumeRPC(d,
		airPlayVolumeContext(airRun.RunID, airRun.PID, true), 0.5); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stale AirPlay run code = %v, want PermissionDenied", status.Code(err))
	}
	if got := d.user.Get().Values.MasterVolume; got != beforeStale {
		t.Fatalf("stale AirPlay run changed durable master from %v to %v", beforeStale, got)
	}
}

// TestApplyLatestMasterVolumeUsesDurableLevel isolates the watcher's live
// apply from the running settings watcher: the store has a newer level than
// the simulated stale live state, and the applySettings helper must read the
// durable field. It deliberately does not call applySettings on a live daemon,
// because that function owns the watcher's lastApplied state.
func TestApplyLatestMasterVolumeUsesDurableLevel(t *testing.T) {
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sim := backend.NewSimulator(backend.SimulatorOptions{})
	mgr := audio.NewManager(ctx, clock, logs, sim.Audio(), audio.Options{})
	t.Cleanup(mgr.Close)
	store, err := settings.OpenUser("", logs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetMasterVolume(0.7, nil); err != nil {
		t.Fatal(err)
	}
	mgr.SetMasterVolume(0.3) // an older live apply that is now stale
	d := &Daemon{audio: mgr, user: store}
	d.applyLatestMasterVolume()
	if got := mgr.MasterVolume(); got != 0.7 {
		t.Fatalf("latest durable master applied %v, want 0.7", got)
	}
	if got := store.Get().Values.MasterVolume; got != 0.7 {
		t.Fatalf("durable master = %v, want 0.7", got)
	}
}

// TestSetMasterVolumeRejectsNonFinite keeps the administrator path out of the
// durable store for values that cannot represent a level. NaN in particular
// passes a plain range comparison.
func TestSetMasterVolumeRejectsNonFinite(t *testing.T) {
	d, cancel, _ := newTestDaemon(t)
	defer cancel()
	waitReady(t, d, clockAppID, 10*time.Second)

	before := d.user.Get().Values.MasterVolume
	for _, level := range []float32{
		float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), -0.01, 1.01,
	} {
		if _, err := setMasterVolumeRPC(d, context.Background(), level); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("volume %v code = %v, want InvalidArgument", level, status.Code(err))
		}
	}
	if got := d.user.Get().Values.MasterVolume; got != before {
		t.Fatalf("invalid volume changed durable master from %v to %v", before, got)
	}
	if got := d.audio.MasterVolume(); got != before {
		t.Fatalf("invalid volume changed mixer master from %v to %v", before, got)
	}
}
