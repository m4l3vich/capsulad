package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"capsula/apps/marusia/internal/settings"
	"capsula/apps/marusia/internal/vkclient"
	"capsula/apps/marusia/internal/vkproto/clientpb"
	"capsula/sdk/capsula"
)

// fakeDeviceService is the DeviceService.GetInfo caller used instead of the
// SDK, so identity resolution is testable without a daemon.
type fakeDeviceService struct {
	deviceID       string
	hardwareSerial string
	err            error
	calls          int
}

func (f *fakeDeviceService) CallJSON(_ context.Context, targetAppID, service, rpc string,
	_ any, response any, _ time.Duration) error {
	f.calls++
	if targetAppID != "" || service != deviceService || rpc != "GetInfo" {
		return errors.New("unexpected call " + service + "/" + rpc)
	}
	if f.err != nil {
		return f.err
	}
	info, ok := response.(*deviceInfo)
	if !ok {
		return errors.New("unexpected response type")
	}
	info.DeviceID = f.deviceID
	info.HardwareSerial = f.hardwareSerial
	return nil
}

// unreadableProc reproduces the stock sandbox: no PID namespace, so /proc is
// absent and the SoC serial cannot be read.
func unreadableProc(t *testing.T) {
	t.Helper()
	t.Setenv(capsula.DeviceSerialEnv, "")
	previous := cpuSerialReader
	cpuSerialReader = func() string { return "" }
	t.Cleanup(func() { cpuSerialReader = previous })
}

func TestIdentityFallsBackToFrameworkDeviceID(t *testing.T) {
	unreadableProc(t)
	client := &fakeDeviceService{deviceID: "device-from-framework"}

	fallback := loadIdentity(context.Background(), client)
	if client.calls != 1 {
		t.Fatalf("GetInfo calls = %d", client.calls)
	}
	if fallback.DeviceID != "device-from-framework" {
		t.Fatalf("fallback = %+v", fallback)
	}
	if fallback.Generated == "" {
		t.Fatal("a process-local id must still be prepared")
	}

	config, warnings, err := settings.Build(settings.Default(), fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.DeviceID != "device-from-framework" {
		t.Fatalf("device id = %q; the framework identity must replace the rotating uuid", config.DeviceID)
	}
	if config.Serial != "" || config.SerialSource != "" {
		t.Fatalf("serial = %q source = %q; a framework device id must never be sent as x-device-sn",
			config.Serial, config.SerialSource)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "device serial is missing") {
		t.Fatalf("warnings = %v", warnings)
	}

	// Explicit settings stay authoritative over the cpuinfo serial and the
	// framework device identity.
	values := settings.Default()
	values.Serial = "SN-setting"
	values.DeviceID = "id-setting"
	explicit, _, err := settings.Build(values, fallback)
	if err != nil {
		t.Fatalf("build explicit: %v", err)
	}
	if explicit.Serial != "SN-setting" || explicit.DeviceID != "id-setting" {
		t.Fatalf("explicit settings must win: %+v", explicit)
	}
}

func TestCPUSerialReachesConfig(t *testing.T) {
	t.Setenv(capsula.DeviceSerialEnv, "")
	previous := cpuSerialReader
	cpuSerialReader = func() string { return "11bfcbc5d63d55fc" }
	t.Cleanup(func() { cpuSerialReader = previous })

	fallback := loadIdentity(context.Background(), &fakeDeviceService{deviceID: "device-from-framework"})
	config, warnings, err := settings.Build(settings.Default(), fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.Serial != "11bfcbc5d63d55fc" || config.SerialSource != settings.SerialSourceCPUInfo {
		t.Fatalf("serial = %q source = %q", config.Serial, config.SerialSource)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestFrameworkSerialWinsOverLocalCPUInfo(t *testing.T) {
	t.Setenv(capsula.DeviceSerialEnv, "")
	previous := cpuSerialReader
	cpuSerialReader = func() string { return "local-cpuinfo-serial" }
	t.Cleanup(func() { cpuSerialReader = previous })

	// The sandbox cannot read /proc, so the framework's hardware_serial is the
	// only source that works on the device; it must win over a local read.
	fallback := loadIdentity(context.Background(), &fakeDeviceService{
		deviceID: "device-from-framework", hardwareSerial: "11bfcbc5d63d55fc",
	})
	config, warnings, err := settings.Build(settings.Default(), fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.Serial != "11bfcbc5d63d55fc" || config.SerialSource != settings.SerialSourceFramework {
		t.Fatalf("serial = %q source = %q", config.Serial, config.SerialSource)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestSerialSettingWinsOverFrameworkSerial(t *testing.T) {
	unreadableProc(t)
	fallback := loadIdentity(context.Background(), &fakeDeviceService{
		deviceID: "device-from-framework", hardwareSerial: "11bfcbc5d63d55fc",
	})
	values := settings.Default()
	values.Serial = "SN-setting"
	config, warnings, err := settings.Build(values, fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.Serial != "SN-setting" || config.SerialSource != settings.SerialSourceSettings {
		t.Fatalf("serial = %q source = %q; the explicit setting is authoritative", config.Serial, config.SerialSource)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestEnvSerialWinsOverFrameworkSerial(t *testing.T) {
	previousEnv, previousCPU := envSerialReader, cpuSerialReader
	envSerialReader = func() string { return "env-serial" }
	cpuSerialReader = func() string { return "local-cpuinfo-serial" }
	t.Cleanup(func() { envSerialReader, cpuSerialReader = previousEnv, previousCPU })

	// The deployed native runtime may predate DeviceInfo.hardware_serial, so a
	// JSON reply can arrive without the field; the launch environment is the
	// descriptor-free source and must win.
	fallback := loadIdentity(context.Background(), &fakeDeviceService{
		deviceID: "device-from-framework", hardwareSerial: "11bfcbc5d63d55fc",
	})
	config, warnings, err := settings.Build(settings.Default(), fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.Serial != "env-serial" || config.SerialSource != settings.SerialSourceEnv {
		t.Fatalf("serial = %q source = %q", config.Serial, config.SerialSource)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}

	if got := fallback.DeviceID; got != "device-from-framework" {
		t.Fatalf("device id = %q; the environment serial must not replace the identity", got)
	}
}

func TestEnvSerialSurvivesEmptyFrameworkSerial(t *testing.T) {
	previousEnv, previousCPU := envSerialReader, cpuSerialReader
	envSerialReader = func() string { return "env-serial" }
	cpuSerialReader = func() string { return "" }
	t.Cleanup(func() { envSerialReader, cpuSerialReader = previousEnv, previousCPU })

	fallback := loadIdentity(context.Background(), &fakeDeviceService{deviceID: "device-from-framework"})
	if fallback.Serial != "env-serial" || fallback.SerialSource != settings.SerialSourceEnv {
		t.Fatalf("fallback = %+v", fallback)
	}
}

func TestEmptyEnvSerialFallsThroughToFramework(t *testing.T) {
	previousEnv, previousCPU := envSerialReader, cpuSerialReader
	envSerialReader = func() string { return "  " }
	cpuSerialReader = func() string { return "" }
	t.Cleanup(func() { envSerialReader, cpuSerialReader = previousEnv, previousCPU })

	fallback := loadIdentity(context.Background(), &fakeDeviceService{
		deviceID: "device-from-framework", hardwareSerial: "11bfcbc5d63d55fc",
	})
	if fallback.Serial != "11bfcbc5d63d55fc" || fallback.SerialSource != settings.SerialSourceFramework {
		t.Fatalf("fallback = %+v", fallback)
	}
}

func TestSerialSettingWinsOverEnvSerial(t *testing.T) {
	previousEnv := envSerialReader
	envSerialReader = func() string { return "env-serial" }
	t.Cleanup(func() { envSerialReader = previousEnv })
	unreadableProc(t)

	fallback := loadIdentity(context.Background(), &fakeDeviceService{deviceID: "device-from-framework"})
	values := settings.Default()
	values.Serial = "SN-setting"
	config, _, err := settings.Build(values, fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.Serial != "SN-setting" || config.SerialSource != settings.SerialSourceSettings {
		t.Fatalf("serial = %q source = %q; the explicit setting is authoritative", config.Serial, config.SerialSource)
	}
}

func TestSerialSourceLoggedOnceWithoutValue(t *testing.T) {
	var logbuf bytes.Buffer
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := &app{}
	a.logSerialSource(settings.Config{Serial: "11bfcbc5d63d55fc", SerialSource: settings.SerialSourceCPUInfo})
	a.logSerialSource(settings.Config{Serial: "SN-other", SerialSource: settings.SerialSourceSettings})

	output := logbuf.String()
	if count := strings.Count(output, "device serial source=cpuinfo"); count != 1 {
		t.Fatalf("source logged %d times, want once:\n%s", count, output)
	}
	if strings.Contains(output, "source=settings") {
		t.Fatalf("the source must be logged once, not on every revision:\n%s", output)
	}
	if strings.Contains(output, "11bfcbc5d63d55fc") || strings.Contains(output, "SN-other") {
		t.Fatalf("the serial value must never be logged:\n%s", output)
	}
}

func TestIdentitySurvivesDeviceInfoFailure(t *testing.T) {
	unreadableProc(t)
	const failure = "GetInfo unavailable"
	var logbuf bytes.Buffer
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	fallback := loadIdentity(context.Background(), &fakeDeviceService{err: errors.New(failure)})
	if fallback.DeviceID != "" {
		t.Fatalf("fallback = %+v", fallback)
	}
	if fallback.Generated == "" {
		t.Fatal("the app needs a usable identity even when GetInfo fails")
	}
	config, warnings, err := settings.Build(settings.Default(), fallback)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if config.DeviceID != fallback.Generated {
		t.Fatalf("device id = %q, generated = %q", config.DeviceID, fallback.Generated)
	}
	if !strings.Contains(logbuf.String(), failure) {
		t.Fatalf("the failure must be logged once: %q", logbuf.String())
	}
	if strings.Count(logbuf.String(), "device info unavailable") != 1 {
		t.Fatalf("the failure must be logged exactly once: %q", logbuf.String())
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "device_id is empty") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestLogsNeverContainMaskedSettings(t *testing.T) {
	const masked = "dev-5f0c9a71c2"
	var logbuf bytes.Buffer
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := &app{
		fallback: settings.Fallback{DeviceID: "framework-identity", Generated: "process-identity"},
		values:   settings.Default(),
		queue:    make(chan activationJob, 1),
	}
	a.applySettings(&capsula.SettingsChanged{Revision: 2, Values: []capsula.SettingValue{
		{Key: settings.KeyDeviceID, ValueJSON: `"` + masked + `"`},
		{Key: settings.KeyXToken, ValueJSON: `"xtoken-secret"`},
	}})
	if a.config().DeviceID != masked {
		t.Fatalf("settings were not applied: %+v", a.config())
	}
	output := logbuf.String()
	if !strings.Contains(output, "settings applied") {
		t.Fatalf("the settings update was not logged: %q", output)
	}

	logbuf.Reset()
	log.Printf("marusia: ready; %s", a.summary())
	output += logbuf.String()
	if !strings.Contains(output, "endpoint=") {
		t.Fatalf("the ready line lost its diagnostic content: %q", output)
	}
	for _, secret := range []string{masked, "xtoken-secret", "framework-identity", "process-identity"} {
		if strings.Contains(output, secret) {
			t.Fatalf("log leaked %q:\n%s", secret, output)
		}
	}
}

// --- microphone logging -----------------------------------------------------

// TestPumpMicLogsFormatOncePerStream guards the log flood: the SDK attaches the
// capture format to every chunk, so a naive implementation logs at 50 lines/s.
func TestPumpMicLogsFormatOncePerStream(t *testing.T) {
	var logbuf bytes.Buffer
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := &app{}
	in := make(chan capsula.MicChunk, 64)
	out := make(chan []byte, 64)
	format := capsula.AudioFormat{
		SampleFormat: capsula.SampleFormatPCM16LE, SampleRateHz: 16000, Channels: 1,
	}
	for range 40 {
		in <- capsula.MicChunk{
			StreamID: "mic_1",
			Format:   &format,
			Chunk:    &capsula.AudioChunk{PCM: make([]byte, 640)},
		}
	}
	close(in)

	done := make(chan struct{})
	go func() {
		a.pumpMic(context.Background(), in, out, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpMic did not finish")
	}
	if got := len(out); got != 40 {
		t.Fatalf("forwarded chunks = %d, want 40", got)
	}
	output := logbuf.String()
	if count := strings.Count(output, "microphone format"); count != 1 {
		t.Fatalf("format logged %d times, want once per stream:\n%s", count, output)
	}
	if !strings.Contains(output, "PCM_S16LE 16000 Hz/1 ch") {
		t.Fatalf("format line = %q", output)
	}
}

// TestPumpMicStopsAfterCancel makes sure a finished turn cannot deliver audio to
// a later one: the pump exits on cancellation and closes its output exactly once.
func TestPumpMicStopsAfterCancel(t *testing.T) {
	a := &app{}
	in := make(chan capsula.MicChunk)
	out := make(chan []byte, 4)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		a.pumpMic(ctx, in, out, nil, nil)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpMic ignored cancellation")
	}
	if _, open := <-out; open {
		t.Fatal("pumpMic must close its output when it returns")
	}
	select {
	case in <- capsula.MicChunk{Chunk: &capsula.AudioChunk{PCM: make([]byte, 640)}}:
		t.Fatal("the pump still accepts audio after it returned")
	default:
	}
}

func TestKeywordOptionsFollowActivationEvidence(t *testing.T) {
	config := settings.Config{
		Format:  vkclient.FormatWave,
		Keyword: "marusya",
		HWID:    "9F25725B",
	}

	wake := &capsula.Activation{
		Origin:         capsula.ActivationOriginWakeword,
		MatchedModelID: "wakeword-model-1",
	}
	wakeInit := vkclient.BuildInit(speechOptionsFor(config, wake), "serial", "version")
	wakeParams := wakeInit.GetKeywordOptions().GetKwParams()
	if wakeParams == nil || wakeParams.GetKeyword() != "marusya" || wakeParams.GetHwId() != "9F25725B" {
		t.Fatalf("wake-word init kw_params = %+v", wakeParams)
	}
	if wakeParams.GetMediaState() != clientpb.MediaState_MEDIA_STATE_STOP {
		t.Fatalf("wake-word media_state = %v, want STOP", wakeParams.GetMediaState())
	}

	// A touch turn states no_kw: the service rejects an absent variant with
	// "Invalid 'kw_type' param: unknown keyword type".
	touch := &capsula.Activation{Origin: capsula.ActivationOriginTouch}
	touchInit := vkclient.BuildInit(speechOptionsFor(config, touch), "serial", "version")
	if touchInit.GetKeywordOptions().GetNoKw() == nil {
		t.Fatalf("touch init = %+v, want no_kw", touchInit.GetKeywordOptions())
	}
	if touchInit.GetKeywordOptions().GetKwParams() != nil {
		t.Fatalf("touch init carried kw_params: %+v", touchInit.GetKeywordOptions().GetKwParams())
	}
}

// Only a wake-word activation carries keyword evidence. An API activation — and
// a touch activation, even one the framework delivered with a stale match — must
// not claim a keyword: the service answers "keyword not confirmed" instead of
// transcribing the phrase.
func TestSpeechOptionsClaimAKeywordOnlyForAWakeword(t *testing.T) {
	config := settings.Config{Format: vkclient.FormatWave, Keyword: "marusya"}
	cases := []struct {
		name       string
		activation *capsula.Activation
		want       string
	}{
		{"api", &capsula.Activation{Origin: capsula.ActivationOriginAPI}, ""},
		{"touch", &capsula.Activation{Origin: capsula.ActivationOriginTouch}, ""},
		{"touch with a model id", &capsula.Activation{Origin: capsula.ActivationOriginTouch, MatchedModelID: "manual.button"}, ""},
		{"unspecified", &capsula.Activation{}, ""},
		{"wakeword", &capsula.Activation{Origin: capsula.ActivationOriginWakeword, MatchedModelID: "word"}, "marusya"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := speechOptionsFor(config, tc.activation)
			if options.Keyword != tc.want {
				t.Fatalf("keyword = %q, want %q", options.Keyword, tc.want)
			}
			if options.MediaState != clientpb.MediaState_MEDIA_STATE_STOP {
				t.Fatalf("media_state = %v, want STOP", options.MediaState)
			}
		})
	}
}

// micChunk makes one forwarded 20 ms capture chunk carrying its index, so a
// test can tell which audio survived the queue.
func micChunk(index int) capsula.MicChunk {
	pcm := make([]byte, 640)
	pcm[0] = byte(index)
	pcm[1] = byte(index >> 8)
	return capsula.MicChunk{
		StreamID: "mic_1",
		Format:   &capsula.AudioFormat{SampleFormat: capsula.SampleFormatPCM16LE, SampleRateHz: 16000, Channels: 1},
		Chunk:    &capsula.AudioChunk{PCM: pcm},
	}
}

// TestPumpMicHoldsUtteranceDuringSlowOpen covers the measured phrase-open
// latency: capture begins before the stream is authorized and opened, so the
// queue must hold the first 500 ms of speech (25 chunks) while the opener is
// in flight. At the old depth of 8 chunks most of that head was lost.
func TestPumpMicHoldsUtteranceDuringSlowOpen(t *testing.T) {
	var logbuf bytes.Buffer
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	a := &app{}
	in := make(chan capsula.MicChunk, 64)
	out := make(chan []byte, captureQueue)

	done := make(chan struct{})
	go func() {
		a.pumpMic(context.Background(), in, out, nil, nil)
		close(done)
	}()

	const held = 25 // 500 ms of 20 ms chunks
	for i := range held {
		in <- micChunk(i)
		time.Sleep(20 * time.Millisecond) // model capture arriving while OpenSpeech stalls
	}
	// The opener is still in flight: nothing has read `out` yet.
	deadline := time.Now().Add(2 * time.Second)
	for len(out) < held {
		if time.Now().After(deadline) {
			t.Fatalf("queued chunks = %d, want %d; audio was dropped before the phrase opened",
				len(out), held)
		}
		time.Sleep(time.Millisecond)
	}
	close(in)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpMic did not finish")
	}
	if got := len(out); got != held {
		t.Fatalf("surviving chunks = %d, want %d", got, held)
	}
	if first := <-out; first[0] != 0 || first[1] != 0 {
		t.Fatalf("the first chunk was not the first captured one: %d", int(first[0])|int(first[1])<<8)
	}
	if output := logbuf.String(); strings.Contains(output, "pre-open") {
		t.Fatalf("a turn with no drops must not report truncation:\n%s", output)
	}
}

// TestPumpMicReportsDroppedPreOpenChunks simulates a stalled opener: 150
// chunks (3 s of 20 ms audio) arrive with nothing draining the bounded queue,
// so the oldest 50 are discarded and counted for the turn's audio summary.
func TestPumpMicReportsDroppedPreOpenChunks(t *testing.T) {
	a := &app{}
	in := make(chan capsula.MicChunk, 256)
	out := make(chan []byte, captureQueue)
	var preOpenDropped atomic.Int64

	const captured = 150 // 3 s of 20 ms chunks
	for i := range captured {
		in <- micChunk(i)
	}
	close(in)

	done := make(chan struct{})
	go func() {
		a.pumpMic(context.Background(), in, out, nil, &preOpenDropped)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pumpMic did not finish")
	}

	if got := len(out); got != captureQueue {
		t.Fatalf("queued chunks = %d, want %d", got, captureQueue)
	}
	dropped := captured - captureQueue
	if first := <-out; int(first[0])|int(first[1])<<8 != dropped {
		t.Fatalf("the first surviving chunk = %d, want %d; the oldest chunks must be the dropped ones",
			int(first[0])|int(first[1])<<8, dropped)
	}
	if got := preOpenDropped.Load(); got != int64(dropped) {
		t.Fatalf("pre-open dropped = %d, want %d", got, dropped)
	}
}

// --- session resolution -----------------------------------------------------

func TestResolveSessionPrecedence(t *testing.T) {
	dir := t.TempDir()
	vendorPath := filepath.Join(dir, "session.json")
	if err := os.WriteFile(vendorPath,
		[]byte(`{"session":"vendor-session","secret":"vendor-secret","account_id":"acct"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device_settings.json"),
		[]byte(`{"general":{"id":":c:d:capsula_mini:vendor-device"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	base := settings.Config{BaseURL: "https://vc.go.mail.ru", SessionFile: vendorPath}

	// Masked settings win over the file.
	explicit := base
	explicit.SessionID, explicit.SessionSecret = "setting-session", "setting-secret"
	got, source, err := resolveSession(explicit)
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if source != "settings" || got.SessionID != "setting-session" || got.SessionSecret != "setting-secret" {
		t.Fatalf("explicit result = %+v (%s)", got, source)
	}

	// Without settings the vendor file supplies the session and device id.
	got, source, err = resolveSession(base)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if source != "vendor session file" || got.SessionID != "vendor-session" ||
		got.SessionSecret != "vendor-secret" || got.DeviceID != ":c:d:capsula_mini:vendor-device" {
		t.Fatalf("file result = %+v (%s)", got, source)
	}
	if !got.HasSession() {
		t.Fatal("HasSession must report the file session")
	}

	// An unreadable file leaves no session and reports a redacted cause.
	denied := base
	denied.SessionFile = filepath.Join(dir, "denied", "session.json")
	if err := os.MkdirAll(filepath.Join(dir, "denied"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(denied.SessionFile, []byte(`{"session":"x","secret":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied.SessionFile, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveSession(denied); err == nil {
		t.Skip("running as a user that can read a 000 file; EACCES not reproducible here")
	} else if strings.Contains(err.Error(), "y") && !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("the error is not the redacted form: %v", err)
	}

	// A missing file simply means "no session": the attach path stays possible.
	absent := base
	absent.SessionFile = filepath.Join(dir, "absent.json")
	got, source, err = resolveSession(absent)
	if err != nil {
		t.Fatalf("absent: %v", err)
	}
	if source != "" || got.HasSession() {
		t.Fatalf("absent result = %+v (%s)", got, source)
	}
}
