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

	once sync.Once
	lost atomic.Bool
	sent atomic.Int32
}

func newReplyStream(parent context.Context, id string, dec Decoder, pb Playback, depth int) *replyStream {
	if depth <= 0 {
		depth = DefaultStreamQueue
	}
	ctx, stop := context.WithCancel(parent)
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

func (s *replyStream) run() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			_ = s.pb.Abort()
			s.dec.Close()
			return
		case p, ok := <-s.ch:
			if !ok {
				// StreamEnd: drain everything already queued, then close.
				_ = s.pb.Close()
				s.dec.Close()
				return
			}
			pcm, err := s.dec.Decode(p.opus)
			if err != nil {
				s.lost.Store(true)
				_ = s.pb.Abort()
				s.dec.Close()
				return
			}
			if err := s.pb.Write(pcm); err != nil {
				s.lost.Store(true)
				_ = s.pb.Abort()
				s.dec.Close()
				return
			}
			s.sent.Add(1)
		}
	}
}
