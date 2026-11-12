package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/settings"
)

// nativeLibDir is the shared libcapsula runtime directory for daemon tests.
// The test binary lives in a temporary directory, so exe-adjacent discovery
// would never see the repo's build/lib. CAPSULA_NATIVE_LIB_DIR overrides the
// repository default; the launcher still validates and loads real metadata.
func nativeLibDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("CAPSULA_NATIVE_LIB_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(moduleRoot(t), "build", "lib")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func buildProbe(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "probe")
	args := []string{"build"}
	if runtime.GOOS == "darwin" {
		args = append(args, "-ldflags", "-linkmode=external")
	}
	args = append(args, "-o", out, "./testdata/probe")
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join(moduleRoot(t), "internal", "daemon")
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, outBytes)
	}
	return out
}

// buildModuleApp builds one of the repository's sample applications for the
// host so tests can exercise the real provider instead of a stand-in. The
// optional assistant is a nested module with its own go.mod.
func buildModuleApp(t *testing.T, pkg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), filepath.Base(pkg))
	args := []string{"build"}
	if runtime.GOOS == "darwin" {
		args = append(args, "-ldflags", "-linkmode=external")
	}
	dir := moduleRoot(t)
	target := "./" + pkg
	if pkg == "apps/dumbassistant" {
		dir = filepath.Join(moduleRoot(t), "apps", "dumbassistant")
		target = "./cmd/dumbassistant"
	}
	args = append(args, "-o", out, target)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, outBytes)
	}
	return out
}

const clockManifest = `
id = "test.clock"
name = "Test clock"
version = "0.1.0"
background = true
start_on_boot = true
restart_on_failure = true
single_instance = true
provides = ["segment_display_provider"]

[entrypoint]
runtime = "native"
path = "clock"
architecture = "aarch64"
abi = "linux-musl"

[[arguments]]
name = "note"
type = "text"

[[settings]]
key = "blink"
type = "boolean"
default = true
`

const assistantManifest = `
id = "test.assistant"
name = "Test assistant"
version = "0.1.0"
background = true
start_on_boot = true
restart_on_failure = true
single_instance = true
provides = ["voice_assistant"]

[entrypoint]
runtime = "native"
path = "assistant"
architecture = "aarch64"
abi = "linux-musl"
`

const probeManifest = `
id = "test.probe"
name = "Test probe"
version = "0.1.0"
background = false
restart_on_failure = false
single_instance = false

[entrypoint]
runtime = "native"
path = "probe"
architecture = "aarch64"
abi = "linux-musl"

[[arguments]]
name = "mode"
type = "text"

[[settings]]
key = "secret"
type = "text_masked"

[[interfaces]]
service = "test.probe.v1.ProbeService"
version = "1.0.0"
`

func installApp(t *testing.T, root, id, binaryName, binaryPath, manifest string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, binaryName), data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// appInstall is an extra real package for tests that need a provider app
// rather than the generic probe.
type appInstall struct {
	id         string
	binaryName string
	binaryPath string
	manifest   []byte
	files      map[string][]byte
}

func newTestDaemon(t *testing.T) (*Daemon, context.CancelFunc, <-chan error) {
	return newTestDaemonWith(t)
}

func newTestDaemonWith(t *testing.T, extras ...appInstall) (*Daemon, context.CancelFunc, <-chan error) {
	t.Helper()
	bin := buildProbe(t)
	// The Unix socket path must fit in sun_path (104 bytes on macOS), so the
	// test root cannot include the long test name that t.TempDir() adds.
	base, err := os.MkdirTemp("", "capsulad-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	apps := filepath.Join(base, "apps")
	installApp(t, apps, "test.clock", "clock", bin, clockManifest)
	installApp(t, apps, "test.assistant", "assistant", bin, assistantManifest)
	installApp(t, apps, "test.probe", "probe", bin, probeManifest)
	for _, extra := range extras {
		dir := filepath.Join(apps, extra.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.toml"), extra.manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(extra.binaryPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, extra.binaryName), data, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range extra.files {
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

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
	go func() { done <- d.Run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("daemon shutdown timed out")
		}
	})
	waitFor(t, 5*time.Second, "daemon socket", func() bool {
		_, err := os.Stat(cfg.SocketPath)
		return err == nil
	})
	return d, cancel, done
}

func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitReady(t *testing.T, d *Daemon, appID string, timeout time.Duration) supervisorRunID {
	t.Helper()
	var runID string
	waitFor(t, timeout, appID+" ready", func() bool {
		run, ok := d.sup.ReadyRun(appID)
		if ok {
			runID = run.RunID
		}
		return ok
	})
	return supervisorRunID(runID)
}

type supervisorRunID string

func dialTest(t *testing.T, socket string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///capsulad",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func routedCall(t *testing.T, conn *grpc.ClientConn, target, service, rpc string, requests [][]byte) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := frameworkv1.NewAppBusServiceClient(conn).Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{TargetAppId: target, GrpcService: service, RpcName: rpc, TimeoutMs: 5000},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, req := range requests {
		if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{RequestMessage: req}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = stream.CloseSend()
	var out [][]byte
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("routed call: %v", err)
		}
		switch m := resp.Message.(type) {
		case *frameworkv1.RoutedCallResponse_ResponseMessage:
			out = append(out, m.ResponseMessage)
		case *frameworkv1.RoutedCallResponse_End:
			if m.End.GrpcStatusCode != 0 {
				t.Fatalf("routed call ended with %d: %s", m.End.GrpcStatusCode, m.End.Detail)
			}
			return out
		}
	}
	return out
}

func TestSimulatorVerticalSlice(t *testing.T) {
	d, _, _ := newTestDaemon(t)
	conn := dialTest(t, d.cfg.SocketPath)

	// Seeded defaults select the bundled providers.
	waitFor(t, 5*time.Second, "seeded defaults", func() bool {
		v := d.user.Get().Values
		return v.DefaultAppByResource["display.segment.front"] == "test.clock" &&
			v.DefaultAssistantAppID == "test.assistant"
	})
	waitReady(t, d, "test.clock", 5*time.Second)
	waitReady(t, d, "test.assistant", 5*time.Second)

	// Clock baseline becomes the selected display source after its first frame.
	waitFor(t, 5*time.Second, "clock baseline selected", func() bool {
		sel := d.router.Current("display.segment.front")
		return sel.SelectedAppID == "test.clock" && sel.Reason == "default_baseline"
	})

	// Singleton launch delivers an in-place invocation and does not spawn again.
	run1, _, spawned1, err := d.Launch("test.clock", map[string]string{"note": "first"})
	if err != nil || spawned1 {
		t.Fatalf("first singleton launch: spawned=%v err=%v", spawned1, err)
	}
	run2, inv2, spawned2, err := d.Launch("test.clock", map[string]string{"note": "second"})
	if err != nil || spawned2 || run1 != run2 {
		t.Fatalf("singleton reused process incorrectly: %s %s spawned=%v err=%v", run1, run2, spawned2, err)
	}
	if inv2 == "" {
		t.Fatal("no invocation id for running singleton")
	}
	if n := len(d.sup.List("test.clock")); n != 1 {
		t.Fatalf("singleton produced %d runs", n)
	}

	// Routed unary and streaming calls through the app bus.
	_, _, spawned, err := d.Launch("test.probe", map[string]string{"mode": "serve"})
	if err != nil || !spawned {
		t.Fatalf("launch probe: spawned=%v err=%v", spawned, err)
	}
	_ = waitReady(t, d, "test.probe", 5*time.Second)
	unary := routedCall(t, conn, "test.probe", "test.probe.v1.ProbeService", "Echo", [][]byte{[]byte("one")})
	if len(unary) != 1 || string(unary[0]) != "echo:one" {
		t.Fatalf("unary routed call: %q", unary)
	}
	streamOut := routedCall(t, conn, "test.probe", "test.probe.v1.ProbeService", "Stream",
		[][]byte{[]byte("a"), []byte("b"), []byte("c")})
	if len(streamOut) != 3 || string(streamOut[0]) != "s:a" || string(streamOut[2]) != "s:c" {
		t.Fatalf("streaming routed call: %q", streamOut)
	}

	// Only one probe run should be ready at a time: routed calls address an
	// app, not a specific run.
	d.sup.Stop(stopRunID(t, d, "test.probe"))
	waitFor(t, 5*time.Second, "probe stopped", func() bool {
		return len(d.sup.List("test.probe")) == 0
	})

	// Partial touch capture receives semantic gestures while the framework
	// keeps volume control; full capture suppresses built-in handling.
	touchPartial, _, _, err := d.Launch("test.probe", map[string]string{"mode": "touchpartial"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "partial touch capture", func() bool {
		sel := d.router.Current("touch.panel.main")
		return sel.SelectedAppID == "test.probe" && sel.TouchMode == 1 && sel.SystemTouchActive
	})
	before := touchCount(t, conn)
	d.input.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "primary", Pressed: true}})
	time.Sleep(30 * time.Millisecond)
	d.input.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "primary", Pressed: false}})
	waitFor(t, 3*time.Second, "partial gesture delivery", func() bool { return touchCount(t, conn) > before })
	before = touchCount(t, conn)
	volBefore := d.user.Get().Values.MasterVolume
	d.input.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	waitFor(t, 3*time.Second, "system volume in partial mode", func() bool {
		return d.user.Get().Values.MasterVolume > volBefore
	})
	d.input.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: false}})
	time.Sleep(200 * time.Millisecond)
	if got := touchCount(t, conn); got != before {
		t.Fatalf("partial capture received a system volume event: %d -> %d", before, got)
	}
	d.sup.Stop(touchPartial)
	waitFor(t, 5*time.Second, "partial probe stopped", func() bool { return len(d.sup.List("test.probe")) == 0 })

	touchFull, _, _, err := d.Launch("test.probe", map[string]string{"mode": "touchfull"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "full touch capture", func() bool {
		sel := d.router.Current("touch.panel.main")
		return sel.SelectedAppID == "test.probe" && sel.TouchMode == 2 && !sel.SystemTouchActive
	})
	before = touchCount(t, conn)
	volBefore = d.user.Get().Values.MasterVolume
	d.input.Handle(backend.InputEvent{Key: &backend.KeyEvent{Key: "volume_up", Pressed: true}})
	waitFor(t, 3*time.Second, "full capture key delivery", func() bool { return touchCount(t, conn) > before })
	time.Sleep(200 * time.Millisecond)
	if got := d.user.Get().Values.MasterVolume; got != volBefore {
		t.Fatalf("full capture did not suppress system volume: %v -> %v", volBefore, got)
	}
	before = touchCount(t, conn)
	d.input.Handle(backend.InputEvent{Report: &backend.RawReport{
		Contacts:         []backend.Contact{{ID: 0, X: 0.25, Y: 0.5, Touching: true}},
		CoordinatesValid: true, SampleFormat: "test.raw",
	}})
	waitFor(t, 3*time.Second, "raw report delivery", func() bool { return touchCount(t, conn) > before })
	d.sup.Stop(touchFull)
	waitFor(t, 5*time.Second, "full probe stopped", func() bool { return len(d.sup.List("test.probe")) == 0 })

	// A crashed app's presentations and captures are released immediately.
	crashRun, _, _, err := d.Launch("test.probe", map[string]string{"mode": "crash"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "crash probe capture selected", func() bool {
		sel := d.router.Current("light.ring.main")
		return sel.SelectedCaptureID != "" && d.ownerOfCapture(sel.SelectedCaptureID) == "test.probe"
	})
	waitFor(t, 5*time.Second, "crash cleanup", func() bool {
		if _, ok := d.sup.Get(crashRun); ok {
			return false
		}
		for _, c := range d.router.ActiveCaptures("light.ring.main") {
			if c.RunID == crashRun {
				return false
			}
		}
		return true
	})
	waitFor(t, 3*time.Second, "ring selection restored", func() bool {
		return d.router.Current("light.ring.main").SelectedAppID != "test.probe"
	})

	// Microphone watchdog: no visible indication → the stream is terminated.
	micRun, _, _, err := d.Launch("test.probe", map[string]string{"mode": "mic"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "mic stream opened", func() bool {
		return hasInputStream(d, "test.probe")
	})
	waitFor(t, 9*time.Second, "watchdog termination", func() bool {
		return !hasInputStream(d, "test.probe")
	})
	if _, ok := d.sup.Get(micRun); ok {
		// The probe exits after reporting the notice; tolerate either state but
		// require the stream to be gone.
	}
	if !strings.Contains(logText(d), "WATCHDOG_EXPIRED") {
		t.Fatalf("watchdog reason missing from logs:\n%s", logText(d))
	}

	// Wakeword activation reaches the selected assistant, which captures the
	// ring and microphone, then explicitly releases both.
	act, ok := d.wake.Trigger("api", "sim.wakeword.manual")
	if !ok {
		t.Fatal("simulator wakeword trigger failed")
	}
	_ = act
	waitFor(t, 5*time.Second, "assistant ring capture", func() bool {
		sel := d.router.Current("light.ring.main")
		return sel.SelectedAppID == "test.assistant"
	})
	waitFor(t, 3*time.Second, "assistant microphone", func() bool {
		return hasInputStream(d, "test.assistant")
	})
	waitFor(t, 6*time.Second, "assistant release", func() bool {
		return !hasInputStream(d, "test.assistant") && d.router.Current("light.ring.main").SelectedAppID != "test.assistant"
	})

	// Muted indication is a temporary system capture and never changes mute state.
	if err := d.writeUserSettings([]string{"muted_microphone_indicator"}, func(v *settings.UserSettings) {
		v.MutedIndicator = settings.MutedDelay
		v.MutedIndicatorDelayMS = 500
	}); err != nil {
		t.Fatal(err)
	}
	d.setMuted(true)
	waitFor(t, 3*time.Second, "muted indication capture", func() bool {
		d.indicatorMu.Lock()
		defer d.indicatorMu.Unlock()
		return d.micCaptureID != ""
	})
	waitFor(t, 3*time.Second, "muted indication expiry", func() bool {
		d.indicatorMu.Lock()
		defer d.indicatorMu.Unlock()
		return d.micCaptureID == ""
	})
	if !d.audio.Muted() {
		t.Fatal("muted indication expiry changed mute state")
	}
	d.setMuted(false)

	// Application settings are redacted over the API.
	cfgSvc := frameworkv1.NewApplicationConfigurationServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot, err := cfgSvc.Get(ctx, &frameworkv1.GetAppConfigurationRequest{AppId: "test.probe"})
	if err != nil {
		t.Fatal(err)
	}
	var rev uint64
	if snapshot != nil {
		rev = snapshot.Revision
	}
	updated, err := cfgSvc.Update(ctx, &frameworkv1.UpdateAppConfigurationRequest{
		AppId: "test.probe", ExpectedRevision: rev,
		Changes: []*frameworkv1.AppSettingChange{{Key: "secret", ValueJson: `"hunter2"`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range updated.Values {
		if v.Key == "secret" {
			if v.ValueJson != "" || !v.HasValue {
				t.Fatalf("masked value leaked over the API: %+v", v)
			}
			return
		}
	}
	t.Fatal("secret setting missing from snapshot")
}

func stopRunID(t *testing.T, d *Daemon, appID string) string {
	t.Helper()
	runs := d.sup.List(appID)
	if len(runs) == 0 {
		t.Fatalf("no run for %s", appID)
	}
	return runs[0].RunID
}

func touchCount(t *testing.T, conn *grpc.ClientConn) int64 {
	t.Helper()
	out := routedCall(t, conn, "test.probe", "test.probe.v1.ProbeService", "TouchCount", nil)
	if len(out) != 1 {
		t.Fatalf("touch count response: %q", out)
	}
	var n int64
	fmt.Sscan(string(out[0]), &n)
	return n
}

func hasInputStream(d *Daemon, appID string) bool {
	for _, s := range d.audio.ActiveStreams() {
		if s.Input && s.AppID == appID {
			return true
		}
	}
	return false
}

func logText(d *Daemon) string {
	var b strings.Builder
	for _, e := range d.logs.Snapshot("") {
		fmt.Fprintf(&b, "%s %s %s\n", e.Component, e.Severity, e.Message)
	}
	return b.String()
}

func (d *Daemon) ownerOfCapture(captureID string) string {
	for _, c := range d.router.ActiveCaptures("light.ring.main") {
		if c.ID == captureID {
			return c.AppID
		}
	}
	return ""
}
