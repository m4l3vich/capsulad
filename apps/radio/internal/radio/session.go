package radio

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"capsulad/apps/radio/internal/stream"
	"capsulad/sdk/capsulad"

	gomp3 "github.com/hajimehoshi/go-mp3"
)

const (
	chunkMS          = 200
	playerChannels   = 2
	openTimeout      = 12 * time.Second
	maxReconnects    = 3
	reconnectWindow  = 30 * time.Second
	maxSampleRate    = 192000
	minSampleRate    = 8000
	maxChunkBytes    = 256 << 10
)

// session is one playback attempt. Its generation is implicit in pointer
// identity: every asynchronous completion asks the app whether this session is
// still the current one, so a superseded open, read or failure can never touch
// a newer stream or selection.
type session struct {
	app     *App
	gen     uint64
	station Station

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	started  time.Time
	progress atomic.Int64 // time.Since(started) at the last accepted chunk

	mu       sync.Mutex
	playback *capsulad.Playback
}

func newSession(app *App, gen uint64, station Station) *session {
	ctx, cancel := context.WithCancel(app.ctx)
	return &session{
		app:     app,
		gen:     gen,
		station: station,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		started: time.Now(),
	}
}

func (s *session) stop() {
	s.cancel()
	s.mu.Lock()
	playback := s.playback
	s.playback = nil
	s.mu.Unlock()
	if playback != nil {
		_ = playback.Abort()
	}
}

func (s *session) wait(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
	}
}

func (s *session) touch() {
	s.progress.Store(int64(time.Since(s.started)))
}

// idleFor is how long the session has gone without accepting an audio chunk.
func (s *session) idleFor() time.Duration {
	return time.Since(s.started) - time.Duration(s.progress.Load())
}

func (s *session) setPlayback(playback *capsulad.Playback) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	s.playback = playback
	return true
}

func (s *session) currentPlayback() *capsulad.Playback {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.playback
}

func (s *session) takePlayback() *capsulad.Playback {
	s.mu.Lock()
	defer s.mu.Unlock()
	playback := s.playback
	s.playback = nil
	return playback
}

func (s *session) releasePlayback() {
	if playback := s.takePlayback(); playback != nil {
		_ = playback.Abort()
	}
}

func (s *session) run() {
	defer close(s.done)
	defer s.app.finishSession(s)
	defer s.releasePlayback()

	st, dec, err := s.open()
	if err != nil {
		if s.ctx.Err() != nil {
			return
		}
		s.app.failSession(s, err.Error())
		return
	}
	defer func() { _ = st.Close() }()

	rate := dec.SampleRate()
	if rate < minSampleRate || rate > maxSampleRate {
		s.app.failSession(s, fmt.Sprintf("stream sample rate %d Hz is out of range", rate))
		return
	}
	playback, err := s.openPlayback(rate)
	if err != nil {
		if s.ctx.Err() != nil {
			return
		}
		s.app.failSession(s, "open speaker stream: "+err.Error())
		return
	}
	if !s.setPlayback(playback) {
		_ = playback.Abort()
		return
	}
	s.touch()
	s.app.logf("session %d playing station=%q rate=%d", s.gen, s.station.Name, rate)

	pcm := make([]byte, chunkBytes(rate))
	var (
		first        = true
		failures     int
		lastFailure  time.Time
		lastArtist   string
		lastTitle    string
		pacerStart   time.Time
		framesQueued uint64
	)
	// Real-time pacing. The framework playback queue is bounded (32 chunks) and
	// Write blocks when it is full, but the SDK's outbound app-bus queue is a
	// separate, smaller bound that overflows first (RESOURCE_EXHAUSTED) if we
	// hand it decoded chunks faster than the mixer consumes them. A live stream
	// arrives at ~1x so this rarely bites, but a finite or burst-delivered
	// source decodes far faster than real time. Pace each chunk against the
	// audio clock so the queued audio stays within cushion of real time.
	const cushion = 500 * time.Millisecond
	resetPacer := func() {
		pacerStart = time.Now()
		framesQueued = 0
	}
	resetPacer()
	// pace blocks until the audio clock permits `frames` more frames, keeping
	// the queue within cushion of real time. It reports false when the session
	// is cancelled while waiting. Overflow-safe: seconds and sub-second
	// remainder are computed separately so a long session cannot overflow.
	pace := func(frames uint64) bool {
		framesQueued += frames
		rateU := uint64(rate)
		written := time.Duration(framesQueued/rateU)*time.Second +
			time.Duration(framesQueued%rateU)*time.Second/time.Duration(rateU)
		lead := written - time.Since(pacerStart)
		if lead <= cushion {
			return true
		}
		timer := time.NewTimer(lead - cushion)
		defer timer.Stop()
		select {
		case <-timer.C:
			return true
		case <-s.ctx.Done():
			return false
		}
	}
	writeChunk := func(n int) bool {
		playback := s.currentPlayback()
		if playback == nil {
			return false
		}
		if !pace(uint64(n / (playerChannels * 2))) {
			return false
		}
		if err := playback.Write(pcm[:n]); err != nil {
			if s.ctx.Err() != nil {
				return false
			}
			s.app.failSession(s, "speaker stream ended: "+err.Error())
			return false
		}
		s.touch()
		if first {
			first = false
			s.app.markPlaying(s)
		}
		return true
	}

	for {
		if s.ctx.Err() != nil {
			return
		}
		n, readErr := io.ReadFull(dec, pcm)
		if n > 0 {
			artist, title := st.Metadata()
			if artist != lastArtist || title != lastTitle {
				lastArtist, lastTitle = artist, title
				s.app.updateMetadata(s, artist, title)
			}
			if !writeChunk(n) {
				return
			}
		}
		if readErr != nil {
			if s.ctx.Err() != nil {
				return
			}
			now := time.Now()
			if failures > 0 && now.Sub(lastFailure) > reconnectWindow {
				failures = 0
			}
			lastFailure = now
			nextStream, nextDecoder, lastErr := s.reconnect(&failures, readErr)
			if nextStream == nil {
				s.app.failSession(s, reconnectDetail(failures, lastErr))
				return
			}
			_ = st.Close()
			st, dec = nextStream, nextDecoder
			newRate := dec.SampleRate()
			if newRate < minSampleRate || newRate > maxSampleRate {
				s.app.failSession(s, fmt.Sprintf("stream sample rate %d Hz is out of range", newRate))
				return
			}
			if newRate != rate {
				oldPlayback := s.takePlayback()
				if oldPlayback != nil {
					_ = oldPlayback.Abort()
				}
				nextPlayback, err := s.openPlayback(newRate)
				if err != nil {
					s.app.failSession(s, "open speaker stream: "+err.Error())
					return
				}
				if !s.setPlayback(nextPlayback) {
					_ = nextPlayback.Abort()
					return
				}
				rate = newRate
				pcm = make([]byte, chunkBytes(rate))
			}
			lastArtist, lastTitle = "", ""
			s.touch()
			resetPacer()
			continue
		}
	}
}

func (s *session) open() (*stream.Stream, *gomp3.Decoder, error) {
	ctx, cancel := context.WithTimeout(s.ctx, openTimeout)
	defer cancel()
	st, err := stream.Open(ctx, stream.Config{Env: s.app.env}, s.station.URL)
	if err != nil {
		return nil, nil, err
	}
	if !st.Codec().Supported() {
		_ = st.Close()
		return nil, nil, fmt.Errorf("unsupported codec: %s", st.Codec())
	}
	dec, err := gomp3.NewDecoder(st)
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("decode MP3 stream: %w", err)
	}
	st.SetContext(s.ctx)
	return st, dec, nil
}

func (s *session) openPlayback(rate int) (*capsulad.Playback, error) {
	if rate < minSampleRate || rate > maxSampleRate {
		return nil, fmt.Errorf("sample rate %d Hz is out of range", rate)
	}
	return s.app.client.OpenPlayback(s.ctx, speakerEndpoint, capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: uint32(rate),
		Channels:     playerChannels,
	}, "")
}

func chunkBytes(rate int) int {
	size := rate * playerChannels * 2 * chunkMS / 1000
	if size <= 0 || size > maxChunkBytes {
		return maxChunkBytes
	}
	return size
}

func (s *session) reconnect(failures *int, cause error) (*stream.Stream, *gomp3.Decoder, error) {
	for *failures < maxReconnects {
		*failures = *failures + 1
		s.app.logf("session %d stream ended (%v); reconnect %d/%d", s.gen, cause, *failures, maxReconnects)
		if !s.backoff(*failures) {
			return nil, nil, cause
		}
		st, dec, err := s.open()
		if err == nil {
			s.app.logf("session %d reconnected", s.gen)
			return st, dec, nil
		}
		cause = err
		s.app.logf("session %d reconnect failed: %v", s.gen, err)
	}
	return nil, nil, cause
}

func (s *session) backoff(attempt int) bool {
	delay := time.Duration(1<<uint(attempt-1)) * time.Second
	if delay > 4*time.Second {
		delay = 4 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func reconnectDetail(attempts int, cause error) string {
	if cause == nil {
		return fmt.Sprintf("stream ended after %d reconnect attempts", attempts)
	}
	return fmt.Sprintf("stream ended after %d reconnect attempts: %v", attempts, cause)
}