// Package ringpalette parses the canonical gradient string stored in the
// built-in ring's `palette` setting.
//
// Accepted forms:
//
//	linear-gradient([<angle>deg,] <stop> [, <stop> ...])
//	<hex>
//
// A stop is `#RGB`/`#RRGGBB` optionally followed by a percentage. The angle is
// 0-360 and defaults to 90. Positions must be nondecreasing; omitted positions
// are distributed between their neighbours, with the first defaulting to 0%
// and the last to 100%. At least one stop is required and at most eight.
//
// A bare hex colour is a one-stop gradient, i.e. a solid palette. This is the
// form used by the `static` ring effect, but a gradient palette is also
// accepted there and rendered as its first stop.
//
// The parser is deliberately small: no CSS variables, colour functions,
// repeating gradients or other CSS functions are accepted.
package ringpalette

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"capsulad/internal/backend"
)

// Stop is one colour at a normalized position.
type Stop struct {
	Position float64 // [0,1]
	Color    backend.RGB
}

// Gradient is a parsed palette. Stops are sorted ascending by position.
type Gradient struct {
	Angle float64
	Stops []Stop
}

const maxStops = 8

type rawStop struct {
	color  backend.RGB
	pos    float64
	hasPos bool
}

// Parse parses a canonical palette string.
func Parse(s string) (Gradient, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Gradient{}, fmt.Errorf("palette is empty")
	}
	if !strings.HasPrefix(text, "linear-gradient(") {
		c, err := parseHex(text)
		if err != nil {
			return Gradient{}, fmt.Errorf("palette must be linear-gradient(...) or #RRGGBB")
		}
		return Gradient{Angle: 90, Stops: []Stop{{Position: 0, Color: c}}}, nil
	}
	if !strings.HasSuffix(text, ")") {
		return Gradient{}, fmt.Errorf("linear-gradient is not closed")
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "linear-gradient("), ")")
	parts := strings.Split(inner, ",")
	angle := 90.0
	first := strings.TrimSpace(parts[0])
	if strings.HasSuffix(first, "deg") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(first, "deg"), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 360 {
			return Gradient{}, fmt.Errorf("gradient angle must be 0-360deg")
		}
		angle = n
		parts = parts[1:]
	}
	if len(parts) == 0 || len(parts) > maxStops {
		return Gradient{}, fmt.Errorf("gradient needs between 1 and %d stops", maxStops)
	}
	stops := make([]rawStop, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return Gradient{}, fmt.Errorf("gradient has an empty stop")
		}
		fields := strings.Fields(part)
		if len(fields) == 0 || len(fields) > 2 {
			return Gradient{}, fmt.Errorf("invalid gradient stop %q", part)
		}
		c, err := parseHex(fields[0])
		if err != nil {
			return Gradient{}, err
		}
		stop := rawStop{color: c}
		if len(fields) == 2 {
			pct := fields[1]
			if !strings.HasSuffix(pct, "%") {
				return Gradient{}, fmt.Errorf("stop position %q must be a percentage", pct)
			}
			n, err := strconv.ParseFloat(strings.TrimSuffix(pct, "%"), 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
				return Gradient{}, fmt.Errorf("stop position %q must be between 0%% and 100%%", pct)
			}
			stop.pos, stop.hasPos = n/100, true
		}
		stops = append(stops, stop)
	}
	fillPositions(stops)
	out := Gradient{Angle: angle, Stops: make([]Stop, 0, len(stops))}
	last := -1.0
	for _, s := range stops {
		if s.pos < last {
			return Gradient{}, fmt.Errorf("gradient stop positions must not decrease")
		}
		last = s.pos
		out.Stops = append(out.Stops, Stop{Position: s.pos, Color: s.color})
	}
	return out, nil
}

// fillPositions interpolates unspecified stop positions evenly between the
// surrounding specified positions.
func fillPositions(stops []rawStop) {
	n := len(stops)
	if n == 1 {
		stops[0].pos = 0
		return
	}
	if !stops[0].hasPos {
		stops[0].pos, stops[0].hasPos = 0, true
	}
	if !stops[n-1].hasPos {
		stops[n-1].pos, stops[n-1].hasPos = 1, true
	}
	for i := 0; i < n; {
		if stops[i].hasPos {
			i++
			continue
		}
		j := i
		for j < n && !stops[j].hasPos {
			j++
		}
		start, end := stops[i-1].pos, stops[j].pos
		span := float64(j - i + 1)
		for k := i; k < j; k++ {
			stops[k].pos = start + (end-start)*float64(k-i+1)/span
		}
		i = j
	}
}

func parseHex(s string) (backend.RGB, error) {
	if !strings.HasPrefix(s, "#") {
		return backend.RGB{}, fmt.Errorf("colour %q must start with #", s)
	}
	hex := strings.TrimPrefix(s, "#")
	switch len(hex) {
	case 3:
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	case 6:
	default:
		return backend.RGB{}, fmt.Errorf("colour %q must be #RGB or #RRGGBB", s)
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return backend.RGB{}, fmt.Errorf("colour %q is not hexadecimal", s)
	}
	return backend.RGB{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}, nil
}

// Sample returns the interpolated colour at t in [0,1].
func (g Gradient) Sample(t float64) backend.RGB {
	if len(g.Stops) == 0 {
		return backend.RGB{}
	}
	t = clampUnit(t)
	if t <= g.Stops[0].Position {
		return g.Stops[0].Color
	}
	for i := 1; i < len(g.Stops); i++ {
		next := g.Stops[i]
		if t > next.Position {
			continue
		}
		prev := g.Stops[i-1]
		span := next.Position - prev.Position
		if span <= 0 {
			return next.Color
		}
		f := (t - prev.Position) / span
		return backend.RGB{
			R: lerp(prev.Color.R, next.Color.R, f),
			G: lerp(prev.Color.G, next.Color.G, f),
			B: lerp(prev.Color.B, next.Color.B, f),
		}
	}
	return g.Stops[len(g.Stops)-1].Color
}

func lerp(a, b uint8, f float64) uint8 {
	return uint8(float64(a) + (float64(b)-float64(a))*f + 0.5)
}

// Solid returns the palette's first colour, used by the static effect.
func (g Gradient) Solid() backend.RGB { return g.Sample(0) }

// Format renders the canonical string, preserving the angle and every stop.
func (g Gradient) Format() string {
	if len(g.Stops) == 1 {
		return formatHex(g.Stops[0].Color)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "linear-gradient(%gdeg", g.Angle)
	for _, s := range g.Stops {
		pct := math.Round(s.Position*100000) / 1000
		fmt.Fprintf(&b, ", %s %g%%", formatHex(s.Color), pct)
	}
	b.WriteString(")")
	return b.String()
}

func formatHex(c backend.RGB) string {
	return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
}

func clampUnit(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
