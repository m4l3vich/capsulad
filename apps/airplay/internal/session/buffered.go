package session

// AirPlay 2 buffered audio (type 103).
//
// Unlike the realtime path, a buffered sender commits its audio to one ordered
// TCP stream: a two-byte length followed by an encrypted RTP packet. There is
// no packet loss to hide, so this path never reorders or evicts audio. It owns
// a bounded, cancellable FIFO, authenticates and decrypts each record through
// rtp.BufferedDecoder, then decodes one of the formats the reference receiver
// actually recognises:
//
//	0x15000000  ALAC, 48 kHz, 24-bit, stereo, 352 samples
//	0x16000000  AAC-LC, 44.1 kHz, stereo, 1024 samples
//	0x17000000  AAC-LC, 48 kHz, stereo, 1024 samples
//
// SSRC 0 is a pause marker and the surround formats are not supported. The
// SSRC is a payload-format code on this path, never the stream's identity;
// authentication of every record with the SETUP shared key is the identity
// check that matters.

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"strings"
	"sync"
	"time"

	"capsulad/apps/airplay/internal/aac"
	"capsulad/apps/airplay/internal/alac"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/rtp"
)

const (
	// bufferedAudioBufferSize is both the queue's hard byte bound and the value
	// reported to a sender as audioBufferSize. It matches the pinned reference
	// receiver's 8 MiB allocation.
	bufferedAudioBufferSize = 8 << 20
	// bufferedWireOverhead counts the framing, RTP header, Poly1305 tag and
	// trailing nonce. The queue bound therefore covers what the TCP reader can
	// retain, not just the decrypted payload.
	bufferedWireOverhead = 2 + 12 + 16 + 8
	// bufferedMaxPackets bounds tiny control-format records that consume almost
	// no bytes. At 352 samples per ALAC frame this is about a minute of audio.
	bufferedMaxPackets = 16384
	// bufferedMaxDeferredFlushes mirrors the reference's small fixed table.
	bufferedMaxDeferredFlushes = 8
	// bufferedMaxSpare caps reusable payload storage. The reader and playout
	// pipeline normally needs only a handful of buffers.
	bufferedMaxSpare = 16
	// bufferedAnchorRetry controls both unresolved anchors and provisional
	// local-fallback reacquisition of the exact named PTP source.
	bufferedAnchorRetry = 100 * time.Millisecond
	// bufferedLateTolerance allows normal scheduling and decode jitter but
	// drops clearly obsolete packets instead of playing them late.
	bufferedLateTolerance = 100 * time.Millisecond

	bufferedSSRCNone         = 0x00000000
	bufferedSSRCALAC48       = 0x15000000
	bufferedSSRCAAC44100     = 0x16000000
	bufferedSSRCAAC48000     = 0x17000000
	bufferedSequenceMask     = 0x7fffff
	bufferedSequenceHalfMask = 1 << 22
)

var errBufferedGeneration = errors.New("buffered audio generation changed")

// bufferedFormat is the decoder and sink format selected by one packet SSRC.
type bufferedFormat struct {
	ssrc       uint32
	codec      string
	sampleRate int
	channels   int
	samples    int
}

// bufferedPacket is one authenticated record. payload aliases queue storage
// owned by bufferedStream; sequence and timestamp are preserved from the wire.
type bufferedPacket struct {
	sequence  uint32
	timestamp uint32
	ssrc      uint32
	payload   []byte
	size      int
}

// bufferedFlushRange is one deferred FLUSHBUFFERED request. Sequence numbers
// select packets, exactly as in the pinned processor. Timestamp bounds are
// validated by the RTSP bridge and accepted by bufferedFlush, but packet
// playout is sequence-driven and does not retain them.
type bufferedFlushRange struct {
	fromSeq  uint32
	untilSeq uint32
}

// bufferedStream owns the ordered input queue, its playout anchor, the active
// decoder and the framework sink. All mutable fields are guarded by mu; sink
// writes are additionally serialized by writeMu so a format change or
// pause cannot overlap a live write.
type bufferedStream struct {
	st *audioStream

	mu   sync.Mutex
	cond *sync.Cond
	done chan struct{}

	closed bool

	packets []bufferedPacket
	head    int
	bytes   int
	spare   [][]byte

	connID            uint64
	connAuthenticated bool

	playRequested  bool
	anchorSet      bool
	// anchorRTP is the sender's validated trim anchor. paceRTP is a separate
	// arrival-time reference that may be rebased after a gap without changing
	// which pre-anchor PCM is admissible.
	anchorRTP      uint32
	anchorMaster   uint64
	anchorTimeline uint64
	paceRTP        uint32
	anchorRate     int
	anchorLocal    time.Time
	anchorResolved bool
	anchorPTP      bool
	anchorNextPTP  time.Time
	pacingPreset   bool
	timingRevision uint64

	generation uint64
	cancel     chan struct{}

	immediateDrop  bool
	immediateUntil uint32
	deferred       []bufferedFlushRange
	flushRevision  uint64
	droppingRange  bool
	pacing         bool
	pacingSequence uint32
	writing        bool
	writingSequence uint32

	hasLastSeq  bool
	lastSeq     uint32
	hasLastTS   bool
	lastTS      uint32
	lastSamples int

	decoder     frameDecoder
	decoderSSRC uint32
	pcm         []byte

	writeMu   sync.Mutex
	sink      Sink
	sinkSSRC  uint32
	published bool
}

func newBufferedStream(st *audioStream) *bufferedStream {
	b := &bufferedStream{
		st:     st,
		done:   make(chan struct{}),
		cancel: make(chan struct{}),
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// formatForSSRC maps only the formats this receiver can actually decode. An
// unknown code, NONE, or an AAC code in a non-FFmpeg build is skipped as a
// control record: it is never turned into noise.
func formatForSSRC(ssrc uint32) (bufferedFormat, bool) {
	switch ssrc {
	case bufferedSSRCALAC48:
		return bufferedFormat{
			ssrc: ssrc, codec: "alac", sampleRate: 48000,
			channels: 2, samples: 352,
		}, true
	case bufferedSSRCNone:
		return bufferedFormat{}, false
	case bufferedSSRCAAC44100:
		if !aac.Available() {
			return bufferedFormat{}, false
		}
		return bufferedFormat{
			ssrc: ssrc, codec: "aac", sampleRate: 44100,
			channels: 2, samples: 1024,
		}, true
	case bufferedSSRCAAC48000:
		if !aac.Available() {
			return bufferedFormat{}, false
		}
		return bufferedFormat{
			ssrc: ssrc, codec: "aac", sampleRate: 48000,
			channels: 2, samples: 1024,
		}, true
	default:
		return bufferedFormat{}, false
	}
}

func (f bufferedFormat) decoder() (frameDecoder, error) {
	switch f.codec {
	case "alac":
		decoder, err := alac.NewDecoder(alac.Config{
			FrameLength: uint32(f.samples),
			BitDepth:    24,
			Channels:    uint8(f.channels),
			SampleRate:  uint32(f.sampleRate),
		})
		if err != nil {
			return nil, err
		}
		return alacFrameDecoder{decoder}, nil
	case "aac":
		decoder, err := aac.New(aac.Config{
			SampleRate: f.sampleRate,
			Channels:   f.channels,
			Profile:    aac.ProfileLC,
		})
		if err != nil {
			return nil, err
		}
		return aacFrameDecoder{decoder}, nil
	default:
		return nil, fmt.Errorf("buffered codec %q is not supported", f.codec)
	}
}

// seq23Compare reports the signed forward distance from a to b in the
// reference's 23-bit sequence space, whose wrap is half a modulus away.
func seq23Compare(a, b uint32) int {
	d := (a - b) & bufferedSequenceMask
	if d >= bufferedSequenceHalfMask {
		return -int(int32((1 << 23) - d))
	}
	return int(d)
}

// sameBufferedPeer keeps the type-103 data channel tied to the control
// connection's sender. IPv4 and its IPv4-mapped IPv6 spelling are equal, and
// a link-local zone is ignored because the daemon supplies the same interface
// for both connections.
func sameBufferedPeer(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	left, err := netip.ParseAddr(strings.Trim(a, "[]"))
	if err == nil {
		right, rightErr := netip.ParseAddr(strings.Trim(b, "[]"))
		if rightErr == nil {
			return left.Unmap().WithZone("") == right.Unmap().WithZone("")
		}
	}
	return strings.EqualFold(strings.Trim(a, "[]"), strings.Trim(b, "[]"))
}

// --- queue -----------------------------------------------------------------

func (b *bufferedStream) pendingLocked() int { return len(b.packets) - b.head }

func (b *bufferedStream) takeSpareLocked(size int) []byte {
	for i := len(b.spare) - 1; i >= 0; i-- {
		buffer := b.spare[i]
		if cap(buffer) < size {
			continue
		}
		copy(b.spare[i:], b.spare[i+1:])
		b.spare[len(b.spare)-1] = nil
		b.spare = b.spare[:len(b.spare)-1]
		return buffer[:0]
	}
	return nil
}

func (b *bufferedStream) keepSpareLocked(payload []byte) {
	if payload == nil || cap(payload) == 0 || len(b.spare) >= bufferedMaxSpare {
		return
	}
	b.spare = append(b.spare, payload[:0])
}

func (b *bufferedStream) recycleLocked(packet *bufferedPacket) {
	if packet.size > 0 {
		b.bytes -= packet.size
	}
	b.keepSpareLocked(packet.payload)
	*packet = bufferedPacket{}
	b.cond.Signal()
}

func (b *bufferedStream) clearQueueLocked() {
	for i := b.head; i < len(b.packets); i++ {
		packet := &b.packets[i]
		if packet.size > 0 {
			b.bytes -= packet.size
		}
		b.keepSpareLocked(packet.payload)
		*packet = bufferedPacket{}
	}
	b.packets = b.packets[:0]
	b.head = 0
}

func (b *bufferedStream) popLocked() bufferedPacket {
	packet := b.packets[b.head]
	b.packets[b.head] = bufferedPacket{}
	b.head++
	if b.head == len(b.packets) {
		b.packets = b.packets[:0]
		b.head = 0
	} else if b.head >= 64 && b.head*2 >= len(b.packets) {
		copy(b.packets, b.packets[b.head:])
		b.packets = b.packets[:len(b.packets)-b.head]
		b.head = 0
	}
	return packet
}

// dropImmediateLocked reports whether an incoming sequence is still inside the
// immediate range. The range is half-open exactly as the pinned processor's
// modulo-23 check treats it: the block at until is preserved. A zero endpoint
// has no timed range; it discards the queue at request time, and later records
// are permitted prefetch for the next anchor.
func (b *bufferedStream) dropImmediateLocked(sequence uint32) bool {
	if !b.immediateDrop {
		return false
	}
	if seq23Compare(sequence, b.immediateUntil) >= 0 {
		b.immediateDrop = false
		return false
	}
	return true
}

func (b *bufferedStream) enqueue(connID uint64, packet rtp.BufferedPacket) {
	sequence := packet.Sequence & bufferedSequenceMask
	size := len(packet.Payload) + bufferedWireOverhead
	_, supported := formatForSSRC(packet.SSRC)

	b.mu.Lock()
	for {
		if b.closed || b.connID != connID {
			b.mu.Unlock()
			return
		}
		if b.dropImmediateLocked(sequence) {
			b.mu.Unlock()
			return
		}
		if b.bytes+size <= bufferedAudioBufferSize && b.pendingLocked() < bufferedMaxPackets {
			break
		}
		b.cond.Wait()
	}
	stored := bufferedPacket{
		sequence:  sequence,
		timestamp: packet.Timestamp,
		ssrc:      packet.SSRC,
		size:      size,
	}
	if supported && len(packet.Payload) > 0 {
		buffer := b.takeSpareLocked(len(packet.Payload))
		stored.payload = append(buffer, packet.Payload...)
	}
	b.packets = append(b.packets, stored)
	b.bytes += size
	b.cond.Signal()
	b.mu.Unlock()
}

func (b *bufferedStream) applyImmediateQueueLocked(until uint32, all bool) {
	source := b.packets[b.head:]
	write := 0
	rangeOpen := !all
	for i := range source {
		packet := source[i]
		drop := all
		if !all && rangeOpen {
			if seq23Compare(packet.sequence, until) < 0 {
				drop = true
			} else {
				// The block at the endpoint is preserved, matching the
				// reference's modulo-23 completion check.
				rangeOpen = false
			}
		}
		if drop {
			if packet.size > 0 {
				b.bytes -= packet.size
			}
			b.keepSpareLocked(packet.payload)
			continue
		}
		b.packets[write] = packet
		write++
	}
	for i := write; i < len(b.packets); i++ {
		b.packets[i] = bufferedPacket{}
	}
	b.packets = b.packets[:write]
	b.head = 0
	b.immediateDrop = !all && rangeOpen
	b.immediateUntil = until
}

func (b *bufferedStream) nextDeferredDropLocked(sequence uint32) (drop bool, entered bool) {
	if len(b.deferred) == 0 {
		b.droppingRange = false
		return false, false
	}
	kept := b.deferred[:0]
	for _, request := range b.deferred {
		// A deferred range is [from, until): the packet at until is decoded.
		if seq23Compare(sequence, request.untilSeq) >= 0 {
			continue
		}
		if seq23Compare(sequence, request.fromSeq) >= 0 {
			drop = true
		}
		kept = append(kept, request)
	}
	b.deferred = kept
	if drop && !b.droppingRange {
		entered = true
	}
	b.droppingRange = drop
	return drop, entered
}

// deferredAtRevision re-evaluates a packet before PCM is committed. A deferred
// request can arrive while this packet is in pace(); flushRevision detects a
// request newer than this packet's dequeue, and the half-open range decides
// whether this packet or only the endpoint is preserved.
func (b *bufferedStream) deferredAtRevision(sequence uint32, revision uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if revision == b.flushRevision {
		return false
	}
	return b.inDeferredRangeLocked(sequence)
}

func (b *bufferedStream) inDeferredRangeLocked(sequence uint32) bool {
	for _, request := range b.deferred {
		if sequenceInFlushRange(sequence, request.fromSeq, request.untilSeq) {
			return true
		}
	}
	return false
}

func (b *bufferedStream) bumpGenerationLocked() {
	if b.closed {
		return
	}
	b.generation++
	close(b.cancel)
	b.cancel = make(chan struct{})
}

func (b *bufferedStream) detachSinkLocked() Sink {
	sink := b.sink
	b.sink = nil
	b.sinkSSRC = 0
	return sink
}

// abortBufferedSink retires a framework playback stream immediately. Abort is
// the local ownership boundary: queued audio cannot survive it and a blocked
// SDK write returns. No caller may write to that sink afterwards.
func abortBufferedSink(sink Sink) {
	if sink != nil {
		_ = sink.Abort()
	}
}

func (b *bufferedStream) publishPausedLocked() {
	if !b.published {
		return
	}
	b.published = false
	if b.st != nil && b.st.s.m.board != nil {
		b.st.s.m.board.SetPlayback(b.st.s.boardGen, media.StatePaused)
	}
}

// --- public stream seam ----------------------------------------------------

// bufferedAnchorPlay installs the sender's SETRATEANCHORTI anchor and
// authorises playout under one lock. Replacing the live timing does not abort
// an already-playing sink; pause, immediate flush, takeover and explicit
// discontinuities remain the abort boundaries.
func (st *audioStream) bufferedAnchorPlay(timelineID uint64, rtpTime uint32, networkNS uint64) {
	if st.buffered == nil {
		return
	}
	st.s.mu.Lock()
	st.s.rate = 1
	st.s.mu.Unlock()
	b := st.buffered
	b.mu.Lock()
	if !b.closed {
		b.installAndPlayLocked(timelineID, rtpTime, networkNS)
	}
	b.mu.Unlock()
}

func (st *audioStream) bufferedPause() {
	if st.buffered == nil {
		return
	}
	st.s.mu.Lock()
	st.s.rate = 0
	st.s.mu.Unlock()
	st.buffered.pause()
}

// bufferedFlush preserves the complete sender range. Both immediate and
// deferred ranges are [from/current, until); the pinned receiver's sequence
// check preserves the until block. untilSeq 0 has no timed endpoint: it clears
// the audio already queued, while records that arrive afterwards remain
// permitted prefetch for the next play anchor. Timestamp bounds are accepted as
// part of the sender's request but sequence numbers select packets, as in the
// reference.
func (st *audioStream) bufferedFlush(fromSeq uint32, _ uint32, untilSeq uint32, _ uint32, immediate bool) {
	if st.buffered == nil {
		return
	}
	if immediate {
		st.s.mu.Lock()
		st.s.rate = 0
		st.s.mu.Unlock()
	}
	st.buffered.flush(fromSeq, untilSeq, immediate)
}

func (b *bufferedStream) installAndPlayLocked(timelineID uint64, rtpTime uint32, masterNS uint64) {
	if b.playRequested && b.anchorSet &&
		b.anchorTimeline == timelineID && b.anchorRTP == rtpTime && b.anchorMaster == masterNS {
		// An identical active anchor is a pinned no-op: do not disturb queued
		// PCM, an in-flight pace/write, or the established local/PTP mapping.
		return
	}

	wasPlaying := b.playRequested
	oldMapped := b.hasValidPacingMappingLocked()
	oldRate := b.anchorRate
	oldPaceRTP := b.paceRTP
	oldPaceLocal := b.anchorLocal

	b.anchorSet = true
	b.anchorRTP = rtpTime
	b.anchorMaster = masterNS
	b.anchorTimeline = timelineID
	b.playRequested = true
	b.hasLastSeq = false
	b.hasLastTS = false
	b.pacing = false
	b.timingRevision++
	if wasPlaying {
		// Timing changes cancel work waiting on the old reference but preserve
		// the live sink, queued PCM and any actual in-flight write bookkeeping.
		close(b.cancel)
		b.cancel = make(chan struct{})
		if oldMapped {
			// Translate the existing mapping to the new RTP reference. This
			// includes a prior changed-anchor preset that has not reached PTP
			// resolution yet, so consecutive refreshes compose from mapped RTP
			// and never lose one interval of continuity.
			delta := int64(int32(rtpTime - oldPaceRTP))
			target := oldPaceLocal.Add(time.Duration(delta) * time.Second / time.Duration(oldRate))
			b.paceRTP = rtpTime
			b.anchorRate = oldRate
			b.anchorLocal = target
			b.anchorPTP = false
			b.anchorNextPTP = time.Time{}
			b.pacingPreset = masterNS != 0
			// With no named PTP timeline the translated mapping is final. With
			// one, resolution first tries that exact source and falls back to
			// this translated mapping if it is absent or undisciplined.
			b.anchorResolved = masterNS == 0
		} else {
			b.anchorResolved = false
			b.anchorPTP = false
			b.anchorLocal = time.Time{}
			b.anchorNextPTP = time.Time{}
			b.pacingPreset = false
		}
	} else {
		// A paused/flushed/reconnected stream starts from a fresh mapping; its
		// sink was retired by that explicit boundary.
		b.anchorResolved = false
		b.anchorPTP = false
		b.anchorLocal = time.Time{}
		b.anchorNextPTP = time.Time{}
		b.pacingPreset = false
		b.writing = false
		b.bumpGenerationLocked()
	}
	b.cond.Broadcast()
}

func (b *bufferedStream) pause() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.playRequested = false
	b.anchorSet = false
	b.anchorResolved = false
	b.anchorPTP = false
	b.anchorLocal = time.Time{}
	b.anchorNextPTP = time.Time{}
	b.pacingPreset = false
	b.pacing = false
	b.writing = false
	b.bumpGenerationLocked()
	sink := b.detachSinkLocked()
	b.publishPausedLocked()
	b.cond.Broadcast()
	b.mu.Unlock()
	abortBufferedSink(sink)
}

func (b *bufferedStream) flush(fromSeq, untilSeq uint32, immediate bool) {
	fromSeq &= bufferedSequenceMask
	untilSeq &= bufferedSequenceMask

	var stale Sink
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	if !immediate {
		if fromSeq == untilSeq || seq23Compare(untilSeq, fromSeq) <= 0 {
			b.mu.Unlock()
			return
		}
		if len(b.deferred) >= bufferedMaxDeferredFlushes {
			copy(b.deferred, b.deferred[1:])
			b.deferred = b.deferred[:bufferedMaxDeferredFlushes-1]
		}
		b.deferred = append(b.deferred, bufferedFlushRange{
			fromSeq: fromSeq, untilSeq: untilSeq,
		})
		b.flushRevision++
		pacingHit := b.pacing && sequenceInFlushRange(b.pacingSequence, fromSeq, untilSeq)
		writingHit := b.writing && sequenceInFlushRange(b.writingSequence, fromSeq, untilSeq)
		if pacingHit || writingHit {
			close(b.cancel)
			b.cancel = make(chan struct{})
		}
		if writingHit {
			stale = b.detachSinkLocked()
		}
		b.cond.Broadcast()
		b.mu.Unlock()
		abortBufferedSink(stale)
		return
	}

	b.playRequested = false
	b.anchorSet = false
	b.anchorResolved = false
	b.anchorPTP = false
	b.anchorLocal = time.Time{}
	b.hasLastSeq = false
	b.hasLastTS = false
	b.pacing = false
	b.writing = false
	b.bumpGenerationLocked()
	stale = b.detachSinkLocked()
	b.publishPausedLocked()
	all := untilSeq == 0
	b.applyImmediateQueueLocked(untilSeq, all)
	b.deferred = b.deferred[:0]
	b.droppingRange = false
	b.cond.Broadcast()
	b.mu.Unlock()
	abortBufferedSink(stale)
}

func (b *bufferedStream) stop() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.done)
	close(b.cancel)
	b.pacing = false
	b.writing = false
	sink := b.detachSinkLocked()
	b.clearQueueLocked()
	b.deferred = nil
	b.published = false
	b.mu.Unlock()
	b.cond.Broadcast()
	if sink != nil {
		_ = sink.Abort()
	}
}

// --- anchor and playout ----------------------------------------------------

// bufferedPTPDeadline converts (master - local) into a monotonic local
// deadline. It rejects only values that cannot be represented safely.
func bufferedPTPDeadline(master uint64, offset int64) (time.Time, bool) {
	localNS := int64(master)
	if offset < 0 {
		if master > uint64(math.MaxInt64+offset) {
			return time.Time{}, false
		}
		localNS -= offset
	} else {
		localNS -= offset
	}
	now := time.Now()
	localNow := now.UnixNano()
	if localNS < math.MinInt64+localNow {
		return time.Time{}, false
	}
	return now.Add(time.Duration(localNS - localNow)), true
}

// resolveAnchorOnce maps the sender's network-time anchor onto local waiting
// time. Only the offset published for this anchor's PTP timeline is
// authoritative; an absent or undisciplined matching source falls back to the
// sender's RTP timestamp at local arrival. The aggregate Offset() belongs to
// independent realtime behavior and is deliberately not consulted here.
func (b *bufferedStream) resolveAnchorOnce(format bufferedFormat) bool {
	b.mu.Lock()
	if b.closed || !b.playRequested || !b.anchorSet {
		b.mu.Unlock()
		return false
	}
	if b.anchorResolved {
		b.mu.Unlock()
		return true
	}
	master := b.anchorMaster
	anchorRTP := b.anchorRTP
	timeline := b.anchorTimeline
	preset := b.pacingPreset
	presetLocal := b.anchorLocal
	presetPaceRTP := b.paceRTP
	presetRate := b.anchorRate
	b.mu.Unlock()

	var resolved time.Time
	fromPTP := false
	if master != 0 && master <= math.MaxInt64 {
		if clock := b.st.s.m.ptpClock(); clock != nil {
			if offset, disciplined := clock.OffsetForClockID(timeline); disciplined {
				if deadline, ok := bufferedPTPDeadline(master, offset); ok {
					resolved = deadline
					fromPTP = true
				}
			}
		}
	}
	if !fromPTP {
		if preset {
			// A changed continuous anchor translated the prior local mapping.
			// Use it if its named PTP source is not ready; never rebuild from
			// now at the old media origin.
			resolved = presetLocal
		} else {
			resolved = time.Now().Add(b.st.s.m.cfg.FrameTarget)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || !b.playRequested || !b.anchorSet || b.anchorResolved {
		return b.anchorResolved
	}
	if b.anchorMaster != master || b.anchorRTP != anchorRTP || b.anchorTimeline != timeline || b.pacingPreset != preset {
		return false
	}
	b.anchorLocal = resolved
	b.paceRTP = anchorRTP
	b.anchorRate = format.sampleRate
	if !fromPTP && preset && presetRate > 0 {
		b.paceRTP = presetPaceRTP
		b.anchorRate = presetRate
	}
	b.anchorResolved = true
	b.anchorPTP = fromPTP
	b.pacingPreset = false
	if fromPTP {
		b.anchorNextPTP = time.Time{}
	} else if master != 0 {
		// Local arrival is provisional when a network-time anchor names a PTP
		// source. Recheck only that source at the next controlled packet boundary.
		b.anchorNextPTP = time.Now().Add(bufferedAnchorRetry)
	} else {
		b.anchorNextPTP = time.Time{}
	}
	return true
}

func waitBufferedTimer(timer *time.Timer, duration time.Duration, cancel <-chan struct{}, done <-chan struct{}) bool {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
	select {
	case <-timer.C:
		return true
	case <-cancel:
		return false
	case <-done:
		return false
	}
}

func (b *bufferedStream) validGenerationLocked(generation uint64) bool {
	return !b.closed && b.playRequested && b.generation == generation
}

func (b *bufferedStream) hasValidPacingMappingLocked() bool {
	return (b.anchorResolved || b.pacingPreset) && b.anchorRate > 0
}

func (b *bufferedStream) beginPacing(sequence uint32, generation, timing, revision uint64) (<-chan struct{}, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.validTimingLocked(generation, timing) {
		return nil, false
	}
	if revision != b.flushRevision && b.inDeferredRangeLocked(sequence) {
		return nil, false
	}
	b.pacing = true
	b.pacingSequence = sequence
	return b.cancel, true
}

func (b *bufferedStream) endPacing(sequence uint32) {
	b.mu.Lock()
	if b.pacingSequence == sequence {
		b.pacing = false
	}
	b.mu.Unlock()
}

func (b *bufferedStream) superseded(generation, timing uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed || b.generation != generation || b.timingRevision != timing
}

func (b *bufferedStream) validTimingLocked(generation, timing uint64) bool {
	return b.validGenerationLocked(generation) && b.timingRevision == timing
}

func (b *bufferedStream) currentCancel() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancel
}

// perhapsReacquirePTP rechecks a fallback anchor at a packet boundary. When the
// named fit becomes disciplined, the current packet and its prefetched queue
// are scheduled against that timeline from this boundary. No unrelated
// aggregate clock is consulted.
func (b *bufferedStream) perhapsReacquirePTP(generation, timing uint64) bool {
	b.mu.Lock()
	if !b.validTimingLocked(generation, timing) {
		b.mu.Unlock()
		return false
	}
	if !b.anchorResolved || b.anchorRate <= 0 || b.anchorPTP || b.anchorMaster == 0 || b.anchorMaster > math.MaxInt64 || time.Now().Before(b.anchorNextPTP) {
		b.mu.Unlock()
		return false
	}
	master := b.anchorMaster
	timeline := b.anchorTimeline
	b.anchorNextPTP = time.Now().Add(bufferedAnchorRetry)
	b.mu.Unlock()

	clock := b.st.s.m.ptpClock()
	if clock == nil {
		return false
	}
	offset, disciplined := clock.OffsetForClockID(timeline)
	if !disciplined {
		return false
	}
	deadline, ok := bufferedPTPDeadline(master, offset)
	if !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.validTimingLocked(generation, timing) || b.anchorMaster != master || b.anchorTimeline != timeline {
		return false
	}
	b.anchorLocal = deadline
	b.paceRTP = b.anchorRTP
	b.anchorResolved = true
	b.anchorPTP = true
	b.anchorNextPTP = time.Time{}
	return true
}

func sequenceInFlushRange(sequence, fromSeq, untilSeq uint32) bool {
	return seq23Compare(sequence, fromSeq) >= 0 && seq23Compare(sequence, untilSeq) < 0
}

// rebaseLocalPacingLocked starts a new arrival-time pacing reference at the
// current packet. The sender's trim anchor is untouched, so pre-anchor PCM
// stays inadmissible while later media does not inherit the skipped interval.
func (b *bufferedStream) rebaseLocalPacingLocked(timestamp uint32, sampleRate int) {
	if b.anchorPTP || !b.hasValidPacingMappingLocked() {
		return
	}
	target := time.Duration(0)
	if b.st != nil && b.st.s != nil && b.st.s.m != nil {
		target = b.st.s.m.cfg.FrameTarget
	}
	b.paceRTP = timestamp
	b.anchorLocal = time.Now().Add(target)
	b.anchorRate = sampleRate
}

// rebasePacingForRateLocked continues a local-fallback stream across a sample
// rate change. The first new-format packet becomes the pacing origin at the
// current wall time; the sender trim anchor is deliberately unchanged.
func (b *bufferedStream) rebasePacingForRateLocked(timestamp uint32, sampleRate int) bool {
	if b.anchorPTP || !b.hasValidPacingMappingLocked() || b.anchorRate == sampleRate {
		return false
	}
	b.paceRTP = timestamp
	b.anchorLocal = time.Now()
	b.anchorRate = sampleRate
	return true
}

func (b *bufferedStream) waitForAnchor(format bufferedFormat, timer *time.Timer, generation, timing uint64, cancel <-chan struct{}) bool {
	for {
		b.mu.Lock()
		if !b.validTimingLocked(generation, timing) {
			b.mu.Unlock()
			return false
		}
		b.mu.Unlock()
		if b.perhapsReacquirePTP(generation, timing) {
			continue
		}
		if b.resolveAnchorOnce(format) {
			return true
		}
		if !waitBufferedTimer(timer, bufferedAnchorRetry, cancel, b.done) {
			return false
		}
	}
}

func (b *bufferedStream) pace(timestamp uint32, timer *time.Timer, generation, timing uint64, cancel <-chan struct{}) bool {
	b.mu.Lock()
	if !b.validTimingLocked(generation, timing) || !b.anchorResolved || b.anchorRate <= 0 {
		b.mu.Unlock()
		return false
	}
	paceRTP := b.paceRTP
	anchorLocal := b.anchorLocal
	rate := b.anchorRate
	b.mu.Unlock()

	delta := int64(int32(timestamp - paceRTP))
	due := anchorLocal.Add(time.Duration(delta) * time.Second / time.Duration(rate))
	wait := time.Until(due)
	if wait > 0 {
		if !waitBufferedTimer(timer, wait, cancel, b.done) {
			return false
		}
		b.mu.Lock()
		valid := b.validTimingLocked(generation, timing)
		b.mu.Unlock()
		return valid
	}
	return -wait <= bufferedLateTolerance
}

func (b *bufferedStream) trimToAnchor(pcm []byte, timestamp uint32, format bufferedFormat, generation, timing uint64) ([]byte, uint32, bool) {
	b.mu.Lock()
	if !b.validTimingLocked(generation, timing) || !b.anchorResolved {
		b.mu.Unlock()
		return nil, timestamp, false
	}
	anchorRTP := b.anchorRTP
	b.mu.Unlock()

	delta := int32(timestamp - anchorRTP)
	if delta >= 0 {
		return pcm, timestamp, true
	}
	frameBytes := format.channels * 2
	frames := len(pcm) / frameBytes
	skip := -int64(delta)
	if skip >= int64(frames) {
		return nil, timestamp, false
	}
	return pcm[skip*int64(frameBytes):], timestamp + uint32(skip), true
}

func (b *bufferedStream) decode(payload []byte) ([]byte, error) {
	b.mu.Lock()
	decoder := b.decoder
	b.mu.Unlock()
	if decoder == nil {
		return nil, errors.New("buffered decoder is not open")
	}
	out, err := decoder.Decode(payload, b.pcm[:0])
	if err != nil {
		return nil, err
	}
	b.pcm = out
	return out, nil
}

// prepareDecoder installs the decoder for a new SSRC format. A format switch
// is a live PCM discontinuity: queued output from the old decoder is aborted
// and the receiver waits for the next valid play anchor or local mapping.
func (b *bufferedStream) prepareDecoder(format bufferedFormat, timestamp uint32, generation uint64) (Sink, error) {
	b.mu.Lock()
	if !b.validGenerationLocked(generation) {
		b.mu.Unlock()
		return nil, errBufferedGeneration
	}
	if b.decoderSSRC == format.ssrc && b.decoder != nil {
		b.mu.Unlock()
		return nil, nil
	}
	sink := b.detachSinkLocked()
	b.hasLastTS = false
	b.mu.Unlock()

	if sink != nil {
		_ = sink.Abort()
	}
	decoder, err := format.decoder()
	if err != nil {
		b.mu.Lock()
		valid := b.validGenerationLocked(generation)
		b.mu.Unlock()
		if !valid {
			return nil, errBufferedGeneration
		}
		return nil, err
	}
	b.mu.Lock()
	if !b.validGenerationLocked(generation) {
		b.mu.Unlock()
		_ = decoder.Close()
		return nil, errBufferedGeneration
	}
	if b.anchorRate != format.sampleRate && !b.rebasePacingForRateLocked(timestamp, format.sampleRate) {
		b.anchorResolved = false
		b.anchorPTP = false
		b.pacingPreset = false
	}
	oldDecoder := b.decoder
	b.decoder = decoder
	b.decoderSSRC = format.ssrc
	b.mu.Unlock()
	if oldDecoder != nil {
		_ = oldDecoder.Close()
	}
	return nil, nil
}

// processPacketLocked applies deferred ranges and detects a live discontinuity
// (lost authenticated record, timestamp gap or format change). The returned
// sink, if any, is aborted by the caller without holding the queue lock.
func (b *bufferedStream) processPacketLocked(packet *bufferedPacket) (drop bool, stale Sink) {
	format, supported := formatForSSRC(packet.ssrc)
	// Deferred ranges already preserve the pacing reference, so entering one
	// does not introduce the skipped media interval as extra wall-clock delay.
	drop, _ = b.nextDeferredDropLocked(packet.sequence)
	if b.hasLastSeq && packet.sequence != (b.lastSeq+1)&bufferedSequenceMask {
		if stale == nil {
			stale = b.detachSinkLocked()
		}
		b.hasLastTS = false
		if supported {
			b.rebaseLocalPacingLocked(packet.timestamp, format.sampleRate)
		}
	}
	if supported && b.hasLastTS {
		expected := b.lastTS + uint32(b.lastSamples)
		if int32(packet.timestamp-expected) != 0 {
			if stale == nil {
				stale = b.detachSinkLocked()
			}
			b.hasLastTS = false
			b.rebaseLocalPacingLocked(packet.timestamp, format.sampleRate)
		}
	}
	b.hasLastSeq = true
	b.lastSeq = packet.sequence
	if supported {
		b.hasLastTS = true
		b.lastTS = packet.timestamp
		b.lastSamples = format.samples
	} else {
		// NONE and unknown formats carry no decodable duration. Do not use
		// their timestamp as an expectation for the next audio block.
		b.hasLastTS = false
	}
	return drop, stale
}

func (b *bufferedStream) noteDecodeFailure(generation, timing uint64) Sink {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.validTimingLocked(generation, timing) {
		// Teardown, a new anchor or an earlier decode already invalidated
		// this packet. Its error must not touch a newer playback generation.
		return nil
	}
	sink := b.detachSinkLocked()
	b.hasLastTS = false
	return sink
}

func (b *bufferedStream) writePCM(format bufferedFormat, pcm []byte, sequence uint32, generation, timing, revision uint64) (bool, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.deferredAtRevision(sequence, revision) {
		return false, nil
	}

	b.mu.Lock()
	if !b.validTimingLocked(generation, timing) {
		b.mu.Unlock()
		return false, nil
	}
	sink := b.sink
	var stale Sink
	if sink != nil && b.sinkSSRC != format.ssrc {
		stale = sink
		sink = nil
		b.sink = nil
		b.sinkSSRC = 0
	}
	b.mu.Unlock()
	abortBufferedSink(stale)
	if sink == nil {
		if b.st.s.m.sinks == nil {
			if b.superseded(generation, timing) {
				return false, nil
			}
			return false, errors.New("no playback sink available")
		}
		opened, err := b.st.s.m.sinks.OpenSink(format.sampleRate, format.channels)
		if err != nil {
			if b.superseded(generation, timing) {
				return false, nil
			}
			return false, err
		}
		b.mu.Lock()
		if !b.validTimingLocked(generation, timing) || b.inDeferredRangeLocked(sequence) {
			b.mu.Unlock()
			abortBufferedSink(opened)
			return false, nil
		}
		b.sink = opened
		b.sinkSSRC = format.ssrc
		sink = opened
		b.mu.Unlock()
	}

	b.mu.Lock()
	if !b.validTimingLocked(generation, timing) || b.inDeferredRangeLocked(sequence) {
		b.mu.Unlock()
		return false, nil
	}
	b.writing = true
	b.writingSequence = sequence
	b.mu.Unlock()

	err := sink.Write(pcm)

	b.mu.Lock()
	if b.writingSequence == sequence {
		b.writing = false
	}
	if b.sink != sink {
		// A pause, flush or generation boundary already retired this exact
		// stream; never reattach it.
		sink = nil
	}
	generationValid := b.validGenerationLocked(generation)
	inRange := b.inDeferredRangeLocked(sequence)
	if !generationValid || inRange {
		if sink != nil {
			b.sink = nil
			b.sinkSSRC = 0
		}
		b.mu.Unlock()
		abortBufferedSink(sink)
		return false, nil
	}
	if err != nil {
		if sink != nil {
			b.sink = nil
			b.sinkSSRC = 0
		}
		b.mu.Unlock()
		abortBufferedSink(sink)
		return false, err
	}
	// A successful write may predate a routine rate-one anchor refresh. The
	// pinned player continues across that refresh, so keep the sink and allow
	// this already-committed PCM; the next packet uses the new timing epoch.
	first := !b.published
	b.published = true
	if b.st.s.m.board != nil {
		b.st.s.m.board.SetPlayback(b.st.s.boardGen, media.StatePlaying)
	}
	b.mu.Unlock()

	b.st.mu.Lock()
	b.st.frames++
	b.st.lastPop = time.Now()
	b.st.mu.Unlock()
	b.st.s.noteEnvelope(pcm)
	if first {
		b.st.s.startRing()
	}
	return true, nil
}

func (b *bufferedStream) playLoop() {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	defer func() {
		b.mu.Lock()
		sink := b.detachSinkLocked()
		decoder := b.decoder
		b.decoder = nil
		b.decoderSSRC = 0
		b.clearQueueLocked()
		b.mu.Unlock()
		if sink != nil {
			_ = sink.Abort()
		}
		if decoder != nil {
			_ = decoder.Close()
		}
	}()

	decodeFailures := 0
	for {
		b.mu.Lock()
		for !b.closed && (!b.playRequested || b.pendingLocked() == 0) {
			b.cond.Wait()
		}
		if b.closed {
			b.mu.Unlock()
			return
		}
		packet := b.popLocked()
		generation := b.generation
		drop, stale := b.processPacketLocked(&packet)
		timing := b.timingRevision
		revision := b.flushRevision
		b.mu.Unlock()
		abortBufferedSink(stale)

		format, supported := formatForSSRC(packet.ssrc)
		if drop || !supported || len(packet.payload) == 0 {
			b.recycle(packet)
			continue
		}

		stale, err := b.prepareDecoder(format, packet.timestamp, generation)
		abortBufferedSink(stale)
		if err != nil {
			b.recycle(packet)
			if errors.Is(err, errBufferedGeneration) {
				continue
			}
			b.st.noteError(err)
			b.st.s.close("buffered audio decoder: " + err.Error())
			return
		}
		cancel := b.currentCancel()
		if b.deferredAtRevision(packet.sequence, revision) {
			b.recycle(packet)
			continue
		}
		if !b.waitForAnchor(format, timer, generation, timing, cancel) {
			b.recycle(packet)
			continue
		}
		if b.deferredAtRevision(packet.sequence, revision) {
			b.recycle(packet)
			continue
		}
		pcm, err := b.decode(packet.payload)
		if err != nil {
			decodeFailures++
			if decodeFailures == 1 || decodeFailures%128 == 0 {
				b.st.s.m.logf("session %s: buffered decode: %v", b.st.s.peer.Address, err)
			}
			stale := b.noteDecodeFailure(generation, timing)
			abortBufferedSink(stale)
			b.recycle(packet)
			continue
		}
		decodeFailures = 0
		if b.deferredAtRevision(packet.sequence, revision) {
			b.recycle(packet)
			continue
		}
		if format.channels <= 0 || len(pcm) == 0 || len(pcm)%(format.channels*2) != 0 {
			b.recycle(packet)
			continue
		}
		pcm, timestamp, ok := b.trimToAnchor(pcm, packet.timestamp, format, generation, timing)
		if !ok {
			b.recycle(packet)
			continue
		}
		cancel, ok = b.beginPacing(packet.sequence, generation, timing, revision)
		if !ok {
			b.recycle(packet)
			continue
		}
		paced := b.pace(timestamp, timer, generation, timing, cancel)
		b.endPacing(packet.sequence)
		if !paced {
			b.recycle(packet)
			continue
		}
		if _, err := b.writePCM(format, pcm, packet.sequence, generation, timing, revision); err != nil {
			if b.superseded(generation, timing) {
				b.recycle(packet)
				continue
			}
			b.st.noteError(err)
			b.st.s.close("buffered playback: " + err.Error())
			b.recycle(packet)
			return
		}
		b.recycle(packet)
	}
}

func (b *bufferedStream) recycle(packet bufferedPacket) {
	b.mu.Lock()
	b.recycleLocked(&packet)
	b.mu.Unlock()
}

// --- TCP data channel ------------------------------------------------------

// beginConnection claims the next data-connection generation. A replacement
// invalidates playout, drops queued/old PCM, and requires a fresh play anchor;
// this prevents a reconnecting or stale socket from mixing generations.
func (b *bufferedStream) beginConnection(reset bool) (uint64, Sink, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, nil, false
	}
	if reset && b.connAuthenticated {
		// The live reader authenticated before this accept could claim the
		// channel. Reject the replacement under the same lock.
		return 0, nil, false
	}
	b.connID++
	b.connAuthenticated = false
	id := b.connID
	if !reset {
		return id, nil, true
	}
	b.playRequested = false
	b.anchorSet = false
	b.anchorResolved = false
	b.anchorPTP = false
	b.anchorLocal = time.Time{}
	b.hasLastSeq = false
	b.hasLastTS = false
	b.pacing = false
	b.writing = false
	b.bumpGenerationLocked()
	sink := b.detachSinkLocked()
	b.publishPausedLocked()
	b.clearQueueLocked()
	b.deferred = b.deferred[:0]
	b.droppingRange = false
	b.immediateDrop = false
	b.cond.Broadcast()
	return id, sink, true
}

func (b *bufferedStream) currentConnection(id uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.closed && b.connID == id
}

func (b *bufferedStream) noteAuthenticated(id uint64) {
	b.mu.Lock()
	if !b.closed && b.connID == id {
		b.connAuthenticated = true
	}
	b.mu.Unlock()
}

func (b *bufferedStream) authenticatedFor(id uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connAuthenticated && b.connID == id
}

func (b *bufferedStream) endConnection(id uint64) {
	b.mu.Lock()
	if b.connID == id {
		b.connAuthenticated = false
	}
	b.mu.Unlock()
}

func (st *audioStream) serveBufferedDataConnections(listener Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !st.isClosed() {
				st.noteError(fmt.Errorf("buffered audio listener: %w", err))
				st.s.close("buffered audio listener: " + err.Error())
			}
			return
		}
		st.mu.Lock()
		previous := st.dataConn
		st.mu.Unlock()
		peer := conn.Peer()
		if !sameBufferedPeer(st.s.peer.Address, peer.Address) {
			st.s.m.logf("session %s: refusing buffered audio channel from unrelated peer %s", st.s.peer.Address, peer.Address)
			_ = conn.Close()
			continue
		}
		decoder, err := rtp.NewBufferedDecoder(st.spec.sharedKey)
		if err != nil {
			_ = conn.Close()
			st.noteError(fmt.Errorf("buffered audio decoder: %w", err))
			st.s.close("buffered audio decoder: " + err.Error())
			return
		}
		id, stale, claimed := st.buffered.beginConnection(previous != nil)
		abortBufferedSink(stale)
		if !claimed || !st.buffered.currentConnection(id) {
			_ = conn.Close()
			if !claimed {
				st.s.m.logf("session %s: refusing buffered audio channel from %s while the authenticated channel is live", st.s.peer.Address, peer.Address)
				continue
			}
			return
		}
		st.mu.Lock()
		if st.closed {
			st.mu.Unlock()
			_ = conn.Close()
			return
		}
		st.dataConn = conn
		st.mu.Unlock()
		if previous != nil {
			_ = previous.Close()
		}
		st.s.m.logf("session %s: buffered audio channel from %s", st.s.peer.Address, peer.Address)
		go st.readBufferedData(conn, id, decoder)
	}
}

func (st *audioStream) readBufferedData(conn Stream, id uint64, decoder *rtp.BufferedDecoder) {
	defer func() {
		st.buffered.endConnection(id)
		st.mu.Lock()
		if st.dataConn == conn {
			st.dataConn = nil
		}
		st.mu.Unlock()
		_ = conn.Close()
	}()
	badAuth := 0
	for {
		packet, err := decoder.Read(conn)
		if err != nil {
			if errors.Is(err, rtp.ErrBufferedAuthentication) {
				badAuth++
				if badAuth == 1 || badAuth%256 == 0 {
					st.s.m.logf("session %s: dropped %d unauthenticated buffered audio record(s)", st.s.peer.Address, badAuth)
				}
				if !st.buffered.currentConnection(id) {
					return
				}
				continue
			}
			if !st.buffered.currentConnection(id) {
				return
			}
			if !st.buffered.authenticatedFor(id) {
				// Framing/I/O failures before one authenticated record are
				// untrusted input. Discard this connection; never let it end
				// the authorized RTSP session.
				return
			}
			if !st.isClosed() {
				if !errors.Is(err, io.EOF) {
					st.s.m.logf("session %s: buffered audio channel ended: %v", st.s.peer.Address, err)
				}
				st.noteError(fmt.Errorf("buffered audio channel: %w", err))
				st.s.close("buffered audio channel ended")
			}
			return
		}
		if !st.buffered.currentConnection(id) {
			return
		}
		st.buffered.noteAuthenticated(id)
		st.s.touch()
		st.s.markAudio()
		st.buffered.enqueue(id, packet)
	}
}


