package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	clockv1 "capsulad/gen/go/capsula/contracts/clock/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presets"
	"capsulad/internal/settings"
)

const (
	defaultAlarmID = "default"
	maxTimerMS     = uint64(30 * 24 * 60 * 60 * 1000)
	timerAlertMS   = uint64(10_000)
	alarmAlertMS   = uint64(60_000)
	tickInterval   = 200 * time.Millisecond
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
	id              string
	label           string
	hour            int
	minute          int
	days            uint8
	enabled         bool
	tz              string
	next            time.Time
	lastTriggeredMS uint64
}

type clockAlertKind int

const (
	alertTimer clockAlertKind = iota
	alertAlarm
)

type clockAlert struct {
	id          string
	kind        clockAlertKind
	sourceID    string
	label       string
	startedMS   uint64
	expiresMono uint64
	audioSent   bool
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

	applied        clockDisplay
	appliedOK      bool
	defaultDisable bool
	alertGen       uint64
	alertMu        sync.Mutex
	alertPres      map[string]*appPresentation
	alertCap       map[string]*appCapture
	alertSig       map[string]clockDisplay
	audioCache     map[string][]byte
}

func newBuiltinClock(d *Daemon, segmentID string) *builtinClock {
	c := &builtinClock{
		d: d, segmentID: segmentID,
		blink: true, displayMode: "clock", timerSecs: 300, loc: time.Local,
		timers: map[string]*clockTimer{}, alarms: map[string]*clockAlarm{},
		alerts:    map[string]*clockAlert{},
		alertPres: map[string]*appPresentation{}, alertCap: map[string]*appCapture{},
		alertSig: map[string]clockDisplay{}, audioCache: map[string][]byte{},
		now:  time.Now,
		mono: func() uint64 { return d.clock.NowNS() },
	}
	c.alarms[defaultAlarmID] = &clockAlarm{id: defaultAlarmID, label: "Alarm", hour: 7}
	c.alarmOrder = []string{defaultAlarmID}
	c.ringID = d.ringResourceID()
	return c
}

func (c *builtinClock) start() {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
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
	c.appliedOK = false
	if a := c.alarms[defaultAlarmID]; a != nil {
		c.alarms = map[string]*clockAlarm{defaultAlarmID: a}
		c.alarmOrder = []string{defaultAlarmID}
	} else {
		c.alarms = map[string]*clockAlarm{}
		c.alarmOrder = nil
	}
	c.client = newBuiltinAppClient(c.d, clockAppID)
	if c.segmentID != "" {
		pres, err := c.client.OpenPresentation(c.ctx, c.segmentID)
		if err != nil {
			c.d.logs.Errorf(clockAppID, "", "open baseline presentation: %v", err)
		} else {
			c.pres = pres
		}
	}
	if format, err := c.client.PlaybackFormat(c.ctx); err == nil {
		c.audioFormat = format
	} else {
		c.d.logs.Warnf(clockAppID, "", "playback format unavailable: %v", err)
	}
	ctx := c.ctx
	for _, a := range c.alarms {
		a.next = nextAlarmTrigger(a, c.now(), c.alarmLocationLocked(a))
	}
	c.mu.Unlock()

	c.loadSettings(ctx)
	c.publish()
	go c.loop(ctx)
}

func (c *builtinClock) stop() {
	c.alertMu.Lock()
	defer c.alertMu.Unlock()
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	c.running = false
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
	c.alertPres = map[string]*appPresentation{}
	c.alertCap = map[string]*appCapture{}
	c.alertSig = map[string]clockDisplay{}
	c.timers = map[string]*clockTimer{}
	c.timerOrder = nil
	c.alerts = map[string]*clockAlert{}
	c.alertOrder = nil
	c.appliedOK = false
	c.alertGen++
	c.mu.Unlock()

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

func (c *builtinClock) loadSettings(ctx context.Context) {
	snap, ch, cancel, err := c.d.appCfg.WatchApp(clockAppID)
	if err != nil {
		c.d.logs.Warnf(clockAppID, "", "settings unavailable: %v", err)
		return
	}
	c.applySettings(snap.Values)
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
				c.applySettings(next.Values)
			}
		}
	}()
}

func (c *builtinClock) applySettings(values []settings.AppSettingView) {
	c.mu.Lock()
	blink := c.blink
	tz := c.appTZ
	mode := c.displayMode
	timerSecs := c.timerSecs
	a := c.alarms[defaultAlarmID]
	alarmLabel, alarmHour, alarmMinute := "Alarm", 7, 0
	alarmDays, alarmTZ, alarmEnabled := 0, "", false
	if a != nil {
		alarmLabel, alarmHour, alarmMinute = a.label, a.hour, a.minute
		alarmDays, alarmTZ, alarmEnabled = int(a.days), a.tz, a.enabled
	}
	c.mu.Unlock()

	decodeSetting(values, "blink", &blink)
	decodeSetting(values, "timezone", &tz)
	decodeSetting(values, "display_mode", &mode)
	var secs int64
	if decodeSetting(values, "timer_seconds", &secs) && secs > 0 {
		timerSecs = uint64(secs)
	}
	var label string
	if decodeSetting(values, "alarm_label", &label) && label != "" {
		alarmLabel = label
	}
	var hour int64
	if decodeSetting(values, "alarm_hour", &hour) && hour >= 0 && hour < 24 {
		alarmHour = int(hour)
	}
	var minute int64
	if decodeSetting(values, "alarm_minute", &minute) && minute >= 0 && minute < 60 {
		alarmMinute = int(minute)
	}
	var days int64
	if decodeSetting(values, "alarm_days", &days) && days >= 0 && days < 128 {
		alarmDays = int(days)
	}
	var atz string
	if decodeSetting(values, "alarm_timezone", &atz) {
		alarmTZ = atz
	}
	var enabled bool
	if decodeSetting(values, "alarm_enabled", &enabled) {
		alarmEnabled = enabled
	}

	switch mode {
	case "clock", "stopwatch", "timer":
	default:
		mode = "clock"
	}
	if timerSecs == 0 {
		timerSecs = 300
	}

	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			c.d.logs.Warnf(clockAppID, "", "timezone %q unavailable (%v); using the device timezone", tz, err)
		}
	}
	if alarmTZ != "" {
		if _, err := time.LoadLocation(alarmTZ); err != nil {
			c.d.logs.Warnf(clockAppID, "", "alarm timezone %q unavailable (%v); using the device timezone", alarmTZ, err)
		}
	}
	c.mu.Lock()
	c.blink = blink
	c.appTZ = tz
	c.loc = c.resolveLocationLocked()
	c.displayMode = mode
	c.timerSecs = timerSecs
	a = c.alarms[defaultAlarmID]
	if a == nil {
		a = &clockAlarm{id: defaultAlarmID}
		c.alarms[defaultAlarmID] = a
		c.alarmOrder = append(c.alarmOrder, defaultAlarmID)
	}
	changed := a.label != alarmLabel || a.hour != alarmHour || a.minute != alarmMinute ||
		int(a.days) != alarmDays || a.tz != alarmTZ || a.enabled != alarmEnabled
	if changed {
		a.label, a.hour, a.minute = alarmLabel, alarmHour, alarmMinute
		a.days, a.tz, a.enabled = uint8(alarmDays), alarmTZ, alarmEnabled
		a.next = nextAlarmTrigger(a, c.now(), c.alarmLocationLocked(a))
	}
	c.mu.Unlock()
	c.publish()
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
	for _, a := range c.alarms {
		a.next = nextAlarmTrigger(a, now, c.alarmLocationLocked(a))
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

func (c *builtinClock) alarmLocationLocked(a *clockAlarm) *time.Location {
	if a != nil && a.tz != "" {
		if loc, err := time.LoadLocation(a.tz); err == nil {
			return loc
		}
	}
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
	persistDisable := c.defaultDisable
	c.defaultDisable = false
	c.mu.Unlock()
	if persistDisable {
		c.persistDefaultDisabled()
	}
	c.publish()
}

// advanceLocked fires due timers and alarms, expires alerts and recomputes
// recurring alarm triggers. The caller holds c.mu.
func (c *builtinClock) advanceLocked(now time.Time, mono uint64) {
	for _, id := range c.timerOrder {
		t := c.timers[id]
		if t == nil || t.status != timerRunning || mono < t.deadlineNS {
			continue
		}
		t.status = timerCompleted
		t.finishedMS = uint64(now.UnixMilli())
		c.addAlertLocked(alertTimer, t.id, t.label, now, mono, timerAlertMS)
	}
	for _, id := range c.alarmOrder {
		a := c.alarms[id]
		if a == nil || !a.enabled || a.next.IsZero() || now.Before(a.next) {
			continue
		}
		a.lastTriggeredMS = uint64(now.UnixMilli())
		if a.days == 0 {
			a.enabled = false
			a.next = time.Time{}
			if id == defaultAlarmID {
				c.defaultDisable = true
			}
		} else {
			a.next = nextAlarmTrigger(a, now.Add(time.Second), c.alarmLocationLocked(a))
		}
		c.addAlertLocked(alertAlarm, a.id, a.label, now, mono, alarmAlertMS)
	}
	for i := len(c.alertOrder) - 1; i >= 0; i-- {
		a := c.alerts[c.alertOrder[i]]
		if a != nil && mono >= a.expiresMono {
			c.removeAlertLocked(a.id)
		}
	}
}

func (c *builtinClock) addAlertLocked(kind clockAlertKind, sourceID, label string, now time.Time, mono, lifetime uint64) string {
	c.alertGen++
	c.nextID++
	id := fmt.Sprintf("alt_%d", c.nextID)
	c.alerts[id] = &clockAlert{
		id: id, kind: kind, sourceID: sourceID, label: label,
		startedMS: uint64(now.UnixMilli()), expiresMono: mono + lifetime*1_000_000,
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

func (c *builtinClock) dismissAlert(id string) bool {
	c.mu.Lock()
	if id == "" {
		if len(c.alertOrder) == 0 {
			c.mu.Unlock()
			return false
		}
		for _, cur := range append([]string(nil), c.alertOrder...) {
			c.removeAlertLocked(cur)
		}
		c.mu.Unlock()
		c.publish()
		return true
	}
	if c.alerts[id] == nil {
		c.mu.Unlock()
		return false
	}
	c.removeAlertLocked(id)
	c.mu.Unlock()
	c.publish()
	return true
}

func (c *builtinClock) activeAlertLocked() *clockAlert {
	for _, id := range c.alertOrder {
		if a := c.alerts[id]; a != nil {
			return a
		}
	}
	return nil
}

// nextAlarmTrigger returns the next instant the alarm should fire. days is a
// bitmask with bit 0 Monday; zero means the next occurrence only. The search
// spans eight days so a time that already passed today still finds the next
// matching weekday.
func nextAlarmTrigger(a *clockAlarm, now time.Time, loc *time.Location) time.Time {
	if a == nil || !a.enabled || loc == nil {
		return time.Time{}
	}
	today := now.In(loc)
	for offset := 0; offset <= 8; offset++ {
		day := today.AddDate(0, 0, offset)
		cand := alarmCandidate(day, a.hour, a.minute, loc)
		if !cand.After(now) {
			continue
		}
		if a.days == 0 {
			return cand
		}
		bit := (int(cand.Weekday()) + 6) % 7
		if a.days&(1<<uint(bit)) != 0 {
			return cand
		}
	}
	return time.Time{}
}

// alarmCandidate builds the wall-clock instant for one day. A spring-forward
// transition can make the requested time nonexistent; in that case the alarm
// fires at the first valid instant after the gap instead of being skipped or
// silently shifted backward into the previous offset.
func alarmCandidate(day time.Time, hour, minute int, loc *time.Location) time.Time {
	year, month, date := day.Date()
	cand := time.Date(year, month, date, hour, minute, 0, 0, loc)
	local := cand.In(loc)
	if local.Hour() == hour && local.Minute() == minute {
		return cand
	}
	target := hour*60 + minute
	probe := time.Date(year, month, date, hour, 0, 0, 0, loc)
	for range 240 {
		at := probe.In(loc)
		if at.Day() == date && at.Hour()*60+at.Minute() >= target {
			return probe
		}
		probe = probe.Add(time.Minute)
	}
	return cand
}

// --- rendering --------------------------------------------------------------

func (c *builtinClock) publish() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	now := c.now()
	mono := c.mono()
	base := c.baselineDisplayLocked(now, mono)
	alert := c.activeAlertLocked()
	var alertDisplay clockDisplay
	haveAlert := alert != nil
	if haveAlert {
		alertDisplay = c.alertDisplayLocked(alert, now)
	}
	baseChanged := !c.appliedOK || base != c.applied
	c.applied = base
	c.appliedOK = true
	var audio []byte
	if haveAlert && !alert.audioSent {
		audio = c.alertAudioLocked(alert.kind)
		if audio != nil {
			alert.audioSent = true
		}
	}
	gen := c.alertGen
	pres := c.pres
	c.mu.Unlock()

	if pres != nil && baseChanged {
		if err := pres.UpdateFrame(segmentPresentationFrame(base.cells[:], base.colon)); err != nil && c.d.ctx.Err() == nil {
			c.d.logs.Debugf(clockAppID, "", "baseline update: %v", err)
		}
	}
	c.publishAlert(alertDisplay, haveAlert, gen)
	if audio != nil {
		c.playAlertAudio(audio)
	}
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
	if c.segmentID != "" {
		c.alertSig[c.segmentID] = display
	}
	ringChanged := c.alertSig[c.ringID] != display
	if c.ringID != "" {
		c.alertSig[c.ringID] = display
	}
	segmentCaptured := c.alertCap[c.segmentID] != nil
	ringCaptured := c.alertCap[c.ringID] != nil
	c.mu.Unlock()
	if client == nil {
		return
	}

	if segment == nil && c.segmentID != "" {
		p, err := client.OpenPresentation(ctx, c.segmentID)
		if err != nil {
			c.d.logs.Debugf(clockAppID, "", "alert segment presentation: %v", err)
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
			c.d.logs.Debugf(clockAppID, "", "alert ring presentation: %v", err)
		} else {
			ring = p
			c.mu.Lock()
			c.alertPres[c.ringID] = p
			c.mu.Unlock()
		}
	}
	if segment != nil && segmentChanged {
		if err := segment.UpdateFrame(segmentPresentationFrame(display.cells[:], display.colon)); err != nil {
			c.d.logs.Debugf(clockAppID, "", "alert segment: %v", err)
		}
	}
	if ring != nil && ringChanged {
		params := mustParams("system.ring.pulse", presets.Params{
			"speed": 1.6, "color": backend.RGB{R: 255, G: 96, B: 24}, "min_intensity": 0.12,
		})
		if err := ring.UpdateFrame(ringEffectPresentationFrame("system.ring.pulse", params)); err != nil {
			c.d.logs.Debugf(clockAppID, "", "alert ring: %v", err)
		}
	}
	if segment != nil && !segmentCaptured {
		if cap, err := client.Capture(ctx, c.segmentID, segment.ID()); err == nil {
			c.mu.Lock()
			c.alertCap[c.segmentID] = cap
			c.mu.Unlock()
		} else {
			c.d.logs.Debugf(clockAppID, "", "alert display capture: %v", err)
		}
	}
	if ring != nil && !ringCaptured {
		if cap, err := client.Capture(ctx, c.ringID, ring.ID()); err == nil {
			c.mu.Lock()
			c.alertCap[c.ringID] = cap
			c.mu.Unlock()
		} else {
			c.d.logs.Debugf(clockAppID, "", "alert ring capture: %v", err)
		}
	}
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
		if alarm := c.alarms[a.sourceID]; alarm != nil {
			return clockDisplay{cells: fourDigits(alarm.hour*100 + alarm.minute), colon: now.UnixMilli()/500%2 == 0}
		}
	}
	cells := [4]uint16{0, 0, 0, 0}
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
	if tz, ok := text("alarm_timezone"); ok && tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("alarm_timezone %q is not an IANA zone", tz)
		}
	}
	if mode, ok := text("display_mode"); ok {
		switch mode {
		case "clock", "stopwatch", "timer":
		default:
			return fmt.Errorf("display_mode %q is not a supported mode", mode)
		}
	}
	if raw, ok := values["timer_seconds"]; ok {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > 86400 {
			return fmt.Errorf("timer_seconds must be between 1 and 86400")
		}
	}
	if raw, ok := values["alarm_hour"]; ok {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 23 {
			return fmt.Errorf("alarm_hour must be between 0 and 23")
		}
	}
	if raw, ok := values["alarm_minute"]; ok {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 59 {
			return fmt.Errorf("alarm_minute must be between 0 and 59")
		}
	}
	if raw, ok := values["alarm_days"]; ok {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 127 {
			return fmt.Errorf("alarm_days must be between 0 and 127")
		}
	}
	return nil
}

// --- service calls ----------------------------------------------------------

func (c *builtinClock) handleCall(ctx context.Context, service, rpc string, request []byte) ([][]byte, error) {
	if service != clockServiceName {
		return nil, status.Errorf(codes.Unimplemented, "clock app does not implement %s", service)
	}
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if !running {
		return nil, status.Error(codes.FailedPrecondition, "clock app is not running")
	}
	var (
		resp proto.Message
		err  error
	)
	switch rpc {
	case "GetState":
		if err = decodeClockRequest(request, &clockv1.GetStateRequest{}); err != nil {
			return nil, err
		}
		resp = c.state()
	case "StopwatchStart", "StopwatchPause", "StopwatchResume", "StopwatchReset":
		if err = decodeClockRequest(request, &clockv1.StopwatchCommandRequest{}); err != nil {
			return nil, err
		}
		resp = c.stopwatchCommand(rpc)
	case "StartTimer":
		req := &clockv1.StartTimerRequest{}
		if err = decodeClockRequest(request, req); err != nil {
			return nil, err
		}
		if req.DurationMs == 0 || req.DurationMs > maxTimerMS {
			return nil, status.Errorf(codes.InvalidArgument, "duration_ms must be between 1 and %d", maxTimerMS)
		}
		resp = c.startTimer(req.DurationMs, req.Label)
	case "CancelTimer":
		req := &clockv1.CancelTimerRequest{}
		if err = decodeClockRequest(request, req); err != nil {
			return nil, err
		}
		resp, err = c.cancelTimer(req.TimerId)
	case "ListTimers":
		if err = decodeClockRequest(request, &clockv1.ListTimersRequest{}); err != nil {
			return nil, err
		}
		resp = c.listTimers()
	case "SetAlarm":
		req := &clockv1.SetAlarmRequest{}
		if err = decodeClockRequest(request, req); err != nil {
			return nil, err
		}
		resp, err = c.setAlarm(req)
	case "SetAlarmEnabled":
		req := &clockv1.SetAlarmEnabledRequest{}
		if err = decodeClockRequest(request, req); err != nil {
			return nil, err
		}
		resp, err = c.setAlarmEnabled(req.AlarmId, req.Enabled)
	case "DeleteAlarm":
		req := &clockv1.DeleteAlarmRequest{}
		if err = decodeClockRequest(request, req); err != nil {
			return nil, err
		}
		resp, err = c.deleteAlarm(req.AlarmId)
	case "ListAlarms":
		if err = decodeClockRequest(request, &clockv1.ListAlarmsRequest{}); err != nil {
			return nil, err
		}
		resp = c.listAlarms()
	case "DismissAlert":
		req := &clockv1.DismissAlertRequest{}
		if err = decodeClockRequest(request, req); err != nil {
			return nil, err
		}
		resp = &clockv1.DismissAlertResponse{Dismissed: c.dismissAlert(req.AlertId)}
	default:
		return nil, status.Errorf(codes.Unimplemented, "unknown ClockService method %s", rpc)
	}
	if err != nil {
		return nil, err
	}
	payload, err := encodeProtoResponse(resp)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return [][]byte{payload}, nil
}

func decodeClockRequest(request []byte, msg proto.Message) error {
	if len(request) == 0 {
		return nil
	}
	if err := protojson.Unmarshal(request, msg); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func (c *builtinClock) state() *clockv1.ClockState {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	mono := c.mono()
	loc := c.locationLocked()
	out := &clockv1.ClockState{
		LocalTimeRfc3339: now.In(loc).Format(time.RFC3339),
		Timezone:         loc.String(),
		Stopwatch:        c.stopwatchInfoLocked(mono),
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

func (c *builtinClock) stopwatchCommand(rpc string) *clockv1.StopwatchInfo {
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

func (c *builtinClock) stopwatchInfoLocked(mono uint64) *clockv1.StopwatchInfo {
	elapsed := c.stopwatch.elapsedNS
	state := clockv1.StopwatchStatus_STOPWATCH_STATUS_IDLE
	if c.stopwatch.running {
		elapsed += mono - c.stopwatch.startedNS
		state = clockv1.StopwatchStatus_STOPWATCH_STATUS_RUNNING
	} else if elapsed > 0 {
		state = clockv1.StopwatchStatus_STOPWATCH_STATUS_PAUSED
	}
	return &clockv1.StopwatchInfo{
		State: state, ElapsedMs: elapsed / 1_000_000, StartedUnixMs: c.stopwatch.startedMS,
	}
}

func (c *builtinClock) startTimer(durationMS uint64, label string) *clockv1.TimerInfo {
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
	c.publish()
	return out
}

func (c *builtinClock) cancelTimer(id string) (*clockv1.TimerInfo, error) {
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
	c.mu.Unlock()
	c.publish()
	return out, nil
}

func (c *builtinClock) listTimers() *clockv1.ListTimersResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &clockv1.ListTimersResponse{Timers: c.timersLocked(c.mono())}
}

func (c *builtinClock) timersLocked(mono uint64) []*clockv1.TimerInfo {
	out := make([]*clockv1.TimerInfo, 0, len(c.timerOrder))
	for _, id := range c.timerOrder {
		t := c.timers[id]
		if t == nil {
			continue
		}
		out = append(out, timerInfo(t, mono))
	}
	return out
}

func timerInfo(t *clockTimer, mono uint64) *clockv1.TimerInfo {
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
	return &clockv1.TimerInfo{
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

func (c *builtinClock) setAlarm(req *clockv1.SetAlarmRequest) (*clockv1.AlarmInfo, error) {
	if req.Hour > 23 || req.Minute > 59 || req.DaysOfWeek > 127 {
		return nil, status.Error(codes.InvalidArgument, "hour must be 0-23, minute 0-59 and days_of_week 0-127")
	}
	if req.Timezone != "" {
		if _, err := time.LoadLocation(req.Timezone); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "timezone %q: %v", req.Timezone, err)
		}
	}
	c.mu.Lock()
	id := req.AlarmId
	if id == "" {
		c.nextID++
		id = fmt.Sprintf("alm_%d", c.nextID)
	}
	a := c.alarms[id]
	if a == nil {
		a = &clockAlarm{id: id}
		c.alarms[id] = a
		c.alarmOrder = append(c.alarmOrder, id)
	}
	label := req.Label
	if label == "" {
		label = "Alarm"
	}
	a.label, a.hour, a.minute = label, int(req.Hour), int(req.Minute)
	a.days, a.tz, a.enabled = uint8(req.DaysOfWeek), req.Timezone, req.Enabled
	a.next = nextAlarmTrigger(a, c.now(), c.alarmLocationLocked(a))
	out := alarmInfo(a, c.alarmLocationLocked(a))
	c.mu.Unlock()

	if id == defaultAlarmID {
		c.persistDefaultAlarm(label, int(req.Hour), int(req.Minute), int(req.DaysOfWeek), req.Timezone, req.Enabled)
	}
	c.publish()
	return out, nil
}

func (c *builtinClock) setAlarmEnabled(id string, enabled bool) (*clockv1.AlarmInfo, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "alarm_id is required")
	}
	c.mu.Lock()
	a := c.alarms[id]
	if a == nil {
		c.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "alarm %s not found", id)
	}
	a.enabled = enabled
	a.next = nextAlarmTrigger(a, c.now(), c.alarmLocationLocked(a))
	out := alarmInfo(a, c.alarmLocationLocked(a))
	label, hour, minute, days, tz := a.label, a.hour, a.minute, int(a.days), a.tz
	c.mu.Unlock()
	if id == defaultAlarmID {
		c.persistDefaultAlarm(label, hour, minute, days, tz, enabled)
	}
	c.publish()
	return out, nil
}

func (c *builtinClock) deleteAlarm(id string) (*clockv1.DeleteAlarmResponse, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "alarm_id is required")
	}
	if id == defaultAlarmID {
		return nil, status.Error(codes.FailedPrecondition, "the default alarm is managed by app settings and cannot be deleted")
	}
	c.mu.Lock()
	if c.alarms[id] == nil {
		c.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "alarm %s not found", id)
	}
	delete(c.alarms, id)
	for i, cur := range c.alarmOrder {
		if cur == id {
			c.alarmOrder = append(c.alarmOrder[:i], c.alarmOrder[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	return &clockv1.DeleteAlarmResponse{Deleted: true}, nil
}

func (c *builtinClock) listAlarms() *clockv1.ListAlarmsResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*clockv1.AlarmInfo, 0, len(c.alarmOrder))
	for _, id := range c.alarmOrder {
		a := c.alarms[id]
		if a == nil {
			continue
		}
		out = append(out, alarmInfo(a, c.alarmLocationLocked(a)))
	}
	return &clockv1.ListAlarmsResponse{Alarms: out}
}

func (c *builtinClock) alarmsLocked() []*clockv1.AlarmInfo {
	out := make([]*clockv1.AlarmInfo, 0, len(c.alarmOrder))
	for _, id := range c.alarmOrder {
		a := c.alarms[id]
		if a == nil {
			continue
		}
		out = append(out, alarmInfo(a, c.alarmLocationLocked(a)))
	}
	return out
}

func alarmInfo(a *clockAlarm, loc *time.Location) *clockv1.AlarmInfo {
	out := &clockv1.AlarmInfo{
		AlarmId: a.id, Label: a.label, Hour: uint32(a.hour), Minute: uint32(a.minute),
		DaysOfWeek: uint32(a.days), Enabled: a.enabled, Timezone: a.tz,
		EffectiveTimezone: loc.String(), LastTriggeredUnixMs: a.lastTriggeredMS,
	}
	if !a.next.IsZero() {
		out.NextTriggerUnixMs = uint64(a.next.UnixMilli())
		out.NextTriggerRfc3339 = a.next.In(loc).Format(time.RFC3339)
	}
	return out
}

func (c *builtinClock) persistDefaultDisabled() {
	c.mu.Lock()
	a := c.alarms[defaultAlarmID]
	if a == nil {
		c.mu.Unlock()
		return
	}
	label, hour, minute, days, tz := a.label, a.hour, a.minute, int(a.days), a.tz
	c.mu.Unlock()
	c.persistDefaultAlarm(label, hour, minute, days, tz, false)
}

func (c *builtinClock) persistDefaultAlarm(label string, hour, minute, days int, tz string, enabled bool) {
	changes := []settings.Change{
		{Key: "alarm_label", Value: label},
		{Key: "alarm_hour", Value: hour},
		{Key: "alarm_minute", Value: minute},
		{Key: "alarm_days", Value: days},
		{Key: "alarm_timezone", Value: tz},
		{Key: "alarm_enabled", Value: enabled},
	}
	if _, _, _, err := c.d.appCfg.Update(clockAppID, 0, changes); err != nil {
		c.d.logs.Warnf(clockAppID, "", "persist alarm settings: %v", err)
	}
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
	case "alarm_enable":
		if _, err := c.setAlarmEnabled(defaultAlarmID, true); err != nil {
			return err
		}
	case "alarm_disable":
		if _, err := c.setAlarmEnabled(defaultAlarmID, false); err != nil {
			return err
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

func (c *builtinClock) playAlertAudio(pcm []byte) {
	c.mu.Lock()
	client := c.client
	format := c.audioFormat
	ctx := c.ctx
	c.mu.Unlock()
	if client == nil || ctx == nil {
		return
	}
	go func() {
		if err := client.Playback(ctx, format, pcm); err != nil && c.d.ctx.Err() == nil {
			c.d.logs.Debugf(clockAppID, "", "alert audio: %v", err)
		}
	}()
}
