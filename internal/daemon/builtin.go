package daemon

import (
	"encoding/json"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/manifest"
	"capsulad/internal/registry"
	"capsulad/internal/settings"
)

// Clock and ring are compiled-in system applications. They live in the same
// registry, bus and application-configuration surfaces as installed packages:
// they are listed, launched and stopped like any other app, and present
// content exclusively through presentations and captures. Only their
// packaging differs: they cannot be installed, removed or shadowed by a
// package, and their runtime is the daemon itself.
//
// They publish standard clock and ring contracts under
// proto/capsulad/contracts. Those services are advertised through
// AppInterfaceService and reachable by any supervised app via the normal app
// bus, exactly like an installed app's declared interface. Their settings,
// actions and hardware gestures remain the primary product surface.
const (
	clockAppID = "capsulad.clock"
	ringAppID  = "capsulad.ring"

	// Settings namespaces are the app IDs themselves. Kept as named constants
	// because persisted settings refer to them.
	builtinClockSettingsID = clockAppID
	builtinRingSettingsID  = ringAppID
)

type builtinProviders struct {
	d     *Daemon
	clock *builtinClock
	ring  *builtinRing
}

// newBuiltins creates the internal system apps and registers them with the
// daemon. Registration makes them visible to application listing, interface
// discovery, default-provider seeding and routed calls before the output loop
// starts. Content is applied when the run lifecycle calls start().
func newBuiltins(d *Daemon) *builtinProviders {
	out := &builtinProviders{d: d}
	for _, info := range d.resources {
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			if out.clock == nil {
				out.clock = newBuiltinClock(d, info.ResourceId)
			}
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			if out.ring == nil {
				out.ring = newBuiltinRing(d, info.ResourceId)
			}
		}
	}
	if out.clock != nil {
		d.RegisterAppSettingsValidator(clockAppID, validateClockSettings)
		app := out.clock
		if err := d.RegisterSystemApp(SystemApp{
			Manifest:       clockManifest(),
			FrameworkOwned: true,
			Interfaces:     []registry.Interface{clockInterface()},
			Start:          app.start,
			Stop:           app.stop,
			HandleCall:     app.handleCall,
			Action:         app.action,
		}); err != nil {
			d.logs.Errorf(clockAppID, "", "register system app: %v", err)
		}
	}
	if out.ring != nil {
		d.RegisterAppSettingsValidator(ringAppID, validateRingSettings)
		app := out.ring
		if err := d.RegisterSystemApp(SystemApp{
			Manifest:   ringManifest(),
			Interfaces: []registry.Interface{ringInterface()},
			Start:      app.start,
			Stop:       app.stop,
			HandleCall: app.handleCall,
		}); err != nil {
			d.logs.Errorf(ringAppID, "", "register system app: %v", err)
		}
	}
	return out
}

// start starts every registered built-in the boot policy requires, through the
// same selection-aware path Run uses. It is idempotent.
func (b *builtinProviders) start() {
	if b == nil || b.d == nil {
		return
	}
	b.d.startSystemApps()
}

// --- system app packaging ---------------------------------------------------

func clockManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ID: clockAppID, Name: "Clock", Version: "1.0.0",
		Description: "Built-in segment clock with stopwatch, timers and alarms.",
		Background:  true, StartOnBoot: true, SingleInstance: true, RestartOnFailure: true,
		Provides:   []string{"segment_display_provider"},
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
		SettingsUI: &manifest.SettingsUI{HTML: "clock.html"},
		Settings: []manifest.Setting{
			{Key: "blink", Label: "Blink colon", Type: "boolean", Default: true},
			{Key: "timezone", Label: "Timezone", Type: "text", Default: "",
				Description: "IANA zone name, e.g. Asia/Shanghai. Empty follows the device timezone."},
			{Key: "display_mode", Label: "Display", Type: "select", Default: "clock",
				Description: "Shown when no timer or stopwatch is active.",
				Choices: []manifest.Choice{
					{Value: "clock", Label: "Clock"},
					{Value: "stopwatch", Label: "Stopwatch"},
					{Value: "timer", Label: "Timer"},
				}},
			{Key: "timer_seconds", Label: "Timer duration (seconds)", Type: "integer", Default: 300,
				Description: "Duration used by the Start timer action.",
				Min:         numberPtr(1), Max: numberPtr(86400)},
			{Key: "stopwatch_reset_gesture", Label: "Stopwatch reset gesture", Type: "select", Default: "hold",
				Description: "How the circle/play-pause button resets a running or paused stopwatch. A single tap pauses or resumes it; the chosen gesture resets it and never summons the assistant.",
				Choices: []manifest.Choice{
					{Value: "hold", Label: "Hold the button"},
					{Value: "double_tap", Label: "Double tap the button"},
				}},
			{Key: "alarms", Label: "Alarms", Type: "text", Default: "",
				Description: "Comma-separated five-field dcron schedules (minute hour day-of-month month day-of-week), evaluated in the device timezone. Optional modifiers: @IANA-zone, !once, !disabled. Example: \"0,30 7 * * 1,3,5, 15 8 * * *\". Commas inside a field are literal; an entry separator is a comma followed by whitespace."},
			{Key: "alarm_labels", Label: "Alarm labels", Type: "text", Default: "{}",
				Description: "Display labels keyed by canonical alarm identity. Schedule semantics live only in the alarms setting."},
		},
		Actions: []manifest.Action{
			{ID: "stopwatch_start", Label: "Start stopwatch"},
			{ID: "stopwatch_pause", Label: "Pause stopwatch"},
			{ID: "stopwatch_resume", Label: "Resume stopwatch"},
			{ID: "stopwatch_reset", Label: "Reset stopwatch"},
			{ID: "timer_start", Label: "Start timer"},
			{ID: "timer_cancel", Label: "Cancel timer"},
			{ID: "alert_dismiss", Label: "Dismiss alert"},
		},
	}
}

func ringManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ID: ringAppID, Name: "Ring", Version: "1.0.0",
		Description: "Built-in LED ring baseline with palette-driven effects.",
		Background:  true, StartOnBoot: true, SingleInstance: true, RestartOnFailure: true,
		Provides:   []string{"led_ring_controller"},
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
		SettingsUI: &manifest.SettingsUI{HTML: "ring.html"},
		Settings: []manifest.Setting{
			{Key: "effect", Label: "Ring effect", Type: "select", Default: "breathe",
				Description: "Baseline effect while no application or system indication owns the ring. Static is one solid colour.",
				Choices: []manifest.Choice{
					{Value: "breathe", Label: "Breathe"},
					{Value: "spinner", Label: "Spinner"},
					{Value: "rainbow", Label: "Rainbow"},
					{Value: "pulse", Label: "Pulse"},
					{Value: "comet", Label: "Comet"},
					{Value: "sparkle", Label: "Sparkle"},
					{Value: "static", Label: "Static colour"},
				}},
			{Key: "palette", Label: "Palette", Type: "text",
				Default:     "linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)",
				Description: "Canonical gradient: linear-gradient(<angle>deg, #RRGGBB <pct>%, ...) or a solid #RRGGBB."},
			{Key: "brightness", Label: "Brightness", Type: "number", Default: 1.0,
				Min: numberPtr(0), Max: numberPtr(1)},
			{Key: "speed", Label: "Speed", Type: "number", Default: 1.0,
				Min: numberPtr(0.1), Max: numberPtr(4)},
			{Key: "schedule_enabled", Label: "Active window", Type: "boolean", Default: false,
				Description: "Light the ring only between the start and end times. Volume, mute and alert indications still use the ring."},
			{Key: "schedule_start", Label: "Active from", Type: "text", Default: "22:00",
				Description: "HH:MM in the device timezone. The window may cross midnight; equal start and end mean all day."},
			{Key: "schedule_end", Label: "Active until", Type: "text", Default: "07:00",
				Description: "HH:MM in the device timezone. Outside the window the app presents a blank frame instead of the effect."},
		},
	}
}

func numberPtr(v float64) *float64 { return &v }

// decodeSetting unmarshals one raw setting value from the app store.
func decodeSetting(values []settings.AppSettingView, key string, out any) bool {
	for _, v := range values {
		if v.Key != key {
			continue
		}
		return json.Unmarshal([]byte(v.Value), out) == nil
	}
	return false
}
