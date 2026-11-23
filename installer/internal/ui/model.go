// Package ui is the keyboard-driven Bubble Tea interface for capsulad-installer.
// It owns the interactive ordering; every device action goes through
// internal/install so the same steps are covered by deterministic tests.
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// ErrAborted is returned when the user stops the installer while a device step
// is in flight.
var ErrAborted = errors.New("installation stopped by the user")

// ErrAbortedDuringDeploy is wrapped by ErrAborted when the detached deployment
// had already been launched, so the device may still be changing.
var ErrAbortedDuringDeploy = errors.New("the detached deployment may still be running")

type screen int

const (
	screenDevice screen = iota
	screenFirmware
	screenAddress
	screenConfirm
	screenReview
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

// formRow is one focusable row of the step 4 installation-options form.
type formRow int

const (
	rowPassGenerate formRow = iota
	rowPassCustom
	rowPassKeep
	rowJingle
	rowIdentityAck
	rowForce
	rowContinue
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
	scroll        int
	bodyWidth     int
	lang          lang
	cat           *catalog
	firstIx       int
	formIx        int

	spinner  spinner.Model
	logs     []string
	progress string
	busy     bool
	op       opKind
	opStart  time.Time
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
	errIx      int
	errCanBack bool
	errCanSkip bool
	errCanAddr bool
	backScreen screen

	hostKey       *hostKeyMsg
	abortIx       int
	aborted       bool
	abortedDeploy bool
	viewport      viewport.Model
}

func newModel(opts Options) *model {
	m := &model{
		opts:          opts,
		screen:        screenDevice,
		lang:          langEN,
		cat:           catalogFor(langEN),
		firstIx:       0,
		spinner:       spinner.New(spinner.WithSpinner(spinner.Dot)),
		appSelected:   map[string]bool{},
		addrInput:     newInput("", false, 48),
		sshUserInput:  newInput("", false, 32),
		sshPassInput:  newInput("", true, 48),
		sshKeyInput:   newInput("", false, 64),
		customPass:    newInput("", true, 48),
		wifiSSIDInput: newInput("", false, 40),
		wifiPassInput: newInput("", true, 48),
		marusiaToken:  newInput("", true, 64),
	}
	m.sshUserInput.SetValue("root")
	if opts.SSHKey != "" {
		m.sshKeyInput.SetValue(opts.SSHKey)
	}
	m.applyPlaceholders()
	m.viewport = viewport.New(76, 12)
	m.viewport.Style = logStyle
	return m
}

// setLanguage switches the interface catalog without losing current input.
func (m *model) setLanguage(l lang) {
	if l == m.lang {
		return
	}
	m.lang = l
	m.cat = catalogFor(l)
	m.applyPlaceholders()
	m.status = ""
}

func (m *model) applyPlaceholders() {
	c := m.cat
	m.addrInput.Placeholder = c.addrPlaceholder
	m.sshUserInput.Placeholder = c.sshUserPlaceholder
	m.sshPassInput.Placeholder = c.sshPassPlaceholder
	m.sshKeyInput.Placeholder = c.keyPlaceholder
	m.customPass.Placeholder = c.adminPassPlaceholder
	m.wifiSSIDInput.Placeholder = c.wifiSSIDPlaceholder
	m.wifiPassInput.Placeholder = c.wifiPassPlaceholder
	m.marusiaToken.Placeholder = c.tokenPlaceholder
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

// Run starts the interface and blocks until it exits. The caller's context
// belongs to the program (signals); device work gets a child context so
// cancelling an operation does not tear down the Bubble Tea event loop.
func Run(ctx context.Context, opts Options) error {
	m := newModel(opts)
	m.ctx, m.cancel = context.WithCancel(ctx)
	defer m.cancel()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	m.prog = p
	final, err := p.Run()
	if err != nil {
		if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
			return ErrAborted
		}
		return err
	}
	fm, ok := final.(*model)
	if !ok || !fm.aborted {
		return nil
	}
	if fm.abortedDeploy {
		return fmt.Errorf("%w: %w", ErrAborted, ErrAbortedDuringDeploy)
	}
	return ErrAborted
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
	prev := m.screen
	model, cmd := m.update(msg)
	if m.screen != prev {
		m.scroll = 0
		m.focusField()
	}
	return model, cmd
}

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m.ctrlC()
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

// ctrlC is the universal escape hatch. While a device step is in flight the
// first press opens the stop dialog; pressing it again on that dialog stops
// immediately. On an idle screen it simply quits.
func (m *model) ctrlC() (tea.Model, tea.Cmd) {
	if m.hostKey != nil {
		m.hostKey.reply <- false
		m.hostKey = nil
		return m, m.stopAndQuit()
	}
	if m.screen == screenAbort {
		return m, m.stopAndQuit()
	}
	if m.busy {
		m.prevScreen = m.screen
		m.abortIx = 0
		m.screen = screenAbort
		return m, nil
	}
	return m, tea.Quit
}

// stopAndQuit cancels the in-flight operation and ends the program. It never
// cancels the program context itself: doing so made Bubble Tea return
// ErrProgramKilled before the aborted model could be inspected.
func (m *model) stopAndQuit() tea.Cmd {
	m.aborted = true
	m.abortedDeploy = m.busy && m.op == opDeploy
	if m.cancel != nil {
		m.cancel()
	}
	return tea.Quit
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
