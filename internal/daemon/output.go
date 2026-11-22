package daemon

import (
	"slices"
	"time"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/settings"
	"capsulad/internal/uichanges"
	"capsulad/internal/wakeword"
)

// applyRouting reacts to capture/selection changes. Visibility is owned by the
// router; this method handles touch ownership cancellation, diagnostics and
// the internal clock/ring baseline state.
func (d *Daemon) applyRouting(resourceID string) {
	sel := d.router.Current(resourceID)
	d.selMu.Lock()
	prev := d.lastSel[resourceID]
	signature := sel.SelectedAppID + "\x00" + sel.SelectedPresentation + "\x00" + sel.SelectedCaptureID
	d.lastSel[resourceID] = signature
	d.selMu.Unlock()
	if prev != signature {
		d.logs.Infof("routing", sel.SelectedAppID,
			"%s selected app=%s presentation=%s reason=%s capture=%s",
			resourceID, emptyDash(sel.SelectedAppID), emptyDash(sel.SelectedPresentation), sel.Reason, emptyDash(sel.SelectedCaptureID))
	}
	if resourceID == "touch.panel.main" {
		d.input.OnRoutingChanged()
	}
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

type lastWrite struct {
	cells        []uint16
	pixels       []backend.RGB
	blankCells   []uint16
	blankPixels  []backend.RGB
	colonTop     bool
	colonBottom  bool
	brightness   float64
	presentation string
	capture      string
	valid        bool
	errAt        time.Time
	traceAt      time.Time
}

// outputLoop renders the selected presentation into the physical backend.
// Effects are executed here so apps do not need to stream animation frames.
func (d *Daemon) outputLoop() {
	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	// Seed the gauge with the loop's own start so a slow daemon bootstrap does
	// not read as a hung runtime before the first tick.
	d.live.beat()
	last := map[string]*lastWrite{}
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
		// The hardware watchdog reads this as the daemon's liveness: the output
		// loop is the physical writer, so a step that stops completing is
		// exactly the failure the timer exists to end.
		d.live.beat()
		d.updateSystemRing()
		for _, info := range d.resources {
			var (
				key  string
				seg  backend.SegmentDevice
				ring backend.RingDevice
			)
			switch info.Kind {
			case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
				seg = d.dev.Segment()
				key = info.ResourceId
			case frameworkv1.ResourceKind_RESOURCE_LED_RING:
				ring = d.dev.Ring()
				key = info.ResourceId
			default:
				continue
			}
			if seg == nil && ring == nil {
				continue
			}
			b, present := d.effectiveBrightness(key)
			if !present {
				b = 1 // missing entries default to full brightness
			}
			sel := d.router.Current(info.ResourceId)
			p, _ := d.pres.Get(sel.SelectedPresentation)
			lw := last[key]
			if lw == nil {
				lw = &lastWrite{}
				last[key] = lw
			}
			ownershipChanged := !lw.valid || lw.presentation != sel.SelectedPresentation || lw.capture != sel.SelectedCaptureID
			brightnessChanged := !lw.valid || lw.brightness != b
			if seg != nil {
				var cells []uint16
				var colonTop, colonBottom, rendered bool
				if p != nil {
					cells, colonTop, colonBottom, rendered = p.RenderSegment(seg.Caps())
				}
				if !rendered {
					if lw.blankCells == nil {
						lw.blankCells = make([]uint16, seg.Caps().DigitCount)
					}
					cells, colonTop, colonBottom = lw.blankCells, false, false
				}
				if !ownershipChanged && !brightnessChanged &&
					colonTop == lw.colonTop && colonBottom == lw.colonBottom &&
					slices.Equal(cells, lw.cells) {
					continue
				}
				if err := seg.WriteSegment(backend.SegmentFrame{
					Cells: cells, Colon: colonTop && colonBottom,
					ColonTop: colonTop, ColonBottom: colonBottom, Brightness: b,
				}); err != nil {
					d.logWriteError(lw, info.ResourceId, err)
					continue
				}
				lw.cells, lw.colonTop, lw.colonBottom = cells, colonTop, colonBottom
			}
			if ring != nil {
				var pixels []backend.RGB
				var rendered bool
				if p != nil {
					pixels, rendered = p.RenderRing(ring.Caps(), d.clock.NowNS())
				}
				if !rendered {
					if lw.blankPixels == nil {
						lw.blankPixels = make([]backend.RGB, ring.Caps().PixelCount)
					}
					pixels = lw.blankPixels
				}
				if !ownershipChanged && !brightnessChanged && slices.Equal(pixels, lw.pixels) {
					continue
				}
				if err := ring.WriteRing(backend.RingFrame{Pixels: pixels, Brightness: b}); err != nil {
					d.logWriteError(lw, info.ResourceId, err)
					continue
				}
				lw.pixels = pixels
			}
			lw.valid = true
			lw.brightness = b
			lw.presentation, lw.capture = sel.SelectedPresentation, sel.SelectedCaptureID
			// Ownership and brightness transitions are always recorded. Opt-in
			// animation samples are bounded to four records/second/resource;
			// the normal rotating log remains bounded on the device.
			if ownershipChanged || brightnessChanged || (d.cfg.OutputTrace && time.Since(lw.traceAt) >= 250*time.Millisecond) {
				lw.traceAt = time.Now()
				if ring != nil {
					d.logs.Infof("output", sel.SelectedAppID,
						"%s wrote presentation=%s capture=%s brightness=%.4f pixels=%v",
						key, emptyDash(sel.SelectedPresentation), emptyDash(sel.SelectedCaptureID), b, lw.pixels)
				} else {
					d.logs.Infof("output", sel.SelectedAppID,
						"%s wrote presentation=%s capture=%s brightness=%.4f cells=%v colon_top=%v colon_bottom=%v",
						key, emptyDash(sel.SelectedPresentation), emptyDash(sel.SelectedCaptureID), b, lw.cells, lw.colonTop, lw.colonBottom)
				}
			}
		}
	}
}

func (d *Daemon) logWriteError(lw *lastWrite, resourceID string, err error) {
	if time.Since(lw.errAt) < 5*time.Second {
		return
	}
	lw.errAt = time.Now()
	d.logs.Warnf("output", "", "write %s failed: %v", resourceID, err)
}

// indicationActive implements the microphone watchdog's visible-indication
// check. Only a presentation actually selected on a ring/segment resource,
// visible at non-zero brightness, counts.
func (d *Daemon) indicationActive(appID string) bool {
	if appID == "" {
		return false
	}
	for _, info := range d.resources {
		if info.Kind != frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY && info.Kind != frameworkv1.ResourceKind_RESOURCE_LED_RING {
			continue
		}
		if b, present := d.effectiveBrightness(info.ResourceId); present && b <= 0 {
			continue
		}
		sel := d.router.Current(info.ResourceId)
		if sel.SelectedAppID != appID || sel.SelectedPresentation == "" {
			continue
		}
		p, ok := d.pres.Get(sel.SelectedPresentation)
		if !ok || !p.Ready() || !p.Visible() {
			continue
		}
		kind := p.ContentKind()
		if kind == presentations.ContentBlank {
			continue
		}
		now := d.clock.NowNS()
		if kind != presentations.ContentRingEffect && now-p.LastUpdateNS() >= uint64(2*time.Second) {
			continue
		}
		if info.Kind == frameworkv1.ResourceKind_RESOURCE_LED_RING && d.dev.Ring() != nil {
			// Check a second phase for effects so the dark instant of a
			// pulse is not mistaken for a permanently invisible indicator.
			for _, offset := range [...]uint64{0, uint64(137 * time.Millisecond)} {
				pixels, rendered := p.RenderRing(d.dev.Ring().Caps(), now+offset)
				if rendered {
					for _, pixel := range pixels {
						if pixel != (backend.RGB{}) {
							return true
						}
					}
				}
				if kind != presentations.ContentRingEffect {
					break
				}
			}
		} else if d.dev.Segment() != nil {
			cells, colonTop, colonBottom, rendered := p.RenderSegment(d.dev.Segment().Caps())
			if rendered {
				if colonTop || colonBottom {
					return true
				}
				for _, cell := range cells {
					if cell != 0 {
						return true
					}
				}
			}
		}
	}
	return false
}

func (d *Daemon) touchState() (captureID, appID string, mode routing.TouchMode) {
	if _, ok := d.resources["touch.panel.main"]; !ok {
		return "", "", routing.TouchNone
	}
	sel, _ := d.router.State("touch.panel.main")
	return sel.SelectedCaptureID, sel.SelectedAppID, sel.TouchMode
}

func (d *Daemon) triggerWakeword(origin, modelID string) {
	d.wake.Trigger(origin, modelID)
}

func (d *Daemon) assistantAppID() string {
	sel := d.router.Current("role.voice_assistant")
	if sel.SelectedAppID != "" {
		return sel.SelectedAppID
	}
	return d.user.Get().Values.DefaultAssistantAppID
}

func (d *Daemon) deliverActivation(act wakeword.Activation) {
	// A real detector activation advanced the bounded detection history even
	// when there is no ready app to receive it; refresh the browser view.
	if act.Origin == "wakeword" {
		d.publishUI(uichanges.Wakeword)
	}
	// A word bound to an app is delivered to that app only. Falling back to the
	// selected assistant here would turn a misconfigured binding into a
	// second-best reroute the user did not ask for, so an unready target is an
	// explicit drop with its own reason.
	appID, reason := wakewordDeliveryTarget(act.AppID, d.assistantAppID(), func(appID string) bool {
		_, ready := d.bus.ReadyApp(appID)
		return ready
	})
	if appID == "" {
		switch reason {
		case dropNoSelection:
			d.logs.Warnf("wakeword", "", "activation=%s dropped: no selected assistant", act.ID)
		case dropBoundUnready:
			d.logs.Warnf("wakeword", act.AppID, "activation=%s dropped: word %s is bound to %s, which is not ready (not rerouted)", act.ID, act.ModelID, act.AppID)
		default:
			d.logs.Warnf("wakeword", "", "activation=%s dropped: selected assistant is not ready", act.ID)
		}
		return
	}
	msg := &frameworkv1.AssistantActivation{
		ActivationId: act.ID, Origin: frameworkv1.ActivationOrigin_ACTIVATION_WAKEWORD,
		MatchedModelId: act.ModelID, MonotonicTimeNs: act.TimeNS, ExpiresMonotonicNs: act.ExpiresNS,
		AppId: appID,
	}
	if wakeword.ManualOrigin(act.Origin) {
		msg.Origin = frameworkv1.ActivationOrigin_ACTIVATION_TOUCH
		// A manual trigger uses a placeholder model id internally (the
		// wake-word manager needs a non-empty identity for its own bookkeeping).
		// It must not reach an app as a matched model: an app that treats a
		// model id as wake-word evidence would then claim it heard a keyword on
		// a key press, which is what made a touch turn fail the service's
		// keyword check.
		msg.MatchedModelId = ""
		msg.HoldToTalk = d.user.Get().Values.TouchHoldMode == settings.HoldToTalk
	}
	// Record the grant before delivery so an app that opens the microphone
	// immediately after receiving the activation still gets its pre-roll. Only
	// the app that received this activation can attach it; it is one-shot and
	// expires with the activation's delivery window.
	d.recordActivationGrant(act.ID, appID, act.ExpiresNS)
	if err := d.bus.SendApp(appID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_Activation{Activation: msg}}); err != nil {
		d.consumeActivationGrant(appID, act.ID)
		d.logs.Warnf("wakeword", appID, "activation=%s delivery failed: %v", act.ID, err)
		return
	}
	if wakeword.ManualOrigin(act.Origin) {
		d.touchActMu.Lock()
		d.touchActivation, d.touchAssistant = act.ID, appID
		d.touchActMu.Unlock()
	}
	d.logs.Infof("wakeword", appID, "activation=%s delivered", act.ID)
}

// Reasons an activation reaches no app. A bound word never falls back to the
// selected assistant: that would reroute a turn the user bound to one app.
const (
	dropNoSelection     = "no-selected-assistant"
	dropBoundUnready    = "bound-app-not-ready"
	dropSelectedUnready = "selected-assistant-not-ready"
)

// wakewordDeliveryTarget resolves the app an activation must reach. A bound app
// wins; an empty binding means the selected assistant. It never substitutes one
// app for another, so the caller can report an explicit drop reason.
func wakewordDeliveryTarget(boundApp, selectedApp string, ready func(string) bool) (string, string) {
	appID := boundApp
	reason := dropBoundUnready
	if appID == "" {
		appID = selectedApp
		reason = dropSelectedUnready
	}
	if appID == "" {
		return "", dropNoSelection
	}
	if !ready(appID) {
		return "", reason
	}
	return appID, ""
}

type activationGrant struct {
	appID     string
	expiresNS uint64
}

// recordActivationGrant bounds the table to the live activation window so a
// stream of undelivered turns cannot grow memory without limit.
func (d *Daemon) recordActivationGrant(activationID, appID string, expiresNS uint64) {
	if activationID == "" {
		return
	}
	now := d.clock.NowNS()
	d.actMu.Lock()
	defer d.actMu.Unlock()
	if d.activations == nil {
		d.activations = map[string]activationGrant{}
	}
	for id, grant := range d.activations {
		if now > grant.expiresNS {
			delete(d.activations, id)
		}
	}
	d.activations[activationID] = activationGrant{appID: appID, expiresNS: expiresNS}
}

// activationGrantAllowed reports whether activationID was issued to appID and
// is still live, without consuming it. Capture startup may yet fail (for
// example when the visible indication is absent), so the grant is consumed
// only after a successful open.
func (d *Daemon) activationGrantAllowed(appID, activationID string) bool {
	if activationID == "" || appID == "" {
		return false
	}
	now := d.clock.NowNS()
	d.actMu.Lock()
	defer d.actMu.Unlock()
	grant, ok := d.activations[activationID]
	return ok && grant.appID == appID && now <= grant.expiresNS
}

// consumeActivationGrant makes an activation grant one-shot: the pre-roll
// buffer cannot be replayed by reusing the ID.
func (d *Daemon) consumeActivationGrant(appID, activationID string) bool {
	if activationID == "" || appID == "" {
		return false
	}
	now := d.clock.NowNS()
	d.actMu.Lock()
	defer d.actMu.Unlock()
	grant, ok := d.activations[activationID]
	if !ok {
		return false
	}
	delete(d.activations, activationID)
	return grant.appID == appID && now <= grant.expiresNS
}

func (d *Daemon) sendTouchRelease() {
	d.touchActMu.Lock()
	actID, appID := d.touchActivation, d.touchAssistant
	d.touchActivation, d.touchAssistant = "", ""
	d.touchActMu.Unlock()
	if appID == "" || actID == "" {
		return
	}
	if err := d.bus.SendApp(appID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_TouchReleased{
		TouchReleased: &frameworkv1.AssistantInputReleased{ActivationId: actID},
	}}); err != nil {
		d.logs.Warnf("input", appID, "touch release delivery failed: %v", err)
	}
}
