package ui

import (
	"context"
	"errors"
	"reflect"
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
	m.firstIx = 2
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenDevice {
		t.Fatalf("Capsula should stay on the device screen, got %v", m.screen)
	}
	if !strings.Contains(m.status, "not supported") {
		t.Fatalf("status = %q", m.status)
	}

	m.firstIx = 3
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

	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown}); m.formIx != 1 {
		t.Fatalf("down selected row %d, want custom password", m.formIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); !m.passEditing {
		t.Fatal("enter on 'Set my own' should start custom-password editing")
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
		screenDevice, screenFirmware, screenAddress, screenConfirm, screenReview, screenWorking,
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
		det:      device.Detection{Platform: device.PlatformRockchipStock, Arch: "aarch64"},
		auth:     install.AuthState{Configured: false, Known: true},
		host:     "capsula-mini",
		identity: device.Identity{Serial: "SN12345", Hostname: "capsula-mini"},
	}); m.screen != screenConfirm {
		t.Fatalf("prepare -> %v, want confirm", m.screen)
	}
	if m.generated == "" {
		t.Fatal("prepare did not generate a password")
	}
	if !strings.Contains(m.View(), "Installation options") {
		t.Fatal("options form missing")
	}
	// The form is step 4; continuing opens the separate installation summary.
	m.formIx = len(m.formRows()) - 1
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenReview {
		t.Fatalf("continue -> %v, want installation summary", m.screen)
	}
	if !strings.Contains(m.View(), "Install capsulad") {
		t.Fatal("installation summary missing")
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
	if !m.errCanBack || m.backScreen != screenReview {
		t.Fatalf("deploy error back target = %v (canBack=%v), want installation summary", m.backScreen, m.errCanBack)
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

func TestFirstScreenArrowOrderAndLanguage(t *testing.T) {
	m := newModel(Options{})
	if m.firstIx != 0 {
		t.Fatalf("default row = %d, want English", m.firstIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown}); m.firstIx != 1 {
		t.Fatalf("down selected row %d, want Russian", m.firstIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.lang != langRU || m.screen != screenDevice {
		t.Fatalf("selecting Russian: lang=%v screen=%v", m.lang, m.screen)
	}
	if !strings.Contains(m.View(), "Выберите язык и устройство") {
		t.Fatal("Russian catalog was not applied")
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp}); m.firstIx != 0 {
		t.Fatalf("row = %d, want English", m.firstIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.lang != langEN {
		t.Fatalf("selecting English: lang=%v", m.lang)
	}
	m.firstIx = 3
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenFirmware {
		t.Fatalf("enter on Mini -> %v, want firmware", m.screen)
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
	m.passMode = passGenerate
	m.generated = "generated-password-1234"
	m.passwordReady = true

	// Letter shortcuts are gone; only arrow navigation and enter work.
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}); m.identityAck {
		t.Fatal("x should not toggle the identity row")
	}

	m.formIx = len(m.formRows()) - 1
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenConfirm {
		t.Fatalf("continue without identity acknowledgement -> %v", m.screen)
	}
	if !strings.Contains(m.status, "stable serial") {
		t.Fatalf("status = %q", m.status)
	}
	if m.formIx != m.formRowIndex(rowIdentityAck) {
		t.Fatalf("cursor moved to %d, want the identity row", m.formIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); !m.identityAck {
		t.Fatal("enter did not acknowledge the identity warning")
	}
	m.formIx = len(m.formRows()) - 1
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.screen != screenReview {
		t.Fatalf("continue after acknowledgement -> %v, want summary", m.screen)
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

func TestFormAndSummaryFitSmallTerminal(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 80, 24
	m.screen = screenConfirm
	m.det = device.Detection{Platform: device.PlatformRockchipStock, Arch: "aarch64"}
	m.authState = install.AuthState{Configured: true, Known: true}
	m.generated = "generated-password-1234"
	m.passwordReady = true
	m.passMode = passGenerate
	m.backup = &device.BackupResult{
		Path:    "/Users/me/.config/capsulad-installer/backups/capsula-mini-20261123.tar.gz",
		Bytes:   12 << 20,
		Entries: 34,
	}
	m.space = &install.SpaceCheck{
		AvailableBytes: 1 << 30, RequiredBytes: 8 << 20, OK: true,
		ReserveBytes: 8 << 20, BundleBytes: 24 << 20, AppBytes: 40 << 20,
	}

	// Step 4: the options form and its continue action.
	view := m.View()
	if lines := strings.Split(view, "\n"); len(lines) > m.height {
		t.Fatalf("form view is %d lines, terminal is %d", len(lines), m.height)
	}
	for _, want := range []string{"Installation options", "Continue"} {
		if !strings.Contains(view, want) {
			t.Fatalf("form hides %q at 24 rows", want)
		}
	}

	// Step 5: the installation summary and its install action.
	m.screen = screenReview
	view = m.View()
	if lines := strings.Split(view, "\n"); len(lines) > m.height {
		t.Fatalf("summary view is %d lines, terminal is %d", len(lines), m.height)
	}
	for _, want := range []string{"Installation summary", "Install capsulad"} {
		if !strings.Contains(view, want) {
			t.Fatalf("summary hides %q at 24 rows", want)
		}
	}

	// Step 6: the completion summary states what was preserved.
	m.screen = screenSummary
	view = m.View()
	if lines := strings.Split(view, "\n"); len(lines) > m.height {
		t.Fatalf("completion view is %d lines, terminal is %d", len(lines), m.height)
	}
	for _, want := range []string{"Installation complete", "Nothing was deleted"} {
		if !strings.Contains(view, want) {
			t.Fatalf("completion hides %q at 24 rows", want)
		}
	}
	m.screen = screenReview

	// A shorter terminal clips, and then says how to scroll.
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

func TestCatalogsComplete(t *testing.T) {
	check := func(c *catalog, name string) {
		v := reflect.ValueOf(*c)
		for i := range v.NumField() {
			f := v.Field(i)
			field := v.Type().Field(i).Name
			switch f.Kind() {
			case reflect.String:
				if strings.TrimSpace(f.String()) == "" {
					t.Errorf("%s.%s is empty", name, field)
				}
			case reflect.Int:
				if f.Int() <= 0 {
					t.Errorf("%s.%s = %d", name, field, f.Int())
				}
			case reflect.Map:
				if f.IsNil() || f.Len() == 0 {
					t.Errorf("%s.%s is empty", name, field)
				}
			}
		}
	}
	check(&enCatalog, "en")
	check(&ruCatalog, "ru")
	for id := range enCatalog.appDescriptions {
		if ruCatalog.appDescriptions[id] == "" {
			t.Errorf("missing Russian app description for %s", id)
		}
	}
	for p := range enCatalog.platformLabels {
		if ruCatalog.platformLabels[p] == "" {
			t.Errorf("missing Russian platform label for %s", p)
		}
	}
}

func TestRussianRenderingAndInputPreserved(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 80, 30
	m.addrInput.SetValue("192.168.1.42")
	m.setLanguage(langRU)
	if m.addrInput.Value() != "192.168.1.42" {
		t.Fatal("switching language cleared the address")
	}
	if got := m.addrInput.Placeholder; got != ruCatalog.addrPlaceholder {
		t.Fatalf("placeholder = %q", got)
	}
	if !strings.Contains(m.View(), "Язык") || !strings.Contains(m.View(), "Устройство") {
		t.Fatal("Russian first screen missing")
	}
	m.screen = screenConfirm
	m.authState = install.AuthState{Configured: true, Known: true}
	m.generated = "generated-password-1234"
	m.passwordReady = true
	if !strings.Contains(m.View(), "Параметры установки") || !strings.Contains(m.View(), "Далее") {
		t.Fatal("Russian options form missing")
	}
	m.screen = screenReview
	m.det = device.Detection{Platform: device.PlatformRockchipStock, Arch: "aarch64"}
	if !strings.Contains(m.View(), "Сводка установки") || !strings.Contains(m.View(), "Установить capsulad") {
		t.Fatal("Russian installation summary missing")
	}
}

func TestFormHasNoLetterShortcuts(t *testing.T) {
	m := newModel(Options{})
	m.width, m.height = 100, 40
	m.screen = screenConfirm
	m.authState = install.AuthState{Configured: true, Known: true}
	m.formIx = m.formRowIndex(rowJingle)
	jingle := m.jingle
	for _, ch := range []string{"j", "a", "f", "x", "1", "2", "3"} {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(ch)})
	}
	if m.jingle != jingle || m.forceInstall || m.passEditing || m.formIx != m.formRowIndex(rowJingle) {
		t.Fatal("a letter shortcut changed the form")
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}); m.jingle == jingle {
		t.Fatal("enter on the jingle row did not toggle it")
	}
	m.identityRequired = true
	m.formIx = m.formRowIndex(rowIdentityAck)
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}); !m.identityAck {
		t.Fatal("space on the acknowledgement row did not toggle it")
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd}); m.formIx != len(m.formRows())-1 {
		t.Fatalf("end selected row %d, want continue", m.formIx)
	}
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome}); m.formIx != 0 {
		t.Fatalf("home selected row %d, want the first row", m.formIx)
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
