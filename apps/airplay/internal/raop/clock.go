package raop

import (
	"sort"
	"time"
)

const (
	// ntpEpochOffsetSeconds converts between the NTP epoch (1900-01-01) and
	// the Unix epoch (1970-01-01).
	ntpEpochOffsetSeconds = 2208988800

	// clockSampleWindow is how many recent exchange results are kept for the
	// quality test.
	clockSampleWindow = 8
	// clockAgreement is the maximum spread for three samples to be considered
	// in agreement.
	clockAgreement = 5 * time.Millisecond
	// clockAgreementSamples is how many agreeing samples make the clock
	// quality flag true.
	clockAgreementSamples = 3
	// maxExchangeRTT bounds a believable round-trip time. rtp.c discards
	// timing replies with a turnaround of 0.2 s or more; a more generous
	// 1 s still rejects timestamp arithmetic that spans an unrelated clock.
	maxExchangeRTT = time.Second
)

// Clock disciplines the sender's NTP clock against the receiver's local clock
// using completed timing exchanges, and converts RTP timestamps to local
// playout times.
//
// The estimate is the standard NTP offset
//
//	offset = ((t2 - t1) + (t3 - t4)) / 2
//
// where t1 is the local send time, t2/t3 are the sender's receive/transmit
// timestamps from the 0xd3 reply and t4 is the local arrival time, with the
// sender's own processing time (t3-t2) removed from the round trip. This
// mirrors the midpoint calculation in rtp.c rtp_timing_receiver():
// remote_time = distant_transmit_time + return_time/2.
//
// A Clock is not safe for concurrent use; the session owns one per
// connection.
type Clock struct {
	sampleRate uint32
	now        func() time.Time

	// samples is the recent offset estimates (remote minus local), oldest
	// first.
	samples []time.Duration
	// offset is the latest accepted estimate.
	offset time.Duration
	// ok reports whether at least three recent samples agree within
	// clockAgreement.
	ok bool

	// anchorRemoteNanos is the sender clock reading, in Unix-epoch
	// nanoseconds, at the anchor exchange; anchorRTP is the RTP timestamp the
	// sender reported for that instant (the localRTP passed to Update).
	anchorRemoteNanos int64
	anchorRTP         uint32
}

// NewClock creates a clock for the given RTP sample rate in Hz. PlayoutTime
// fails until the clock has been disciplined.
func NewClock(sampleRate uint32) *Clock {
	return &Clock{sampleRate: sampleRate, now: time.Now}
}

// Update folds one completed timing exchange into the clock. localNTP is the
// receiver's NTP timestamp at the moment the request was sent and localRTP is
// the receiver's RTP timestamp at that moment; remote is the decoded 0xd3
// reply. It returns the current offset estimate in microseconds (positive
// when the sender's clock is ahead of the receiver's) and the quality flag.
//
// Exchanges that cannot produce a believable sample (zero timestamps, a reply
// that predates the request, a round trip longer than maxExchangeRTT or a
// non-timing-reply Kind) leave the clock unchanged and return the previous
// estimate.
func (c *Clock) Update(localNTP uint64, localRTP uint32, remote TimingPacket) (int64, bool) {
	if c == nil || c.sampleRate == 0 {
		return 0, false
	}
	if remote.Kind != 0 && remote.Kind != KindTimingReply {
		return c.offset.Microseconds(), c.ok
	}
	if localNTP == 0 || remote.NTPRemote == 0 || remote.NTPNow == 0 {
		return c.offset.Microseconds(), c.ok
	}

	now := c.currentTime()
	t1 := ntpToUnixNanos(localNTP)
	t2 := ntpToUnixNanos(remote.NTPRemote)
	t3 := ntpToUnixNanos(remote.NTPNow)
	t4 := now.UnixNano()

	remoteProcessing := t3 - t2
	if remoteProcessing < 0 {
		// A sender clock step can make transmit precede receive; the reference
		// treats this as zero processing time (rtp.c logs "remote processing
		// time greater than return time").
		remoteProcessing = 0
	}
	rtt := t4 - t1 - remoteProcessing
	if rtt < 0 || rtt > int64(maxExchangeRTT) {
		return c.offset.Microseconds(), c.ok
	}

	offset := (t2 - t1) - rtt/2

	c.samples = append(c.samples, time.Duration(offset))
	if len(c.samples) > clockSampleWindow {
		c.samples = append(c.samples[:0], c.samples[1:]...)
	}
	c.offset = time.Duration(offset)
	c.ok = c.quality()

	// The sender's clock read t1+offset when the RTP timestamp was localRTP.
	c.anchorRemoteNanos = t1 + offset
	c.anchorRTP = localRTP

	return c.offset.Microseconds(), c.ok
}

// PlayoutTime returns the local wall-clock time at which rtpTimestamp is
// audible, given the latency to add, using the current offset estimate and
// the most recent anchor. The returned time is derived from c.now(), so it
// carries the same monotonic reading as time.Now(). ok is false until the
// clock is disciplined (three agreeing samples).
//
// The RTP timestamp is projected from the anchor using sampleRate frames per
// second, mapped into the sender's clock via the offset, and converted back
// to local time with the current offset; latency is then added.
func (c *Clock) PlayoutTime(rtpTimestamp uint32, latency time.Duration) (time.Time, bool) {
	if c == nil || c.sampleRate == 0 || !c.ok {
		return time.Time{}, false
	}

	// RTP timestamps are 32-bit and wrap; the signed difference keeps frames
	// just before or after the anchor monotonic.
	deltaFrames := int64(int32(rtpTimestamp - c.anchorRTP))
	frameDelta := time.Duration(deltaFrames * int64(time.Second) / int64(c.sampleRate))

	// The frame's sender-clock instant, then its local-clock instant.
	frameRemoteNanos := c.anchorRemoteNanos + int64(frameDelta)
	frameLocalNanos := frameRemoteNanos - int64(c.offset)

	now := c.currentTime()
	delay := time.Duration(frameLocalNanos-now.UnixNano()) + latency
	return now.Add(delay), true
}

// quality reports whether at least clockAgreementSamples of the retained
// samples lie within clockAgreement of each other.
func (c *Clock) quality() bool {
	if len(c.samples) < clockAgreementSamples {
		return false
	}
	s := append([]time.Duration(nil), c.samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	for i := 0; i+clockAgreementSamples-1 < len(s); i++ {
		if s[i+clockAgreementSamples-1]-s[i] <= clockAgreement {
			return true
		}
	}
	return false
}

func (c *Clock) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// ntpToUnixNanos converts a 64-bit NTP fixed-point timestamp to Unix-epoch
// nanoseconds. The fractional part has a resolution of about 0.23 ns.
func ntpToUnixNanos(ntp uint64) int64 {
	seconds := int64(ntp>>32) - ntpEpochOffsetSeconds
	fraction := int64(uint32(ntp))
	return seconds*int64(time.Second) + (fraction*int64(time.Second))>>32
}
