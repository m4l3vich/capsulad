package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/rtp"
)

// bufferedALAC352Frame builds one real verbatim ALAC CPE frame at the buffered
// path's 48 kHz / 24-bit / stereo block length. The pinned buffered SSRC uses
// 352-sample blocks, unlike the repository's legacy 4096-sample fixtures.
func bufferedALAC352Frame(seed int) []byte {
	const samples = 352
	writer := bufferedBitWriter{data: make([]byte, 0, 2300)}
	writer.write(1, 3)  // CPE element
	writer.write(0, 4)  // element instance
	writer.write(0, 12) // reserved / unused
	writer.write(0, 1)  // no explicit sample count
	writer.write(0, 2)  // no extra low-order bytes
	writer.write(1, 1)  // verbatim, not compressed prediction
	for i := range samples {
		for channel := range 2 {
			value := int32((i*97 + seed*101 + channel*7919) & 0xffffff)
			if value&0x800000 != 0 {
				value -= 1 << 24
			}
			writer.write(uint32(value)&0xffffff, 24)
		}
	}
	writer.write(7, 3) // END element
	return writer.finish()
}

func bufferedALAC352Frames(count int) [][]byte {
	frames := make([][]byte, count)
	for i := range frames {
		frames[i] = bufferedALAC352Frame(i + 1)
	}
	return frames
}

type bufferedBitWriter struct {
	data  []byte
	avail uint
}

func (w *bufferedBitWriter) write(value uint32, count uint) {
	for bit := count; bit > 0; bit-- {
		if w.avail == 0 {
			w.data = append(w.data, 0)
			w.avail = 8
		}
		shift := bit - 1
		if value&(1<<shift) != 0 {
			w.data[len(w.data)-1] |= 1 << (w.avail - 1)
		}
		w.avail--
	}
}

func (w *bufferedBitWriter) finish() []byte { return w.data }

func TestBufferedALAC352FrameDecodes(t *testing.T) {
	format, ok := formatForSSRC(bufferedSSRCALAC48)
	if !ok {
		t.Fatal("buffered ALAC format is not recognised")
	}
	decoder, err := format.decoder()
	if err != nil {
		t.Fatalf("open decoder: %v", err)
	}
	defer decoder.Close()
	pcm, err := decoder.Decode(bufferedALAC352Frame(1), nil)
	if err != nil {
		t.Fatalf("decode verbatim frame: %v", err)
	}
	if got, want := len(pcm), 352*2*2; got != want {
		t.Fatalf("decoded PCM = %d bytes, want %d", got, want)
	}
	nonZero := false
	for _, value := range pcm {
		if value != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Fatal("decoded verbatim frame is all zero")
	}
}

func TestBufferedPeerIdentityNormalizesMappedIPv4(t *testing.T) {
	cases := []struct {
		control, data string
		want          bool
	}{
		{"192.0.2.10", "::ffff:192.0.2.10", true},
		{"fd00::10", "fd00::10%en0", true},
		{"192.0.2.10", "192.0.2.11", false},
		{"fd00::10", "fd00::11", false},
		{"", "192.0.2.10", false},
	}
	for _, test := range cases {
		if got := sameBufferedPeer(test.control, test.data); got != test.want {
			t.Errorf("sameBufferedPeer(%q, %q) = %v, want %v", test.control, test.data, got, test.want)
		}
	}
}

func TestBufferedSequenceWrapComparison(t *testing.T) {
	cases := []struct {
		a, b uint32
		want int
	}{
		{10, 10, 0},
		{10, 11, -1},
		{11, 10, 1},
		{0, bufferedSequenceMask, 1},
		{bufferedSequenceMask, 0, -1},
		{bufferedSequenceMask - 2, 2, -5},
	}
	for _, test := range cases {
		if got := seq23Compare(test.a, test.b); got != test.want {
			t.Errorf("seq23Compare(%#x, %#x) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestBufferedImmediateFlushPreservesEndpointAndLaterPrefetch(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.connID = 1
	for sequence := uint32(10); sequence <= 12; sequence++ {
		stream.enqueue(1, rtp.BufferedPacket{Sequence: sequence, Timestamp: sequence * 352})
	}
	stream.flush(0, 11, true)
	if stream.pendingLockedForTest() != 2 {
		t.Fatalf("buffered queue after immediate flush has %d packets, want 2", stream.pendingLockedForTest())
	}
	packet := stream.popForTest()
	if packet.sequence != 11 {
		t.Fatalf("buffered queue preserved sequence %d, want endpoint 11", packet.sequence)
	}
}

func TestBufferedZeroEndpointFlushClearsQueueButAllowsPrefetch(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.connID = 1
	for sequence := uint32(1); sequence <= 3; sequence++ {
		stream.enqueue(1, rtp.BufferedPacket{Sequence: sequence, Timestamp: sequence * 352})
	}
	stream.flush(0, 0, true)
	if stream.pendingLockedForTest() != 0 {
		t.Fatalf("zero-endpoint flush left %d queued packets", stream.pendingLockedForTest())
	}
	stream.enqueue(1, rtp.BufferedPacket{Sequence: 20, Timestamp: 20 * 352})
	if stream.pendingLockedForTest() != 1 {
		t.Fatalf("zero-endpoint flush dropped post-flush prefetch")
	}
}

func TestBufferedTrimHandlesTimestampWrap(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorResolved = true
	stream.anchorRTP = 0x10
	stream.generation = 0
	stream.mu.Unlock()

	format := bufferedFormat{channels: 2}
	pcm := make([]byte, 352*format.channels*2)
	trimmed, timestamp, ok := stream.trimToAnchor(pcm, 0xfffffff0, format, 0, 0)
	if !ok {
		t.Fatal("timestamp-wrap frame was dropped instead of trimmed")
	}
	if timestamp != 0x10 {
		t.Fatalf("trimmed timestamp = %#x, want %#x", timestamp, uint32(0x10))
	}
	if got, want := len(trimmed), (352-32)*format.channels*2; got != want {
		t.Fatalf("trimmed PCM = %d bytes, want %d", got, want)
	}
}

func TestBufferedDeferredFlushRangeIsHalfOpenAcrossWrap(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.flush(bufferedSequenceMask, 1, false)
	cases := []struct {
		sequence uint32
		drop     bool
		entered  bool
	}{
		{bufferedSequenceMask - 1, false, false},
		{bufferedSequenceMask, true, true},
		{0, true, false},
		{1, false, false},
	}
	for _, test := range cases {
		drop, entered := stream.nextDeferredDropLocked(test.sequence)
		if drop != test.drop || entered != test.entered {
			t.Errorf("sequence %#x: drop=%v entered=%v, want drop=%v entered=%v",
				test.sequence, drop, entered, test.drop, test.entered)
		}
	}
}

func TestBufferedQueueBackpressureUnblocksOnStop(t *testing.T) {
	stream := newBufferedStream(nil)
	stream.connID = 1
	stream.mu.Lock()
	stream.bytes = bufferedAudioBufferSize
	stream.mu.Unlock()
	returned := make(chan struct{})
	go func() {
		stream.enqueue(1, rtp.BufferedPacket{Sequence: 1, Timestamp: 1})
		close(returned)
	}()
	stream.stop()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("buffered enqueue did not unblock on stop")
	}
}

func TestBufferedDecodeFailureAfterStopDoesNotCloseAgain(t *testing.T) {
	stream := newBufferedStream(nil)
	stream.mu.Lock()
	stream.playRequested = true
	stream.mu.Unlock()
	stream.stop()
	stream.noteDecodeFailure(0, 0)
	stream.mu.Lock()
	stream.bumpGenerationLocked()
	stream.mu.Unlock()
}

func TestBufferedDeferredFlushPreservesPacingReference(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	pacing := time.Now()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorResolved = true
	stream.anchorRTP = 1000
	stream.paceRTP = 1000
	stream.anchorRate = 48000
	stream.anchorLocal = pacing
	stream.mu.Unlock()

	stream.flush(3, 4, false)
	inRange := bufferedPacket{sequence: 3, timestamp: 700, ssrc: bufferedSSRCALAC48}
	drop, sink := stream.processPacketLocked(&inRange)
	if sink != nil {
		_ = sink.Abort()
	}
	if !drop {
		t.Fatal("in-range block was not selected for deferral")
	}
	endpoint := bufferedPacket{sequence: 4, timestamp: 1052, ssrc: bufferedSSRCALAC48}
	drop, sink = stream.processPacketLocked(&endpoint)
	if sink != nil {
		_ = sink.Abort()
	}
	if drop {
		t.Fatal("deferred endpoint was dropped")
	}
	stream.mu.Lock()
	gotPacing := stream.anchorLocal
	gotRTP := stream.paceRTP
	stream.mu.Unlock()
	if !gotPacing.Equal(pacing) || gotRTP != 1000 {
		t.Fatalf("deferred flush remapped pacing to %v/%d", gotPacing, gotRTP)
	}
}

func TestBufferedConcurrentStopAndDecodeFailure(t *testing.T) {
	for range 100 {
		stream := newBufferedStream(nil)
		stream.mu.Lock()
		stream.playRequested = true
		stream.mu.Unlock()
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			stream.noteDecodeFailure(0, 0)
		}()
		go func() {
			defer wait.Done()
			<-start
			stream.stop()
		}()
		close(start)
		wait.Wait()
	}
}

func TestBufferedDeferredFlushCancelsOnlyInRangePacing(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.mu.Lock()
	stream.playRequested = true
	stream.pacing = true
	stream.pacingSequence = 5
	cancel := stream.cancel
	stream.mu.Unlock()

	stream.flush(5, 6, false)
	select {
	case <-cancel:
	default:
		t.Fatal("deferred flush did not cancel the in-flight in-range packet")
	}
	if !stream.deferredAtRevision(5, 0) {
		t.Fatal("deferred flush did not report its in-range packet")
	}
	if stream.deferredAtRevision(6, 0) {
		t.Fatal("deferred flush dropped its half-open endpoint")
	}
	if stream.deferredAtRevision(4, 0) {
		t.Fatal("deferred flush dropped PCM before its range")
	}
}

func TestBufferedDiscontinuityKeepsSenderTrimAnchor(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorRTP = 1000
	stream.anchorResolved = true
	stream.anchorRate = 48000
	stream.paceRTP = 1000
	stream.hasLastSeq = true
	stream.lastSeq = 5
	packet := bufferedPacket{sequence: 7, timestamp: 700, ssrc: bufferedSSRCALAC48}
	drop, sink := stream.processPacketLocked(&packet)
	stream.mu.Unlock()
	if drop {
		t.Fatal("unexpected deferred drop")
	}
	if sink != nil {
		_ = sink.Abort()
	}
	if stream.anchorRTP != 1000 {
		t.Fatalf("sequence gap replaced sender trim anchor with %d", stream.anchorRTP)
	}
	if !stream.anchorResolved {
		t.Fatal("local pacing mapping was not retained/rebased")
	}
	if stream.paceRTP != 700 {
		t.Fatalf("pacing reference = %d, want rebased packet timestamp 700", stream.paceRTP)
	}
}

type bufferedBusySink struct {
	mu      sync.Mutex
	aborted bool
}

func (s *bufferedBusySink) Write([]byte) error { return errors.New("busy") }
func (s *bufferedBusySink) Flush() error       { return nil }
func (s *bufferedBusySink) Close() error       { return nil }
func (s *bufferedBusySink) Abort() error {
	s.mu.Lock()
	s.aborted = true
	s.mu.Unlock()
	return nil
}

type bufferedBusySinks struct{ sink *bufferedBusySink }

func (f bufferedBusySinks) OpenSink(int, int) (Sink, error) { return f.sink, nil }

func TestBufferedWriteFailureAbortsSink(t *testing.T) {
	sink := &bufferedBusySink{}
	mgr := &Manager{cfg: Config{FrameTarget: time.Second}, sinks: bufferedBusySinks{sink: sink}}
	session := &Session{m: mgr}
	stream := newBufferedStream(&audioStream{s: session})
	defer stream.stop()
	stream.playRequested = true
	format := bufferedFormat{ssrc: bufferedSSRCALAC48, codec: "alac", sampleRate: 48000, channels: 2}
	if _, err := stream.writePCM(format, make([]byte, 8), 1, 0, 0, 0); err == nil {
		t.Fatal("busy sink write did not report an error")
	}
	if stream.sink != nil {
		t.Fatal("failed sink handle was retained")
	}
	sink.mu.Lock()
	aborted := sink.aborted
	sink.mu.Unlock()
	if !aborted {
		t.Fatal("failed sink was not aborted")
	}
}

type bufferedProvisionalClock struct {
	mu          sync.Mutex
	disciplined bool
	offset      int64
}

func (c *bufferedProvisionalClock) Offset() (int64, bool) {
	return 3_000_000_000, true
}

func (c *bufferedProvisionalClock) OffsetForClockID(uint64) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset, c.disciplined
}

func TestBufferedPredisciplinedAnchorDoesNotStarve(t *testing.T) {
	clock := &bufferedProvisionalClock{disciplined: true, offset: 1_000_000_000}
	mgr := &Manager{
		cfg: Config{FrameTarget: 20 * time.Millisecond},
		ptp: func() Clock { return clock },
	}
	session := &Session{m: mgr, done: make(chan struct{})}
	stream := newBufferedStream(&audioStream{s: session})
	defer stream.stop()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorSet = true
	stream.anchorRTP = 1000
	stream.anchorMaster = 5_000_000_000
	stream.anchorTimeline = 1
	cancel := stream.cancel
	stream.mu.Unlock()

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	format := bufferedFormat{sampleRate: 48000}
	if !stream.waitForAnchor(format, timer, 0, 0, cancel) {
		t.Fatal("predisciplined anchor did not resolve")
	}
	stream.mu.Lock()
	resolved, ptp, rate, paceRTP := stream.anchorResolved, stream.anchorPTP, stream.anchorRate, stream.paceRTP
	stream.mu.Unlock()
	if !resolved || !ptp {
		t.Fatalf("anchor resolved=%v ptp=%v, want both true", resolved, ptp)
	}
	if rate != 48000 || paceRTP != 1000 {
		t.Fatalf("pacing rate/reference = %d/%d, want 48000/1000", rate, paceRTP)
	}
}

func TestBufferedPTPFallbackReacquiresNamedTimeline(t *testing.T) {
	clock := &bufferedProvisionalClock{offset: 1_000_000_000}
	mgr := &Manager{
		cfg: Config{FrameTarget: 20 * time.Millisecond},
		ptp: func() Clock { return clock },
	}
	session := &Session{m: mgr, done: make(chan struct{})}
	stream := newBufferedStream(&audioStream{s: session})
	defer stream.stop()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorSet = true
	stream.anchorMaster = 5_000_000_000
	stream.anchorTimeline = 1
	stream.anchorResolved = true
	stream.anchorRate = 48000
	stream.anchorPTP = false
	stream.mu.Unlock()

	if stream.perhapsReacquirePTP(0, 0) {
		t.Fatal("all-zero local fallback installed the clock")
	}
	clock.mu.Lock()
	clock.disciplined = true
	clock.mu.Unlock()
	stream.mu.Lock()
	stream.anchorNextPTP = time.Time{}
	stream.mu.Unlock()
	if !stream.perhapsReacquirePTP(0, 0) {
		t.Fatal("disciplined named timeline was not adopted")
	}
	stream.mu.Lock()
	ptp := stream.anchorPTP
	local := stream.anchorLocal
	stream.mu.Unlock()
	if !ptp {
		t.Fatal("PTP authority was not recorded after adoption")
	}
	if got, want := local.UnixNano(), int64(4_000_000_000); got != want {
		t.Fatalf("adopted PTP local anchor = %d, want %d", got, want)
	}
}

func TestBufferedSampleRateSwitchRebasesLocalPacing(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorSet = true
	stream.anchorRTP = 1000
	stream.anchorResolved = true
	stream.anchorRate = 48000
	stream.paceRTP = 1000
	stream.anchorLocal = time.Now().Add(-time.Hour)
	stream.mu.Unlock()

	format := bufferedFormat{
		ssrc: 0x15000000, codec: "alac", sampleRate: 44100,
		channels: 2, samples: 352,
	}
	if _, err := stream.prepareDecoder(format, 500000, 0); err != nil {
		t.Fatalf("prepare rate-switch decoder: %v", err)
	}
	stream.mu.Lock()
	anchorRTP, paceRTP, rate, resolved, local :=
		stream.anchorRTP, stream.paceRTP, stream.anchorRate, stream.anchorResolved, stream.anchorLocal
	decoder := stream.decoder
	stream.decoder = nil
	stream.decoderSSRC = 0
	stream.mu.Unlock()
	if decoder != nil {
		_ = decoder.Close()
	}
	if anchorRTP != 1000 {
		t.Fatalf("sender trim anchor changed to %d", anchorRTP)
	}
	if paceRTP != 500000 || rate != 44100 || !resolved {
		t.Fatalf("rate-switch pacing = rtp:%d rate:%d resolved:%v", paceRTP, rate, resolved)
	}
	if local.Before(time.Now().Add(-time.Second)) {
		t.Fatalf("rate-switch pacing inherited elapsed media time: %v", local)
	}
}

func TestBufferedChangedAnchorUsesTranslatedFallback(t *testing.T) {
	clock := &bufferedProvisionalClock{}
	mgr := &Manager{
		cfg: Config{FrameTarget: 20 * time.Millisecond},
		ptp: func() Clock { return clock },
	}
	session := &Session{m: mgr, done: make(chan struct{})}
	stream := newBufferedStream(&audioStream{s: session})
	defer stream.stop()
	pacing := time.Now()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorSet = true
	stream.anchorRTP = 100
	stream.anchorTimeline = 1
	stream.anchorResolved = true
	stream.anchorRate = 48000
	stream.paceRTP = 100
	stream.anchorLocal = pacing
	stream.installAndPlayLocked(1, 48100, 5_000_000_000)
	stream.mu.Unlock()

	format := bufferedFormat{sampleRate: 48000}
	if !stream.resolveAnchorOnce(format) {
		t.Fatal("translated fallback did not resolve")
	}
	stream.mu.Lock()
	anchorRTP, paceRTP, local, ptp := stream.anchorRTP, stream.paceRTP, stream.anchorLocal, stream.anchorPTP
	stream.mu.Unlock()
	if anchorRTP != 48100 || paceRTP != 48100 || ptp {
		t.Fatalf("fallback mapping = anchor:%d pace:%d ptp:%v", anchorRTP, paceRTP, ptp)
	}
	if want := pacing.Add(time.Second); !local.Equal(want) {
		t.Fatalf("fallback local mapping = %v, want translated %v", local, want)
	}
}

func TestBufferedConsecutiveChangedAnchorsPreserveTranslation(t *testing.T) {
	clock := &bufferedProvisionalClock{}
	mgr := &Manager{
		cfg: Config{FrameTarget: 20 * time.Millisecond},
		ptp: func() Clock { return clock },
	}
	session := &Session{m: mgr, done: make(chan struct{})}
	stream := newBufferedStream(&audioStream{s: session})
	defer stream.stop()
	pacing := time.Now()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorSet = true
	stream.anchorRTP = 100
	stream.anchorTimeline = 1
	stream.anchorResolved = true
	stream.anchorRate = 48000
	stream.paceRTP = 100
	stream.anchorLocal = pacing
	// Two changed anchors arrive before the first can resolve. Each interval
	// is exactly one second at 48 kHz.
	stream.installAndPlayLocked(1, 48100, 5_000_000_000)
	stream.installAndPlayLocked(1, 96100, 5_000_000_000)
	preset, resolved, paceRTP := stream.pacingPreset, stream.anchorResolved, stream.paceRTP
	translated := stream.anchorLocal
	stream.mu.Unlock()
	if !preset || resolved {
		t.Fatalf("consecutive refresh state preset=%v resolved=%v, want preset/unresolved", preset, resolved)
	}
	if paceRTP != 96100 {
		t.Fatalf("second translated pacing RTP = %d, want 96100", paceRTP)
	}
	if want := pacing.Add(2 * time.Second); !translated.Equal(want) {
		t.Fatalf("second translated mapping = %v, want %v", translated, want)
	}

	format := bufferedFormat{
		ssrc: 0x15000000, codec: "alac", sampleRate: 44100,
		channels: 2, samples: 352,
	}
	if _, err := stream.prepareDecoder(format, 500000, 0); err != nil {
		t.Fatalf("prepare pending-preset rate switch: %v", err)
	}
	stream.mu.Lock()
	preset, resolved, paceRTP = stream.pacingPreset, stream.anchorResolved, stream.paceRTP
	rate := stream.anchorRate
	trimRTP := stream.anchorRTP
	rateLocal := stream.anchorLocal
	decoder := stream.decoder
	stream.decoder = nil
	stream.decoderSSRC = 0
	stream.mu.Unlock()
	if decoder != nil {
		_ = decoder.Close()
	}
	if !preset || resolved || paceRTP != 500000 || rate != 44100 || trimRTP != 96100 {
		t.Fatalf("pending-preset rate switch = preset:%v resolved:%v pace:%d rate:%d trim:%d",
			preset, resolved, paceRTP, rate, trimRTP)
	}

	if !stream.resolveAnchorOnce(format) {
		t.Fatal("rate-switched translated fallback did not resolve")
	}
	stream.mu.Lock()
	local, finalPace, finalRate, finalTrim, ptp :=
		stream.anchorLocal, stream.paceRTP, stream.anchorRate, stream.anchorRTP, stream.anchorPTP
	stream.mu.Unlock()
	if finalPace != 500000 || finalRate != 44100 || finalTrim != 96100 || ptp {
		t.Fatalf("resolved preset = pace:%d rate:%d trim:%d ptp:%v",
			finalPace, finalRate, finalTrim, ptp)
	}
	if !local.Equal(rateLocal) {
		t.Fatalf("resolved preset local mapping = %v, want %v", local, rateLocal)
	}
}

func TestBufferedAnchorRefreshKeepsSinkAndIdenticalIsNoop(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	sink := &fakeSink{}
	pacing := time.Now()
	stream.mu.Lock()
	stream.playRequested = true
	stream.anchorSet = true
	stream.anchorRTP = 100
	stream.anchorTimeline = 1
	stream.anchorResolved = true
	stream.anchorRate = 48000
	stream.paceRTP = 100
	stream.anchorLocal = pacing
	stream.generation = 5
	stream.sink = sink
	stream.sinkSSRC = 0x15000000
	stream.writing = true
	stream.writingSequence = 7
	stream.installAndPlayLocked(1, 48100, 0)
	generation, timing := stream.generation, stream.timingRevision
	translated := stream.anchorLocal
	paceRTP, writing, writingSequence := stream.paceRTP, stream.writing, stream.writingSequence
	currentSink := stream.sink
	stream.mu.Unlock()

	if generation != 5 || timing != 1 {
		t.Fatalf("changed anchor generation/timing = %d/%d, want 5/1", generation, timing)
	}
	if currentSink != sink {
		t.Fatal("changed anchor detached the live sink")
	}
	if !writing || writingSequence != 7 {
		t.Fatal("changed anchor cleared actual in-flight write bookkeeping")
	}
	if paceRTP != 48100 {
		t.Fatalf("translated pacing RTP = %d, want 48100", paceRTP)
	}
	if want := pacing.Add(time.Second); !translated.Equal(want) {
		t.Fatalf("translated local mapping = %v, want %v", translated, want)
	}

	stream.mu.Lock()
	stream.pacing = true
	stream.pacingSequence = 9
	stream.installAndPlayLocked(1, 48100, 0)
	noopTiming := stream.timingRevision
	noopLocal := stream.anchorLocal
	noopPace := stream.paceRTP
	noopWriting := stream.writing
	noopPacing := stream.pacing
	stream.mu.Unlock()
	if noopTiming != timing || !noopLocal.Equal(translated) || noopPace != paceRTP || !noopWriting || !noopPacing {
		t.Fatal("identical active anchor was not a no-op")
	}
	if _, aborted := sink.state(); aborted {
		t.Fatal("anchor refresh aborted queued PCM")
	}
	if stream.sink != sink {
		t.Fatal("anchor refresh lost the live sink before the deferred boundary")
	}
	stream.flush(7, 8, false)
	if _, aborted := sink.state(); !aborted {
		t.Fatal("deferred range did not retire the in-flight writing sink")
	}
	stream.mu.Lock()
	retained := stream.sink
	stream.mu.Unlock()
	if retained != nil {
		t.Fatal("deferred range retained the retired writing sink")
	}
}

func TestBufferedConnectionClaimRejectsAuthenticatedReplacement(t *testing.T) {
	stream := newBufferedStream(nil)
	defer stream.stop()
	stream.mu.Lock()
	stream.connID = 7
	stream.connAuthenticated = true
	stream.mu.Unlock()
	if id, _, claimed := stream.beginConnection(true); claimed || id != 0 {
		t.Fatalf("authenticated owner was replaced: id=%d claimed=%v", id, claimed)
	}
	stream.mu.Lock()
	if stream.connID != 7 || !stream.connAuthenticated {
		stream.mu.Unlock()
		t.Fatal("rejected replacement mutated the live connection identity")
	}
	stream.connAuthenticated = false
	stream.mu.Unlock()
	if id, _, claimed := stream.beginConnection(true); !claimed || id != 8 {
		t.Fatalf("unauthenticated replacement claim = %d/%v, want 8/true", id, claimed)
	}
}

func (b *bufferedStream) pendingLockedForTest() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pendingLocked()
}

func (b *bufferedStream) popForTest() bufferedPacket {
	b.mu.Lock()
	defer b.mu.Unlock()
	packet := b.popLocked()
	sequence := packet.sequence
	b.recycleLocked(&packet)
	return bufferedPacket{sequence: sequence}
}
