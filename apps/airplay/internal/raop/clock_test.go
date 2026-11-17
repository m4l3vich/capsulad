package raop

import (
	"testing"
	"time"
)

// unixNanosToNTP is the inverse of ntpToUnixNanos, used to build synthetic
// exchanges with distinguishable sender and receiver clocks.
func unixNanosToNTP(ns int64) uint64 {
	sec := ns / int64(time.Second)
	rem := ns % int64(time.Second)
	if rem < 0 {
		sec--
		rem += int64(time.Second)
	}
	ntpSec := uint64(sec + ntpEpochOffsetSeconds)
	frac := (uint64(rem) << 32) / uint64(time.Second)
	return ntpSec<<32 | frac
}

func TestClockDiscipline(t *testing.T) {
	const (
		sampleRate  = uint32(44100)
		fixedOffset = int64(2_500_000_000) // sender clock is 2.5 s ahead
		driftPPM    = 500.0                // sender clock runs 500 ppm fast
		oneWay      = int64(time.Millisecond)
		base        = int64(1_700_000_000_000_000_000)
	)

	// offsetAt is the sender-minus-receiver offset at a local instant.
	offsetAt := func(localUnixNanos int64) int64 {
		elapsed := localUnixNanos - base
		return fixedOffset + int64(float64(elapsed)*driftPPM/1e6)
	}

	clock := NewClock(sampleRate)
	var (
		anchorRTP   uint32
		anchorLocal int64
		lastArrival int64
	)

	for i := range 3 {
		t1 := base + int64(i)*int64(100*time.Millisecond)
		// The sender receives the request one way later and replies
		// immediately on its own, drifted clock.
		t2 := t1 + oneWay + offsetAt(t1+oneWay)
		t3 := t2
		t4 := t1 + 2*oneWay

		now := time.Unix(0, t4)
		clock.now = func() time.Time { return now }
		localRTP := uint32(1000 + i*4410)

		got, ok := clock.Update(unixNanosToNTP(t1), localRTP, TimingPacket{
			Kind:      KindTimingReply,
			NTPRemote: unixNanosToNTP(t2),
			NTPNow:    unixNanosToNTP(t3),
		})

		wantMicros := offsetAt(t1+oneWay) / 1000
		if diff := got - wantMicros; diff < -1000 || diff > 1000 {
			t.Errorf("exchange %d: offset = %d us, want %d us (within 1 ms)", i+1, got, wantMicros)
		}
		if i < 2 && ok {
			t.Errorf("exchange %d: ok = true before three agreeing samples", i+1)
		}
		if i == 2 && !ok {
			t.Error("exchange 3: ok = false with three agreeing samples")
		}

		anchorRTP = localRTP
		anchorLocal = t1
		lastArrival = t4
	}

	// PlayoutTime maps the anchor and later RTP timestamps onto the local
	// clock, adding the requested latency.
	anchorPlayout, ok := clock.PlayoutTime(anchorRTP, 2*time.Second)
	if !ok {
		t.Fatal("PlayoutTime not ok after discipline")
	}
	wantAnchor := time.Unix(0, anchorLocal).Add(2 * time.Second)
	if diff := anchorPlayout.Sub(wantAnchor); diff < -2*time.Microsecond || diff > 2*time.Microsecond {
		t.Errorf("playout at anchor = %v, want %v (diff %v)", anchorPlayout, wantAnchor, diff)
	}
	if lastArrival <= anchorLocal {
		t.Fatalf("test setup: arrival %d is not after send %d", lastArrival, anchorLocal)
	}

	later, ok := clock.PlayoutTime(anchorRTP+44100, 2*time.Second)
	if !ok {
		t.Fatal("PlayoutTime not ok for the later timestamp")
	}
	if diff := later.Sub(anchorPlayout); diff != time.Second {
		t.Errorf("playout after 44100 frames is %v later, want exactly 1s", diff)
	}

	// Monotonicity over a run of increasing RTP timestamps.
	prev := anchorPlayout
	for i := 1; i <= 64; i++ {
		next, ok := clock.PlayoutTime(anchorRTP+uint32(i*441), 2*time.Second)
		if !ok {
			t.Fatalf("PlayoutTime(%d) not ok", i)
		}
		if !next.After(prev) {
			t.Fatalf("playout is not monotonic at step %d: %v <= %v", i, next, prev)
		}
		prev = next
	}

	// 32-bit wrap: a timestamp before the anchor must map before the anchor
	// playout, not ~26 hours later.
	before, ok := clock.PlayoutTime(anchorRTP-4410, 2*time.Second)
	if !ok {
		t.Fatal("PlayoutTime before the anchor not ok")
	}
	if !before.Before(anchorPlayout) {
		t.Errorf("playout before the anchor = %v, not before %v", before, anchorPlayout)
	}
}

func TestClockUndisciplined(t *testing.T) {
	clock := NewClock(44100)
	if _, ok := clock.PlayoutTime(1234, 0); ok {
		t.Error("PlayoutTime succeeded before any exchange")
	}

	// One good exchange is not enough for the quality flag.
	now := time.Unix(0, 1_700_000_000_000_000_000)
	clock.now = func() time.Time { return now }
	t1 := now.UnixNano()
	_, ok := clock.Update(unixNanosToNTP(t1), 0, TimingPacket{
		Kind:      KindTimingReply,
		NTPRemote: unixNanosToNTP(t1 + int64(time.Millisecond)),
		NTPNow:    unixNanosToNTP(t1 + int64(time.Millisecond)),
	})
	if ok {
		t.Error("ok = true after one sample")
	}
	if _, ok := clock.PlayoutTime(0, 0); ok {
		t.Error("PlayoutTime succeeded after one sample")
	}
}

func TestClockRejectsBadExchanges(t *testing.T) {
	const base = int64(1_700_000_000_000_000_000)

	clock := NewClock(44100)
	now := time.Unix(0, base+int64(2*time.Millisecond))
	clock.now = func() time.Time { return now }
	t1 := base
	good := TimingPacket{
		Kind:      KindTimingReply,
		NTPRemote: unixNanosToNTP(t1 + int64(51*time.Millisecond)), // sender is 50 ms ahead
		NTPNow:    unixNanosToNTP(t1 + int64(51*time.Millisecond)),
	}
	offsetBefore, okBefore := clock.Update(unixNanosToNTP(t1), 0, good)
	if offsetBefore == 0 {
		t.Fatal("test setup: good exchange produced a zero offset")
	}

	cases := []struct {
		name   string
		local  uint64
		remote TimingPacket
		now    time.Time
	}{
		{"zero local timestamp", 0, good, now},
		{"zero receive timestamp", unixNanosToNTP(t1), TimingPacket{Kind: KindTimingReply, NTPNow: good.NTPNow}, now},
		{"zero transmit timestamp", unixNanosToNTP(t1), TimingPacket{Kind: KindTimingReply, NTPRemote: good.NTPRemote}, now},
		{"not a timing reply", unixNanosToNTP(t1), TimingPacket{Kind: KindSync, NTPRemote: good.NTPRemote, NTPNow: good.NTPNow}, now},
		{"reply predates request", unixNanosToNTP(t1), good, time.Unix(0, t1-int64(time.Second))},
		{"round trip too long", unixNanosToNTP(t1), good, time.Unix(0, t1+int64(2*time.Second))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock.now = func() time.Time { return tc.now }
			gotOffset, gotOK := clock.Update(tc.local, 0, tc.remote)
			if gotOffset != offsetBefore || gotOK != okBefore {
				t.Errorf("Update = (%d, %v), want the unchanged (%d, %v)", gotOffset, gotOK, offsetBefore, okBefore)
			}
		})
	}
}

func TestClockZeroSampleRate(t *testing.T) {
	clock := NewClock(0)
	now := time.Unix(0, 1_700_000_000_000_000_000)
	clock.now = func() time.Time { return now }
	if _, ok := clock.Update(unixNanosToNTP(now.UnixNano()), 0, TimingPacket{
		Kind:      KindTimingReply,
		NTPRemote: unixNanosToNTP(now.UnixNano()),
		NTPNow:    unixNanosToNTP(now.UnixNano()),
	}); ok {
		t.Error("Update with sample rate 0 reported ok")
	}
	if _, ok := clock.PlayoutTime(0, 0); ok {
		t.Error("PlayoutTime with sample rate 0 reported ok")
	}

	var nilClock *Clock
	if _, ok := nilClock.Update(0, 0, TimingPacket{}); ok {
		t.Error("nil Clock Update reported ok")
	}
	if _, ok := nilClock.PlayoutTime(0, 0); ok {
		t.Error("nil Clock PlayoutTime reported ok")
	}
}
