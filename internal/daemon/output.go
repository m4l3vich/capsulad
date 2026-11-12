package daemon

import (
	"fmt"
	"strings"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presentations"
	"capsulad/internal/routing"
	"capsulad/internal/wakeword"
)

// applyRouting reacts to capture/selection changes. Visibility is owned by the
// router; this method handles touch ownership cancellation, diagnostics and
// the internal clock/ring baseline state.
func (d *Daemon) applyRouting(resourceID string) {
	sel := d.router.Current(resourceID)
	d.selMu.Lock()
	prev := d.lastSel[resourceID]
	if prev != sel.SelectedAppID {
		d.lastSel[resourceID] = sel.SelectedAppID
	}
	d.selMu.Unlock()
	if prev != sel.SelectedAppID {
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
	segment string
	ring    string
	errAt   time.Time
}

// outputLoop renders the selected presentation into the physical backend.
// Effects are executed here so apps do not need to stream animation frames.
func (d *Daemon) outputLoop() {
	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	last := map[string]*lastWrite{}
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
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
			if sel.SelectedPresentation == "" {
				continue
			}
			p, ok := d.pres.Get(sel.SelectedPresentation)
			if !ok {
				continue
			}
			lw := last[key]
			if lw == nil {
				lw = &lastWrite{}
				last[key] = lw
			}
			if seg != nil {
				cells, colon, ok := p.RenderSegment(seg.Caps())
				if !ok {
					continue
				}
				sig := fmt.Sprintf("%v|%v|%.2f", cells, colon, b)
				if sig == lw.segment {
					continue
				}
				if err := seg.WriteSegment(backend.SegmentFrame{Cells: cells, Colon: colon, Brightness: b}); err != nil {
					d.logWriteError(lw, info.ResourceId, err)
					continue
				}
				lw.segment = sig
			}
			if ring != nil {
				pixels, ok := p.RenderRing(ring.Caps(), d.clock.NowNS())
				if !ok {
					continue
				}
				var sb strings.Builder
				for _, px := range pixels {
					fmt.Fprintf(&sb, "%02x%02x%02x", px.R, px.G, px.B)
				}
				sig := sb.String()
				if sig == lw.ring {
					continue
				}
				if err := ring.WriteRing(backend.RingFrame{Pixels: pixels, Brightness: b}); err != nil {
					d.logWriteError(lw, info.ResourceId, err)
					continue
				}
				lw.ring = sig
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
		switch p.ContentKind() {
		case presentations.ContentRingEffect:
			return true // continuously perceptible; framework animates it
		case presentations.ContentSegmentFrame, presentations.ContentSegmentFace, presentations.ContentRingFrame, presentations.ContentBlank:
			if d.clock.NowNS()-p.LastUpdateNS() < uint64(2*time.Second) {
				return true
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
	appID := d.assistantAppID()
	if appID == "" {
		d.logs.Warnf("wakeword", "", "activation=%s dropped: no selected assistant", act.ID)
		return
	}
	if _, ok := d.bus.ReadyApp(appID); !ok {
		d.logs.Warnf("wakeword", "", "activation=%s dropped: selected assistant %s is not ready", act.ID, appID)
		return
	}
	msg := &frameworkv1.AssistantActivation{
		ActivationId: act.ID, Origin: frameworkv1.ActivationOrigin_ACTIVATION_WAKEWORD,
		MatchedModelId: act.ModelID, MonotonicTimeNs: act.TimeNS, ExpiresMonotonicNs: act.ExpiresNS,
	}
	if act.Origin == "touch" {
		msg.Origin = frameworkv1.ActivationOrigin_ACTIVATION_TOUCH
	}
	if err := d.bus.SendApp(appID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_Activation{Activation: msg}}); err != nil {
		d.logs.Warnf("wakeword", appID, "activation=%s delivery failed: %v", act.ID, err)
		return
	}
	d.lastActMu.Lock()
	d.lastActivation = act.ID
	d.lastActMu.Unlock()
	d.logs.Infof("wakeword", appID, "activation=%s delivered", act.ID)
}

func (d *Daemon) sendTouchRelease() {
	appID := d.assistantAppID()
	d.lastActMu.Lock()
	actID := d.lastActivation
	d.lastActMu.Unlock()
	if appID == "" || actID == "" {
		return
	}
	if err := d.bus.SendApp(appID, &frameworkv1.AppInbound{Message: &frameworkv1.AppInbound_TouchReleased{
		TouchReleased: &frameworkv1.AssistantInputReleased{ActivationId: actID},
	}}); err != nil {
		d.logs.Warnf("input", appID, "touch release delivery failed: %v", err)
	}
}
