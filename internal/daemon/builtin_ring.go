package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	ringv1 "capsulad/gen/go/capsula/contracts/ring/v1"
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
	ctx     context.Context
	cancel  context.CancelFunc

	client *builtinAppClient
	pres   *appPresentation

	effect     string
	palette    string
	brightness float64
	speed      float64

	applied string
}

func newBuiltinRing(d *Daemon, ringID string) *builtinRing {
	return &builtinRing{
		d: d, ringID: ringID,
		effect: "breathe", palette: "aurora", brightness: 1, speed: 1,
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
	r.ctx, r.cancel = context.WithCancel(r.d.ctx)
	r.applied = ""
	r.client = newBuiltinAppClient(r.d, ringAppID)
	pres, err := r.client.OpenPresentation(r.ctx, r.ringID)
	if err != nil {
		r.d.logs.Errorf(ringAppID, "", "open baseline presentation: %v", err)
	} else {
		r.pres = pres
	}
	ctx := r.ctx
	r.mu.Unlock()

	r.loadSettings(ctx)
	r.reassert()
}

func (r *builtinRing) stop() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	r.running = false
	if r.cancel != nil {
		r.cancel()
	}
	pres := r.pres
	client := r.client
	r.pres = nil
	r.client = nil
	r.applied = ""
	r.mu.Unlock()
	if pres != nil {
		pres.Close()
	}
	if client != nil {
		client.Close()
	}
	if r.cancel != nil {
		r.cancel()
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
	r.mu.Unlock()
	r.reassert()
}

func (r *builtinRing) reassert() {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return
	}
	id, params, sig := r.contentLocked()
	if sig == r.applied {
		r.mu.Unlock()
		return
	}
	r.applied = sig
	pres := r.pres
	r.mu.Unlock()
	if pres == nil {
		return
	}
	if err := pres.UpdateFrame(ringEffectPresentationFrame(id, params)); err != nil && r.d.ctx.Err() == nil {
		r.d.logs.Debugf(ringAppID, "", "baseline: %v", err)
	}
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
	}
	r.mu.Unlock()
	payload, err := encodeProtoResponse(out)
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
