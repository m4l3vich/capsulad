package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
	stepStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	ruleStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

const (
	minBodyLines = 3
	labelWidth   = 12
)

// View renders the persistent frame: title and step, a body sized to the
// terminal, then the status line and key hints. The footer never scrolls away,
// so the action for every screen stays discoverable.
func (m *model) View() string {
	width := m.width
	if width <= 0 {
		width = 92
	}
	height := m.height
	if height <= 0 {
		height = 30
	}
	cw := m.contentWidth()

	status := strings.TrimSpace(m.status)
	if m.screen == screenWorking || m.screen == screenAbort {
		// Those screens already render the current step in their body.
		status = ""
	}
	statusLines := 0
	if status != "" {
		statusLines = 1
	}
	// One leading blank, header, rule, gap, body, gap, status, footer.
	avail := height - 6 - statusLines
	if avail < minBodyLines {
		avail = minBodyLines
	}
	body := m.bodyView(avail, cw)
	overflow := len(splitLines(body)) > avail

	lines := make([]string, 0, height)
	lines = append(lines,
		"",
		m.headerLine(cw),
		ruleStyle.Render(strings.Repeat("─", cw)),
		"",
	)
	lines = append(lines, splitLines(m.scrolledBody(body, avail, cw))...)
	lines = append(lines, "")
	if status != "" {
		lines = append(lines, warnStyle.Render("! "+ansi.Truncate(cleanLine(status), max(1, cw-2), "…")))
	}
	lines = append(lines, m.hint(overflow, cw))
	return strings.Join(lines, "\n")
}

func (m *model) headerLine(width int) string {
	left := titleStyle.Render("capsulad installer")
	if v := m.footerVersion(); v != "" {
		left += subtitleStyle.Render("  " + v)
	}
	right := stepStyle.Render(m.stepLabel())
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) stepLabel() string {
	switch m.screen {
	case screenDevice:
		return "Step 1 · Device"
	case screenFirmware:
		return "Step 2 · Firmware"
	case screenAddress:
		return "Step 3 · Address"
	case screenConfirm:
		return "Step 4 · Review"
	case screenWorking:
		return "Working"
	case screenShowPassword, screenChangePassword:
		return "Step 5 · Admin password"
	case screenWiFi, screenWiFiPass, screenWiFiManual:
		return "Step 6 · Wi-Fi"
	case screenApps:
		return "Step 7 · Apps"
	case screenMarusia:
		return "Step 8 · Marusia"
	case screenSummary:
		return "Complete"
	case screenError:
		return "Stopped"
	case screenHostKey:
		return "SSH host key"
	case screenAbort:
		return "Confirm stop"
	}
	return ""
}

func (m *model) footerVersion() string {
	return m.opts.Version
}

func (m *model) contentWidth() int {
	if m.width <= 0 {
		return 88
	}
	return max(20, min(m.width-2, 88))
}

// bodyView dispatches to the current screen. Screens that embed a scrolling
// pane (the operation log) receive the available height directly.
func (m *model) bodyView(avail, width int) string {
	switch m.screen {
	case screenDevice:
		return m.viewDevice()
	case screenFirmware:
		return m.viewFirmware()
	case screenAddress:
		return m.viewAddress()
	case screenConfirm:
		return m.viewConfirm(width)
	case screenWorking:
		return m.viewWorking(avail, width)
	case screenShowPassword:
		return m.viewShowPassword()
	case screenChangePassword:
		return m.viewChangePassword()
	case screenWiFi:
		return m.viewWiFi()
	case screenWiFiPass:
		return m.viewWiFiPass()
	case screenWiFiManual:
		return m.viewWiFiManual()
	case screenApps:
		return m.viewApps()
	case screenMarusia:
		return m.viewMarusia()
	case screenSummary:
		return m.viewSummary(width)
	case screenError:
		return m.viewError(width)
	case screenHostKey:
		return m.viewHostKey()
	case screenAbort:
		return m.viewAbort()
	}
	return ""
}

func (m *model) scrolledBody(body string, avail, width int) string {
	lines := splitLines(body)
	maxScroll := len(lines) - avail
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.screen == screenWorking {
		m.scroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
	out := make([]string, 0, avail)
	end := min(len(lines), m.scroll+avail)
	for _, ln := range lines[m.scroll:end] {
		out = append(out, ansi.Truncate(ln, width, "…"))
	}
	for len(out) < avail {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

func (m *model) scrollBy(delta int) {
	m.scroll += delta
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m *model) pageStep() int {
	h := m.height
	if h <= 0 {
		h = 30
	}
	return max(3, (h-6)/2)
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func (m *model) hint(overflow bool, width int) string {
	var parts []string
	switch m.screen {
	case screenDevice:
		parts = []string{"↑/↓ select", "enter continue", "ctrl+c quit"}
	case screenFirmware:
		parts = []string{"↑/↓ select", "enter continue", "esc back", "ctrl+c quit"}
	case screenAddress:
		if m.firmware == device.FirmwareAlpine {
			parts = []string{"tab/shift+tab fields", "enter connect", "esc back"}
		} else {
			parts = []string{"enter connect", "esc back"}
		}
	case screenConfirm:
		parts = []string{"i/enter install", "1/2/3 password", "j jingle", "a ack", "b back"}
		if m.identityRequired {
			parts = append(parts, "x identity")
		}
	case screenWorking:
		parts = []string{"pgup/pgdn log", "esc stop", "ctrl+c stop"}
	case screenShowPassword:
		parts = []string{"enter continue", "space confirm stored", "c copy", "n change"}
	case screenChangePassword:
		parts = []string{"enter set password", "esc back"}
	case screenWiFi:
		parts = []string{"enter join", "↑/↓ select", "m manual", "r rescan", "s skip", "esc back"}
	case screenWiFiPass:
		parts = []string{"enter join", "esc back"}
	case screenWiFiManual:
		parts = []string{"enter continue", "ctrl+o open network", "esc back"}
	case screenApps:
		parts = []string{"enter install", "space toggle", "a all", "n none", "s skip", "esc back"}
	case screenMarusia:
		parts = []string{"enter save token", "esc skip"}
	case screenSummary:
		parts = []string{"enter/q quit"}
	case screenError:
		parts = []string{"↑/↓ choose", "enter run", "q quit"}
	case screenHostKey:
		parts = []string{"y trust", "n refuse"}
	case screenAbort:
		parts = []string{"↑/↓ choose", "enter confirm", "ctrl+c stop now"}
	}
	if overflow && m.screen != screenWorking {
		parts = append(parts, "↕ pgup/pgdn")
	}
	return footerStyle.Render(ansi.Truncate(strings.Join(parts, " · "), width, "…"))
}

func (m *model) viewDevice() string {
	lines := []string{
		titleStyle.Render("Select your device"),
		"",
		radio(m.deviceIx == 0, "Capsula (original)", "not supported"),
		subtitleStyle.Render("    No maintained firmware image; this installer cannot prepare it."),
		"",
		radio(m.deviceIx == 1, "Capsula Mini", "supported"),
		subtitleStyle.Render("    RK3308 (Rockchip) or A113X (Amlogic), on stock or Alpine."),
		"",
		subtitleStyle.Render("Capsula Mini is the only supported device."),
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewFirmware() string {
	lines := []string{
		titleStyle.Render("How is the device running today?"),
		"",
		radio(m.firmwareIx == 0, "Jailbroken stock (capsula-jb)", ""),
		subtitleStyle.Render("    Connects over ADB on port 5555. Rockchip RK3308 or Amlogic A113X."),
		"",
		radio(m.firmwareIx == 1, "Alpine Linux", ""),
		subtitleStyle.Render("    Connects over SSH as root. Amlogic A113X."),
		"",
		subtitleStyle.Render("The board is discovered after connecting; the firmware decides the transport."),
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewAddress() string {
	lines := []string{
		titleStyle.Render("Device address"),
		"",
		labelStyle.Render("Address"),
		m.field(0, &m.addrInput),
		"",
	}
	if m.firmware == device.FirmwareStock {
		lines = append(lines,
			subtitleStyle.Render("ADB connects to <address>:5555. Accept the on-device debugging prompt when it appears."),
		)
	} else {
		lines = append(lines,
			labelStyle.Render("SSH user"),
			m.field(1, &m.sshUserInput),
			"",
			labelStyle.Render("SSH password"),
			m.field(2, &m.sshPassInput),
			"",
			labelStyle.Render("Private key (optional)"),
			m.field(3, &m.sshKeyInput),
			"",
			subtitleStyle.Render("A password or a private key is required. The host key is pinned on first use."),
		)
	}
	return strings.Join(lines, "\n")
}

func (m *model) field(ix int, in interface{ View() string }) string {
	if ix == m.fieldIx {
		return selectedStyle.Render("▸ ") + in.View()
	}
	return "  " + in.View()
}

func row(label, value string) string {
	return labelStyle.Render(padRight(label, labelWidth)) + value
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// wrapIndent word-wraps a long value under a labelled row, keeping
// continuation lines aligned with the value column.
func wrapIndent(s string, width, indent int) string {
	wrapped := ansi.Wordwrap(s, max(10, width-indent), "/-_")
	return strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", indent))
}

func (m *model) viewConfirm(width int) string {
	det := m.det
	var b strings.Builder
	b.WriteString(titleStyle.Render("Review the installation") + "\n\n")
	b.WriteString(row("Device", valueStyle.Render(det.Platform.Label()+" · "+cleanLine(det.Arch))) + "\n")
	b.WriteString(row("Identity", valueStyle.Render(cleanLine(m.identity.String()))) + "\n")
	if m.identityRequired {
		b.WriteString("            " + warnStyle.Render(wrapIndent("No stable serial: ADB cannot authenticate this device across a reconnect.", width, labelWidth)) + "\n")
	}
	if m.backup != nil {
		b.WriteString(row("Backup", valueStyle.Render(wrapIndent(cleanLine(m.backup.Path), width, labelWidth))) + "\n")
		b.WriteString("            " + subtitleStyle.Render(fmt.Sprintf("%s · %d entries · verified",
			install.BytesLabel(m.backup.Bytes), m.backup.Entries)) + "\n")
	} else {
		b.WriteString(row("Backup", valueStyle.Render("not needed on Alpine")) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(m.spaceLines())
	b.WriteString(titleStyle.Render("Administrator password") + "\n")
	if !m.authState.Known {
		b.WriteString(warnStyle.Render("  Existing password state could not be read; a new password will be created.") + "\n")
	}
	b.WriteString("  " + radio(m.passMode == passGenerate, "1  Generate a strong password", "recommended") + "\n")
	b.WriteString("  " + radio(m.passMode == passCustom, "2  Set my own", "") + "\n")
	if m.authState.Configured {
		b.WriteString("  " + radio(m.passMode == passKeep, "3  Keep the existing password", "") + "\n")
	}
	if m.passMode == passCustom {
		b.WriteString("\n  " + m.customPass.View() + "\n")
		b.WriteString(subtitleStyle.Render("  Type the password, then press enter to capture it (esc cancels).") + "\n")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  %-3s %-36s %s\n", "j", "One-time installation jingle", onOff(m.jingle))
	if m.identityRequired {
		fmt.Fprintf(&b, "  %-3s %-36s %s\n", "x", "ADB cannot authenticate this device", checkbox(m.identityAck))
	}
	fmt.Fprintf(&b, "  %-3s %-36s %s\n", "a", "Nothing is deleted; vendor files stay", checkbox(m.acknowledged))
	if m.space != nil && !m.space.OK {
		fmt.Fprintf(&b, "  %-3s %-36s %s\n", "f", "Install despite low free space", checkbox(m.forceInstall))
	}
	b.WriteString(selectedStyle.Render("▸ Install capsulad") + subtitleStyle.Render("   press i or enter") + "\n")
	b.WriteString(subtitleStyle.Render("Deploy is detached: ADB/SSH drops while the daemon stops, then reconnects."))
	return b.String()
}

func (m *model) spaceLines() string {
	if m.space == nil {
		return ""
	}
	s := m.space
	var b strings.Builder
	tone := okStyle
	if !s.OK {
		tone = warnStyle
	}
	b.WriteString(row("Space", tone.Render(install.BytesLabel(s.AvailableBytes))+" available, "+
		tone.Render(install.BytesLabel(s.RequiredBytes))+" required") + "\n")
	if s.StagedDaemon > 0 && !s.OK {
		b.WriteString("            " + subtitleStyle.Render(fmt.Sprintf(
			"= 8 MiB reserve + %s daemon growth (staged %s, installed %s)",
			install.BytesLabel(s.RequiredBytes-s.ReserveBytes),
			install.BytesLabel(s.StagedDaemon), install.BytesLabel(s.InstalledDaemon))) + "\n")
	}
	if !s.OK {
		b.WriteString(warnStyle.Render("            Short by "+install.BytesLabel(s.RequiredBytes-s.AvailableBytes)+
			". Nothing will be deleted.") + "\n")
		if len(m.spaceApps) > 0 {
			var idle []string
			for _, a := range m.spaceApps {
				if !a.Running {
					idle = append(idle, cleanLine(a.ID))
				}
			}
			if len(idle) > 0 {
				b.WriteString(subtitleStyle.Render("            Installed apps not running (they may be relocated manually): "+
					strings.Join(idle, ", ")) + "\n")
			}
		}
		b.WriteString(subtitleStyle.Render("            The deployer refuses rather than freeing space; press f to force or b to abort.") + "\n")
	}
	return b.String()
}

func (m *model) viewWorking(avail, width int) string {
	var b strings.Builder
	elapsed := ""
	if !m.opStart.IsZero() {
		elapsed = " · " + shortDuration(time.Since(m.opStart))
	}
	fmt.Fprintf(&b, "%s %s%s\n", m.spinner.View(), titleStyle.Render(cleanLine(m.status)), subtitleStyle.Render(elapsed))
	if m.progress != "" {
		b.WriteString("  " + subtitleStyle.Render(cleanLine(m.progress)) + "\n")
	} else {
		b.WriteString("  \n")
	}
	b.WriteString("\n")
	if avail >= 8 {
		m.viewport.Width = max(20, width-4)
		m.viewport.Height = avail - 5
		b.WriteString(boxStyle.Render(m.viewport.View()))
	}
	return b.String()
}

func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func (m *model) viewShowPassword() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Your administrator password") + "\n\n")
	pw := m.chosenPassword()
	if pw == "" {
		b.WriteString(valueStyle.Render("Keeping the existing administrator password.") + "\n\n")
		b.WriteString(subtitleStyle.Render("Recover it with `capsulad-cli auth set-password` in the device shell.") + "\n")
	} else {
		b.WriteString(boxStyle.Copy().
			BorderForeground(lipgloss.Color("42")).Padding(0, 2).MarginLeft(2).
			Render(selectedStyle.Render(escapeControls(pw))) + "\n\n")
		b.WriteString(subtitleStyle.Render("This is the only time it is shown. Store it in a password manager.") + "\n")
		b.WriteString(subtitleStyle.Render("Recovery: `capsulad-cli auth set-password` in the device shell.") + "\n")
		if m.copied {
			b.WriteString(okStyle.Render("Copy requested (OSC 52); paste it into your password manager.") + "\n")
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s I have stored this password   %s\n", checkbox(m.passwordAcked), subtitleStyle.Render("(space)"))
	return b.String()
}

func (m *model) viewChangePassword() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Set a new administrator password") + "\n\n")
	b.WriteString(subtitleStyle.Render("It is sent to the device on stdin and never enters argv or the log.") + "\n\n")
	b.WriteString("  " + m.customPass.View() + "\n")
	return b.String()
}

func (m *model) viewWiFi() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Wi-Fi") + "\n\n")
	fmt.Fprintf(&b, "%s\n", row("State", valueStyle.Render(orUnknown(m.wifi.State))))
	if m.wifi.Connected() {
		fmt.Fprintf(&b, "%s\n", row("Connected", valueStyle.Render(cleanLine(m.wifi.SSID)+" ("+cleanLine(m.wifi.IPAddress)+")")))
	}
	if m.wifi.APActive {
		fmt.Fprintf(&b, "%s\n", row("Setup AP", valueStyle.Render(cleanLine(m.wifi.APSSID))))
		if m.wifi.APPortalURL != "" {
			fmt.Fprintf(&b, "%s\n", row("Portal", valueStyle.Render(cleanLine(m.wifi.APPortalURL))))
		}
		if m.wifi.APPasswordHint != "" {
			fmt.Fprintf(&b, "%s\n", row("AP password", valueStyle.Render(cleanLine(m.wifi.APPasswordHint))))
		}
	}
	b.WriteString("\n")
	if len(m.networks) == 0 {
		b.WriteString(subtitleStyle.Render("No scan results. Press m to enter a network name.") + "\n\n")
	}
	for i, n := range m.networks {
		label := fmt.Sprintf("%s  %s  %d dBm  %s", cleanLine(n.SSID),
			cleanLine(securityLabel(n.Security)), n.SignalDBM, signalLabel(n.SignalDBM))
		b.WriteString(item(i == m.wifiIx, label) + "\n")
	}
	b.WriteString(item(m.wifiIx == len(m.networks), "Enter a network name manually") + "\n")
	return b.String()
}

func signalLabel(dbm int) string {
	switch {
	case dbm >= -50:
		return "excellent"
	case dbm >= -60:
		return "good"
	case dbm >= -70:
		return "fair"
	default:
		return "weak"
	}
}

func (m *model) viewWiFiPass() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Join "+cleanLine(m.selectedSSID())) + "\n\n")
	b.WriteString(labelStyle.Render("Passphrase") + "\n")
	b.WriteString("  " + m.wifiPassInput.View() + "\n")
	b.WriteString("\n")
	b.WriteString(subtitleStyle.Render("The passphrase goes to the device over stdin; it never enters argv or logs."))
	return b.String()
}

func (m *model) viewWiFiManual() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Enter a Wi-Fi network") + "\n\n")
	b.WriteString(labelStyle.Render("SSID") + "\n")
	b.WriteString("  " + m.wifiSSIDInput.View() + "\n\n")
	fmt.Fprintf(&b, "%s Open network (no passphrase)   %s\n", checkbox(m.wifiOpen), subtitleStyle.Render("(ctrl+o)"))
	return b.String()
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
			b.WriteString(selectedStyle.Render("▸ "+line) + "\n")
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
	if countSelected(m.appSelected) == 0 {
		b.WriteString(warnStyle.Render("Nothing selected: enter installs no apps. Press a for all, or s to skip.") + "\n")
	}
	return b.String()
}

func (m *model) viewMarusia() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Marusia needs a VK access token") + "\n\n")
	b.WriteString(subtitleStyle.Render("No stock voice-agent session was found, so nothing could be migrated.") + "\n")
	b.WriteString(subtitleStyle.Render("Paste a token (hidden) or skip; you can set it later in the Web UI.") + "\n\n")
	b.WriteString("  " + m.marusiaToken.View() + "\n")
	return b.String()
}

func (m *model) viewSummary(width int) string {
	var b strings.Builder
	if m.appError != "" {
		b.WriteString(warnStyle.Render("Installation finished with errors") + "\n\n")
		b.WriteString(errStyle.Render(ansi.Wordwrap(cleanLine(m.appError), max(20, width-2), "")) + "\n\n")
	} else {
		b.WriteString(okStyle.Render("Installation complete") + "\n\n")
	}
	if m.inst != nil {
		base := cleanLine(install.HTTPSBase(m.addrInput.Value(), 8443))
		b.WriteString(row("Web UI", valueStyle.Render(base)) + "\n")
		if m.fingerprint != "" {
			b.WriteString(row("Cert SHA-256", valueStyle.Render(m.fingerprint)) + "\n")
		}
		b.WriteString(row("Download", valueStyle.Render(base+"/api/cert.pem")) + "\n")
		b.WriteString(row("Verify", valueStyle.Render("capsulad-cli cert status")) + "\n")
		b.WriteString(subtitleStyle.Render("            Trust the certificate after comparing the fingerprint; the Web UI is self-signed.") + "\n")
	}
	if m.chosenPassword() != "" {
		b.WriteString(row("Admin", valueStyle.Render("password set (shown after deployment)")) + "\n")
	} else {
		b.WriteString(row("Admin", valueStyle.Render("existing password kept")) + "\n")
	}
	if m.wifiSkip {
		b.WriteString(row("Wi-Fi", warnStyle.Render("skipped")) + "\n")
	} else if m.wifi.Connected() {
		b.WriteString(row("Wi-Fi", valueStyle.Render(cleanLine(m.wifi.SSID)+" ("+cleanLine(m.wifi.IPAddress)+")")) + "\n")
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
		b.WriteString(row("Marusia", valueStyle.Render("stock session migrated; vendor file left in place")) + "\n")
	} else if m.marusia.TokenSet && marusiaInstalled(m.appResults) {
		b.WriteString(row("Marusia", valueStyle.Render("VK access token stored")) + "\n")
	} else if m.appSelected["org.capsulad.marusia"] && marusiaInstalled(m.appResults) {
		b.WriteString(row("Marusia", warnStyle.Render("installed without a token")) + "\n")
	}
	if m.backup != nil {
		b.WriteString(row("Backup", valueStyle.Render(wrapIndent(cleanLine(m.backup.Path), width, labelWidth))) + "\n")
	}
	if m.inst != nil {
		b.WriteString(row("Rollback", valueStyle.Render(wrapIndent(m.inst.Platform.Rollback(), width, labelWidth))) + "\n")
	}
	b.WriteString("\n" + valueStyle.Render(ansi.Wordwrap("Nothing was deleted. Vendor software and files are preserved; the boot hook can be disabled.", max(20, width-2), "")) + "\n")
	return b.String()
}

func (m *model) viewError(width int) string {
	var b strings.Builder
	b.WriteString(errStyle.Render("Installation stopped") + "\n\n")
	b.WriteString(valueStyle.Render(ansi.Wordwrap(cleanLine(m.errText), max(20, width-2), "")) + "\n\n")

	// The recovery choices come first: they are the reason this screen exists.
	b.WriteString(titleStyle.Render("What next?") + "\n")
	acts := m.errorActions()
	sel := min(m.errIx, len(acts)-1)
	for i, a := range acts {
		line := fmt.Sprintf("[%s] %s", a.key, a.label)
		if i == sel {
			b.WriteString(selectedStyle.Render("▸ "+line) + "\n")
		} else {
			b.WriteString("  " + itemStyle.Render(line) + "\n")
		}
	}
	b.WriteString("\n" + titleStyle.Render("Details") + "\n")
	wrap := func(s string) string {
		return ansi.Wordwrap(cleanText(s), max(20, width-2), "")
	}
	if m.errOp == opDeploy {
		b.WriteString(subtitleStyle.Render(wrap("The device may be part-way through. Reconnect and inspect /tmp/capsulad-installer/deploy.log before retrying.")) + "\n")
		b.WriteString(subtitleStyle.Render(wrap(device.RecoveryAdvice(cleanLine(m.addrInput.Value()), cleanLine(m.wifi.APSSID), cleanLine(m.wifi.APPasswordHint)))) + "\n")
		if m.deployTail != "" {
			b.WriteString("\n" + titleStyle.Render("Deploy log (verbatim, last lines)") + "\n")
			b.WriteString(logStyle.Render(cleanText(device.LogTail(m.deployTail, 40))))
			b.WriteString("\n")
		}
	} else if m.errOp == opWiFiStatus || m.errOp == opWiFiConnect {
		b.WriteString(subtitleStyle.Render(wrap(device.RecoveryAdvice(cleanLine(m.addrInput.Value()), cleanLine(m.wifi.APSSID), cleanLine(m.wifi.APPasswordHint)))) + "\n")
	} else if m.errOp == opApps {
		b.WriteString(subtitleStyle.Render(wrap("Some selected apps may already be installed; the summary lists each result. Retrying reinstalls the selection.")) + "\n")
	} else {
		b.WriteString(subtitleStyle.Render(wrap("No deployment was started by this step; the device is unchanged.")) + "\n")
	}
	return b.String()
}

func (m *model) viewHostKey() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("New SSH host key") + "\n\n")
	if m.hostKey == nil {
		b.WriteString(subtitleStyle.Render("Waiting for the host key...") + "\n")
		return b.String()
	}
	b.WriteString(row("Host", valueStyle.Render(cleanLine(m.hostKey.host))) + "\n")
	b.WriteString(row("SHA-256", selectedStyle.Render(cleanLine(m.hostKey.fingerprint))) + "\n\n")
	b.WriteString(subtitleStyle.Render("Trust on first use pins this key under your config directory.") + "\n")
	b.WriteString(subtitleStyle.Render("A later mismatch is a hard failure.") + "\n\n")
	b.WriteString(selectedStyle.Render("▸ [y] Trust and continue") + "\n")
	b.WriteString("  " + itemStyle.Render("[n] Refuse") + "\n")
	return b.String()
}

func (m *model) viewAbort() string {
	var b strings.Builder
	b.WriteString(errStyle.Render("Stop the installation?") + "\n\n")
	if m.op == opDeploy {
		b.WriteString("Current step: deploying capsulad.\n\n")
		b.WriteString("The deployment runs detached on the device, because the daemon owns Wi-Fi.\n")
		b.WriteString("Leaving now stops this interface, not that script; the device may finish or fail on its own.\n\n")
		b.WriteString(subtitleStyle.Render("Check later with: cat /tmp/capsulad-installer/deploy.log") + "\n\n")
	} else {
		step := cleanLine(m.status)
		if strings.TrimSpace(step) == "" {
			step = "a device step"
		}
		b.WriteString("Current step: " + step + ".\n\n")
		b.WriteString("This cancels the current step before any deployment starts.\n")
		b.WriteString("The device is left as it was; the last completed step is in the log.\n\n")
	}
	options := []string{"[n] Keep waiting", "[y] Stop and quit"}
	for i, label := range options {
		if i == m.abortIx {
			b.WriteString(selectedStyle.Render("▸ "+label) + "\n")
		} else {
			b.WriteString("  " + itemStyle.Render(label) + "\n")
		}
	}
	return b.String()
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
		return selectedStyle.Render("▸ " + label)
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

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return cleanLine(s)
}

func marusiaInstalled(results []install.AppResult) bool {
	for _, r := range results {
		if r.ID == "org.capsulad.marusia" && r.Status == "installed" {
			return true
		}
	}
	return false
}
