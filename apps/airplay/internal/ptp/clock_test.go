package ptp_test

import (
	"testing"
	"time"

	"capsulad/apps/airplay/internal/ptp"
)

var gmID = [8]byte{0x00, 0x11, 0x22, 0xFF, 0xFE, 0x33, 0x44, 0x55}

// tsFromNanos builds a PTP timestamp from int64 nanoseconds, flooring so that
// times before the epoch stay monotonic.
func tsFromNanos(ns int64) ptp.Timestamp {
	const perSecond = 1_000_000_000
	sec, rem := ns/perSecond, ns%perSecond
	if rem < 0 {
		sec--
		rem += perSecond
	}
	return ptp.Timestamp{Seconds: uint64(sec), Nanos: uint32(rem)}
}

func timestampNanos(t *testing.T, ts ptp.Timestamp) int64 {
	t.Helper()
	return int64(ts.Seconds)*1_000_000_000 + int64(ts.Nanos)
}

func syncMessage(seq uint16, origin ptp.Timestamp, correction int64) *ptp.Message {
	return &ptp.Message{
		Header: ptp.Header{
			Type:            ptp.MsgSync,
			Version:         2,
			Flags:           0x0208,
			CorrectionNanos: correction,
			SourceClockID:   gmID,
			SourcePort:      1,
			SequenceID:      seq,
			Control:         0,
			LogInterval:     -3,
		},
		OriginTimestamp: origin,
		HasOrigin:       true,
	}
}

func followUpMessage(seq uint16, origin ptp.Timestamp, correction int64) *ptp.Message {
	m := syncMessage(seq, origin, correction)
	m.Header.Type = ptp.MsgFollowUp
	m.Header.Control = 2
	return m
}

// feedPair delivers one two-step sample: the Sync first, then the Follow_Up that
// carries the master's precise send time.
func feedPair(t *testing.T, c *ptp.Clock, seq uint16, localNanos, originNanos, syncCorrection, fupCorrection int64) {
	t.Helper()
	if c.AddSync(syncMessage(seq, ptp.Timestamp{}, syncCorrection), time.Unix(0, localNanos)) {
		t.Fatalf("seq %d: a two-step Sync alone completed a pair", seq)
	}
	if !c.AddFollowUp(followUpMessage(seq, tsFromNanos(originNanos), fupCorrection), time.Unix(0, localNanos+50_000_000)) {
		t.Fatalf("seq %d: Follow_Up did not complete its Sync", seq)
	}
	if n := c.Pending(); n != 0 {
		t.Fatalf("seq %d: %d messages left pending", seq, n)
	}
}

// clockModel is the synthetic truth the clock has to recover: the master runs
// offset0 ahead of the local clock and drifts by slope nanoseconds per
// nanosecond from a reference instant.
type clockModel struct {
	base    int64
	offset0 int64
	slope   float64
}

func (m clockModel) offsetAt(localNanos int64) int64 {
	return m.offset0 + int64(m.slope*float64(localNanos-m.base))
}

func (m clockModel) masterAt(localNanos int64) int64 {
	return localNanos + m.offsetAt(localNanos)
}

// discipline fills a clock with eight samples of the model, one every 125 ms,
// starting at base.
func discipline(t *testing.T, c *ptp.Clock, m clockModel, syncCorrection, fupCorrection int64) {
	t.Helper()
	const interval = 125 * time.Millisecond
	for i := uint16(0); i < 8; i++ {
		local := m.base + int64(i)*int64(interval)
		feedPair(t, c, i, local, m.masterAt(local), syncCorrection, fupCorrection)
	}
}

func within(t *testing.T, what string, got, want, tolerance int64) {
	t.Helper()
	if d := got - want; d < -tolerance || d > tolerance {
		t.Errorf("%s = %d, want %d ± %d (off by %.3f µs)", what, got, want, tolerance, float64(d)/1000)
	}
}

func TestClockDiscipline(t *testing.T) {
	got := ptp.NewClock()
	// The master runs 3 s ahead of the local clock and gains 50 ppm.
	const (
		offset0 = int64(3_000_000_000)
		slope   = 50e-6
	)
	series := clockModel{base: time.Now().Add(-2 * time.Second).UnixNano(), offset0: offset0, slope: slope}
	discipline(t, got, series, 0, 0)

	estimate, ok := got.OffsetNanos()
	if !ok {
		t.Fatal("OffsetNanos: clock not disciplined after eight agreeing pairs")
	}
	now := time.Now().UnixNano()
	within(t, "OffsetNanos", estimate, series.offsetAt(now), 200_000)

	// Local to master must recover the model's master time at a local instant.
	local := time.Now()
	ts, ok := got.LocalToMaster(local)
	if !ok {
		t.Fatal("LocalToMaster: not ok")
	}
	within(t, "LocalToMaster", timestampNanos(t, ts), series.masterAt(local.UnixNano()), 200_000)

	// MasterToLocal must invert LocalToMaster exactly.
	back, ok := got.MasterToLocal(ts, local)
	if !ok {
		t.Fatal("MasterToLocal: not ok")
	}
	if !back.Equal(local) {
		t.Fatalf("MasterToLocal(LocalToMaster(local)) = %v, want %v", back, local)
	}

	// The fit must also have recovered the drift, not just the offset: a second
	// of local time is a second plus 50 µs of master time.
	earlier, ok := got.LocalToMaster(local.Add(-time.Second))
	if !ok {
		t.Fatal("LocalToMaster: not ok")
	}
	advance := timestampNanos(t, ts) - timestampNanos(t, earlier)
	within(t, "one second of master time", advance, 1_000_000_000+int64(slope*1e9), 10_000)
}

func TestClockNeedsFourPairs(t *testing.T) {
	got := ptp.NewClock()
	series := clockModel{base: time.Now().Add(-2 * time.Second).UnixNano(), offset0: 1_000_000}
	for i := uint16(0); i < 3; i++ {
		local := series.base + int64(i)*int64(125*time.Millisecond)
		feedPair(t, got, i, local, series.masterAt(local), 0, 0)
		if _, ok := got.OffsetNanos(); ok {
			t.Fatalf("clock disciplined after %d pairs", i+1)
		}
	}
	local := series.base + 3*int64(125*time.Millisecond)
	feedPair(t, got, 3, local, series.masterAt(local), 0, 0)
	if _, ok := got.OffsetNanos(); !ok {
		t.Fatal("clock not disciplined after four agreeing pairs")
	}
}

func TestClockToleratesJitter(t *testing.T) {
	got := ptp.NewClock()
	series := clockModel{base: time.Now().Add(-2 * time.Second).UnixNano(), offset0: -250_000}
	// Network jitter of ±100 µs on top of the model, well inside the 1 ms window.
	jitter := []int64{100_000, -80_000, 40_000, -100_000, 90_000, -10_000, 60_000, -60_000}
	for i, j := range jitter {
		local := series.base + int64(i)*int64(125*time.Millisecond)
		feedPair(t, got, uint16(i), local, series.masterAt(local)+j, 0, 0)
	}
	estimate, ok := got.OffsetNanos()
	if !ok {
		t.Fatal("OffsetNanos: 1 ms of agreement should discipline the clock")
	}
	within(t, "OffsetNanos", estimate, series.offsetAt(time.Now().UnixNano()), 200_000)
}

func TestClockRejectsDisagreeingPairs(t *testing.T) {
	got := ptp.NewClock()
	series := clockModel{base: time.Now().Add(-2 * time.Second).UnixNano(), offset0: 0}
	// 20 ms of wander, far outside the 1 ms agreement window.
	for i := range 8 {
		local := series.base + int64(i)*int64(125*time.Millisecond)
		var wander int64
		if i%2 == 0 {
			wander = 20_000_000
		} else {
			wander = -20_000_000
		}
		feedPair(t, got, uint16(i), local, series.masterAt(local)+wander, 0, 0)
	}
	if _, ok := got.OffsetNanos(); ok {
		t.Fatal("OffsetNanos: pairs that disagree by 40 ms were accepted")
	}
	if _, ok := got.LocalToMaster(time.Now()); ok {
		t.Fatal("LocalToMaster: undisciplined clock answered")
	}
}

func TestClockGoesStale(t *testing.T) {
	got := ptp.NewClock()
	series := clockModel{base: time.Now().Add(-5 * time.Second).UnixNano(), offset0: 7_000_000}
	discipline(t, got, series, 0, 0)

	lastIngress := time.Unix(0, series.base+7*int64(125*time.Millisecond))
	ts := tsFromNanos(series.masterAt(series.base + 7*int64(125*time.Millisecond)))
	if _, ok := got.MasterToLocal(ts, lastIngress); !ok {
		t.Fatal("MasterToLocal: fresh estimate refused")
	}
	if _, ok := got.LocalToMaster(lastIngress); !ok {
		t.Fatal("LocalToMaster: fresh estimate refused")
	}

	now := time.Now()
	if _, ok := got.MasterToLocal(ts, now); ok {
		t.Fatal("MasterToLocal: answered with an estimate older than 4 s")
	}
	if _, ok := got.LocalToMaster(now); ok {
		t.Fatal("LocalToMaster: answered with an estimate older than 4 s")
	}
	if _, ok := got.OffsetNanos(); ok {
		t.Fatal("OffsetNanos: reported a stale estimate as current")
	}
}

func TestClockOneStepSync(t *testing.T) {
	got := ptp.NewClock()
	base := time.Now().Add(-2 * time.Second).UnixNano()
	const offset0 = int64(-1_000_000)
	for i := uint16(0); i < 8; i++ {
		local := base + int64(i)*int64(125*time.Millisecond)
		origin := tsFromNanos(local + offset0)
		if !got.AddSync(syncMessage(i, origin, 0), time.Unix(0, local)) {
			t.Fatalf("seq %d: one-step Sync did not complete a pair", i)
		}
		if n := got.Pending(); n != 0 {
			t.Fatalf("seq %d: one-step Sync left %d messages pending", i, n)
		}
	}
	estimate, ok := got.OffsetNanos()
	if !ok {
		t.Fatal("OffsetNanos: not disciplined after one-step Syncs")
	}
	within(t, "OffsetNanos", estimate, offset0, 200_000)
}

func TestClockFollowUpBeforeSync(t *testing.T) {
	got := ptp.NewClock()
	base := time.Now().Add(-1 * time.Second).UnixNano()
	origin := tsFromNanos(base + 5_000_000)

	if got.AddFollowUp(followUpMessage(9, origin, 0), time.Unix(0, base)) {
		t.Fatal("AddFollowUp: matched without a Sync")
	}
	if n := got.Pending(); n != 1 {
		t.Fatalf("Pending = %d, want 1", n)
	}
	if !got.AddSync(syncMessage(9, ptp.Timestamp{}, 0), time.Unix(0, base)) {
		t.Fatal("AddSync: did not complete the out-of-order pair")
	}
	if n := got.Pending(); n != 0 {
		t.Fatalf("Pending = %d, want 0", n)
	}
}

func TestClockUnmatchedFollowUpAndReset(t *testing.T) {
	got := ptp.NewClock()
	base := time.Now().UnixNano()
	got.AddFollowUp(followUpMessage(1, tsFromNanos(base), 0), time.Unix(0, base))
	got.AddSync(syncMessage(2, ptp.Timestamp{}, 0), time.Unix(0, base))
	if n := got.Pending(); n != 2 {
		t.Fatalf("Pending = %d, want 2", n)
	}
	got.Reset()
	if n := got.Pending(); n != 0 {
		t.Fatalf("Pending after Reset = %d, want 0", n)
	}
	if _, ok := got.OffsetNanos(); ok {
		t.Fatal("OffsetNanos: Reset clock is disciplined")
	}
}

func TestClockAppliesCorrections(t *testing.T) {
	series := clockModel{base: time.Now().Add(-2 * time.Second).UnixNano(), offset0: 1_000_000}
	plain := ptp.NewClock()
	corrected := ptp.NewClock()
	discipline(t, plain, series, 0, 0)
	discipline(t, corrected, series, 400, 600)

	want, ok := plain.OffsetNanos()
	if !ok {
		t.Fatal("OffsetNanos: undisciplined")
	}
	got, ok := corrected.OffsetNanos()
	if !ok {
		t.Fatal("OffsetNanos: undisciplined")
	}
	// The Follow_Up and Sync correction fields add up and shift the estimate by
	// exactly their sum (802.1AS-2020 11.4.4.2.1).
	if d := got - want; d != 1000 {
		t.Fatalf("corrected offset - plain offset = %d ns, want 1000", d)
	}
}

func TestClockIgnoresWrongMessages(t *testing.T) {
	now := time.Now()
	got := ptp.NewClock()
	if got.AddSync(followUpMessage(1, ptp.Timestamp{}, 0), now) {
		t.Fatal("AddSync accepted a Follow_Up")
	}
	if got.AddSync(nil, now) {
		t.Fatal("AddSync accepted nil")
	}
	if got.AddFollowUp(syncMessage(1, ptp.Timestamp{}, 0), now) {
		t.Fatal("AddFollowUp accepted a Sync")
	}
	if got.AddFollowUp(nil, now) {
		t.Fatal("AddFollowUp accepted nil")
	}
	// A timestamp that cannot be expressed in nanoseconds is dropped, not
	// folded in as zero.
	broken := syncMessage(3, ptp.Timestamp{}, 0)
	broken.OriginTimestamp = ptp.Timestamp{Seconds: 1 << 47, Nanos: 0}
	if got.AddSync(broken, now) {
		t.Fatal("AddSync accepted an out-of-range origin timestamp")
	}
	if n := got.Pending(); n != 0 {
		t.Fatalf("Pending = %d, want 0", n)
	}
}
