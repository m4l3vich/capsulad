package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presets"
	"capsulad/internal/ringpalette"
	"capsulad/internal/settings"
	"capsulad/internal/uichanges"
)

type builtinRing struct {
	d      *Daemon
	ringID string

	mu      sync.Mutex
	running bool
	runID   string
	cancel  context.CancelFunc

	client *builtinAppClient
	pres   *appPresentation

	effect     string
	palette    string
	gradient   ringpalette.Gradient
	brightness float64
	speed      float64

	// The optional active window is local wall-clock time. Outside it the app
	// keeps its normal baseline presentation but presents an explicit blank
	// frame, so the ring goes dark while other captures still preempt it and
	// the selected effect returns at the boundary.
	now             func() time.Time
	deviceTZ        string
	loc             *time.Location
	scheduleEnabled bool
	activeFrom      int // minutes since local midnight
	activeUntil     int
	active          bool
	wake            chan struct{}

	applied string

	// settingsRevision mirrors the app-settings store revision. Watchers use
	// it for compare-and-swap and convergence; an older queued snapshot must
	// never overwrite a newer synchronous update.
	settingsMu       sync.Mutex
	settingsLoaded   bool
	settingsRevision uint64
	// runGen invalidates watcher callbacks from a previous start/stop.
	runGen uint64
	// frameMu serializes baseline frame compute+send and applied bookkeeping.
	frameMu sync.Mutex
	// failureMu serializes recovery with the next generation start, so a
	// recovery already in flight cannot fail a run started under the reused
	// stable run ID.
	failureMu sync.Mutex
	// failedGen records the generation whose single recovery has been claimed.
	// It is protected by mu and never reset; runGen is monotonically increasing.
	failedGen uint64
	// pixelCount is derived from the advertised ring capability at start.
	pixelCount  uint32
	watchers    map[int]chan *ringv1.RingState
	nextWatch   int
	settingSubs map[int]chan *ringv1.RingSettingsSnapshot
	nextSetting int
}

func newBuiltinRing(d *Daemon, ringID string) *builtinRing {
	r := &builtinRing{
		d: d, ringID: ringID,
		effect: "breathe", brightness: 1, speed: 1,
		now: time.Now, active: true, loc: time.Local,
		activeFrom: 22 * 60, activeUntil: 7 * 60,
		wake: make(chan struct{}, 1),
	}
	r.palette, r.gradient = defaultRingPalette()
	if info := d.resources[ringID]; info != nil && info.GetRing() != nil {
		r.pixelCount = info.GetRing().GetPixelCount()
	}
	r.watchers = map[int]chan *ringv1.RingState{}
	r.settingSubs = map[int]chan *ringv1.RingSettingsSnapshot{}
	return r
}

// defaultRingPalette is the canonical aurora gradient. Preset names are a UI
// convenience; the persisted setting is always this canonical form.
func defaultRingPalette() (string, ringpalette.Gradient) {
	const text = "linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)"
	g, err := ringpalette.Parse(text)
	if err != nil {
		panic(err)
	}
	return text, g
}

func (r *builtinRing) start() {
	r.failureMu.Lock()
	r.settingsMu.Lock()
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		r.settingsMu.Unlock()
		r.failureMu.Unlock()
		return
	}
	r.running = true
	r.runGen++
	gen := r.runGen
	r.settingsLoaded = false
	r.settingsRevision = 0
	r.runID = r.d.SystemAppRunID(ringAppID)
	ctx, cancel := context.WithCancel(r.d.ctx)
	r.cancel = cancel
	r.applied = ""
	client := newBuiltinAppClient(r.d, ringAppID)
	r.client = client
	r.mu.Unlock()
	r.settingsMu.Unlock()
	r.failureMu.Unlock()
	client.SetFailureHandler(func(err error) { r.failRun(gen, err) })

	// Opening the stream talks to the daemon's own gRPC endpoint. Doing that
	// outside the lock keeps the output loop, which reasserts the baseline on
	// every tick, from stalling if the stream is slow or fails.
	pres, err := client.OpenPresentation(ctx, r.ringID)

	r.mu.Lock()
	if !r.running || r.client != client {
		// A stop or a newer start won the race while the stream was opening.
		r.mu.Unlock()
		cancel()
		if pres != nil {
			pres.Close()
		}
		client.Close()
		return
	}
	if err == nil {
		r.pres = pres
	}
	r.mu.Unlock()
	if err != nil {
		r.d.logs.Errorf(ringAppID, "", "open baseline presentation: %v", err)
		r.failRun(gen, err)
		return
	}
	r.d.markSystemHealthy(ringAppID)
	r.loadSettings(ctx, gen)
	r.refreshSchedule()
	go r.scheduleLoop(ctx)
}

// failRun recovers an internal stream failure by failing this run; the daemon
// schedules a bounded restart. Recovery is claimed at most once per run
// generation: a callback from an earlier start or a normal stop is ignored.
func (r *builtinRing) failRun(gen uint64, err error) {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	r.mu.Lock()
	if r.runGen != gen || !r.running || r.failedGen == gen {
		r.mu.Unlock()
		return
	}
	r.failedGen = gen
	r.mu.Unlock()
	r.d.logs.Errorf(ringAppID, "", "internal stream failed: %v; recovering run", err)
	r.d.recoverSystemApp(ringAppID)
}

func (r *builtinRing) stop() {
	r.settingsMu.Lock()
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		r.settingsMu.Unlock()
		return
	}
	r.running = false
	r.runGen++
	r.settingsLoaded = false
	r.settingsRevision = 0
	cancel := r.cancel
	r.cancel = nil
	pres := r.pres
	client := r.client
	r.pres = nil
	r.client = nil
	r.applied = ""
	for id, ch := range r.watchers {
		delete(r.watchers, id)
		close(ch)
	}
	for id, ch := range r.settingSubs {
		delete(r.settingSubs, id)
		close(ch)
	}
	r.mu.Unlock()
	r.settingsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if pres != nil {
		pres.Close()
	}
	if client != nil {
		client.Close()
	}
}

func (r *builtinRing) loadSettings(ctx context.Context, gen uint64) {
	snap, ch, cancel, err := r.d.appCfg.WatchApp(ringAppID)
	if err != nil {
		r.d.logs.Warnf(ringAppID, "", "settings unavailable: %v", err)
		return
	}
	r.applySettingsGen(gen, snap.Revision, snap.Values)
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
				r.applySettingsGen(gen, next.Revision, next.Values)
			}
		}
	}()
}

// applySettings applies settings for the current run. Mutating callers use it
// so their committed snapshot is visible synchronously.
func (r *builtinRing) applySettings(revision uint64, values []settings.AppSettingView) {
	r.settingsMu.Lock()
	gen := r.runGen
	r.settingsMu.Unlock()
	r.applySettingsGen(gen, revision, values)
}

// applySettingsGen drops callbacks from a previous run and enforces monotonic
// store revisions for this run. revision 0 is an unversioned local apply.
func (r *builtinRing) applySettingsGen(gen, revision uint64, values []settings.AppSettingView) {
	r.settingsMu.Lock()
	if gen != r.runGen {
		r.settingsMu.Unlock()
		return
	}
	if revision != 0 {
		r.mu.Lock()
		if r.settingsLoaded && revision <= r.settingsRevision {
			r.mu.Unlock()
			r.settingsMu.Unlock()
			return
		}
		r.settingsLoaded = true
		r.settingsRevision = revision
		r.mu.Unlock()
	}
	r.mu.Lock()
	effect, palette := r.effect, r.palette
	brightness, speed := r.brightness, r.speed
	scheduleEnabled := r.scheduleEnabled
	activeFrom, activeUntil := r.activeFrom, r.activeUntil
	r.mu.Unlock()

	var nextEffect, nextPalette string
	if decodeSetting(values, "effect", &nextEffect) {
		effect = nextEffect
	}
	if decodeSetting(values, "palette", &nextPalette) {
		palette = strings.TrimSpace(nextPalette)
	}
	var level, rate float64
	if decodeSetting(values, "brightness", &level) {
		brightness = clampUnit(level)
	}
	if decodeSetting(values, "speed", &rate) {
		speed = clampRange(rate, 0.1, 4)
	}
	var enabled bool
	if decodeSetting(values, "schedule_enabled", &enabled) {
		scheduleEnabled = enabled
	}
	var fromText, untilText string
	if decodeSetting(values, "schedule_start", &fromText) {
		if minute, ok := parseClockTime(fromText); ok {
			activeFrom = minute
		}
	}
	if decodeSetting(values, "schedule_end", &untilText) {
		if minute, ok := parseClockTime(untilText); ok {
			activeUntil = minute
		}
	}
	if !validRingEffect(effect) {
		effect = "breathe"
	}
	parsed, err := ringpalette.Parse(palette)
	if err != nil {
		r.d.logs.Warnf(ringAppID, "", "invalid palette %q (%v); keeping the previous gradient", palette, err)
		r.mu.Lock()
		palette, parsed = r.palette, r.gradient
		r.mu.Unlock()
	} else {
		// Persist the canonical text only for values already in canonical
		// form; the setting is the user's source of truth and the validator
		// enforces the grammar, so no rewrite is needed here.
	}

	r.mu.Lock()
	r.effect, r.palette = effect, palette
	r.gradient = parsed
	r.brightness, r.speed = brightness, speed
	r.scheduleEnabled = scheduleEnabled
	r.activeFrom, r.activeUntil = activeFrom, activeUntil
	r.mu.Unlock()
	r.settingsMu.Unlock()
	r.refreshSchedule()
	r.signalSchedule()
	r.notifySettings()
	r.notifyState()
}

func validRingEffect(effect string) bool {
	switch effect {
	case "breathe", "spinner", "rainbow", "pulse", "comet", "sparkle", "static":
		return true
	}
	return false
}

// refreshSchedule re-evaluates the active window against the current wall
// clock and republishes the baseline when the answer changed.
func (r *builtinRing) refreshSchedule() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.active = r.scheduleActiveLocked(r.now())
	r.mu.Unlock()
	r.reassert()
}

// setDeviceTimezone follows the device-wide zone. The active window is local
// wall-clock time, so the same window can open or close on a zone change.
func (r *builtinRing) setDeviceTimezone(name string) {
	r.mu.Lock()
	if name == r.deviceTZ {
		r.mu.Unlock()
		return
	}
	r.deviceTZ = name
	// Resolve once per zone change: the boundary timer must not read tzdata
	// under the app lock on every tick.
	r.loc = loadLocationOrDefault(name)
	r.mu.Unlock()
	r.refreshSchedule()
	r.signalSchedule()
}

func (r *builtinRing) reassert() {
	// Compute and send under one frame lock so concurrent settings/output
	// callbacks cannot deliver an older frame after a newer one. applied is
	// updated only after a successful write; a failed stream keeps the old
	// signature so the next tick retries.
	r.frameMu.Lock()
	defer r.frameMu.Unlock()
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	frame, sig := r.frameLocked()
	pres := r.pres
	if pres == nil {
		// The baseline stream is not attached yet (startup) or is being torn
		// down. Recording the signature here would make the first reassert
		// after attachment believe the frame was already written.
		r.mu.Unlock()
		return
	}
	if sig == r.applied {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if err := pres.UpdateFrame(frame); err != nil {
		if r.d.ctx.Err() == nil {
			r.d.logs.Debugf(ringAppID, "", "baseline: %v", err)
		}
		return
	}
	r.mu.Lock()
	r.applied = sig
	r.mu.Unlock()
	r.notifyState()
}

// frameLocked builds the baseline frame for the current state. A closed active
// window yields an explicit blank frame: the presentation stays owned and
// eligible, so the backend writes zeroes instead of freezing the last effect,
// and the selected effect resumes when the window opens again.
func (r *builtinRing) frameLocked() (*frameworkv1.PresentationFrame, string) {
	if !r.active {
		return blankPresentationFrame(), "blank"
	}
	id, params, sig := r.contentLocked()
	return ringEffectPresentationFrame(id, params), sig
}

func (r *builtinRing) contentLocked() (string, presets.Params, string) {
	primary := r.gradient.Sample(0)
	secondary := r.gradient.Sample(1)
	brightness := clampUnit(r.brightness)
	speed := clampRange(r.speed, 0.1, 4)
	var (
		id     string
		params presets.Params
	)
	switch r.effect {
	case "static":
		id = "system.ring.static"
		params = presets.Params{
			"color": scaleColor(r.gradient.Solid(), brightness), "intensity": 1.0,
		}
	case "spinner":
		id = "system.ring.spinner"
		params = presets.Params{
			"speed": 0.45 * speed, "color": scaleColor(primary, brightness),
			"intensity": 1.0, "tail": int64(5),
		}
	case "rainbow":
		id = "system.ring.rainbow"
		params = presets.Params{
			"speed": 0.15 * speed,
			"color": r.gradient.Sample(0), "color_b": r.gradient.Sample(1.0 / 3.0),
			"color_c": r.gradient.Sample(2.0 / 3.0), "brightness": brightness,
		}
	case "pulse":
		id = "system.ring.pulse"
		params = presets.Params{
			"speed": 0.8 * speed, "color": scaleColor(primary, brightness),
			"min_intensity": 0.06,
		}
	case "comet":
		id = "system.ring.comet"
		params = presets.Params{
			"speed": 0.35 * speed, "color": primary, "tail_color": secondary,
			"tail": int64(8), "intensity": brightness,
		}
	case "sparkle":
		id = "system.ring.sparkle"
		params = presets.Params{
			"speed": 0.9 * speed, "color": primary, "color_b": secondary,
			"density": 0.6, "intensity": brightness,
		}
	default:
		id = "system.ring.breathe"
		period := 4.0
		if speed > 0 {
			period = 4.0 / speed
		}
		params = presets.Params{"period_s": period, "color": scaleColor(primary, brightness)}
	}
	return id, mustParams(id, params),
		fmt.Sprintf("%s|%s|%.3f|%.3f|%s", r.effect, r.palette, r.brightness, r.speed,
			gradientSignature(r.gradient))
}

// gradientSignature keeps the frame deduplication key sensitive to parsed
// colour changes without formatting frames or re-parsing the string.
func gradientSignature(g ringpalette.Gradient) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%g", g.Angle)
	for _, s := range g.Stops {
		fmt.Fprintf(&b, "/%.3f:%02x%02x%02x", s.Position, s.Color.R, s.Color.G, s.Color.B)
	}
	return b.String()
}

// --- active window ----------------------------------------------------------

const (
	// scheduleRecheck bounds one wait so a wall-clock step that does not pass
	// through the settings watcher (a manual `date` or NTP correction) is
	// picked up within the hour instead of at the next boundary. The timer
	// fires at a boundary or this ceiling, never per output frame.
	scheduleRecheck = time.Hour
	// scheduleMinWait keeps a boundary that just passed from arming a
	// zero-delay timer in a tight loop.
	scheduleMinWait = time.Second
)

// scheduleLoop arms one timer for the next boundary (or settings/timezone
// wake-up) instead of polling the clock on every output tick.
func (r *builtinRing) scheduleLoop(ctx context.Context) {
	for {
		r.mu.Lock()
		wait := r.scheduleWaitLocked()
		r.mu.Unlock()
		var (
			timer  *time.Timer
			timerC <-chan time.Time
		)
		if wait > 0 {
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-r.wake:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
			r.refreshSchedule()
		}
	}
}

// signalSchedule wakes the boundary loop after a settings or timezone change.
func (r *builtinRing) signalSchedule() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// scheduleWaitLocked returns the delay until the next window boundary, or zero
// when no boundary can change the state and only an explicit wake-up will.
func (r *builtinRing) scheduleWaitLocked() time.Duration {
	if !r.scheduleEnabled || r.activeFrom == r.activeUntil {
		return 0
	}
	now := r.now()
	next, ok := nextScheduleChange(now, r.activeFrom, r.activeUntil, r.locationLocked())
	if !ok {
		return 0
	}
	wait := next.Sub(now)
	if wait < scheduleMinWait {
		wait = scheduleMinWait
	}
	if wait > scheduleRecheck {
		wait = scheduleRecheck
	}
	return wait
}

// scheduleActiveLocked reports whether the configured window contains now.
func (r *builtinRing) scheduleActiveLocked(now time.Time) bool {
	if !r.scheduleEnabled {
		return true
	}
	return withinSchedule(now, r.activeFrom, r.activeUntil, r.locationLocked())
}

func (r *builtinRing) locationLocked() *time.Location {
	if r.loc != nil {
		return r.loc
	}
	return time.Local
}

// withinSchedule reports whether a wall-clock instant falls in the active
// window. The window is half-open, may cross midnight, and equal bounds mean
// the whole day.
func withinSchedule(now time.Time, start, end int, loc *time.Location) bool {
	if start == end {
		return true
	}
	local := now.In(loc)
	current := local.Hour()*60 + local.Minute()
	if start < end {
		return current >= start && current < end
	}
	return current >= start || current < end
}

// nextScheduleChange returns the next local instant at which withinSchedule
// flips. It scans today and tomorrow so an overnight window works; a whole-day
// window has no boundary.
func nextScheduleChange(now time.Time, start, end int, loc *time.Location) (time.Time, bool) {
	if start == end {
		return time.Time{}, false
	}
	local := now.In(loc)
	var next time.Time
	for day := 0; day <= 1; day++ {
		d := local.AddDate(0, 0, day)
		for _, minute := range [2]int{start, end} {
			t := time.Date(d.Year(), d.Month(), d.Day(), minute/60, minute%60, 0, 0, loc)
			if !t.After(local) {
				continue
			}
			if next.IsZero() || t.Before(next) {
				next = t
			}
		}
	}
	return next, !next.IsZero()
}

// parseClockTime parses a 24-hour HH:MM wall-clock time into minutes since
// midnight.
func parseClockTime(s string) (int, bool) {
	s = strings.TrimSpace(s)
	hours, minutes, ok := strings.Cut(s, ":")
	if !ok || len(hours) < 1 || len(hours) > 2 || len(minutes) != 2 {
		return 0, false
	}
	hour, err := strconv.Atoi(hours)
	if err != nil {
		return 0, false
	}
	minute, err := strconv.Atoi(minutes)
	if err != nil {
		return 0, false
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func (r *builtinRing) handleCall(ctx context.Context, service, rpc string, request []byte, emit func([]byte) error) error {
	if service != ringServiceName {
		return status.Errorf(codes.Unimplemented, "ring app does not implement %s", service)
	}
	r.mu.Lock()
	running := r.running
	r.mu.Unlock()
	if !running {
		return status.Error(codes.FailedPrecondition, "ring app is not running")
	}
	switch rpc {
	case "GetState":
		if err := decodeRingRequest(request, &ringv1.GetStateRequest{}); err != nil {
			return err
		}
		return emitProto(emit, r.stateSnapshot())
	case "WatchState":
		if err := decodeRingRequest(request, &ringv1.WatchStateRequest{}); err != nil {
			return err
		}
		return r.watchState(ctx, emit)
	case "GetSettings":
		if err := decodeRingRequest(request, &ringv1.GetSettingsRequest{}); err != nil {
			return err
		}
		return emitProto(emit, r.settingsSnapshot())
	case "UpdateSettings":
		req := &ringv1.UpdateSettingsRequest{}
		if err := decodeRingRequest(request, req); err != nil {
			return err
		}
		snap, err := r.updateSettings(req)
		if err != nil {
			return err
		}
		return emitProto(emit, snap)
	case "WatchSettings":
		if err := decodeRingRequest(request, &ringv1.WatchSettingsRequest{}); err != nil {
			return err
		}
		return r.watchSettings(ctx, emit)
	default:
		return status.Errorf(codes.Unimplemented, "unknown RingService method %s", rpc)
	}
}

func decodeRingRequest(request []byte, msg proto.Message) error {
	if err := proto.Unmarshal(request, msg); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func emitProto(emit func([]byte) error, msg proto.Message) error {
	payload, err := encodeRingState(msg)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return emit(payload)
}

func (r *builtinRing) stateSnapshot() *ringv1.RingState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateSnapshotLocked()
}

func (r *builtinRing) settingsSnapshot() *ringv1.RingSettingsSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settingsSnapshotLocked()
}

// updateSettings writes only the declared setting paths named in the request,
// through the same CAS store and validator as the generic configuration API.
func (r *builtinRing) updateSettings(req *ringv1.UpdateSettingsRequest) (*ringv1.RingSettingsSnapshot, error) {
	if len(req.GetUpdateFields()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_fields is required")
	}
	if req.GetValues() == nil {
		return nil, status.Error(codes.InvalidArgument, "values is required")
	}
	values := req.GetValues()
	changes := make([]settings.Change, 0, len(req.GetUpdateFields()))
	for _, field := range req.GetUpdateFields() {
		switch field {
		case "effect":
			changes = append(changes, settings.Change{Key: field, Value: values.GetEffect()})
		case "palette":
			changes = append(changes, settings.Change{Key: field, Value: values.GetPalette()})
		case "brightness":
			changes = append(changes, settings.Change{Key: field, Value: float64(values.GetBrightness())})
		case "speed":
			changes = append(changes, settings.Change{Key: field, Value: float64(values.GetSpeed())})
		case "schedule_enabled":
			changes = append(changes, settings.Change{Key: field, Value: values.GetScheduleEnabled()})
		case "schedule_start":
			changes = append(changes, settings.Change{Key: field, Value: values.GetScheduleStart()})
		case "schedule_end":
			changes = append(changes, settings.Change{Key: field, Value: values.GetScheduleEnd()})
		default:
			return nil, status.Errorf(codes.InvalidArgument, "setting %q is not declared by the ring app", field)
		}
	}
	snap, rev, _, err := r.d.appCfg.Update(ringAppID, req.GetExpectedRevision(), changes)
	if err != nil {
		return nil, r.d.appSettingsStatus(ringAppID, err)
	}
	// Apply synchronously so the reply carries the committed values; the
	// watcher will observe the same revision and is idempotent.
	r.applySettings(rev, snap.Values)
	r.d.publishUI(uichanges.AppSettings)
	return r.settingsSnapshot(), nil
}

// watchState streams the current render state first, then every state change.
func (r *builtinRing) watchState(ctx context.Context, emit func([]byte) error) error {
	ch, cancel := r.subscribeState()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case snap, ok := <-ch:
			if !ok {
				return nil
			}
			if err := emitProto(emit, snap); err != nil {
				return err
			}
		}
	}
}

func (r *builtinRing) watchSettings(ctx context.Context, emit func([]byte) error) error {
	ch, cancel := r.subscribeSettings()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case snap, ok := <-ch:
			if !ok {
				return nil
			}
			if err := emitProto(emit, snap); err != nil {
				return err
			}
		}
	}
}

func (r *builtinRing) subscribeState() (<-chan *ringv1.RingState, func()) {
	ch := make(chan *ringv1.RingState, 1)
	r.mu.Lock()
	r.nextWatch++
	id := r.nextWatch
	r.watchers[id] = ch
	ch <- r.stateSnapshotLocked()
	r.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			if cur, ok := r.watchers[id]; ok {
				delete(r.watchers, id)
				close(cur)
			}
			r.mu.Unlock()
		})
	}
}

func (r *builtinRing) subscribeSettings() (<-chan *ringv1.RingSettingsSnapshot, func()) {
	ch := make(chan *ringv1.RingSettingsSnapshot, 1)
	r.mu.Lock()
	r.nextSetting++
	id := r.nextSetting
	r.settingSubs[id] = ch
	ch <- r.settingsSnapshotLocked()
	r.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			if cur, ok := r.settingSubs[id]; ok {
				delete(r.settingSubs, id)
				close(cur)
			}
			r.mu.Unlock()
		})
	}
}

// stateSnapshotLocked builds a state snapshot. Callers hold r.mu.
func (r *builtinRing) stateSnapshotLocked() *ringv1.RingState {
	return &ringv1.RingState{
		Effect: r.effect, Palette: r.palette,
		Brightness: float32(r.brightness), Speed: float32(r.speed),
		PresentationId:   r.presentationIDLocked(),
		Active:           r.active,
		SettingsRevision: r.settingsRevision,
		ScheduleEnabled:  r.scheduleEnabled,
		ScheduleStart:    formatClockTime(r.activeFrom),
		ScheduleEnd:      formatClockTime(r.activeUntil),
		PixelCount:       r.pixelCount,
	}
}

func (r *builtinRing) settingsSnapshotLocked() *ringv1.RingSettingsSnapshot {
	return &ringv1.RingSettingsSnapshot{
		Revision: r.settingsRevision,
		Values: &ringv1.RingSettings{
			Effect: r.effect, Palette: r.palette,
			Brightness: float32(r.brightness), Speed: float32(r.speed),
			ScheduleEnabled: r.scheduleEnabled,
			ScheduleStart:   formatClockTime(r.activeFrom),
			ScheduleEnd:     formatClockTime(r.activeUntil),
		},
	}
}

func (r *builtinRing) notifyState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.watchers) == 0 {
		return
	}
	snap := r.stateSnapshotLocked()
	// Sends are non-blocking and happen under the same lock that closes
	// subscription channels, so a cancel can never race into send-on-closed.
	for _, ch := range r.watchers {
		pushLatest(ch, snap)
	}
}

func (r *builtinRing) notifySettings() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.settingSubs) == 0 {
		return
	}
	snap := r.settingsSnapshotLocked()
	for _, ch := range r.settingSubs {
		pushLatest(ch, snap)
	}
}

// pushLatest keeps only the newest snapshot in a one-slot subscriber channel.
func pushLatest[T any](ch chan T, value T) {
	select {
	case ch <- value:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- value:
	default:
	}
}

func formatClockTime(minute int) string {
	minute = ((minute % (24 * 60)) + 24*60) % (24 * 60)
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// validateRingSettings rejects values the generic form can submit but the app
// cannot render: unsupported effect names, malformed gradients or clock times.
func validateRingSettings(values map[string]json.RawMessage) error {
	var effect string
	if raw, ok := values["effect"]; ok {
		if json.Unmarshal(raw, &effect) != nil {
			return fmt.Errorf("effect must be text")
		}
		if !validRingEffect(effect) {
			return fmt.Errorf("effect %q is not supported", effect)
		}
	}
	var palette string
	if raw, ok := values["palette"]; ok {
		if json.Unmarshal(raw, &palette) != nil {
			return fmt.Errorf("palette must be text")
		}
		if _, err := ringpalette.Parse(palette); err != nil {
			return fmt.Errorf("palette: %w", err)
		}
	}
	if raw, ok := values["brightness"]; ok {
		var n float64
		if json.Unmarshal(raw, &n) != nil || n < 0 || n > 1 {
			return fmt.Errorf("brightness must be between 0 and 1")
		}
	}
	if raw, ok := values["speed"]; ok {
		var n float64
		if json.Unmarshal(raw, &n) != nil || n < 0.1 || n > 4 {
			return fmt.Errorf("speed must be between 0.1 and 4")
		}
	}
	var enabled bool
	if raw, ok := values["schedule_enabled"]; ok {
		if json.Unmarshal(raw, &enabled) != nil {
			return fmt.Errorf("schedule_enabled must be true or false")
		}
	}
	for _, key := range []string{"schedule_start", "schedule_end"} {
		raw, ok := values[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return fmt.Errorf("%s must be text", key)
		}
		if _, ok := parseClockTime(text); !ok {
			return fmt.Errorf("%s must be a 24-hour HH:MM time, got %q", key, text)
		}
	}
	return nil
}

func (r *builtinRing) presentationIDLocked() string {
	if r.pres == nil {
		return ""
	}
	return r.pres.ID()
}

// encodeRingState emits the public binary protobuf response. The schema's
// `active` flag is a proto3 bool with no explicit presence: an inactive
// baseline arrives as false (its documented zero value), and the blank frame
// plus empty presentation_id still make the state observable.
func encodeRingState(msg proto.Message) ([]byte, error) {
	return proto.Marshal(msg)
}

func mustParams(id string, params presets.Params) presets.Params {
	info, ok := presets.Lookup(id)
	if !ok {
		panic("built-in preset " + id)
	}
	out, err := presets.ValidateParams(*info, params)
	if err != nil {
		panic("built-in preset " + id + ": " + err.Error())
	}
	return out
}

func scaleColor(c backend.RGB, level float64) backend.RGB {
	level = clampUnit(level)
	return backend.RGB{
		R: uint8(float64(c.R)*level + 0.5),
		G: uint8(float64(c.G)*level + 0.5),
		B: uint8(float64(c.B)*level + 0.5),
	}
}

func clampUnit(v float64) float64 { return clampRange(v, 0, 1) }

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
