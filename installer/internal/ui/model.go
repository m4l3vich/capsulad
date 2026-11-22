// Package ui is the keyboard-driven Bubble Tea interface for capsulad-installer.
// It owns the interactive ordering; every device action goes through
// internal/install so the same steps are covered by deterministic tests.
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"capsulad/installer/internal/device"
	"capsulad/installer/internal/install"
)

// Options are the host-side flags. Empty values select the online defaults.
type Options struct {
	Version      string
	ArtifactsDir string
	ReleaseURL   string
	ReleaseTag   string
	AppsURL      string
	AppsTag      string
	ADBPath      string
	SSHKey       string
	ConfigDir    string
	CacheDir     string
}

// ErrAborted is returned when the user confirms an abort during a deployment.
var ErrAborted = errors.New("installation aborted; the device may be mid-deployment")

type screen int

const (
	screenDevice screen = iota
	screenFirmware
	screenAddress
	screenConfirm
	screenWorking
	screenShowPassword
	screenChangePassword
	screenWiFi
	screenWiFiPass
	screenWiFiManual
	screenApps
	screenMarusia
	screenSummary
	screenError
	screenHostKey
	screenAbort
)

type opKind int

const (
	opNone opKind = iota
	opPrepare
	opDeploy
	opWiFiStatus
	opWiFiConnect
	opApps
	opMarusiaProbe
	opMarusiaToken
	opChangePassword
)

type passwordMode int

const (
	passGenerate passwordMode = iota
	passCustom
	passKeep
)

type logMsg string

type progressMsg struct {
	label       string
	done, total int64
}

type hostKeyMsg struct {
	host        string
	fingerprint string
	reply       chan bool
}

type opDoneMsg struct {
	kind opKind

	inst     *install.Installer
	det      device.Detection
	auth     install.AuthState
	backup   *device.BackupResult
	space    *install.SpaceCheck
	appsOff  []install.AppState
	host     string
	identity device.Identity

	deploy      *install.DeployResult
	fingerprint string
	wifi        install.WiFi
	networks    []install.Network
	results     []install.AppResult
	marusia     install.MarusiaResult
	err         error
}

type model struct {
	opts    Options
	ctx     context.Context
	prog    *tea.Program
	cancel  context.CancelFunc
	fieldIx int

	width, height int
	screen        screen
	prevScreen    screen
	cursor        int

	spinner  spinner.Model
	logs     []string
	progress string
	busy     bool
	op       opKind
	status   string

	addrInput     textinput.Model
	sshUserInput  textinput.Model
	sshPassInput  textinput.Model
	sshKeyInput   textinput.Model
	customPass    textinput.Model
	wifiSSIDInput textinput.Model
	wifiPassInput textinput.Model
	marusiaToken  textinput.Model

	firmware   device.Firmware
	firmwareIx int
	deviceIx   int

	inst             *install.Installer
	det              device.Detection
	authState        install.AuthState
	backup           *device.BackupResult
	space            *install.SpaceCheck
	spaceApps        []install.AppState
	hostname         string
	identity         device.Identity
	identityRequired bool
	identityAck      bool
	deploy           *install.DeployResult
	deployTail       string
	fingerprint      string

	passMode      passwordMode
	generated     string
	passwordReady bool
	passEditing   bool
	jingle        bool
	acknowledged  bool
	forceInstall  bool

	passwordAcked bool
	copied        bool
	pendingPass   string

	wifi         install.WiFi
	networks     []install.Network
	wifiIx       int
	wifiSkip     bool
	wifiManual   bool
	wifiOpen     bool
	wifiSecurity string

	appSelected map[string]bool
	appIx       int
	appResults  []install.AppResult
	appError    string

	marusia install.MarusiaResult

	errText    string
	errOp      opKind
	errCanBack bool
	errCanSkip bool
	errCanAddr bool
	backScreen screen

	hostKey  *hostKeyMsg
	aborted  bool
	viewport viewport.Model
}

func newModel(opts Options) *model {
	m := &model{
		opts:          opts,
		screen:        screenDevice,
		deviceIx:      1,
		spinner:       spinner.New(spinner.WithSpinner(spinner.Dot)),
		appSelected:   map[string]bool{},
		addrInput:     newInput("device address, e.g. 192.168.1.42", false, 48),
		sshUserInput:  newInput("root", false, 32),
		sshPassInput:  newInput("SSH password (hidden)", true, 48),
		sshKeyInput:   newInput("SSH private key path (optional)", false, 64),
		customPass:    newInput("your admin password (hidden)", true, 48),
		wifiSSIDInput: newInput("Wi-Fi network name", false, 40),
		wifiPassInput: newInput("Wi-Fi passphrase (hidden)", true, 48),
		marusiaToken:  newInput("VK access token (hidden, optional)", true, 64),
	}
	m.sshUserInput.SetValue("root")
	if opts.SSHKey != "" {
		m.sshKeyInput.SetValue(opts.SSHKey)
	}
	m.viewport = viewport.New(76, 12)
	m.viewport.Style = logStyle
	return m
}

func newInput(placeholder string, secret bool, width int) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.CharLimit = 256
	ti.Width = width
	if secret {
		ti.EchoMode = textinput.EchoPassword
	}
	return ti
}

// Run starts the interface and blocks until it exits.
func Run(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := newModel(opts)
	m.ctx = ctx
	m.cancel = cancel
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	m.prog = p
	final, err := p.Run()
	if err != nil {
		return err
	}
	if fm, ok := final.(*model); ok && fm.aborted {
		return ErrAborted
	}
	return nil
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, textinput.Blink)
}

func (m *model) logFunc() func(string, ...any) {
	return func(format string, args ...any) {
		if m.prog != nil {
			m.prog.Send(logMsg(fmt.Sprintf(format, args...)))
		}
	}
}

func (m *model) progressFunc() func(string, int64, int64) {
	return func(label string, done, total int64) {
		if m.prog != nil {
			m.prog.Send(progressMsg{label: label, done: done, total: total})
		}
	}
}

func (m *model) hostKeyPrompt(host, fingerprint string) bool {
	reply := make(chan bool, 1)
	select {
	case <-m.ctx.Done():
		return false
	default:
	}
	m.prog.Send(hostKeyMsg{host: host, fingerprint: fingerprint, reply: reply})
	select {
	case ok := <-reply:
		return ok
	case <-m.ctx.Done():
		return false
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = max(20, msg.Width-6)
		m.viewport.Height = max(5, msg.Height-12)
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.hostKey != nil {
				m.hostKey.reply <- false
				m.hostKey = nil
				m.aborted = true
				m.cancel()
				return m, tea.Quit
			}
			if m.busy || m.screen == screenAbort {
				if m.screen != screenAbort {
					m.prevScreen = m.screen
					m.screen = screenAbort
				}
				return m, nil
			}
			return m, tea.Quit
		}
		if handled, cmd := m.key(msg); handled {
			m.focusField()
			return m, cmd
		}
		// Unhandled keys go to the focused text field below.
	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case logMsg:
		m.appendLog(string(msg))
		return m, nil
	case progressMsg:
		m.progress = fmt.Sprintf("%s  %s / %s", msg.label, install.BytesLabel(msg.done), install.BytesLabel(msg.total))
		return m, nil
	case hostKeyMsg:
		m.hostKey = &msg
		m.prevScreen = m.screen
		m.screen = screenHostKey
		return m, nil
	case opDoneMsg:
		return m.handleOpDone(msg)
	}
	if in := m.focusedInput(); in != nil {
		var cmd tea.Cmd
		*in, cmd = in.Update(msg)
		// Non-secret fields are single-line identifiers; strip pasted controls
		// before they can reach the terminal or a remote argv.
		if in == &m.wifiSSIDInput || in == &m.addrInput || in == &m.sshUserInput || in == &m.sshKeyInput || in == &m.marusiaToken {
			if hasControl(in.Value()) {
				in.SetValue(cleanLine(in.Value()))
			}
		}
		return m, cmd
	}
	return m, nil
}

func (m *model) appendLog(line string) {
	for _, part := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		m.logs = append(m.logs, cleanText(part))
	}
	if len(m.logs) > 500 {
		m.logs = m.logs[len(m.logs)-500:]
	}
	m.viewport.SetContent(strings.Join(m.logs, "\n"))
	m.viewport.GotoBottom()
}

func (m *model) focusedInput() *textinput.Model {
	switch m.screen {
	case screenAddress:
		switch m.fieldIx {
		case 0:
			return &m.addrInput
		case 1:
			return &m.sshUserInput
		case 2:
			return &m.sshPassInput
		case 3:
			return &m.sshKeyInput
		}
	case screenConfirm:
		if m.passEditing {
			return &m.customPass
		}
	case screenWiFiManual:
		return &m.wifiSSIDInput
	case screenWiFiPass:
		return &m.wifiPassInput
	case screenChangePassword:
		return &m.customPass
	case screenMarusia:
		return &m.marusiaToken
	}
	return nil
}

// focusField blurs every input and focuses the one the current screen uses.
func (m *model) focusField() {
	for _, in := range []*textinput.Model{
		&m.addrInput, &m.sshUserInput, &m.sshPassInput, &m.sshKeyInput,
		&m.customPass, &m.wifiSSIDInput, &m.wifiPassInput, &m.marusiaToken,
	} {
		in.Blur()
	}
	if in := m.focusedInput(); in != nil {
		in.Focus()
	}
}
