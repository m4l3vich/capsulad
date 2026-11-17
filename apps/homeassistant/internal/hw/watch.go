package hw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"capsulad/apps/homeassistant/internal/appcfg"
	"capsulad/apps/homeassistant/internal/ha"
	"capsulad/sdk/capsulad"
)

// Watcher cadences and bounds.
const (
	// mediaRetry bounds a failed Bluetooth media subscription.
	mediaRetry = 5 * time.Second
	// touchRetry bounds a failed touch capture or input subscription.
	touchRetry = 3 * time.Second
	// sensorRetry bounds a failed sensor watch stream.
	sensorRetry = 10 * time.Second
	// sensorGiveUp is the number of consecutive failures after which one
	// sensor's watch stops until the app restarts. A vanished sensor must not
	// become an endless reconnect loop.
	sensorGiveUp = 5
	// touchSettle is how long the loop waits after acquiring a touch capture
	// before subscribing; a capture is selected when its stream is granted.
	touchSettle = 150 * time.Millisecond
)

// Watchers publishes device state on a cadence and on framework changes. Every
// loop is independent and bounded: a failing surface logs, backs off and never
// blocks the MQTT session.
type Watchers struct {
	framework  *Framework
	bridge     *ha.Bridge
	cfg        appcfg.Config
	controller *Controller
	logf       func(format string, args ...any)

	sensors []ha.SensorInfo
	// published is the set of "component/slug" keys the catalog actually
	// publishes. Every watcher is gated on it, so a watcher can never emit a
	// state topic that has no discovery payload — which would be dead traffic
	// forever, and for Bluetooth an endless retry loop on a device with no
	// adapter.
	published map[string]bool
}

// NewWatchers returns the state watchers for one bridge configuration. Which
// watchers run is derived from the published entity set, not from the settings
// that produced it: the catalog is the single source of truth for what Home
// Assistant can see.
func NewWatchers(framework *Framework, bridge *ha.Bridge, cfg appcfg.Config,
	sensors []ha.SensorInfo, controller *Controller, catalog ha.Catalog,
	logf func(string, ...any)) *Watchers {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Watchers{
		framework: framework, bridge: bridge, cfg: cfg, sensors: sensors,
		controller: controller, published: publishedSet(catalog), logf: logf,
	}
}

// publishedSet indexes a catalog by "component/slug".
func publishedSet(catalog ha.Catalog) map[string]bool {
	set := make(map[string]bool, len(catalog.Entities))
	for _, entity := range catalog.Entities {
		set[entity.Component+"/"+entity.Slug] = true
	}
	return set
}

// enabled reports whether the catalog publishes one entity.
func (w *Watchers) enabled(component, slug string) bool {
	return w.published[component+"/"+slug]
}

// loop is one state watcher plus the name that says why it runs.
type loop struct {
	name string
	run  func(context.Context)
}

// loops lists the watchers this bridge will run. The decision is derived from
// the published entity set, so it is testable without a broker: a watcher can
// never emit a state topic that has no discovery payload.
func (w *Watchers) loops() []loop {
	loops := make([]loop, 0, 4+len(w.sensors))
	if w.enabled("sensor", ha.SlugUptime) || w.enabled("sensor", ha.SlugVolume) ||
		w.enabled("binary_sensor", ha.SlugMicrophoneMute) {
		loops = append(loops, loop{name: "device-state", run: w.runDeviceState})
	}
	for _, sensor := range w.sensors {
		if !w.enabled("sensor", sensor.Slug) {
			continue
		}
		sensor := sensor
		loops = append(loops, loop{
			name: "sensor:" + sensor.Slug,
			run:  func(ctx context.Context) { w.runSensor(ctx, sensor) },
		})
	}
	if w.enabled("sensor", ha.SlugBluetoothState) {
		loops = append(loops, loop{name: "media", run: w.runMedia})
	}
	if w.enabled("event", ha.SlugTouch) {
		loops = append(loops, loop{name: "touch", run: w.runTouch})
	}
	return loops
}

// Run starts every watcher and returns when ctx ends.
func (w *Watchers) Run(ctx context.Context) {
	loops := w.loops()
	done := make([]chan struct{}, 0, len(loops))
	for _, l := range loops {
		finished := make(chan struct{})
		done = append(done, finished)
		go func(l loop, finished chan struct{}) {
			defer close(finished)
			l.run(ctx)
		}(l, finished)
	}
	for _, finished := range done {
		<-finished
	}
}

// runDeviceState publishes the uptime and mixer state immediately and then on
// the configured cadence.
func (w *Watchers) runDeviceState(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.StateInterval)
	defer ticker.Stop()
	var lastFailure string
	publish := func() {
		uptimeCtx, cancel := context.WithTimeout(ctx, callTimeout)
		uptime, err := w.framework.UptimeSeconds(uptimeCtx)
		cancel()
		if err != nil {
			w.fail("uptime", err, &lastFailure)
		} else if err := w.bridge.PublishUptime(ctx, uptime); err != nil {
			w.fail("publish uptime", err, &lastFailure)
		} else {
			lastFailure = ""
		}

		audioCtx, cancel := context.WithTimeout(ctx, callTimeout)
		state, err := w.framework.Client().AudioState(audioCtx)
		cancel()
		if err != nil {
			w.fail("audio state", err, &lastFailure)
			return
		}
		if err := w.bridge.PublishAudio(ctx, float64(state.MasterVolume), state.MicrophoneMuted); err != nil {
			w.fail("publish audio", err, &lastFailure)
		}
	}
	publish()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

// runSensor keeps one sensor watch stream alive and publishes every sample.
func (w *Watchers) runSensor(ctx context.Context, sensor ha.SensorInfo) {
	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.watchSensor(ctx, sensor)
		if ctx.Err() != nil {
			return
		}
		failures++
		w.logf("watchers: sensor %s: %v (%d/%d)", sensor.ID, err, failures, sensorGiveUp)
		if failures >= sensorGiveUp {
			w.logf("watchers: sensor %s: giving up after %d failures; it stays unpublished until the app restarts", sensor.ID, failures)
			return
		}
		sleep(ctx, sensorRetry)
	}
}

func (w *Watchers) watchSensor(ctx context.Context, sensor ha.SensorInfo) error {
	stream, err := w.framework.Client().OpenStream(ctx, sensorService, "WatchSensor", map[string]any{
		"sensorId":          sensor.ID,
		"minimumIntervalMs": int(w.cfg.SensorInterval / time.Millisecond),
	})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	defer stream.Close()
	for {
		var sample struct {
			SensorID string  `json:"sensorId"`
			Value    float64 `json:"value"`
			Unit     string  `json:"unit"`
		}
		if err := stream.RecvJSON(&sample); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("watch stream ended")
			}
			return fmt.Errorf("read sample: %w", err)
		}
		if err := w.bridge.PublishSensor(ctx, sensor, sample.Value); err != nil {
			return fmt.Errorf("publish sample: %w", err)
		}
	}
}

// runMedia follows the connected Bluetooth source.
func (w *Watchers) runMedia(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		stateCtx, cancel := context.WithTimeout(ctx, callTimeout)
		state, err := w.framework.MediaState(stateCtx)
		cancel()
		if err == nil {
			if err := w.bridge.PublishMedia(ctx, state); err != nil {
				w.logf("watchers: publish media state: %v", err)
			}
		} else {
			w.logf("watchers: media state: %v", err)
		}

		stream, err := w.framework.WatchMedia(ctx)
		if err != nil {
			w.logf("watchers: watch media: %v", err)
			sleep(ctx, mediaRetry)
			continue
		}
		w.consumeMedia(ctx, stream)
	}

}

func (w *Watchers) consumeMedia(ctx context.Context, stream *capsulad.BluetoothMediaStream) {
	defer stream.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case state, ok := <-stream.C:
			if !ok {
				w.logf("watchers: media watch stream ended; resubscribing")
				sleep(ctx, mediaRetry)
				return
			}
			if err := w.bridge.PublishMedia(ctx, convertMediaState(state)); err != nil {
				w.logf("watchers: publish media state: %v", err)
			}
		}
	}
}

// runTouch keeps a partial touch capture and its input subscription alive and
// forwards panel events to Home Assistant. The capture is released whenever the
// subscription ends, so the panel never stays claimed by a dead stream.
func (w *Watchers) runTouch(ctx context.Context) {
	if w.controller == nil {
		w.logf("watchers: touch exposure is on but no controller is available")
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		captureCtx, cancel := context.WithTimeout(ctx, presentTimeout)
		capture, err := w.controller.AcquireTouch(captureCtx)
		cancel()
		if err != nil {
			w.logf("watchers: touch capture: %v", err)
			sleep(ctx, touchRetry)
			continue
		}
		// A capture is selected when it is granted; give routing a moment
		// before subscribing. WatchInput requires the caller's selected
		// capture, so an early subscribe is refused and retried.
		sleep(ctx, touchSettle)
		if err := w.watchTouch(ctx, capture.ID()); err != nil && ctx.Err() == nil {
			w.logf("watchers: touch input: %v", err)
			sleep(ctx, touchRetry)
		}
		w.controller.ReleaseTouch()
	}
}

func (w *Watchers) watchTouch(ctx context.Context, captureID string) error {
	if captureID == "" {
		return fmt.Errorf("touch capture has no id")
	}
	input, err := w.framework.Client().WatchInput(ctx, captureID)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer input.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-input.Events():
			if !ok {
				return fmt.Errorf("input stream ended")
			}
			w.forwardTouch(ctx, event)
		}
	}
}

// forwardTouch publishes one panel event. Only the event types the entity
// declares are published: Home Assistant warns about any other type, and a
// partial capture never delivers the volume or microphone keys.
func (w *Watchers) forwardTouch(ctx context.Context, event capsulad.InputEvent) {
	switch {
	case event.Key != nil:
		kind := strings.ToLower(event.Key.Key)
		if kind != "play_pause" && kind != "center" {
			return
		}
		suffix := "press"
		if !event.Key.Pressed {
			suffix = "release"
		}
		if err := w.bridge.PublishTouchEvent(ctx, kind+"_"+suffix, nil); err != nil && !errors.Is(err, ha.ErrNoSession) {
			w.logf("watchers: publish touch event: %v", err)
		}
	case event.Gesture != nil:
		gesture := event.Gesture.GestureID
		switch gesture {
		case "swipe_left", "swipe_right", "swipe_up", "swipe_down":
		default:
			return
		}
		if err := w.bridge.PublishTouchEvent(ctx, gesture, map[string]any{"phase": event.Gesture.Phase}); err != nil && !errors.Is(err, ha.ErrNoSession) {
			w.logf("watchers: publish touch event: %v", err)
		}
	}
}

func (w *Watchers) fail(what string, err error, lastFailure *string) {
	message := what + ": " + err.Error()
	if message == *lastFailure {
		return
	}
	*lastFailure = message
	w.logf("watchers: %s", message)
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
