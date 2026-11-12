package daemon

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/manifest"
	"capsulad/internal/registry"
	"capsulad/internal/settings"
)

// Clock and ring are compiled-in system applications. They live in the same
// registry, bus and application-configuration surfaces as installed packages:
// they are listed, launched, stopped and discovered like any other app, and
// present content exclusively through presentations and captures. Only their
// packaging differs: they cannot be installed, removed or shadowed by a
// package, and their runtime is the daemon itself.
const (
	clockAppID = "capsula.clock"
	ringAppID  = "capsula.ring"

	clockServiceName = "capsula.contracts.clock.v1.ClockService"
	ringServiceName  = "capsula.contracts.ring.v1.RingService"

	// Legacy IDs from the removed sample applications. They are used only to
	// migrate persisted state, never to launch or resolve a provider.
	legacyClockAppID     = "org.capsula.clock"
	legacyRingAppID      = "org.example.ring-controller"
	legacyAssistantAppID = "org.capsula.assistant"
	legacyMediaAppID     = "org.capsula.media"

	// Settings namespaces are the app IDs themselves. Kept as named constants
	// because persisted settings and migration code refer to them.
	builtinClockSettingsID = clockAppID
	builtinRingSettingsID  = ringAppID
)

//go:embed builtinschemas/clock.protoset
var clockDescriptorSet []byte

//go:embed builtinschemas/ring.protoset
var ringDescriptorSet []byte

type builtinProviders struct {
	clock *builtinClock
	ring  *builtinRing
}

// newBuiltins creates the internal system apps and registers them with the
// daemon. Registration makes them visible to application listing, interface
// discovery, default-provider seeding and routed calls before the output loop
// starts. Content is applied when the run lifecycle calls start().
func newBuiltins(d *Daemon) *builtinProviders {
	out := &builtinProviders{}
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
			Manifest:   clockManifest(),
			Interfaces: []registry.Interface{clockInterface()},
			Start:      app.start,
			Stop:       app.stop,
			HandleCall: app.handleCall,
			Action:     app.action,
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

// start is idempotent. Run starts apps through their SystemApp.Start hooks, so
// this helper exists for callers that own both runtimes explicitly.
func (b *builtinProviders) start() {
	if b == nil {
		return
	}
	if b.clock != nil {
		b.clock.start()
	}
	if b.ring != nil {
		b.ring.start()
	}
}

// --- system app packaging ---------------------------------------------------

func clockManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ID: clockAppID, Name: "Clock", Version: "1.0.0",
		Description: "Built-in segment clock with stopwatch, timers and alarms.",
		Background:  true, StartOnBoot: true, SingleInstance: true, RestartOnFailure: true,
		Provides:   []string{"segment_display_provider"},
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
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
			{Key: "alarm_label", Label: "Alarm label", Type: "text", Default: "Alarm"},
			{Key: "alarm_hour", Label: "Alarm hour", Type: "integer", Default: 7,
				Min: numberPtr(0), Max: numberPtr(23)},
			{Key: "alarm_minute", Label: "Alarm minute", Type: "integer", Default: 0,
				Min: numberPtr(0), Max: numberPtr(59)},
			{Key: "alarm_days", Label: "Alarm days", Type: "integer", Default: 0,
				Description: "Weekday bitmask: bit 0 Monday through bit 6 Sunday. 0 fires once at the next occurrence and then disables.",
				Min:         numberPtr(0), Max: numberPtr(127)},
			{Key: "alarm_timezone", Label: "Alarm timezone", Type: "text", Default: "",
				Description: "IANA zone for the alarm. Empty follows the device timezone."},
			{Key: "alarm_enabled", Label: "Alarm enabled", Type: "boolean", Default: false},
		},
		Actions: []manifest.Action{
			{ID: "stopwatch_start", Label: "Start stopwatch"},
			{ID: "stopwatch_pause", Label: "Pause stopwatch"},
			{ID: "stopwatch_resume", Label: "Resume stopwatch"},
			{ID: "stopwatch_reset", Label: "Reset stopwatch"},
			{ID: "timer_start", Label: "Start timer"},
			{ID: "timer_cancel", Label: "Cancel timer"},
			{ID: "alarm_enable", Label: "Enable alarm"},
			{ID: "alarm_disable", Label: "Disable alarm"},
			{ID: "alert_dismiss", Label: "Dismiss alert"},
		},
		Interfaces: []manifest.Interface{{
			Service: clockServiceName, Version: "v1",
		}},
	}
}

func ringManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ID: ringAppID, Name: "Ring", Version: "1.0.0",
		Description: "Built-in LED ring baseline with palette-driven effects.",
		Background:  true, StartOnBoot: true, SingleInstance: true, RestartOnFailure: true,
		Provides:   []string{"led_ring_controller"},
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeBuiltin},
		Settings: []manifest.Setting{
			{Key: "effect", Label: "Ring effect", Type: "select", Default: "breathe",
				Description: "Baseline effect while no application or system indication owns the ring.",
				Choices: []manifest.Choice{
					{Value: "breathe", Label: "Breathe"},
					{Value: "spinner", Label: "Spinner"},
					{Value: "rainbow", Label: "Rainbow"},
					{Value: "pulse", Label: "Pulse"},
					{Value: "comet", Label: "Comet"},
					{Value: "sparkle", Label: "Sparkle"},
				}},
			{Key: "palette", Label: "Palette", Type: "select", Default: "aurora",
				Choices: []manifest.Choice{
					{Value: "aurora", Label: "Aurora"},
					{Value: "ocean", Label: "Ocean"},
					{Value: "sunset", Label: "Sunset"},
					{Value: "ember", Label: "Ember"},
					{Value: "mono", Label: "Mono"},
					{Value: "spectrum", Label: "Spectrum"},
				}},
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
		Interfaces: []manifest.Interface{{
			Service: ringServiceName, Version: "v1",
		}},
	}
}

func numberPtr(v float64) *float64 { return &v }

func clockInterface() registry.Interface {
	names := []string{
		"GetState", "StopwatchStart", "StopwatchPause", "StopwatchResume", "StopwatchReset",
		"StartTimer", "CancelTimer", "ListTimers", "SetAlarm", "SetAlarmEnabled",
		"DeleteAlarm", "ListAlarms", "DismissAlert",
	}
	methods := make([]registry.Method, 0, len(names))
	for _, name := range names {
		methods = append(methods, registry.Method{Name: name})
	}
	return registry.Interface{
		Service: clockServiceName, Version: "v1",
		Summary:    "Stopwatch, timers and alarms for the built-in segment clock.",
		Descriptor: clockDescriptorSet, Methods: methods, Docs: clockServiceDocs,
	}
}

func ringInterface() registry.Interface {
	return registry.Interface{
		Service: ringServiceName, Version: "v1",
		Summary:    "Current palette-driven effect rendered by the built-in ring app.",
		Descriptor: ringDescriptorSet,
		Methods:    []registry.Method{{Name: "GetState"}},
		Docs:       ringServiceDocs,
	}
}

const clockServiceDocs = `# ClockService
Stopwatch, timer and alarm control for the built-in Clock system app.
All calls are unary and routed with AppBusService.Call targeting app_id capsula.clock.
Requests and responses are protobuf JSON (snake_case fields).

## GetState
Returns local time, the effective timezone, stopwatch, timers, alarms and active alerts.

## StopwatchStart
Starts a stopwatch at zero when idle; no-op while already running.

## StopwatchPause
Pauses a running stopwatch and preserves monotonic elapsed time.

## StopwatchResume
Continues a paused stopwatch. On an idle stopwatch it starts at zero.

## StopwatchReset
Stops the stopwatch and clears elapsed time.

## StartTimer
Starts a countdown. duration_ms must be positive. The completion is also
announced audibly and on the segment/ring display through normal captures.

## CancelTimer
Cancels a running timer by ID. Unknown IDs return NOT_FOUND.

## ListTimers
Lists running timers and completed timers whose alert is still active.

## SetAlarm
Creates an alarm with an empty alarm_id or updates one in place. days_of_week
bit 0 is Monday through bit 6 Sunday; 0 is a one-shot alarm that disables after
firing. timezone is an IANA name; empty follows the device timezone. The alarm
ID "default" is additionally managed by this app's settings.

## SetAlarmEnabled
Enables or disables an alarm and recomputes its next trigger.

## DeleteAlarm
Deletes a service-created alarm. The settings-managed "default" alarm cannot be deleted.

## ListAlarms
Lists alarms with effective timezone and next trigger.

## DismissAlert
Stops the visible/audible alert for a timer or alarm immediately.
`

const ringServiceDocs = `# RingService
Current state of the built-in Ring system app. Calls are unary and routed with
AppBusService.Call targeting app_id capsula.ring.

## GetState
Returns the effective effect, palette, brightness, speed, the owned baseline
presentation ID and whether the baseline is currently lit. State changes are
made through application settings.

The optional active window (schedule_enabled, schedule_start, schedule_end) is
evaluated against local wall-clock time in the device timezone. Outside the
window the app presents an explicit blank frame on its normal baseline
presentation, so the ring goes dark and returns at the next boundary without
losing ownership; system and app indications still preempt it as usual.
`

// --- legacy state migration -------------------------------------------------

// migrateLegacyState rewrites persisted choices that referred to the removed
// sample applications and carries their app settings across. It runs once per
// boot before defaults are seeded, so a migrated empty default does not get
// replaced by a stale provider hint.
func (d *Daemon) migrateLegacyState() {
	d.migrateLegacyDefaults()
	d.migrateLegacyAppSettings(legacyClockAppID, clockAppID)
	d.migrateLegacyAppSettings(legacyRingAppID, ringAppID)
	d.migrateLegacyRingMode()
}

func (d *Daemon) migrateLegacyDefaults() {
	snap := d.user.Get()
	patch := snap.Values
	changed := false
	assistantChanged := false
	for resourceID, appID := range patch.DefaultAppByResource {
		switch appID {
		case legacyClockAppID:
			patch.DefaultAppByResource[resourceID] = systemAppForResource(d, resourceID)
			changed = true
		case legacyRingAppID:
			if info := d.resources[resourceID]; info != nil && info.Kind == frameworkv1.ResourceKind_RESOURCE_LED_RING {
				patch.DefaultAppByResource[resourceID] = ringAppID
			} else {
				patch.DefaultAppByResource[resourceID] = clockAppID
			}
			changed = true
		case legacyAssistantAppID, legacyMediaAppID:
			delete(patch.DefaultAppByResource, resourceID)
			changed = true
		}
	}
	if patch.DefaultAssistantAppID == legacyAssistantAppID {
		patch.DefaultAssistantAppID = ""
		changed = true
		assistantChanged = true
	}
	if !changed {
		return
	}
	fields := []string{"default_app_by_resource"}
	if assistantChanged {
		fields = append(fields, "default_assistant_app_id")
	}
	if _, err := d.user.Update(snap.Revision, patch, fields); err != nil {
		d.logs.Warnf("daemon", "", "could not migrate legacy provider defaults: %v", err)
		return
	}
	d.logs.Infof("daemon", "", "migrated legacy provider defaults to the system apps")
}

func systemAppForResource(d *Daemon, resourceID string) string {
	if info := d.resources[resourceID]; info != nil && info.Kind == frameworkv1.ResourceKind_RESOURCE_LED_RING {
		return ringAppID
	}
	return clockAppID
}

// migrateLegacyAppSettings copies a retired app's settings file into the
// system app namespace once. An existing destination is authoritative.
func (d *Daemon) migrateLegacyAppSettings(srcID, dstID string) {
	root := filepath.Join(d.cfg.DataDir, "app-settings")
	src := filepath.Join(root, srcID, "settings.json")
	dst := filepath.Join(root, dstID, "settings.json")
	if _, err := os.Stat(dst); err == nil {
		return
	} else if !os.IsNotExist(err) {
		d.logs.Warnf(dstID, "", "settings migration: %v", err)
		return
	}
	data, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		d.logs.Warnf(dstID, "", "settings migration: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		d.logs.Warnf(dstID, "", "settings migration: %v", err)
		return
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		d.logs.Warnf(dstID, "", "settings migration: %v", err)
		return
	}
	d.logs.Infof(dstID, "", "migrated legacy %s settings", srcID)
}

// migrateLegacyRingMode carries the hidden provider's "mode" key forward to
// the ring app's "effect" key when no explicit effect exists, then removes the
// obsolete key so no competing alias survives. Explicit effects are preserved.
func (d *Daemon) migrateLegacyRingMode() {
	path := filepath.Join(d.cfg.DataDir, "app-settings", ringAppID, "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc struct {
		Revision uint64                     `json:"revision"`
		Values   map[string]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.Values == nil {
		return
	}
	raw, ok := doc.Values["mode"]
	if !ok {
		return
	}
	if _, hasEffect := doc.Values["effect"]; !hasEffect {
		var mode string
		if json.Unmarshal(raw, &mode) != nil {
			return
		}
		effect := "breathe"
		switch mode {
		case "spinner":
			effect = "spinner"
		case "rainbow":
			effect = "rainbow"
		}
		encoded, err := json.Marshal(effect)
		if err != nil {
			return
		}
		doc.Values["effect"] = encoded
	}
	// The obsolete alias is removed after its one-time translation; keeping it
	// would leave two competing settings for the same behavior.
	delete(doc.Values, "mode")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		d.logs.Warnf(ringAppID, "", "mode migration: %v", err)
		return
	}
	d.logs.Infof(ringAppID, "", "removed obsolete ring mode setting")
}

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
