package turn_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"capsulad/sdk/capsulad"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/turn"
)

var errPlayback = errors.New("playback failed")

func announceStart(id, session string) *cadencev2.Envelope {
	return &cadencev2.Envelope{Body: &cadencev2.Envelope_StreamStart{StreamStart: &cadencev2.StreamStart{
		StreamId: id, SessionId: session, Purpose: cadencev2.StreamPurpose_STREAM_PURPOSE_ANNOUNCE,
		Codec: cadencev2.Codec_CODEC_OPUS, Channels: 1, Duck: true,
	}}}
}

func announceAudio(id string, seq uint32) *cadencev2.Envelope {
	return &cadencev2.Envelope{Body: &cadencev2.Envelope_AudioDown{AudioDown: &cadencev2.AudioDown{
		StreamId: id, Seq: seq, Opus: []byte{byte(seq), 1, 2, 3},
	}}}
}

func announceEnd(id string) *cadencev2.Envelope {
	return &cadencev2.Envelope{Body: &cadencev2.Envelope_StreamEnd{StreamEnd: &cadencev2.StreamEnd{StreamId: id}}}
}

// An announcement is speech outside a session: it plays on the speaker with
// no turn, no microphone and no ring capture (the ring is the recognition
// indication, not an output meter).
func TestAnnouncementPlaysWithoutATurn(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "announcement playback", func() bool { return h.audio.playback(0) != nil })
	pb := h.audio.playback(0)

	h.message(announceAudio("an1", 0))
	h.message(announceAudio("an1", 1))
	waitFor(t, "announcement frames", func() bool {
		writes, _, _ := pb.state()
		return writes == 2
	})
	h.message(announceEnd("an1"))
	waitFor(t, "announcement drained", func() bool {
		_, closed, _ := pb.state()
		return closed
	})
	if _, _, aborted := pb.state(); aborted {
		t.Fatal("a successful announcement was aborted")
	}
	waitFor(t, "announcement conversation closed", func() bool {
		conv := h.audio.conversation(0)
		return conv != nil && conv.closed.Load()
	})
	select {
	case <-pb.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("successful announcement did not release its playback context")
	}
	if h.ring.openCount() != 0 || h.audio.mic.closed.Load() {
		t.Fatalf("announcement opened capture: ring=%d micClosed=%v", h.ring.openCount(), h.audio.mic.closed.Load())
	}
	if n := h.ring.closeCount(); n != 0 {
		t.Fatalf("announcement closed the ring %d times", n)
	}
	if n := len(h.link.snapshot()); n != 0 {
		t.Fatalf("announcement sent %d frames", n)
	}
}

// A newer announcement cuts the older one, its queued tail included.
func TestNewAnnouncementSupersedesTheOld(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "first playback", func() bool { return h.audio.playback(0) != nil })
	h.message(announceAudio("an1", 0))
	h.message(announceStart("an2", ""))
	waitFor(t, "second playback", func() bool { return h.audio.playback(1) != nil })

	old := h.audio.playback(0)
	if _, _, aborted := old.state(); !aborted {
		t.Fatal("the older announcement was not aborted")
	}
	waitFor(t, "old conversation closed", func() bool {
		conv := h.audio.conversation(0)
		return conv != nil && conv.closed.Load()
	})
	h.message(announceAudio("an2", 0))
	newPB := h.audio.playback(1)
	waitFor(t, "new frame", func() bool {
		writes, _, _ := newPB.state()
		return writes == 1
	})
	h.message(announceEnd("an2"))
	waitFor(t, "new drained", func() bool {
		_, closed, _ := newPB.state()
		return closed
	})
}

// StreamEnd starts the tail drain; a newer announcement still cuts that tail
// and releases the old conversation instead of letting two announcements
// overlap.
func TestNewAnnouncementCutsADrainingTail(t *testing.T) {
	h := newHarness(t)
	h.audio.gate = make(chan struct{})
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "first playback", func() bool { return h.audio.playback(0) != nil })
	h.message(announceAudio("an1", 0))
	time.Sleep(10 * time.Millisecond) // the writer is blocked in the gated playback
	h.message(announceEnd("an1"))
	h.message(announceStart("an2", ""))
	waitFor(t, "second playback", func() bool { return h.audio.playback(1) != nil })
	waitFor(t, "old conversation closed", func() bool {
		conv := h.audio.conversation(0)
		return conv != nil && conv.closed.Load()
	})
	close(h.audio.gate)
	waitFor(t, "old playback aborted", func() bool {
		_, _, aborted := h.audio.playback(0).state()
		return aborted
	})
}

// An announcement that names a session is a protocol error: it would speak
// inside somebody's turn.
func TestAnnouncementWithSessionRefused(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", "s1"))
	time.Sleep(40 * time.Millisecond)
	if h.audio.playback(0) != nil {
		t.Fatal("announcement with a session opened playback")
	}
}

// Duck is the server's request to lower other playback for this stream. A
// false value plays without a ducking conversation.
func TestAnnouncementWithoutDuckOpensNoConversation(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	env := announceStart("an1", "")
	env.GetStreamStart().Duck = false
	h.message(env)
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	h.message(announceEnd("an1"))
	waitFor(t, "drained", func() bool {
		_, closed, _ := h.audio.playback(0).state()
		return closed
	})
	if conv := h.audio.conversation(0); conv != nil {
		t.Fatal("an un-ducked announcement opened a conversation")
	}
}

func TestAnnouncementRefusesOtherCodec(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	env := announceStart("an1", "")
	env.GetStreamStart().Codec = 99
	h.message(env)
	time.Sleep(40 * time.Millisecond)
	if h.audio.playback(0) != nil {
		t.Fatal("an announcement with a non-Opus codec opened playback")
	}
}

// A playback failure reports its outcome without waiting for another frame:
// the monitor releases the ducking conversation and the controller logs the
// real code.
func TestAnnouncementPlaybackFailureReleasesTheConversation(t *testing.T) {
	h := newHarness(t)
	h.audio.writeErr = errPlayback
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	h.message(announceAudio("an1", 0))
	waitFor(t, "conversation closed", func() bool {
		conv := h.audio.conversation(0)
		return conv != nil && conv.closed.Load()
	})
	waitFor(t, "failed announcement logged", func() bool {
		return strings.Contains(h.logBuf.String(), "announce_end stream=an1 code=playback_failed")
	})
	if _, _, aborted := h.audio.playback(0).state(); !aborted {
		t.Fatal("the failed playback was not aborted")
	}
	select {
	case <-h.audio.playback(0).ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("failed announcement did not release its playback context")
	}
}

func TestDismissStopsAnnouncement(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	h.ctrl.Dismiss()
	waitFor(t, "announcement aborted", func() bool {
		_, _, aborted := h.audio.playback(0).state()
		return aborted
	})
	waitFor(t, "conversation closed", func() bool {
		conv := h.audio.conversation(0)
		return conv != nil && conv.closed.Load()
	})
}

func TestLinkDownStopsAnnouncement(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	h.link.setUp(false)
	h.ctrl.LinkChanged(false, 1)
	waitFor(t, "announcement aborted", func() bool {
		_, _, aborted := h.audio.playback(0).state()
		return aborted
	})
}

// A session and an announcement are independent, as on the upstream device:
// the session's reply and the announcement share the mixer, and closing the
// announcement does not close the turn's conversation.
func TestAnnouncementAndTurnAreIndependent(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.Offer(turn.Trigger{ActivationID: "a1", Origin: capsulad.ActivationOriginWakeword, QueuedAt: time.Now()})
	waitFor(t, "session", func() bool { return len(sessionStarts(h.link)) == 1 })
	sessionID := sessionStarts(h.link)[0].GetSessionId()

	h.message(announceStart("an1", ""))
	waitFor(t, "announcement playback", func() bool { return h.audio.playback(0) != nil })
	h.message(&cadencev2.Envelope{Body: &cadencev2.Envelope_StreamStart{StreamStart: &cadencev2.StreamStart{
		StreamId: "reply-1", SessionId: sessionID, Purpose: cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY, Channels: 1,
	}}})
	waitFor(t, "reply playback", func() bool { return h.audio.playback(1) != nil })

	h.message(announceAudio("an1", 0))
	h.message(&cadencev2.Envelope{Body: &cadencev2.Envelope_AudioDown{AudioDown: &cadencev2.AudioDown{
		StreamId: "reply-1", Seq: 0, Opus: []byte{1},
	}}})
	waitFor(t, "both streams wrote", func() bool {
		aw, _, _ := h.audio.playback(0).state()
		rw, _, _ := h.audio.playback(1).state()
		return aw == 1 && rw == 1
	})

	h.message(announceEnd("an1"))
	waitFor(t, "announcement conversation closed", func() bool {
		conv := h.audio.conversation(1)
		return conv != nil && conv.closed.Load()
	})
	if conv := h.audio.conversation(0); conv == nil || conv.closed.Load() {
		t.Fatal("ending the announcement closed the turn's conversation")
	}

	h.message(&cadencev2.Envelope{Body: &cadencev2.Envelope_SessionClose{SessionClose: &cadencev2.SessionClose{
		SessionId: sessionID, Code: "ok",
	}}})
	waitFor(t, "turn cleanup", func() bool { return h.audio.mic.closed.Load() && h.ring.closeCount() == 1 })
}

// StreamEnd does not mean success: the terminal code comes from the drained
// playback, not from the envelope that asked for the drain.
func TestAnnouncementStreamEndReportsDrainOutcome(t *testing.T) {
	h := newHarness(t)
	h.audio.gate = make(chan struct{})
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	h.message(announceAudio("an1", 0))
	h.message(announceEnd("an1"))
	time.Sleep(30 * time.Millisecond)
	if strings.Contains(h.logBuf.String(), "announce_end stream=an1 code=ok") {
		t.Fatal("StreamEnd logged ok before the playback drained")
	}
	close(h.audio.gate)
	waitFor(t, "drained outcome", func() bool {
		return strings.Contains(h.logBuf.String(), "announce_end stream=an1 code=ok")
	})
}

func TestAnnouncementStreamEndAfterFailureLogsFailure(t *testing.T) {
	h := newHarness(t)
	h.audio.writeErr = errPlayback
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	h.message(announceAudio("an1", 0))
	h.message(announceEnd("an1"))
	waitFor(t, "failure outcome", func() bool {
		return strings.Contains(h.logBuf.String(), "announce_end stream=an1 code=playback_failed")
	})
	if strings.Contains(h.logBuf.String(), "announce_end stream=an1 code=ok") {
		t.Fatal("a failed announcement ended as ok")
	}
}

// The SDK playback is opened on the announcement's context, so a cut cancels
// the handle instead of leaving it in a drain wait.
func TestAnnouncementAbortCancelsPlaybackContext(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("an1", ""))
	waitFor(t, "playback", func() bool { return h.audio.playback(0) != nil })
	pb := h.audio.playback(0)
	select {
	case <-pb.ctx.Done():
		t.Fatal("playback context was already cancelled")
	default:
	}
	h.ctrl.Dismiss()
	select {
	case <-pb.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("aborting the announcement did not cancel the SDK playback context")
	}
}

// A frame from an older connection must not open playback after a newer
// generation is live.
func TestStaleLinkGenerationDropsAnnouncement(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.LinkChanged(true, 2)
	h.ctrl.Message(1, announceStart("old", ""))
	time.Sleep(40 * time.Millisecond)
	if h.audio.playback(0) != nil {
		t.Fatal("a stale generation opened playback")
	}
	if !strings.Contains(h.logBuf.String(), "stale_link_frame gen=1") {
		t.Fatalf("stale frame was not logged: %q", h.logBuf.String())
	}
	h.ctrl.Message(2, announceStart("new", ""))
	waitFor(t, "current generation playback", func() bool { return h.audio.playback(0) != nil })
}

// Link-down and envelopes share one ordered queue: a StreamStart queued after
// the down event is dropped, so it cannot duck or play on a dead link.
func TestQueuedAnnouncementAfterLinkDownIsDropped(t *testing.T) {
	h := newHarness(t)
	cancel := h.run(t)
	defer cancel()

	h.ctrl.LinkChanged(false, 1)
	h.ctrl.Message(1, announceStart("an1", ""))
	time.Sleep(40 * time.Millisecond)
	if h.audio.playback(0) != nil {
		t.Fatal("an announcement queued after link-down opened playback")
	}
	if conv := h.audio.conversation(0); conv != nil {
		t.Fatal("an announcement queued after link-down opened a conversation")
	}
}

// A reused server stream id must not let an older completion clear the newer
// announcement. The controller matches the local owner token, not the id.
func TestAnnouncementCompletionMatchesOwnerAcrossIDReuse(t *testing.T) {
	h := newHarness(t)
	oldGate := make(chan struct{})
	h.audio.gate = oldGate
	cancel := h.run(t)
	defer cancel()

	h.message(announceStart("same", ""))
	waitFor(t, "old playback", func() bool { return h.audio.playback(0) != nil })
	old := h.audio.playback(0)
	h.message(announceAudio("same", 0))
	time.Sleep(10 * time.Millisecond) // old writer is blocked in the gated playback

	// The replacement is not gated; close the old gate only after it starts.
	h.audio.mu.Lock()
	h.audio.gate = nil
	h.audio.mu.Unlock()
	h.message(announceStart("same", ""))
	waitFor(t, "replacement playback", func() bool { return h.audio.playback(1) != nil })
	close(oldGate)
	waitFor(t, "old playback aborted", func() bool {
		_, _, aborted := old.state()
		return aborted
	})
	time.Sleep(20 * time.Millisecond) // let the old completion reach the controller
	h.message(announceAudio("same", 0))
	replacement := h.audio.playback(1)
	waitFor(t, "replacement still owns the announcement", func() bool {
		writes, _, _ := replacement.state()
		return writes == 1
	})
}
