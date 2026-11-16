// Marusia is a first-party voice assistant that speaks the recovered VK
// voice-agent protocol: it streams microphone PCM to the vendor's gRPC
// Phrase/Speech service and plays the TTS URL the service returns.
//
// The workload is deliberately small: one turn at a time, one conversation per
// activation, a visible ring indication for the whole capture, and a complete
// teardown on every terminal path. All device and account credentials come
// from manifest settings; nothing is invented.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"capsula/apps/marusia/internal/media"
	"capsula/apps/marusia/internal/settings"
	"capsula/apps/marusia/internal/turn"
	"capsula/apps/marusia/internal/vkclient"
	"capsula/apps/marusia/internal/vkproto/clientpb"
	"capsula/sdk/capsula"
)

const (
	ringResource   = "light.ring.main"
	outputEndpoint = "audio.output.speaker"

	dismissActionID = "dismiss"
	reauthActionID  = "reauth"

	// captureQueue is 100 x 20 ms = 2 s: capture starts before the speech
	// stream is authorized and opened, so the queue has to hold the utterance
	// head while the opener is in flight. A device turn measured roughly
	// 400-500 ms from capture start to phrase open; 2 s leaves margin for a
	// slow authorization round trip without ever blocking the mic reader,
	// which would make the daemon drop audio.
	captureQueue = 100
	// playbackWriteMS is how much PCM one speaker write carries.
	playbackWriteMS = 100
	// errorIndication is how long a terminal error stays visible.
	errorIndication = 1500 * time.Millisecond
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsula.Connect(ctx, env)
	if err != nil {
		log.Fatalf("marusia: connect: %v", err)
	}
	defer client.Close()

	a := newApp(ctx, client)
	client.SetHandlers(capsula.Handlers{
		Ready: func() {
			log.Printf("marusia: ready; %s", a.summary())
		},
		Activation: func(_ context.Context, activation *capsula.Activation) {
			a.enqueue(activation)
		},
		TouchRelease: func(released *capsula.InputReleased) {
			a.releaseInput(released)
		},
		Settings: a.applySettings,
		Action: func(_ context.Context, action *capsula.ActionInvoked) {
			a.action(action)
		},
		Invocation: func(_ context.Context, invocation *capsula.LaunchInvocation) {
			detail := "marusia is ready"
			if !a.config().HasSession() {
				detail = "marusia is ready; no session is configured, so the next turn tries the " +
					"provisioning attach"
			}
			_ = client.NotifyInvocationResult(invocation.ID, true, detail)
		},
	})

	queueDone := make(chan struct{})
	go func() {
		a.runQueue()
		close(queueDone)
	}()
	log.Printf("marusia: starting bus")
	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Printf("marusia: bus stopped: %v", err)
	}
	a.shutdown()
	select {
	case <-queueDone:
	case <-time.After(5 * time.Second):
		log.Printf("marusia: shutdown timed out waiting for the turn loop")
	}
}

// app is the process-wide state: last good settings, one protocol client, one
// active turn and the activation queue.
type app struct {
	ctx    context.Context
	client *capsula.Client
	fetch  *media.Fetch

	// fallback is the identity used while the serial and device_id settings
	// are empty. It is resolved once, at startup, and never persisted.
	fallback settings.Fallback

	mu           sync.Mutex
	values       settings.Values
	cfg          settings.Config
	protocol     *vkclient.Client
	protocolKey  protocolKey
	active       *activeTurn
	pendingInput string
	queue        chan activationJob
	sessionOnce  sync.Once
	serialOnce   sync.Once
}

// activationJob is one queued activation plus the framework's expiry as a
// local deadline. The activation carries monotonic nanoseconds; the queue
// converts the remaining window once, at enqueue time.
type activationJob struct {
	activation  *capsula.Activation
	deadline    time.Time
	hasDeadline bool
}

type protocolKey struct {
	base, xtoken, serial, deviceID, version string
	sessionID, sessionSecret                string
}

// logSession records the first successful attach of the process: whether the
// service accepted an empty xtoken_id is the one provisioning fact an operator
// needs from the log.
func (a *app) logSession(session vkclient.Session) {
	config := a.config()
	a.sessionOnce.Do(func() {
		if config.Provisioned() {
			log.Printf("marusia: attach succeeded with the provisioned xtoken_id; account=%s",
				redactAccount(session.AccountID))
			return
		}
		log.Printf("marusia: attach succeeded with an empty xtoken_id (the service accepted an "+
			"unprovisioned attach); account=%s; configure session_id/session_secret if that changes",
			redactAccount(session.AccountID))
	})
}

type activeTurn struct {
	activationID string
	cancel       context.CancelFunc
	release      chan struct{}
	releaseOnce  sync.Once
}

func (t *activeTurn) signalRelease() {
	t.releaseOnce.Do(func() { close(t.release) })
}

// logSerialSource records where the device serial came from, once per process.
// The source (settings, env, framework or cpuinfo) is the one thing an operator
// needs to know; the serial value itself is never logged.
func (a *app) logSerialSource(config settings.Config) {
	if config.SerialSource == "" {
		return
	}
	a.serialOnce.Do(func() {
		log.Printf("marusia: device serial source=%s", config.SerialSource)
	})
}

func newApp(ctx context.Context, client *capsula.Client) *app {
	a := &app{
		ctx:      ctx,
		client:   client,
		fetch:    media.NewFetch(),
		fallback: loadIdentity(ctx, client),
		values:   settings.Default(),
		queue:    make(chan activationJob, 1),
	}
	config, warnings, err := settings.Build(a.values, a.fallback)
	if err != nil {
		log.Printf("marusia: initial settings: %v", err)
		a.cfg = settings.Config{}
	} else {
		a.cfg = config
		a.logSerialSource(config)
		for _, warning := range warnings {
			log.Printf("marusia: %s", warning)
		}
	}
	return a
}

func (a *app) config() settings.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// deviceService is the local framework service that reports the stable device
// identity; GetInfo is app-readable.
const deviceService = "capsula.framework.v1.DeviceService"

// deviceInfoTimeout bounds the one identity call at startup.
const deviceInfoTimeout = 5 * time.Second

// deviceInfo is the subset of DeviceService.GetInfo this app uses.
type deviceInfo struct {
	DeviceID       string `json:"deviceId"`
	HardwareSerial string `json:"hardwareSerial"`
}

// deviceInfoCaller is the SDK surface identity resolution needs;
// *capsula.Client implements it with CallJSON.
type deviceInfoCaller interface {
	CallJSON(ctx context.Context, targetAppID, service, rpc string, request, response any, timeout time.Duration) error
}

// cpuSerialReader is a variable so a test can simulate a host without
// /proc/cpuinfo.
var cpuSerialReader = settings.ReadCPUSerial

// envSerialReader reads the launcher-injected SoC serial. The SDK constant is
// the contract; it is a variable so a test can drive the precedence without
// touching the process environment.
var envSerialReader = func() string { return capsula.FromEnv().DeviceSerial }

// loadIdentity resolves the identity the settings do not supply. The SoC
// serial is resolved once per process, in this order: the launcher's
// CAPSULA_DEVICE_SERIAL, the framework's DeviceService.GetInfo hardware_serial,
// a local /proc/cpuinfo read (the stock sandbox has no /proc, so this only
// works on a host), then empty. The environment comes first because a native
// runtime built before DeviceInfo.hardware_serial existed decodes JSON replies
// with an older descriptor and drops the field. Every settings revision and
// every client then sees the same x-device-sn. The framework also supplies the
// stable device_id; a failure is logged once and the app continues with
// whatever is available; nothing is persisted.
func loadIdentity(ctx context.Context, client deviceInfoCaller) settings.Fallback {
	fallback := settings.Fallback{Generated: vkclient.NewUUID()}
	if serial := strings.TrimSpace(envSerialReader()); serial != "" {
		fallback.Serial, fallback.SerialSource = serial, settings.SerialSourceEnv
	}
	setLocalSerial := func() {
		if fallback.Serial != "" {
			return
		}
		if serial := strings.TrimSpace(cpuSerialReader()); serial != "" {
			fallback.Serial, fallback.SerialSource = serial, settings.SerialSourceCPUInfo
		}
	}
	if client == nil {
		setLocalSerial()
		return fallback
	}
	var info deviceInfo
	if err := client.CallJSON(ctx, "", deviceService, "GetInfo", nil, &info, deviceInfoTimeout); err != nil {
		log.Printf("marusia: device info unavailable; falling back to the per-process device id: %v", err)
		setLocalSerial()
		return fallback
	}
	if fallback.Serial == "" {
		if serial := strings.TrimSpace(info.HardwareSerial); serial != "" {
			fallback.Serial, fallback.SerialSource = serial, settings.SerialSourceFramework
		} else {
			setLocalSerial()
		}
	}
	fallback.DeviceID = strings.TrimSpace(info.DeviceID)
	return fallback
}

func (a *app) summary() string {
	config := a.config()
	switch {
	case config.HasSession():
		return fmt.Sprintf("endpoint=%s upload=%s auth=session (session_id/session_secret settings)",
			config.BaseURL, config.Format)
	case config.Provisioned():
		return fmt.Sprintf("endpoint=%s upload=%s auth=attach (xtoken_id provisioned)",
			config.BaseURL, config.Format)
	default:
		return fmt.Sprintf("endpoint=%s upload=%s auth=attach (no session, xtoken_id is empty; "+
			"the service will refuse this device with 4002 until session_id/session_secret are set)",
			config.BaseURL, config.Format)
	}
}

// --- events -----------------------------------------------------------------

func (a *app) enqueue(activation *capsula.Activation) {
	if activation == nil {
		return
	}
	if activation.ID == "" {
		log.Printf("marusia: activation without an id dropped")
		return
	}
	job := activationJob{activation: activation}
	if activation.ExpiresMonotonicNS > activation.MonotonicTimeNS {
		job.deadline = time.Now().Add(time.Duration(activation.ExpiresMonotonicNS - activation.MonotonicTimeNS))
		job.hasDeadline = true
	}
	select {
	case a.queue <- job:
		log.Printf("marusia: activation=%s origin=%s queued", activation.ID, activation.Origin)
	default:
		log.Printf("marusia: activation=%s origin=%s dropped; a turn is already pending",
			activation.ID, activation.Origin)
	}
}

func (a *app) releaseInput(released *capsula.InputReleased) {
	if released == nil || released.ActivationID == "" {
		return
	}
	a.mu.Lock()
	if a.active != nil && a.active.activationID == released.ActivationID {
		active := a.active
		a.mu.Unlock()
		active.signalRelease()
		return
	}
	a.pendingInput = released.ActivationID
	a.mu.Unlock()
}

func (a *app) applySettings(changed *capsula.SettingsChanged) {
	values, err := settings.Read(changed)
	if err != nil {
		log.Printf("marusia: settings rejected: %v", err)
		return
	}
	config, warnings, err := settings.Build(values, a.fallback)
	if err != nil {
		log.Printf("marusia: settings rejected: %v", err)
		return
	}
	for _, warning := range warnings {
		log.Printf("marusia: %s", warning)
	}
	a.mu.Lock()
	a.values = values
	a.cfg = config
	a.mu.Unlock()
	a.logSerialSource(config)
	log.Printf("marusia: settings applied; %s", a.summary())
}

func (a *app) action(action *capsula.ActionInvoked) {
	if action == nil {
		return
	}
	switch action.ActionID {
	case dismissActionID:
		a.dismiss()
		_ = a.client.NotifyActionResult(action.ID, true, "turn dismissed")
	case reauthActionID:
		go a.reauth(action)
	default:
		log.Printf("marusia: unknown action %q", action.ActionID)
	}
}

func (a *app) dismiss() {
	a.mu.Lock()
	active := a.active
	a.pendingInput = ""
	a.mu.Unlock()
	if active == nil || active.cancel == nil {
		log.Printf("marusia: dismiss ignored; no active turn")
		return
	}
	log.Printf("marusia: activation=%s dismissed", active.activationID)
	active.cancel()
	for {
		select {
		case job := <-a.queue:
			log.Printf("marusia: activation=%s dropped; dismissed", job.activation.ID)
		default:
			return
		}
	}
}

// reauth runs the attach handshake with the current settings and reports the
// outcome through the action result. It uses its own short-lived client so it
// can never close the channel an active turn is using.
func (a *app) reauth(action *capsula.ActionInvoked) {
	config := a.config()
	resolved, source, resolveErr := resolveSession(config)
	config = resolved
	if resolveErr != nil {
		log.Printf("marusia: reauth: %v", resolveErr)
	}
	session := vkclient.Session{SessionID: config.SessionID, SessionSecret: config.SessionSecret}
	protocol, err := vkclient.New(vkclient.Config{
		BaseURL:       config.BaseURL,
		XTokenID:      config.XTokenID,
		Serial:        config.Serial,
		DeviceID:      config.DeviceID,
		Session:       session,
		ClientVersion: config.ClientVersion,
	})
	if err != nil {
		log.Printf("marusia: reauth: %v", err)
		_ = a.client.NotifyActionResult(action.ID, false, err.Error())
		return
	}
	defer protocol.Close()
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()

	if session.Present() {
		// A configured session is validated, never replaced by an attach: the
		// attach needs a manufacturing credential this device does not hold.
		if err := protocol.Authorize(ctx); err != nil {
			log.Printf("marusia: reauth failed: %s", describeReauth(err))
			_ = a.client.NotifyActionResult(action.ID, false, describeReauth(err))
			return
		}
		log.Printf("marusia: reauth succeeded with the session from %s; account=%s",
			source, redactAccount(session.AccountID))
		_ = a.client.NotifyActionResult(action.ID, true, "session accepted from "+source)
		return
	}

	attached, err := protocol.Attach(ctx)
	if err != nil {
		log.Printf("marusia: reauth failed: %s", describeReauth(err))
		_ = a.client.NotifyActionResult(action.ID, false, describeReauth(err))
		return
	}
	log.Printf("marusia: reauth succeeded with the provisioning attach; session=%s account=%s",
		attached.SessionID, attached.AccountID)
	_ = a.client.NotifyActionResult(action.ID, true, "session attach succeeded")
}

// describeReauth is the one line an operator sees for a failed reauth. It names
// the cause without echoing any credential.
func describeReauth(err error) string {
	if vkclient.ProvisioningRequired(err) {
		return "the service requires provisioning (4002: xtoken_id is required); set " +
			"session_id and session_secret instead of attaching"
	}
	var attachErr *vkclient.AttachError
	if errors.As(err, &attachErr) {
		return fmt.Sprintf("the service rejected the attach with HTTP %d", attachErr.Status)
	}
	return err.Error()
}

// redactAccount keeps an account id useful for correlation without logging the
// whole value.
func redactAccount(account string) string {
	if len(account) <= 8 {
		return "…"
	}
	return account[:8] + "…"
}

func (a *app) shutdown() {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active != nil && active.cancel != nil {
		active.cancel()
	}
	// Give the active turn its teardown before the protocol channel goes away:
	// closing the channel under a live stream would turn a clean stop into a
	// transport error.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		busy := a.active != nil
		a.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.mu.Lock()
	protocol := a.protocol
	a.protocol = nil
	a.mu.Unlock()
	if protocol != nil {
		_ = protocol.Close()
	}
}

// --- one turn ---------------------------------------------------------------

func (a *app) runQueue() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case job := <-a.queue:
			if job.hasDeadline && time.Now().After(job.deadline) {
				log.Printf("marusia: activation=%s dropped; it expired while queued", job.activation.ID)
				continue
			}
			a.runActivation(job.activation)
		}
	}
}

func (a *app) runActivation(activation *capsula.Activation) {
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	active := &activeTurn{activationID: activation.ID, cancel: cancel, release: make(chan struct{})}
	a.mu.Lock()
	a.active = active
	pending := a.pendingInput
	a.pendingInput = ""
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.active == active {
			a.active = nil
		}
		a.mu.Unlock()
	}()
	if pending == activation.ID {
		// A very quick hold-to-talk press can be released before the turn
		// starts; honour it instead of listening for the full window.
		active.signalRelease()
	}

	ring, err := a.openRing(ctx)
	if err != nil {
		log.Printf("marusia: activation=%s: %v", activation.ID, err)
		return
	}
	defer ring.Close()
	ring.Show(turn.StateThinking)

	config := a.config()
	protocol, err := a.protocolFor(config)
	if err != nil {
		log.Printf("marusia: activation=%s: %v", activation.ID, err)
		ring.Show(turn.StateError)
		sleep(ctx, errorIndication)
		return
	}

	conversation, conversationID := a.beginConversation(ctx, activation.ID)
	if conversation != nil {
		defer conversation.Close()
	}

	mic, err := a.client.OpenMic(ctx, activation.ID, conversationID)
	if err != nil {
		log.Printf("marusia: activation=%s microphone: %v", activation.ID, err)
		ring.Show(turn.StateError)
		sleep(ctx, errorIndication)
		return
	}
	// Safety net: every path closes the capture, including a panic above.
	defer func() { _ = mic.Close() }()

	opened := make(chan struct{})
	var openOnce sync.Once
	var preOpenDropped atomic.Int64
	chunks := make(chan []byte, captureQueue)
	pumpDone := make(chan struct{})
	go func() {
		a.pumpMic(ctx, mic.Chunks(), chunks, opened, &preOpenDropped)
		close(pumpDone)
	}()

	hold := activation.Origin == capsula.ActivationOriginTouch && activation.HoldToTalk
	speech := speechOptionsFor(config, activation)
	mode := "touch"
	if keywordEvidence(activation) {
		mode = "keyword"
	}
	// One line per turn: with kw_params the service requires the utterance to
	// begin with the keyword, without it the command itself is expected first.
	log.Printf("marusia: phrase mode=%s", mode)
	result := turn.Run(ctx, turn.Options{
		Endpoint:   endpoint{client: protocol},
		Speech:     speech,
		Chunks:     chunks,
		Release:    active.release,
		HoldToTalk: hold,
		PhraseOpened: func() {
			openOnce.Do(func() { close(opened) })
		},
		PreOpenDrops: func() int { return int(preOpenDropped.Load()) },
		Player:       &speaker{client: a.client, fetch: a.fetch, conversationID: conversationID},
		Indicator:    ring,
		Logf:         log.Printf,
	})

	// Release the microphone before anything can be shown for the terminal
	// state: a failed or answered turn must not keep capturing, and the audio
	// buffered for this turn must never reach the next activation.
	closeMic(ctx, mic, pumpDone)

	switch result.Outcome {
	case turn.Failed:
		log.Printf("marusia: activation=%s failed: %s", activation.ID, result.Detail)
		ring.Show(turn.StateError)
		sleep(ctx, errorIndication)
	case turn.NoAnswer, turn.NoAudio:
		log.Printf("marusia: activation=%s ended: %s", activation.ID, result.Detail)
		ring.Show(turn.StateError)
		sleep(ctx, errorIndication)
	case turn.Dismissed:
		log.Printf("marusia: activation=%s dismissed", activation.ID)
	case turn.NoSpeech:
		log.Printf("marusia: activation=%s ended: %s", activation.ID, result.Detail)
	case turn.Completed:
		log.Printf("marusia: activation=%s completed text=%q played=%q",
			activation.ID, result.Text, result.PlayedURL)
	}
	if result.AuthRejected {
		a.dropSession(protocol)
	}
}

// speechOptionsFor builds one turn's init fields. kw_params is only sent when
// the activation carries wake-word evidence: a matched wake-word model, or an
// origin other than touch (a touch activation is the user pressing the talk
// key, so the utterance begins with the command and the service must not
// demand the keyword). media_state is always set inside the block because the
// service refuses an unspecified state.
func speechOptionsFor(config settings.Config, activation *capsula.Activation) vkclient.SpeechOptions {
	options := vkclient.SpeechOptions{
		Format:           config.Format,
		Timezone:         timezone(),
		HWID:             config.HWID,
		MediaState:       clientpb.MediaState_MEDIA_STATE_STOP,
		StatusIntervalMS: 1000,
	}
	if keywordEvidence(activation) {
		options.Keyword = config.Keyword
	}
	return options
}

// keywordEvidence reports whether the activation was produced by a wake word.
// Only that origin carries keyword evidence: a key press and an API activation
// both start a phrase the service must transcribe without a keyword, and
// claiming one makes the service answer "keyword not confirmed" instead.
func keywordEvidence(activation *capsula.Activation) bool {
	return activation != nil && activation.Origin == capsula.ActivationOriginWakeword
}

// closeMic ends the capture and waits for both the framework stream and the
func closeMic(ctx context.Context, mic *capsula.Mic, pumpDone <-chan struct{}) {
	_ = mic.Close()
	select {
	case <-mic.Done():
	case <-ctx.Done():
		return
	}
	select {
	case <-pumpDone:
	case <-ctx.Done():
	}
}

// dropSession discards rejected credentials. The next turn re-resolves them in
// the documented order (masked settings, then the vendor session file, then
// attach), so a settings push or a refreshed file is picked up without a
// restart.
func (a *app) dropSession(protocol *vkclient.Client) {
	a.mu.Lock()
	if a.protocol == protocol {
		a.protocol = nil
		a.protocolKey = protocolKey{}
	}
	config := a.cfg
	a.mu.Unlock()
	if protocol != nil {
		protocol.InvalidateSession()
		_ = protocol.Close()
	}
	log.Printf("marusia: the service rejected the session; it has been dropped and will be "+
		"re-resolved from settings, then %s, on the next activation", config.SessionFile)
}

// resolveSession applies the documented credential order: the explicit masked
// settings first, then the vendor agent's own state file, then no session at
// all (the provisioning attach). It returns the configuration with the resolved
// credentials and a label naming the source, or "" when there is none.
//
// The device's vendor state directory is root-only, so a sandboxed app usually
// cannot read the file: the settings are the supported path and the file is a
// best-effort fallback for a development run outside the sandbox.
func resolveSession(config settings.Config) (settings.Config, string, error) {
	if config.HasSession() {
		return config, "settings", nil
	}
	vendor, err := settings.LoadVendorSession(config.SessionFile)
	if err != nil {
		return config, "", err
	}
	if vendor.SessionID == "" || vendor.Secret == "" {
		return config, "", nil
	}
	config.SessionID = vendor.SessionID
	config.SessionSecret = vendor.Secret
	if config.DeviceID == "" {
		config.DeviceID = vendor.DeviceID
	}
	return config, "vendor session file", nil
}

func (a *app) protocolFor(config settings.Config) (*vkclient.Client, error) {
	resolved, source, resolveErr := resolveSession(config)
	config = resolved
	if resolveErr != nil {
		log.Printf("marusia: %v; the provisioning attach will be attempted", resolveErr)
	}
	key := protocolKey{
		base: config.BaseURL, xtoken: config.XTokenID, serial: config.Serial,
		deviceID: config.DeviceID, version: config.ClientVersion,
		sessionID: config.SessionID, sessionSecret: config.SessionSecret,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.protocol != nil && a.protocolKey == key {
		return a.protocol, nil
	}
	if a.protocol != nil {
		_ = a.protocol.Close()
		a.protocol = nil
	}
	client, err := vkclient.New(vkclient.Config{
		BaseURL:       config.BaseURL,
		XTokenID:      config.XTokenID,
		Serial:        config.Serial,
		DeviceID:      config.DeviceID,
		Session:       vkclient.Session{SessionID: config.SessionID, SessionSecret: config.SessionSecret},
		ClientVersion: config.ClientVersion,
		OnSession:     a.logSession,
	})
	if err != nil {
		return nil, err
	}
	a.protocol = client
	a.protocolKey = key
	if source != "" {
		a.sessionOnce.Do(func() {
			log.Printf("marusia: using the session from %s for %s; no attach is attempted",
				source, config.BaseURL)
		})
	}
	return client, nil
}

func (a *app) beginConversation(ctx context.Context, activationID string) (*capsula.Conversation, string) {
	conversation, err := a.client.BeginConversation(ctx, activationID)
	if err != nil {
		// Capture and playback still work; the mixer will not duck others.
		log.Printf("marusia: activation=%s conversation: %v", activationID, err)
		return nil, ""
	}
	return conversation, conversation.ID()
}

// pumpMic forwards captured PCM to the turn as 16 kHz mono s16le. It never
// blocks on the reader: when the queue is full the oldest chunk is dropped and
// the gap is logged. It returns when the microphone stream or the turn ends;
// chunks lost before the phrase opened are counted into preOpenDropped, which
// the turn's one-line audio summary reports.
func (a *app) pumpMic(ctx context.Context, in <-chan capsula.MicChunk, out chan []byte,
	opened <-chan struct{}, preOpenDropped *atomic.Int64) {
	// The SDK attaches the capture format to every chunk, so the format
	// announcement is logged once per stream instead of once per chunk.
	var (
		format         capsula.AudioFormat
		haveFormat     bool
		formatWarned   bool
		loggedStreamID string
		formatLogged   bool
		dropped        int
	)
	defer close(out)
	phraseOpen := func() bool {
		select {
		case <-opened:
			return true
		default:
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-in:
			if !ok {
				return
			}
			if chunk.Format != nil && (!formatLogged || chunk.StreamID != loggedStreamID) {
				format = *chunk.Format
				haveFormat = true
				loggedStreamID = chunk.StreamID
				formatLogged = true
				log.Printf("marusia: microphone format %s %d Hz/%d ch", format.SampleFormat,
					format.SampleRateHz, format.Channels)
			}
			if chunk.Notice != nil {
				log.Printf("marusia: microphone notice %s: %s", chunk.Notice.Code, chunk.Notice.Detail)
				continue
			}
			if chunk.Chunk == nil || len(chunk.Chunk.PCM) == 0 {
				continue
			}
			if !haveFormat {
				format = capsula.DefaultAudioFormat()
				haveFormat = true
			}
			if format.SampleFormat != capsula.SampleFormatPCM16LE {
				if !formatWarned {
					formatWarned = true
					log.Printf("marusia: microphone sample format %s is not PCM s16le; audio is being dropped",
						format.SampleFormat)
				}
				continue
			}
			pcm := chunk.Chunk.PCM
			if format.Channels != 1 || format.SampleRateHz != uint32(vkclient.SampleRate) {
				pcm = media.ToMono16k(pcm, int(format.SampleRateHz), int(format.Channels))
			}
			if len(pcm) == 0 {
				continue
			}
			select {
			case out <- pcm:
			default:
				// Drop the oldest frame, then retry once.
				select {
				case <-out:
				default:
				}
				select {
				case out <- pcm:
				default:
				}
				dropped++
				if !phraseOpen() {
					// Loss before the server opened the phrase is speaker audio
					// the service never saw; a later drop is ordinary
					// backpressure and the turn reports it above.
					if preOpenDropped != nil {
						preOpenDropped.Add(1)
					}
				}
				if dropped == 1 || dropped%50 == 0 {
					log.Printf("marusia: microphone queue full; dropped %d chunk(s)", dropped)
				}
			}
		}
	}
}

// --- adapters ---------------------------------------------------------------

// endpoint adapts the protocol client to turn.Endpoint.
type endpoint struct{ client *vkclient.Client }

func (e endpoint) Authorize(ctx context.Context) error { return e.client.Authorize(ctx) }

func (e endpoint) OpenSpeech(ctx context.Context, opts vkclient.SpeechOptions) (turn.Stream, error) {
	return e.client.OpenSpeech(ctx, opts)
}

// speaker fetches a play_url body, decodes it and writes PCM to the framework
// mixer. It closes the playback stream on every path; a write failure aborts.
type speaker struct {
	client         *capsula.Client
	fetch          *media.Fetch
	conversationID string
}

func (s *speaker) Play(ctx context.Context, url string) error {
	body, err := s.fetch.Fetch(ctx, url)
	if err != nil {
		return err
	}
	pcm, err := media.DecodeToPlayback(body)
	if err != nil {
		return err
	}
	if len(pcm) == 0 {
		return errors.New("the reply audio decoded to zero samples")
	}
	playback, err := s.client.OpenPlayback(ctx, outputEndpoint, capsula.AudioFormat{
		SampleFormat: capsula.SampleFormatPCM16LE,
		SampleRateHz: vkclient.SampleRate,
		Channels:     1,
	}, s.conversationID)
	if err != nil {
		return fmt.Errorf("open playback: %w", err)
	}
	defer playback.Close()

	block := playbackWriteMS * vkclient.SampleRate / 1000 * 2
	for offset := 0; offset < len(pcm); offset += block {
		if err := ctx.Err(); err != nil {
			_ = playback.Abort()
			return err
		}
		end := offset + block
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := playback.Write(pcm[offset:end]); err != nil {
			_ = playback.Abort()
			return fmt.Errorf("write playback: %w", err)
		}
	}
	return nil
}

// --- ring -------------------------------------------------------------------

var (
	ringTeal   = capsula.RGB{G: 128, B: 128}
	ringBlue   = capsula.RGB{G: 64, B: 255}
	ringGreen  = capsula.RGB{G: 220}
	ringRed    = capsula.RGB{R: 255}
	ringPixels = 12
)

// ringView renders the visible turn indication. The framework requires a
// visible, non-blank presentation refreshed at least every 2 s for capture, and
// this renderer writes a frame at 25 Hz for the whole turn.
type ringView struct {
	pres    *capsula.Presentation
	capture *capsula.Capture

	mu    sync.Mutex
	state turn.State

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	pixels    []capsula.RGB
}

func (a *app) openRing(ctx context.Context) (*ringView, error) {
	pres, err := a.client.OpenPresentation(ctx, ringResource)
	if err != nil {
		return nil, fmt.Errorf("ring unavailable: %w", err)
	}
	capture, err := a.client.OpenCapture(ctx, ringResource, pres.ID(), capsula.CaptureModeUnspecified)
	if err != nil {
		_ = pres.Close()
		return nil, fmt.Errorf("ring capture: %w", err)
	}
	view := &ringView{
		pres:    pres,
		capture: capture,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		pixels:  make([]capsula.RGB, ringPixels),
	}
	if err := view.renderFrame(); err != nil {
		capture.Close()
		_ = pres.Close()
		return nil, fmt.Errorf("ring frame: %w", err)
	}
	go view.render()
	return view, nil
}

func (v *ringView) Show(state turn.State) {
	v.mu.Lock()
	v.state = state
	v.mu.Unlock()
}

func (v *ringView) render() {
	defer close(v.done)
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-ticker.C:
			if err := v.renderFrame(); err != nil {
				return
			}
		}
	}
}

func (v *ringView) renderFrame() error {
	v.mu.Lock()
	state := v.state
	v.mu.Unlock()
	phase := float64(time.Now().UnixNano()%int64(time.Second)) / float64(time.Second)
	for i := range v.pixels {
		switch state {
		case turn.StateListening:
			pulse := 0.35 + 0.35*(1+math.Sin(2*phase))/2
			v.pixels[i] = scaleRGB(ringTeal, pulse)
		case turn.StateThinking:
			head := int(phase*float64(ringPixels)) % ringPixels
			brightness := 0.12
			if i == head {
				brightness = 0.9
			}
			v.pixels[i] = scaleRGB(ringBlue, brightness)
		case turn.StateSpeaking:
			head := int(phase*2*float64(ringPixels)) % ringPixels
			brightness := 0.2
			if i == head || (i+1)%ringPixels == head {
				brightness = 1.0
			}
			v.pixels[i] = scaleRGB(ringGreen, brightness)
		case turn.StateError:
			brightness := 0.1
			if phase < 0.5 {
				brightness = 1.0
			}
			v.pixels[i] = scaleRGB(ringRed, brightness)
		}
	}
	return v.pres.Ring(v.pixels)
}

func (v *ringView) Close() {
	v.closeOnce.Do(func() {
		close(v.stop)
		<-v.done
		v.capture.Close()
		_ = v.pres.Blank()
		_ = v.pres.Close()
	})
}

func scaleRGB(color capsula.RGB, brightness float64) capsula.RGB {
	clamp := func(v float64) uint8 {
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		return uint8(v)
	}
	return capsula.RGB{
		R: clamp(float64(color.R) * brightness),
		G: clamp(float64(color.G) * brightness),
		B: clamp(float64(color.B) * brightness),
	}
}

// --- small helpers ----------------------------------------------------------

// timezone returns the IANA zone name when the sandbox exposes one. The vendor
// fills client_options.timezone; an abbreviation such as MSK would not identify
// the zone, so /etc/timezone and TZ are preferred and the abbreviation is last.
func timezone() string {
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	if name := strings.TrimSpace(os.Getenv("TZ")); name != "" {
		return name
	}
	if name, _ := time.Now().Zone(); name != "" {
		return name
	}
	return "UTC"
}

func sleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
