package daemon

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/settings"
)

const dumbAssistantAppID = "org.capsula.dumbassistant"

// TestDumbAssistantDelayedStaticReply installs the real optional package,
// selects it as the voice assistant, and proves that activations are answered
// only after the configured delay and that each reply streams for the packaged
// clip's duration.
func TestDumbAssistantDelayedStaticReply(t *testing.T) {
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
	clipDuration := wavDuration(t, reply)
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: dumbAssistantAppID, binaryName: "dumbassistant", binaryPath: binary,
		manifest: manifest, files: map[string][]byte{"assets/reply.wav": reply},
	})
	conn := dialTest(t, d.cfg.SocketPath)
	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = dumbAssistantAppID
	}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, d, dumbAssistantAppID, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := frameworkv1.NewApplicationConfigurationServiceClient(conn)
	snapshot, err := cfg.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: dumbAssistantAppID})
	if err != nil {
		t.Fatal(err)
	}
	const delayMS = 700
	if _, err := cfg.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{
		AppId: dumbAssistantAppID, ExpectedRevision: snapshot.GetRevision(),
		Changes: []*frameworkv1.AppSettingChange{
			{Key: "delay_mode", ValueJson: `"constant"`},
			{Key: "delay_ms", ValueJson: `700`},
		},
	}); err != nil {
		t.Fatal(err)
	}
	settingsApplied := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logText(d), "constant=700ms") {
			settingsApplied = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !settingsApplied {
		t.Logf("daemon log:\n%s", logText(d))
		t.Fatal("timed out waiting for assistant settings applied")
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if _, ok := d.wake.Trigger("api", "sim.wakeword.manual"); !ok {
			t.Fatal("simulator wakeword trigger failed")
		}
		started := time.Now()
		if playbackOpen(d, dumbAssistantAppID) {
			t.Fatalf("attempt %d: playback started immediately", attempt)
		}
		waitFor(t, 3*time.Second, "teal ring active", func() bool {
			return d.router.Current("light.ring.main").SelectedAppID == dumbAssistantAppID
		})
		waitFor(t, 5*time.Second, "delayed reply playback", func() bool {
			return playbackOpen(d, dumbAssistantAppID)
		})
		elapsed := time.Since(started)
		if elapsed < (delayMS-200)*time.Millisecond {
			t.Fatalf("attempt %d: playback opened after %s, configured delay is %d ms", attempt, elapsed, delayMS)
		}
		opened := time.Now()
		waitFor(t, 10*time.Second, "reply playback finished", func() bool {
			return !playbackOpen(d, dumbAssistantAppID)
		})
		if played := time.Since(opened); played < clipDuration/2 {
			t.Fatalf("attempt %d: reply stream lasted %s; packaged clip is %s", attempt, played, clipDuration)
		}
		waitFor(t, 3*time.Second, "teal ring released", func() bool {
			return d.router.Current("light.ring.main").SelectedAppID != dumbAssistantAppID
		})
	}
	if got := strings.Count(logText(d), "playing reply for activation="); got != 2 {
		t.Fatalf("log shows %d replies, want 2:\n%s", got, logText(d))
	}
	if got := strings.Count(logText(d), "ring glowing teal"); got != 2 {
		t.Fatalf("log shows %d teal ring activations, want 2:\n%s", got, logText(d))
	}
	if got := strings.Count(logText(d), "ring released"); got != 2 {
		t.Fatalf("log shows %d ring releases, want 2:\n%s", got, logText(d))
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
