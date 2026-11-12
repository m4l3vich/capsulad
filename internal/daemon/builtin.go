package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
)

// The framework owns the segment clock and ring baseline directly. These are
// not installed applications: no manifest, process, run ID, bus connection or
// supervision entry exists for them. Their presentations belong to the
// reserved BuiltinAppID, which routing treats as the internal baseline after a
// user-selected default application and before a blank system fallback.
const (
	// builtinClockSettingsID is the app-settings namespace for the internal
	// clock. It never appears in the registry or application list.
	builtinClockSettingsID = "capsula.clock"
	builtinRingSettingsID  = "capsula.ring"
	builtinRunID           = "builtin"

	// Legacy IDs from the removed sample applications. They are used only to
	// migrate persisted state, never to launch or resolve a provider.
	legacyClockAppID     = "org.capsula.clock"
	legacyRingAppID      = "org.example.ring-controller"
	legacyAssistantAppID = "org.capsula.assistant"
	legacyMediaAppID     = "org.capsula.media"
)

// Seven-segment masks: bit 0 = a (top) ... bit 6 = g (middle).
var clockDigits = map[rune]uint16{
	'0': 0b0111111, '1': 0b0000110, '2': 0b1011011, '3': 0b1001111,
	'4': 0b1100110, '5': 0b1101101, '6': 0b1111101, '7': 0b0000111,
	'8': 0b1111111, '9': 0b1101111, '-': 0b1000000, ' ': 0,
}

type builtinProviders struct {
	clock *builtinClock
	ring  *builtinRing
}

// newBuiltins opens the internal baseline presentations. Content is applied
// once the daemon has settings and the output loop is running.
func newBuiltins(d *Daemon) *builtinProviders {
	out := &builtinProviders{}
	for _, info := range d.resources {
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			p := d.pres.Open(info.ResourceId, routing.BuiltinAppID, builtinRunID, presentations.Segment)
			out.clock = &builtinClock{d: d, pres: p, blink: true}
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			p := d.pres.Open(info.ResourceId, routing.BuiltinAppID, builtinRunID, presentations.Ring)
			out.ring = &builtinRing{d: d, pres: p}
		}
	}
	return out
}

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

// --- internal clock ---------------------------------------------------------

type builtinClock struct {
	d    *Daemon
	pres *presentations.Presentation

	mu       sync.Mutex
	blink    bool
	appTZ    string
	deviceTZ string
	loc      *time.Location
	now      func() time.Time
}

func (c *builtinClock) start() {
	c.mu.Lock()
	if c.now == nil {
		c.now = time.Now
	}
	c.mu.Unlock()
	c.loadSettings()
	c.render()
	go c.loop()
}

func (c *builtinClock) loadSettings() {
	snap, ch, cancel, err := c.d.appCfg.WatchApp(builtinClockSettingsID)
	if err != nil {
		c.d.logs.Warnf("clock", "", "settings unavailable: %v", err)
		return
	}
	c.applySettings(snap.Values)
	go func() {
		defer cancel()
		for {
			select {
			case <-c.d.ctx.Done():
				return
			case next, ok := <-ch:
				if !ok {
					return
				}
				c.applySettings(next.Values)
				c.render()
			}
		}
	}()
}

func (c *builtinClock) applySettings(values []settings.AppSettingView) {
	var blink *bool
	var tz *string
	for _, v := range values {
		switch v.Key {
		case "blink":
			var b bool
			if json.Unmarshal([]byte(v.Value), &b) == nil {
				blink = &b
			}
		case "timezone":
			var s string
			if json.Unmarshal([]byte(v.Value), &s) == nil {
				tz = &s
			}
		}
	}
	c.mu.Lock()
	changed := false
	if blink != nil && *blink != c.blink {
		c.blink = *blink
		changed = true
	}
	if tz != nil && *tz != c.appTZ {
		c.appTZ = *tz
		changed = true
	}
	loc := c.resolveLocationLocked()
	if changed || loc != c.loc {
		c.loc = loc
	}
	c.mu.Unlock()
}

// setDeviceTimezone follows the device-wide setting. The clock's own timezone
// setting wins when it is non-empty, matching the removed bundled app.
func (c *builtinClock) setDeviceTimezone(name string) {
	c.mu.Lock()
	if name == c.deviceTZ {
		c.mu.Unlock()
		return
	}
	c.deviceTZ = name
	loc := c.resolveLocationLocked()
	c.loc = loc
	c.mu.Unlock()
	c.render()
}

func (c *builtinClock) resolveLocationLocked() *time.Location {
	name := c.appTZ
	if name == "" {
		name = c.deviceTZ
	}
	if name == "" {
		return nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		c.d.logs.Warnf("clock", "", "timezone %q unavailable (%v); using the device timezone", name, err)
		if c.deviceTZ != "" && c.deviceTZ != name {
			if device, derr := time.LoadLocation(c.deviceTZ); derr == nil {
				return device
			}
		}
		return nil
	}
	return loc
}

func (c *builtinClock) loop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.d.ctx.Done():
			return
		case <-ticker.C:
			c.render()
		}
	}
}

func clockFrame(now time.Time, blink bool) ([]uint16, bool) {
	hhmm := now.Format("1504")
	cells := make([]uint16, 0, len(hhmm))
	for _, r := range hhmm {
		cells = append(cells, clockDigits[r])
	}
	colon := true
	if blink {
		colon = now.Second()%2 == 0
	}
	return cells, colon
}

func (c *builtinClock) render() {
	c.mu.Lock()
	loc := c.loc
	blink := c.blink
	now := c.now
	c.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	at := now()
	if loc != nil {
		at = at.In(loc)
	}
	cells, colon := clockFrame(at, blink)
	if err := c.d.pres.Update(c.pres, presentations.SegmentFrame(cells, colon)); err != nil &&
		c.d.ctx.Err() == nil && !c.pres.Closed() {
		c.d.logs.Warnf("clock", "", "frame: %v", err)
	}
}

// --- internal ring ----------------------------------------------------------

type builtinRing struct {
	d    *Daemon
	pres *presentations.Presentation

	mu        sync.Mutex
	micActive bool
	muted     bool
	mode      string
	pulse     *presets.Params
	pulseSeq  uint64
	applied   string

	ambientParams presets.Params
	micParams     presets.Params
	spinnerParams presets.Params
	rainbowParams presets.Params
}

func (r *builtinRing) start() {
	r.ambientParams = mustParams("system.ring.breathe", presets.Params{
		"period_s": 4.0, "color": rgb(0, 200, 120),
	})
	r.micParams = mustParams("system.ring.pulse", presets.Params{
		"speed": 0.9, "color": rgb(0, 136, 255), "min_intensity": 0.08,
	})
	r.spinnerParams = mustParams("system.ring.spinner", presets.Params{
		"speed": 0.8, "color": rgb(0, 136, 255), "intensity": 1.0, "tail": 5,
	})
	r.rainbowParams = mustParams("system.ring.rainbow", presets.Params{
		"speed": 0.3, "saturation": 1.0,
	})
	r.loadSettings()
	r.forceRefresh()
	r.update()
}

func (r *builtinRing) forceRefresh() {
	r.mu.Lock()
	content, sig := r.contentLocked()
	r.applied = sig
	r.mu.Unlock()
	r.write(content)
}

func (r *builtinRing) loadSettings() {
	snap, ch, cancel, err := r.d.appCfg.WatchApp(builtinRingSettingsID)
	if err != nil {
		r.d.logs.Warnf("ring", "", "settings unavailable: %v", err)
		return
	}
	r.applySettings(snap.Values)
	go func() {
		defer cancel()
		for {
			select {
			case <-r.d.ctx.Done():
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
	mode := r.mode
	r.mu.Unlock()
	for _, v := range values {
		if v.Key != "mode" {
			continue
		}
		var s string
		if json.Unmarshal([]byte(v.Value), &s) != nil {
			continue
		}
		switch s {
		case "ambient", "spinner", "rainbow":
			mode = s
		default:
			mode = "ambient"
		}
	}
	if mode == "" {
		mode = "ambient"
	}
	r.mu.Lock()
	changed := mode != r.mode
	r.mode = mode
	var (
		content presentations.Content
		sig     string
	)
	if changed {
		content, sig = r.contentLocked()
		r.applied = sig
	}
	r.mu.Unlock()
	if changed {
		r.write(content)
	}
}

// update reconciles the ring baseline with the microphone mute state and
// active capture streams. It is called by the output loop and lifecycle
// events, so an application start/stop restores the correct internal state.
func (r *builtinRing) update() {
	if r == nil {
		return
	}
	muted := r.d.user.Get().Values.MicrophoneMuted
	micActive := false
	if !muted {
		for _, s := range r.d.audio.ActiveStreams() {
			if s.Input && s.AppID != "" && s.AppID != routing.SystemAppID {
				micActive = true
				break
			}
		}
	}
	r.mu.Lock()
	changed := r.micActive != micActive || r.muted != muted
	r.micActive, r.muted = micActive, muted
	var (
		content presentations.Content
		sig     string
		write   bool
	)
	if changed {
		content, sig = r.contentLocked()
		write = sig != r.applied
		if write {
			r.applied = sig
		}
	}
	r.mu.Unlock()
	if write {
		r.write(content)
	}
}

func (r *builtinRing) contentLocked() (presentations.Content, string) {
	if r.pulse != nil {
		return presentations.RingEffect("system.ring.pulse", *r.pulse), fmt.Sprintf("pulse:%d", r.pulseSeq)
	}
	if r.muted {
		return presentations.Blank(), "blank"
	}
	if r.micActive {
		return presentations.RingEffect("system.ring.pulse", r.micParams), "mic"
	}
	switch r.mode {
	case "spinner":
		return presentations.RingEffect("system.ring.spinner", r.spinnerParams), "mode:spinner"
	case "rainbow":
		return presentations.RingEffect("system.ring.rainbow", r.rainbowParams), "mode:rainbow"
	default:
		return presentations.RingEffect("system.ring.breathe", r.ambientParams), "mode:ambient"
	}
}

// setPulse applies the transient Bluetooth connection pulse without opening a
// second presentation. The presentation is captured by the caller so it wins
// routing over an external baseline.
func (r *builtinRing) setPulse(params presets.Params) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.pulse = &params
	r.pulseSeq++
	content, sig := r.contentLocked()
	r.applied = sig
	r.mu.Unlock()
	r.write(content)
}

func (r *builtinRing) clearPulse() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.pulse = nil
	content, sig := r.contentLocked()
	r.applied = sig
	r.mu.Unlock()
	r.write(content)
}

func (r *builtinRing) write(content presentations.Content) {
	if err := r.d.pres.Update(r.pres, content); err != nil &&
		r.d.ctx.Err() == nil && !r.pres.Closed() {
		r.d.logs.Warnf("ring", "", "baseline: %v", err)
	}
}

func mustParams(id string, params presets.Params) presets.Params {
	out, err := presets.ValidateParams(mustPreset(id), params)
	if err != nil {
		panic("built-in preset " + id + ": " + err.Error())
	}
	return out
}

// --- legacy state migration -------------------------------------------------

// migrateLegacyState removes persisted choices that referred to the removed
// sample applications and carries the clock's typed settings across to the
// internal namespace. It runs once per boot before defaults are seeded, so a
// migrated empty default does not get replaced by a stale provider hint.
func (d *Daemon) migrateLegacyState() {
	d.migrateLegacyDefaults()
	d.migrateLegacyClockSettings()
}

func (d *Daemon) migrateLegacyDefaults() {
	snap := d.user.Get()
	patch := snap.Values
	changed := false
	assistantChanged := false
	for resourceID, appID := range patch.DefaultAppByResource {
		switch appID {
		case legacyClockAppID, legacyRingAppID, legacyAssistantAppID, legacyMediaAppID:
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
	d.logs.Infof("daemon", "", "migrated legacy provider defaults to the built-in baseline")
}

func (d *Daemon) migrateLegacyClockSettings() {
	root := filepath.Join(d.cfg.DataDir, "app-settings")
	src := filepath.Join(root, legacyClockAppID, "settings.json")
	dst := filepath.Join(root, builtinClockSettingsID, "settings.json")
	if _, err := os.Stat(dst); err == nil {
		return
	} else if !os.IsNotExist(err) {
		d.logs.Warnf("clock", "", "settings migration: %v", err)
		return
	}
	data, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		d.logs.Warnf("clock", "", "settings migration: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		d.logs.Warnf("clock", "", "settings migration: %v", err)
		return
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		d.logs.Warnf("clock", "", "settings migration: %v", err)
		return
	}
	d.logs.Infof("clock", "", "migrated legacy clock settings")
}

// isBuiltinSettingsID reports the reserved settings namespaces owned by the
// internal providers rather than by a supervised application.
func isBuiltinSettingsID(appID string) bool {
	return appID == builtinClockSettingsID || appID == builtinRingSettingsID
}

// builtinManifestSchema exposes the internal providers' typed settings through
// the same app-configuration surface as installed applications. The providers
// are never registered or listed; only their settings namespaces exist.
func builtinManifestSchema(appID string) (*settings.ManifestSchema, bool) {
	switch appID {
	case builtinClockSettingsID:
		return &settings.ManifestSchema{
			AppID: appID,
			Settings: []settings.SchemaSetting{
				{Key: "blink", Type: "boolean", DefaultJSON: "true"},
				{Key: "timezone", Type: "text", DefaultJSON: `""`},
			},
		}, true
	case builtinRingSettingsID:
		return &settings.ManifestSchema{
			AppID: appID,
			Settings: []settings.SchemaSetting{
				{Key: "mode", Type: "select", DefaultJSON: `"ambient"`},
			},
		}, true
	default:
		return nil, false
	}
}

// builtinConfigurationSchema describes the internal providers' settings to the
// same clients that enumerate installed applications. The provider itself has
// no application record, action list, or launch lifecycle.
func builtinConfigurationSchema(appID string) (*frameworkv1.AppConfigurationSchema, bool) {
	schema, ok := builtinManifestSchema(appID)
	if !ok {
		return nil, false
	}
	out := &frameworkv1.AppConfigurationSchema{AppId: appID}
	for _, def := range schema.Settings {
		entry := &frameworkv1.AppSettingDefinition{
			Key: def.Key, Type: settingTypeProto(def.Type), DefaultValueJson: def.DefaultJSON,
		}
		switch {
		case appID == builtinClockSettingsID && def.Key == "blink":
			entry.Label = "Blink colon"
		case appID == builtinClockSettingsID && def.Key == "timezone":
			entry.Label = "Timezone"
			entry.Description = "IANA zone name. Empty follows the device timezone."
		case appID == builtinRingSettingsID && def.Key == "mode":
			entry.Label = "Ring mode"
			entry.Description = "Default effect while no application or system indication owns the ring."
			for _, mode := range []struct{ value, label string }{
				{"ambient", "Ambient"}, {"spinner", "Spinner"}, {"rainbow", "Rainbow"},
			} {
				entry.Choices = append(entry.Choices, &frameworkv1.SettingChoice{Value: mode.value, Label: mode.label})
			}
		}
		out.Settings = append(out.Settings, entry)
	}
	return out, true
}
