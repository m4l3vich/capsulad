package input

import (
	"sync/atomic"
	"testing"
	"time"
)

// A chain must end its own sequence. The shadow used to keep the middle centre
// of a matched chain, so the next edge that arrived inside the released-centre
// window joined that stale centre and produced a second, phantom skip. Contacts
// sharing one kernel timestamp (a backend that reports a whole swipe at once)
// reproduced it.
func TestConsecutiveChainsDoNotDoubleFire(t *testing.T) {
	var mode atomic.Int32
	m, c := newPanelModeManager(t, &mode, "")
	const base = uint64(2_000_000_000)
	for _, key := range []string{"volume_down", "center", "volume_up"} {
		panelKeyAt(m, key, true, base)
	}
	for _, key := range []string{"volume_down", "center", "volume_up"} {
		panelKeyAt(m, key, false, base+1)
	}
	for _, key := range []string{"volume_up", "center", "volume_down"} {
		panelKeyAt(m, key, true, base+2)
	}
	for _, key := range []string{"volume_up", "center", "volume_down"} {
		panelKeyAt(m, key, false, base+3)
	}
	if next, previous := c.mediaNext.Load(), c.mediaPrevious.Load(); next != 1 || previous != 1 {
		t.Fatalf("two chains: next=%d previous=%d, want 1/1", next, previous)
	}
}

// The released-centre gate is measured from the centre's release, not its
// press: a centre held for a while must still gate a quick edge after it is
// let go, and must not gate an unrelated edge that lands late.
func TestReleasedCentreGateMeasuresFromTheRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		release  time.Duration
		edge     time.Duration
		next     int32
		previous int32
	}{
		{"held-then-quick-edge", 600 * time.Millisecond, 700 * time.Millisecond, 0, 1},
		{"released-then-late-edge", 600 * time.Millisecond, 1500 * time.Millisecond, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mode atomic.Int32
			m, c := newPanelModeManager(t, &mode, "")
			const base = uint64(3_000_000_000)
			panelKeyAt(m, "center", true, base)
			panelKeyAt(m, "center", false, base+uint64(tc.release))
			panelKeyAt(m, "volume_down", true, base+uint64(tc.edge))
			panelKeyAt(m, "volume_down", false, base+uint64(tc.edge)+uint64(80*time.Millisecond))
			if got := c.mediaPrevious.Load(); got != tc.previous {
				t.Fatalf("previous=%d, want %d", got, tc.previous)
			}
			if got := c.mediaNext.Load(); got != tc.next {
				t.Fatalf("next=%d, want %d", got, tc.next)
			}
		})
	}
}
