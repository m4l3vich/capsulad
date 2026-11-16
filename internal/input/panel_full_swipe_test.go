package input

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/routing"
)

// fullSwipeVolume is a synchronous master volume for the full-panel tests. It
// records the steps that were attempted and the levels that were restored, so
// a test can prove the leading edge was undone through the handler rather than
// left applied.
type fullSwipeVolume struct {
	mu       sync.Mutex
	level    float64
	stepOK   bool
	attempts int
	restores int
}

func (v *fullSwipeVolume) step(delta float64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.attempts++
	if !v.stepOK {
		return false
	}
	next := v.level + delta
	if next < 0 {
		next = 0
	}
	if next > 1 {
		next = 1
	}
	if next == v.level {
		return false
	}
	v.level = next
	return true
}

func (v *fullSwipeVolume) current() float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.level
}

func (v *fullSwipeVolume) restore(level float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.restores++
	v.level = level
}

func (v *fullSwipeVolume) counts() (attempts, restores int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.attempts, v.restores
}

func newFullSwipeManager(t *testing.T, stepOK bool) (*Manager, *controlCounters, *fullSwipeVolume, *logbus.Bus) {
	t.Helper()
	clock := ids.NewClock()
	logs, err := logbus.New(clock, 200, "")
	if err != nil {
		t.Fatal(err)
	}
	c := &controlCounters{}
	v := &fullSwipeVolume{level: 0.5, stepOK: stepOK}
	m := NewManager(context.Background(), clock, logs,
		backend.TouchCaps{PartialGestures: true, FullRaw: true},
		func() (string, string, routing.TouchMode) { return "", "", routing.TouchNone },
		Handlers{
			VolumeUp:      func() bool { return v.step(0.05) },
			VolumeDown:    func() bool { return v.step(-0.05) },
			VolumeLevel:   v.current,
			VolumeRestore: v.restore,
			MediaNext:     func() { c.mediaNext.Add(1) },
			MediaPrevious: func() { c.mediaPrevious.Add(1) },
		})
	t.Cleanup(m.Close)
	return m, c, v, logs
}

func near(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// A west->east crossing with no centre is swipe_right: the leading volume_down
// step applies on its own press, then the completing volume_up dispatches next
// and reverts that one step instead of adding a second.
func TestFullPanelSwipeRightRevertsTheLeadingStep(t *testing.T) {
	m, c, v, _ := newFullSwipeManager(t, true)
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_down", true, base)
	panelKeyAt(m, "volume_down", false, base+60*ms)
	if got := v.current(); !near(got, 0.45) {
		t.Fatalf("leading edge level = %v, want 0.45 applied on its own press", got)
	}

	panelKeyAt(m, "volume_up", true, base+120*ms)
	panelKeyAt(m, "volume_up", false, base+180*ms)

	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("full-panel crossing delivered %d next, want 1", got)
	}
	if got := c.mediaPrevious.Load(); got != 0 {
		t.Fatalf("full-panel crossing delivered %d previous", got)
	}
	if attempts, restores := v.counts(); attempts != 1 || restores != 1 {
		t.Fatalf("steps=%d restores=%d, want the completing edge suppressed and one revert", attempts, restores)
	}
	if got := v.current(); !near(got, 0.5) {
		t.Fatalf("level after revert = %v, want the pre-step 0.5", got)
	}
}

// The mirrored crossing is swipe_left and reverts the leading volume_up step
// the same way, so neither direction adds gain.
func TestFullPanelSwipeLeftRevertsTheLeadingStep(t *testing.T) {
	m, c, v, _ := newFullSwipeManager(t, true)
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_up", true, base)
	panelKeyAt(m, "volume_up", false, base+60*ms)
	panelKeyAt(m, "volume_down", true, base+120*ms)
	panelKeyAt(m, "volume_down", false, base+180*ms)

	if got := c.mediaPrevious.Load(); got != 1 {
		t.Fatalf("full-panel crossing delivered %d previous, want 1", got)
	}
	if got := c.mediaNext.Load(); got != 0 {
		t.Fatalf("full-panel crossing delivered %d next", got)
	}
	if attempts, restores := v.counts(); attempts != 1 || restores != 1 {
		t.Fatalf("steps=%d restores=%d, want the completing edge suppressed and one revert", attempts, restores)
	}
	if got := v.current(); !near(got, 0.5) {
		t.Fatalf("level after revert = %v, want the pre-step 0.5", got)
	}
}

// A slow opposite pair is two deliberate steps: no gesture, no revert.
func TestSlowOppositePairStaysTwoSteps(t *testing.T) {
	m, c, v, _ := newFullSwipeManager(t, true)
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_down", true, base)
	panelKeyAt(m, "volume_down", false, base+60*ms)
	panelKeyAt(m, "volume_up", true, base+450*ms)
	panelKeyAt(m, "volume_up", false, base+510*ms)

	if got := c.mediaNext.Load(); got != 0 {
		t.Fatalf("slow opposite pair delivered %d next", got)
	}
	if got := c.mediaPrevious.Load(); got != 0 {
		t.Fatalf("slow opposite pair delivered %d previous", got)
	}
	if attempts, restores := v.counts(); attempts != 2 || restores != 0 {
		t.Fatalf("steps=%d restores=%d, want two steps and no revert", attempts, restores)
	}
	if got := v.current(); !near(got, 0.5) {
		t.Fatalf("level after two steps = %v, want 0.5", got)
	}
}

// A real hand crossing can be slower than the old 250 ms bound: 330 ms still
// resolves tier=full, suppresses the completing edge and makes exactly one
// restore call.
func TestFullPanelSwipeAcceptsASlowHandCrossing(t *testing.T) {
	m, c, v, logs := newFullSwipeManager(t, true)
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_down", true, base)
	panelKeyAt(m, "volume_down", false, base+60*ms)
	panelKeyAt(m, "volume_up", true, base+330*ms)
	panelKeyAt(m, "volume_up", false, base+390*ms)

	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("330 ms crossing delivered %d next, want exactly 1", got)
	}
	if attempts, restores := v.counts(); attempts != 1 || restores != 1 {
		t.Fatalf("steps=%d restores=%d, want the completing edge suppressed and exactly one revert", attempts, restores)
	}
	if got := v.current(); !near(got, 0.5) {
		t.Fatalf("level after 330 ms crossing = %v, want the pre-step 0.5", got)
	}
	var joined string
	for _, entry := range logs.Snapshot("") {
		if entry.Severity == "INFO" {
			joined += entry.Message + "\n"
		}
	}
	if !strings.Contains(joined, "panel gesture swipe_right resolved tier=full") ||
		!strings.Contains(joined, "press_gap=330ms window=400ms") {
		t.Fatalf("330 ms crossing was not logged as tier=full within the 400 ms window:\n%s", joined)
	}
}

// Two same-direction taps followed by the opposite edge are three deliberate
// volume steps, never a full-panel crossing, even though the last two contacts
// are opposite and inside the window.
func TestRepeatedDirectionPrefixStaysSteps(t *testing.T) {
	m, c, v, _ := newFullSwipeManager(t, true)
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_down", true, base)
	panelKeyAt(m, "volume_down", false, base+60*ms)
	panelKeyAt(m, "volume_down", true, base+120*ms)
	panelKeyAt(m, "volume_down", false, base+180*ms)
	panelKeyAt(m, "volume_up", true, base+250*ms)
	panelKeyAt(m, "volume_up", false, base+310*ms)

	if got := c.mediaNext.Load(); got != 0 {
		t.Fatalf("repeated-direction prefix delivered %d next", got)
	}
	if got := c.mediaPrevious.Load(); got != 0 {
		t.Fatalf("repeated-direction prefix delivered %d previous", got)
	}
	if attempts, restores := v.counts(); attempts != 3 || restores != 0 {
		t.Fatalf("steps=%d restores=%d, want three steps and no revert", attempts, restores)
	}
	if got := v.current(); !near(got, 0.45) {
		t.Fatalf("level after three steps = %v, want 0.45", got)
	}
}

// A centre contact between the edges keeps the centre-gated semantics: the
// gesture comes from the centre gate, so the leading edge's step is NOT
// reverted and no full-panel tier is resolved.
func TestCentreBetweenEdgesKeepsTheCentreGate(t *testing.T) {
	m, c, v, _ := newFullSwipeManager(t, true)
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_down", true, base)
	panelKeyAt(m, "volume_down", false, base+40*ms)
	panelKeyAt(m, "center", true, base+80*ms)
	panelKeyAt(m, "center", false, base+110*ms)
	panelKeyAt(m, "volume_up", true, base+180*ms)
	panelKeyAt(m, "volume_up", false, base+250*ms)

	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("centre-gated crossing delivered %d next, want exactly 1", got)
	}
	if attempts, restores := v.counts(); attempts != 1 || restores != 0 {
		t.Fatalf("steps=%d restores=%d, want the centre gate to keep the leading step", attempts, restores)
	}
	if got := v.current(); !near(got, 0.45) {
		t.Fatalf("level after centre-gated crossing = %v, want 0.45", got)
	}
}

// A gesture whose leading step did not apply (a bound) is still dispatched
// exactly once; there is nothing to revert.
func TestFullPanelSwipeWithoutAnAppliedStepStillDispatches(t *testing.T) {
	m, c, v, _ := newFullSwipeManager(t, false)
	const base = uint64(1_000_000_000)
	const ms = uint64(time.Millisecond)

	panelKeyAt(m, "volume_down", true, base)
	panelKeyAt(m, "volume_down", false, base+60*ms)
	panelKeyAt(m, "volume_up", true, base+120*ms)
	panelKeyAt(m, "volume_up", false, base+180*ms)

	if got := c.mediaNext.Load(); got != 1 {
		t.Fatalf("unrevertable crossing delivered %d next, want exactly 1", got)
	}
	if attempts, restores := v.counts(); attempts != 1 || restores != 0 {
		t.Fatalf("steps=%d restores=%d, want one failed step and no revert", attempts, restores)
	}
	if got := v.current(); !near(got, 0.5) {
		t.Fatalf("level after failed step = %v, want 0.5", got)
	}
}
