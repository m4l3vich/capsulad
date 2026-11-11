package oww

import "fmt"

// streamStage is one layer of the incremental embedding pipeline.
//
// Every stage keeps a sliding run of its own output positions for the current
// window, position-major (pos, channel, frequency). When the window advances by
// WindowStep mel frames, the run shifts by shift output positions, so only the
// new tail is evaluated. Dropped positions are gone; the retained values were
// computed from exactly the same source frames as a full-window run would use,
// which makes the incremental result bit-identical to Embedder.Embed.
type streamStage struct {
	spec  layerSpec
	post  []layerSpec // pointwise activations fused into this stage's output
	ratio int         // previous-stage positions consumed per output position
	shift int         // output positions advanced per WindowStep mel frames
	pos   int         // output positions in one embedding window
	c, w  int         // output channels and frequency bins
	buf   []float32
}

// streamNet is the sliding-window embedding network. It owns one reusable
// activation buffer per stage plus the current 76-frame mel window.
type streamNet struct {
	stages  []streamStage
	mel     []float32 // WindowFrames * MelBins
	started bool
}

func newStreamNet(a *Assets) (*streamNet, error) {
	layers, err := parseLayers(a)
	if err != nil {
		return nil, err
	}
	n := &streamNet{mel: make([]float32, WindowFrames*MelBins)}
	c, w := 1, MelBins
	stride, rf := 1, 1 // the mel input itself: one frame per position
	for _, l := range layers {
		switch l.op {
		case "leakyrelu", "max":
			// Pointwise activations are fused into the conv that feeds them,
			// so they need no buffer of their own and never copy a window.
			if len(n.stages) == 0 || n.stages[len(n.stages)-1].spec.op != "conv" {
				return nil, fmt.Errorf("oww: %q does not follow a convolution", l.op)
			}
			prev := &n.stages[len(n.stages)-1]
			prev.post = append(prev.post, l)
			continue
		}
		st := streamStage{spec: l}
		switch l.op {
		case "conv":
			rf += (l.kh - 1) * stride
			c = l.out
			w = w + l.pl + l.pr - l.kw + 1
			st.ratio = 1
		case "maxpool":
			rf += (l.kh - 1) * stride
			st.ratio = l.sh
			stride *= l.sh
			w = (w-l.kw)/l.sw + 1
		}
		span := WindowFrames - rf
		if span < 0 || span%stride != 0 || WindowStep%stride != 0 {
			return nil, fmt.Errorf("oww: layer %q does not tile the %d-frame window at stride %d",
				l.op, WindowFrames, stride)
		}
		st.shift = WindowStep / stride
		st.pos = span/stride + 1
		st.c, st.w = c, w
		st.buf = make([]float32, st.pos*c*w)
		if len(n.stages) > 0 {
			prev := &n.stages[len(n.stages)-1]
			if (st.pos-1)*st.ratio+l.kh > prev.pos {
				return nil, fmt.Errorf("oww: layer %q reads past its input window", l.op)
			}
		}
		n.stages = append(n.stages, st)
	}
	if rf != WindowFrames {
		return nil, fmt.Errorf("oww: receptive field %d does not cover the %d-frame window", rf, WindowFrames)
	}
	last := &n.stages[len(n.stages)-1]
	if last.pos != 1 || last.c*last.w != EmbeddingDim {
		return nil, fmt.Errorf("oww: final stage yields %d positions of %d values",
			last.pos, last.c*last.w)
	}
	return n, nil
}

// reset returns the pipeline to its pre-audio phase: the first window after a
// reset is evaluated from scratch, so no activation from the other side of a
// mute, capture gap or activation can survive.
func (n *streamNet) reset() { n.started = false }

// window returns the embedding for one complete 76-frame mel window. Successive
// calls must advance by exactly WindowStep frames; frames is that window.
func (n *streamNet) window(frames []float32) []float32 {
	if !n.started {
		copy(n.mel, frames[:WindowFrames*MelBins])
		n.started = true
		for i := range n.stages {
			n.compute(i, 0, n.stages[i].pos)
		}
	} else {
		newFrames := WindowStep * MelBins
		copy(n.mel, n.mel[newFrames:])
		copy(n.mel[len(n.mel)-newFrames:], frames[len(frames)-newFrames:])
		for i := range n.stages {
			st := &n.stages[i]
			drop := st.shift * st.c * st.w
			copy(st.buf, st.buf[drop:])
			n.compute(i, st.pos-st.shift, st.pos)
		}
	}
	last := &n.stages[len(n.stages)-1]
	out := make([]float32, last.c*last.w)
	copy(out, last.buf)
	return out
}

// compute evaluates output positions [from, to) of stage i from the previous
// stage's current window. Positions before from are retained from the previous
// window and are already exact.
func (n *streamNet) compute(i, from, to int) {
	st := &n.stages[i]
	l := &st.spec
	src := n.mel
	inW := MelBins
	if i > 0 {
		prev := &n.stages[i-1]
		src = prev.buf
		inW = prev.w
	}
	for p := from; p < to; p++ {
		pos := p * st.c * st.w
		dst := st.buf[pos:]
		switch l.op {
		case "conv":
			l.convPosition(dst, src, inW, st.w, p*st.ratio-l.pt)
			for _, post := range st.post {
				post.applyElementwise(dst[:st.c*st.w])
			}
		case "maxpool":
			l.poolPosition(dst, src, inW, st.w, p*st.ratio, st.c)
		}
	}
}
