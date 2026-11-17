package ptp

import (
	"math"
	"sync"
	"time"
)

// Sample is one Sync/Follow_Up measurement as the slave saw it.
type Sample struct {
	// Sequence is the sequenceId that paired the Sync with its Follow_Up.
	Sequence uint16
	// SyncIngress is when the Sync arrived.
	SyncIngress time.Time
	// Origin is the master's send time: the originTimestamp of a one-step Sync,
	// or the preciseOriginTimestamp of the Follow_Up.
	Origin Timestamp
	// CorrectionNanos is the sum of the correctionFields of the Sync and its
	// Follow_Up, which is what 802.1AS-2020 11.4.4.2.1 requires be added to the
	// preciseOriginTimestamp.
	CorrectionNanos int64
	// PathDelayNanos is the measured mean path delay, to be added to the
	// master's send time as well. The Clock does not run the
	// Delay_Req/Delay_Resp exchange itself (NewDelayReq is the message to send
	// and Parse decodes the answer), so samples carry 0 here.
	PathDelayNanos int64
}

// Clock tuning.
const (
	// maxSamples is the width of the linear fit: the last eight matched pairs.
	maxSamples = 8
	// minSamples is how many pairs must have been seen before the estimate is
	// offered at all.
	minSamples = 4
	// agreeWindowNanos is how closely the pairs must agree once the fitted
	// drift is removed: 1 ms.
	agreeWindowNanos = 1_000_000
	// staleAfter is how long the estimate stays usable without a fresh pair.
	staleAfter = 4 * time.Second
	// pendingLifetime is how long an unmatched Sync or Follow_Up is kept while
	// waiting for its partner.
	pendingLifetime = time.Second
	// maxPending caps each pairing buffer, so a link that drops alternate
	// messages cannot grow it without bound.
	maxPending = 64
)

// pendingSync is a Sync whose precise timestamp has not arrived yet.
type pendingSync struct {
	ingress    time.Time
	correction int64
}

// pendingFollowUp is a Follow_Up whose Sync has not been seen (out of order, or
// the Sync was lost after the Follow_Up already completed an earlier pairing).
type pendingFollowUp struct {
	ingress    time.Time
	origin     Timestamp
	correction int64
}

// fit is the least-squares line offset(t) = offset0 + yMean + slope*((t-t0)-xMean)
// over the retained samples, where y is the offset relative to offset0 and x is
// the local time relative to t0. Keeping the fit in coordinates relative to the
// first sample preserves nanosecond resolution: at an absolute time of 1.7e18 ns
// a float64 would quantise to ~256 ns.
type fit struct {
	// disciplined reports that at least minSamples pairs are held and that their
	// offsets agree within agreeWindowNanos once the fitted drift is removed.
	disciplined bool
	// t0 and offset0 are the local time and the master-minus-local offset of the
	// first retained sample.
	t0      int64
	offset0 int64
	// xMean and yMean are the means of the relative coordinates.
	xMean float64
	yMean float64
	// slope is the drift in nanoseconds per nanosecond.
	slope float64
}

// Clock estimates how the grandmaster's clock relates to the local one from the
// Sync/Follow_Up pairs it is fed. A Clock must not be copied after first use; it
// is safe for concurrent use.
type Clock struct {
	mu sync.Mutex

	// samples is a ring of the last maxSamples measurements, oldest at
	// (next+maxSamples-count)%maxSamples.
	samples [maxSamples]Sample
	count   int
	next    int

	fit fit

	pendingSyncs     map[uint16]pendingSync
	pendingFollowUps map[uint16]pendingFollowUp
}

// NewClock returns an undisciplined clock.
func NewClock() *Clock {
	return &Clock{
		pendingSyncs:     make(map[uint16]pendingSync),
		pendingFollowUps: make(map[uint16]pendingFollowUp),
	}
}

// AddSync folds a Sync in. It reports true once the pair is complete: a one-step
// Sync carries its own originTimestamp, and a two-step Sync is complete as soon
// as its Follow_Up is known (which is the case when the Follow_Up arrived
// first). A Sync whose precise timestamp is still to come only updates the
// pairing state and returns false. ingress is when the Sync was received.
func (c *Clock) AddSync(m *Message, ingress time.Time) (matched bool) {
	if m == nil || m.Header.Type != MsgSync || !m.HasOrigin {
		return false
	}
	if _, ok := m.OriginTimestamp.nanos(); !ok {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	seq := m.Header.SequenceID
	if m.OriginTimestamp != (Timestamp{}) {
		// One-step: the Sync itself carries the master's send time.
		delete(c.pendingSyncs, seq)
		delete(c.pendingFollowUps, seq)
		c.record(Sample{
			Sequence:        seq,
			SyncIngress:     ingress,
			Origin:          m.OriginTimestamp,
			CorrectionNanos: m.Header.CorrectionNanos,
		})
		return true
	}

	if fup, ok := c.pendingFollowUps[seq]; ok {
		delete(c.pendingFollowUps, seq)
		c.record(Sample{
			Sequence:        seq,
			SyncIngress:     ingress,
			Origin:          fup.origin,
			CorrectionNanos: m.Header.CorrectionNanos + fup.correction,
		})
		return true
	}

	c.prune(ingress)
	c.pendingSyncs[seq] = pendingSync{ingress: ingress, correction: m.Header.CorrectionNanos}
	return false
}

// AddFollowUp folds a Follow_Up in by sequence id. It reports true when it
// completed a Sync that was already held, in which case the pair is measured
// against the Sync's ingress time; otherwise the Follow_Up waits for its Sync.
func (c *Clock) AddFollowUp(m *Message, ingress time.Time) bool {
	if m == nil || m.Header.Type != MsgFollowUp || !m.HasOrigin {
		return false
	}
	if _, ok := m.OriginTimestamp.nanos(); !ok {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	seq := m.Header.SequenceID
	if sync, ok := c.pendingSyncs[seq]; ok {
		delete(c.pendingSyncs, seq)
		c.record(Sample{
			Sequence:        seq,
			SyncIngress:     sync.ingress,
			Origin:          m.OriginTimestamp,
			CorrectionNanos: sync.correction + m.Header.CorrectionNanos,
		})
		return true
	}

	c.prune(ingress)
	c.pendingFollowUps[seq] = pendingFollowUp{
		ingress:    ingress,
		origin:     m.OriginTimestamp,
		correction: m.Header.CorrectionNanos,
	}
	return false
}

// record appends a completed pair to the ring and refits the estimate. The
// caller holds c.mu.
func (c *Clock) record(s Sample) {
	c.samples[c.next] = s
	c.next = (c.next + 1) % maxSamples
	if c.count < maxSamples {
		c.count++
	}
	c.refit()
}

// prune drops pairing state that is too old to be useful and caps both buffers.
// The caller holds c.mu.
func (c *Clock) prune(now time.Time) {
	for seq, p := range c.pendingSyncs {
		if now.Sub(p.ingress) > pendingLifetime {
			delete(c.pendingSyncs, seq)
		}
	}
	for seq, p := range c.pendingFollowUps {
		if now.Sub(p.ingress) > pendingLifetime {
			delete(c.pendingFollowUps, seq)
		}
	}
	for seq := range c.pendingSyncs {
		if len(c.pendingSyncs) <= maxPending {
			break
		}
		delete(c.pendingSyncs, seq)
	}
	for seq := range c.pendingFollowUps {
		if len(c.pendingFollowUps) <= maxPending {
			break
		}
		delete(c.pendingFollowUps, seq)
	}
}

// refit recomputes the least-squares line over the retained samples. The caller
// holds c.mu.
func (c *Clock) refit() {
	c.fit = fit{}
	if c.count == 0 {
		return
	}
	first := c.sample(0)
	c.fit.t0 = first.localNanos()
	c.fit.offset0 = first.offsetNanos()

	var sumX, sumY float64
	for i := range c.count {
		s := c.sample(i)
		sumX += float64(s.localNanos() - c.fit.t0)
		sumY += float64(s.offsetNanos() - c.fit.offset0)
	}
	n := float64(c.count)
	c.fit.xMean = sumX / n
	c.fit.yMean = sumY / n

	var sxx, sxy float64
	for i := range c.count {
		s := c.sample(i)
		x := float64(s.localNanos()-c.fit.t0) - c.fit.xMean
		y := float64(s.offsetNanos()-c.fit.offset0) - c.fit.yMean
		sxx += x * x
		sxy += x * y
	}
	if sxx > 0 {
		c.fit.slope = sxy / sxx
	}

	worst := 0.0
	for i := range c.count {
		s := c.sample(i)
		x := float64(s.localNanos()-c.fit.t0) - c.fit.xMean
		y := float64(s.offsetNanos()-c.fit.offset0) - c.fit.yMean
		if r := math.Abs(y - c.fit.slope*x); r > worst {
			worst = r
		}
	}
	c.fit.disciplined = c.count >= minSamples && worst <= agreeWindowNanos
}

// sample returns the i'th oldest retained sample. The caller holds c.mu.
func (c *Clock) sample(i int) Sample {
	return c.samples[(c.next+maxSamples-c.count+i)%maxSamples]
}

// localNanos is the sample's local clock reading.
func (s Sample) localNanos() int64 { return s.SyncIngress.UnixNano() }

// offsetNanos is the sample's master-minus-local offset. Both sides are int64
// nanoseconds, so the difference is exact; the callers that record samples
// reject timestamps whose nanoseconds would not fit an int64.
func (s Sample) offsetNanos() int64 {
	masterNs, _ := s.Origin.nanos()
	return masterNs + s.CorrectionNanos + s.PathDelayNanos - s.localNanos()
}

// offsetAt evaluates the fitted line at a local time. The caller holds c.mu.
func (c *Clock) offsetAt(localNanos int64) (int64, bool) {
	if !c.fit.disciplined {
		return 0, false
	}
	t := float64(localNanos-c.fit.t0) - c.fit.xMean
	return c.fit.offset0 + int64(math.Round(c.fit.yMean+c.fit.slope*t)), true
}

// ready reports whether the estimate may be used for a conversion at now: the
// clock must be disciplined and the newest pair must not be older than
// staleAfter. The caller holds c.mu.
func (c *Clock) ready(now time.Time) bool {
	if !c.fit.disciplined || c.count == 0 {
		return false
	}
	age := now.Sub(c.sample(c.count - 1).SyncIngress)
	return age <= staleAfter && age >= -staleAfter
}

// OffsetNanos returns the current estimate of (master - local) in nanoseconds.
// The flag is true only when the estimate is trustworthy: at least four matched
// pairs must agree within 1 ms once the fitted drift is removed, and the newest
// pair must be less than staleAfter old. The estimate is a linear fit of the
// last maxSamples pairs, so it accounts for the drift between the two clocks.
func (c *Clock) OffsetNanos() (int64, bool) {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.ready(now) {
		return 0, false
	}
	return c.offsetAt(now.UnixNano())
}

// LocalToMaster converts a local time to grandmaster time with the current
// estimate. The flag is false while the clock is undisciplined or the estimate
// is stale, exactly as for MasterToLocal.
func (c *Clock) LocalToMaster(local time.Time) (Timestamp, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.ready(local) {
		return Timestamp{}, false
	}
	offset, ok := c.offsetAt(local.UnixNano())
	if !ok {
		return Timestamp{}, false
	}
	return timestampFromNanos(local.UnixNano() + offset), true
}

// MasterToLocal is the inverse of LocalToMaster: it converts a grandmaster
// timestamp to the local clock, measured from now (the returned time keeps now's
// monotonic reading when it has one). The flag is false while the clock is
// undisciplined, when the offset is stale (no matched pair for 4 s), or when ts
// cannot be expressed in nanoseconds.
func (c *Clock) MasterToLocal(ts Timestamp, now time.Time) (time.Time, bool) {
	ns, ok := ts.nanos()
	if !ok {
		return time.Time{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.ready(now) {
		return time.Time{}, false
	}
	offset, ok := c.offsetAt(now.UnixNano())
	if !ok {
		return time.Time{}, false
	}
	return now.Add(time.Duration(ns - (now.UnixNano() + offset))), true
}

// Pending returns how many Sync and Follow_Up messages are held waiting for
// their partner. It exists for tests and for health reporting.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pendingSyncs) + len(c.pendingFollowUps)
}

// Reset forgets every sample and every unmatched message, leaving the clock
// undisciplined.
func (c *Clock) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.samples = [maxSamples]Sample{}
	c.count = 0
	c.next = 0
	c.fit = fit{}
	clear(c.pendingSyncs)
	clear(c.pendingFollowUps)
}

// timestampFromNanos converts int64 nanoseconds to a PTP timestamp, flooring so
// that times before the epoch stay monotonic.
func timestampFromNanos(ns int64) Timestamp {
	sec, rem := ns/nanosPerSecond, ns%nanosPerSecond
	if rem < 0 {
		sec--
		rem += nanosPerSecond
	}
	return Timestamp{Seconds: uint64(sec), Nanos: uint32(rem)}
}
