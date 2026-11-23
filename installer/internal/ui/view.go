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
	logStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	footerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	stepStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	ruleStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

const minBodyLines = 3

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
	var statusLines []string
	if status != "" {
		wrapped := ansi.Wordwrap(cleanLine(status), max(10, cw-2), "")
		statusLines = strings.Split(wrapped, "\n")
		if len(statusLines) > 3 {
			statusLines = statusLines[:3]
			statusLines[2] = ansi.Truncate(statusLines[2]+"…", max(1, cw-4), "…")
		}
	}
	// One leading blank, header, rule, gap, body, gap, status, footer.
	avail := height - 6 - len(statusLines)
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
	for _, ln := range statusLines {
		lines = append(lines, warnStyle.Render("! "+ln))
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
	var n int
	var title string
	switch m.screen {
	case screenDevice:
		n, title = 1, m.cat.stepLanguageDevice
	case screenFirmware:
		n, title = 2, m.cat.stepFirmware
	case screenAddress:
		n, title = 3, m.cat.stepAddress
	case screenConfirm:
		n, title = 4, m.cat.stepOptions
	case screenReview:
		n, title = 5, m.cat.stepSummary
	case screenWorking:
		return m.cat.stepWorking
	case screenShowPassword, screenChangePassword:
		n, title = 6, m.cat.stepPassword
	case screenWiFi, screenWiFiPass, screenWiFiManual:
		n, title = 7, m.cat.stepWiFi
	case screenApps:
		n, title = 8, m.cat.stepApps
	case screenMarusia:
		n, title = 9, m.cat.stepMarusia
	case screenSummary:
		return m.cat.stepComplete
	case screenError:
		return m.cat.stepStopped
	case screenHostKey:
		return m.cat.stepHostKey
	case screenAbort:
		return m.cat.stepAbort
	}
	if n == 0 {
		return title
	}
	return fmt.Sprintf(m.cat.stepFormat, n, title)
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
	m.bodyWidth = width
	switch m.screen {
	case screenDevice:
		return m.viewDevice()
	case screenFirmware:
		return m.viewFirmware()
	case screenAddress:
		return m.viewAddress()
	case screenConfirm:
		return m.viewForm(width)
	case screenReview:
		return m.viewReview(width)
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
	var suffix string
	switch m.screen {
	case screenDevice:
		suffix = m.cat.hintDevice
	case screenFirmware:
		suffix = m.cat.hintFirmware
	case screenAddress:
		if m.firmware == device.FirmwareAlpine {
			suffix = m.cat.hintAddressAlpine
		} else {
			suffix = m.cat.hintAddressStock
		}
	case screenConfirm:
		if m.passEditing {
			suffix = m.cat.hintFormEditing
		} else {
			suffix = m.cat.hintForm
		}
	case screenReview:
		suffix = m.cat.hintSummary
	case screenWorking:
		suffix = m.cat.hintWorking
	case screenShowPassword:
		suffix = m.cat.hintPassword
	case screenChangePassword:
		suffix = m.cat.hintChangePassword
	case screenWiFi:
		suffix = m.cat.hintWiFi
	case screenWiFiPass:
		suffix = m.cat.hintWiFiPass
	case screenWiFiManual:
		suffix = m.cat.hintWiFiManual
	case screenApps:
		suffix = m.cat.hintApps
	case screenMarusia:
		suffix = m.cat.hintMarusia
	case screenSummary:
		suffix = m.cat.hintComplete
	case screenError:
		suffix = m.cat.hintError
	case screenHostKey:
		suffix = m.cat.hintHostKey
	case screenAbort:
		suffix = m.cat.hintAbort
	}
	if overflow && m.screen != screenWorking {
		suffix += " · " + m.cat.hintScroll
	}
	return footerStyle.Render(ansi.Truncate(suffix, width, "…"))
}

func (m *model) firstRow(ix int, active bool, label, suffix string) string {
	row := radio(active, label, suffix)
	if ix == m.firstIx {
		return "▸ " + row
	}
	return "  " + row
}

// wrapText wraps a paragraph to the content width.
func (m *model) wrapText(s string) string {
	w := m.bodyWidth
	if w <= 0 {
		w = 78
	}
	return ansi.Wordwrap(s, max(20, w-2), "")
}

// wrapIndented wraps a paragraph and aligns continuation lines under indent.
func (m *model) wrapIndented(s string, indent int) string {
	w := m.bodyWidth
	if w <= 0 {
		w = 78
	}
	wrapped := ansi.Wordwrap(s, max(10, w-indent), "/-_")
	return strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", indent))
}

func (m *model) viewDevice() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.deviceHeading) + "\n\n")
	b.WriteString(labelStyle.Render(m.cat.languageLabel) + "\n")
	b.WriteString(m.firstRow(0, m.lang == langEN, m.cat.langEnglish, "") + "\n")
	b.WriteString(m.firstRow(1, m.lang == langRU, m.cat.langRussian, "") + "\n\n")
	b.WriteString(labelStyle.Render(m.cat.deviceLabel) + "\n")
	b.WriteString(m.firstRow(2, false, m.cat.deviceOriginal, m.cat.notSupported) + "\n")
	b.WriteString("    " + subtitleStyle.Render(m.wrapIndented(m.cat.originalNote, 4)) + "\n\n")
	b.WriteString(m.firstRow(3, true, m.cat.deviceMini, m.cat.supported) + "\n")
	b.WriteString("    " + subtitleStyle.Render(m.wrapIndented(m.cat.miniNote, 4)) + "\n\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.onlyMiniNote)))
	return b.String()
}

func (m *model) viewFirmware() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.firmwareHeading) + "\n\n")
	b.WriteString(radio(m.firmwareIx == 0, m.cat.firmwareStock, "") + "\n")
	b.WriteString("    " + subtitleStyle.Render(m.wrapIndented(m.cat.firmwareStockNote, 4)) + "\n\n")
	b.WriteString(radio(m.firmwareIx == 1, m.cat.firmwareAlpine, "") + "\n")
	b.WriteString("    " + subtitleStyle.Render(m.wrapIndented(m.cat.firmwareAlpineNote, 4)) + "\n\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.firmwareBoardNote)))
	return b.String()
}

func (m *model) viewAddress() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.addressHeading) + "\n\n")
	b.WriteString(labelStyle.Render(m.cat.addressLabel) + "\n")
	b.WriteString(m.field(0, &m.addrInput) + "\n\n")
	if m.firmware == device.FirmwareStock {
		b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.addressStockNote)))
	} else {
		b.WriteString(labelStyle.Render(m.cat.sshUserLabel) + "\n")
		b.WriteString(m.field(1, &m.sshUserInput) + "\n\n")
		b.WriteString(labelStyle.Render(m.cat.sshPasswordLabel) + "\n")
		b.WriteString(m.field(2, &m.sshPassInput) + "\n\n")
		b.WriteString(labelStyle.Render(m.cat.keyLabel) + "\n")
		b.WriteString(m.field(3, &m.sshKeyInput) + "\n\n")
		b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.addressAlpineNote)))
	}
	return b.String()
}

func (m *model) field(ix int, in interface{ View() string }) string {
	if ix == m.fieldIx {
		return selectedStyle.Render("▸ ") + in.View()
	}
	return "  " + in.View()
}

func (m *model) row(label, value string) string {
	w := m.cat.rowLabelWidth
	return labelStyle.Render(padRight(label, w)) + value
}

func (m *model) indent() string {
	return strings.Repeat(" ", m.cat.rowLabelWidth)
}

func padRight(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// wrapIndent word-wraps a long value under a labelled row, keeping
// continuation lines aligned with the value column.
func wrapIndent(s string, width, indent int) string {
	wrapped := ansi.Wordwrap(s, max(10, width-indent), "/-_")
	return strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", indent))
}

func (m *model) viewForm(width int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.optionsHeading) + "\n\n")
	b.WriteString(titleStyle.Render(m.cat.passwordHeading) + "\n")
	if !m.authState.Known {
		b.WriteString(warnStyle.Render("  "+m.wrapIndented(m.cat.authUnknownNote, 2)) + "\n")
	}

	rows := m.formRows()
	sel := rowContinue
	if m.formIx >= 0 && m.formIx < len(rows) {
		sel = rows[m.formIx]
	}
	line := func(k formRow, text string) string {
		if sel == k {
			return "▸ " + text
		}
		return "  " + text
	}

	b.WriteString(line(rowPassGenerate, radio(m.passMode == passGenerate, m.cat.passGenerate, m.cat.passRecommended)) + "\n")
	b.WriteString(line(rowPassCustom, radio(m.passMode == passCustom, m.cat.passCustom, "")) + "\n")
	if m.authState.Configured {
		b.WriteString(line(rowPassKeep, radio(m.passMode == passKeep, m.cat.passKeep, "")) + "\n")
	}
	if m.passEditing {
		b.WriteString("\n  " + m.customPass.View() + "\n")
		b.WriteString(subtitleStyle.Render("  "+m.wrapIndented(m.cat.passCustomHint, 2)) + "\n")
	}
	b.WriteString("\n")

	opt := func(k formRow, label, state string) string {
		return line(k, fmt.Sprintf("%-*s %s", m.cat.optLabelWidth, label, state))
	}
	b.WriteString(opt(rowJingle, m.cat.optJingle, m.onOff(m.jingle)) + "\n")
	if m.identityRequired {
		b.WriteString(opt(rowIdentityAck, m.cat.optIdentity, checkbox(m.identityAck)) + "\n")
	}
	b.WriteString(opt(rowAcknowledge, m.cat.optAcknowledge, checkbox(m.acknowledged)) + "\n")
	if m.space != nil && !m.space.OK {
		b.WriteString(opt(rowForce, m.cat.optForce, checkbox(m.forceInstall)) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(line(rowContinue, selectedStyle.Render(m.cat.formContinue)) + "\n")
	return b.String()
}

func (m *model) viewReview(width int) string {
	det := m.det
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.installSummaryHeading) + "\n\n")
	b.WriteString(m.row(m.cat.labelDevice, valueStyle.Render(m.platformLabel(det.Platform)+" · "+cleanLine(det.Arch))) + "\n")
	b.WriteString(m.row(m.cat.labelIdentity, valueStyle.Render(cleanLine(m.identity.String()))) + "\n")
	if m.identityRequired {
		b.WriteString(m.indent() + warnStyle.Render(wrapIndent(m.cat.noStableSerialNote, width, m.cat.rowLabelWidth)) + "\n")
	}
	if m.backup != nil {
		b.WriteString(m.row(m.cat.labelBackup, valueStyle.Render(wrapIndent(cleanLine(m.backup.Path), width, m.cat.rowLabelWidth))) + "\n")
		b.WriteString(m.indent() + subtitleStyle.Render(fmt.Sprintf(m.cat.backupDetailFormat,
			install.BytesLabel(m.backup.Bytes), m.backup.Entries)) + "\n")
	} else {
		b.WriteString(m.row(m.cat.labelBackup, valueStyle.Render(m.cat.backupNotNeeded)) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(m.spaceLines())
	b.WriteString("\n")
	b.WriteString(selectedStyle.Render("▸ "+m.cat.installAction) + subtitleStyle.Render("   "+m.cat.installActionHint) + "\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.deployDetachedNote)))
	return b.String()
}

func (m *model) spaceLines() string {
	if m.space == nil {
		return ""
	}
	s := m.space
	tone := okStyle
	if !s.OK {
		tone = warnStyle
	}
	var b strings.Builder
	b.WriteString(m.row(m.cat.labelSpace, tone.Render(fmt.Sprintf(m.cat.spaceFormat,
		install.BytesLabel(s.AvailableBytes), install.BytesLabel(s.RequiredBytes)))) + "\n")
	if s.StagedDaemon > 0 && !s.OK {
		b.WriteString(m.indent() + subtitleStyle.Render(fmt.Sprintf(m.cat.spaceStagedFormat,
			install.BytesLabel(s.RequiredBytes-s.ReserveBytes),
			install.BytesLabel(s.StagedDaemon), install.BytesLabel(s.InstalledDaemon))) + "\n")
	}
	if !s.OK {
		b.WriteString(m.indent() + warnStyle.Render(fmt.Sprintf(m.cat.spaceShortFormat,
			install.BytesLabel(s.RequiredBytes-s.AvailableBytes))) + "\n")
		if len(m.spaceApps) > 0 {
			var idle []string
			for _, a := range m.spaceApps {
				if !a.Running {
					idle = append(idle, cleanLine(a.ID))
				}
			}
			if len(idle) > 0 {
				b.WriteString(m.indent() + subtitleStyle.Render(fmt.Sprintf(m.cat.spaceIdleAppsFormat,
					strings.Join(idle, ", "))) + "\n")
			}
		}
		b.WriteString(m.indent() + subtitleStyle.Render(m.wrapIndented(m.cat.spaceDeployerNote, m.cat.rowLabelWidth)) + "\n")
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
	b.WriteString(titleStyle.Render(m.cat.passwordScreenHeading) + "\n\n")
	pw := m.chosenPassword()
	if pw == "" {
		b.WriteString(valueStyle.Render(m.cat.keepingPassword) + "\n\n")
		b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.recoverPassword)) + "\n")
	} else {
		b.WriteString(boxStyle.Copy().
			BorderForeground(lipgloss.Color("42")).Padding(0, 2).MarginLeft(2).
			Render(selectedStyle.Render(escapeControls(pw))) + "\n\n")
		b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.onlyTimeShown)) + "\n")
		b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.recoverPassword)) + "\n")
		if m.copied {
			b.WriteString(okStyle.Render(m.cat.copyRequested) + "\n")
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s %s   %s\n", checkbox(m.passwordAcked), m.cat.storedPassword, subtitleStyle.Render("(space)"))
	return b.String()
}

func (m *model) viewChangePassword() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.changePasswordHeading) + "\n\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.changePasswordNote)) + "\n\n")
	b.WriteString("  " + m.customPass.View() + "\n")
	return b.String()
}

func (m *model) viewWiFi() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.wifiHeading) + "\n\n")
	fmt.Fprintf(&b, "%s\n", m.row(m.cat.wifiStateLabel, valueStyle.Render(m.orUnknown(m.wifi.State))))
	if m.wifi.Connected() {
		fmt.Fprintf(&b, "%s\n", m.row(m.cat.wifiConnectedLabel, valueStyle.Render(cleanLine(m.wifi.SSID)+" ("+cleanLine(m.wifi.IPAddress)+")")))
	}
	if m.wifi.APActive {
		fmt.Fprintf(&b, "%s\n", m.row(m.cat.wifiSetupAPLabel, valueStyle.Render(cleanLine(m.wifi.APSSID))))
		if m.wifi.APPortalURL != "" {
			fmt.Fprintf(&b, "%s\n", m.row(m.cat.wifiPortalLabel, valueStyle.Render(cleanLine(m.wifi.APPortalURL))))
		}
		if m.wifi.APPasswordHint != "" {
			fmt.Fprintf(&b, "%s\n", m.row(m.cat.wifiAPPasswordLabel, valueStyle.Render(cleanLine(m.wifi.APPasswordHint))))
		}
	}
	b.WriteString("\n")
	if len(m.networks) == 0 {
		b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.wifiNoScan)) + "\n\n")
	}
	for i, n := range m.networks {
		label := fmt.Sprintf("%s  %s  %d dBm  %s", cleanLine(n.SSID),
			m.securityLabel(n.Security), n.SignalDBM, m.signalLabel(n.SignalDBM))
		b.WriteString(item(i == m.wifiIx, label) + "\n")
	}
	b.WriteString(item(m.wifiIx == len(m.networks), m.cat.wifiManual) + "\n")
	return b.String()
}

func (m *model) signalLabel(dbm int) string {
	switch {
	case dbm >= -50:
		return m.cat.signalExcellent
	case dbm >= -60:
		return m.cat.signalGood
	case dbm >= -70:
		return m.cat.signalFair
	default:
		return m.cat.signalWeak
	}
}

func (m *model) viewWiFiPass() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fmt.Sprintf(m.cat.wifiJoinFormat, cleanLine(m.selectedSSID()))) + "\n\n")
	b.WriteString(labelStyle.Render(m.cat.wifiPassphraseLabel) + "\n")
	b.WriteString("  " + m.wifiPassInput.View() + "\n\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.wifiPassphraseNote)))
	return b.String()
}

func (m *model) viewWiFiManual() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.wifiManualHeading) + "\n\n")
	b.WriteString(labelStyle.Render(m.cat.wifiSSIDLabel) + "\n")
	b.WriteString("  " + m.wifiSSIDInput.View() + "\n\n")
	fmt.Fprintf(&b, "%s %s   %s\n", checkbox(m.wifiOpen), m.cat.wifiOpenNetwork, subtitleStyle.Render("(ctrl+o)"))
	return b.String()
}

func (m *model) viewApps() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.appsHeading) + "\n\n")
	for i, a := range install.Catalogue {
		mark := "[ ]"
		if m.appSelected[a.ID] {
			mark = "[x]"
		}
		line := fmt.Sprintf("%s %-18s %s", mark, a.Name, subtitleStyle.Render(m.appDescription(a)))
		if i == m.appIx {
			b.WriteString(selectedStyle.Render("▸ "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s", labelStyle.Render(fmt.Sprintf(m.cat.appsSelectedFormat, countSelected(m.appSelected))))
	if m.inst != nil && m.inst.Provider != nil {
		fmt.Fprintf(&b, m.cat.appsReleaseFormat, cleanLine(m.inst.Provider.Version()), m.inst.Platform)
	}
	b.WriteString("\n")
	if countSelected(m.appSelected) == 0 {
		b.WriteString(warnStyle.Render(m.wrapText(m.cat.appsNoneNote)) + "\n")
	}
	return b.String()
}

func (m *model) viewMarusia() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.marusiaHeading) + "\n\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.marusiaNoSession)) + "\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.marusiaPaste)) + "\n\n")
	b.WriteString("  " + m.marusiaToken.View() + "\n")
	return b.String()
}

func (m *model) viewSummary(width int) string {
	var b strings.Builder
	if m.appError != "" {
		b.WriteString(warnStyle.Render(m.cat.finishedWithErrors) + "\n\n")
		b.WriteString(errStyle.Render(ansi.Wordwrap(cleanLine(m.appError), max(20, width-2), "")) + "\n\n")
	} else {
		b.WriteString(okStyle.Render(m.cat.completeHeading) + "\n\n")
	}
	if m.inst != nil {
		base := cleanLine(install.HTTPSBase(m.addrInput.Value(), 8443))
		b.WriteString(m.row(m.cat.labelWebUI, valueStyle.Render(base)) + "\n")
		if m.fingerprint != "" {
			b.WriteString(m.row(m.cat.labelCertFingerprint, valueStyle.Render(m.fingerprint)) + "\n")
		}
		b.WriteString(m.row(m.cat.labelDownload, valueStyle.Render(base+"/api/cert.pem")) + "\n")
		b.WriteString(m.row(m.cat.labelVerify, valueStyle.Render("capsulad-cli cert status")) + "\n")
		b.WriteString(m.indent() + subtitleStyle.Render(m.wrapIndented(m.cat.trustCertNote, m.cat.rowLabelWidth)) + "\n")
	}
	if m.chosenPassword() != "" {
		b.WriteString(m.row(m.cat.labelAdmin, valueStyle.Render(m.cat.adminSet)) + "\n")
	} else {
		b.WriteString(m.row(m.cat.labelAdmin, valueStyle.Render(m.cat.adminKept)) + "\n")
	}
	if m.wifiSkip {
		b.WriteString(m.row(m.cat.labelWiFi, warnStyle.Render(m.cat.wifiSkipped)) + "\n")
	} else if m.wifi.Connected() {
		b.WriteString(m.row(m.cat.labelWiFi, valueStyle.Render(cleanLine(m.wifi.SSID)+" ("+cleanLine(m.wifi.IPAddress)+")")) + "\n")
	}
	if len(m.appResults) > 0 {
		b.WriteString("\n" + titleStyle.Render(m.cat.appsSectionHeading) + "\n")
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
		b.WriteString(m.row(m.cat.labelMarusia, valueStyle.Render(m.cat.marusiaMigrated)) + "\n")
	} else if m.marusia.TokenSet && marusiaInstalled(m.appResults) {
		b.WriteString(m.row(m.cat.labelMarusia, valueStyle.Render(m.cat.marusiaTokenSet)) + "\n")
	} else if m.appSelected["org.capsulad.marusia"] && marusiaInstalled(m.appResults) {
		b.WriteString(m.row(m.cat.labelMarusia, warnStyle.Render(m.cat.marusiaNoToken)) + "\n")
	}
	if m.backup != nil {
		b.WriteString(m.row(m.cat.labelBackup, valueStyle.Render(wrapIndent(cleanLine(m.backup.Path), width, m.cat.rowLabelWidth))) + "\n")
	}
	if m.inst != nil {
		b.WriteString(m.row(m.cat.labelRollback, valueStyle.Render(wrapIndent(m.inst.Platform.Rollback(), width, m.cat.rowLabelWidth))) + "\n")
	}
	b.WriteString("\n" + valueStyle.Render(ansi.Wordwrap(m.cat.nothingDeleted, max(20, width-2), "")) + "\n")
	return b.String()
}

func (m *model) viewError(width int) string {
	var b strings.Builder
	b.WriteString(errStyle.Render(m.cat.errorHeading) + "\n\n")
	b.WriteString(valueStyle.Render(ansi.Wordwrap(cleanLine(m.errText), max(20, width-2), "")) + "\n\n")

	// The recovery choices come first: they are the reason this screen exists.
	b.WriteString(titleStyle.Render(m.cat.whatNextHeading) + "\n")
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
	b.WriteString("\n" + titleStyle.Render(m.cat.detailsHeading) + "\n")
	wrap := func(s string) string {
		return ansi.Wordwrap(cleanText(s), max(20, width-2), "")
	}
	if m.errOp == opDeploy {
		b.WriteString(subtitleStyle.Render(wrap(m.cat.deployPartialNote)) + "\n")
		b.WriteString(subtitleStyle.Render(wrap(m.recoveryAdvice(cleanLine(m.addrInput.Value()), cleanLine(m.wifi.APSSID), cleanLine(m.wifi.APPasswordHint)))) + "\n")
		if m.deployTail != "" {
			b.WriteString("\n" + titleStyle.Render(m.cat.deployLogHeading) + "\n")
			b.WriteString(logStyle.Render(cleanText(device.LogTail(m.deployTail, 40))))
			b.WriteString("\n")
		}
	} else if m.errOp == opWiFiStatus || m.errOp == opWiFiConnect {
		b.WriteString(subtitleStyle.Render(wrap(m.recoveryAdvice(cleanLine(m.addrInput.Value()), cleanLine(m.wifi.APSSID), cleanLine(m.wifi.APPasswordHint)))) + "\n")
	} else if m.errOp == opApps {
		b.WriteString(subtitleStyle.Render(wrap(m.cat.appsErrorNote)) + "\n")
	} else {
		b.WriteString(subtitleStyle.Render(wrap(m.cat.noDeployNote)) + "\n")
	}
	return b.String()
}

func (m *model) viewHostKey() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.cat.hostKeyHeading) + "\n\n")
	if m.hostKey == nil {
		b.WriteString(subtitleStyle.Render(m.cat.hostKeyWaiting) + "\n")
		return b.String()
	}
	b.WriteString(m.row(m.cat.hostLabel, valueStyle.Render(cleanLine(m.hostKey.host))) + "\n")
	b.WriteString(m.row(m.cat.fingerprintLabel, selectedStyle.Render(cleanLine(m.hostKey.fingerprint))) + "\n\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.hostKeyTOFU)) + "\n")
	b.WriteString(subtitleStyle.Render(m.wrapText(m.cat.hostKeyMismatch)) + "\n\n")
	b.WriteString(selectedStyle.Render("▸ [y] "+m.cat.trustContinue) + "\n")
	b.WriteString("  " + itemStyle.Render("[n] "+m.cat.refuse) + "\n")
	return b.String()
}

func (m *model) viewAbort() string {
	var b strings.Builder
	b.WriteString(errStyle.Render(m.cat.abortHeading) + "\n\n")
	step := m.cat.abortStepFallback
	if m.op != opNone {
		step = m.opLabel(m.op)
	}
	fmt.Fprintf(&b, m.cat.abortCurrentStepFormat+"\n\n", step)
	if m.op == opDeploy {
		b.WriteString(m.wrapText(m.cat.abortDeployLine1) + "\n")
		b.WriteString(m.wrapText(m.cat.abortDeployLine2) + "\n\n")
		b.WriteString(subtitleStyle.Render(m.cat.abortCheckLog) + "\n\n")
	} else {
		b.WriteString(m.wrapText(m.cat.abortOtherLine1) + "\n")
		b.WriteString(m.wrapText(m.cat.abortOtherLine2) + "\n\n")
	}
	options := []string{m.cat.abortKeepWaiting, m.cat.abortStopQuit}
	for i, label := range options {
		if i == m.abortIx {
			b.WriteString(selectedStyle.Render("▸ "+label) + "\n")
		} else {
			b.WriteString("  " + itemStyle.Render(label) + "\n")
		}
	}
	return b.String()
}

func (m *model) onOff(on bool) string {
	if on {
		return m.cat.onOn
	}
	return m.cat.onOff
}

func (m *model) orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return m.cat.wifiUnknown
	}
	return cleanLine(s)
}

func (m *model) securityLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return m.cat.securityOpen
	}
	return strings.ToUpper(s)
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

func countSelected(sel map[string]bool) int {
	n := 0
	for _, on := range sel {
		if on {
			n++
		}
	}
	return n
}

func marusiaInstalled(results []install.AppResult) bool {
	for _, r := range results {
		if r.ID == "org.capsulad.marusia" && r.Status == "installed" {
			return true
		}
	}
	return false
}
