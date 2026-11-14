package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

		stopSpeech := startSpeechInjector(t, d)
		waitFor(t, 2*time.Second, "teal input-level ring", func() bool {
			return ringShowsTeal(d, 30)
		})
		waitFor(t, 2*time.Second, "speech detected", func() bool {
			return strings.Count(logText(d), "speech detected") >= attempt
		})
		// Let voiced time exceed minSpeech (140 ms) before the injector stops;
		// otherwise the short burst is rejected and no end-of-speech follows.
		time.Sleep(300 * time.Millisecond)
		stopSpeech()

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

// Intended contract for the reported regression — a substantial colored room
// (fan/AC hum plus low-frequency rumble whose level swings) around a real
// utterance:
//
//	trigger: the wakeword activation, then a voice in the 300-3400 Hz band whose
//	  level is modulated, mixed over that room.
//	feedback: teal level ring while listening; after the voice stops, 700 ms of
//	  unmodulated room audio ends listening and starts the blue delay chase.
//	owner: the assistant's turn; capture, ring and conversation are released at
//	  the end of the turn.
//	interruption: the same gap/mute/dismiss/cap behaviour as any turn.
//	dismissal: room audio is never an utterance; a noise-only hypothesis is
//	  rejected without a reply.
//	final state: exactly one reply, every stream released, microphone closed.
//
// The old detector needed a high zero-crossing rate to call audio stationary
// background, so this room stayed "speech" until the listen cap.
func TestDumbAssistantEndsAfterSpeechOverColoredRoom(t *testing.T) {
	d, appID := startDumbAssistant(t)

	// One timeline: the room already running before the activation (so the
	// daemon's pre-roll carries the room, as it does in the real room), then
	// the utterance over the same room, then the room again. 1.2 s pre-roll +
	// 0.6 s room + 1.2 s speech + 3 s room.
	room := roomNoisePCMFrames(16000, 16000)
	speech := speechPCMFrames(16000, 16000*12/10)
	over := mixPCMStreams(speech, roomNoisePCMFrames(16000, len(speech)/2))
	timeline := append(append(append([]byte{}, room[:16000*2]...), room[:16000*2*6/10]...), over...)
	timeline = append(timeline, roomNoisePCMFrames(16000, 16000*3)...)
	stopSpeech := startPCMInjector(t, d, timeline)
	defer stopSpeech()
	time.Sleep(1100 * time.Millisecond)

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

	waitFor(t, 3*time.Second, "speech detected over the room", func() bool {
		return strings.Contains(logText(d), "speech detected")
	})
	waitSpeechEnd(t, d, 1)
	if strings.Contains(logText(d), "steady room noise; speech hypothesis rejected") &&
		!strings.Contains(logText(d), "speech ended") {
		t.Fatalf("the room was rejected as noise instead of ending real speech:\n%s", logText(d))
	}
	waitFor(t, 5*time.Second, "reply playback open", func() bool {
		return playbackOpen(d, appID)
	})
	waitFor(t, 5*time.Second, "reply playback finished", func() bool {
		return !playbackOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
	waitFor(t, 3*time.Second, "microphone stream closed", func() bool {
		return !inputOpen(d, appID)
	})
	if got := strings.Count(logText(d), "playing reply for activation="); got != 1 {
		t.Fatalf("log shows %d replies, want 1:\n%s", got, logText(d))
	}
}

// Colored room noise with no utterance must never produce a reply. The turn is
// bounded by the no-speech window or the listen cap, and every stream is
// released.
func TestDumbAssistantColoredRoomNoiseProducesNoReply(t *testing.T) {
	d, appID := startDumbAssistant(t)
	// Fresh daemon and app: no capture has existed yet, so the drop total (and
	// any retained snapshot) must already be zero before a capture is created.
	// This baseline cannot hide an unobserved early drop.
	if drops := d.audio.CaptureDroppedFrames(appID); drops != 0 {
		t.Fatalf("fresh app already reports %d dropped frames before any capture", drops)
	}
	// The fan/AC room runs before the activation so the pre-roll carries it.
	stopRoom := startPCMInjectorConsumed(t, d, roomNoisePCMFrames(16000, 16000*6))
	defer stopRoom()
	time.Sleep(1100 * time.Millisecond)
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
	// Cumulative capture drops for this app: the negative must prove the app
	// heard a continuous room, not that its stream was dropped into an abort.
	// The active count must still be zero (no drop between capture creation and
	// this read); the post-close read below must also be zero.
	if dropsBefore := d.audio.CaptureDroppedFrames(appID); dropsBefore != 0 {
		t.Fatalf("the app capture dropped %d frames before the room was observed", dropsBefore)
	}

	// Structural evidence that the app actually heard the room: its teal level
	// ring is driven by the PCM it receives. A capture that dropped its stream
	// before classification would leave the level at silence and could satisfy
	// the no-speech/cap exit vacuously.
	waitFor(t, 3*time.Second, "teal level from the room audio", func() bool {
		return ringShowsTeal(d, 20)
	})

	// The turn must end by the app's own no-speech/cap path, observed
	// structurally: the app closes its capture and releases the ring. A reply
	// would appear as an active playback stream instead.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if playbackOpen(d, appID) {
			stopRoom()
			t.Fatalf("colored room noise produced reply playback; daemon log:\n%s", logText(d))
		}
		if !inputOpen(d, appID) {
			break
		}
		if time.Now().After(deadline) {
			stopRoom()
			t.Fatalf("noise-only turn did not finish (capture still open); daemon log:\n%s", logText(d))
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopRoom()
	// Read the capture-drop total after the capture is gone: the accessor
	// retains the removed capture's cumulative total for its app, so a drop in
	// the final chunks before close cannot be lost to polling. The negative is
	// valid only when the app heard a continuous room (total zero).
	if drops := d.audio.CaptureDroppedFrames(appID); drops != 0 {
		t.Fatalf("the app capture dropped %d frames during the noise-only turn", drops)
	}
	if playbackOpen(d, appID) {
		t.Fatal("colored room noise produced reply playback")
	}
	if owner := d.router.Current("light.ring.main").SelectedAppID; owner == appID {
		t.Fatal("ring ownership was not released after the noise-only turn")
	}
	if _, ok := d.sup.ReadyRun(appID); !ok {
		t.Fatal("the assistant is no longer ready after the noise-only turn")
	}
}

// The simulator answers an empty capture queue with a silent read, so a starved
// producer splices silence into the room stream. That splice is the only
// nondeterminism a loaded host can add to these acoustic scenarios, and it must
// never become a reply: the pause is a level drop, the room is rejected, and the
// turn ends through the app's own safety window.
//
// Trigger: sustained colored room whose producer stalls 250 ms after 1 s.
// Feedback: teal listening. Owner: the assistant turn. Interruption: the app's
// no-speech window or 10 s cap. Final state: no reply, streams released.
func TestDumbAssistantRoomSpliceDoesNotReply(t *testing.T) {
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

	stopRoom := startPCMInjectorStalled(t, d, roomNoisePCMFrames(16000, 16000*6), 50, 250*time.Millisecond)
	defer stopRoom()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if playbackOpen(d, appID) || strings.Contains(logText(d), "playing reply for activation=") {
			stopRoom()
			t.Fatalf("a room-stream splice produced a reply; daemon log:\n%s", logText(d))
		}
		text := logText(d)
		if strings.Contains(text, "no speech within") || strings.Contains(text, "listen window elapsed") {
			break
		}
		if time.Now().After(deadline) {
			stopRoom()
			t.Fatalf("room-splice turn did not finish; daemon log:\n%s", text)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopRoom()
	if strings.Contains(logText(d), "playing reply for activation=") {
		t.Fatal("room-stream splice produced a reply log entry")
	}
	waitFor(t, 3*time.Second, "ring released", func() bool {
		return strings.Contains(logText(d), "ring released")
	})
	waitFor(t, 3*time.Second, "microphone stream closed", func() bool {
		return !inputOpen(d, appID)
	})
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
// framework release event, not on a silence window. It uses the real talk key,
// which the framework labels as a touch-origin activation, so the app must
// honor the hold-to-talk setting for physical keys too: after speech stops, the
// key is still held and no reply may start. A wakeword activation is delivered
// while the key is still held, so the release must be routed to the original
// key activation rather than whichever activation was delivered last.
func TestDumbAssistantTalkReleaseEndsTurn(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if err := d.writeUserSettings([]string{"touch_hold_mode"}, func(v *settings.UserSettings) {
		v.TouchHoldMode = settings.HoldToTalk
	}); err != nil {
		t.Fatal(err)
	}

	d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
	waitFor(t, 3*time.Second, "ring captured for listening", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID
	})
	waitFor(t, 3*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, appID)
	})
	waitAssistantListening(t, d, 1)
	stopSpeech := startSpeechInjector(t, d)
	defer stopSpeech()
	waitFor(t, 2*time.Second, "speech detected", func() bool {
		return strings.Contains(logText(d), "speech detected")
	})
	// Let voiced time exceed minSpeech (140 ms), then stop speaking while the
	// key remains held. A turn that ignored the hold-to-talk mode would end on
	// the 700 ms silence and open its reply after the configured delay.
	time.Sleep(300 * time.Millisecond)
	stopSpeech()
	quiet := time.Now()
	for time.Since(quiet) < 2600*time.Millisecond {
		if playbackOpen(d, appID) {
			t.Fatalf("key turn ended on silence before release; daemon log:\n%s", logText(d))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Deliver a wakeword activation while the key turn is held and listening.
	// The release below belongs to the circle key; a release path that tags the
	// most recently delivered activation would end the wrong turn and leave
	// this one listening until the safety cap.
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}

	released := time.Now()
	d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: false}})
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

// TestDumbAssistantHoldModeTapDoesNotActivate proves a sub-threshold panel tap
// in hold-to-talk mode is not a turn. The framework activates on a hold, not on
// a tap, so the assistant must not claim the ring or open the microphone.
func TestDumbAssistantHoldModeTapDoesNotActivate(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if err := d.writeUserSettings([]string{"touch_hold_mode"}, func(v *settings.UserSettings) {
		v.TouchHoldMode = settings.HoldToTalk
	}); err != nil {
		t.Fatal(err)
	}

	d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "primary", Pressed: true}})
	time.Sleep(100 * time.Millisecond)
	d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "primary", Pressed: false}})

	// Wait past the hold threshold so a late activation would be visible.
	time.Sleep(1200 * time.Millisecond)
	if got := d.router.Current("light.ring.main").SelectedAppID; got == appID {
		t.Fatalf("hold-mode tap captured the ring; selected app is %q", got)
	}
	if inputOpen(d, appID) {
		t.Fatal("hold-mode tap opened the microphone")
	}
	if text := logText(d); strings.Contains(text, "ACTIVATION_TOUCH queued") || strings.Contains(text, "microphone listening") {
		t.Fatalf("hold-mode tap activated the assistant:\n%s", text)
	}
}

// TestDumbAssistantUnpairedTouchActivationCancels proves a touch activation
// whose release never arrives still cancels on the no-speech window. This is
// the lost-release safety net: without it an otherwise silent hold-to-talk turn
// would keep the ring and microphone claimed for the full listen cap.
func TestDumbAssistantUnpairedTouchActivationCancels(t *testing.T) {
	d, appID := startDumbAssistant(t)
	if err := d.writeUserSettings([]string{"touch_hold_mode"}, func(v *settings.UserSettings) {
		v.TouchHoldMode = settings.HoldToTalk
	}); err != nil {
		t.Fatal(err)
	}

	// A manual touch activation with no coordinating release simulates a lost
	// TouchRelease for the active turn.
	if _, ok := d.wake.Trigger("touch", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator touch trigger failed")
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
	if strings.Contains(logText(d), "listen window elapsed") {
		t.Fatal("lost-release turn reached the listen cap instead of the no-speech window")
	}
	if playbackOpen(d, appID) {
		t.Fatal("lost-release turn produced reply playback")
	}
	waitFor(t, 3*time.Second, "microphone stream closed", func() bool {
		return !inputOpen(d, appID)
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
	stopSpeech := startSpeechInjector(t, d)
	defer stopSpeech()
	capDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(capDeadline) && !strings.Contains(logText(d), "listen window elapsed") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logText(d), "listen window elapsed") {
		t.Fatalf("listen cap cancellation not logged; daemon log:\n%s", logText(d))
	}
	stopSpeech()
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
	stopSpeech := startSpeechInjector(t, d)
	defer stopSpeech()
	waitFor(t, 2*time.Second, "speech detected", func() bool {
		return strings.Contains(logText(d), "speech detected")
	})
	// Let voiced time exceed minSpeech (140 ms) before the injector stops.
	time.Sleep(300 * time.Millisecond)
	stopSpeech()
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
// overflows the real transport with voiced PCM while the consumer is paused,
// then resumes speech and waits past the configured delay with no playback.
// Only genuine end-of-speech may finish the turn.
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

	lead := startSpeechInjector(t, d)
	waitFor(t, 2*time.Second, "speech before the gap", func() bool {
		return strings.Contains(logText(d), "speech detected")
	})
	time.Sleep(300 * time.Millisecond)
	lead()

	// Pause the real consumer while a voiced burst exceeds the transport
	// window. Preserved pre-gap chunks must remain voiced: enough known
	// silence before the gap would legitimately end the utterance.
	run, ok := d.sup.ReadyRun(appID)
	if !ok {
		t.Fatal("assistant is not running")
	}
	process, err := os.FindProcess(run.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	paused := true
	defer func() {
		if paused {
			_ = process.Signal(syscall.SIGCONT)
		}
	}()
	frames, unsubscribe := d.audio.SubscribeFrames(8192)
	defer unsubscribe()
	const burstFrames = 16000 * 120
	sim.InjectPCM(repeatPCM(speechPCMFrames(16000, 16000*2), burstFrames))
	drained := 0
	// The full 120 s burst must still be drained: that is what overflows the
	// paused assistant's transport and produces the drop/gap assertion. The
	// processed capture tap and the race detector both add per-chunk dispatch
	// cost, so the old 2 s deadline measured the fixture's throughput rather
	// than the product. This remains a bounded wait, not an unbounded one.
	deadline := time.After(20 * time.Second)
	for drained < burstFrames {
		select {
		case f := <-frames:
			drained += len(f.PCM) / 2
		case <-deadline:
			t.Fatal("capture source did not deliver the queued burst")
		}
	}
	if err := process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	paused = false
	resumed := startSpeechInjector(t, d)
	defer resumed()
	gapDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(gapDeadline) && !strings.Contains(logText(d), "audio gap") {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logText(d), "audio gap") {
		t.Fatalf("audio gap not observed; daemon log:\n%s", logText(d))
	}
	gapAt := time.Now()
	for time.Now().Before(gapAt.Add(1500 * time.Millisecond)) {
		if playbackOpen(d, appID) {
			t.Fatalf("audio gap started a reply; daemon log:\n%s", logText(d))
		}
		time.Sleep(20 * time.Millisecond)
	}

	resumed()
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
	// A wait only; the delay is measured after the end is observed. Five
	// seconds keeps host contention (e.g. a concurrent cross-build) from
	// looking like a missing end.
	deadline := time.Now().Add(5 * time.Second)
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

// tonePCMFrames builds a steady 440 Hz tone. It remains the reply-asset
// waveform (its duration is all those tests need); microphone fixtures use
// speechPCMFrames, because the assistant measures a *modulated* speech band and
// a steady tone is deliberately not speech evidence.
func tonePCMFrames(rate, frames int) []byte {
	pcm := make([]byte, frames*2)
	for i := range frames {
		sample := int16(9000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
	}
	return pcm
}

// pcmAtLevel normalizes samples to the requested full-scale RMS and encodes
// 16-bit little-endian mono PCM.
func pcmAtLevel(samples []float64, level float64) []byte {
	sum := 0.0
	for _, s := range samples {
		sum += s * s
	}
	scale := 1.0
	if rms := math.Sqrt(sum / float64(len(samples))); rms > 0 {
		scale = level / rms
	}
	pcm := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := s * scale
		if v > 1 {
			v = 1
		}
		if v < -1 {
			v = -1
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(v*32767)))
	}
	return pcm
}

// speechPCMFrames renders deterministic, copyright-free speech-like audio for
// the simulator microphone: a glottal harmonic stack with a moving spectral
// tilt, a 4.5 Hz syllable envelope, and periodic fricative bursts. That is the
// structure the assistant's speech-band modulation test is about; a pure tone
// has none of it.
func speechPCMFrames(rate, frames int) []byte {
	samples := make([]float64, frames)
	rng := rand.New(rand.NewPCG(0x51ed270b, 0xc2b2ae35))
	var phase float64
	for i := range samples {
		t := float64(i) / float64(rate)
		f0 := 118 + 10*math.Sin(2*math.Pi*0.9*t)
		phase += 2 * math.Pi * f0 / float64(rate)
		tilt := 0.85 + 0.45*math.Sin(2*math.Pi*1.3*t+0.8)
		src := 0.0
		for k := 1; k <= 40; k++ {
			if 130*k > rate/2 {
				break
			}
			src += math.Sin(float64(k)*phase+float64(k)*0.7) / math.Pow(float64(k), tilt)
		}
		env := math.Max(0, math.Sin(2*math.Pi*4.5*t+0.2))
		samples[i] = math.Pow(env, 0.5) * src
	}
	burst := rate * 50 / 1000
	for start := rate * 60 / 1000; start < frames; start += rate / 4 {
		for j := 0; j < burst && start+j < frames; j++ {
			samples[start+j] += 0.6 * (rng.Float64()*2 - 1) * float64(burst-j) / float64(burst)
		}
	}
	return pcmAtLevel(samples, 0.2)
}

// roomNoisePCMFrames renders a colored, non-stationary room: a low-frequency
// harmonic hum with slow wobble, shaped hiss, and fan buffeting that moves the
// level by several times. It is the ambient class that made the old
// zero-crossing escape useless.
func roomNoisePCMFrames(rate, frames int) []byte {
	samples := make([]float64, frames)
	rng := rand.New(rand.NewPCG(0x9e3779b9, 0x85ebca6b))
	var shaped [3]float64
	for i := range samples {
		t := float64(i) / float64(rate)
		hum := 0.0
		for k, tone := range [][2]float64{{95, 1}, {145, .7}, {205, .5}, {310, .35}, {430, .25}, {620, .15}} {
			wobble := 1 + 0.02*math.Sin(2*math.Pi*0.11*t+float64(k)*1.7)
			hum += tone[1] * math.Sin(2*math.Pi*tone[0]*wobble*t+float64(k)*1.3)
		}
		white := rng.Float64()*2 - 1
		for k := range shaped {
			shaped[k] += (white - shaped[k]) * 0.25
		}
		// A synthetic fan/AC room matched to the authorized capture's band
		// statistics: band/full RMS 0.39, 160 ms band-level relative deviation
		// 0.118 median / 0.180 at the 90th percentile. (That capture is external
		// music, not a fan and not speech-free; only its level statistics are
		// borrowed here, not a noise label.) Fan buffeting moves the low hum,
		// in-band air movement wanders the hiss, and the room breathes slightly.
		am := 1 + 0.28*math.Sin(2*math.Pi*0.17*t) + 0.18*math.Sin(2*math.Pi*3.7*t+1.1)
		wander := 1 + 0.4*math.Sin(2*math.Pi*0.23*t+0.7) + 0.2*math.Sin(2*math.Pi*4.3*t+2.1)
		glob := 1 + 0.25*math.Sin(2*math.Pi*0.31*t+1.9) + 0.15*math.Sin(2*math.Pi*6.7*t)
		// The authorized capture's 99th-percentile deviation came from music
		// from external speakers, not from the fan this fixture models; its
		// p50/p90 (matched above) are the noise profile to preserve.
		samples[i] = (hum*am + shaped[2]*wander) * glob
	}
	return pcmAtLevel(samples, 0.06)
}

// mixPCMStreams adds two PCM streams over their common length, clamping to full
// scale. Speech over the same room is the reported real-world case.
func mixPCMStreams(a, b []byte) []byte {
	n := min(len(a), len(b))
	out := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		sum := int32(int16(binary.LittleEndian.Uint16(a[i:]))) +
			int32(int16(binary.LittleEndian.Uint16(b[i:])))
		if sum > math.MaxInt16 {
			sum = math.MaxInt16
		}
		if sum < math.MinInt16 {
			sum = math.MinInt16
		}
		binary.LittleEndian.PutUint16(out[i:], uint16(int16(sum)))
	}
	return out
}

// repeatPCM tiles pcm to exactly frames samples.
func repeatPCM(pcm []byte, frames int) []byte {
	out := make([]byte, frames*2)
	for i := range frames {
		out[i*2] = pcm[(i*2)%len(pcm)]
		out[i*2+1] = pcm[(i*2+1)%len(pcm)]
	}
	return out
}

// micFrameBytes is one processed capture frame at 16 kHz: 20 ms.
const micFrameBytes = 16000 * 20 / 1000 * 2

// injectorLead bounds the unread audio queued in the simulator. The simulator
// answers a read with a silent frame after a 20 ms timeout when its queue is
// empty, so a wall-clock paced producer can splice silence into the stream when
// the host is loaded — that splice, not the assistant, is the only
// nondeterminism in these acoustic scenarios. Keeping a small consumption-
// driven lead means the capture pipeline always has the next frame queued.
const injectorLead = 4

// startPCMInjector streams pcm into the simulator microphone, looping, under a
// bounded consumption-driven lead. The producer queues at most injectorLead
// frames (bounded memory and latency), refills one frame for each frame the
// capture pipeline actually consumed, and blocks when the lead is full
// (backpressure, not an unbounded burst). The audio therefore advances with
// consumption instead of racing a wall-clock timer.
// startPCMInjector streams pcm in real time (10 ms steps) for the speech
// journeys, where the application must keep up with continuous audio and its
// bounded capture queue drops rather than blocking. A wall-clock producer can
// be starved under host load; the no-reply journeys use the consumption-driven
// injector below instead, and the deliberate-stall test bounds that hazard.
func startPCMInjector(t *testing.T, d *Daemon, pcm []byte) func() {
	t.Helper()
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatalf("test daemon backend is %T, want *backend.Simulator", d.dev)
	}
	const stepBytes = 16000 * 10 / 1000 * 2 // 10 ms of audio
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		offset := 0
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			end := offset + stepBytes
			var chunk []byte
			if end <= len(pcm) {
				chunk = pcm[offset:end]
				offset = end
			} else {
				chunk = append(append([]byte{}, pcm[offset:]...), pcm[:end-len(pcm)]...)
				offset = end - len(pcm)
			}
			sim.InjectPCM(chunk)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

// startPCMInjectorConsumed streams pcm under a bounded consumption-driven lead;
// the no-reply journeys use it because it cannot splice silence into the room
// under host load.
func startPCMInjectorConsumed(t *testing.T, d *Daemon, pcm []byte) func() {
	return startPCMInjectorStalled(t, d, pcm, 0, 0)
}

// startPCMInjectorStalled is startPCMInjector with one deliberate producer
// stall after stallAfterChunks frames. The simulator answers the resulting
// empty queue with silent reads, which reproduces the only splice a loaded host
// can create; acoustic tests use it to prove that splice cannot become a reply.
func startPCMInjectorStalled(t *testing.T, d *Daemon, pcm []byte, stallAfterChunks int, stallFor time.Duration) func() {
	t.Helper()
	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatalf("test daemon backend is %T, want *backend.Simulator", d.dev)
	}
	if len(pcm) < micFrameBytes {
		t.Fatalf("injector fixture is %d bytes, shorter than one %d-byte frame", len(pcm), micFrameBytes)
	}
	frames, unsubscribe := d.audio.SubscribeFrames(256)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer unsubscribe()
		pos := 0
		injected, consumed := 0, 0
		emit := func() {
			end := pos + micFrameBytes
			var chunk []byte
			if end <= len(pcm) {
				chunk = pcm[pos:end]
				pos = end
			} else {
				chunk = append(append([]byte{}, pcm[pos:]...), pcm[:end-len(pcm)]...)
				pos = end - len(pcm)
			}
			sim.InjectPCM(chunk)
			injected++
		}
		prime := min(injectorLead, len(pcm)/micFrameBytes)
		for i := 0; i < prime; i++ {
			emit()
		}
		// Both a bounded lead and a rate limit: emit at most one 20 ms frame per
		// 18 ms (just faster than the source's 20 ms silent-read timeout, so the
		// queue cannot empty into a splice, and slow enough that the application
		// consumer's bounded capture queue does not drop). The lead still absorbs
		// short producer stalls.
		ticker := time.NewTicker(18 * time.Millisecond)
		defer ticker.Stop()
		stalled := false
		for {
			select {
			case <-stop:
				return
			case <-frames:
				consumed++
				continue
			case <-ticker.C:
			}
			if stallFor > 0 && !stalled && injected >= stallAfterChunks {
				stalled = true
				time.Sleep(stallFor)
			}
			if injected-consumed < injectorLead {
				emit()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

// startSpeechInjector streams the speech fixture until the returned stop
// function is called.
func startSpeechInjector(t *testing.T, d *Daemon) func() {
	return startPCMInjector(t, d, speechPCMFrames(16000, 16000*2))
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

// The circle/play-pause key dismisses an in-flight assistant turn instead of
// starting another one. It must stop listening or playback immediately and
// release the ring and microphone, leaving the assistant ready for the next
// activation.
func TestDumbAssistantPrimaryKeyDismissesTurn(t *testing.T) {
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

	d.input.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
	time.Sleep(60 * time.Millisecond)
	d.input.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: false}})

	waitFor(t, 3*time.Second, "assistant dismiss action", func() bool {
		return strings.Contains(logText(d), "assistant turn dismissed by the circle key") &&
			strings.Contains(logText(d), "dismissed")
	})
	waitFor(t, 3*time.Second, "ring released after dismissal", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID != appID
	})
	waitFor(t, 3*time.Second, "microphone stream closed after dismissal", func() bool {
		return !inputOpen(d, appID)
	})
	if playbackOpen(d, appID) {
		t.Fatal("dismissed turn opened reply playback")
	}
	if got := strings.Count(logText(d), "playing reply for activation="); got != 0 {
		t.Fatalf("dismissed turn played %d replies:\n%s", got, logText(d))
	}
	if _, ok := d.sup.ReadyRun(appID); !ok {
		t.Fatal("assistant was not ready after dismissal")
	}
}

// A killed assistant must come back by itself and accept a new activation.
// Previously the lost bus connection was treated as a deliberate stop, so the
// run never restarted and both wakeword and manual activation had no target.
func TestDumbAssistantKilledMidTurnRestartsAndActivates(t *testing.T) {
	d, appID := startDumbAssistant(t)
	first, ok := d.sup.ReadyRun(appID)
	if !ok {
		t.Fatal("assistant is not ready")
	}
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	waitFor(t, 3*time.Second, "microphone stream open before the kill", func() bool {
		return inputOpen(d, appID)
	})

	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill assistant: %v", err)
	}
	replacement := ""
	waitFor(t, 10*time.Second, "assistant restarts after being killed", func() bool {
		run, ok := d.sup.ReadyRun(appID)
		if !ok || run.RunID == first.RunID {
			return false
		}
		replacement = run.RunID
		return true
	})
	if replacement == "" {
		t.Fatal("no replacement run")
	}
	waitFor(t, 3*time.Second, "killed turn released its microphone stream", func() bool {
		return !inputOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "killed turn released the ring", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID != appID
	})

	// The restarted process must be usable: a fresh activation opens a real
	// microphone stream and reaches the listening state again.
	if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
		t.Fatal("wakeword trigger after restart failed")
	}
	waitFor(t, 3*time.Second, "restarted assistant listens", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == appID && inputOpen(d, appID)
	})
	waitFor(t, 3*time.Second, "second listening turn", func() bool {
		return strings.Count(logText(d), "microphone listening") >= 2
	})
}
