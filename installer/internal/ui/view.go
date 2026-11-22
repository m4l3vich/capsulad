package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"capsulad/installer/internal/device"
	"capsulad/installer/internal/install"
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	subtitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	itemStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
	labelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	valueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	boxStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1)
	logStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	footerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

func (m *model) View() string {
	width := m.width
	if width <= 0 {
		width = 92
	}
	body := ""
	switch m.screen {
	case screenDevice:
		body = m.viewDevice()
	case screenFirmware:
		body = m.viewFirmware()
	case screenAddress:
		body = m.viewAddress()
	case screenConfirm:
		body = m.viewConfirm()
	case screenWorking:
		body = m.viewWorking()
	case screenShowPassword:
		body = m.viewShowPassword()
	case screenChangePassword:
		body = m.viewChangePassword()
	case screenWiFi:
		body = m.viewWiFi()
	case screenWiFiPass:
		body = m.viewWiFiPass()
	case screenWiFiManual:
		body = m.viewWiFiManual()
	case screenApps:
		body = m.viewApps()
	case screenMarusia:
		body = m.viewMarusia()
	case screenSummary:
		body = m.viewSummary()
	case screenError:
		body = m.viewError()
	case screenHostKey:
		body = m.viewHostKey()
	case screenAbort:
		body = m.viewAbort()
	}
	if m.status != "" {
		body += "\n\n" + warnStyle.Render(cleanLine(m.status))
	}
	header := lipgloss.JoinHorizontal(lipgloss.Center,
		titleStyle.Render("capsulad installer"),
		subtitleStyle.Render("  "+m.footerVersion()),
	)
	return "\n" + header + "\n" + subtitleStyle.Render(strings.Repeat("─", min(width, 88))) + "\n\n" + body + "\n"
}

func (m *model) footerVersion() string {
	if m.opts.Version != "" {
		return "installer " + m.opts.Version
	}
	return ""
}

func (m *model) contentWidth() int {
	if m.width <= 0 {
		return 88
	}
	return max(40, min(m.width-4, 88))
}

func (m *model) frame(body, footer string) string {
	out := body
	if footer != "" {
		out += "\n\n" + footerStyle.Render(footer)
	}
	return lipgloss.NewStyle().Width(m.contentWidth()).Render(out)
}

func (m *model) viewDevice() string {
	lines := []string{
		titleStyle.Render("Select your device"),
		"",
		radio(m.deviceIx == 0, "Capsula", "not supported"),
		subtitleStyle.Render("    No maintained firmware image; this installer cannot prepare it."),
		"",
		radio(m.deviceIx == 1, "Capsula Mini", "supported"),
		subtitleStyle.Render("    RK3308 rockchip-stock or Amlogic A113X (stock or Alpine)."),
	}
	return m.frame(strings.Join(lines, "\n"), "up/down select · enter continue · ctrl+c quit")
}

func (m *model) viewFirmware() string {
	lines := []string{
		titleStyle.Render("Select firmware"),
		"",
		radio(m.firmwareIx == 0, "Jailbroken stock (capsula-jb)", ""),
		subtitleStyle.Render("    ADB on port 5555 · Rockchip RK3308 or Amlogic A113X"),
		"",
		radio(m.firmwareIx == 1, "Alpine Linux", ""),
		subtitleStyle.Render("    SSH as root · Amlogic A113X"),
	}
	return m.frame(strings.Join(lines, "\n"), "up/down select · enter continue · esc back")
}

func (m *model) viewAddress() string {
	lines := []string{
		titleStyle.Render("Device address"),
		"",
		labelStyle.Render("Address"),
		m.field("> ", 0, &m.addrInput),
		"",
	}
	if m.firmware == device.FirmwareStock {
		lines = append(lines,
			subtitleStyle.Render("ADB connects to <address>:5555. Accept the on-device debugging prompt when it appears."),
		)
	} else {
		lines = append(lines,
			labelStyle.Render("SSH user"),
			m.field("> ", 1, &m.sshUserInput),
			"",
			labelStyle.Render("SSH password"),
			m.field("> ", 2, &m.sshPassInput),
			"",
			labelStyle.Render("Private key (optional)"),
			m.field("> ", 3, &m.sshKeyInput),
			"",
			subtitleStyle.Render("The host key is pinned on first use and must match on every later connection."),
		)
	}
	return m.frame(strings.Join(lines, "\n"), "tab next field · enter connect · esc back")
}

func (m *model) field(marker string, ix int, in interface{ View() string }) string {
	prefix := "  "
	if ix == m.fieldIx {
		prefix = selectedStyle.Render("> ")
	}
	return prefix + in.View()
}

func (m *model) viewConfirm() string {
	det := m.det
	var b strings.Builder
	b.WriteString(titleStyle.Render("Ready to install") + "\n\n")
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Device: "), valueStyle.Render(det.Platform.Label()))
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Detected: "), valueStyle.Render(string(det.Platform)+" · "+cleanLine(det.Arch)))
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Identity: "), valueStyle.Render(cleanLine(m.identity.String())))
	if m.identityRequired {
		b.WriteString(warnStyle.Render("  No stable serial: ADB cannot authenticate this device across a reconnect.") + "\n")
	}
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Data directory: "), valueStyle.Render(det.Platform.DataDir()))
	if m.backup != nil {
		fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Backup: "), valueStyle.Render(cleanLine(m.backup.Path)))
		fmt.Fprintf(&b, "%s %s · %d entries · verified\n", labelStyle.Render("          "), install.BytesLabel(m.backup.Bytes), m.backup.Entries)
	} else {
		b.WriteString(labelStyle.Render("Backup: ") + valueStyle.Render("not needed on Alpine") + "\n")
	}
	b.WriteString("\n")
	b.WriteString(m.spaceLines())
	b.WriteString("\n")
	b.WriteString(titleStyle.Render("Administrator password") + "\n")
	if !m.authState.Known {
		b.WriteString(warnStyle.Render("  Existing password state could not be read; a new password will be created.") + "\n")
	}
	keepAvailable := m.authState.Configured
	b.WriteString(radio(m.passMode == passGenerate, "Generate a strong password", "default"))
	b.WriteString("\n")
	b.WriteString(radio(m.passMode == passCustom, "Set my own", ""))
	b.WriteString("\n")
	if keepAvailable {
		b.WriteString(radio(m.passMode == passKeep, "Keep the existing password", ""))
		b.WriteString("\n")
	}
	if m.passMode == passCustom {
		fmt.Fprintf(&b, "  %s\n", m.customPass.View())
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s Play the one-time installation jingle  [%s]\n", titleStyle.Render("Jingle:"), onOff(m.jingle))
	if m.identityRequired {
		fmt.Fprintf(&b, "%s %s\n", checkbox(m.identityAck), "I understand ADB cannot authenticate this device; a LAN impostor at the same address cannot be detected (x).")
	}
	fmt.Fprintf(&b, "%s %s\n", checkbox(m.acknowledged), "I understand vendor software and files are preserved and nothing is deleted.")
	if m.space != nil && !m.space.OK {
		fmt.Fprintf(&b, "%s %s\n", checkbox(m.forceInstall), "Free space is below the deployment floor; install anyway (f).")
	}
	b.WriteString("\n")
	b.WriteString(selectedStyle.Render("> Install Capsula OS") + "\n")
	b.WriteString(subtitleStyle.Render("Deploy is detached: ADB/SSH will drop while the daemon stops, then reconnect."))
	footer := "1 generate · 2 set · 3 keep · j jingle · a acknowledge · i install · b back"
	if m.identityRequired {
		footer = "x identity · " + footer
	}
	return m.frame(b.String(), footer)
}

func (m *model) spaceLines() string {
	if m.space == nil {
		return ""
	}
	s := m.space
	var b strings.Builder
	tone := okStyle
	label := "Space:"
	if !s.OK {
		tone = warnStyle
	}
	fmt.Fprintf(&b, "%s %s available, %s required\n", labelStyle.Render(label),
		tone.Render(install.BytesLabel(s.AvailableBytes)), tone.Render(install.BytesLabel(s.RequiredBytes)))
	if s.StagedDaemon > 0 {
		fmt.Fprintf(&b, "       %s 8 MiB reserve + %s daemon growth (staged %s, installed %s)\n",
			subtitleStyle.Render("="), subtitleStyle.Render(install.BytesLabel(s.RequiredBytes-s.ReserveBytes)),
			install.BytesLabel(s.StagedDaemon), install.BytesLabel(s.InstalledDaemon))
	} else {
		fmt.Fprintf(&b, "       %s bundle %s + apps %s + reserve %s\n", subtitleStyle.Render("="),
			install.BytesLabel(s.BundleBytes), install.BytesLabel(s.AppBytes), install.BytesLabel(s.ReserveBytes))
	}
	if !s.OK {
		b.WriteString(warnStyle.Render("       Short by "+install.BytesLabel(s.RequiredBytes-s.AvailableBytes)+". Nothing will be deleted.") + "\n")
		if len(m.spaceApps) > 0 {
			var idle []string
			for _, a := range m.spaceApps {
				if !a.Running {
					idle = append(idle, cleanLine(a.ID))
				}
			}
			if len(idle) > 0 {
				b.WriteString(subtitleStyle.Render("       Installed apps not running (they may be relocated manually): ") + strings.Join(idle, ", ") + "\n")
			}
		}
		b.WriteString(subtitleStyle.Render("       The deployer will refuse rather than free space. Abort with b, or force with f.") + "\n")
	}
	return b.String()
}

func (m *model) viewWorking() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", m.spinner.View(), titleStyle.Render(m.status))
	if m.progress != "" {
		b.WriteString("  " + subtitleStyle.Render(cleanLine(m.progress)) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(boxStyle.Render(m.viewport.View()))
	return m.frame(b.String(), "pgup/pgdn scroll · ctrl+c abort (asks first)")
}

func (m *model) viewShowPassword() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Administrator password") + "\n\n")
	pw := m.chosenPassword()
	if pw == "" {
		b.WriteString(valueStyle.Render("Keeping the existing administrator password.") + "\n")
		b.WriteString(subtitleStyle.Render("Recover it with `capsulad-cli auth set-password` in the device shell.") + "\n")
	} else {
		b.WriteString("  " + selectedStyle.Render(escapeControls(pw)) + "\n\n")
		b.WriteString(subtitleStyle.Render("This is the only time it is shown. Store it in a password manager.") + "\n")
		b.WriteString(subtitleStyle.Render("Recovery: `capsulad-cli auth set-password` in the device shell.") + "\n")
		if m.copied {
			b.WriteString(okStyle.Render("Copy requested (OSC 52); paste it into your password manager.") + "\n")
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s I have stored the password\n", checkbox(m.passwordAcked))
	return m.frame(b.String(), "c copy · n change · space confirm stored · enter continue")
}

func (m *model) viewChangePassword() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Set a new administrator password") + "\n\n")
	b.WriteString(subtitleStyle.Render("It is sent to the device on stdin and never enters argv or the log.") + "\n\n")
	b.WriteString("  " + m.customPass.View() + "\n")
	return m.frame(b.String(), "enter set password · esc back")
}

func (m *model) viewWiFi() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Wi-Fi") + "\n\n")
	fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("State:"), valueStyle.Render(cleanLine(m.wifi.State)))
	if m.wifi.APActive {
		fmt.Fprintf(&b, "%s %s · portal %s\n", labelStyle.Render("Setup AP:"),
			valueStyle.Render(cleanLine(m.wifi.APSSID)), valueStyle.Render(cleanLine(m.wifi.APPortalURL)))
		if m.wifi.APPasswordHint != "" {
			fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Password hint:"), valueStyle.Render(cleanLine(m.wifi.APPasswordHint)))
		}
	}
	b.WriteString("\n")
	if len(m.networks) == 0 {
		b.WriteString(subtitleStyle.Render("No scan results. Enter the network name manually with m.") + "\n")
	}
	for i, n := range m.networks {
		label := fmt.Sprintf("%s  %s  %d dBm", cleanLine(n.SSID), cleanLine(securityLabel(n.Security)), n.SignalDBM)
		b.WriteString(item(i == m.wifiIx, label) + "\n")
	}
	b.WriteString(item(m.wifiIx == len(m.networks), "Enter SSID manually") + "\n")
	return m.frame(b.String(), "up/down · enter choose · m manual · r rescan · s skip · b back")
}

func (m *model) viewWiFiPass() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Join "+cleanLine(m.selectedSSID())) + "\n\n")
	b.WriteString(labelStyle.Render("Passphrase") + "\n")
	b.WriteString("  " + m.wifiPassInput.View() + "\n")
	b.WriteString("\n")
	b.WriteString(subtitleStyle.Render("The passphrase goes to the device over stdin; it is never put in argv or logs."))
	return m.frame(b.String(), "enter connect · esc back")
}

func (m *model) viewWiFiManual() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Enter a Wi-Fi network") + "\n\n")
	b.WriteString(labelStyle.Render("SSID") + "\n")
	b.WriteString("  " + m.wifiSSIDInput.View() + "\n\n")
	fmt.Fprintf(&b, "%s Open network (no passphrase)  [%s]\n", checkbox(m.wifiOpen), onOff(m.wifiOpen))
	return m.frame(b.String(), "enter continue · ctrl+o open toggle · esc back")
}

func (m *model) viewApps() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Choose first-party apps") + "\n\n")
	for i, a := range install.Catalogue {
		mark := "[ ]"
		if m.appSelected[a.ID] {
			mark = "[x]"
		}
		line := fmt.Sprintf("%s %-18s %s", mark, a.Name, subtitleStyle.Render(a.Description))
		if i == m.appIx {
			b.WriteString(selectedStyle.Render("> "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s %d selected", labelStyle.Render("Selected:"), countSelected(m.appSelected))
	if m.inst != nil && m.inst.Provider != nil {
		fmt.Fprintf(&b, " · release %s · platform %s", cleanLine(m.inst.Provider.Version()), m.inst.Platform)
	}
	b.WriteString("\n")
	return m.frame(b.String(), "up/down · space toggle · a all · n none · enter install · s skip · b back")
}

func (m *model) viewMarusia() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Marusia needs a VK access token") + "\n\n")
	b.WriteString(subtitleStyle.Render("No stock voice-agent session was found, so nothing could be migrated.") + "\n")
	b.WriteString(subtitleStyle.Render("Paste a token (hidden) or skip; you can set it later in the Web UI.") + "\n\n")
	b.WriteString("  " + m.marusiaToken.View() + "\n")
	return m.frame(b.String(), "enter save · esc skip")
}

func (m *model) viewSummary() string {
	var b strings.Builder
	if m.appError != "" {
		b.WriteString(warnStyle.Render("Installation finished with errors") + "\n\n")
		b.WriteString(errStyle.Render("Apps: "+m.appError) + "\n\n")
	} else {
		b.WriteString(okStyle.Render("Installation complete") + "\n\n")
	}
	if m.inst != nil {
		base := cleanLine(install.HTTPSBase(m.addrInput.Value(), 8443))
		fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Web UI: "), valueStyle.Render(base))
		if m.fingerprint != "" {
			fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Certificate SHA-256: "), valueStyle.Render(m.fingerprint))
		}
		fmt.Fprintf(&b, "%s %s/api/cert.pem\n", labelStyle.Render("Download: "), base)
		fmt.Fprintf(&b, "%s capsulad-cli cert status\n", labelStyle.Render("Verify:   "))
		b.WriteString(subtitleStyle.Render("Trust it in your browser or OS keychain after comparing the fingerprint above; the Web UI is self-signed.") + "\n")
	}
	if m.chosenPassword() != "" {
		b.WriteString(labelStyle.Render("Admin password: ") + valueStyle.Render("set (shown after deployment)") + "\n")
	} else {
		b.WriteString(labelStyle.Render("Admin password: ") + valueStyle.Render("existing password kept") + "\n")
	}
	if m.wifiSkip {
		b.WriteString(labelStyle.Render("Wi-Fi: ") + warnStyle.Render("skipped") + "\n")
	} else if m.wifi.Connected() {
		fmt.Fprintf(&b, "%s %s (%s)\n", labelStyle.Render("Wi-Fi: "), valueStyle.Render(cleanLine(m.wifi.SSID)), valueStyle.Render(cleanLine(m.wifi.IPAddress)))
	}
	if len(m.appResults) > 0 {
		b.WriteString("\n" + titleStyle.Render("Apps") + "\n")
		for _, r := range m.appResults {
			line := cleanLine(r.Name)
			if r.Status == "installed" {
				b.WriteString("  " + okStyle.Render("✓ ") + line + subtitleStyle.Render(" "+cleanLine(r.Version)) + "\n")
			} else {
				b.WriteString("  " + errStyle.Render("✗ ") + line + " — " + cleanLine(r.Detail) + "\n")
			}
		}
	}
	if m.marusia.Migrated && marusiaInstalled(m.appResults) {
		b.WriteString(labelStyle.Render("Marusia: ") + valueStyle.Render("existing stock session migrated; vendor file left in place") + "\n")
	} else if m.marusia.TokenSet && marusiaInstalled(m.appResults) {
		b.WriteString(labelStyle.Render("Marusia: ") + valueStyle.Render("VK access token stored") + "\n")
	} else if m.appSelected["org.capsulad.marusia"] && marusiaInstalled(m.appResults) {
		b.WriteString(labelStyle.Render("Marusia: ") + warnStyle.Render("installed without a token") + "\n")
	}
	if m.backup != nil {
		fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Backup: "), valueStyle.Render(cleanLine(m.backup.Path)))
	}
	if m.inst != nil {
		fmt.Fprintf(&b, "%s %s\n", labelStyle.Render("Rollback: "), valueStyle.Render(m.inst.Platform.Rollback()))
	}
	b.WriteString("\n" + valueStyle.Render("Nothing was deleted. Vendor software and files are preserved; the boot hook retires the stock assistant chain and can be disabled.") + "\n")
	return m.frame(b.String(), "enter/q quit")
}

func (m *model) viewError() string {
	var b strings.Builder
	b.WriteString(errStyle.Render("Installation stopped") + "\n\n")
	b.WriteString(valueStyle.Render(cleanLine(m.errText)) + "\n")
	b.WriteString("\n")
	if m.errOp == opDeploy {
		if m.deployTail != "" {
			b.WriteString(titleStyle.Render("Deploy log (verbatim, last lines)") + "\n")
			b.WriteString(logStyle.Render(cleanText(device.LogTail(m.deployTail, 40))))
			b.WriteString("\n\n")
		}
		b.WriteString(subtitleStyle.Render("The device may be part-way through. Reconnect and inspect /tmp/capsulad-installer/deploy.log before retrying.") + "\n")
		b.WriteString(subtitleStyle.Render(cleanText(device.RecoveryAdvice(cleanLine(m.addrInput.Value()), cleanLine(m.wifi.APSSID), cleanLine(m.wifi.APPasswordHint)))) + "\n")
	} else if m.errOp == opWiFiStatus || m.errOp == opWiFiConnect {
		b.WriteString(subtitleStyle.Render(cleanText(device.RecoveryAdvice(cleanLine(m.addrInput.Value()), cleanLine(m.wifi.APSSID), cleanLine(m.wifi.APPasswordHint)))) + "\n")
	} else if m.errOp == opApps {
		b.WriteString(subtitleStyle.Render("Some selected apps may already be installed; the summary lists each result. Retrying reinstalls the selection.") + "\n")
	} else {
		b.WriteString(subtitleStyle.Render("No deployment was started by this step; the device is unchanged.") + "\n")
	}
	footer := "r retry · q quit"
	if m.errCanAddr {
		footer = "a change address · " + footer
	}
	if m.errCanBack {
		footer = "b back · " + footer
	}
	if m.errCanSkip {
		footer = "s skip to summary · " + footer
	}
	return m.frame(b.String(), footer)
}

func (m *model) viewHostKey() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("New SSH host key") + "\n\n")
	b.WriteString(labelStyle.Render("Host: ") + valueStyle.Render(cleanLine(m.hostKey.host)) + "\n")
	b.WriteString(labelStyle.Render("SHA-256: ") + selectedStyle.Render(m.hostKey.fingerprint) + "\n\n")
	b.WriteString(subtitleStyle.Render("Trust-on-first-use pins this key under your config directory. A later mismatch is a hard failure.") + "\n")
	return m.frame(b.String(), "y trust and continue · n refuse")
}

func (m *model) viewAbort() string {
	var b strings.Builder
	b.WriteString(errStyle.Render("Stop the installation?") + "\n\n")
	if m.op == opDeploy {
		b.WriteString("The deployment runs detached on the device, because the daemon owns Wi-Fi.\n")
		b.WriteString("Leaving now stops this interface, not that script; the device may finish or fail on its own.\n\n")
		b.WriteString(subtitleStyle.Render("State can be checked after reconnecting: cat /tmp/capsulad-installer/deploy.log") + "\n")
	} else {
		b.WriteString("This cancels the current step before any deployment starts.\n")
		b.WriteString("The device is left as it was; the last completed step is listed in the log.\n")
	}
	return m.frame(b.String(), "y stop and quit · n continue waiting")
}

func radio(active bool, label, suffix string) string {
	marker := "○"
	if active {
		marker = "●"
	}
	line := marker + " " + label
	if suffix != "" {
		line += "  " + subtitleStyle.Render("("+suffix+")")
	}
	if active {
		return selectedStyle.Render(line)
	}
	return itemStyle.Render(line)
}

func item(active bool, label string) string {
	if active {
		return selectedStyle.Render("> " + label)
	}
	return "  " + itemStyle.Render(label)
}

func checkbox(on bool) string {
	if on {
		return okStyle.Render("[x]")
	}
	return "[ ]"
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func countSelected(sel map[string]bool) int {
	n := 0
	for _, on := range sel {
		if on {
			n++
		}
	}
	return n
}

func securityLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return "open"
	}
	return strings.ToUpper(s)
}

func marusiaInstalled(results []install.AppResult) bool {
	for _, r := range results {
		if r.ID == "org.capsulad.marusia" && r.Status == "installed" {
			return true
		}
	}
	return false
}
