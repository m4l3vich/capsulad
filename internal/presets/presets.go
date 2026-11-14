// Package presets implements the built-in, backend-adapted presentation
// catalog: LED ring effects and segment display faces.
//
// Presets are content for an existing presentation. They are not services,
// resources or captures. Preset IDs and parameter schemas are discoverable;
// unsupported IDs or arguments return an explicit error and never silently
// substitute another preset.
package presets

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"capsulad/internal/backend"
)

type Kind string

const (
	KindRingEffect  Kind = "PRESET_RING_EFFECT"
	KindSegmentFace Kind = "PRESET_SEGMENT_FACE"
)

type ParamDef struct {
	Name        string
	Description string
	Type        string // text, integer, number, boolean, rgb
	Default     any
	HasMin      bool
	Min         float64
	HasMax      bool
	Max         float64
}

type Info struct {
	ID          string
	Kind        Kind
	DisplayName string
	Description string
	Params      []ParamDef
}

// Params are validated, typed preset arguments.
type Params map[string]any

var catalog = []Info{
	{
		ID: "system.ring.spinner", Kind: KindRingEffect, DisplayName: "Spinner",
		Description: "A rotating head with a soft fading tail.",
		Params: []ParamDef{
			{Name: "speed", Description: "Revolutions per second.", Type: "number", Default: 0.5, HasMin: true, Min: 0, HasMax: true, Max: 5},
			{Name: "color", Description: "Head color.", Type: "rgb", Default: backend.RGB{R: 0, G: 136, B: 255}},
			{Name: "intensity", Description: "Overall brightness.", Type: "number", Default: 1.0, HasMin: true, Min: 0, HasMax: true, Max: 1},
			{Name: "tail", Description: "Number of trailing pixels.", Type: "integer", Default: int64(5), HasMin: true, Min: 0, HasMax: true, Max: 12},
		},
	},
	{
		ID: "system.ring.pulse", Kind: KindRingEffect, DisplayName: "Pulse",
		Description: "The whole ring pulses together.",
		Params: []ParamDef{
			{Name: "speed", Description: "Pulses per second.", Type: "number", Default: 0.8, HasMin: true, Min: 0.05, HasMax: true, Max: 5},
			{Name: "color", Description: "Pulse color.", Type: "rgb", Default: backend.RGB{R: 0, G: 136, B: 255}},
			{Name: "min_intensity", Description: "Lowest brightness between pulses.", Type: "number", Default: 0.05, HasMin: true, Min: 0, HasMax: true, Max: 1},
		},
	},
	{
		ID: "system.ring.indicator", Kind: KindRingEffect, DisplayName: "Indicator",
		Description: "Lights one contiguous run of pixels, for status indications.",
		Params: []ParamDef{
			{Name: "color", Description: "Indicator color.", Type: "rgb", Default: backend.RGB{R: 255, G: 0, B: 0}},
			{Name: "start", Description: "First pixel index.", Type: "integer", Default: int64(0), HasMin: true, Min: 0, HasMax: true, Max: 11},
			{Name: "count", Description: "How many pixels to light.", Type: "integer", Default: int64(3), HasMin: true, Min: 0, HasMax: true, Max: 12},
			{Name: "intensity", Description: "Overall brightness.", Type: "number", Default: 1.0, HasMin: true, Min: 0, HasMax: true, Max: 1},
			{Name: "fade_in", Description: "Seconds to fade in from dark.", Type: "number", Default: 0.25, HasMin: true, Min: 0, HasMax: true, Max: 2},
		},
	},
	{
		ID: "system.ring.breathe", Kind: KindRingEffect, DisplayName: "Breathe",
		Description: "A slow sine breathing effect.",
		Params: []ParamDef{
			{Name: "period_s", Description: "Seconds per breath.", Type: "number", Default: 4.0, HasMin: true, Min: 0.2, HasMax: true, Max: 60},
			{Name: "color", Description: "Breathing color.", Type: "rgb", Default: backend.RGB{R: 0, G: 200, B: 120}},
		},
	},
	{
		ID: "system.ring.static", Kind: KindRingEffect, DisplayName: "Static",
		Description: "A solid, non-animated colour. The palette's first colour is used.",
		Params: []ParamDef{
			{Name: "color", Description: "Solid colour.", Type: "rgb", Default: backend.RGB{R: 0, G: 220, B: 180}},
			{Name: "intensity", Description: "Overall brightness.", Type: "number", Default: 1.0, HasMin: true, Min: 0, HasMax: true, Max: 1},
		},
	},
	{
		ID: "system.ring.rainbow", Kind: KindRingEffect, DisplayName: "Rainbow",
		Description: "Rotates the palette colours around the ring instead of inventing hues.",
		Params: []ParamDef{
			{Name: "speed", Description: "Palette rotations per second.", Type: "number", Default: 0.2, HasMin: true, Min: 0, HasMax: true, Max: 3},
			{Name: "color", Description: "Palette colour at 0%.", Type: "rgb", Default: backend.RGB{R: 0, G: 220, B: 180}},
			{Name: "color_b", Description: "Palette colour at 1/3.", Type: "rgb", Default: backend.RGB{R: 40, G: 175, B: 217}},
			{Name: "color_c", Description: "Palette colour at 2/3.", Type: "rgb", Default: backend.RGB{R: 80, G: 130, B: 255}},
			{Name: "brightness", Description: "Overall brightness.", Type: "number", Default: 1.0, HasMin: true, Min: 0, HasMax: true, Max: 1},
		},
	},
	{
		ID: "system.ring.comet", Kind: KindRingEffect, DisplayName: "Comet",
		Description: "A bright head with a long tail that fades into a second color.",
		Params: []ParamDef{
			{Name: "speed", Description: "Revolutions per second.", Type: "number", Default: 0.35, HasMin: true, Min: 0, HasMax: true, Max: 3},
			{Name: "color", Description: "Head color.", Type: "rgb", Default: backend.RGB{R: 0, G: 220, B: 255}},
			{Name: "tail_color", Description: "Color the tail fades toward.", Type: "rgb", Default: backend.RGB{R: 0, G: 64, B: 180}},
			{Name: "tail", Description: "Number of trailing pixels.", Type: "integer", Default: int64(8), HasMin: true, Min: 0, HasMax: true, Max: 11},
			{Name: "intensity", Description: "Overall brightness.", Type: "number", Default: 1.0, HasMin: true, Min: 0, HasMax: true, Max: 1},
		},
	},
	{
		ID: "system.ring.sparkle", Kind: KindRingEffect, DisplayName: "Sparkle",
		Description: "Deterministic twinkles in two colors; no random flicker between frames.",
		Params: []ParamDef{
			{Name: "speed", Description: "Twinkle cycles per second.", Type: "number", Default: 0.9, HasMin: true, Min: 0.02, HasMax: true, Max: 5},
			{Name: "color", Description: "Primary twinkle color.", Type: "rgb", Default: backend.RGB{R: 210, G: 240, B: 255}},
			{Name: "color_b", Description: "Secondary twinkle color.", Type: "rgb", Default: backend.RGB{R: 120, G: 80, B: 255}},
			{Name: "density", Description: "Fraction of pixels that can twinkle.", Type: "number", Default: 0.6, HasMin: true, Min: 0, HasMax: true, Max: 1},
			{Name: "intensity", Description: "Overall brightness.", Type: "number", Default: 1.0, HasMin: true, Min: 0, HasMax: true, Max: 1},
		},
	},
	{
		ID: "system.display.face.neutral", Kind: KindSegmentFace, DisplayName: "Neutral",
		Description: "Dashes with an illuminated colon.",
		Params:      nil,
	},
	{
		ID: "system.display.face.sleepy", Kind: KindSegmentFace, DisplayName: "Sleepy",
		Description: "Only the outer cells show a dash.",
		Params:      nil,
	},
	{
		ID: "system.display.face.surprised", Kind: KindSegmentFace, DisplayName: "Surprised",
		Description: "All cells show a zero glyph.",
		Params:      nil,
	},
}

// Lookup returns a preset definition by ID.
func Lookup(id string) (*Info, bool) {
	for i := range catalog {
		if catalog[i].ID == id {
			return &catalog[i], true
		}
	}
	return nil, false
}

// Catalog returns a copy of all presets, optionally filtered by kind.
func Catalog(kind Kind, resourceID string) []Info {
	out := make([]Info, 0, len(catalog))
	for _, p := range catalog {
		if kind != "" && p.Kind != kind {
			continue
		}
		if resourceID != "" && !Supports(p, resourceID) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Supports reports whether this preset is meaningful for a resource. Built-in
// presets are framework-owned and resource-agnostic apart from their kind.
func Supports(p Info, resourceID string) bool {
	switch p.Kind {
	case KindRingEffect:
		return strings.HasPrefix(resourceID, "light.ring")
	case KindSegmentFace:
		return strings.HasPrefix(resourceID, "display.")
	}
	return false
}

// ValidateParams merges defaults with supplied arguments and checks types and
// ranges. Unknown arguments are rejected (no silent substitution).
func ValidateParams(info Info, in Params) (Params, error) {
	out := Params{}
	for _, def := range info.Params {
		out[def.Name] = def.Default
	}
	for k, v := range in {
		var def *ParamDef
		for i := range info.Params {
			if info.Params[i].Name == k {
				def = &info.Params[i]
				break
			}
		}
		if def == nil {
			return nil, fmt.Errorf("preset %s: unknown argument %q", info.ID, k)
		}
		if err := validate(*def, v); err != nil {
			return nil, fmt.Errorf("preset %s: %w", info.ID, err)
		}
		out[k] = v
	}
	for _, def := range info.Params {
		if def.Type == "rgb" {
			if _, ok := out[def.Name].(backend.RGB); !ok {
				return nil, fmt.Errorf("preset %s: argument %s must be an RGB color", info.ID, def.Name)
			}
		}
	}
	return out, nil
}

func validate(def ParamDef, v any) error {
	switch def.Type {
	case "text":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("argument %s must be text", def.Name)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("argument %s must be boolean", def.Name)
		}
	case "rgb":
		if _, ok := v.(backend.RGB); !ok {
			return fmt.Errorf("argument %s must be an RGB color", def.Name)
		}
	case "integer":
		n, ok := num(v)
		if !ok || n != math.Trunc(n) {
			return fmt.Errorf("argument %s must be an integer", def.Name)
		}
		return bounds(def, n)
	case "number":
		n, ok := num(v)
		if !ok {
			return fmt.Errorf("argument %s must be a number", def.Name)
		}
		return bounds(def, n)
	default:
		return fmt.Errorf("argument %s has unsupported type %s", def.Name, def.Type)
	}
	return nil
}

func bounds(def ParamDef, n float64) error {
	if def.HasMin && n < def.Min {
		return fmt.Errorf("argument %s is below minimum %v", def.Name, def.Min)
	}
	if def.HasMax && n > def.Max {
		return fmt.Errorf("argument %s is above maximum %v", def.Name, def.Max)
	}
	return nil
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// RingFrame renders a ring effect at time t (seconds since the effect was
// applied) for a given geometry.
func RingFrame(presetID string, params Params, pixels int, t float64) ([]backend.RGB, error) {
	info, ok := Lookup(presetID)
	if !ok || info.Kind != KindRingEffect {
		return nil, fmt.Errorf("unknown ring effect %q", presetID)
	}
	if pixels <= 0 {
		return nil, fmt.Errorf("ring has no pixels")
	}
	vals, err := ValidateParams(*info, params)
	if err != nil {
		return nil, err
	}
	out := make([]backend.RGB, pixels)
	switch presetID {
	case "system.ring.spinner":
		speed := paramFloat(vals, "speed")
		color := paramRGB(vals, "color")
		intensity := paramFloat(vals, "intensity")
		tail := int(paramFloat(vals, "tail"))
		head := int(math.Floor(t*speed*float64(pixels))) % pixels
		for i := range pixels {
			dist := ((head-i)%pixels + pixels) % pixels
			var level float64
			if dist == 0 {
				level = 1
			} else if tail > 0 && dist <= tail {
				level = (1 - float64(dist)/float64(tail+1)) * 0.8
			}
			out[i] = scale(color, intensity*level)
		}
	case "system.ring.pulse":
		speed := paramFloat(vals, "speed")
		color := paramRGB(vals, "color")
		floor := paramFloat(vals, "min_intensity")
		lvl := floor + (1-floor)*(0.5+0.5*math.Sin(2*math.Pi*speed*t-math.Pi/2))
		for i := range out {
			out[i] = scale(color, lvl)
		}
	case "system.ring.indicator":
		level := paramFloat(vals, "intensity")
		if fade := paramFloat(vals, "fade_in"); fade > 0 && t < fade {
			level *= t / fade
		}
		color := scale(paramRGB(vals, "color"), level)
		start := int(paramFloat(vals, "start"))
		count := int(paramFloat(vals, "count"))
		// The run is clamped to the ring, never wrapped: an indicator that
		// straddles index 11 would otherwise light two disconnected arcs.
		for i := range out {
			if i >= start && i < start+count {
				out[i] = color
			}
		}
	case "system.ring.breathe":
		period := paramFloat(vals, "period_s")
		if period <= 0 {
			period = 4
		}
		color := paramRGB(vals, "color")
		lvl := 0.5 - 0.5*math.Cos(2*math.Pi*t/period)
		for i := range out {
			out[i] = scale(color, lvl*0.9)
		}
	case "system.ring.static":
		color := scale(paramRGB(vals, "color"), paramFloat(vals, "intensity"))
		for i := range out {
			out[i] = color
		}
	case "system.ring.rainbow":
		speed := paramFloat(vals, "speed")
		level := paramFloat(vals, "brightness")
		stops := [3]backend.RGB{
			paramRGB(vals, "color"), paramRGB(vals, "color_b"), paramRGB(vals, "color_c"),
		}
		for i := range pixels {
			pos := math.Mod(t*speed+float64(i)/float64(pixels), 1)
			out[i] = scale(paletteAt(stops, pos), level)
		}
	case "system.ring.comet":
		speed := paramFloat(vals, "speed")
		headColor := paramRGB(vals, "color")
		tailColor := paramRGB(vals, "tail_color")
		intensity := paramFloat(vals, "intensity")
		tail := int(paramFloat(vals, "tail"))
		head := int(math.Floor(t*speed*float64(pixels))) % pixels
		if head < 0 {
			head += pixels
		}
		for i := range pixels {
			dist := ((head-i)%pixels + pixels) % pixels
			switch {
			case dist == 0:
				out[i] = scale(headColor, intensity)
			case tail > 0 && dist <= tail:
				fade := 1 - float64(dist)/float64(tail+1)
				out[i] = scale(lerpRGB(headColor, tailColor, float64(dist)/float64(tail+1)), intensity*fade)
			}
		}
	case "system.ring.sparkle":
		speed := paramFloat(vals, "speed")
		colorA := paramRGB(vals, "color")
		colorB := paramRGB(vals, "color_b")
		density := paramFloat(vals, "density")
		intensity := paramFloat(vals, "intensity")
		for i := range pixels {
			gate := hash01(i, 0xc2b2ae35)
			if gate > density {
				continue
			}
			rate := 0.55 + 0.9*hash01(i, 0x9e3779b9)
			phase := hash01(i, 0x85ebca6b)
			s := math.Sin(2 * math.Pi * (t*speed*rate + phase))
			if s <= 0 {
				continue
			}
			lvl := math.Pow(s, 6)
			color := colorA
			if hash01(i, 0x27d4eb2f) < 0.5 {
				color = colorB
			}
			out[i] = scale(color, intensity*lvl)
		}
	default:
		return nil, fmt.Errorf("unsupported ring effect %q", presetID)
	}
	return out, nil
}

// SegmentMask returns digit masks for a face at a segment geometry. Faces are
// mapped onto the available digit cells; extra cells stay dark. Unsupported
// faces fail rather than drawing a misleading approximation.
func SegmentMask(presetID string, caps backend.SegmentCaps) (cells []uint16, colon bool, err error) {
	info, ok := Lookup(presetID)
	if !ok || info.Kind != KindSegmentFace {
		return nil, false, fmt.Errorf("unknown segment face %q", presetID)
	}
	if caps.DigitCount <= 0 || caps.SegmentsPerDigit < 7 {
		return nil, false, fmt.Errorf("segment backend does not expose 7-segment digits")
	}
	const segG = uint16(1 << 6) // middle dash
	const zero = uint16(1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5)
	cells = make([]uint16, caps.DigitCount)
	switch presetID {
	case "system.display.face.neutral":
		for i := range cells {
			cells[i] = segG
		}
		colon = caps.SupportsColon
	case "system.display.face.sleepy":
		if caps.DigitCount > 0 {
			cells[0] = segG
		}
		if caps.DigitCount > 1 {
			cells[caps.DigitCount-1] = segG
		}
	case "system.display.face.surprised":
		for i := range cells {
			cells[i] = zero
		}
	default:
		return nil, false, fmt.Errorf("unsupported segment face %q", presetID)
	}
	return cells, colon, nil
}

func paramFloat(p Params, name string) float64 {
	if n, ok := num(p[name]); ok {
		return n
	}
	return 0
}

func paramRGB(p Params, name string) backend.RGB {
	if c, ok := p[name].(backend.RGB); ok {
		return c
	}
	return backend.RGB{}
}

func scale(c backend.RGB, lvl float64) backend.RGB {
	if lvl < 0 {
		lvl = 0
	}
	if lvl > 1 {
		lvl = 1
	}
	return backend.RGB{
		R: uint8(math.Round(float64(c.R) * lvl)),
		G: uint8(math.Round(float64(c.G) * lvl)),
		B: uint8(math.Round(float64(c.B) * lvl)),
	}
}

// paletteAt interpolates a closed 3-stop palette. Position wraps so the first
// colour returns after the last one, keeping the rotation seamless.
func paletteAt(stops [3]backend.RGB, pos float64) backend.RGB {
	pos = pos - math.Floor(pos)
	scaled := pos * 3
	seg := int(scaled)
	if seg > 2 {
		seg = 2
	}
	f := scaled - float64(seg)
	next := (seg + 1) % 3
	return lerpRGB(stops[seg], stops[next], f)
}

// lerpRGB blends two colors by t in [0,1].
func lerpRGB(a, b backend.RGB, t float64) backend.RGB {
	if t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	mix := func(x, y uint8) uint8 {
		return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t))
	}
	return backend.RGB{R: mix(a.R, b.R), G: mix(a.G, b.G), B: mix(a.B, b.B)}
}

// hash01 maps a pixel index and salt to a stable value in [0,1). Effects use
// it instead of a random source so a frame is reproducible and does not
// flicker between renders.
func hash01(index int, salt uint32) float64 {
	x := uint32(index)*0x9e3779b1 + salt
	x ^= x >> 16
	x *= 0x7feb352d
	x ^= x >> 15
	x *= 0x846ca68b
	x ^= x >> 16
	return float64(x>>8) / float64(1<<24)
}
