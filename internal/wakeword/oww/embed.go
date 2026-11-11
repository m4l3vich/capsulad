package oww

import "fmt"

// layerSpec is one exported network layer. Convolutions and pooling carry their
// padding, stride and tensor geometry so both the full-window reference and the
// incremental streaming pipeline can execute exactly the same arithmetic.
type layerSpec struct {
	op string

	// conv
	in, out        int
	kh, kw         int
	pt, pl, pb, pr int
	weights        []float32 // [out][in][kh][kw]
	bias           []float32

	// leakyrelu / clamp floor
	alpha, floor float32

	// maxpool
	sh, sw int
}

// tensor is a time-major activation window: n positions of c*w floats each,
// channel-major inside a position. Keeping time as the outer axis lets the
// streaming pipeline retain a sliding run of positions and recompute only the
// tail that a new window actually needs.
type tensor struct {
	c, w, n int
	data    []float32
}

func parseLayers(a *Assets) ([]layerSpec, error) {
	layers := make([]layerSpec, 0, len(a.Manifest.Layers))
	for _, spec := range a.Manifest.Layers {
		op, _ := spec["op"].(string)
		switch op {
		case "conv":
			l := layerSpec{
				op: "conv",
				in: intOf(spec["in"]), out: intOf(spec["out"]),
				kh: intOf(spec["kh"]), kw: intOf(spec["kw"]),
				pt: intOf(spec["pt"]), pl: intOf(spec["pl"]),
				pb: intOf(spec["pb"]), pr: intOf(spec["pr"]),
			}
			wName, _ := spec["w"].(string)
			weights, ok := a.Tensor(wName)
			if !ok {
				return nil, fmt.Errorf("oww: missing %s", wName)
			}
			l.weights = weights
			if bName, ok := spec["b"].(string); ok {
				bias, ok := a.Tensor(bName)
				if !ok {
					return nil, fmt.Errorf("oww: missing %s", bName)
				}
				l.bias = bias
			}
			layers = append(layers, l)
		case "leakyrelu":
			layers = append(layers, layerSpec{op: "leakyrelu", alpha: floatOf(spec["alpha"])})
		case "max":
			layers = append(layers, layerSpec{op: "max", floor: floatOf(spec["value"])})
		case "maxpool":
			layers = append(layers, layerSpec{
				op: "maxpool",
				kh: intOf(spec["kh"]), kw: intOf(spec["kw"]),
				sh: intOf(spec["sh"]), sw: intOf(spec["sw"]),
			})
		default:
			return nil, fmt.Errorf("oww: unsupported layer %q", op)
		}
	}
	return layers, nil
}

// Embedder is the full-window reference network: 76 mel frames in, one
// 96-dimensional vector out. It exists for parity tests and for offline feature
// export; the live Frontend uses the incremental pipeline in stream.go.
type Embedder struct {
	layers []layerSpec
}

// NewEmbedder builds the network from the exported layer sequence.
func NewEmbedder(a *Assets) (*Embedder, error) {
	layers, err := parseLayers(a)
	if err != nil {
		return nil, err
	}
	return &Embedder{layers: layers}, nil
}

// Embed takes 76 mel frames (76 x 32) and returns the 96-dim embedding. This is
// the reference path: every position is evaluated from scratch.
func (e *Embedder) Embed(frames [][]float32) []float32 {
	t := tensor{c: 1, w: MelBins, n: len(frames), data: make([]float32, len(frames)*MelBins)}
	for y, f := range frames {
		copy(t.data[y*MelBins:], f)
	}
	for i := range e.layers {
		t = e.layers[i].applyWindow(t)
	}
	out := make([]float32, t.c*t.w)
	copy(out, t.data)
	return out
}

// applyWindow evaluates one layer over every position of a complete window.
func (l *layerSpec) applyWindow(t tensor) tensor {
	switch l.op {
	case "conv":
		outN := t.n + l.pt + l.pb - l.kh + 1
		outW := t.w + l.pl + l.pr - l.kw + 1
		out := tensor{c: l.out, w: outW, n: outN, data: make([]float32, outN*l.out*outW)}
		for p := range outN {
			l.convPosition(out.data[p*l.out*outW:], t.data, t.w, outW, p-l.pt)
		}
		return out
	case "leakyrelu", "max":
		l.applyElementwise(t.data)
		return t
	case "maxpool":
		outN := (t.n-l.kh)/l.sh + 1
		outW := (t.w-l.kw)/l.sw + 1
		out := tensor{c: t.c, w: outW, n: outN, data: make([]float32, outN*t.c*outW)}
		for p := range outN {
			l.poolPosition(out.data[p*t.c*outW:], t.data, t.w, outW, p*l.sh, t.c)
		}
		return out
	}
	panic("oww: unknown layer " + l.op)
}

// convPosition computes every output channel for one time position. src holds
// inW floats per source position and inPos is the first source position it
// reads.
//
// The exported network only uses two convolution shapes in bulk: 3x1 (time)
// and 1x3 (frequency). Both go through a batched kernel that loops channels,
// taps and spatial lanes inside one call, so the per-tap function-call and Go
// indexing overhead that dominated the profile disappears. Any other shape
// (the 3x3 first layer, or a future asset) uses the generic per-tap path.
// All paths accumulate in the same (channel, tap) order, so streaming and the
// full-window reference stay bit-identical.
func (l *layerSpec) convPosition(dst, src []float32, inW, outW, inPos int) {
	if l.bias != nil && l.pt == 0 && l.pb == 0 && l.in > 0 && l.out > 0 && inW > 0 && outW > 0 {
		off := inPos * l.in * inW
		switch {
		case l.kh == 3 && l.kw == 1 && l.pl == 0 && l.pr == 0:
			convTime(dst, src[off:], l.weights, l.bias, l.in, l.out, inW, outW)
			return
		case l.kh == 1 && l.kw == 3 && l.pl == 1 && l.pr == 1:
			convFreq(dst, src[off:], l.weights, l.bias, l.in, l.out, inW, outW)
			return
		}
	}
	l.convGeneric(dst, src, inW, outW, inPos)
}

// convGeneric is the portable per-tap fallback. Edge handling is hoisted out
// of the pixel loop so the inner loop has no branches: a Cortex-A35 cannot
// afford them. The accumulation order (ic, ky, kx) matches the original
// full-window implementation exactly.
func (l *layerSpec) convGeneric(dst, src []float32, inW, outW, inPos int) {
	inN := len(src) / (l.in * inW)
	for oc := range l.out {
		d := dst[oc*outW : (oc+1)*outW]
		if l.bias != nil {
			b := l.bias[oc]
			for i := range d {
				d[i] = b
			}
		} else {
			for i := range d {
				d[i] = 0
			}
		}
		for ic := range l.in {
			for ky := range l.kh {
				sy := inPos + ky
				if sy < 0 || sy >= inN {
					continue
				}
				row := src[sy*l.in*inW+ic*inW:]
				for kx := range l.kw {
					xLo := max(0, l.pl-kx)
					xHi := min(outW, inW+l.pl-kx)
					if xLo >= xHi {
						continue
					}
					w := l.weights[((oc*l.in+ic)*l.kh+ky)*l.kw+kx]
					if w == 0 {
						continue
					}
					axpy(d[xLo:xHi], row[xLo+kx-l.pl:xHi+kx-l.pl], w)
				}
			}
		}
	}
}

// applyElementwise applies leaky-relu or the clamp floor in place.
func (l *layerSpec) applyElementwise(data []float32) {
	switch l.op {
	case "leakyrelu":
		for i, v := range data {
			if v < 0 {
				data[i] = v * l.alpha
			}
		}
	case "max":
		for i, v := range data {
			if v < l.floor {
				data[i] = l.floor
			}
		}
	}
}

// poolPosition computes one output time position of a max pool. ch is the
// channel count (pooling preserves channels).
func (l *layerSpec) poolPosition(dst, src []float32, inW, outW, inPos, ch int) {
	for c := range ch {
		d := dst[c*outW : (c+1)*outW]
		base := c * inW
		for x := range outW {
			best := float32(-1e30)
			for ky := range l.kh {
				row := src[(inPos+ky)*ch*inW+base:]
				for kx := range l.kw {
					if v := row[x*l.sw+kx]; v > best {
						best = v
					}
				}
			}
			d[x] = best
		}
	}
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func floatOf(v any) float32 {
	if f, ok := v.(float64); ok {
		return float32(f)
	}
	return 0
}
