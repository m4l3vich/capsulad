package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"capsulad/installer/internal/device"
	"capsulad/installer/internal/install"
)

func TestNavigationAndDisabledCapsula(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40

	// The original Capsula is refused with a reason, not silently skipped.
	m.deviceIx = 0
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenDevice {
		t.Fatalf("Capsula should stay on the device screen, got %v", m.screen)
	}
	if !strings.Contains(m.status, "not supported") {
		t.Fatalf("status = %q", m.status)
	}

	m.deviceIx = 1
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenFirmware {
		t.Fatalf("screen = %v, want firmware", m.screen)
	}
	m.firmwareIx = 1
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenAddress {
		t.Fatalf("screen = %v, want address", m.screen)
	}
	if m.firmware != device.FirmwareAlpine {
		t.Fatalf("firmware = %v", m.firmware)
	}
}

func TestCustomPasswordEditing(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenConfirm
	m.authState = install.AuthState{Configured: true, Known: true}
	m.passMode = passGenerate
	m.passwordReady = true

	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")}); !m.passEditing {
		t.Fatal("pressing 2 should start custom-password editing")
	}
	if m.focusedInput() == nil {
		t.Fatal("custom-password field is not focused")
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a-long-password")}); m.customPass.Value() != "a-long-password" {
		t.Fatalf("typed password = %q", m.customPass.Value())
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); !m.passwordReady || m.passEditing {
		t.Fatalf("password not captured: ready=%v editing=%v", m.passwordReady, m.passEditing)
	}
}

func TestCtrlCConfirmsWhileBusy(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.busy = true
	m.screen = screenWorking
	m.cancel = func() {}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); m.screen != screenAbort {
		t.Fatalf("screen = %v, want abort confirmation", m.screen)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); m.screen != screenWorking {
		t.Fatalf("declining abort returned to %v", m.screen)
	}
}

func TestAllScreensRender(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.generated = "generated-password-1234"
	m.passwordReady = true
	m.screen = screenConfirm
	m.det = device.Detection{Platform: device.PlatformRockchipStock, Arch: "aarch64"}
	m.hostKey = &hostKeyMsg{host: "10.0.0.2:22", fingerprint: "SHA256:abc", reply: make(chan bool, 1)}

	for _, s := range []screen{
		screenDevice, screenFirmware, screenAddress, screenConfirm, screenWorking,
		screenShowPassword, screenChangePassword, screenWiFi, screenWiFiPass, screenWiFiManual, screenApps,
		screenMarusia, screenSummary, screenError, screenHostKey, screenAbort,
	} {
		m.screen = s
		view := m.View()
		if strings.TrimSpace(view) == "" {
			t.Fatalf("screen %v rendered nothing", s)
		}
	}
}

func TestOSC52HasNoNewline(t *testing.T) {
	if strings.ContainsAny(osc52("secret"), "\r\n") {
		t.Fatal("OSC 52 payload must stay one line")
	}
}

type noopDevice struct{}

func (noopDevice) Connect(context.Context) error { return nil }
func (noopDevice) Run(context.Context, device.Command) (device.Result, error) {
	return device.Result{}, nil
}
func (noopDevice) Push(context.Context, string, string) error { return nil }
func (noopDevice) Addr() string                               { return "192.168.1.20:5555" }
func (noopDevice) Kind() string                               { return "adb" }
func (noopDevice) Close() error                               { return nil }

func TestOpDoneTransitions(t *testing.T) {
	m := newModel(Options{ConfigDir: t.TempDir()})
	m.width, m.height = 100, 40
	inst, err := install.New(noopDevice{}, nil, device.PlatformRockchipStock, m.opts.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	space := install.SpaceCheck{AvailableBytes: 1 << 30, RequiredBytes: 8 << 20, OK: true}
	if _, _ = m.Update(opDoneMsg{
		kind: opPrepare, inst: inst, space: &space,
		det:  device.Detection{Platform: device.PlatformRockchipStock, Arch: "aarch64"},
		auth: install.AuthState{Configured: false, Known: true},
		host: "capsula-mini",
	}); m.screen != screenConfirm {
		t.Fatalf("prepare -> %v, want confirm", m.screen)
	}
	if m.generated == "" {
		t.Fatal("prepare did not generate a password")
	}
	if !strings.Contains(m.View(), "Install capsulad") {
		t.Fatal("confirm view missing")
	}

	if _, _ = m.Update(opDoneMsg{kind: opDeploy, deploy: &install.DeployResult{Exit: 0}, fingerprint: "AA:BB"}); m.screen != screenShowPassword {
		t.Fatalf("deploy -> %v, want password", m.screen)
	}
	if _, _ = m.Update(opDoneMsg{
		kind:     opWiFiStatus,
		wifi:     install.WiFi{State: "disconnected", APActive: true, APSSID: "Capsula", APPasswordHint: "hint"},
		networks: []install.Network{{SSID: "Home", Security: "wpa2", SignalDBM: -40}},
	}); m.screen != screenWiFi {
		t.Fatalf("wifi status -> %v", m.screen)
	}
	if _, _ = m.Update(opDoneMsg{kind: opWiFiConnect, wifi: install.WiFi{State: "connected", SSID: "Home", IPAddress: "192.168.1.21"}}); m.screen != screenApps {
		t.Fatalf("wifi connect -> %v", m.screen)
	}
	if _, _ = m.Update(opDoneMsg{kind: opApps, results: []install.AppResult{{ID: "org.capsulad.simon", Name: "Simon Says", Status: "installed"}}}); m.screen != screenSummary {
		t.Fatalf("apps -> %v", m.screen)
	}
	if _, _ = m.Update(opDoneMsg{kind: opMarusiaProbe, marusia: install.MarusiaResult{Skipped: true}}); m.screen != screenMarusia {
		t.Fatalf("marusia probe -> %v", m.screen)
	}
	if _, _ = m.Update(opDoneMsg{kind: opMarusiaToken, marusia: install.MarusiaResult{TokenSet: true}}); m.screen != screenSummary {
		t.Fatalf("marusia token -> %v", m.screen)
	}
	m.screen = screenChangePassword
	m.pendingPass = "a-long-password"
	if _, _ = m.Update(opDoneMsg{kind: opChangePassword}); m.screen != screenShowPassword || m.generated != "a-long-password" {
		t.Fatalf("change password -> %v, generated=%q", m.screen, m.generated)
	}
	if m.pendingPass != "" {
		t.Fatal("pending password was not cleared")
	}

	if _, _ = m.Update(opDoneMsg{kind: opDeploy, err: errors.New("boom")}); m.screen != screenError {
		t.Fatalf("deploy error -> %v", m.screen)
	}
	if !m.errCanAddr {
		t.Fatal("deploy error should offer address re-entry")
	}
}

func TestWiFiSecuritySelection(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40

	// An open scan result joins with the explicit open mode.
	m.screen = screenWiFi
	m.networks = []install.Network{{SSID: "Cafe", Security: "", SignalDBM: -50}}
	m.wifiIx = 0
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.wifiSecurity != "open" || m.screen != screenWorking {
		t.Fatalf("open selection: security=%q screen=%v cmd=%v", m.wifiSecurity, m.screen, cmd)
	}

	// A secured scan result keeps its mode and asks for a passphrase.
	m.busy = false
	m.screen = screenWiFi
	m.networks = []install.Network{{SSID: "Home", Security: "WPA2", SignalDBM: -40}}
	m.wifiIx = 0
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenWiFiPass || m.wifiSecurity != "WPA2" {
		t.Fatalf("secured selection: security=%q screen=%v", m.wifiSecurity, m.screen)
	}

	// Manual open entry also passes open explicitly.
	m.screen = screenWiFiManual
	m.wifiManual = true
	m.wifiOpen = true
	m.wifiSSIDInput.SetValue("Cafe")
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.wifiSecurity != "open" || m.screen != screenWorking {
		t.Fatalf("manual open: security=%q screen=%v cmd=%v", m.wifiSecurity, m.screen, cmd)
	}

	// Manual secured entry leaves the backend's WPA2 default.
	m.busy = false
	m.wifiOpen = false
	m.screen = screenWiFiManual
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenWiFiPass || m.wifiSecurity != "" {
		t.Fatalf("manual secured: security=%q screen=%v", m.wifiSecurity, m.screen)
	}
}

func TestDeviceArrowOrder(t *testing.T) {
	m := newModel(Options{})
	if m.deviceIx != 1 {
		t.Fatalf("default device = %d, want Mini", m.deviceIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp}); m.deviceIx != 0 {
		t.Fatalf("up selected %d, want Capsula", m.deviceIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown}); m.deviceIx != 1 {
		t.Fatalf("down selected %d, want Mini", m.deviceIx)
	}
}

func TestMarusiaTokenFocus(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenWorking
	if _, _ = m.Update(opDoneMsg{kind: opMarusiaProbe, marusia: install.MarusiaResult{Skipped: true}}); m.screen != screenMarusia {
		t.Fatalf("screen = %v, want Marusia", m.screen)
	}
	if m.focusedInput() == nil {
		t.Fatal("Marusia token field was not focused")
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("tok")}); m.marusiaToken.Value() != "tok" {
		t.Fatalf("token value = %q", m.marusiaToken.Value())
	}
}

func TestChangePasswordKeepsAppliedValue(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenShowPassword
	m.passMode = passCustom
	m.customPass.SetValue("applied-password")
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); m.screen != screenChangePassword {
		t.Fatalf("screen = %v, want change password", m.screen)
	}
	if m.passMode != passGenerate || m.generated != "applied-password" {
		t.Fatalf("applied value lost: mode=%v generated=%q", m.passMode, m.generated)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}); m.screen != screenShowPassword {
		t.Fatalf("screen = %v, want password", m.screen)
	}
	if m.chosenPassword() != "applied-password" {
		t.Fatalf("chosen password = %q", m.chosenPassword())
	}
}

func TestIdentityAcknowledgement(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 50
	m.screen = screenConfirm
	m.det = device.Detection{Platform: device.PlatformRockchipStock, Arch: "aarch64"}
	m.identity = device.Identity{Hostname: "capsula-mini"}
	m.identityRequired = true
	m.acknowledged = true
	m.passMode = passGenerate
	m.generated = "generated-password-1234"
	m.passwordReady = true

	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}); m.screen != screenConfirm {
		t.Fatalf("install without identity acknowledgement -> %v", m.screen)
	}
	if !strings.Contains(m.status, "stable serial") {
		t.Fatalf("status = %q", m.status)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}); !m.identityAck {
		t.Fatal("x did not acknowledge the identity warning")
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}); m.screen != screenWorking {
		t.Fatalf("install after acknowledgement -> %v", m.screen)
	}
}

func TestSanitizesUntrustedTerminalText(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 50
	m.screen = screenWiFi
	m.networks = []install.Network{{
		SSID:      "evil\x1b]52;c;Zm9v\x07Home",
		Security:  "wpa2\x1b[31m",
		SignalDBM: -40,
	}}
	view := m.View()
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x07") {
		t.Fatalf("control sequence reached the view: %q", view)
	}
	if !strings.Contains(view, "evil") || !strings.Contains(view, "Home") {
		t.Fatalf("sanitising dropped the visible text: %q", view)
	}

	m.screen = screenShowPassword
	m.passMode = passGenerate
	m.generated = "pass\x1b[31m"
	view = m.View()
	if strings.Contains(view, "pass\x1b[31m") || !strings.Contains(view, `pass\x1b[31m`) {
		t.Fatalf("password control bytes were not escaped: %q", view)
	}
	if got := escapeControls("a\x1b\x07b"); got != `a\x1b\x07b` {
		t.Fatalf("escapeControls = %q", got)
	}

	m.logs = nil
	m.appendLog("line\x1b]52;c;bad\x07ok")
	if strings.Contains(strings.Join(m.logs, "\n"), "\x1b") || strings.Contains(strings.Join(m.logs, "\n"), "\x07") {
		t.Fatalf("log retained controls: %q", m.logs)
	}
}

func TestSecondCtrlCStopsImmediately(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.busy = true
	m.screen = screenWorking
	m.cancel = func() {}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); m.screen != screenAbort || m.prevScreen != screenWorking {
		t.Fatalf("first ctrl+c: screen=%v prev=%v", m.screen, m.prevScreen)
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil || !m.aborted {
		t.Fatalf("second ctrl+c should stop immediately: aborted=%v cmd=%v", m.aborted, cmd)
	}
	if m.abortedDeploy {
		t.Fatal("a non-deploy step must not be reported as a detached deployment")
	}
}

func TestManualSSIDThenPassphraseTyping(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenWiFi
	m.networks = nil
	m.wifiIx = 0

	// Enter on the synthetic "manual" row opens the SSID form.
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenWiFiManual {
		t.Fatalf("screen = %v, want manual SSID", m.screen)
	}
	if m.focusedInput() != &m.wifiSSIDInput {
		t.Fatal("SSID field is not focused")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("HomeNet")})
	if m.wifiSSIDInput.Value() != "HomeNet" {
		t.Fatalf("SSID value = %q", m.wifiSSIDInput.Value())
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenWiFiPass {
		t.Fatalf("screen = %v, want passphrase", m.screen)
	}
	// Regression: the passphrase field must receive focus after the manual
	// SSID form, otherwise every typed character is dropped.
	if m.focusedInput() != &m.wifiPassInput {
		t.Fatal("passphrase field is not focused after manual SSID entry")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("secret-pass")})
	if m.wifiPassInput.Value() != "secret-pass" {
		t.Fatalf("passphrase value = %q", m.wifiPassInput.Value())
	}
}

func TestConfirmViewFitsSmallTerminal(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 80, 24
	m.screen = screenConfirm
	m.det = device.Detection{Platform: device.PlatformRockchipStock, Arch: "aarch64"}
	m.authState = install.AuthState{Configured: true, Known: true}
	m.generated = "generated-password-1234"
	m.passwordReady = true
	m.passMode = passGenerate
	m.space = &install.SpaceCheck{
		AvailableBytes: 1 << 30, RequiredBytes: 8 << 20, OK: true,
		ReserveBytes: 8 << 20, BundleBytes: 24 << 20, AppBytes: 40 << 20,
	}

	view := m.View()
	lines := strings.Split(view, "\n")
	if len(lines) > m.height {
		t.Fatalf("view is %d lines, terminal is %d", len(lines), m.height)
	}
	if !strings.Contains(lines[len(lines)-1], "install") {
		t.Fatalf("primary action missing from the fixed footer: %q", lines[len(lines)-1])
	}
	// The required acknowledgement and the install action must be visible
	// without scrolling on a stock 80x24 terminal.
	for _, want := range []string{"Nothing is deleted", "Install capsulad"} {
		if !strings.Contains(view, want) {
			t.Fatalf("review screen hides %q at 24 rows", want)
		}
	}

	// A shorter terminal does clip, and then says how to scroll.
	m.height = 12
	view = m.View()
	if lines := strings.Split(view, "\n"); len(lines) > m.height {
		t.Fatalf("short view is %d lines, terminal is %d", len(lines), m.height)
	}
	if !strings.Contains(strings.Split(view, "\n")[len(strings.Split(view, "\n"))-1], "pgup/pgdn") {
		t.Fatalf("clipped view has no scroll hint: %q", view)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown}); m.scroll == 0 {
		t.Fatal("pgdown did not scroll the body")
	}
}

func TestErrorScreenMenuSelectsAction(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.showError(opDoneMsg{kind: opDeploy, err: errors.New("boom")})
	if m.screen != screenError {
		t.Fatalf("screen = %v, want error", m.screen)
	}
	acts := m.errorActions()
	if acts[0].key != "r" {
		t.Fatalf("first action = %q, want retry", acts[0].key)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown}); acts[m.errIx].key != "a" {
		t.Fatalf("after down: %q, want change-address", acts[m.errIx].key)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenAddress || m.fieldIx != 0 {
		t.Fatalf("enter on change-address: screen=%v field=%d", m.screen, m.fieldIx)
	}
}

func TestAbortMenuDefaultsToContinue(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenWorking
	m.busy = true
	m.op = opWiFiStatus
	m.cancel = func() {}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); m.screen != screenAbort {
		t.Fatalf("screen = %v, want abort", m.screen)
	}
	// The safe option is selected, so a stray enter does not stop the install.
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenWorking {
		t.Fatalf("enter on default option: screen = %v, want working", m.screen)
	}
	if m.aborted {
		t.Fatal("aborted without selecting the stop option")
	}
}

func TestDeployAbortIsTagged(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenWorking
	m.busy = true
	m.op = opDeploy
	m.cancel = func() {}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc}); m.screen != screenAbort {
		t.Fatalf("esc on working: screen = %v, want abort", m.screen)
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); cmd == nil {
		t.Fatal("y did not return a quit command")
	}
	if !m.aborted || !m.abortedDeploy {
		t.Fatalf("aborted=%v abortedDeploy=%v", m.aborted, m.abortedDeploy)
	}
}

func TestAddressTabMovesInputFocus(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenAddress
	m.firmware = device.FirmwareAlpine
	m.focusField()
	if m.focusedInput() != &m.addrInput {
		t.Fatal("address field is not focused on entry")
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}); m.fieldIx != 1 || m.focusedInput() != &m.sshUserInput {
		t.Fatalf("tab: field=%d focused=%v", m.fieldIx, m.focusedInput() == &m.sshUserInput)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.sshUserInput.Value() == "root" {
		t.Fatalf("typing went to the wrong field: ssh user = %q", m.sshUserInput.Value())
	}
}

func TestManualSSIDRejectsUnsafeValue(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenWiFiManual
	m.wifiManual = true
	m.wifiSSIDInput.SetValue("-evil")
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenWiFiManual {
		t.Fatalf("unsafe manual SSID advanced to %v", m.screen)
	}
	if !strings.Contains(m.status, "starts with") {
		t.Fatalf("status = %q", m.status)
	}
}
