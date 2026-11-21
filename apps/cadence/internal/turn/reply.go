package turn

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// packet is one reply packet queued for the stream's writer.
type packet struct {
	seq  uint32
	opus []byte
}

var errStreamGone = errors.New("turn: reply stream is gone")

// streamResult is how a reply/announcement writer ended.
type streamResult int32

const (
	streamRunning streamResult = iota
	streamOK
	streamFailed
	streamCanceled
)

// replyStream decodes one server stream and feeds the framework playback. It
// owns a goroutine so a blocking playback write (the mixer's backpressure)
// never stalls the link's read path; the decoder is used only there.
type replyStream struct {
	id   string
	dec  Decoder
	pb   Playback
	ch   chan packet
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}

	once   sync.Once
	lost   atomic.Bool
	sent   atomic.Int32
	result atomic.Int32
}

func newReplyStream(parent context.Context, id string, dec Decoder, pb Playback, depth int) *replyStream {
	ctx, stop := context.WithCancel(parent)
	return newReplyStreamContext(ctx, stop, id, dec, pb, depth)
}

// newReplyStreamContext builds a writer on an existing context. An
// announcement passes the same context to OpenPlayback, so canceling the
// writer also cancels the SDK playback handle rather than waiting for its
// drain timeout.
func newReplyStreamContext(ctx context.Context, stop context.CancelFunc, id string, dec Decoder, pb Playback, depth int) *replyStream {
	if depth <= 0 {
		depth = DefaultStreamQueue
	}
	s := &replyStream{
		id: id, dec: dec, pb: pb,
		ch: make(chan packet, depth), ctx: ctx, stop: stop, done: make(chan struct{}),
	}
	go s.run()
	return s
}

// push queues one packet. It fails when the stream is gone or when a stalled
// playback does not drain within pushTimeout; the caller marks the reply lost.
func (s *replyStream) push(p packet) error {
	if s.lost.Load() {
		// The decoder or playback already failed; do not let a dead stream
		// absorb frames until its queue fills.
		return errStreamGone
	}
	select {
	case s.ch <- p:
		return nil
	case <-s.ctx.Done():
		return errStreamGone
	default:
	}
	t := time.NewTimer(pushTimeout)
	defer t.Stop()
	select {
	case s.ch <- p:
		return nil
	case <-s.ctx.Done():
		return errStreamGone
	case <-t.C:
		return errStreamGone
	}
}

// finish ends the stream. drain half-closes the playback so its queued tail
// plays; !drain aborts it and discards the queue. It is idempotent.
func (s *replyStream) finish(drain bool) {
	s.once.Do(func() {
		if drain {
			close(s.ch)
			return
		}
		s.stop()
	})
}

// abort cancels the writer even after a drain half-close began. finish(true)
// has already released the channel, so a later cut needs the context to stop
// the queued tail.
func (s *replyStream) abort() { s.stop() }

// waitFor waits for the stream's goroutine to end, bounded.
func (s *replyStream) waitFor(d time.Duration) bool {
	if d <= 0 {
		select {
		case <-s.done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return true
	case <-t.C:
		return false
	}
}

// resultCode is the terminal outcome for the controller: ok after a drained
// playback, playback_failed after a decode/write/close failure, aborted after
// a cut. It is meaningful once done is closed.
func (s *replyStream) resultCode() string {
	switch streamResult(s.result.Load()) {
	case streamOK:
		return "ok"
	case streamFailed:
		return "playback_failed"
	case streamCanceled:
		return "aborted"
	default:
		return "running"
	}
}

func (s *replyStream) run() {
	result := streamCanceled
	defer func() {
		s.result.Store(int32(result))
		close(s.done)
	}()
	for {
		select {
		case <-s.ctx.Done():
			_ = s.pb.Abort()
			s.dec.Close()
			return
		case p, ok := <-s.ch:
			if s.ctx.Err() != nil {
				// A cut after a drain half-close still wins over queued audio.
				_ = s.pb.Abort()
				s.dec.Close()
				return
			}
			if !ok {
				// StreamEnd: drain everything already queued, then close.
				if err := s.pb.Close(); err != nil {
					result = streamFailed
				} else {
					result = streamOK
				}
				s.dec.Close()
				return
			}
			pcm, err := s.dec.Decode(p.opus)
			if err != nil {
				s.lost.Store(true)
				result = streamFailed
				_ = s.pb.Abort()
				s.dec.Close()
				return
			}
			if err := s.pb.Write(pcm); err != nil {
				s.lost.Store(true)
				result = streamFailed
				_ = s.pb.Abort()
				s.dec.Close()
				return
			}
			s.sent.Add(1)
		}
	}
}
