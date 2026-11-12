package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/settings"
)

const dumbAssistantAppID = "org.capsula.dumbassistant"

// TestDumbAssistantSpeechEndedReply installs the real optional package, selects
// it as the voice assistant, feeds real PCM through the simulator's capture
// path, and proves that the delay begins only after speech ends. It also checks
// the teal level ring, the blue delay chase, the configured alternate asset,
// synchronous settings rejection and full capture/presentation cleanup.
func TestDumbAssistantSpeechEndedReply(t *testing.T) {
	binary := buildModuleApp(t, "apps/dumbassistant")
	root := moduleRoot(t)
	manifest, err := os.ReadFile(filepath.Join(root, "apps", "dumbassistant", "manifest.toml"))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := os.ReadFile(filepath.Join(root, "apps", "dumbassistant", "assets", "reply.wav"))
	if err != nil {
		t.Fatal(err)
	}
	alt := toneWAV(16000, 16000*300/1000)
	altDuration := wavDuration(t, alt)

	d, _, _ := newTestDaemonWith(t, appInstall{
		id: dumbAssistantAppID, binaryName: "dumbassistant", binaryPath: binary,
		manifest: manifest, files: map[string][]byte{"assets/reply.wav": reply},
	})
	store := d.cfg.AssetsDir
	if err := os.MkdirAll(filepath.Join(store, "audio", "assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alt.wav", "undeclared.wav"} {
		if err := os.WriteFile(filepath.Join(store, "audio", "assistant", name), alt, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storeManifest := `{"assets":[
		{"id":"assistant.alt","path":"audio/assistant/alt.wav","media_type":"audio/wav"}
	]}`
	if err := os.WriteFile(filepath.Join(store, "manifest.json"), []byte(storeManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	conn := dialTest(t, d.cfg.SocketPath)
	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = dumbAssistantAppID
	}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, d, dumbAssistantAppID, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := frameworkv1.NewApplicationConfigurationServiceClient(conn)
	snapshot, err := cfg.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: dumbAssistantAppID})
	if err != nil {
		t.Fatal(err)
	}

	rejected := []struct{ key, value string }{
		{"delay", jsonString("900-100")},
		{"delay", jsonString("60001")},
		{"delay", jsonString("abc")},
		{"reply_asset", jsonString("/etc/passwd")},
		{"reply_asset", jsonString("../escape.wav")},
		{"reply_asset", jsonString("audio/assistant/missing.wav")},
		{"reply_asset", jsonString("audio/assistant/undeclared.wav")},
	}
	for _, change := range rejected {
		_, err := cfg.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{
			AppId: dumbAssistantAppID, ExpectedRevision: snapshot.GetRevision(),
			Changes: []*frameworkv1.AppSettingChange{{Key: change.key, ValueJson: change.value}},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("Update(%s=%s) error = %v, want InvalidArgument", change.key, change.value, err)
		}
	}
	unchanged, err := cfg.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: dumbAssistantAppID})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.GetRevision() != snapshot.GetRevision() {
		t.Fatalf("rejected updates advanced revision %d -> %d", snapshot.GetRevision(), unchanged.GetRevision())
	}

	const delayMS = 700
	if _, err := cfg.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{
		AppId: dumbAssistantAppID, ExpectedRevision: snapshot.GetRevision(),
		Changes: []*frameworkv1.AppSettingChange{
			{Key: "delay", ValueJson: jsonString("700")},
			{Key: "reply_asset", ValueJson: jsonString("audio/assistant/alt.wav")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "assistant settings applied", func() bool {
		text := logText(d)
		return strings.Contains(text, "delay=700ms") && strings.Contains(text, "asset=audio/assistant/alt.wav")
	})

	for attempt := 1; attempt <= 2; attempt++ {
		if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
			t.Fatal("simulator wakeword trigger failed")
		}
		if playbackOpen(d, dumbAssistantAppID) {
			t.Fatalf("attempt %d: playback started before speech", attempt)
		}
		waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
			return d.router.Current("light.ring.main").SelectedAppID == dumbAssistantAppID
		})
		waitFor(t, 3*time.Second, "microphone stream open", func() bool {
			return inputOpen(d, dumbAssistantAppID)
		})
		waitAssistantListening(t, d, attempt)

		sim, ok := d.dev.(*backend.Simulator)
		if !ok {
			t.Fatalf("test daemon backend is %T, want *backend.Simulator", d.dev)
		}
		stopTone := startToneInjector(sim, 100)
		waitFor(t, 2*time.Second, "teal input-level ring", func() bool {
			return ringShowsTeal(d, 30)
		})
		waitFor(t, 2*time.Second, "speech detected", func() bool {
			return strings.Count(logText(d), "speech detected") >= attempt
		})
		// Let voiced time exceed minSpeech (140 ms) before the injector stops;
		// otherwise the short burst is rejected and no end-of-speech follows.
		time.Sleep(300 * time.Millisecond)
		stopTone()

		waitSpeechEnd(t, d, attempt)
		speechEnded := time.Now()
		waitFor(t, 2*time.Second, "blue delay chase", func() bool {
			return ringShowsBlue(d)
		})

		waitFor(t, 5*time.Second, "reply playback open", func() bool {
			return playbackOpen(d, dumbAssistantAppID)
		})
		if elapsed := time.Since(speechEnded); elapsed < (delayMS-250)*time.Millisecond {
			t.Fatalf("attempt %d: reply opened %s after speech end, delay is %d ms", attempt, elapsed, delayMS)
		}
		opened := time.Now()
		waitFor(t, 5*time.Second, "reply playback finished", func() bool {
			return !playbackOpen(d, dumbAssistantAppID)
		})
		if played := time.Since(opened); played < altDuration/2 {
			t.Fatalf("attempt %d: reply stream lasted %s; configured clip is %s", attempt, played, altDuration)
		}
		waitFor(t, 3*time.Second, "ring released", func() bool {
			return strings.Count(logText(d), "ring released") >= attempt
		})
		waitFor(t, 3*time.Second, "microphone stream closed", func() bool {
			return !inputOpen(d, dumbAssistantAppID)
		})
	}

	text := logText(d)
	if got := strings.Count(text, "playing reply for activation="); got != 2 {
		t.Fatalf("log shows %d replies, want 2:\n%s", got, text)
	}
	if got := strings.Count(text, "ring listening"); got != 2 {
		t.Fatalf("log shows %d listening rings, want 2:\n%s", got, text)
	}
	if got := strings.Count(text, "ring released"); got != 2 {
		t.Fatalf("log shows %d ring releases, want 2:\n%s", got, text)
	}
	if got := strings.Count(text, "speech ended"); got != 2 {
		t.Fatalf("log shows %d speech ends, want 2:\n%s", got, text)
	}
}

// TestDumbAssistantNoSpeechCancels proves silence never becomes a reply and
// never leaves the capture or ring claimed.
func TestDumbAssistantNoSpeechCancels(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, appID)
	})
	waitAssistantListening(t, d, 1)
	waitFor(t, 6*time.Second, "no speech cancellation", func() bool {
		return strings.Contains(logText(d), "no speech within")
	})
	if playbackOpen(d, appID) {
		t.Fatal("silence produced reply playback")
	}
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
	waitFor(t, 3*time.Second, "microphone stream closed", func() bool {
		return !inputOpen(d, appID)
	})
	if strings.Contains(logText(d), "playing reply for activation=") {
		t.Fatal("silence produced a reply log entry")
	}
}

// TestDumbAssistantMuteCancels proves a global mute during listening aborts
// the turn cleanly instead of leaving a stuck microphone capture.
func TestDumbAssistantMuteCancels(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, appID)
	})
	waitAssistantListening(t, d, 1)
	d.setMuted(true)
	t.Cleanup(func() { d.setMuted(false) })
	waitFor(t, 4*time.Second, "mute cancellation", func() bool {
		text := logText(d)
		return strings.Contains(text, "microphone notice MICROPHONE_MUTED") && strings.Contains(text, "cancelled before reply")
	})
	if playbackOpen(d, appID) {
		t.Fatal("muted microphone produced reply playback")
	}
	waitFor(t, 3*time.Second, "microphone stream closed", func() bool {
		return !inputOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
}

// TestDumbAssistantTalkReleaseEndsTurn proves a hold-to-talk turn ends on the
// framework release event, not on a silence window: the injected tone never
// stops before release.
func TestDumbAssistantTalkReleaseEndsTurn(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if err := d.writeUserSettings([]string{"touch_hold_mode"}, func(v *settings.UserSettings) {
		v.TouchHoldMode = settings.HoldToTalk
	}); err != nil {
		t.Fatal(err)
	}

	d.simTouchSignal(backend.InputEvent{Gesture: &backend.GestureEvent{ID: "hold", Phase: "start"}})
	waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, appID)
	})
	waitAssistantListening(t, d, 1)
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatalf("test daemon backend is %T, want *backend.Simulator", d.dev)
	}
	stopTone := startToneInjector(sim, 200)
	defer stopTone()
	waitFor(t, 2*time.Second, "speech detected", func() bool {
		return strings.Contains(logText(d), "speech detected")
	})

	released := time.Now()
	d.simTouchSignal(backend.InputEvent{Gesture: &backend.GestureEvent{ID: "hold", Phase: "end"}})
	stopTone()
	waitFor(t, 2*time.Second, "talk button release", func() bool {
		return strings.Contains(logText(d), "talk button released")
	})
	waitFor(t, 5*time.Second, "reply playback open", func() bool {
		return playbackOpen(d, appID)
	})
	if elapsed := time.Since(released); elapsed < 750*time.Millisecond {
		t.Fatalf("reply opened %s after release; default delay is 1000 ms", elapsed)
	}
	waitFor(t, 5*time.Second, "reply playback finished", func() bool {
		return !playbackOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
}

// TestDumbAssistantContinuousSpeechCancels proves the 10 s listen cap never
// pretends continuous speech finished: it cancels without a reply.
func TestDumbAssistantContinuousSpeechCancels(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, appID)
	})
	waitAssistantListening(t, d, 1)
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatalf("test daemon backend is %T, want *backend.Simulator", d.dev)
	}
	stopTone := startToneInjector(sim, 200)
	defer stopTone()
	capDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(capDeadline) && !strings.Contains(logText(d), "listen window elapsed") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logText(d), "listen window elapsed") {
		t.Fatalf("listen cap cancellation not logged; daemon log:\n%s", logText(d))
	}
	stopTone()
	if playbackOpen(d, appID) {
		t.Fatal("continuous speech at the listen cap produced a reply")
	}
	waitFor(t, 3*time.Second, "microphone stream closed", func() bool {
		return !inputOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
	if strings.Contains(logText(d), "playing reply for activation=") {
		t.Fatal("continuous speech produced a reply log entry")
	}
}

// TestDumbAssistantZeroDelayTransitionsDuringReply proves empty delay adds no
// wait while the teal-to-blue transition still runs, continuing into playback.
func TestDumbAssistantZeroDelayTransitionsDuringReply(t *testing.T) {
	d, appID := startDumbAssistant(t)
	conn := dialTest(t, d.cfg.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := frameworkv1.NewApplicationConfigurationServiceClient(conn)
	snapshot, err := cfg.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: appID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{
		AppId: appID, ExpectedRevision: snapshot.GetRevision(),
		Changes: []*frameworkv1.AppSettingChange{{Key: "delay", ValueJson: jsonString("")}},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "empty delay applied", func() bool {
		return strings.Contains(logText(d), "delay=none")
	})

	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, appID)
	})
	waitAssistantListening(t, d, 1)
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatalf("test daemon backend is %T, want *backend.Simulator", d.dev)
	}
	stopTone := startToneInjector(sim, 100)
	defer stopTone()
	waitFor(t, 2*time.Second, "speech detected", func() bool {
		return strings.Contains(logText(d), "speech detected")
	})
	// Let voiced time exceed minSpeech (140 ms) before the injector stops.
	time.Sleep(300 * time.Millisecond)
	stopTone()
	waitSpeechEnd(t, d, 1)
	speechEnded := time.Now()
	sawIntermediate := false
	var openedAt time.Time
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if openedAt.IsZero() && playbackOpen(d, appID) {
			openedAt = time.Now()
		}
		if intermediate, _ := ringBlendStates(d); intermediate {
			sawIntermediate = true
		}
		if !openedAt.IsZero() && sawIntermediate {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if openedAt.IsZero() {
		t.Fatal("empty delay did not open reply playback")
	}
	if elapsed := openedAt.Sub(speechEnded); elapsed >= 250*time.Millisecond {
		t.Fatalf("empty delay waited %s for its transition", elapsed)
	}
	if !sawIntermediate {
		t.Fatal("zero-delay reply skipped the intermediate teal-to-blue ring frame")
	}
	waitFor(t, 2*time.Second, "fully blue chase during reply", func() bool {
		_, fullBlue := ringBlendStates(d)
		return playbackOpen(d, appID) && fullBlue
	})
	waitFor(t, 5*time.Second, "reply playback finished", func() bool {
		return !playbackOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
}

// TestDumbAssistantAudioGapDoesNotReply proves a dropped-audio gap while
// speech status is unknown does not start the reply path. It deliberately
// overflows the capture queue with silence after a voiced burst, waits past
// the configured delay with no playback, then speaks and quiets normally to
// finish the turn.
func TestDumbAssistantAudioGapDoesNotReply(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, appID)
	})
	waitAssistantListening(t, d, 1)
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatalf("test daemon backend is %T, want *backend.Simulator", d.dev)
	}

	tone := startToneInjector(sim, 100)
	waitFor(t, 2*time.Second, "speech before the gap", func() bool {
		return strings.Contains(logText(d), "speech detected")
	})
	tone()

	// A silence burst bigger than the capture queue forces dropped-frame
	// chunks; the tracker is active from the tone, so the gap is exactly the
	// uncertain-status case the app must not treat as end-of-speech.
	sim.InjectPCM(make([]byte, 16000*2*60))
	gapDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(gapDeadline) && !strings.Contains(logText(d), "audio gap") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logText(d), "audio gap") {
		t.Fatalf("audio gap not observed; daemon log:\n%s", logText(d))
	}
	speech := startToneInjector(sim, 200)
	defer speech()
	gapAt := time.Now()
	for time.Now().Before(gapAt.Add(1500 * time.Millisecond)) {
		if playbackOpen(d, appID) {
			t.Fatalf("audio gap started a reply; daemon log:\n%s", logText(d))
		}
		time.Sleep(20 * time.Millisecond)
	}

	speech()
	endDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(endDeadline) && !strings.Contains(logText(d), "speech ended") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logText(d), "speech ended") {
		t.Fatalf("speech end after gap not logged; daemon log:\n%s", logText(d))
	}
	waitFor(t, 5*time.Second, "reply after real speech", func() bool {
		return playbackOpen(d, appID)
	})
	waitFor(t, 5*time.Second, "reply playback finished", func() bool {
		return !playbackOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
}

func startDumbAssistant(t *testing.T) (*Daemon, string) {
	t.Helper()
	binary := buildModuleApp(t, "apps/dumbassistant")
	root := moduleRoot(t)
	manifest, err := os.ReadFile(filepath.Join(root, "apps", "dumbassistant", "manifest.toml"))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := os.ReadFile(filepath.Join(root, "apps", "dumbassistant", "assets", "reply.wav"))
	if err != nil {
		t.Fatal(err)
	}
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: dumbAssistantAppID, binaryName: "dumbassistant", binaryPath: binary,
		manifest: manifest, files: map[string][]byte{"assets/reply.wav": reply},
	})
	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = dumbAssistantAppID
	}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, d, dumbAssistantAppID, 10*time.Second)
	return d, dumbAssistantAppID
}

func inputOpen(d *Daemon, appID string) bool {
	for _, stream := range d.audio.ActiveStreams() {
		if stream.Input && stream.AppID == appID {
			return true
		}
	}
	return false
}

func waitAssistantListening(t *testing.T, d *Daemon, count int) {
	t.Helper()
	waitFor(t, 3*time.Second, "assistant microphone listening", func() bool {
		return strings.Count(logText(d), "microphone listening") >= count
	})
}

func waitSpeechEnd(t *testing.T, d *Daemon, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && strings.Count(logText(d), "speech ended") < count {
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Count(logText(d), "speech ended") < count {
		t.Fatalf("speech end %d not detected; daemon log:\n%s", count, logText(d))
	}
}

func playbackOpen(d *Daemon, appID string) bool {
	for _, stream := range d.audio.ActiveStreams() {
		if !stream.Input && stream.AppID == appID {
			return true
		}
	}
	return false
}

func ringShowsTeal(d *Daemon, minGreen uint8) bool {
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		return false
	}
	for _, px := range sim.RingSnapshot().Pixels {
		if px.R == 0 && px.G >= minGreen && px.G == px.B {
			return true
		}
	}
	return false
}

func ringShowsBlue(d *Daemon) bool {
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		return false
	}
	for _, px := range sim.RingSnapshot().Pixels {
		if px.B > px.G && px.B > 0 {
			return true
		}
	}
	return false
}

// ringBlendStates classifies the rendered ring by the blue/green ratio, which
// is independent of per-pixel chase brightness. Intermediate frames sit
// between pure teal (ratio 1) and fully blended blue (ratio ~3.98).
func ringBlendStates(d *Daemon) (intermediate, fullBlue bool) {
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		return false, false
	}
	for _, px := range sim.RingSnapshot().Pixels {
		if px.G < 10 || px.B <= px.G {
			continue
		}
		ratio := float64(px.B) / float64(px.G)
		if ratio > 1.02 && ratio < 3.5 {
			intermediate = true
		}
		if ratio >= 3.5 {
			fullBlue = true
		}
	}
	return intermediate, fullBlue
}

func jsonString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// tonePCM builds real 16 kHz mono 16-bit PCM for the simulator microphone.
func tonePCM(rate, durationMS int) []byte {
	return tonePCMFrames(rate, rate*durationMS/1000)
}

func tonePCMFrames(rate, frames int) []byte {
	pcm := make([]byte, frames*2)
	for i := range frames {
		sample := int16(9000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	return pcm
}

// startToneInjector feeds short tone steps in real time. The simulator source
// returns queued bytes as fast as it is read, so one large blob becomes a burst
// that overflows the capture queue and looks like a gap; paced steps model a
// continuously speaking microphone instead.
func startToneInjector(sim *backend.Simulator, stepMS int) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			sim.InjectPCM(tonePCM(16000, stepMS))
			time.Sleep(time.Duration(stepMS) * time.Millisecond)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

// toneWAV wraps tone PCM in a PCM 16-bit mono WAV so tests can install a real
// alternate reply asset with a different duration.
func toneWAV(rate, frames int) []byte {
	pcm := tonePCMFrames(rate, frames)
	data := make([]byte, 44+len(pcm))
	copy(data[0:], "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+len(pcm)))
	copy(data[8:], "WAVE")
	copy(data[12:], "fmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], uint32(rate))
	binary.LittleEndian.PutUint32(data[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(pcm)))
	copy(data[44:], pcm)
	return data
}

func wavDuration(t *testing.T, data []byte) time.Duration {
	t.Helper()
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatal("reply asset is not RIFF/WAVE")
	}
	var channels, rate, bits, pcmBytes int
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + size
		if end > len(data) {
			t.Fatalf("reply asset has a truncated %q chunk", id)
		}
		switch id {
		case "fmt ":
			if size >= 16 {
				channels = int(binary.LittleEndian.Uint16(data[start+2 : start+4]))
				rate = int(binary.LittleEndian.Uint32(data[start+4 : start+8]))
				bits = int(binary.LittleEndian.Uint16(data[start+14 : start+16]))
			}
		case "data":
			pcmBytes = size
		}
		offset = end + size%2
	}
	if channels <= 0 || rate <= 0 || bits <= 0 || pcmBytes <= 0 {
		t.Fatal("reply asset has an unusable fmt/data chunk")
	}
	frames := pcmBytes / (channels * bits / 8)
	return time.Duration(frames) * time.Second / time.Duration(rate)
}
