package session

// The routing capture is an LED-ring concern, not a playback prerequisite. A
// grant can wait for another owner, so these tests stall it deliberately and
// pin the two consumer-visible startup paths: the legacy RECORD reply and the
// first buffered PCM write. They also pin teardown while a grant is in flight:
// cancellation must bound the wait, and a late ring must be closed instead of
// left holding the resource.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// stallingRings holds a ring grant until the test releases it, or until the
// session cancels the acquisition.
type stallingRings struct {
	ring        *fakeRing
	started     chan struct{}
	release     chan struct{}
	canceled    chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	cancelOnce  sync.Once
}

func newStallingRings(ring *fakeRing) *stallingRings {
	return &stallingRings{
		ring:     ring,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
}

func (r *stallingRings) OpenRing(ctx context.Context) (Ring, error) {
	r.startedOnce.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return r.ring, nil
	case <-ctx.Done():
		r.cancelOnce.Do(func() { close(r.canceled) })
		return nil, ctx.Err()
	}
}

func (r *stallingRings) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func TestRecordDoesNotWaitForStalledRingCapture(t *testing.T) {
	mgr, transport, _, ring, _, _, _ := newTestManager(t)
	stalled := newStallingRings(ring)
	mgr.rings = stalled
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()
	announce(t, client)
	waitUntil(t, "the announced session", func() bool { return mgr.Active() != nil })

	setup, err := client.doHeaders("SETUP", "/1", nil,
		"Transport: RTP/AVP/TCP;unicast;interleaved=0-1")
	if err != nil || setup.status != 200 {
		t.Fatalf("SETUP = %v %v", setup, err)
	}

	type recordResult struct {
		response *clientResponse
		err      error
	}
	record := make(chan recordResult, 1)
	go func() {
		response, err := client.do("RECORD", "/1", "", nil)
		record <- recordResult{response: response, err: err}
	}()

	// RECORD starts the acquisition, but its reply must not wait for the grant.
	select {
	case <-stalled.started:
	case <-time.After(2 * time.Second):
		t.Fatal("RECORD never attempted the ring capture")
	}
	select {
	case got := <-record:
		if got.err != nil || got.response.status != 200 {
			t.Fatalf("RECORD = %v %v", got.response, got.err)
		}
	case <-time.After(2 * time.Second):
		stalled.unblock()
		t.Fatal("RECORD waited for the stalled ring capture grant")
	}

	// The grant still lands and starts the ring after the reply.
	stalled.unblock()
	waitUntil(t, "the ring frames", func() bool {
		frames, _ := ring.snapshot()
		return frames > 0
	})

	if response, err := client.do("TEARDOWN", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", response, err)
	}
	waitUntil(t, "the ring to close", func() bool {
		_, closed := ring.snapshot()
		return closed
	})
}

// A grant that wins the race against session teardown must be closed, never
// adopted: otherwise the capture keeps the AirPlay presentation selected over
// the configured baseline with no session driving it.
type lateRings struct {
	ring        *fakeRing
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

func newLateRings(ring *fakeRing) *lateRings {
	return &lateRings{ring: ring, started: make(chan struct{}), release: make(chan struct{})}
}

func (r *lateRings) OpenRing(context.Context) (Ring, error) {
	r.startedOnce.Do(func() { close(r.started) })
	<-r.release
	return r.ring, nil
}

func (r *lateRings) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func TestLateRingGrantAfterTeardownIsClosed(t *testing.T) {
	ring := &fakeRing{}
	late := newLateRings(ring)
	mgr := &Manager{
		cfg:   Config{RingEnabled: true, Logf: t.Logf},
		rings: late,
	}
	ringCtx, ringCancel := context.WithCancel(context.Background())
	session := &Session{
		m:          mgr,
		peer:       Peer{Address: "late-ring.test"},
		done:       make(chan struct{}),
		ringCtx:    ringCtx,
		ringCancel: ringCancel,
	}
	defer session.stopRing()
	session.startRing()
	select {
	case <-late.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the session never attempted the ring capture")
	}

	session.close("test teardown")
	late.unblock()
	waitUntil(t, "the late ring grant to close", func() bool {
		_, closed := ring.snapshot()
		return closed
	})
	if frames, _ := ring.snapshot(); frames != 0 {
		t.Fatalf("late ring wrote %d frames after teardown", frames)
	}
}

// Teardown must bound the grant wait: the session context is cancelled even
// though no ring exists yet, so the acquisition goroutine exits instead of
// waiting out the framework's 10 s grant timeout.
func TestTeardownCancelsStalledRingAcquisition(t *testing.T) {
	ring := &fakeRing{}
	stalled := newStallingRings(ring)
	mgr := &Manager{
		cfg:   Config{RingEnabled: true, Logf: t.Logf},
		rings: stalled,
	}
	ringCtx, ringCancel := context.WithCancel(context.Background())
	session := &Session{
		m:          mgr,
		peer:       Peer{Address: "cancelled-ring.test"},
		done:       make(chan struct{}),
		ringCtx:    ringCtx,
		ringCancel: ringCancel,
	}
	defer session.stopRing()
	session.startRing()
	select {
	case <-stalled.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the session never attempted the ring capture")
	}

	session.close("test teardown")
	select {
	case <-stalled.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown did not cancel the stalled ring acquisition")
	}
	if _, closed := ring.snapshot(); closed {
		t.Fatal("a refused acquisition closed a ring it never received")
	}
}

func TestBufferedFirstPCMWriteDoesNotWaitForRingCapture(t *testing.T) {
	sink := &fakeSink{}
	ring := &fakeRing{}
	stalled := newStallingRings(ring)
	mgr := &Manager{
		cfg:   Config{RingEnabled: true, FrameTarget: time.Second, Logf: t.Logf},
		rings: stalled,
		sinks: fakeSinks{sink: sink},
	}
	session := &Session{
		m:    mgr,
		peer: Peer{Address: "buffered-ring.test"},
		done: make(chan struct{}),
	}
	stream := newBufferedStream(&audioStream{s: session})
	defer stream.stop()
	stream.playRequested = true
	format := bufferedFormat{
		ssrc: bufferedSSRCALAC48, codec: "alac",
		sampleRate: 48000, channels: 2, samples: 352,
	}

	written := make(chan error, 1)
	go func() {
		_, err := stream.writePCM(format, make([]byte, 8), 1, 0, 0, 0)
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("first buffered PCM write: %v", err)
		}
	case <-time.After(2 * time.Second):
		stalled.unblock()
		t.Fatal("the first buffered PCM write waited for the stalled ring capture grant")
	}
	select {
	case <-stalled.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first buffered PCM write never attempted the ring capture")
	}
	if bytes, _ := sink.state(); bytes != 8 {
		t.Fatalf("sink received %d bytes before the ring grant, want the 8-byte block", bytes)
	}

	stalled.unblock()
	waitUntil(t, "the ring frames", func() bool {
		frames, _ := ring.snapshot()
		return frames > 0
	})
	session.stopRing()
	if _, closed := ring.snapshot(); !closed {
		t.Fatal("the ring was not closed on teardown")
	}
}
