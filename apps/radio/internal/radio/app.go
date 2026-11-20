// Package radio is the Internet Radio application: station settings, the
// in-process MP3 stream, the speaker stream, the segment presentation and the
// standard media contract.
//
// One native app owns the whole radio. Station audio is fetched over the
// shared capsulad transport, decoded in this process and written to the
// framework speaker; there is no companion app and no app-to-app audio hop.
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
	now              func() time.Time
	settingsRevision uint64
	persistedStation int
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

	persistMu sync.Mutex
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
	}
	go a.displayLoop()
	go a.watchdogLoop()
	return a
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
		} else if a.stationEchoLocked(*station, event.Revision) {
			// This snapshot reflects a write this app made for an older
			// selection; a newer next/previous must not be reverted by it.
			a.persistedStation = *station
		} else {
			a.stationNumber = *station
			a.persistedStation = *station
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
	active := a.phase == PhasePlaying || a.phase == PhaseBuffering || a.phase == PhasePaused
	retune := false
	if active {
		selected, ok := a.currentStationLocked()
		retune = !ok || selected.URL != a.sessionURL
	}
	autoplayNow := first && a.autoplay && a.phase == PhaseStopped
	registeredAutoplay := a.autoplay
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
	if autoplayNow {
		go a.play("autoplay")
		return
	}
	if retune {
		go a.retune("settings")
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
		return a.stop("stop")
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

func (a *App) pause() (bool, string) {
	a.mu.Lock()
	if a.phase != PhasePlaying && a.phase != PhaseBuffering {
		a.mu.Unlock()
		return false, "nothing is playing"
	}
	old := a.session
	a.session = nil
	a.phase = PhasePaused
	a.detail = "paused"
	url := a.sessionURL
	a.revision++
	a.signalWatchersLocked()
	a.mu.Unlock()
	if old != nil {
		old.stop()
	}
	a.requestDraw()
	a.logf("state paused %s", url)
	return true, "paused"
}

func (a *App) stop(why string) (bool, string) {
	a.mu.Lock()
	old := a.session
	a.session = nil
	a.sessionURL = ""
	a.phase = PhaseStopped
	a.displayUntil = time.Time{}
	a.detail = reasonText(why)
	detail := a.detail
	a.nowTitle = ""
	a.nowArtist = ""
	a.revision++
	a.signalWatchersLocked()
	a.mu.Unlock()
	if old != nil {
		old.stop()
	}
	a.requestDraw()
	a.logf("state stopped %s", detail)
	return true, detail
}

func (a *App) step(delta int) (bool, string) {
	a.mu.Lock()
	if len(a.stations) == 0 {
		a.mu.Unlock()
		return false, "no stations configured"
	}
	a.stationNumber = StepNumber(a.stationNumber, delta, len(a.stations))
	active := a.phase == PhasePlaying || a.phase == PhaseBuffering ||
		a.phase == PhasePaused || a.phase == PhaseError
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

// retune starts a new session for the current selection, replacing any live
// one. It is used by next/previous, a station-setting change and recovery.
func (a *App) retune(why string) (bool, string) {
	a.mu.Lock()
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
	a.beginDisplayRunLocked()
	a.sessionSeq++
	s := newSession(a, a.sessionSeq, station)
	a.session = s
	a.sessionURL = station.URL
	a.phase = PhaseBuffering
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
	a.detail = detail
	a.nowTitle = ""
	a.nowArtist = ""
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
	a.detail = detail
	a.nowTitle = ""
	a.nowArtist = ""
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
		a.detail = "station stream ended"
		a.nowTitle = ""
		a.nowArtist = ""
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
