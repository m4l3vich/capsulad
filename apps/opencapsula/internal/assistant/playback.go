package assistant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"capsulad/apps/opencapsula/internal/protocol"
	"capsulad/sdk/capsulad"
)

const (
	playbackQueue = 32
	writeWait     = 250 * time.Millisecond
)

var (
	// errPlaybackBackpressure means the writer did not accept a chunk within
	// the bounded wait. The turn keeps going; one dropped frame is preferable
	// to stalling control messages.
	errPlaybackBackpressure = errors.New("playback queue full")
	errPlaybackClosed       = errors.New("playback sink closed")
)

// playbackSink paces server PCM at real time and writes it through one
// framework playback stream. Pacing keeps the mixer queue small and makes the
// final drain wait the length of what the caller actually heard.
type playbackSink struct {
	playback *capsulad.Playback
	format   protocol.AudioFormat

	ctx    context.Context
	cancel context.CancelFunc

	input    chan []byte
	drainReq chan struct{}
	done     chan struct{}

	aborted atomic.Bool
	closing atomic.Bool
	drainIn sync.Once
	abortIn sync.Once

	mu  sync.Mutex
	err error
}

func newPlaybackSink(parent context.Context, client *capsulad.Client,
	format protocol.AudioFormat, conversationID string) (*playbackSink, error) {
	if err := format.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	playback, err := client.OpenPlayback(ctx, "audio.output.speaker", capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: uint32(format.SampleRateHertz),
		Channels:     uint32(format.Channels),
	}, conversationID)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &playbackSink{
		playback: playback,
		format:   format,
		ctx:      ctx,
		cancel:   cancel,
		input:    make(chan []byte, playbackQueue),
		drainReq: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.writeLoop()
	return s, nil
}

func (s *playbackSink) Write(pcm []byte) error {
	if s.aborted.Load() || s.closing.Load() {
		return errPlaybackClosed
	}
	timer := time.NewTimer(writeWait)
	defer timer.Stop()
	select {
	case s.input <- pcm:
		return nil
	case <-s.ctx.Done():
		return errPlaybackClosed
	case <-timer.C:
		return errPlaybackBackpressure
	}
}

func (s *playbackSink) writeLoop() {
	defer close(s.done)
	var next time.Time
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.drainReq:
			for {
				select {
				case pcm := <-s.input:
					if !s.writePaced(pcm, &next) {
						return
					}
				default:
					return
				}
			}
		case pcm := <-s.input:
			if !s.writePaced(pcm, &next) {
				return
			}
		}
	}
}

func (s *playbackSink) writePaced(pcm []byte, next *time.Time) bool {
	if !next.IsZero() {
		if wait := time.Until(*next); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-s.ctx.Done():
				timer.Stop()
				return false
			}
		}
	}
	if err := s.playback.Write(pcm); err != nil {
		s.setErr(err)
		return false
	}
	*next = time.Now().Add(s.duration(len(pcm)))
	return true
}

// Drain stops accepting audio, waits for the writer to hand every queued chunk
// to the mixer, then half-closes the playback stream so its tail is heard.
func (s *playbackSink) Drain(ctx context.Context) error {
	s.closing.Store(true)
	s.drainIn.Do(func() { close(s.drainReq) })
	select {
	case <-s.done:
	case <-ctx.Done():
		s.Abort()
		<-s.done
		return ctx.Err()
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.playback.Close() }()
	select {
	case err := <-errCh:
		if err != nil {
			s.setErr(err)
			return err
		}
		return s.getErr()
	case <-ctx.Done():
		s.Abort()
		select {
		case <-errCh:
		case <-time.After(time.Second):
		}
		return ctx.Err()
	}
}

// Abort discards queued audio immediately. It is safe to call repeatedly.
func (s *playbackSink) Abort() {
	s.abortIn.Do(func() {
		s.aborted.Store(true)
		s.closing.Store(true)
		s.cancel()
		_ = s.playback.Abort()
	})
}

func (s *playbackSink) duration(bytes int) time.Duration {
	rate := s.format.BytesPerSecond()
	if rate <= 0 {
		return 0
	}
	return time.Duration(int64(bytes) * int64(time.Second) / int64(rate))
}

func (s *playbackSink) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = fmt.Errorf("playback: %w", err)
	}
	s.mu.Unlock()
}

func (s *playbackSink) getErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
