package daemon

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/settings"
)

// cadenceAppID is the optional Cadence assistant package.
const cadenceAppID = "org.capsulad.cadence"

// TestCadenceCaptureAndMicOverLocalServer is the integrated boundary proof for
// the Cadence client: the real app runs under the real daemon, connects to a
// local fake Cadence device link, opens a turn, and the daemon actually grants
// the ring capture and the microphone. That grant is what the earlier bug
// missed: a presentation without the routing capture is not an indication, so
// every capture open was refused. It then dismisses and proves the capture and
// audio are released.
//
// No remote service and no device are involved: the "server" is a loopback
// websocket handshake and this host's simulator backend.
func TestCadenceCaptureAndMicOverLocalServer(t *testing.T) {
	addr := startFakeCadenceServer(t)
	d, _, _ := newTestDaemonWith(t, cadenceInstall(t))

	// The app talks to the loopback fake over http; the token is any value.
	_, rev, private, err := d.appCfg.Update(cadenceAppID, 0, []settings.Change{
		{Key: "server_url", Value: "http://" + addr},
		{Key: "device_token", Value: "sim-token"},
	})
	if err != nil {
		t.Fatalf("app settings: %v", err)
	}
	d.pushAppSettings(cadenceAppID, rev, private)

	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = cadenceAppID
	}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, d, cadenceAppID, 20*time.Second)
	waitFor(t, 10*time.Second, "the app's link to the fake server", func() bool {
		return strings.Contains(logText(d), "link_up")
	})

	if err := d.writeUserSettings([]string{"touch_hold_mode"}, func(v *settings.UserSettings) {
		v.TouchHoldMode = settings.HoldToTalk
	}); err != nil {
		t.Fatal(err)
	}
	d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: true}})
	defer d.simTouchSignal(backend.InputEvent{Key: &backend.KeyEvent{Key: "play_pause", Pressed: false}})

	// The capture is the daemon's selection, the frame is the indication, and
	// the input stream proves OpenMic succeeded after the grant.
	waitFor(t, 8*time.Second, "ring capture selected", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID == cadenceAppID
	})
	waitFor(t, 8*time.Second, "a non-blank backend ring frame", func() bool {
		return ringShowsTeal(d, 1)
	})
	waitFor(t, 8*time.Second, "microphone stream open", func() bool {
		return inputOpen(d, cadenceAppID)
	})

	// Dismiss the turn: the microphone and the capture must both be released,
	// and the app must report the terminal path.
	if _, err := d.TriggerAction(cadenceAppID, "dismiss"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	waitFor(t, 8*time.Second, "microphone released", func() bool {
		return !inputOpen(d, cadenceAppID)
	})
	waitFor(t, 8*time.Second, "capture released", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID != cadenceAppID
	})
	waitFor(t, 8*time.Second, "turn_end logged", func() bool {
		return strings.Contains(logText(d), "turn_end activation=")
	})
	if got := logText(d); strings.Contains(got, "mic_failed") {
		t.Fatalf("the daemon refused the capture; log:\n%s", got)
	}
}

// buildCadenceApp builds the nested cadence module for the host, like the
// dumbassistant harness does for its own module.
func buildCadenceApp(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "cadence")
	args := []string{"build"}
	if runtime.GOOS == "darwin" {
		args = append(args, "-ldflags", "-linkmode=external")
	}
	args = append(args, "-o", out, "./cmd/cadence")
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join(moduleRoot(t), "apps", "cadence")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build apps/cadence: %v\n%s", err, b)
	}
	return out
}

// cadenceInstall stages the real package layout the daemon requires: the
// manifest declares [settings_ui], so the packaged settings document must be
// present beside the binary or the registry rejects the app.
func cadenceInstall(t *testing.T) appInstall {
	t.Helper()
	root := moduleRoot(t)
	manifest, err := os.ReadFile(filepath.Join(root, "apps", "cadence", "manifest.toml"))
	if err != nil {
		t.Fatal(err)
	}
	settingsUI, err := os.ReadFile(filepath.Join(root, "apps", "cadence", "ui", "settings.html"))
	if err != nil {
		t.Fatal(err)
	}
	return appInstall{
		id: cadenceAppID, binaryName: "cadence", binaryPath: buildCadenceApp(t),
		manifest: manifest,
		files:    map[string][]byte{"ui/settings.html": settingsUI},
	}
}

// fakeCadenceWelcome is the one server frame the app's handshake needs:
// Envelope{welcome{server_version:"sim", send_full_report:true}}.
var fakeCadenceWelcome = []byte{
	0x82, 0x09, // binary, final, 9-byte payload
	0x12, 0x07, // Envelope.welcome (field 2), 7-byte message
	0x0A, 0x03, 's', 'i', 'm', // Welcome.server_version
	0x10, 0x01, // Welcome.send_full_report
}

// startFakeCadenceServer accepts one websocket upgrade on loopback, answers the
// handshake with a Welcome, and drains whatever the app sends. It speaks no
// more of the voice protocol: the test only needs the link up so the app can
// send SessionStart and open the microphone.
func startFakeCadenceServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go serveFakeCadence(conn)
		}
	}()
	return lis.Addr().String()
}

func serveFakeCadence(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	key := headers["sec-websocket-key"]
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
	_, _ = conn.Write(fakeCadenceWelcome)
	// The app's frames (Hello, the report, SessionStart, MicAudio) are drained:
	// this fake proves the link, not the server's voice behaviour.
	_, _ = io.Copy(io.Discard, br)
}
