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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presets"
	"capsulad/internal/settings"
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
}

func newBuiltinRing(d *Daemon, ringID string) *builtinRing {
	return &builtinRing{
		d: d, ringID: ringID,
		effect: "breathe", palette: "aurora", brightness: 1, speed: 1,
		now: time.Now, active: true, loc: time.Local,
		activeFrom: 22 * 60, activeUntil: 7 * 60,
		wake: make(chan struct{}, 1),
	}
}

func (r *builtinRing) start() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.runID = r.d.SystemAppRunID(ringAppID)
	ctx, cancel := context.WithCancel(r.d.ctx)
	r.cancel = cancel
	r.applied = ""
	client := newBuiltinAppClient(r.d, ringAppID)
	r.client = client
	r.mu.Unlock()

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
	if err != nil {
		r.d.logs.Errorf(ringAppID, "", "open baseline presentation: %v", err)
	} else {
		r.pres = pres
	}
	r.mu.Unlock()

	r.loadSettings(ctx)
	r.refreshSchedule()
	go r.scheduleLoop(ctx)
}

func (r *builtinRing) stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	cancel := r.cancel
	r.cancel = nil
	pres := r.pres
	client := r.client
	r.pres = nil
	r.client = nil
	r.applied = ""
	r.mu.Unlock()
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

func (r *builtinRing) loadSettings(ctx context.Context) {
	snap, ch, cancel, err := r.d.appCfg.WatchApp(ringAppID)
	if err != nil {
		r.d.logs.Warnf(ringAppID, "", "settings unavailable: %v", err)
		return
	}
	r.applySettings(snap.Values)
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
				r.applySettings(next.Values)
			}
		}
	}()
}

func (r *builtinRing) applySettings(values []settings.AppSettingView) {
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
		palette = nextPalette
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
	switch effect {
	case "breathe", "spinner", "rainbow", "pulse", "comet", "sparkle":
	default:
		effect = "breathe"
	}
	switch palette {
	case "aurora", "ocean", "sunset", "ember", "mono", "spectrum":
	default:
		palette = "aurora"
	}

	r.mu.Lock()
	r.effect, r.palette = effect, palette
	r.brightness, r.speed = brightness, speed
	r.scheduleEnabled = scheduleEnabled
	r.activeFrom, r.activeUntil = activeFrom, activeUntil
	r.mu.Unlock()
	r.refreshSchedule()
	r.signalSchedule()
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
	r.applied = sig
	r.mu.Unlock()
	if err := pres.UpdateFrame(frame); err != nil && r.d.ctx.Err() == nil {
		r.d.logs.Debugf(ringAppID, "", "baseline: %v", err)
	}
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
	primary, secondary, saturation := paletteColors(r.palette)
	brightness := clampUnit(r.brightness)
	speed := clampRange(r.speed, 0.1, 4)
	var (
		id     string
		params presets.Params
	)
	switch r.effect {
	case "spinner":
		id = "system.ring.spinner"
		params = presets.Params{
			"speed": 0.45 * speed, "color": scaleColor(primary, brightness),
			"intensity": 1.0, "tail": int64(5),
		}
	case "rainbow":
		id = "system.ring.rainbow"
		params = presets.Params{
			"speed": 0.15 * speed, "saturation": saturation, "brightness": brightness,
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
		fmt.Sprintf("%s|%s|%.3f|%.3f", r.effect, r.palette, r.brightness, r.speed)
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

func (r *builtinRing) handleCall(ctx context.Context, service, rpc string, request []byte) ([][]byte, error) {
	if service != ringServiceName {
		return nil, status.Errorf(codes.Unimplemented, "ring app does not implement %s", service)
	}
	if rpc != "GetState" {
		return nil, status.Errorf(codes.Unimplemented, "unknown RingService method %s", rpc)
	}
	if len(request) > 0 {
		msg := &ringv1.GetStateRequest{}
		if err := protojson.Unmarshal(request, msg); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "ring app is not running")
	}
	out := &ringv1.RingState{
		Effect: r.effect, Palette: r.palette,
		Brightness: float32(r.brightness), Speed: float32(r.speed),
		PresentationId: r.presentationIDLocked(),
		Active:         r.active,
	}
	r.mu.Unlock()
	payload, err := encodeRingState(out)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return [][]byte{payload}, nil
}

// validateRingSettings rejects select values the generic form can submit but
// the app cannot render.
func validateRingSettings(values map[string]json.RawMessage) error {
	var effect string
	if raw, ok := values["effect"]; ok {
		if json.Unmarshal(raw, &effect) != nil {
			return fmt.Errorf("effect must be text")
		}
		switch effect {
		case "breathe", "spinner", "rainbow", "pulse", "comet", "sparkle":
		default:
			return fmt.Errorf("effect %q is not supported", effect)
		}
	}
	var palette string
	if raw, ok := values["palette"]; ok {
		if json.Unmarshal(raw, &palette) != nil {
			return fmt.Errorf("palette must be text")
		}
		switch palette {
		case "aurora", "ocean", "sunset", "ember", "mono", "spectrum":
		default:
			return fmt.Errorf("palette %q is not supported", palette)
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

func encodeProtoResponse(msg proto.Message) ([]byte, error) {
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
}

// encodeRingState emits default values so the built-in app always reports
// active=false while its window is closed. Without that, proto3 would omit the
// field and a reader could not tell "inactive" from "this provider does not
// report activity".
func encodeRingState(msg proto.Message) ([]byte, error) {
	return protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}.Marshal(msg)
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

func paletteColors(name string) (primary, secondary backend.RGB, saturation float64) {
	switch name {
	case "ocean":
		return backend.RGB{R: 0, G: 180, B: 255}, backend.RGB{R: 0, G: 70, B: 210}, 1
	case "sunset":
		return backend.RGB{R: 255, G: 120, B: 40}, backend.RGB{R: 255, G: 60, B: 150}, 1
	case "ember":
		return backend.RGB{R: 255, G: 40, B: 20}, backend.RGB{R: 255, G: 170, B: 20}, 1
	case "mono":
		return backend.RGB{R: 255, G: 255, B: 255}, backend.RGB{R: 150, G: 150, B: 150}, 0
	case "spectrum":
		return backend.RGB{R: 255, G: 70, B: 210}, backend.RGB{R: 70, G: 200, B: 255}, 1
	default:
		return backend.RGB{R: 0, G: 220, B: 180}, backend.RGB{R: 80, G: 130, B: 255}, 1
	}
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
