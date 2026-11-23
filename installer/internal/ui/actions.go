package ui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"capsulad/installer/internal/device"
	"capsulad/installer/internal/install"
	"capsulad/installer/internal/release"
)

func (m *model) key(k tea.KeyMsg) (bool, tea.Cmd) {
	s := k.String()
	// Screens taller than the terminal scroll as whole-body pages; screens with
	// their own cursor keep up/down for selection.
	if m.screen != screenWorking {
		switch s {
		case "pgup":
			m.scrollBy(-m.pageStep())
			return true, nil
		case "pgdown":
			m.scrollBy(m.pageStep())
			return true, nil
		case "home":
			if m.screen != screenConfirm {
				m.scroll = 0
				return true, nil
			}
		case "end":
			if m.screen != screenConfirm {
				m.scroll = 1 << 20
				return true, nil
			}
		}
	}
	switch m.screen {
	case screenHostKey:
		switch s {
		case "y", "Y", "enter":
			m.hostKey.reply <- true
			m.hostKey = nil
			m.screen = m.prevScreen
			m.status = m.cat.statusHostKeyPinned
		case "n", "N", "esc":
			m.hostKey.reply <- false
			m.hostKey = nil
			m.screen = m.prevScreen
			m.status = m.cat.statusHostKeyRefused
		}
		return true, nil

	case screenAbort:
		switch s {
		case "up", "k", "down", "j":
			m.abortIx = 1 - m.abortIx
		case "y", "Y":
			return true, m.stopAndQuit()
		case "enter":
			if m.abortIx == 1 {
				return true, m.stopAndQuit()
			}
			m.screen = m.prevScreen
		case "n", "N", "esc":
			m.screen = m.prevScreen
		}
		return true, nil

	case screenDevice:
		switch s {
		case "up", "k":
			if m.firstIx > 0 {
				m.firstIx--
			}
			m.status = ""
		case "down", "j":
			if m.firstIx < 3 {
				m.firstIx++
			}
			m.status = ""
		case "enter", " ":
			switch m.firstIx {
			case 0:
				m.setLanguage(langEN)
			case 1:
				m.setLanguage(langRU)
			case 2:
				m.status = m.cat.statusCapsulaUnsupported
			case 3:
				m.screen = screenFirmware
			}
		case "esc", "q":
			return true, tea.Quit
		}
		return true, nil

	case screenFirmware:
		switch s {
		case "up", "k":
			m.firmwareIx = 0
		case "down", "j":
			m.firmwareIx = 1
		case "enter":
			m.firmware = device.FirmwareStock
			if m.firmwareIx == 1 {
				m.firmware = device.FirmwareAlpine
			}
			m.screen = screenAddress
			m.fieldIx = 0
			m.status = ""
		case "esc":
			m.screen = screenDevice
		}
		return true, nil

	case screenAddress:
		if s == "enter" {
			if strings.TrimSpace(m.addrInput.Value()) == "" {
				m.status = m.cat.statusAddressEmpty
				return true, nil
			}
			if m.firmware == device.FirmwareAlpine && m.sshPassInput.Value() == "" && strings.TrimSpace(m.sshKeyInput.Value()) == "" {
				m.status = m.cat.statusAlpineCreds
				return true, nil
			}
			m.status = ""
			return true, m.startOp(opPrepare)
		}
		if s == "tab" || s == "shift+tab" {
			if m.firmware == device.FirmwareAlpine {
				if s == "tab" {
					m.fieldIx = (m.fieldIx + 1) % 4
				} else {
					m.fieldIx = (m.fieldIx + 3) % 4
				}
			}
			return true, nil
		}
		if s == "esc" {
			m.screen = screenFirmware
			return true, nil
		}
		return false, nil

	case screenConfirm:
		if m.passEditing {
			switch s {
			case "enter", "tab":
				pw := m.customPass.Value()
				if err := install.ValidatePassword(pw); err != nil {
					m.status = m.passwordError(pw, err)
					return true, nil
				}
				m.passwordReady = true
				m.passEditing = false
				m.customPass.Blur()
				m.status = m.cat.passwordCaptured
				return true, nil
			case "esc":
				m.passEditing = false
				m.customPass.Blur()
				m.passMode = passGenerate
				m.passwordReady = true
				m.formIx = m.formRowIndex(rowPassGenerate)
				m.status = ""
				return true, nil
			}
			return false, nil
		}
		rows := m.formRows()
		if len(rows) == 0 {
			return true, nil
		}
		if m.formIx >= len(rows) {
			m.formIx = len(rows) - 1
		}
		if m.formIx < 0 {
			m.formIx = 0
		}
		switch s {
		case "up":
			if m.formIx > 0 {
				m.formIx--
			}
		case "down":
			if m.formIx < len(rows)-1 {
				m.formIx++
			}
		case "home":
			m.formIx = 0
		case "end":
			m.formIx = len(rows) - 1
		case "esc":
			m.screen = screenAddress
			m.status = ""
		case "enter", " ":
			switch rows[m.formIx] {
			case rowPassGenerate:
				m.passMode = passGenerate
				m.passwordReady = true
			case rowPassCustom:
				m.passMode = passCustom
				m.passwordReady = false
				m.passEditing = true
				m.customPass.Focus()
				m.status = m.cat.passCustomHint
			case rowPassKeep:
				m.passMode = passKeep
				m.passwordReady = true
			case rowJingle:
				m.jingle = !m.jingle
			case rowIdentityAck:
				m.identityAck = !m.identityAck
			case rowAcknowledge:
				m.acknowledged = !m.acknowledged
			case rowForce:
				m.forceInstall = !m.forceInstall
			case rowContinue:
				if msg := m.formValidation(); msg != "" {
					m.status = msg
					return true, nil
				}
				m.status = ""
				m.screen = screenReview
			}
		}
		return true, nil

	case screenReview:
		switch s {
		case "enter", "i":
			m.status = ""
			return true, m.startOp(opDeploy)
		case "esc":
			m.screen = screenConfirm
			m.status = ""
		}
		return true, nil

	case screenWorking:
		if s == "pgup" || s == "pgdown" || s == "up" || s == "down" {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(k)
			return true, cmd
		}
		if s == "esc" {
			m.prevScreen = screenWorking
			m.abortIx = 0
			m.screen = screenAbort
		}
		return true, nil

	case screenShowPassword:
		switch s {
		case "c":
			if m.chosenPassword() != "" {
				m.copied = true
				return true, tea.Printf("%s", osc52(m.chosenPassword()))
			}
		case "n":
			// The applied custom password must survive opening the change
			// screen; otherwise the UI would later claim it kept the old one.
			if m.passMode == passCustom && m.customPass.Value() != "" {
				m.generated = m.customPass.Value()
				m.passMode = passGenerate
			}
			m.customPass.SetValue("")
			m.pendingPass = ""
			m.status = ""
			m.screen = screenChangePassword
			return true, nil
		case " ":
			m.passwordAcked = !m.passwordAcked
		case "enter":
			if !m.passwordAcked && m.chosenPassword() != "" {
				m.status = m.cat.statusConfirmStored
				return true, nil
			}
			m.screen = screenWorking
			return true, m.startOp(opWiFiStatus)
		}
		return true, nil

	case screenChangePassword:
		switch s {
		case "enter":
			pw := m.customPass.Value()
			if err := install.ValidatePassword(pw); err != nil {
				m.status = m.passwordError(pw, err)
				return true, nil
			}
			m.pendingPass = pw
			m.customPass.SetValue("")
			m.screen = screenWorking
			return true, m.startOp(opChangePassword)
		case "esc":
			m.customPass.SetValue("")
			m.screen = screenShowPassword
		}
		return false, nil

	case screenWiFi:
		total := len(m.networks) + 1
		switch s {
		case "up", "k":
			if m.wifiIx > 0 {
				m.wifiIx--
			}
		case "down", "j":
			if m.wifiIx < total-1 {
				m.wifiIx++
			}
		case "enter":
			if m.wifiIx == len(m.networks) {
				m.wifiManual = true
				m.screen = screenWiFiManual
				return true, nil
			}
			if m.wifiIx < len(m.networks) {
				n := m.networks[m.wifiIx]
				m.wifiManual = false
				m.wifiSecurity = install.SecurityMode(n.Security)
				if install.SecurityNeedsPassword(n.Security) {
					m.wifiPassInput.SetValue("")
					m.screen = screenWiFiPass
					return true, nil
				}
				m.screen = screenWorking
				return true, m.startWiFiConnect(n.SSID, m.wifiSecurity, "")
			}
		case "m":
			m.wifiManual = true
			m.screen = screenWiFiManual
		case "r":
			m.screen = screenWorking
			return true, m.startOp(opWiFiStatus)
		case "s":
			m.wifiSkip = true
			m.screen = screenApps
			m.logFunc()(m.cat.logWiFiSkipped)
		case "b":
			m.screen = screenShowPassword
		}
		return true, nil

	case screenWiFiPass:
		switch s {
		case "enter":
			ssid := m.selectedSSID()
			if ssid == "" {
				m.status = m.cat.statusNoNetwork
				return true, nil
			}
			m.screen = screenWorking
			return true, m.startWiFiConnect(ssid, m.wifiSecurity, m.wifiPassInput.Value())
		case "esc":
			m.screen = screenWiFi
		}
		return false, nil

	case screenWiFiManual:
		switch s {
		case "ctrl+o":
			m.wifiOpen = !m.wifiOpen
		case "enter":
			ssid := strings.TrimSpace(m.wifiSSIDInput.Value())
			if ssid == "" {
				m.status = m.cat.statusEnterSSID
				return true, nil
			}
			if err := install.ValidateSSID(ssid); err != nil {
				m.status = m.ssidError(ssid)
				if m.status == "" {
					m.status = err.Error()
				}
				return true, nil
			}
			if m.wifiOpen {
				m.wifiSecurity = "open"
				m.screen = screenWorking
				return true, m.startWiFiConnect(ssid, "open", "")
			}
			m.wifiSecurity = ""
			m.wifiPassInput.SetValue("")
			m.screen = screenWiFiPass
			return true, nil
		case "esc":
			m.screen = screenWiFi
		}
		return false, nil

	case screenApps:
		switch s {
		case "up", "k":
			if m.appIx > 0 {
				m.appIx--
			}
		case "down", "j":
			if m.appIx < len(install.Catalogue)-1 {
				m.appIx++
			}
		case " ":
			id := install.Catalogue[m.appIx].ID
			m.appSelected[id] = !m.appSelected[id]
		case "a":
			for _, a := range install.Catalogue {
				m.appSelected[a.ID] = true
			}
		case "n":
			m.appSelected = map[string]bool{}
		case "enter":
			m.screen = screenWorking
			return true, m.startOp(opApps)
		case "s":
			m.appSelected = map[string]bool{}
			m.screen = screenWorking
			return true, m.startOp(opApps)
		case "b":
			m.screen = screenWiFi
		}
		return true, nil

	case screenMarusia:
		switch s {
		case "enter":
			m.screen = screenWorking
			return true, m.startOp(opMarusiaToken)
		case "esc":
			m.screen = screenSummary
		}
		return false, nil

	case screenSummary:
		switch s {
		case "enter", "q", "esc":
			return true, tea.Quit
		}
		return true, nil

	case screenError:
		acts := m.errorActions()
		switch s {
		case "up", "k":
			if m.errIx > 0 {
				m.errIx--
			}
		case "down", "j":
			if m.errIx < len(acts)-1 {
				m.errIx++
			}
		case "enter":
			if m.errIx < len(acts) {
				return m.runErrAction(acts[m.errIx].key)
			}
		case "r", "b", "a", "s", "q":
			return m.runErrAction(s)
		}
		return true, nil
	}
	return false, nil
}

// errAction is one selectable recovery step on the error screen.
type errAction struct {
	key   string
	label string
}

// errorActions lists the safe recovery choices for the step that failed. The
// first entry is the primary action and the last is always Quit.
func (m *model) errorActions() []errAction {
	var acts []errAction
	if m.errOp != opNone {
		acts = append(acts, errAction{"r", m.cat.actionRetry})
	}
	switch m.errOp {
	case opWiFiStatus, opWiFiConnect:
		if m.errCanAddr {
			acts = append(acts, errAction{"a", m.cat.actionChangeAddress})
		}
		if m.errCanSkip {
			acts = append(acts, errAction{"s", m.cat.actionSkipWiFi})
		}
	case opApps:
		if m.errCanBack {
			acts = append(acts, errAction{"b", m.cat.actionBackApps})
		}
		if m.errCanSkip {
			acts = append(acts, errAction{"s", m.cat.actionSkipSummary})
		}
	case opMarusiaProbe, opMarusiaToken:
		if m.errCanSkip {
			acts = append(acts, errAction{"s", m.cat.actionSkipSummary})
		}
	default:
		if m.errCanAddr {
			acts = append(acts, errAction{"a", m.cat.actionChangeAddress})
		}
		if m.errCanBack {
			acts = append(acts, errAction{"b", m.cat.actionBack})
		}
	}
	return append(acts, errAction{"q", m.cat.actionQuit})
}

func (m *model) runErrAction(key string) (bool, tea.Cmd) {
	switch key {
	case "r":
		if m.errOp != opNone {
			return true, m.startOp(m.errOp)
		}
	case "b":
		if m.errCanBack {
			m.screen = m.backScreen
		}
	case "a":
		if m.errCanAddr {
			m.fieldIx = 0
			m.screen = screenAddress
			m.status = m.cat.statusReenterAddress
		}
	case "s":
		if m.errCanSkip {
			m.screen = screenSummary
		}
	case "q":
		return true, tea.Quit
	}
	return true, nil
}

// formRows lists the focusable rows of the options form in display order.
func (m *model) formRows() []formRow {
	rows := []formRow{rowPassGenerate, rowPassCustom}
	if m.authState.Configured {
		rows = append(rows, rowPassKeep)
	}
	rows = append(rows, rowJingle)
	if m.identityRequired {
		rows = append(rows, rowIdentityAck)
	}
	rows = append(rows, rowAcknowledge)
	if m.space != nil && !m.space.OK {
		rows = append(rows, rowForce)
	}
	return append(rows, rowContinue)
}

func (m *model) formRowIndex(k formRow) int {
	for i, r := range m.formRows() {
		if r == k {
			return i
		}
	}
	return 0
}

// formValidation checks the choices before moving to the installation summary.
// It moves the cursor to the row that needs attention.
func (m *model) formValidation() string {
	if m.passMode == passCustom && !m.passwordReady {
		m.formIx = m.formRowIndex(rowPassCustom)
		return m.cat.statusCustomRequired
	}
	if m.identityRequired && !m.identityAck {
		m.formIx = m.formRowIndex(rowIdentityAck)
		return m.cat.statusIdentityRequired
	}
	if !m.acknowledged {
		m.formIx = m.formRowIndex(rowAcknowledge)
		return m.cat.statusAckRequired
	}
	if m.space != nil && !m.space.OK && !m.forceInstall {
		m.formIx = m.formRowIndex(rowForce)
		return m.cat.statusSpaceRequired
	}
	return ""
}

// passwordError localises the two cheap preconditions install enforces.
func (m *model) passwordError(pw string, err error) string {
	if len(pw) < install.MinCustomPasswordLength {
		return fmt.Sprintf(m.cat.errPasswordShortFormat, install.MinCustomPasswordLength)
	}
	if strings.ContainsAny(pw, "\r\n") {
		return m.cat.errPasswordNewline
	}
	return err.Error()
}

// ssidError localises install.ValidateSSID's conditions.
func (m *model) ssidError(ssid string) string {
	switch {
	case strings.TrimSpace(ssid) == "":
		return m.cat.errSSIDEmpty
	case strings.HasPrefix(ssid, "-"):
		return fmt.Sprintf(m.cat.errSSIDStartsDashFormat, ssid)
	case hasControl(ssid):
		return m.cat.errSSIDControl
	}
	return ""
}

func (m *model) platformLabel(p device.Platform) string {
	if s, ok := m.cat.platformLabels[p]; ok {
		return s
	}
	return p.Label()
}

func (m *model) appDescription(a install.App) string {
	if s, ok := m.cat.appDescriptions[a.ID]; ok {
		return s
	}
	return a.Description
}

func (m *model) recoveryAdvice(address, apSSID, apHint string) string {
	var b strings.Builder
	fmt.Fprintf(&b, m.cat.recoveryAddressFormat+"\n", address)
	b.WriteString(m.cat.recoveryWait + "\n")
	if apSSID != "" {
		fmt.Fprintf(&b, m.cat.recoveryNetworkFormat+"\n", apSSID)
	}
	if apHint != "" {
		fmt.Fprintf(&b, m.cat.recoveryHintFormat+"\n", apHint)
	} else {
		b.WriteString(m.cat.recoveryPasswordNote + "\n")
	}
	fmt.Fprintf(&b, m.cat.recoveryPortalFormat+"\n", "http://192.168.4.1")
	b.WriteString(m.cat.recoveryRetry)
	return b.String()
}

func (m *model) selectedSSID() string {
	if m.wifiManual {
		return strings.TrimSpace(m.wifiSSIDInput.Value())
	}
	if m.wifiIx >= 0 && m.wifiIx < len(m.networks) {
		return m.networks[m.wifiIx].SSID
	}
	return ""
}

func (m *model) chosenPassword() string {
	switch m.passMode {
	case passKeep:
		return ""
	case passCustom:
		return m.customPass.Value()
	default:
		return m.generated
	}
}

func osc52(s string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07"
}

func (m *model) startOp(kind opKind) tea.Cmd {
	m.busy = true
	m.op = kind
	m.opStart = time.Now()
	m.screen = screenWorking
	m.status = m.opLabel(kind)
	m.progress = ""
	switch kind {
	case opPrepare:
		return m.prepareCmd()
	case opDeploy:
		m.acknowledged = false
		return m.deployCmd()
	case opWiFiStatus:
		return m.wifiStatusCmd()
	case opWiFiConnect:
		return m.wifiConnectCmd(m.selectedSSID(), m.wifiSecurity, m.wifiPassInput.Value())
	case opApps:
		return m.appsCmd()
	case opMarusiaProbe:
		return m.marusiaCmd("")
	case opMarusiaToken:
		return m.marusiaCmd(m.marusiaToken.Value())
	case opChangePassword:
		return m.changePasswordCmd()
	}
	return nil
}

func (m *model) startWiFiConnect(ssid, security, pass string) tea.Cmd {
	m.busy = true
	m.op = opWiFiConnect
	m.opStart = time.Now()
	m.screen = screenWorking
	m.status = m.cat.connectingWiFi
	return m.wifiConnectCmd(ssid, security, pass)
}

func (m *model) opLabel(kind opKind) string {
	switch kind {
	case opPrepare:
		return m.cat.opPrepare
	case opDeploy:
		return m.cat.opDeploy
	case opWiFiStatus:
		return m.cat.opWiFiStatus
	case opWiFiConnect:
		return m.cat.opWiFiConnect
	case opApps:
		return m.cat.opApps
	case opMarusiaProbe, opMarusiaToken:
		return m.cat.opMarusia
	case opChangePassword:
		return m.cat.opChangePassword
	}
	return m.cat.opWorking
}

func (m *model) prepareCmd() tea.Cmd {
	opts := m.opts
	firmware := m.firmware
	addr := strings.TrimSpace(m.addrInput.Value())
	user := m.sshUserInput.Value()
	pass := m.sshPassInput.Value()
	key := strings.TrimSpace(m.sshKeyInput.Value())
	ctx := m.ctx
	logf := m.logFunc()
	progress := m.progressFunc()
	prompt := m.hostKeyPrompt
	return func() tea.Msg {
		res := opDoneMsg{kind: opPrepare}
		if addr == "" {
			res.err = errors.New("device address is required")
			return res
		}

		var provider release.Provider
		if opts.ArtifactsDir != "" {
			coreProvider, manifest, err := release.OfflineProvider(opts.ArtifactsDir)
			if err != nil {
				res.err = err
				return res
			}
			logf("offline release %s from %s", manifest.Version, opts.ArtifactsDir)
			provider = coreProvider
			appManifest, err := release.LoadAppsFile(filepath.Join(opts.ArtifactsDir, release.AppsFile))
			if err == nil {
				logf("offline app channel %s", appManifest.Version)
				provider = release.NewCombinedProvider(coreProvider, release.NewLocalAppsProvider(opts.ArtifactsDir, appManifest))
			} else if !os.IsNotExist(err) {
				res.err = err
				return res
			}
		} else {
			f := release.NewFetcher(releaseBase(opts), opts.CacheDir)
			f.Log = logf
			f.Progress = func(label string, done, total int64) { progress(label, done, total) }
			logf("fetching the release manifest from %s", f.ManifestURL())
			manifest, err := f.FetchManifest(ctx)
			if err != nil {
				res.err = err
				return res
			}
			logf("release %s", manifest.Version)
			coreProvider := release.NewRemoteProvider(f, manifest)
			provider = coreProvider

			appFetcher := release.NewFetcher(appsBase(opts), opts.CacheDir)
			appFetcher.Log = logf
			appFetcher.Progress = func(label string, done, total int64) { progress(label, done, total) }
			logf("fetching the app channel from %s", appFetcher.AppsURL())
			appManifest, appErr := appFetcher.FetchAppsManifest(ctx)
			if appErr != nil {
				if opts.AppsURL != "" || opts.AppsTag != "" {
					res.err = appErr
					return res
				}
				logf("app channel unavailable (%v); using apps embedded in the core release", appErr)
			} else {
				logf("app channel %s", appManifest.Version)
				provider = release.NewCombinedProvider(coreProvider, release.NewRemoteAppsProvider(appFetcher, appManifest))
			}
		}

		var dev device.Device
		switch firmware {
		case device.FirmwareAlpine:
			known, err := device.LoadKnownHosts(filepath.Join(opts.ConfigDir, "known_hosts.json"))
			if err != nil {
				res.err = err
				return res
			}
			sshDev := device.NewSSH(addr, user)
			sshDev.Password = pass
			sshDev.KeyPath = key
			sshDev.Known = known
			sshDev.Log = logf
			sshDev.Prompt = prompt
			dev = sshDev
			logf("SSH to %s as %s (host key pinned on first use)", addr, user)
		default:
			adb := device.NewADB(opts.ADBPath, addr)
			adb.Log = logf
			dev = adb
			logf("ADB to %s", adb.Addr())
		}

		err := dev.Connect(ctx)
		if errors.Is(err, device.ErrUnauthorized) {
			logf(m.cat.logAcceptDebugging)
			if waiter, ok := dev.(interface {
				WaitAuthorized(context.Context) error
			}); ok {
				authCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
				err = waiter.WaitAuthorized(authCtx)
				cancel()
			}
		}
		if err != nil {
			res.err = err
			return res
		}
		det, err := device.Detect(ctx, dev)
		if err != nil {
			res.err = err
			return res
		}
		logf("detected %s (%s, %s)", det.Platform.Label(), det.Platform, det.Arch)
		if err := device.CheckFirmware(firmware, det.Platform); err != nil {
			res.err = err
			return res
		}
		identity, idErr := device.ReadIdentity(ctx, dev)
		if idErr != nil {
			logf("warning: could not read a stable device identity: %v", idErr)
		}
		res.identity = identity
		if identity.Available() {
			logf("device identity: %s", identity)
		} else if dev.Kind() == "adb" {
			logf(m.cat.logNoStableSerial)
		}
		inst, err := install.New(dev, provider, det.Platform, opts.ConfigDir)
		if err != nil {
			res.err = err
			return res
		}
		inst.Log = logf
		auth, err := inst.AuthStatus(ctx)
		if err != nil {
			logf("could not read the existing admin-password state: %v", err)
		}
		host := inst.Hostname(ctx)
		res.inst, res.det, res.auth, res.host = inst, det, auth, host

		if det.Platform.Stock() {
			b, err := inst.Backup(ctx, addr)
			if err != nil {
				res.err = err
				return res
			}
			res.backup = &b
			check, err := inst.PreflightStock(ctx)
			if err != nil {
				res.err = err
				return res
			}
			res.space = &check
			if !check.OK {
				apps, _ := inst.InstalledApps(ctx)
				res.appsOff = apps
			}
		} else {
			check, err := inst.PreflightAlpine(ctx, nil)
			if err != nil {
				res.err = err
				return res
			}
			res.space = &check
		}
		return res
	}
}

func (m *model) deployCmd() tea.Cmd {
	inst := m.inst
	if inst == nil {
		return func() tea.Msg {
			return opDoneMsg{kind: opDeploy, err: errors.New("no device session")}
		}
	}
	password := m.chosenPassword()
	mode := m.passMode
	configured := m.authState.Configured
	hostname := m.hostname
	jingle := m.jingle
	addr := strings.TrimSpace(m.addrInput.Value())
	ctx := m.ctx
	logf := m.logFunc()
	expectedIdentity := m.identity

	var before install.BeforeLaunch
	if configured && mode == passKeep {
		logf(m.cat.logKeepingPassword)
	} else {
		before = func(ctx context.Context) error { return inst.ApplyPassword(ctx, password, hostname, addr) }
	}

	return func() tea.Msg {
		res := opDoneMsg{kind: opDeploy}
		if configured && mode == passKeep {
			if err := inst.VerifyKeptCredentials(ctx); err != nil {
				res.err = err
				return res
			}
		}
		// Confirm the peer before sending the admin password; ADB has no
		// cryptographic transport identity.
		if err := inst.VerifyIdentity(ctx, expectedIdentity); err != nil {
			res.err = err
			return res
		}
		// Credentials are created before the detached launch: a fresh daemon
		// only opens its HTTPS listener when they exist at startup.
		dep, err := inst.Deploy(ctx, jingle, before)
		res.deploy = &dep
		if err != nil {
			res.err = err
			return res
		}
		if err := inst.VerifyIdentity(ctx, expectedIdentity); err != nil {
			if dep.CredentialsApplied {
				err = fmt.Errorf("%w (credentials were written before the deployment)", err)
			}
			res.err = err
			return res
		}
		fp, err := inst.WaitHealthy(ctx)
		if err != nil {
			if dep.CredentialsApplied {
				res.err = fmt.Errorf("credentials were written before the deployment, but the Web UI health check failed: %w", err)
			} else {
				res.err = err
			}
			return res
		}
		res.fingerprint = fp
		return res
	}
}

func (m *model) wifiStatusCmd() tea.Cmd {
	inst := m.inst
	ctx := m.ctx
	logf := m.logFunc()
	return func() tea.Msg {
		res := opDoneMsg{kind: opWiFiStatus}
		w, err := inst.WiFiStatus(ctx)
		if err != nil {
			res.err = err
			return res
		}
		res.wifi = w
		if w.Connected() {
			logf(m.cat.logWiFiConnectedFormat, w.SSID, w.IPAddress)
			return res
		}
		if !w.Available {
			logf(m.cat.logWiFiUnavailableFormat, w.Detail)
			return res
		}
		if !w.ScanSupported {
			logf(m.cat.logNoScanManual)
			return res
		}
		nets, err := inst.WiFiScan(ctx)
		if err != nil {
			logf(m.cat.logScanFailedFormat, err)
			return res
		}
		res.networks = nets
		return res
	}
}

func (m *model) wifiConnectCmd(ssid, security, pass string) tea.Cmd {
	inst := m.inst
	ctx := m.ctx
	return func() tea.Msg {
		res := opDoneMsg{kind: opWiFiConnect}
		w, err := inst.WiFiConnect(ctx, ssid, security, pass)
		res.wifi = w
		res.err = err
		return res
	}
}

func (m *model) appsCmd() tea.Cmd {
	inst := m.inst
	ctx := m.ctx
	selected := map[string]bool{}
	for id, on := range m.appSelected {
		selected[id] = on
	}
	return func() tea.Msg {
		res := opDoneMsg{kind: opApps}
		if !inst.Platform.Stock() {
			ids := make([]string, 0, len(selected))
			for id, on := range selected {
				if on {
					ids = append(ids, id)
				}
			}
			check, err := inst.PreflightAlpine(ctx, ids)
			res.space = &check
			if err != nil {
				res.err = err
				return res
			}
			if !check.OK {
				res.err = fmt.Errorf("not enough free space for the selected apps: %s available, %s required",
					install.BytesLabel(check.AvailableBytes), install.BytesLabel(check.RequiredBytes))
				return res
			}
		}
		results, err := inst.InstallApps(ctx, selected)
		res.results = results
		res.err = err
		return res
	}
}

func (m *model) marusiaCmd(token string) tea.Cmd {
	inst := m.inst
	ctx := m.ctx
	kind := opMarusiaProbe
	if token != "" {
		kind = opMarusiaToken
	}
	return func() tea.Msg {
		res := opDoneMsg{kind: kind}
		got, err := inst.Marusia(ctx, token)
		res.marusia = got
		res.err = err
		return res
	}
}

func (m *model) changePasswordCmd() tea.Cmd {
	inst := m.inst
	ctx := m.ctx
	password := m.pendingPass
	return func() tea.Msg {
		res := opDoneMsg{kind: opChangePassword}
		res.err = inst.AuthSetPassword(ctx, password)
		return res
	}
}

func (m *model) handleOpDone(msg opDoneMsg) (tea.Model, tea.Cmd) {
	wasAbort := m.screen == screenAbort
	model, cmd := m.applyOpDone(msg)
	// A step can finish while the user is deciding whether to stop. Keep the
	// result rather than silently yanking them back to the stop dialog.
	if wasAbort && msg.err == nil && m.screen != screenWorking {
		m.status = m.cat.statusFinishedBeforeStop
	}
	return model, cmd
}

func (m *model) applyOpDone(msg opDoneMsg) (tea.Model, tea.Cmd) {
	m.busy = false
	m.opStart = time.Time{}
	m.status = ""
	m.progress = ""
	if msg.inst != nil {
		m.inst = msg.inst
	}
	if msg.err != nil {
		switch msg.kind {
		case opApps:
			// Keep the per-app results visible; a partial install is never hidden.
			m.appResults = msg.results
			m.appError = msg.err.Error()
		case opDeploy:
			if msg.deploy != nil {
				m.deployTail = msg.deploy.Log
			}
		}
		m.showError(msg)
		return m, nil
	}
	switch msg.kind {
	case opPrepare:
		m.det = msg.det
		m.authState = msg.auth
		m.backup = msg.backup
		m.space = msg.space
		m.spaceApps = msg.appsOff
		m.hostname = msg.host
		m.identity = msg.identity
		m.identityRequired = msg.inst != nil && msg.inst.Dev.Kind() == "adb" && !msg.identity.Available()
		m.identityAck = false
		if m.generated == "" {
			p, err := install.GeneratePassword(install.GeneratedPasswordLength)
			if err != nil {
				m.errText = err.Error()
				m.errOp = opNone
				m.screen = screenError
				return m, nil
			}
			m.generated = p
		}
		if m.authState.Configured {
			m.passMode = passKeep
		} else {
			m.passMode = passGenerate
		}
		m.passwordReady = true
		m.passEditing = false
		m.formIx = 0
		m.acknowledged = false
		m.forceInstall = false
		m.appResults = nil
		m.appError = ""
		m.marusia = install.MarusiaResult{}
		m.wifiSkip = false
		m.screen = screenConfirm
	case opDeploy:
		m.deploy = msg.deploy
		m.fingerprint = msg.fingerprint
		m.passwordAcked = false
		m.copied = false
		m.screen = screenShowPassword
	case opWiFiStatus:
		m.wifi = msg.wifi
		m.networks = msg.networks
		m.wifiIx = 0
		if msg.wifi.Connected() {
			m.screen = screenApps
			return m, nil
		}
		m.screen = screenWiFi
	case opWiFiConnect:
		m.wifi = msg.wifi
		m.screen = screenApps
	case opApps:
		m.appResults = msg.results
		m.appError = ""
		if m.space == nil && msg.space != nil {
			m.space = msg.space
		}
		if m.appSelected["org.capsulad.marusia"] {
			return m, m.startOp(opMarusiaProbe)
		}
		m.screen = screenSummary
	case opMarusiaProbe:
		m.marusia = msg.marusia
		if msg.marusia.Skipped {
			m.screen = screenMarusia
			m.focusField()
			return m, nil
		}
		m.screen = screenSummary
	case opMarusiaToken:
		m.marusia = msg.marusia
		m.screen = screenSummary
	case opChangePassword:
		m.generated = m.pendingPass
		m.pendingPass = ""
		m.passMode = passGenerate
		m.customPass.SetValue("")
		m.passwordAcked = false
		m.copied = false
		m.screen = screenShowPassword
	}
	return m, nil
}

func (m *model) showError(msg opDoneMsg) {
	m.errText = msg.err.Error()
	m.errOp = msg.kind
	m.errIx = 0
	m.status = ""
	m.progress = ""
	m.errCanBack = false
	m.errCanSkip = false
	m.errCanAddr = false
	switch msg.kind {
	case opPrepare:
		m.backScreen = screenAddress
		m.errCanBack = true
	case opDeploy:
		m.backScreen = screenReview
		m.errCanBack = true
		m.errCanAddr = true
	case opWiFiStatus, opWiFiConnect:
		m.backScreen = screenWiFi
		m.errCanSkip = true
		m.errCanAddr = true
	case opApps:
		m.backScreen = screenApps
		m.errCanBack = true
		m.errCanSkip = true
	case opMarusiaProbe, opMarusiaToken:
		m.backScreen = screenMarusia
		m.errCanSkip = true
	case opChangePassword:
		m.backScreen = screenChangePassword
		m.errCanBack = true
	}
	m.screen = screenError
}

func releaseBase(opts Options) string {
	if opts.ReleaseURL != "" {
		return release.NormalizeBaseURL(opts.ReleaseURL)
	}
	return release.DefaultBaseURL(opts.ReleaseTag)
}

func appsBase(opts Options) string {
	if opts.AppsURL != "" {
		return release.NormalizeBaseURL(opts.AppsURL)
	}
	return release.DefaultAppsBaseURL(opts.AppsTag)
}
