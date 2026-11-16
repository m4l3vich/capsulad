package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/dcron"
	"capsulad/internal/presets"
	"capsulad/internal/settings"
)

const (
	maxTimerMS   = uint64(30 * 24 * 60 * 60 * 1000)
	tickInterval = 200 * time.Millisecond

	// Alerts repeat until the user dismisses them. These are the silences
	// between clips, not lifetimes.
	timerAlertGap = 1100 * time.Millisecond
	alarmAlertGap = 1800 * time.Millisecond
)

// Seven-segment masks: bit 0 = a (top) ... bit 6 = g (middle).
var clockDigitMasks = [10]uint16{
	0b0111111, 0b0000110, 0b1011011, 0b1001111, 0b1100110,
	0b1101101, 0b1111101, 0b0000111, 0b1111111, 0b1101111,
}

type clockTimerStatus int

const (
	timerRunning clockTimerStatus = iota
	timerCompleted
	timerCancelled
)

type clockTimer struct {
	id         string
	label      string
	durationNS uint64
	deadlineNS uint64
	startedMS  uint64
	finishedMS uint64
	status     clockTimerStatus
}

type clockAlarm struct {
	// id is the stable alarm identity (expression + immutable modifiers);
	// text is the canonical persisted entry.
	id       string
	text     string
	expr     string
	zone     string
	label    string
	once     bool
	disabled bool
	sched    dcron.Schedule
	next     time.Time
	// lastTriggerKey identifies the local wall-clock minute in which this
	// schedule last fired. Wall time (not UTC) deduplicates the repeated hour
	// at a fall-back DST transition and still prevents a 200 ms tick from
	// re-ringing the same minute.
	lastTriggerKey  string
	lastTriggeredMS uint64
}

func (a *clockAlarm) entry() dcron.Entry {
	return dcron.Entry{Expr: a.expr, Zone: a.zone, Once: a.once, Disabled: a.disabled}
}

type clockAlertKind int

const (
	alertTimer clockAlertKind = iota
	alertAlarm
)

type clockAlert struct {
	id        string
	kind      clockAlertKind
	sourceID  string
	label     string
	startedMS uint64
}

type clockDisplay struct {
	cells [4]uint16
	colon bool
}

type builtinClock struct {
	d *Daemon

	segmentID string
	ringID    string

	mu      sync.Mutex
	running bool
	runID   string
	ctx     context.Context
	cancel  context.CancelFunc

	client      *builtinAppClient
	pres        *appPresentation
	audioFormat backend.AudioFormat

	// now and mono are replaceable in tests. mono follows the daemon's
	// monotonic clock; stopwatch/timer scheduling never depends on wall time.
	now  func() time.Time
	mono func() uint64

	blink       bool
	appTZ       string
	deviceTZ    string
	loc         *time.Location
	displayMode string
	timerSecs   uint64
	// stopwatchReset chooses which primary-key gesture resets/dismisses the
	// stopwatch: "hold" (default) or "double_tap". A tap always pauses/resumes.
	stopwatchReset string

	stopwatch struct {
		running   bool
		elapsedNS uint64
		startedNS uint64
		startedMS uint64
	}

	timers     map[string]*clockTimer
	timerOrder []string
	alarms     map[string]*clockAlarm
	alarmOrder []string
	alerts     map[string]*clockAlert
	alertOrder []string
	nextID     int

	applied   clockDisplay
	appliedOK bool
	// settingsMu serializes settings application so an older watcher snapshot
	// cannot overwrite a newer synchronous mutation.
	settingsMu       sync.Mutex
	settingsLoaded   bool
	settingsRevision uint64
	// runGen invalidates watcher callbacks from a previous start/stop.
	runGen uint64
	// frameMu serializes baseline frame compute+send so concurrent publishes
	// cannot reorder frames or mark a failed write as applied.
	frameMu sync.Mutex
	// failureMu serializes recovery with the next generation start. Without
	// it, a failure could validate an old generation, pause, and then fail a
	// newer run after the stable run ID has been reused.
	failureMu sync.Mutex
	// failedGen records the generation whose single recovery has been claimed.
	// It is protected by mu and never reset; runGen is monotonically increasing.
	failedGen uint64
	// pendingOnce collects one-shot alarms that fired; tick persists their
	// disabled marker outside the clock lock.
	pendingOnce []string
	// selfDisabledIDs marks one-shots the clock disarmed. Explicit
	// settings-driven disables dismiss the alert; these keep ringing.
	selfDisabledIDs map[string]bool
	// watchRevision increments on every observable state change and is
	// carried by snapshots. watchSig deduplicates the once-per-second stream
	// while a stopwatch or timer is running.
	watchRevision uint64
	watchSig      string
	watchers      map[int]chan *clockv1.ClockState
	nextWatcher   int
	alertGen      uint64
	alertMu       sync.Mutex
	alertPres     map[string]*appPresentation
	alertCap      map[string]*appCapture
	alertSig      map[string]clockDisplay
	alertRingSig  string
	alertErrAt    time.Time
	audioCache    map[string][]byte

	// countdown* is the clock's normal ring capture while a timer runs. It is
	// distinct from the alert overlay so the ring baseline returns as soon as
	// the countdown ends and an external capture can always preempt it.
	countPres       *appPresentation
	countCap        *appCapture
	countFrameTimer string
	countFrameLit   int
	countFramePix   int
	countLogTimer   string
	countLogLit     int
	countErrAt      time.Time

	// audioMu serializes the single alert-audio loop. At most one clip plays at
	// a time; dismissal cancels the loop and the in-flight clip.
	audioMu      sync.Mutex
	audioCancel  context.CancelFunc
	audioAlertID string
	// playAudio is nil in production, where the app client's Playback is used.
	playAudio func(context.Context, backend.AudioFormat, []byte) error
}

func newBuiltinClock(d *Daemon, segmentID string) *builtinClock {
	c := &builtinClock{
		d: d, segmentID: segmentID,
		blink: true, displayMode: "clock", timerSecs: 300, loc: time.Local,
		stopwatchReset: "hold",
		timers:         map[string]*clockTimer{}, alarms: map[string]*clockAlarm{},
		alerts:    map[string]*clockAlert{},
		alertPres: map[string]*appPresentation{}, alertCap: map[string]*appCapture{},
		alertSig: map[string]clockDisplay{}, audioCache: map[string][]byte{},
		watchers:        map[int]chan *clockv1.ClockState{},
		selfDisabledIDs: map[string]bool{},
		now:             time.Now,
		mono:            func() uint64 { return d.clock.NowNS() },
	}
	c.ringID = d.ringResourceID()
	return c
}

func (c *builtinClock) start() {
	c.failureMu.Lock()
	c.settingsMu.Lock()
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		c.settingsMu.Unlock()
		c.failureMu.Unlock()
		return
	}
	c.running = true
	c.runGen++
	gen := c.runGen
	c.settingsLoaded = false
	c.settingsRevision = 0
	c.runID = c.d.SystemAppRunID(clockAppID)
	c.ctx, c.cancel = context.WithCancel(c.d.ctx)
	c.stopwatch.running = false
	c.stopwatch.elapsedNS = 0
	c.stopwatch.startedNS = 0
	c.stopwatch.startedMS = 0
	c.timers = map[string]*clockTimer{}
	c.timerOrder = nil
	c.alerts = map[string]*clockAlert{}
	c.alertOrder = nil
	c.alertPres = map[string]*appPresentation{}
	c.alertCap = map[string]*appCapture{}
	c.alertSig = map[string]clockDisplay{}
	c.alertRingSig = ""
	c.alertErrAt = time.Time{}
	c.countPres = nil
	c.countCap = nil
	c.countFrameTimer = ""
	c.countFrameLit = 0
	c.countFramePix = 0
	c.countLogTimer = ""
	c.countLogLit = -1
	c.countErrAt = time.Time{}
	c.appliedOK = false
	c.watchSig = ""
	c.alarms = map[string]*clockAlarm{}
	c.alarmOrder = nil
	c.selfDisabledIDs = map[string]bool{}
	c.pendingOnce = nil
	c.client = newBuiltinAppClient(c.d, clockAppID)
	client := c.client
	ctx := c.ctx
	c.mu.Unlock()
	c.settingsMu.Unlock()
	c.failureMu.Unlock()
	client.SetFailureHandler(func(err error) { c.failRun(gen, err) })

	// Open the internal streams outside lifecycle/settings locks. A slow
	// daemon endpoint must not block Launch/Stop, RPCs or settings updates.
	var pres *appPresentation
	var openErr error
	if c.segmentID != "" {
		pres, openErr = client.OpenPresentation(ctx, c.segmentID)
	}
	format, formatErr := client.PlaybackFormat(ctx)

	c.mu.Lock()
	if !c.running || c.client != client {
		c.mu.Unlock()
		if pres != nil {
			pres.Close()
		}
		client.Close()
		return
	}
	if pres != nil {
		c.pres = pres
	}
	if formatErr == nil {
		c.audioFormat = format
	} else {
		c.d.logs.Warnf(clockAppID, "", "playback format unavailable: %v", formatErr)
	}
	c.mu.Unlock()
	if openErr != nil {
		c.d.logs.Errorf(clockAppID, "", "open baseline presentation: %v", openErr)
		c.failRun(gen, openErr)
		return
	}

	c.d.markSystemHealthy(clockAppID)
	c.loadSettings(ctx, gen)
	c.publish()
	go c.loop(ctx)
}

// failRun recovers an internal stream failure by failing this run; the daemon
// schedules a bounded restart. Recovery is claimed at most once per run
// generation: a callback from an earlier start or a normal stop is ignored, so
// a stale stream can never fail a newer run.
func (c *builtinClock) failRun(gen uint64, err error) {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	c.mu.Lock()
	if c.runGen != gen || !c.running || c.failedGen == gen {
		c.mu.Unlock()
		return
	}
	c.failedGen = gen
	c.mu.Unlock()
	c.d.logs.Errorf(clockAppID, "", "internal stream failed: %v; recovering run", err)
	c.d.recoverSystemApp(clockAppID)
}

func (c *builtinClock) stop() {
	c.alertMu.Lock()
	defer c.alertMu.Unlock()
	c.settingsMu.Lock()
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		c.settingsMu.Unlock()
		return
	}
	c.running = false
	c.runGen++
	c.settingsLoaded = false
	c.settingsRevision = 0
	if c.cancel != nil {
		c.cancel()
	}
	pres := c.pres
	c.pres = nil
	client := c.client
	c.client = nil
	alertPres := make([]*appPresentation, 0, len(c.alertPres))
	for _, p := range c.alertPres {
		alertPres = append(alertPres, p)
	}
	caps := make([]*appCapture, 0, len(c.alertCap))
	for _, cap := range c.alertCap {
		caps = append(caps, cap)
	}
	if c.countPres != nil {
		alertPres = append(alertPres, c.countPres)
	}
	if c.countCap != nil {
		caps = append(caps, c.countCap)
	}
	c.alertPres = map[string]*appPresentation{}
	c.alertCap = map[string]*appCapture{}
	c.alertSig = map[string]clockDisplay{}
	c.alertRingSig = ""
	c.alertErrAt = time.Time{}
	c.countPres = nil
	c.countCap = nil
	c.countFrameTimer = ""
	c.countFrameLit = 0
	c.countFramePix = 0
	c.countLogTimer = ""
	c.countLogLit = -1
	c.countErrAt = time.Time{}
	c.timers = map[string]*clockTimer{}
	c.timerOrder = nil
	c.alerts = map[string]*clockAlert{}
	c.alertOrder = nil
	c.selfDisabledIDs = map[string]bool{}
	c.pendingOnce = nil
	c.appliedOK = false
	c.alertGen++
	for id, ch := range c.watchers {
		delete(c.watchers, id)
		close(ch)
	}
	c.mu.Unlock()
	c.settingsMu.Unlock()

	c.stopAlertAudio()
	for _, cap := range caps {
		cap.Release()
	}
	for _, p := range alertPres {
		p.Close()
	}
	if pres != nil {
		pres.Close()
	}
	if client != nil {
		client.Close()
	}
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *builtinClock) loop(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tick()
		}
	}
}

// --- settings ---------------------------------------------------------------

func (c *builtinClock) loadSettings(ctx context.Context, gen uint64) {
	snap, ch, cancel, err := c.d.appCfg.WatchApp(clockAppID)
	if err != nil {
		c.d.logs.Warnf(clockAppID, "", "settings unavailable: %v", err)
		return
	}
	c.applySettingsGen(gen, snap.Revision, snap.Values)
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case next, ok := <-ch:
				if !ok {
					return
				}
				c.applySettingsGen(gen, next.Revision, next.Values)
			}
		}
	}()
}

// applySettings applies settings for the current run. Mutating callers use it
// so their committed snapshot is visible synchronously.
func (c *builtinClock) applySettings(revision uint64, values []settings.AppSettingView) {
	c.settingsMu.Lock()
	gen := c.runGen
	c.settingsMu.Unlock()
	c.applySettingsGen(gen, revision, values)
}

// applySettingsGen drops callbacks from a previous run and enforces monotonic
// store revisions for this run. revision 0 is an unversioned local apply
// (initial tests); persisted revisions are always >= 1.
func (c *builtinClock) applySettingsGen(gen, revision uint64, values []settings.AppSettingView) {
	c.settingsMu.Lock()
	if gen != c.runGen {
		c.settingsMu.Unlock()
		return
	}
	if revision != 0 {
		c.mu.Lock()
		if c.settingsLoaded && revision <= c.settingsRevision {
			c.mu.Unlock()
			c.settingsMu.Unlock()
			return
		}
		c.settingsLoaded = true
		c.settingsRevision = revision
		c.mu.Unlock()
	}
	c.mu.Lock()
	blink := c.blink
	tz := c.appTZ
	mode := c.displayMode
	timerSecs := c.timerSecs
	resetGesture := c.stopwatchReset
	c.mu.Unlock()

	decodeSetting(values, "blink", &blink)
	decodeSetting(values, "timezone", &tz)
	decodeSetting(values, "display_mode", &mode)
	decodeSetting(values, "stopwatch_reset_gesture", &resetGesture)
	var secs int64
	if decodeSetting(values, "timer_seconds", &secs) && secs > 0 {
		timerSecs = uint64(secs)
	}
	var alarmText string
	if !decodeSetting(values, "alarms", &alarmText) {
		alarmText = ""
	}
	alarmLabels := map[string]string{}
	var labelsText string
	if decodeSetting(values, "alarm_labels", &labelsText) && labelsText != "" {
		if err := json.Unmarshal([]byte(labelsText), &alarmLabels); err != nil {
			c.d.logs.Warnf(clockAppID, "", "invalid alarm_labels setting: %v", err)
			alarmLabels = map[string]string{}
		}
	}

	switch mode {
	case "clock", "stopwatch", "timer":
	default:
		mode = "clock"
	}
	switch resetGesture {
	case "hold", "double_tap":
	default:
		resetGesture = "hold"
	}
	if timerSecs == 0 {
		timerSecs = 300
	}
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			c.d.logs.Warnf(clockAppID, "", "timezone %q unavailable (%v); using the device timezone", tz, err)
		}
	}

	entries, err := dcron.ParseEntries(alarmText)
	if err != nil {
		// The synchronous settings validator rejects this shape, so a value
		// that reaches here is a hand-edited file. Keep the running schedule
		// instead of silently dropping alarms.
		c.d.logs.Warnf(clockAppID, "", "invalid alarms setting: %v", err)
		c.mu.Lock()
		entries = entries[:0]
		for _, id := range c.alarmOrder {
			if a := c.alarms[id]; a != nil {
				entries = append(entries, a.entry())
			}
		}
		c.mu.Unlock()
	}

	c.mu.Lock()
	c.blink = blink
	c.appTZ = tz
	c.loc = c.resolveLocationLocked()
	c.displayMode = mode
	c.timerSecs = timerSecs
	c.stopwatchReset = resetGesture
	loc := c.alarmLocationLocked()
	now := c.now()
	previous := c.alarms
	next := make(map[string]*clockAlarm, len(entries))
	order := make([]string, 0, len(entries))
	removed := 0
	for _, entry := range entries {
		sched, err := dcron.Parse(entry.Expr)
		if err != nil {
			c.d.logs.Warnf(clockAppID, "", "skip alarm %q: %v", entry.Expr, err)
			continue
		}
		if entry.Zone != "" {
			if _, err := time.LoadLocation(entry.Zone); err != nil {
				c.d.logs.Warnf(clockAppID, "", "alarm %q timezone %q unavailable; using the device timezone", entry.Expr, entry.Zone)
				entry.Zone = ""
			}
		}
		id := entry.ID()
		if _, dup := next[id]; dup {
			continue
		}
		a := previous[id]
		if a == nil {
			a = &clockAlarm{id: id}
		}
		a.text, a.expr, a.zone = entry.Canonical(), entry.Expr, entry.Zone
		a.label = alarmLabels[id]
		if a.label == "" {
			a.label = "Alarm"
		}
		a.once, a.disabled = entry.Once, entry.Disabled
		if entry.Disabled {
			if !c.selfDisabledIDs[id] {
				// An explicit settings disable (ApplicationConfiguration or
				// the friendly editor) dismisses the alert. Only the clock's
				// own one-shot disarm keeps its alert ringing.
				removed += c.removeAlertsLocked(func(alert *clockAlert) bool {
					return alert.kind == alertAlarm && alert.sourceID == id
				})
			}
		} else {
			delete(c.selfDisabledIDs, id)
		}
		a.sched = sched
		a.next = sched.Next(now, alarmLocation(a, loc))
		next[id] = a
		order = append(order, id)
	}
	for id := range previous {
		if next[id] == nil {
			removed += c.removeAlertsLocked(func(alert *clockAlert) bool {
				return alert.kind == alertAlarm && alert.sourceID == id
			})
		}
	}
	for _, a := range next {
		if a.disabled {
			a.next = time.Time{}
		}
	}
	c.alarms = next
	c.alarmOrder = order
	c.mu.Unlock()
	c.settingsMu.Unlock()
	if removed > 0 {
		c.d.logs.Infof(clockAppID, "", "alarm schedule change dismissed %d active alert(s)", removed)
		c.reconcileAlertAudio()
	}
	c.publish()
}

// alarmLocation returns the zone an entry is evaluated in: its own IANA zone
// when present, otherwise the device zone.
func alarmLocation(a *clockAlarm, device *time.Location) *time.Location {
	if a != nil && a.zone != "" {
		if loc, err := time.LoadLocation(a.zone); err == nil {
			return loc
		}
	}
	return device
}

// setDeviceTimezone follows the device-wide setting; an alarm without its own
// timezone uses it. Each alarm's next trigger is recomputed because the same
// wall-clock hour is a different instant in a different zone.
func (c *builtinClock) setDeviceTimezone(name string) {
	c.mu.Lock()
	if name == c.deviceTZ {
		c.mu.Unlock()
		return
	}
	c.deviceTZ = name
	c.loc = c.resolveLocationLocked()
	now := c.now()
	device := c.alarmLocationLocked()
	for _, a := range c.alarms {
		if a.disabled {
			continue
		}
		a.next = a.sched.Next(now, alarmLocation(a, device))
	}
	c.mu.Unlock()
	c.publish()
}

func (c *builtinClock) locationLocked() *time.Location {
	if c.loc != nil {
		return c.loc
	}
	return time.Local
}

// resolveLocationLocked recomputes the cached display location after the app
// or device timezone changes. Callers hold c.mu.
func (c *builtinClock) resolveLocationLocked() *time.Location {
	if loc := loadLocation(c.appTZ); loc != nil {
		return loc
	}
	return loadLocationOrDefault(c.deviceTZ)
}

// alarmLocationLocked returns the zone alarm expressions are evaluated in.
// Alarm schedules use the device wall clock; the app's display timezone does
// not move them.
func (c *builtinClock) alarmLocationLocked() *time.Location {
	return loadLocationOrDefault(c.deviceTZ)
}

func loadLocation(name string) *time.Location {
	if name == "" {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}

func loadLocationOrDefault(name string) *time.Location {
	if loc := loadLocation(name); loc != nil {
		return loc
	}
	return time.Local
}

// --- scheduling -------------------------------------------------------------

func (c *builtinClock) tick() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	now := c.now()
	mono := c.mono()
	c.advanceLocked(now, mono)
	once := c.pendingOnce
	c.pendingOnce = nil
	c.mu.Unlock()
	c.disableAlarmEntries(once)
	c.publish()
}

// advanceLocked fires due timers and alarms and recomputes recurring alarm
// triggers. Alerts stay active until explicitly dismissed; the caller holds
// c.mu.
func (c *builtinClock) advanceLocked(now time.Time, mono uint64) {
	for _, id := range c.timerOrder {
		t := c.timers[id]
		if t == nil || t.status != timerRunning || mono < t.deadlineNS {
			continue
		}
		t.status = timerCompleted
		t.finishedMS = uint64(now.UnixMilli())
		alertID := c.addAlertLocked(alertTimer, t.id, t.label, now)
		c.d.logs.Infof(clockAppID, "", "timer %s (%s) completed; alert %s rings until dismissed", t.id, t.label, alertID)
	}
	device := c.alarmLocationLocked()
	for _, id := range c.alarmOrder {
		a := c.alarms[id]
		if a == nil || a.disabled {
			continue
		}
		loc := alarmLocation(a, device)
		if !a.sched.Matches(now, loc) {
			continue
		}
		key := now.In(loc).Format("2006-01-02T15:04")
		if a.lastTriggerKey == key {
			continue
		}
		a.lastTriggerKey = key
		a.lastTriggeredMS = uint64(now.UnixMilli())
		if a.once {
			// Match the previous one-shot alarm: it rings once at the next
			// occurrence and is then disabled; the alert waits for dismissal.
			a.disabled = true
			c.selfDisabledIDs[a.id] = true
			a.next = time.Time{}
			a.text = a.entry().Canonical()
			c.pendingOnce = append(c.pendingOnce, a.id)
		} else {
			a.next = a.sched.Next(now, loc)
		}
		alertID := c.addAlertLocked(alertAlarm, a.id, a.label, now)
		c.d.logs.Infof(clockAppID, "", "alarm %s fired; alert %s rings until dismissed", a.id, alertID)
	}
}

func (c *builtinClock) addAlertLocked(kind clockAlertKind, sourceID, label string, now time.Time) string {
	// A recurring alarm that is already ringing must not add a second alert for
	// the same source; otherwise an ignored daily alarm grows the alert list
	// without bound.
	for _, id := range c.alertOrder {
		if alert := c.alerts[id]; alert != nil && alert.kind == kind && alert.sourceID == sourceID {
			alert.label = label
			c.alertGen++
			return id
		}
	}
	c.alertGen++
	c.nextID++
	id := fmt.Sprintf("alt_%d", c.nextID)
	c.alerts[id] = &clockAlert{
		id: id, kind: kind, sourceID: sourceID, label: label,
		startedMS: uint64(now.UnixMilli()),
	}
	c.alertOrder = append(c.alertOrder, id)
	return id
}

func (c *builtinClock) removeAlertLocked(id string) {
	a := c.alerts[id]
	if a == nil {
		return
	}
	delete(c.alerts, id)
	c.alertGen++
	for i, cur := range c.alertOrder {
		if cur == id {
			c.alertOrder = append(c.alertOrder[:i], c.alertOrder[i+1:]...)
			break
		}
	}
	if a.kind == alertTimer {
		delete(c.timers, a.sourceID)
		for i, cur := range c.timerOrder {
			if cur == a.sourceID {
				c.timerOrder = append(c.timerOrder[:i], c.timerOrder[i+1:]...)
				break
			}
		}
	}
}

// removeAlertsLocked dismisses every alert matching match, for cases where the
// source disappears (alarm disabled or deleted) and no user dismissal will
// ever arrive. The caller holds c.mu.
func (c *builtinClock) removeAlertsLocked(match func(*clockAlert) bool) int {
	removed := 0
	for _, id := range append([]string(nil), c.alertOrder...) {
		if alert := c.alerts[id]; alert != nil && match(alert) {
			c.removeAlertLocked(id)
			removed++
		}
	}
	return removed
}

func (c *builtinClock) dismissAlert(id string) bool {
	return c.dismissAlertCount(id) > 0
}

// dismissAlertCount stops one alert or every alert and reports how many were
// dismissed. The public DismissAlert response carries that count.
func (c *builtinClock) dismissAlertCount(id string) uint32 {
	c.mu.Lock()
	if id == "" {
		count := uint32(len(c.alertOrder))
		if count == 0 {
			c.mu.Unlock()
			return 0
		}
		for _, cur := range append([]string(nil), c.alertOrder...) {
			c.removeAlertLocked(cur)
		}
		c.mu.Unlock()
		c.d.logs.Infof(clockAppID, "", "%d active alert(s) dismissed", count)
		c.reconcileAlertAudio()
		c.publish()
		return count
	}
	alert := c.alerts[id]
	if alert == nil {
		c.mu.Unlock()
		return 0
	}
	kind, sourceID := alert.kind, alert.sourceID
	c.removeAlertLocked(id)
	c.mu.Unlock()
	c.d.logs.Infof(clockAppID, "", "%s alert %s (%s) dismissed", clockAlertKindName(kind), id, sourceID)
	c.reconcileAlertAudio()
	c.publish()
	return 1
}

func (c *builtinClock) activeAlertLocked() *clockAlert {
	for _, id := range c.alertOrder {
		if a := c.alerts[id]; a != nil {
			return a
		}
	}
	return nil
}

// --- rendering --------------------------------------------------------------

func (c *builtinClock) publish() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	now := c.now()
	alert := c.activeAlertLocked()
	var alertDisplay clockDisplay
	haveAlert := alert != nil
	if haveAlert {
		alertDisplay = c.alertDisplayLocked(alert, now)
	}
	gen := c.alertGen
	c.mu.Unlock()

	c.reconcileAlertAudio()
	c.sendBaseline()
	c.publishAlert(alertDisplay, haveAlert, gen)
	c.publishCountdown()
	c.broadcastState()
}

// sendBaseline serializes compute+send so concurrent publishes cannot deliver
// frames out of order. applied is updated only after a successful write, so a
// failed stream is retried on the next publish instead of freezing old content.
func (c *builtinClock) sendBaseline() {
	c.frameMu.Lock()
	defer c.frameMu.Unlock()
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	base := c.baselineDisplayLocked(c.now(), c.mono())
	pres := c.pres
	if pres == nil || (c.appliedOK && base == c.applied) {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if err := pres.UpdateFrame(segmentPresentationFrame(base.cells[:], base.colon)); err != nil {
		if c.d.ctx.Err() == nil {
			c.d.logs.Debugf(clockAppID, "", "baseline update: %v", err)
		}
		return
	}
	c.mu.Lock()
	c.applied = base
	c.appliedOK = true
	c.mu.Unlock()
}

func (c *builtinClock) publishAlert(display clockDisplay, active bool, gen uint64) {
	c.alertMu.Lock()
	defer c.alertMu.Unlock()

	c.mu.Lock()
	if !c.running || c.alertGen != gen {
		c.mu.Unlock()
		return
	}
	client := c.client
	ctx := c.ctx
	if !active {
		if len(c.alertPres) == 0 && len(c.alertCap) == 0 && len(c.alertSig) == 0 {
			c.mu.Unlock()
			return
		}
		pres := make([]*appPresentation, 0, len(c.alertPres))
		for _, p := range c.alertPres {
			pres = append(pres, p)
		}
		caps := make([]*appCapture, 0, len(c.alertCap))
		for _, cap := range c.alertCap {
			caps = append(caps, cap)
		}
		c.alertPres = map[string]*appPresentation{}
		c.alertCap = map[string]*appCapture{}
		c.alertSig = map[string]clockDisplay{}
		c.alertRingSig = ""
		c.mu.Unlock()
		for _, cap := range caps {
			cap.Release()
		}
		for _, p := range pres {
			p.Close()
		}
		return
	}

	segment := c.alertPres[c.segmentID]
	ring := c.alertPres[c.ringID]
	segmentChanged := c.alertSig[c.segmentID] != display
	ringChanged := c.alertRingSig != "ring-pulse"
	segmentCaptured := c.alertCap[c.segmentID] != nil
	ringCaptured := c.alertCap[c.ringID] != nil
	c.mu.Unlock()
	if client == nil {
		return
	}

	if segment == nil && c.segmentID != "" {
		p, err := client.OpenPresentation(ctx, c.segmentID)
		if err != nil {
			c.logAlertFailure("alert segment presentation", err)
		} else {
			segment = p
			c.mu.Lock()
			c.alertPres[c.segmentID] = p
			c.mu.Unlock()
		}
	}
	if ring == nil && c.ringID != "" {
		p, err := client.OpenPresentation(ctx, c.ringID)
		if err != nil {
			c.logAlertFailure("alert ring presentation", err)
		} else {
			ring = p
			c.mu.Lock()
			c.alertPres[c.ringID] = p
			c.mu.Unlock()
		}
	}
	if segment != nil && segmentChanged {
		if err := segment.UpdateFrame(segmentPresentationFrame(display.cells[:], display.colon)); err != nil {
			c.logAlertFailure("alert segment", err)
		} else if c.segmentID != "" {
			c.mu.Lock()
			c.alertSig[c.segmentID] = display
			c.mu.Unlock()
		}
	}
	if ring != nil && ringChanged {
		params := mustParams("system.ring.pulse", presets.Params{
			"speed": 1.6, "color": ringCountdownColor, "min_intensity": 0.12,
		})
		if err := ring.UpdateFrame(ringEffectPresentationFrame("system.ring.pulse", params)); err != nil {
			c.logAlertFailure("alert ring", err)
		} else {
			c.mu.Lock()
			c.alertRingSig = "ring-pulse"
			c.mu.Unlock()
		}
	}
	if segment != nil && !segmentCaptured {
		if cap, err := client.Capture(ctx, c.segmentID, segment.ID()); err == nil {
			c.mu.Lock()
			c.alertCap[c.segmentID] = cap
			c.mu.Unlock()
		} else {
			c.logAlertFailure("alert display capture", err)
		}
	}
	if ring != nil && !ringCaptured {
		if cap, err := client.Capture(ctx, c.ringID, ring.ID()); err == nil {
			c.mu.Lock()
			c.alertCap[c.ringID] = cap
			c.mu.Unlock()
		} else {
			c.logAlertFailure("alert ring capture", err)
		}
	}
}

// publishCountdown owns the clock's normal ring capture while at least one
// timer runs. It is separate from the alert overlay: an alert takes the ring,
// and the countdown resumes beneath it once the alert is dismissed. Releasing
// the capture returns the configured ring baseline.
func (c *builtinClock) publishCountdown() {
	c.alertMu.Lock()
	defer c.alertMu.Unlock()

	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	var (
		active     bool
		stopwatch  bool
		timerID    string
		lit        int
		pixels     int
		logTimer   string
		logLit     int
		logPixels  int
		logSeconds int
		logState   bool
	)
	if c.activeAlertLocked() == nil {
		if t := c.nearestRunningTimerLocked(); t != nil {
			if px := c.ringPixelCount(); px > 0 {
				mono := c.mono()
				remaining := uint64(0)
				if t.deadlineNS > mono {
					remaining = t.deadlineNS - mono
				}
				active = true
				timerID, pixels = t.id, px
				lit = countdownLitPixels(pixels, t.durationNS, remaining)
				if c.countLogTimer != t.id || c.countLogLit != lit {
					c.countLogTimer, c.countLogLit = t.id, lit
					logState = true
					logTimer, logLit, logPixels = t.id, lit, pixels
					logSeconds = int((remaining + 999_999_999) / 1_000_000_000)
				}
			}
		} else if c.stopwatch.running && c.ringID != "" {
			// A running stopwatch owns the ring with a slow orange pulse. A
			// timer countdown takes precedence, and pausing the stopwatch
			// releases the capture so the selected baseline returns.
			active = true
			stopwatch = true
			timerID = stopwatchCueID
		}
	}
	if !active {
		c.countLogTimer, c.countLogLit = "", -1
	}
	client, ctx := c.client, c.ctx
	pres, cap := c.countPres, c.countCap
	curTimer, curLit, curPixels := c.countFrameTimer, c.countFrameLit, c.countFramePix
	if !active {
		c.countPres, c.countCap = nil, nil
		c.countFrameTimer, c.countFrameLit, c.countFramePix = "", 0, 0
	}
	c.mu.Unlock()

	if logState {
		c.d.logs.Infof(clockAppID, "", "timer %s countdown %d/%d pixels lit (%ds remaining)",
			logTimer, logLit, logPixels, logSeconds)
	}
	if !active {
		if cap != nil {
			cap.Release()
		}
		if pres != nil {
			pres.Close()
		}
		return
	}
	if client == nil {
		return
	}
	if pres == nil {
		p, err := client.OpenPresentation(ctx, c.ringID)
		if err != nil {
			c.logCountdownFailure("countdown ring presentation", err)
			return
		}
		pres = p
		c.mu.Lock()
		c.countPres = p
		c.countFrameTimer, c.countFrameLit, c.countFramePix = "", 0, 0
		c.mu.Unlock()
		curTimer, curLit, curPixels = "", 0, 0
	}
	if curTimer != timerID || curLit != lit || curPixels != pixels {
		var frame *frameworkv1.PresentationFrame
		if stopwatch {
			params := mustParams("system.ring.pulse", presets.Params{
				"speed": 0.8, "color": ringCountdownColor, "min_intensity": 0.08,
			})
			frame = ringEffectPresentationFrame("system.ring.pulse", params)
		} else {
			frame = ringFramePresentationFrame(countdownRingPixels(pixels, lit))
		}
		if err := pres.UpdateFrame(frame); err != nil {
			c.logCountdownFailure("countdown ring frame", err)
		} else {
			c.mu.Lock()
			c.countFrameTimer, c.countFrameLit, c.countFramePix = timerID, lit, pixels
			c.mu.Unlock()
		}
	}
	if cap == nil {
		acquired, err := client.Capture(ctx, c.ringID, pres.ID())
		if err != nil {
			c.logCountdownFailure("countdown ring capture", err)
			return
		}
		c.mu.Lock()
		c.countCap = acquired
		c.mu.Unlock()
	}
}

// logAlertFailure reports a broken alert overlay at most once per five seconds.
// Alerts now ring until dismissed, so retry logs must not fill the log.
func (c *builtinClock) logAlertFailure(what string, err error) {
	c.mu.Lock()
	now := c.now()
	if !c.alertErrAt.IsZero() && now.Sub(c.alertErrAt) < 5*time.Second {
		c.mu.Unlock()
		return
	}
	c.alertErrAt = now
	c.mu.Unlock()
	c.d.logs.Debugf(clockAppID, "", "%s: %v", what, err)
}

// logCountdownFailure reports a broken overlay at most once per five seconds;
// a timer can run for days and must not fill the log with retry noise.
func (c *builtinClock) logCountdownFailure(what string, err error) {
	c.mu.Lock()
	now := c.now()
	if !c.countErrAt.IsZero() && now.Sub(c.countErrAt) < 5*time.Second {
		c.mu.Unlock()
		return
	}
	c.countErrAt = now
	c.mu.Unlock()
	c.d.logs.Debugf(clockAppID, "", "%s: %v", what, err)
}

// ringPixelCount reads the proven ring geometry; a missing capability means no
// countdown rather than a guessed pixel count.
func (c *builtinClock) ringPixelCount() int {
	if c.ringID == "" {
		return 0
	}
	if info := c.d.resources[c.ringID]; info != nil && info.GetRing() != nil {
		return int(info.GetRing().GetPixelCount())
	}
	return 0
}

const stopwatchCueID = "stopwatch"

// ringCountdownColor is the warm orange used by both clock ring cues: the timer
// arc extinguishes, while a running stopwatch pulses the whole ring, so the two
// states are distinguishable by motion rather than by a second color.
var ringCountdownColor = backend.RGB{R: 255, G: 96, B: 24}

// countdownLitPixels maps the remaining fraction onto ring geometry. Pixel 0
// is the top and indices advance clockwise, so extinguishing counterclockwise
// from the top means the top pixel goes first, then N-1 down toward index 1.
// The lit arc is contiguous: 1..lit, or the whole ring when nothing has
// extinguished yet.
func countdownLitPixels(pixels int, durationNS, remainingNS uint64) int {
	if pixels <= 0 || durationNS == 0 || remainingNS == 0 {
		return 0
	}
	if remainingNS >= durationNS {
		return pixels
	}
	lit := int((remainingNS*uint64(pixels) + durationNS - 1) / durationNS)
	if lit < 1 {
		lit = 1
	}
	if lit > pixels {
		lit = pixels
	}
	return lit
}

// countdownRingPixels renders the lit arc. When the arc is partial it starts at
// index 1 because index 0 (top) is the first pixel to go dark.
func countdownRingPixels(pixels, lit int) []backend.RGB {
	if pixels <= 0 {
		return nil
	}
	out := make([]backend.RGB, pixels)
	if lit <= 0 {
		return out
	}
	if lit >= pixels {
		for i := range out {
			out[i] = ringCountdownColor
		}
		return out
	}
	for i := 1; i <= lit; i++ {
		out[i] = ringCountdownColor
	}
	return out
}

func ringFramePresentationFrame(pixels []backend.RGB) *frameworkv1.PresentationFrame {
	out := make([]*frameworkv1.RgbPixel, 0, len(pixels))
	for _, px := range pixels {
		out = append(out, &frameworkv1.RgbPixel{
			Red: uint32(px.R), Green: uint32(px.G), Blue: uint32(px.B),
		})
	}
	return &frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_Ring{
		Ring: &frameworkv1.RingFrame{Pixels: out},
	}}
}

func (c *builtinClock) baselineDisplayLocked(now time.Time, mono uint64) clockDisplay {
	if t := c.nearestRunningTimerLocked(); t != nil {
		remaining := uint64(0)
		if t.deadlineNS > mono {
			remaining = t.deadlineNS - mono
		}
		return durationDisplay(remaining, true)
	}
	if c.stopwatch.running || c.stopwatch.elapsedNS > 0 {
		elapsed := c.stopwatch.elapsedNS
		if c.stopwatch.running {
			elapsed += mono - c.stopwatch.startedNS
		}
		return durationDisplay(elapsed, true)
	}
	switch c.displayMode {
	case "stopwatch":
		return durationDisplay(0, true)
	case "timer":
		return durationDisplay(c.timerSecs*1_000_000_000, true)
	default:
		loc := c.locationLocked()
		return clockDisplayFor(now.In(loc), c.blink)
	}
}

func (c *builtinClock) nearestRunningTimerLocked() *clockTimer {
	var best *clockTimer
	for _, id := range c.timerOrder {
		t := c.timers[id]
		if t == nil || t.status != timerRunning {
			continue
		}
		if best == nil || t.deadlineNS < best.deadlineNS {
			best = t
		}
	}
	return best
}

func (c *builtinClock) alertDisplayLocked(a *clockAlert, now time.Time) clockDisplay {
	if a.kind == alertAlarm {
		if alarm := c.alarms[a.sourceID]; alarm != nil && alarm.lastTriggeredMS != 0 {
			// Show when the alarm actually fired in its evaluation zone, not
			// the current wall clock.
			loc := alarmLocation(alarm, c.alarmLocationLocked())
			at := time.UnixMilli(int64(alarm.lastTriggeredMS)).In(loc)
			return clockDisplay{
				cells: fourDigits(at.Hour()*100 + at.Minute()),
				colon: now.UnixMilli()/500%2 == 0,
			}
		}
	}
	cells := fourDigits(0)
	colons := now.UnixMilli()/500%2 == 0
	return clockDisplay{cells: cells, colon: colons}
}

func clockDisplayFor(at time.Time, blink bool) clockDisplay {
	out := clockDisplay{cells: fourDigits(at.Hour()*100 + at.Minute()), colon: true}
	if blink {
		out.colon = at.Second()%2 == 0
	}
	return out
}

// durationDisplay renders a nanosecond duration as mm:ss below 100 minutes and
// hh:mm above, so the same four cells stay meaningful without a decimal point.
func durationDisplay(ns uint64, colon bool) clockDisplay {
	total := ns / 1_000_000_000
	if ns%1_000_000_000 != 0 {
		total++
	}
	var value int
	if total < 100*60 {
		value = int(total/60)*100 + int(total%60)
	} else {
		hours := total / 3600
		if hours > 99 {
			hours = 99
		}
		value = int(hours)*100 + int((total%3600)/60)
	}
	return clockDisplay{cells: fourDigits(value), colon: colon}
}

func fourDigits(v int) [4]uint16 {
	if v < 0 {
		v = 0
	}
	if v > 9999 {
		v = 9999
	}
	return [4]uint16{
		clockDigitMasks[v/1000%10], clockDigitMasks[v/100%10],
		clockDigitMasks[v/10%10], clockDigitMasks[v%10],
	}
}

// validateClockSettings rejects semantically invalid values before they are
// stored. The generic store checks types and manifest bounds; timezone names
// and select values need an app-level check.
func validateClockSettings(values map[string]json.RawMessage) error {
	text := func(key string) (string, bool) {
		raw, ok := values[key]
		if !ok {
			return "", false
		}
		var out string
		if json.Unmarshal(raw, &out) != nil {
			return "", false
		}
		return out, true
	}
	if tz, ok := text("timezone"); ok && tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("timezone %q is not an IANA zone", tz)
		}
	}
	if mode, ok := text("display_mode"); ok {
		switch mode {
		case "clock", "stopwatch", "timer":
		default:
			return fmt.Errorf("display_mode %q is not a supported mode", mode)
		}
	}
	if gesture, ok := text("stopwatch_reset_gesture"); ok {
		switch gesture {
		case "hold", "double_tap":
		default:
			return fmt.Errorf("stopwatch_reset_gesture %q is not supported", gesture)
		}
	}
	if raw, ok := values["timer_seconds"]; ok {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > 86400 {
			return fmt.Errorf("timer_seconds must be between 1 and 86400")
		}
	}
	if alarms, ok := text("alarms"); ok {
		entries, err := dcron.ParseEntries(alarms)
		if err != nil {
			return fmt.Errorf("alarms: %w", err)
		}
		for _, entry := range entries {
			if entry.Zone != "" {
				if _, err := time.LoadLocation(entry.Zone); err != nil {
					return fmt.Errorf("alarms: %q is not an IANA zone", entry.Zone)
				}
			}
		}
	}
	if raw, ok := values["alarm_labels"]; ok {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return fmt.Errorf("alarm_labels must be text")
		}
		var labels map[string]string
		if text != "" {
			if err := json.Unmarshal([]byte(text), &labels); err != nil {
				return fmt.Errorf("alarm_labels must be a JSON object of identity to label: %w", err)
			}
		}
		if len(labels) > 64 {
			return fmt.Errorf("alarm_labels has too many entries")
		}
		for id, label := range labels {
			if _, err := dcron.ParseEntry(id); err != nil {
				return fmt.Errorf("alarm_labels key %q is not an alarm identity", id)
			}
			if len(label) > 80 {
				return fmt.Errorf("alarm label for %q is too long", id)
			}
		}
	}
	return nil
}

// --- service calls ----------------------------------------------------------

// handleCall answers the public ClockService over the app bus. System-app
// calls are in-process, but the wire shape is identical to a supervised app's
// SDK: binary protobuf requests and responses, including every WatchState
// snapshot.
func (c *builtinClock) handleCall(ctx context.Context, service, rpc string, request []byte, emit func([]byte) error) error {
	if service != clockServiceName {
		return status.Errorf(codes.Unimplemented, "clock app does not implement %s", service)
	}
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if !running {
		return status.Error(codes.FailedPrecondition, "clock app is not running")
	}
	var resp proto.Message
	switch rpc {
	case "GetState":
		if err := decodeClockRequest(request, &clockv1.GetStateRequest{}); err != nil {
			return err
		}
		resp = c.state()
	case "WatchState":
		if err := decodeClockRequest(request, &clockv1.WatchStateRequest{}); err != nil {
			return err
		}
		return c.watchState(ctx, emit)
	case "StopwatchStart", "StopwatchPause", "StopwatchResume", "StopwatchReset":
		if err := decodeClockRequest(request, &clockv1.StopwatchCommandRequest{}); err != nil {
			return err
		}
		resp = c.stopwatchCommand(rpc)
	case "StartTimer":
		req := &clockv1.StartTimerRequest{}
		if err := decodeClockRequest(request, req); err != nil {
			return err
		}
		if req.DurationMs == 0 || req.DurationMs > maxTimerMS {
			return status.Errorf(codes.InvalidArgument, "duration_ms must be between 1 and %d", maxTimerMS)
		}
		resp = c.startTimer(req.DurationMs, req.Label)
	case "CancelTimer":
		req := &clockv1.CancelTimerRequest{}
		if err := decodeClockRequest(request, req); err != nil {
			return err
		}
		timer, err := c.cancelTimer(req.TimerId)
		if err != nil {
			return err
		}
		resp = timer
	case "ListTimers":
		if err := decodeClockRequest(request, &clockv1.ListTimersRequest{}); err != nil {
			return err
		}
		resp = c.listTimers()
	case "ListAlarms":
		if err := decodeClockRequest(request, &clockv1.ListAlarmsRequest{}); err != nil {
			return err
		}
		resp = c.listAlarms()
	case "AddAlarm":
		req := &clockv1.AddAlarmRequest{}
		if err := decodeClockRequest(request, req); err != nil {
			return err
		}
		alarm, err := c.addAlarm(req.Schedule, req.Timezone, req.Label, req.OneShot, req.Disabled, req.ExpectedRevision)
		if err != nil {
			return err
		}
		resp = alarm
	case "DeleteAlarm":
		req := &clockv1.DeleteAlarmRequest{}
		if err := decodeClockRequest(request, req); err != nil {
			return err
		}
		out, err := c.deleteAlarm(req.AlarmId, req.ExpectedRevision)
		if err != nil {
			return err
		}
		resp = out
	case "SetAlarmEnabled":
		req := &clockv1.SetAlarmEnabledRequest{}
		if err := decodeClockRequest(request, req); err != nil {
			return err
		}
		alarm, err := c.setAlarmEnabled(req.AlarmId, req.Enabled, req.ExpectedRevision)
		if err != nil {
			return err
		}
		resp = alarm
	case "DismissAlert":
		req := &clockv1.DismissAlertRequest{}
		if err := decodeClockRequest(request, req); err != nil {
			return err
		}
		resp = &clockv1.DismissAlertResponse{Dismissed: c.dismissAlertCount(req.AlertId)}
	default:
		return status.Errorf(codes.Unimplemented, "unknown ClockService method %s", rpc)
	}
	payload, err := encodeClockResponse(resp)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return emit(payload)
}

// watchState streams a full snapshot first and then every observable change.
// A bounded one-slot subscription means a slow caller converges on the newest
// state instead of growing a backlog of stale transitions.
func (c *builtinClock) watchState(ctx context.Context, emit func([]byte) error) error {
	ch, cancel := c.subscribeState()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case snap, ok := <-ch:
			if !ok {
				return nil
			}
			payload, err := encodeClockResponse(snap)
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			if err := emit(payload); err != nil {
				return err
			}
		}
	}
}

func encodeClockResponse(msg proto.Message) ([]byte, error) {
	return proto.Marshal(msg)
}

// decodeClockRequest parses the request exactly as it arrives on AppBus. The
// public wire is binary protobuf; an empty request is a valid empty message
// for the RPCs that take no fields.
func decodeClockRequest(request []byte, msg proto.Message) error {
	if err := proto.Unmarshal(request, msg); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func (c *builtinClock) state() *clockv1.ClockState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked(c.mono())
}

// stateLocked builds the authoritative snapshot. Callers hold c.mu.
func (c *builtinClock) stateLocked(mono uint64) *clockv1.ClockState {
	now := c.now()
	loc := c.locationLocked()
	out := &clockv1.ClockState{
		LocalTimeRfc3339: now.In(loc).Format(time.RFC3339),
		Timezone:         loc.String(),
		Stopwatch:        c.stopwatchInfoLocked(mono),
		Revision:         c.watchRevision,
		AlarmsSetting:    c.alarmTextLocked(),
	}
	out.Timers = c.timersLocked(mono)
	out.Alarms = c.alarmsLocked()
	for _, id := range c.alertOrder {
		a := c.alerts[id]
		if a == nil {
			continue
		}
		kind := clockv1.AlertKind_ALERT_KIND_TIMER
		if a.kind == alertAlarm {
			kind = clockv1.AlertKind_ALERT_KIND_ALARM
		}
		out.ActiveAlerts = append(out.ActiveAlerts, &clockv1.ActiveAlert{
			AlertId: a.id, Kind: kind, SourceId: a.sourceID, Label: a.label,
			StartedUnixMs: a.startedMS,
		})
	}
	return out
}

func (c *builtinClock) alarmTextLocked() string {
	entries := make([]dcron.Entry, 0, len(c.alarmOrder))
	for _, id := range c.alarmOrder {
		if a := c.alarms[id]; a != nil {
			entries = append(entries, a.entry())
		}
	}
	return dcron.FormatEntries(entries)
}

// subscribeState registers a converging snapshot stream. The first snapshot is
// always delivered; a slow subscriber keeps only the newest one.
func (c *builtinClock) subscribeState() (<-chan *clockv1.ClockState, func()) {
	ch := make(chan *clockv1.ClockState, 1)
	c.mu.Lock()
	c.nextWatcher++
	id := c.nextWatcher
	c.watchers[id] = ch
	ch <- c.stateLocked(c.mono())
	c.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.mu.Lock()
			if cur, ok := c.watchers[id]; ok {
				delete(c.watchers, id)
				close(cur)
			}
			c.mu.Unlock()
		})
	}
}

// broadcastState advances the watch revision on an observable change and
// pushes the newest snapshot to every subscriber.
func (c *builtinClock) broadcastState() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return
	}
	sig := c.watchSignatureLocked()
	if sig == c.watchSig {
		// Idle state is unchanged: do not allocate a snapshot or wake watchers.
		return
	}
	c.watchSig = sig
	c.watchRevision++
	if len(c.watchers) == 0 {
		return
	}
	snap := c.stateLocked(c.mono())
	// Sends are non-blocking and happen under the same lock that closes
	// subscription channels, so a cancel can never race into a send-on-closed.
	for _, ch := range c.watchers {
		pushLatest(ch, snap)
	}
}

// watchSignatureLocked captures every watch-visible transition. Local time is
// included to the minute while idle and to the second while a stopwatch or
// timer is running, so an idle clock does not stream once per tick.
func (c *builtinClock) watchSignatureLocked() string {
	loc := c.locationLocked()
	now := c.now().In(loc)
	granularity := "2006-01-02T15:04"
	sw := c.stopwatchInfoLocked(c.mono())
	if sw.Status != clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE {
		granularity = "2006-01-02T15:04:05"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%d|%d", now.Format(granularity), loc, sw.Status, sw.ElapsedMs)
	for _, t := range c.timersLocked(c.mono()) {
		fmt.Fprintf(&b, "|t:%s:%d:%d", t.TimerId, t.Status, t.RemainingMs)
	}
	for _, id := range c.alertOrder {
		if a := c.alerts[id]; a != nil {
			fmt.Fprintf(&b, "|a:%s:%d", a.id, a.startedMS)
		}
	}
	for _, id := range c.alarmOrder {
		if a := c.alarms[id]; a != nil {
			fmt.Fprintf(&b, "|m:%s:%d:%d:%s", a.id, a.next.Unix(), a.lastTriggeredMS, c.alertForAlarmLocked(id))
		}
	}
	return b.String()
}

func (c *builtinClock) stopwatchCommand(rpc string) *clockv1.StopwatchState {
	c.mu.Lock()
	mono := c.mono()
	switch rpc {
	case "StopwatchStart":
		if !c.stopwatch.running {
			c.stopwatch.running = true
			c.stopwatch.elapsedNS = 0
			c.stopwatch.startedNS = mono
			c.stopwatch.startedMS = uint64(c.now().UnixMilli())
		}
	case "StopwatchPause":
		if c.stopwatch.running {
			c.stopwatch.elapsedNS += mono - c.stopwatch.startedNS
			c.stopwatch.running = false
		}
	case "StopwatchResume":
		if !c.stopwatch.running {
			c.stopwatch.running = true
			c.stopwatch.startedNS = mono
			if c.stopwatch.startedMS == 0 {
				c.stopwatch.startedMS = uint64(c.now().UnixMilli())
			}
		}
	case "StopwatchReset":
		c.stopwatch.running = false
		c.stopwatch.elapsedNS = 0
		c.stopwatch.startedMS = 0
	}
	info := c.stopwatchInfoLocked(mono)
	c.mu.Unlock()
	c.publish()
	return info
}

// stopwatchEngaged reports whether the stopwatch owns the primary button: it is
// running or paused with elapsed time.
func (c *builtinClock) stopwatchEngaged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return false
	}
	return c.stopwatch.running || c.stopwatch.elapsedNS > 0
}

// stopwatchToggle pauses a running stopwatch or resumes a paused one. It
// reports false when no stopwatch is engaged, so the press keeps its normal
// assistant meaning.
func (c *builtinClock) stopwatchToggle() bool {
	c.mu.Lock()
	mono := c.mono()
	if !c.running || (!c.stopwatch.running && c.stopwatch.elapsedNS == 0) {
		c.mu.Unlock()
		return false
	}
	paused := c.stopwatch.running
	if paused {
		c.stopwatch.elapsedNS += mono - c.stopwatch.startedNS
		c.stopwatch.running = false
	} else {
		c.stopwatch.running = true
		c.stopwatch.startedNS = mono
		if c.stopwatch.startedMS == 0 {
			c.stopwatch.startedMS = uint64(c.now().UnixMilli())
		}
	}
	c.mu.Unlock()
	if paused {
		c.d.logs.Infof(clockAppID, "", "stopwatch paused by the primary key")
	} else {
		c.d.logs.Infof(clockAppID, "", "stopwatch resumed by the primary key")
	}
	c.publish()
	return true
}

// stopwatchResetGesture resets a running or paused stopwatch only when the
// configured reset gesture matches. The press is still consumed by the caller,
// so a non-matching hold or double tap never summons the assistant.
func (c *builtinClock) stopwatchResetGesture(gesture string) bool {
	c.mu.Lock()
	engaged := c.running && (c.stopwatch.running || c.stopwatch.elapsedNS > 0)
	if !engaged || c.stopwatchReset != gesture {
		c.mu.Unlock()
		return false
	}
	c.stopwatch.running = false
	c.stopwatch.elapsedNS = 0
	c.stopwatch.startedMS = 0
	c.mu.Unlock()
	c.d.logs.Infof(clockAppID, "", "stopwatch reset and dismissed by a %s", gesture)
	c.publish()
	return true
}

// primaryGesture dispatches a framework-owned primary-key gesture to the
// stopwatch. Every gesture that engages the stopwatch is consumed.
func (c *builtinClock) primaryGesture(kind string) bool {
	switch kind {
	case "tap":
		return c.stopwatchToggle()
	case "hold", "double_tap":
		return c.stopwatchResetGesture(kind)
	}
	return false
}

func (c *builtinClock) stopwatchInfoLocked(mono uint64) *clockv1.StopwatchState {
	elapsed := c.stopwatch.elapsedNS
	state := clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE
	if c.stopwatch.running {
		elapsed += mono - c.stopwatch.startedNS
		state = clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING
	} else if elapsed > 0 {
		state = clockv1.StopwatchStatus_STOPWATCH_STATUS_PAUSED
	}
	return &clockv1.StopwatchState{
		Status: state, ElapsedMs: elapsed / 1_000_000, StartedUnixMs: c.stopwatch.startedMS,
	}
}

func (c *builtinClock) startTimer(durationMS uint64, label string) *clockv1.Timer {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("tmr_%d", c.nextID)
	now := c.now()
	mono := c.mono()
	if label == "" {
		label = "Timer"
	}
	t := &clockTimer{
		id: id, label: label, durationNS: durationMS * 1_000_000,
		startedMS: uint64(now.UnixMilli()), status: timerRunning,
	}
	t.deadlineNS = mono + t.durationNS
	c.timers[id] = t
	c.timerOrder = append(c.timerOrder, id)
	out := timerInfo(t, mono)
	c.mu.Unlock()
	c.d.logs.Infof(clockAppID, "", "timer %s (%s) started for %d ms", id, label, durationMS)
	c.publish()
	return out
}

func (c *builtinClock) cancelTimer(id string) (*clockv1.Timer, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "timer_id is required")
	}
	c.mu.Lock()
	t := c.timers[id]
	if t == nil {
		c.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "timer %s is not active", id)
	}
	if t.status != timerRunning {
		c.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "timer %s is %s", id, timerStatusName(t.status))
	}
	mono := c.mono()
	out := timerInfo(t, mono)
	out.Status = clockv1.TimerStatus_TIMER_STATUS_CANCELLED
	out.RemainingMs = 0
	out.FinishedUnixMs = uint64(c.now().UnixMilli())
	delete(c.timers, id)
	for i, cur := range c.timerOrder {
		if cur == id {
			c.timerOrder = append(c.timerOrder[:i], c.timerOrder[i+1:]...)
			break
		}
	}
	label := t.label
	c.mu.Unlock()
	c.d.logs.Infof(clockAppID, "", "timer %s (%s) cancelled", id, label)
	c.publish()
	return out, nil
}

func (c *builtinClock) listTimers() *clockv1.ListTimersResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &clockv1.ListTimersResponse{Timers: c.timersLocked(c.mono())}
}

func (c *builtinClock) timersLocked(mono uint64) []*clockv1.Timer {
	out := make([]*clockv1.Timer, 0, len(c.timerOrder))
	for _, id := range c.timerOrder {
		t := c.timers[id]
		if t == nil {
			continue
		}
		out = append(out, timerInfo(t, mono))
	}
	return out
}

func timerInfo(t *clockTimer, mono uint64) *clockv1.Timer {
	remaining := uint64(0)
	if t.status == timerRunning && t.deadlineNS > mono {
		remaining = (t.deadlineNS - mono) / 1_000_000
	}
	status := clockv1.TimerStatus_TIMER_STATUS_RUNNING
	switch t.status {
	case timerCompleted:
		status = clockv1.TimerStatus_TIMER_STATUS_COMPLETED
	case timerCancelled:
		status = clockv1.TimerStatus_TIMER_STATUS_CANCELLED
	}
	return &clockv1.Timer{
		TimerId: t.id, Label: t.label, DurationMs: t.durationNS / 1_000_000,
		RemainingMs: remaining, Status: status, StartedUnixMs: t.startedMS,
		FinishedUnixMs: t.finishedMS,
	}
}

func timerStatusName(s clockTimerStatus) string {
	switch s {
	case timerCompleted:
		return "completed"
	case timerCancelled:
		return "cancelled"
	default:
		return "running"
	}
}

// alarmEntriesSetting returns the current canonical entries and revision.
func (c *builtinClock) alarmEntriesSetting() ([]dcron.Entry, uint64, error) {
	snap, err := c.d.appCfg.Snapshot(clockAppID)
	if err != nil {
		return nil, 0, err
	}
	for _, v := range snap.Values {
		if v.Key != "alarms" {
			continue
		}
		var text string
		if v.HasValue && json.Unmarshal([]byte(v.Value), &text) == nil {
			entries, err := dcron.ParseEntries(text)
			return entries, snap.Revision, err
		}
	}
	return nil, snap.Revision, nil
}

func (c *builtinClock) alarmLabelSetting() (map[string]string, error) {
	snap, err := c.d.appCfg.Snapshot(clockAppID)
	if err != nil {
		return nil, err
	}
	for _, v := range snap.Values {
		if v.Key != "alarm_labels" || !v.HasValue {
			continue
		}
		var text string
		if json.Unmarshal([]byte(v.Value), &text) != nil || text == "" {
			return map[string]string{}, nil
		}
		var labels map[string]string
		if err := json.Unmarshal([]byte(text), &labels); err != nil {
			return nil, err
		}
		return labels, nil
	}
	return map[string]string{}, nil
}

func (c *builtinClock) writeAlarmState(entries []dcron.Entry, labels map[string]string, expected uint64) (settings.AppSnapshot, error) {
	encoded, err := json.Marshal(labels)
	if err != nil {
		return settings.AppSnapshot{}, err
	}
	snap, _, _, err := c.d.appCfg.Update(clockAppID, expected, []settings.Change{
		{Key: "alarms", Value: dcron.FormatEntries(entries)},
		{Key: "alarm_labels", Value: string(encoded)},
	})
	return snap, err
}

// addAlarm appends one schedule to the canonical `alarms` app setting. The
// store's normal CAS and validator decide whether the new value is accepted,
// and the settings watcher rebuilds the running alarm table.
func (c *builtinClock) addAlarm(schedule, zone, label string, oneShot, disabled bool, expectedRevision uint64) (*clockv1.Alarm, error) {
	sched, err := dcron.Parse(schedule)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "schedule: %v", err)
	}
	if zone != "" {
		if _, err := time.LoadLocation(zone); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "timezone %q is not an IANA zone", zone)
		}
	}
	entry := dcron.Entry{Expr: sched.Text, Zone: zone, Once: oneShot, Disabled: disabled}
	current, rev, err := c.alarmEntriesSetting()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	labels, err := c.alarmLabelSetting()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if expectedRevision != 0 && expectedRevision != rev {
		return nil, status.Errorf(codes.Aborted, "app settings revision %d is stale (current %d)", expectedRevision, rev)
	}
	if label != "" {
		labels[entry.ID()] = label
	}
	for _, existing := range current {
		if existing.ID() == entry.ID() {
			snap, err := c.writeAlarmState(current, labels, rev)
			if err != nil {
				return nil, c.d.appSettingsStatus(clockAppID, err)
			}
			c.applySettings(snap.Revision, snap.Values)
			return c.alarmInfoForEntry(existing), nil
		}
	}
	next := append(append([]dcron.Entry(nil), current...), entry)
	snap, err := c.writeAlarmState(next, labels, rev)
	if err != nil {
		return nil, c.d.appSettingsStatus(clockAppID, err)
	}
	// Apply synchronously so a GetState immediately after AddAlarm observes
	// the new entry; the settings watcher will apply the same revision again.
	c.applySettings(snap.Revision, snap.Values)
	return c.alarmInfoForEntry(entry), nil
}

// deleteAlarm removes an alarm by its stable entry identity.
func (c *builtinClock) deleteAlarm(alarmID string, expectedRevision uint64) (*clockv1.DeleteAlarmResponse, error) {
	target, err := dcron.ParseEntry(alarmID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "alarm_id: %v", err)
	}
	current, rev, err := c.alarmEntriesSetting()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	labels, err := c.alarmLabelSetting()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if expectedRevision != 0 && expectedRevision != rev {
		return nil, status.Errorf(codes.Aborted, "app settings revision %d is stale (current %d)", expectedRevision, rev)
	}
	next := make([]dcron.Entry, 0, len(current))
	found := false
	for _, existing := range current {
		if existing.ID() == target.ID() {
			found = true
			continue
		}
		next = append(next, existing)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "alarm %s not found", target.ID())
	}
	delete(labels, target.ID())
	snap, err := c.writeAlarmState(next, labels, rev)
	if err != nil {
		return nil, c.d.appSettingsStatus(clockAppID, err)
	}
	c.mu.Lock()
	delete(c.selfDisabledIDs, target.ID())
	c.mu.Unlock()
	c.applySettings(snap.Revision, snap.Values)
	c.dropAlarmAlert(target.ID())
	return &clockv1.DeleteAlarmResponse{Deleted: true}, nil
}

// setAlarmEnabled toggles the persisted disabled marker. An explicit disable
// dismisses that alarm's active alert; the clock's own one-shot disable does
// not, so a ringing one-shot still waits for the user's dismissal.
func (c *builtinClock) setAlarmEnabled(alarmID string, enabled bool, expectedRevision uint64) (*clockv1.Alarm, error) {
	target, err := dcron.ParseEntry(alarmID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "alarm_id: %v", err)
	}
	current, rev, err := c.alarmEntriesSetting()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	labels, err := c.alarmLabelSetting()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if expectedRevision != 0 && expectedRevision != rev {
		return nil, status.Errorf(codes.Aborted, "app settings revision %d is stale (current %d)", expectedRevision, rev)
	}
	found := false
	next := make([]dcron.Entry, 0, len(current))
	for _, existing := range current {
		if existing.ID() == target.ID() {
			found = true
			existing.Disabled = !enabled
			target = existing
		}
		next = append(next, existing)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "alarm %s not found", target.ID())
	}
	snap, err := c.writeAlarmState(next, labels, rev)
	if err != nil {
		return nil, c.d.appSettingsStatus(clockAppID, err)
	}
	// Clear the one-shot self-disable marker only after the write commits.
	// A stale/failed CAS therefore leaves the ringing one-shot untouched.
	c.mu.Lock()
	delete(c.selfDisabledIDs, target.ID())
	c.mu.Unlock()
	c.applySettings(snap.Revision, snap.Values)
	if !enabled {
		c.dropAlarmAlert(target.ID())
	}
	return c.alarmInfoForEntry(target), nil
}

// disableAlarmEntries persists the !disabled marker after a one-shot fires.
// The alert keeps ringing until the user dismisses it, matching the previous
// one-shot alarm behavior.
func (c *builtinClock) disableAlarmEntries(ids []string) {
	if len(ids) == 0 {
		return
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	// A concurrent settings edit can invalidate the read revision; retry a
	// bounded number of times before leaving the in-memory disable in place.
	for range 3 {
		current, rev, err := c.alarmEntriesSetting()
		if err != nil {
			c.d.logs.Warnf(clockAppID, "", "disable one-shot alarms: %v", err)
			return
		}
		labels, err := c.alarmLabelSetting()
		if err != nil {
			c.d.logs.Warnf(clockAppID, "", "disable one-shot alarms: %v", err)
			return
		}
		changed := false
		for i := range current {
			if want[current[i].ID()] && !current[i].Disabled {
				current[i].Disabled = true
				changed = true
			}
		}
		if !changed {
			return
		}
		if _, err := c.writeAlarmState(current, labels, rev); err == nil {
			return
		}
	}
	c.d.logs.Warnf(clockAppID, "", "could not persist one-shot alarm disable after concurrent settings changes")
}

func (c *builtinClock) dropAlarmAlert(id string) {
	c.mu.Lock()
	dismissed := c.removeAlertsLocked(func(alert *clockAlert) bool {
		return alert.kind == alertAlarm && alert.sourceID == id
	})
	c.mu.Unlock()
	if dismissed > 0 {
		c.d.logs.Infof(clockAppID, "", "alarm %s disabled; %d active alert(s) dismissed", id, dismissed)
		c.reconcileAlertAudio()
		c.publish()
	}
}

func (c *builtinClock) listAlarms() *clockv1.ListAlarmsResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &clockv1.ListAlarmsResponse{Alarms: c.alarmsLocked()}
}

func (c *builtinClock) alarmsLocked() []*clockv1.Alarm {
	device := c.alarmLocationLocked()
	out := make([]*clockv1.Alarm, 0, len(c.alarmOrder))
	for _, id := range c.alarmOrder {
		a := c.alarms[id]
		if a == nil {
			continue
		}
		out = append(out, alarmInfo(a, alarmLocation(a, device), c.alertForAlarmLocked(id)))
	}
	return out
}

func (c *builtinClock) alertForAlarmLocked(id string) string {
	for _, alertID := range c.alertOrder {
		if alert := c.alerts[alertID]; alert != nil && alert.kind == alertAlarm && alert.sourceID == id {
			return alert.id
		}
	}
	return ""
}

// alarmInfoForEntry builds the response for an entry the settings watcher may
// not have applied yet.
func (c *builtinClock) alarmInfoForEntry(entry dcron.Entry) *clockv1.Alarm {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.alarms[entry.ID()]; a != nil {
		return alarmInfo(a, alarmLocation(a, c.alarmLocationLocked()), c.alertForAlarmLocked(a.id))
	}
	sched, err := dcron.Parse(entry.Expr)
	if err != nil {
		return nil
	}
	a := &clockAlarm{
		id: entry.ID(), text: entry.Canonical(), expr: entry.Expr, label: "Alarm",
		zone: entry.Zone, once: entry.Once, disabled: entry.Disabled, sched: sched,
	}
	device := c.alarmLocationLocked()
	a.next = sched.Next(c.now(), alarmLocation(a, device))
	if entry.Disabled {
		a.next = time.Time{}
	}
	return alarmInfo(a, alarmLocation(a, device), c.alertForAlarmLocked(a.id))
}

func (c *builtinClock) alarmState(id string) *clockv1.Alarm {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.alarms[id]
	if a == nil {
		return nil
	}
	return alarmInfo(a, alarmLocation(a, c.alarmLocationLocked()), c.alertForAlarmLocked(id))
}

func alarmInfo(a *clockAlarm, loc *time.Location, alertID string) *clockv1.Alarm {
	out := &clockv1.Alarm{
		AlarmId: a.id, Schedule: a.expr, Timezone: a.zone, Label: a.label,
		Enabled: !a.disabled, OneShot: a.once,
		EffectiveTimezone:   loc.String(),
		LastTriggeredUnixMs: a.lastTriggeredMS, AlertId: alertID, Ringing: alertID != "",
	}
	if !a.next.IsZero() {
		out.NextTriggerUnixMs = uint64(a.next.UnixMilli())
		out.NextTriggerRfc3339 = a.next.In(loc).Format(time.RFC3339)
	}
	return out
}

// --- actions ----------------------------------------------------------------

func (c *builtinClock) action(ctx context.Context, actionID string) error {
	c.mu.Lock()
	running := c.running
	timerSecs := c.timerSecs
	c.mu.Unlock()
	if !running {
		return fmt.Errorf("clock app is not running")
	}
	switch actionID {
	case "stopwatch_start":
		c.stopwatchCommand("StopwatchStart")
	case "stopwatch_pause":
		c.stopwatchCommand("StopwatchPause")
	case "stopwatch_resume":
		c.stopwatchCommand("StopwatchResume")
	case "stopwatch_reset":
		c.stopwatchCommand("StopwatchReset")
	case "timer_start":
		if timerSecs == 0 {
			timerSecs = 300
		}
		c.startTimer(timerSecs*1000, "Timer")
	case "timer_cancel":
		c.mu.Lock()
		id := ""
		for i := len(c.timerOrder) - 1; i >= 0; i-- {
			if t := c.timers[c.timerOrder[i]]; t != nil && t.status == timerRunning {
				id = t.id
				break
			}
		}
		c.mu.Unlock()
		if id != "" {
			if _, err := c.cancelTimer(id); err != nil {
				return err
			}
		}
	case "alert_dismiss":
		c.dismissAlert("")
	default:
		return fmt.Errorf("unknown clock action %q", actionID)
	}
	return nil
}

// --- alert audio ------------------------------------------------------------

func (c *builtinClock) alertAudioLocked(kind clockAlertKind) []byte {
	format := c.audioFormat
	key := fmt.Sprintf("%d/%d/%d", kind, format.RateHz, format.Channels)
	if data, ok := c.audioCache[key]; ok {
		return data
	}
	data := synthAlertPCM(kind, format)
	c.audioCache[key] = data
	return data
}

func synthAlertPCM(kind clockAlertKind, format backend.AudioFormat) []byte {
	rate := format.RateHz
	if rate <= 0 {
		rate = 48000
	}
	channels := format.Channels
	if channels <= 0 {
		channels = 2
	}
	seconds := 1.2
	if kind == alertTimer {
		seconds = 0.9
	}
	frames := int(float64(rate) * seconds)
	out := make([]byte, frames*channels*2)
	for i := range frames {
		t := float64(i) / float64(rate)
		var v float64
		if kind == alertAlarm {
			freq := 660.0
			if int(t*4)%2 == 1 {
				freq = 880.0
			}
			env := 0.5 - 0.5*math.Cos(2*math.Pi*1.4*t)
			v = 0.18 * env * math.Sin(2*math.Pi*freq*t)
		} else {
			beep := math.Mod(t, 0.3)
			if beep < 0.16 {
				env := math.Sin(math.Pi * beep / 0.16)
				v = 0.16 * env * math.Sin(2*math.Pi*880*t)
			}
		}
		if v > 0.25 {
			v = 0.25
		} else if v < -0.25 {
			v = -0.25
		}
		s := int16(v * 32767)
		for ch := 0; ch < channels; ch++ {
			i2 := (i*channels + ch) * 2
			out[i2] = byte(s)
			out[i2+1] = byte(s >> 8)
		}
	}
	return out
}

// reconcileAlertAudio keeps exactly one repeating audio loop for the currently
// active alert. Repeated publishes while the same alert rings are no-ops, so a
// 200 ms tick never accumulates goroutines or overlapping clips.
func (c *builtinClock) reconcileAlertAudio() {
	c.mu.Lock()
	var (
		alertID string
		kind    clockAlertKind
		pcm     []byte
		format  backend.AudioFormat
		client  *builtinAppClient
		ctx     context.Context
		play    func(context.Context, backend.AudioFormat, []byte) error
	)
	if c.running && c.client != nil && c.ctx != nil {
		if alert := c.activeAlertLocked(); alert != nil {
			alertID = alert.id
			kind = alert.kind
			pcm = c.alertAudioLocked(alert.kind)
			format = c.audioFormat
			client = c.client
			ctx = c.ctx
			play = c.playAudio
		}
	}
	c.mu.Unlock()

	c.audioMu.Lock()
	defer c.audioMu.Unlock()
	if alertID == "" {
		c.stopAlertAudioLocked()
		return
	}
	if c.audioAlertID == alertID && c.audioCancel != nil {
		return
	}
	c.stopAlertAudioLocked()
	if client == nil || ctx == nil || pcm == nil {
		return
	}
	if play == nil {
		play = client.Playback
	}
	wrapped := func(ctx context.Context, format backend.AudioFormat, pcm []byte) error {
		err := play(ctx, format, pcm)
		if err != nil && ctx.Err() == nil && c.d.ctx.Err() == nil {
			c.d.logs.Debugf(clockAppID, "", "alert audio: %v", err)
		}
		return err
	}
	gap := timerAlertGap
	if kind == alertAlarm {
		gap = alarmAlertGap
	}
	audioCtx, cancel := context.WithCancel(ctx)
	c.audioAlertID = alertID
	c.audioCancel = cancel
	c.d.logs.Infof(clockAppID, "", "%s alert %s audio repeating until dismissed", clockAlertKindName(kind), alertID)
	go alertAudioLoop(audioCtx, format, pcm, gap, wrapped)
}

func (c *builtinClock) stopAlertAudio() {
	c.audioMu.Lock()
	defer c.audioMu.Unlock()
	c.stopAlertAudioLocked()
}

func (c *builtinClock) stopAlertAudioLocked() {
	if c.audioCancel != nil {
		c.audioCancel()
		c.audioCancel = nil
	}
	c.audioAlertID = ""
}

// alertAudioLoop plays one clip, then waits gap before the next. Cancellation
// interrupts both the in-flight clip and the silence.
func alertAudioLoop(ctx context.Context, format backend.AudioFormat, pcm []byte, gap time.Duration,
	play func(context.Context, backend.AudioFormat, []byte) error,
) {
	if gap <= 0 {
		gap = time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		_ = play(ctx, format, pcm)
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(gap)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func clockAlertKindName(kind clockAlertKind) string {
	if kind == alertAlarm {
		return "alarm"
	}
	return "timer"
}
