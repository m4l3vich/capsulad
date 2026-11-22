package ui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	switch m.screen {
	case screenHostKey:
		switch s {
		case "y", "Y", "enter":
			m.hostKey.reply <- true
			m.hostKey = nil
			m.screen = m.prevScreen
			m.status = "host key pinned"
		case "n", "N", "esc":
			m.hostKey.reply <- false
			m.hostKey = nil
			m.screen = m.prevScreen
			m.status = "host key refused"
		}
		return true, nil

	case screenAbort:
		switch s {
		case "y", "Y", "enter":
			m.aborted = true
			m.cancel()
			return true, tea.Quit
		case "n", "N", "esc":
			m.screen = m.prevScreen
			return true, nil
		}
		return true, nil

	case screenDevice:
		switch s {
		case "up", "k":
			if m.deviceIx > 0 {
				m.deviceIx--
			}
			m.status = ""
		case "down", "j":
			if m.deviceIx < 1 {
				m.deviceIx++
			}
			m.status = ""
		case "enter":
			if m.deviceIx == 0 {
				m.status = "The original Capsula is not supported by this installer: no maintained firmware image exists."
				return true, nil
			}
			m.screen = screenFirmware
		case "esc":
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
				m.status = "Enter the device address first."
				return true, nil
			}
			if m.firmware == device.FirmwareAlpine && m.sshPassInput.Value() == "" && strings.TrimSpace(m.sshKeyInput.Value()) == "" {
				m.status = "Alpine needs an SSH password or a private key."
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
				if err := install.ValidatePassword(m.customPass.Value()); err != nil {
					m.status = err.Error()
					return true, nil
				}
				m.passwordReady = true
				m.passEditing = false
				m.customPass.Blur()
				m.status = "Password captured; it will be sent over stdin."
				return true, nil
			case "esc":
				m.passEditing = false
				m.customPass.Blur()
				m.passMode = passGenerate
				m.passwordReady = false
				return true, nil
			}
			return false, nil
		}
		switch s {
		case "1":
			m.passMode = passGenerate
			m.passwordReady = true
			m.passEditing = false
		case "2":
			m.passMode = passCustom
			m.passwordReady = false
			m.passEditing = true
			m.customPass.Focus()
			m.status = "Type the password, then press Enter."
		case "3":
			if m.authState.Configured {
				m.passMode = passKeep
				m.passwordReady = true
				m.passEditing = false
			}
		case "j":
			m.jingle = !m.jingle
		case "a":
			m.acknowledged = !m.acknowledged
		case "x":
			if m.identityRequired {
				m.identityAck = !m.identityAck
			}
		case "f":
			if m.space != nil && !m.space.OK {
				m.forceInstall = !m.forceInstall
			}
		case "i", "enter":
			if !m.acknowledged {
				m.status = "Tick the acknowledgement (a) before installing."
				return true, nil
			}
			if m.identityRequired && !m.identityAck {
				m.status = "This device has no stable serial; ADB cannot authenticate it. Press x to acknowledge, or abort."
				return true, nil
			}
			if m.space != nil && !m.space.OK && !m.forceInstall {
				m.status = "Free space is below the deployment floor; press f to install anyway or b to abort."
				return true, nil
			}
			if m.passMode == passCustom && !m.passwordReady {
				m.status = "Press 2 to edit the password, type it, then press Enter."
				return true, nil
			}
			m.status = ""
			return true, m.startOp(opDeploy)
		case "b":
			m.screen = screenAddress
			m.status = ""
		case "esc":
			m.screen = screenAddress
		}
		return true, nil

	case screenWorking:
		if s == "pgup" || s == "pgdown" || s == "up" || s == "down" {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(k)
			return true, cmd
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
				m.status = "Confirm you have stored the password (space), or press c to copy it."
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
				m.status = err.Error()
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
			m.logFunc()("Wi-Fi skipped by the user")
		case "b":
			m.screen = screenShowPassword
		}
		return true, nil

	case screenWiFiPass:
		switch s {
		case "enter":
			ssid := m.selectedSSID()
			if ssid == "" {
				m.status = "No network selected."
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
				m.status = "Enter a network name."
				return true, nil
			}
			if err := install.ValidateSSID(ssid); err != nil {
				m.status = err.Error()
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
		case "enter", "q":
			return true, tea.Quit
		}
		return true, nil

	case screenError:
		switch s {
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
				m.screen = screenAddress
				m.status = "Re-enter the address the device has now; then continue."
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
	return false, nil
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
	m.screen = screenWorking
	m.status = opLabel(kind)
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
	m.screen = screenWorking
	m.status = "Connecting to Wi-Fi"
	return m.wifiConnectCmd(ssid, security, pass)
}

func opLabel(kind opKind) string {
	switch kind {
	case opPrepare:
		return "Connecting to the device and preparing the installation"
	case opDeploy:
		return "Deploying capsulad (detached; the link may drop)"
	case opWiFiStatus:
		return "Reading Wi-Fi state"
	case opWiFiConnect:
		return "Joining the Wi-Fi network"
	case opApps:
		return "Installing first-party apps"
	case opMarusiaProbe, opMarusiaToken:
		return "Configuring Marusia"
	case opChangePassword:
		return "Updating the administrator password"
	}
	return "Working"
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
			p, manifest, err := release.OfflineProvider(opts.ArtifactsDir)
			if err != nil {
				res.err = err
				return res
			}
			logf("offline release %s from %s", manifest.Version, opts.ArtifactsDir)
			provider = p
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
			provider = release.NewRemoteProvider(f, manifest)
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
			logf("On the device, accept the \"Allow USB debugging?\" prompt for this computer.")
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
			logf("warning: this device has no stable serial; ADB cannot authenticate it across a reconnect")
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
		logf("keeping the existing administrator password; verifying it before launch")
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
			logf("Wi-Fi is already connected: %s (%s)", w.SSID, w.IPAddress)
			return res
		}
		if !w.Available {
			logf("Wi-Fi is not available: %s", w.Detail)
			return res
		}
		if !w.ScanSupported {
			logf("this firmware cannot scan; enter the network name manually")
			return res
		}
		nets, err := inst.WiFiScan(ctx)
		if err != nil {
			logf("scan failed: %v", err)
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
	m.busy = false
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
		m.backScreen = screenConfirm
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
