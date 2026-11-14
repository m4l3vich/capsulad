package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/dcron"
	"capsulad/internal/manifest"
	"capsulad/internal/registry"
	"capsulad/internal/ringpalette"
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
// proto/capsula/contracts. Those services are advertised through
// AppInterfaceService and reachable by any supervised app via the normal app
// bus, exactly like an installed app's declared interface. Their settings,
// actions and hardware gestures remain the primary product surface.
const (
	clockAppID = "capsula.clock"
	ringAppID  = "capsula.ring"

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

// --- legacy state migration -------------------------------------------------

// migrateLegacyState rewrites persisted choices that referred to the removed
// sample applications and carries their app settings across. It runs once per
// boot before defaults are seeded, so a migrated empty default does not get
// replaced by a stale provider hint.
func (d *Daemon) migrateLegacyState() {
	d.migrateLegacyFlatAppSettings(clockAppID)
	d.migrateLegacyFlatAppSettings(ringAppID)
	d.migrateLegacyDefaults()
	d.migrateLegacyAppSettings(legacyClockAppID, clockAppID)
	d.migrateLegacyAppSettings(legacyRingAppID, ringAppID)
	d.migrateLegacyRingMode()
	d.migrateLegacyRingPalette()
	d.migrateLegacyClockAlarms()
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

// migrateLegacyFlatAppSettings converts the older deployed layout
// app-settings/<id>.json into the canonical directory layout once. An existing
// canonical file is authoritative and is never overwritten; the flat source is
// left in place so a downgrade still finds its preferences.
func (d *Daemon) migrateLegacyFlatAppSettings(appID string) {
	root := filepath.Join(d.cfg.DataDir, "app-settings")
	flat := filepath.Join(root, appID+".json")
	canonical := filepath.Join(root, appID, "settings.json")
	if _, err := os.Stat(canonical); err == nil {
		return
	} else if !os.IsNotExist(err) {
		d.logs.Warnf(appID, "", "settings migration: %v", err)
		return
	}
	doc, ok := readAppSettingsDoc(flat)
	if !ok {
		return
	}
	if !writeAppSettingsDoc(canonical, doc) {
		d.logs.Warnf(appID, "", "could not convert flat settings file")
		return
	}
	d.logs.Infof(appID, "", "converted legacy flat settings file to the canonical layout")
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
	if err := writeFileAtomic(dst, data, 0o600); err != nil {
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
	if err := writeFileAtomic(path, out, 0o600); err != nil {
		d.logs.Warnf(ringAppID, "", "mode migration: %v", err)
		return
	}
	d.logs.Infof(ringAppID, "", "removed obsolete ring mode setting")
}

// migrateLegacyClockAlarms converts the retired single-alarm settings into the
// canonical `alarms` entry list once. Time, recurrence, IANA timezone, one-shot
// semantics and the enabled state are all preserved: the old weekday bitmask
// (Monday is bit 0) becomes a dcron day-of-week list, days==0 becomes a daily
// expression carrying !once, and enabled=false becomes !disabled.
func (d *Daemon) migrateLegacyClockAlarms() {
	path := filepath.Join(d.cfg.DataDir, "app-settings", clockAppID, "settings.json")
	doc, ok := readAppSettingsDoc(path)
	if !ok {
		return
	}
	legacy := []string{"alarm_label", "alarm_hour", "alarm_minute", "alarm_days", "alarm_timezone", "alarm_enabled"}
	present := false
	for _, key := range legacy {
		if _, ok := doc.Values[key]; ok {
			present = true
		}
	}
	if !present {
		return
	}
	if _, has := doc.Values["alarms"]; !has {
		hour, minute, days := 7, 0, 0
		textInt(doc.Values["alarm_hour"], &hour)
		textInt(doc.Values["alarm_minute"], &minute)
		textInt(doc.Values["alarm_days"], &days)
		if hour < 0 || hour > 23 {
			hour = 7
		}
		if minute < 0 || minute > 59 {
			minute = 0
		}
		var enabled bool
		if raw, ok := doc.Values["alarm_enabled"]; ok {
			_ = json.Unmarshal(raw, &enabled)
		}
		var zone string
		if raw, ok := doc.Values["alarm_timezone"]; ok {
			_ = json.Unmarshal(raw, &zone)
		}
		if zone != "" {
			if _, err := time.LoadLocation(zone); err != nil {
				d.logs.Warnf(clockAppID, "", "legacy alarm timezone %q is unavailable; alarm will follow the device timezone", zone)
				zone = ""
			}
		}
		entry := dcron.Entry{
			Expr: legacyAlarmSchedule(hour, minute, days), Zone: zone,
			Once: days == 0, Disabled: !enabled,
		}
		text, err := json.Marshal(dcron.FormatEntries([]dcron.Entry{entry}))
		if err == nil {
			doc.Values["alarms"] = text
		}
		var label string
		if raw, ok := doc.Values["alarm_label"]; ok {
			_ = json.Unmarshal(raw, &label)
		}
		if label == "" {
			label = "Alarm"
		}
		labels := map[string]string{}
		if raw, ok := doc.Values["alarm_labels"]; ok {
			var stored string
			if json.Unmarshal(raw, &stored) == nil && stored != "" {
				_ = json.Unmarshal([]byte(stored), &labels)
			}
		}
		if _, exists := labels[entry.ID()]; !exists {
			labels[entry.ID()] = label
			if encoded, err := json.Marshal(labels); err == nil {
				if wrapped, err := json.Marshal(string(encoded)); err == nil {
					doc.Values["alarm_labels"] = wrapped
				}
			}
		}
	}
	for _, key := range legacy {
		delete(doc.Values, key)
	}
	if writeAppSettingsDoc(path, doc) {
		d.logs.Infof(clockAppID, "", "migrated legacy alarm settings to the alarms schedule list")
	}
}

// legacyAlarmSchedule builds one five-field expression from the old hour,
// minute and Monday-first weekday bitmask. days==0 was a one-shot alarm; the
// caller adds !once so the daily expression fires at its next occurrence and
// then disables itself.
func legacyAlarmSchedule(hour, minute, days int) string {
	if days == 0 {
		return fmt.Sprintf("%d %d * * *", minute, hour)
	}
	dow := make([]string, 0, 7)
	if days&(1<<6) != 0 {
		dow = append(dow, "0")
	}
	for bit := 0; bit < 6; bit++ {
		if days&(1<<uint(bit)) != 0 {
			dow = append(dow, strconv.Itoa(bit+1))
		}
	}
	if len(dow) == 0 {
		return fmt.Sprintf("%d %d * * *", minute, hour)
	}
	return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(dow, ","))
}

// migrateLegacyRingPalette rewrites a named legacy palette to its canonical
// gradient. Explicit gradient strings are already canonical and are untouched.
func (d *Daemon) migrateLegacyRingPalette() {
	path := filepath.Join(d.cfg.DataDir, "app-settings", ringAppID, "settings.json")
	doc, ok := readAppSettingsDoc(path)
	if !ok {
		return
	}
	raw, exists := doc.Values["palette"]
	if !exists {
		return
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return
	}
	gradient, legacy := legacyPaletteGradients[name]
	if !legacy {
		if _, err := ringpalette.Parse(name); err != nil {
			// A hand-edited malformed value must not block normal startup;
			// the app falls back to the default and the validator rejects the
			// next edit until it is corrected.
			return
		}
		return
	}
	encoded, err := json.Marshal(gradient)
	if err != nil {
		return
	}
	doc.Values["palette"] = encoded
	if writeAppSettingsDoc(path, doc) {
		d.logs.Infof(ringAppID, "", "migrated legacy palette %q to a canonical gradient", name)
	}
}

var legacyPaletteGradients = map[string]string{
	"aurora":   "linear-gradient(90deg, #00DCB4 0%, #5082FF 100%)",
	"ocean":    "linear-gradient(90deg, #00B4FF 0%, #0046D2 100%)",
	"sunset":   "linear-gradient(90deg, #FF7828 0%, #FF3C96 100%)",
	"ember":    "linear-gradient(90deg, #FF2814 0%, #FFAA14 100%)",
	"mono":     "linear-gradient(90deg, #FFFFFF 0%, #969696 100%)",
	"spectrum": "linear-gradient(90deg, #FF46D2 0%, #46C8FF 100%)",
}

type appSettingsDoc struct {
	Revision uint64                     `json:"revision"`
	Values   map[string]json.RawMessage `json:"values"`
}

func readAppSettingsDoc(path string) (appSettingsDoc, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return appSettingsDoc{}, false
	}
	var doc appSettingsDoc
	if json.Unmarshal(data, &doc) != nil || doc.Values == nil {
		return appSettingsDoc{}, false
	}
	return doc, true
}

func writeAppSettingsDoc(path string, doc appSettingsDoc) bool {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false
	}
	if err := writeFileAtomic(path, out, 0o600); err != nil {
		return false
	}
	return true
}

// writeFileAtomic replaces path with data using a same-directory temp file,
// fsync and rename. A crash during a migration can therefore never truncate an
// existing settings file into an unparseable one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func textInt(raw json.RawMessage, out *int) {
	if len(raw) == 0 {
		return
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		*out = int(n)
	}
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
