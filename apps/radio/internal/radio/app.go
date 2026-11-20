// Package radio is the Internet Radio application: station settings, the
// in-process MP3 stream, the speaker stream, the segment presentation and the
// standard media contract.
//
// One native app owns the whole radio. Station audio is fetched over the
// shared capsulad transport, decoded in this process and written to the
// framework speaker; there is no companion app and no app-to-app audio hop.
//
// A run exists only while it has audio or a live indication. A bare launch
// waits one idle window for Play, a pause holds its pause sign for one minute,
// and a terminal error holds its dashes for its display window; each then
// releases the presentation and exits. Resuming or retuning supersedes the
// armed expiry by generation, so a stale timer can never end a newer stream.
package radio

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"capsulad/sdk/capsulad"
)

// Framework and contract names used by the app.
const (
	mediaService  = "capsulad.contracts.media.v1.MediaPlayerService"
	configService = "capsulad.framework.v1.ApplicationConfigurationService"

	segmentResource = "display.segment.front"
	speakerEndpoint = "audio.output.speaker"

	playerID = "radio"
	appName  = "Internet radio"

	noAudioTimeout = 60 * time.Second
	watchInterval  = 500 * time.Millisecond

	// idleExitWindow bounds a run that has no audio and no pending display
	// indication: a bare launch waiting for a play, or a terminal error whose
	// "Always display" indication has run for a minute. The radio does not
	// stay resident between listens.
	idleExitWindow = time.Minute

	// pauseHoldWindow is how long a paused radio keeps the pause sign on the
	// segment display and stays alive for a resume. It is deliberately a
	// minute, not indefinite: the next launch is the recovery path.
	pauseHoldWindow = time.Minute
)

// DefaultStations is the manifest default station list. It is also the
// in-memory fallback until the first settings snapshot arrives, so a launch
// invocation that races the settings push still has a usable selection.
const DefaultStations = "# One station per line: Name | stream URL\n" +
	"SomaFM Groove Salad | https://ice1.somafm.com/groovesalad-128-mp3\n" +
	"Radio Paradise Main Mix | https://stream.radioparadise.com/mp3-192"

// Phase is the user-visible radio state.
type Phase int

const (
	PhaseStopped Phase = iota
	PhaseBuffering
	PhasePlaying
	PhasePaused
	PhaseError
)

func (p Phase) String() string {
	switch p {
	case PhaseBuffering:
		return "buffering"
	case PhasePlaying:
		return "playing"
	case PhasePaused:
		return "paused"
	case PhaseError:
		return "error"
	default:
		return "stopped"
	}
}

// mediaState maps the radio phase to capsulad.contracts.media.v1.PlaybackState.
func (p Phase) mediaState() string {
	switch p {
	case PhaseBuffering:
		return "PLAYBACK_BUFFERING"
	case PhasePlaying:
		return "PLAYBACK_PLAYING"
	case PhasePaused:
		return "PLAYBACK_PAUSED"
	default:
		return "PLAYBACK_STOPPED"
	}
}

// App owns the radio state. Its mutex protects the state copied into media
// snapshots, settings reconciliation and session ownership; blocking I/O
// (network, playback, presentations) always happens outside it.
type App struct {
	ctx    context.Context
	cancel context.CancelFunc
	client *capsulad.Client
	env    capsulad.Env
	logf   func(string, ...any)

	// display is the presentation/capture seam used by displayLoop.
	display displayHost

	mu               sync.Mutex
	phase            Phase
	detail           string
	stations         []Station
	stationNumber    int
	autoplay         bool
	alwaysDisplay    bool
	displayUntil     time.Time
	displayWindow    time.Duration
	pauseWindow      time.Duration
	idleWindow       time.Duration
	now              func() time.Time
	settingsRevision uint64
	// persistedStation is the station the store is known to hold. Zero means
	// unknown: no snapshot has arrived yet, or a compare-and-swap write may
	// have committed without an acknowledgment.
	persistedStation int
	// unsettledRevision is the expected revision of the newest write whose
	// commit outcome is not known: either it is still in flight, or its RPC
	// failed or was cancelled after the request reached the daemon. A
	// snapshot at or below this revision predates that possible commit and
	// cannot say which station is stored, so it must not clear a selection or
	// restore persistedStation. A later snapshot proves the revision moved
	// past the attempt, and the write can no longer commit.
	unsettledRevision uint64
	// selectionDirty marks a selection the user explicitly accepted (a
	// next/previous step or a named launch) that is not yet confirmed durable.
	// A settings snapshot only reports the stored value; it must never make
	// shutdown flush an untouched launch default over that stored value.
	selectionDirty   bool
	pendingStations  map[int]int
	recentWrites     map[uint64]int
	settingsSeen     bool
	settingsOnce     sync.Once
	settingsReady    chan struct{}
	session          *session
	sessionSeq       uint64
	sessionURL       string
	nowTitle         string
	nowArtist        string
	revision         uint64
	speakerAvailable bool

	watchers  map[uint64]chan struct{}
	nextWatch uint64

	drawCh      chan struct{}
	displayDone chan struct{}

	// idle exit: a single armed expiry driven by exitLoop. Every transition
	// that must win over it (resume, retune, stop, shutdown) bumps exitSeq.
	exitCh   chan struct{}
	exitOnce sync.Once
	exitWake chan struct{}
	exitSeq  uint64
	exitAt   time.Time
	exitWhy  string
	exiting  bool

	// halted distinguishes the playback Stop hold from Pause for the media
	// contract: both show the pause sign and keep the run resumable, but a
	// stop must not claim PLAYBACK_PAUSED.
	halted bool

	// settingsRetuneGen is the generation of the newest applied settings
	// snapshot. A retune queued by one snapshot is dropped if a newer snapshot
	// has since been applied; the newer snapshot queues its own retune if one
	// is still needed.
	settingsRetuneGen uint64

	// updateStationFn is the settings write seam. It is nil in production,
	// where the app bus performs the compare-and-swap write; tests substitute
	// it to exercise the bounded shutdown flush without a daemon.
	updateStationFn func(ctx context.Context, station int, revision uint64) (uint64, error)

	// readRevisionFn is the settings read seam used by stale-revision
	// reconciliation. It is nil in production, where refreshRevision asks the
	// configuration service.
	readRevisionFn func(ctx context.Context) (uint64, bool)

	// persistSampledFn is a test seam invoked with a.mu held after a writer
	// samples the selection and before it skips or registers a write. Tests
	// use it to prove no scheduling window exists between those steps.
	persistSampledFn func()

	// persistGate serializes settings writes without an uninterruptible
	// mutex: a shutdown flush must be able to stop waiting at its own bounded
	// deadline even if an earlier write is still in flight.
	persistGateOnce sync.Once
	persistGate     chan struct{}
}

// New returns an idle radio bound to client. ctx is the app lifetime.
func New(ctx context.Context, client *capsulad.Client, env capsulad.Env, logf func(string, ...any)) *App {
	if logf == nil {
		logf = log.Printf
	}
	appCtx, cancel := context.WithCancel(ctx)
	a := &App{
		ctx:              appCtx,
		cancel:           cancel,
		client:           client,
		env:              env,
		logf:             logf,
		display:          clientDisplayHost{client: client},
		phase:            PhaseStopped,
		stations:         ParseStations(DefaultStations, logf),
		stationNumber:    1,
		pendingStations:  make(map[int]int),
		recentWrites:     make(map[uint64]int),
		settingsReady:    make(chan struct{}),
		speakerAvailable: true,
		watchers:         make(map[uint64]chan struct{}),
		drawCh:           make(chan struct{}, 1),
		displayDone:      make(chan struct{}),
		exitCh:           make(chan struct{}),
		exitWake:         make(chan struct{}, 1),
	}
	a.mu.Lock()
	a.armExitLocked(a.idleHold(), "idle")
	a.mu.Unlock()
	go a.displayLoop()
	go a.watchdogLoop()
	go a.exitLoop()
	return a
}

// Done closes when the radio has nothing left to do and the process should end
// cleanly. It never closes while audio is playing or buffering; a later launch
// starts a fresh run.
func (a *App) Done() <-chan struct{} { return a.exitCh }

func (a *App) idleHold() time.Duration {
	if a.idleWindow > 0 {
		return a.idleWindow
	}
	return idleExitWindow
}

func (a *App) pauseHold() time.Duration {
	if a.pauseWindow > 0 {
		return a.pauseWindow
	}
	return pauseHoldWindow
}

// armExitLocked schedules this run to end at now+d unless a newer transition
// supersedes it. Every idle path goes through this one scheduler: a bare
// launch, a playback stop or pause that is waiting for a resume, and a
// terminal error whose indication is still visible. The generation is checked
// when the timer fires, so a resume, a retune or a newer terminal state always
// wins over a stale expiry. The caller holds a.mu.
func (a *App) armExitLocked(d time.Duration, why string) {
	if d < 0 {
		d = 0
	}
	a.exitSeq++
	a.exitAt = a.clockNow().Add(d)
	a.exitWhy = why
	a.wakeExit()
}

// cancelExitLocked supersedes any armed expiry. Every session start calls it,
// so resuming a paused radio or retuning cancels the wait instead of racing
// it. A stale timer wakes to a mismatched generation and does nothing. The
// caller holds a.mu.
func (a *App) cancelExitLocked() {
	a.exitSeq++
	a.exitAt = time.Time{}
	a.exitWhy = ""
}

func (a *App) wakeExit() {
	select {
	case a.exitWake <- struct{}{}:
	default:
	}
}

// exitLoop is the single idle-expiry timer. It replaces any earlier deadline
// on a wake, so a busy client cannot accumulate one goroutine per step, and it
// passes the generation to expireRun so a timer armed before a resume cannot
// end the newer session. A wake can arrive late or twice; the timer is always
// computed from the armed absolute deadline, never restarted at full length,
// so the display release and the run exit cannot drift apart.
func (a *App) exitLoop() {
	for {
		if a.ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		gen, at := a.exitSeq, a.exitAt
		exiting := a.exiting
		a.mu.Unlock()
		if exiting {
			return
		}
		if at.IsZero() {
			select {
			case <-a.ctx.Done():
				return
			case <-a.exitWake:
				continue
			}
		}
		remaining := at.Sub(a.clockNow())
		if remaining <= 0 {
			a.expireRun(gen)
			continue
		}
		timer := time.NewTimer(remaining)
		select {
		case <-a.ctx.Done():
			timer.Stop()
			return
		case <-a.exitWake:
			timer.Stop()
		case <-timer.C:
			a.expireRun(gen)
		}
	}
}

// expireRun ends the run only while gen still owns the idle state. A late
// timer must never stop audio started by a resume or a retune. The terminal
// state is published before the process is asked to leave, and the display
// loop releases its capture and presentation through Close.
func (a *App) expireRun(gen uint64) {
	a.mu.Lock()
	if gen != a.exitSeq || a.exitAt.IsZero() {
		a.mu.Unlock()
		return
	}
	if a.phase == PhasePlaying || a.phase == PhaseBuffering {
		a.cancelExitLocked()
		a.mu.Unlock()
		return
	}
	why := a.exitWhy
	a.exitAt, a.exitWhy = time.Time{}, ""
	a.exiting = true
	a.halted = false
	if a.phase != PhaseStopped {
		a.phase = PhaseStopped
		a.detail = why
		a.sessionURL = ""
		a.nowTitle, a.nowArtist = "", ""
		a.revision++
		a.signalWatchersLocked()
	}
	a.mu.Unlock()
	a.requestDraw()
	a.requestExit(why)
}

func (a *App) requestExit(why string) {
	a.exitOnce.Do(func() {
		a.logf("exiting run: %s", why)
		close(a.exitCh)
	})
}

// armErrorExitLocked bounds a terminal failure. The error pattern follows the
// tuning window when "Always display station number" is off; with it on the
// pattern lasts at most one idle window. When that indication ends the run
// exits instead of lingering as a silent process. The caller holds a.mu.
func (a *App) armErrorExitLocked() {
	hold := a.idleHold()
	if !a.alwaysDisplay {
		hold = a.displayUntil.Sub(a.clockNow())
	}
	if hold < 0 {
		hold = 0
	}
	a.armExitLocked(hold, "error")
}

// WaitSettings waits for the first app-settings snapshot.
func (a *App) WaitSettings(ctx context.Context, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-a.settingsReady:
		return true
	case <-ctx.Done():
		return false
	case <-a.ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// HandleSettings applies one app-private settings snapshot. It runs on the SDK
// event thread, so it only updates memory and starts reconciliation in a
// goroutine.
func (a *App) HandleSettings(event *capsulad.SettingsChanged) {
	if event == nil {
		return
	}
	var stationsText *string
	var station *int
	var autoplay *bool
	var alwaysDisplay *bool

	a.mu.Lock()
	if event.Revision != 0 && event.Revision < a.settingsRevision {
		a.mu.Unlock()
		return
	}
	if event.Revision != 0 {
		a.settingsRevision = event.Revision
		if a.unsettledRevision != 0 && event.Revision > a.unsettledRevision {
			// The store revision moved past the expected revision of a write
			// whose RPC outcome was lost. That write can no longer commit, so
			// this snapshot is authoritative for the station below.
			a.unsettledRevision = 0
		}
	}
	first := !a.settingsSeen
	a.settingsSeen = true
	for _, value := range event.Values {
		switch value.Key {
		case "stations":
			if text, ok := decodeString(value.ValueJSON); ok {
				stationsText = &text
			}
		case "station":
			if number, ok := decodeInt(value.ValueJSON); ok {
				station = &number
			}
		case "autoplay":
			if enabled, ok := decodeBool(value.ValueJSON); ok {
				autoplay = &enabled
			}
		case "always_display":
			if enabled, ok := decodeBool(value.ValueJSON); ok {
				alwaysDisplay = &enabled
			}
		}
	}
	if stationsText != nil {
		a.stations = ParseStations(*stationsText, a.logf)
	}
	if station != nil {
		if *station < 1 || *station > MaxStations {
			a.logf("station setting %d is outside the supported range 1..%d; keeping the previous selection", *station, MaxStations)
		} else if a.unsettledRevision != 0 {
			// This snapshot predates, or raced, a write whose acknowledgment
			// may have been lost. The daemon could still commit another
			// station after this snapshot was produced, so keep the accepted
			// selection dirty and leave persistedStation unknown; the write
			// response or the shutdown flush settles which value is stored.
		} else if a.selectionDirty && *station != a.stationNumber {
			// This snapshot predates an explicit next/previous or named
			// launch. Keep the newer selection and remember the stored value
			// it is replacing; the pending write records the selection once a
			// compare-and-swap revision is available.
			a.persistedStation = *station
		} else if a.stationEchoLocked(*station, event.Revision) {
			// This snapshot reflects a write this app made for an older
			// selection; a newer next/previous must not be reverted by it.
			a.persistedStation = *station
		} else {
			a.stationNumber = *station
			a.persistedStation = *station
			a.selectionDirty = false
		}
	}
	if autoplay != nil {
		a.autoplay = *autoplay
	}
	displayChanged := false
	if alwaysDisplay != nil && a.alwaysDisplay != *alwaysDisplay {
		a.alwaysDisplay = *alwaysDisplay
		displayChanged = true
	}
	// A playback Stop hold is user intent: a settings edit may update the
	// selection, but it must not start audio or cancel the hold without Play.
	active := !a.halted &&
		(a.phase == PhasePlaying || a.phase == PhaseBuffering || a.phase == PhasePaused)
	if displayChanged && !a.exiting && a.phase == PhaseError {
		// A terminal error's run lifetime follows its indication: turning the
		// setting off after an expired tuning window ends the silent run
		// instead of leaving it invisible for the rest of the idle window.
		a.armErrorExitLocked()
	}
	// Every applied snapshot supersedes a retune queued by an earlier one,
	// even when this snapshot does not need a retune itself.
	a.settingsRetuneGen++
	settingsGen := a.settingsRetuneGen
	retune := false
	var guard retuneGuard
	if active {
		selected, ok := a.currentStationLocked()
		retune = !ok || selected.URL != a.sessionURL
		if retune {
			guard = retuneGuard{
				settings: settingsGen,
				state:    a.exitSeq,
				session:  a.session,
			}
		}
	}
	autoplayNow := first && a.autoplay && a.phase == PhaseStopped
	registeredAutoplay := a.autoplay
	persistDirty := a.selectionDirty && a.settingsRevision != 0
	a.revision++
	a.signalWatchersLocked()
	phase := a.phase
	stationNumber := a.stationNumber
	count := len(a.stations)
	a.mu.Unlock()

	a.settingsOnce.Do(func() { close(a.settingsReady) })
	if first {
		a.logf("settings ready revision=%d stations=%d station=%d autoplay=%t",
			event.Revision, count, stationNumber, registeredAutoplay)
	}
	if persistDirty {
		// A step or named launch may have run before the first snapshot
		// supplied a compare-and-swap revision. Now that it has, retry the
		// write instead of waiting for shutdown.
		go a.persistSelection()
	}
	if autoplayNow {
		go a.play("autoplay")
		return
	}
	if retune {
		go a.retuneFromSettings(guard)
	} else if phase == PhaseStopped || displayChanged {
		a.requestDraw()
	}
}

// ProbeCapabilities records whether the device exposes the speaker endpoint.
// A failed lookup is not evidence that playback cannot work, so it leaves the
// optimistic default in place.
func (a *App) ProbeCapabilities(ctx context.Context) {
	endpoints, err := a.client.ListAudioEndpoints(ctx)
	if err != nil {
		a.logf("speaker probe failed: %v", err)
		return
	}
	found := false
	for _, endpoint := range endpoints {
		if endpoint.EndpointID == speakerEndpoint {
			found = true
			break
		}
	}
	a.mu.Lock()
	a.speakerAvailable = found
	a.mu.Unlock()
	if !found {
		a.logf("no %s endpoint on this device; play will report that", speakerEndpoint)
	}
}

// Control applies one declared action. It never blocks on the network.
func (a *App) Control(action string) (bool, string) {
	switch action {
	case "play":
		return a.play("play")
	case "pause":
		return a.pause()
	case "stop":
		return a.stopPlayback()
	case "dismiss":
		return a.stop("dismiss")
	case "next":
		return a.step(1)
	case "previous":
		return a.step(-1)
	default:
		return false, "unknown action " + action
	}
}

func (a *App) play(why string) (bool, string) {
	a.mu.Lock()
	if a.exiting {
		a.mu.Unlock()
		return false, "radio is exiting"
	}
	selected, ok := a.currentStationLocked()
	if !ok {
		a.beginDisplayRunLocked()
		reason := a.missingStationReasonLocked()
		old := a.failLocked(reason)
		a.mu.Unlock()
		if old != nil {
			old.stop()
		}
		a.requestDraw()
		return false, reason
	}
	if !a.speakerAvailable {
		a.beginDisplayRunLocked()
		old := a.failLocked("no speaker endpoint on this device")
		a.mu.Unlock()
		if old != nil {
			old.stop()
		}
		a.requestDraw()
		return false, "no speaker endpoint on this device"
	}
	if (a.phase == PhasePlaying || a.phase == PhaseBuffering) && selected.URL == a.sessionURL {
		a.cancelExitLocked()
		a.mu.Unlock()
		return true, "already playing"
	}
	old := a.session
	s := a.startSessionLocked(selected, why)
	a.mu.Unlock()
	if old != nil {
		old.stop()
	}
	a.requestDraw()
	go s.run()
	return true, "tuning " + selected.Name
}

// pause closes the station stream and keeps the run for a bounded resume
// window: the pause sign owns the display for pauseHoldWindow, and Play within
// it cancels the armed exit. A pause is not a parked buffer.
func (a *App) pause() (bool, string) {
	a.mu.Lock()
	if a.phase != PhasePlaying && a.phase != PhaseBuffering {
		a.mu.Unlock()
		return false, "nothing is playing"
	}
	old := a.session
	a.session = nil
	a.phase = PhasePaused
	a.halted = false
	a.detail = "paused"
	url := a.sessionURL
	hold := a.pauseHold()
	a.displayUntil = a.clockNow().Add(hold)
	a.armExitLocked(hold, "pause expired")
	a.revision++
	a.signalWatchersLocked()
	a.mu.Unlock()
	if old != nil {
		old.stop()
	}
	a.requestDraw()
	a.logf("state paused %s; run exits in %s unless resumed", url, hold)
	return true, "paused"
}

// stopPlayback is the playback Stop action. It closes the station stream like
// Pause but reports PLAYBACK_STOPPED rather than PLAYBACK_PAUSED, shows the
// same pause sign, and arms the same one-minute resume hold. Stop must not
// tear the display down on the first frame: the user may press Play again
// within the hold. The terminal, immediate path belongs to Dismiss and to an
// administrative process stop, not to this action.
func (a *App) stopPlayback() (bool, string) {
	a.mu.Lock()
	switch a.phase {
	case PhasePlaying, PhaseBuffering:
		old := a.session
		a.session = nil
		a.phase = PhasePaused
		a.halted = true
		a.detail = "stopped"
		hold := a.pauseHold()
		a.displayUntil = a.clockNow().Add(hold)
		a.armExitLocked(hold, "stop expired")
		a.revision++
		a.signalWatchersLocked()
		a.mu.Unlock()
		if old != nil {
			old.stop()
		}
		a.requestDraw()
		a.logf("state stopped playback; run exits in %s unless resumed", hold)
		return true, "stopped"
	case PhasePaused:
		// Stop during a pause converts the hold's media state without moving
		// its deadline; pressing Stop twice must not extend the run.
		if !a.halted {
			a.halted = true
			a.detail = "stopped"
			a.revision++
			a.signalWatchersLocked()
			a.requestDraw()
		}
		a.mu.Unlock()
		return true, "stopped"
	}
	a.mu.Unlock()
	// An idle or terminal-error run has no playback to hold: Stop remains the
	// immediate terminal path it was before.
	return a.stop("stop")
}

// stop is the terminal administrative path (Dismiss and process shutdown). It
// releases the display immediately and signals the process to leave, so no
// resident idle state remains. Close performs the matching audio and
// presentation teardown.
func (a *App) stop(why string) (bool, string) {
	a.mu.Lock()
	old := a.session
	a.session = nil
	a.sessionURL = ""
	a.phase = PhaseStopped
	a.halted = false
	a.displayUntil = time.Time{}
	a.detail = reasonText(why)
	detail := a.detail
	a.nowTitle = ""
	a.nowArtist = ""
	a.cancelExitLocked()
	a.exiting = true
	a.revision++
	a.signalWatchersLocked()
	a.mu.Unlock()
	if old != nil {
		old.stop()
	}
	a.requestDraw()
	a.logf("state stopped %s", detail)
	a.requestExit(detail)
	return true, detail
}

func (a *App) step(delta int) (bool, string) {
	a.mu.Lock()
	if len(a.stations) == 0 {
		a.mu.Unlock()
		return false, "no stations configured"
	}
	previous := a.stationNumber
	a.stationNumber = StepNumber(a.stationNumber, delta, len(a.stations))
	if a.stationNumber != previous {
		a.selectionDirty = true
	}
	active := a.phase == PhasePlaying || a.phase == PhaseBuffering ||
		a.phase == PhasePaused || a.phase == PhaseError
	if !active && !a.exiting {
		// A step on an idle radio proves someone is using the run: keep it
		// available for the following Play instead of letting the launch
		// grace expire under the user's finger.
		a.armExitLocked(a.idleHold(), "idle")
	}
	a.revision++
	a.signalWatchersLocked()
	selected, ok := a.currentStationLocked()
	a.mu.Unlock()

	go a.persistSelection()
	if active {
		return a.retune("station changed")
	}
	if !ok {
		return false, a.missingStationReason()
	}
	a.requestDraw()
	return true, "selected " + selected.Name
}

// retuneGuard pins the state that justified a settings-driven retune. The
// settings generation invalidates the work when a newer snapshot is applied;
// state is the idle/exit generation and session is the playback owner captured
// under the same lock. Any Stop, Pause, Play, Dismiss or expiry between the
// snapshot and the queued goroutine changes at least one of them.
type retuneGuard struct {
	settings uint64
	state    uint64
	session  *session
}

// retune starts a new session for the current selection, replacing any live
// one. It is used by next/previous and recovery.
func (a *App) retune(why string) (bool, string) {
	return a.retuneGuarded(nil, why)
}

// retuneFromSettings publishes a retune only while the snapshot that queued it
// still owns the radio state. A playback Stop hold must never turn into audio,
// and a newer selection or control must win over a delayed settings edit.
func (a *App) retuneFromSettings(guard retuneGuard) (bool, string) {
	return a.retuneGuarded(&guard, "settings")
}

func (a *App) retuneGuarded(guard *retuneGuard, why string) (bool, string) {
	a.mu.Lock()
	if guard != nil {
		if guard.settings == 0 || guard.settings != a.settingsRetuneGen ||
			guard.state != a.exitSeq || guard.session != a.session ||
			a.halted || a.exiting {
			a.mu.Unlock()
			return false, "settings retune superseded"
		}
	}
	if a.exiting {
		a.mu.Unlock()
		return false, "radio is exiting"
	}
	selected, ok := a.currentStationLocked()
	if !ok {
		a.beginDisplayRunLocked()
		reason := a.missingStationReasonLocked()
		old := a.failLocked(reason)
		a.mu.Unlock()
		if old != nil {
			old.stop()
		}
		a.requestDraw()
		return false, reason
	}
	if !a.speakerAvailable {
		a.beginDisplayRunLocked()
		old := a.failLocked("no speaker endpoint on this device")
		a.mu.Unlock()
		if old != nil {
			old.stop()
		}
		a.requestDraw()
		return false, "no speaker endpoint on this device"
	}
	old := a.session
	s := a.startSessionLocked(selected, why)
	a.mu.Unlock()
	if old != nil {
		old.stop()
	}
	a.requestDraw()
	go s.run()
	return true, "tuning " + selected.Name
}

// startSessionLocked replaces the session pointer and enters buffering. The
// caller owns the returned session and must run/stop it outside the lock.
func (a *App) startSessionLocked(station Station, why string) *session {
	a.cancelExitLocked()
	a.beginDisplayRunLocked()
	a.sessionSeq++
	s := newSession(a, a.sessionSeq, station)
	a.session = s
	a.sessionURL = station.URL
	a.phase = PhaseBuffering
	a.halted = false
	a.detail = why
	a.nowTitle = ""
	a.nowArtist = ""
	a.revision++
	a.signalWatchersLocked()
	return s
}

func (a *App) failLocked(detail string) *session {
	old := a.session
	a.session = nil
	a.sessionURL = ""
	a.phase = PhaseError
	a.halted = false
	a.detail = detail
	a.nowTitle = ""
	a.nowArtist = ""
	a.armErrorExitLocked()
	a.revision++
	a.signalWatchersLocked()
	a.logf("error: %s", detail)
	return old
}

// failSession ends s only if it still owns the radio. A superseded session's
// late failure therefore cannot clear a newer stream or selection.
func (a *App) failSession(s *session, detail string) {
	a.mu.Lock()
	if a.session != s {
		a.mu.Unlock()
		return
	}
	a.session = nil
	a.sessionURL = ""
	a.phase = PhaseError
	a.halted = false
	a.detail = detail
	a.nowTitle = ""
	a.nowArtist = ""
	a.armErrorExitLocked()
	a.revision++
	a.signalWatchersLocked()
	a.mu.Unlock()
	a.logf("error: %s", detail)
	s.stop()
	a.requestDraw()
}

// finishSession is the pump's final hook. It clears only the session it owns;
// an old async completion can never clear a newer one.
func (a *App) finishSession(s *session) {
	a.mu.Lock()
	if a.session != s {
		a.mu.Unlock()
		return
	}
	a.session = nil
	a.sessionURL = ""
	if s.ctx.Err() == nil {
		a.phase = PhaseError
		a.halted = false
		a.detail = "station stream ended"
		a.nowTitle = ""
		a.nowArtist = ""
		a.armErrorExitLocked()
		a.logf("error: station stream ended")
	}
	a.revision++
	a.signalWatchersLocked()
	a.mu.Unlock()
	a.requestDraw()
}

// markPlaying promotes a session that queued its first audio chunk.
func (a *App) markPlaying(s *session) {
	a.mu.Lock()
	if a.session != s {
		a.mu.Unlock()
		return
	}
	if a.phase != PhasePlaying {
		a.phase = PhasePlaying
		a.detail = "station audio"
		a.revision++
		a.signalWatchersLocked()
	}
	a.mu.Unlock()
	a.logf("state playing station audio")
	a.requestDraw()
}

// updateMetadata publishes ICY metadata for the owning session only.
func (a *App) updateMetadata(s *session, artist, title string) {
	a.mu.Lock()
	if a.session != s || (a.nowArtist == artist && a.nowTitle == title) {
		a.mu.Unlock()
		return
	}
	a.nowArtist, a.nowTitle = artist, title
	a.revision++
	a.signalWatchersLocked()
	a.mu.Unlock()
}

func (a *App) currentStationLocked() (Station, bool) {
	if a.stationNumber < 1 || a.stationNumber > len(a.stations) {
		return Station{}, false
	}
	return a.stations[a.stationNumber-1], true
}

func (a *App) missingStationReasonLocked() string {
	return MissingStationReason(a.stationNumber, len(a.stations))
}

func (a *App) missingStationReason() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.missingStationReasonLocked()
}

func (a *App) signalWatchersLocked() {
	for _, ch := range a.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (a *App) requestDraw() {
	select {
	case a.drawCh <- struct{}{}:
	default:
	}
}

func reasonText(why string) string {
	switch why {
	case "stop":
		return "stopped"
	case "dismiss":
		return "dismissed"
	default:
		if why == "" {
			return "stopped"
		}
		return why
	}
}

// HandleInvocation applies one launch. A bare launch only proves the app is
// running; naming a station is the explicit "play this" request.
func (a *App) HandleInvocation(ctx context.Context, invocation *capsulad.LaunchInvocation) {
	if invocation == nil {
		return
	}
	a.WaitSettings(ctx, 5*time.Second)
	wanted := ""
	for _, argument := range invocation.Arguments {
		if argument.Name == "station" && strings.TrimSpace(argument.Value) != "" {
			wanted = argument.Value
		}
	}
	ok := true
	detail := "radio ready"
	if wanted != "" {
		a.mu.Lock()
		number, found := SelectText(a.stations, wanted)
		if found {
			if a.stationNumber != number || a.persistedStation != number {
				a.selectionDirty = true
			}
			a.stationNumber = number
			a.revision++
			a.signalWatchersLocked()
		}
		a.mu.Unlock()
		if !found {
			ok, detail = false, "unknown station "+wanted
		} else {
			go a.persistSelection()
			ok, detail = a.play("invocation")
		}
	} else {
		// A bare launch is a user asking for the radio to be available. It
		// still only proves readiness, but the run gets a fresh idle window
		// rather than exiting immediately after the launch.
		a.mu.Lock()
		exiting := a.exiting
		if !exiting && a.phase == PhaseStopped {
			a.armExitLocked(a.idleHold(), "idle")
		}
		a.mu.Unlock()
		if exiting {
			ok, detail = false, "radio is exiting"
		}
	}
	a.logf("invocation %s ok=%t %s", invocation.ID, ok, detail)
	if err := a.client.NotifyInvocationResult(invocation.ID, ok, detail); err != nil {
		a.logf("reporting invocation result: %v", err)
	}
}

// HandleAction applies one manifest-declared action and reports its result.
func (a *App) HandleAction(invocation *capsulad.ActionInvoked) {
	if invocation == nil {
		return
	}
	a.logf("action %s", invocation.ActionID)
	ok, detail := a.Control(invocation.ActionID)
	if err := a.client.NotifyActionResult(invocation.ID, ok, detail); err != nil {
		a.logf("reporting %q action result: %v", invocation.ActionID, err)
	}
}

// Close stops the live session, the watchdog and the display loop. It is safe
// to call after the bus has already ended.
func (a *App) Close() {
	a.cancel()

	a.mu.Lock()
	s := a.session
	a.session = nil
	a.sessionURL = ""
	a.cancelExitLocked()
	a.exiting = true
	a.halted = false
	if a.phase != PhaseStopped {
		a.phase = PhaseStopped
		a.detail = "shutdown"
		a.revision++
		a.signalWatchersLocked()
	}
	a.mu.Unlock()
	if s != nil {
		s.stop()
		s.wait(2 * time.Second)
	}
	select {
	case <-a.displayDone:
	case <-time.After(2 * time.Second):
	}
	// Audio and display are already released above. Only now make an accepted
	// selection durable: an in-flight write was cancelled with the app
	// context, and a dirty next/previous or named launch must survive the
	// Dismiss. The flush is gated on that dirty flag and an initialized
	// settings revision, so an untouched launch default never overwrites the
	// stored station.
	a.flushSelection(selectionFlushTimeout)
}

func (a *App) watchdogLoop() {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		a.mu.Lock()
		s := a.session
		phase := a.phase
		a.mu.Unlock()
		if s == nil || (phase != PhasePlaying && phase != PhaseBuffering) {
			continue
		}
		if s.idleFor() < noAudioTimeout {
			continue
		}
		detail := "radio stalled (no queued audio for " + noAudioTimeout.String() + ")"
		if phase == PhaseBuffering {
			detail = "radio did not start (no audio for " + noAudioTimeout.String() + ")"
		}
		a.failSession(s, detail)
	}
}

// --- setting value decoding -------------------------------------------------

func decodeString(raw string) (string, bool) {
	var value string
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", false
	}
	return value, true
}

func decodeInt(raw string) (int, bool) {
	var value int
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return 0, false
	}
	return value, true
}

func decodeBool(raw string) (bool, bool) {
	var value bool
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return false, false
	}
	return value, true
}

func formatUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}
