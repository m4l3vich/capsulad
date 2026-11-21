package turn

import (
	"context"
	"sync"

	"capsulad/sdk/capsulad"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
)

// speakerEndpoint is the framework output the client speaks and announces on.
const speakerEndpoint = "audio.output.speaker"

// announceDone is the writer's terminal outcome, delivered to the controller
// goroutine so ducking is released even when no further envelope arrives.
type announceDone struct {
	owner uint64
	id    string
	code  string
}

// announcement is one server announce stream. cadence.v2 sends it outside a
// session with an empty session_id: it is speech the household asked for
// (a task event, an API announcement), not a turn, so it opens no microphone,
// no capture indication and no session. A newer announcement supersedes the
// older one, as the upstream device does.
type announcement struct {
	owner   uint64
	id      string
	gen     uint64
	conv    Conversation
	stream  *replyStream
	packets int
	end     chan struct{}
	endOnce sync.Once
	// ending is set when StreamEnd arrived and the queue is draining.
	ending bool
	// ended is set once the terminal line was logged.
	ended bool
}

func (a *announcement) closeEnd() { a.endOnce.Do(func() { close(a.end) }) }

// handleAnnouncement takes the announcement arms of an envelope. It reports
// whether the envelope belonged to an announcement, so a turn (or the no-turn
// refusal path) never handles the same frame twice.
func (c *Controller) handleAnnouncement(gen uint64, env *cadencev2.Envelope) bool {
	switch body := env.GetBody().(type) {
	case *cadencev2.Envelope_StreamStart:
		if body.StreamStart.GetPurpose() != cadencev2.StreamPurpose_STREAM_PURPOSE_ANNOUNCE {
			return false
		}
		c.startAnnouncement(gen, body.StreamStart)
		return true
	case *cadencev2.Envelope_AudioDown:
		a := c.announce
		if a == nil || a.id != body.AudioDown.GetStreamId() {
			return false
		}
		if a.ending {
			// Frames after StreamEnd belong to no stream; ignore them.
			return true
		}
		if err := a.stream.push(packet{seq: body.AudioDown.GetSeq(), opus: body.AudioDown.GetOpus()}); err != nil {
			c.abortAnnouncement("stream_gap")
			return true
		}
		a.packets++
		return true
	case *cadencev2.Envelope_StreamEnd:
		a := c.announce
		if a == nil || a.id != body.StreamEnd.GetStreamId() {
			return false
		}
		if !a.ending {
			c.endAnnouncement()
		}
		return true
	default:
		return false
	}
}

// startAnnouncement opens playback for one announce stream. Playback failure
// refuses only that stream; the link and any running turn stay up.
func (c *Controller) startAnnouncement(gen uint64, st *cadencev2.StreamStart) {
	id := st.GetStreamId()
	if id == "" {
		c.d.Log.Printf("announce_refused: stream has no id")
		return
	}
	if gen == 0 || gen != c.linkGen || !c.linkUp {
		c.d.Log.Printf("announce_refused stream=%s: stale link generation", id)
		return
	}
	if st.GetSessionId() != "" {
		c.d.Log.Printf("announce_refused stream=%s: announcement carries session=%s", id, st.GetSessionId())
		return
	}
	if st.GetCodec() != cadencev2.Codec_CODEC_OPUS && st.GetCodec() != cadencev2.Codec_CODEC_UNSPECIFIED {
		c.d.Log.Printf("announce_refused stream=%s: codec is not Opus", id)
		return
	}
	if c.runCtx == nil || c.runCtx.Err() != nil {
		return
	}
	// A newer announcement cuts the older, even one already playing its tail.
	c.abortAnnouncement("superseded")

	channels := int(st.GetChannels())
	if channels != 1 && channels != 2 {
		channels = 1
	}
	dec, err := c.d.Codecs.NewDecoder(channels)
	if err != nil {
		c.d.Log.Printf("announce_refused stream=%s: opus decoder", id)
		return
	}
	// One context owns the writer and the SDK playback handle. Cancelling it
	// aborts playback instead of waiting for the SDK's drain timeout.
	streamCtx, streamStop := context.WithCancel(c.runCtx)
	var conv Conversation
	if st.GetDuck() && c.d.Audio != nil {
		conv, err = c.d.Audio.BeginConversation(streamCtx, "announce:"+id)
		if err != nil {
			// Playback still works; the mixer will not duck other streams.
			c.d.Log.Printf("announce stream=%s: conversation: %v", id, err)
			conv = nil
		}
	}
	convID := ""
	if conv != nil {
		convID = conv.ID()
	}
	pb, err := c.d.Audio.OpenPlayback(streamCtx, speakerEndpoint, capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: 48000,
		Channels:     uint32(channels),
	}, convID)
	if err != nil {
		streamStop()
		dec.Close()
		if conv != nil {
			conv.Close()
		}
		c.d.Log.Printf("announce_refused stream=%s: playback", id)
		return
	}
	a := &announcement{
		owner: c.announceSeq.Add(1), id: id, gen: gen, conv: conv, end: make(chan struct{}),
		stream: newReplyStreamContext(streamCtx, streamStop, id, dec, pb, c.d.StreamQueue),
	}
	c.announce = a
	go c.monitorAnnouncement(a)
	c.d.Log.Printf("announce_start stream=%s channels=%d", id, channels)
}

// endAnnouncement marks the stream ended and half-closes playback. The
// monitor reports the actual drain outcome; the controller does not log ok
// before the playback has finished.
func (c *Controller) endAnnouncement() {
	a := c.announce
	if a == nil || a.ending {
		return
	}
	a.ending = true
	a.closeEnd()
	a.stream.finish(true)
}

// abortAnnouncement cuts the streaming announcement, its tail included, and
// releases its playback and ducking conversation at once. It is a no-op after
// the normal end was already reported.
func (c *Controller) abortAnnouncement(code string) {
	a := c.announce
	if a == nil {
		return
	}
	c.announce = nil
	a.closeEnd()
	a.stream.finish(false)
	// A draining stream already released its channel; the context still cuts
	// the tail a newer announcement supersedes.
	a.stream.abort()
	if a.conv != nil {
		a.conv.Close()
	}
	if a.ended {
		return
	}
	a.ended = true
	c.d.Log.Printf("announce_end stream=%s code=%s packets=%d", a.id, code, a.packets)
}

// monitorAnnouncement waits for the writer to end (on its own failure or after
// StreamEnd) and reports the real outcome. It never touches c.announce; the
// controller owns that field.
func (c *Controller) monitorAnnouncement(a *announcement) {
	code := "aborted"
	select {
	case <-a.stream.done:
		code = a.stream.resultCode()
	case <-a.end:
		if a.stream.waitFor(maxTailDrain) {
			code = a.stream.resultCode()
		} else {
			a.stream.abort()
			code = "drain_timeout"
		}
	}
	if a.conv != nil {
		a.conv.Close()
	}
	// Release the run-scoped child context on completion; the writer's result
	// is final and nothing may still use the playback handle.
	a.stream.abort()
	select {
	case c.announceDone <- announceDone{owner: a.owner, id: a.id, code: code}:
	case <-c.stopped:
	}
}
