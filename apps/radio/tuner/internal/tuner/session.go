package tuner

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"capsulad/apps/radio/tuner/internal/stream"
	"capsulad/sdk/capsulad"

	gomp3 "github.com/hajimehoshi/go-mp3"
)

const playerChannels = 2

// session is one tuned station. Its context is the session lifetime: Stop, a
// superseding Tune, app shutdown and the manager's Close all cancel it.
type session struct {
	mgr       *Manager
	id        string
	url       string
	station   string
	playerApp string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	rate    uint32
	seq     uint32
	scratch []byte
}

func newSession(m *Manager, req TuneRequest) *session {
	ctx, cancel := context.WithCancel(m.ctx)
	player := req.PlayerAppID
	if player == "" {
		player = m.PlayerApp()
	}
	return &session{
		mgr:       m,
		id:        req.SessionID,
		url:       req.URL,
		station:   req.StationName,
		playerApp: player,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
}

func (s *session) stop() { s.cancel() }

func (s *session) wait(d time.Duration) {
	select {
	case <-s.done:
	case <-time.After(d):
	}
}

// open establishes the stream and decodes the first MP3 frame. The caller's
// context bounds the whole open; the returned stream keeps using it until the
// session goroutine replaces it with the session context.
func (s *session) open(ctx context.Context) (*stream.Stream, *gomp3.Decoder, error) {
	st, err := stream.Open(ctx, s.mgr.cfg, s.url)
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
	return st, dec, nil
}

func (s *session) reopen() (*stream.Stream, *gomp3.Decoder, error) {
	ctx, cancel := context.WithTimeout(s.ctx, s.mgr.tuneOpenTimeout)
	defer cancel()
	st, dec, err := s.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	st.SetContext(s.ctx)
	return st, dec, nil
}

func (s *session) start(st *stream.Stream, dec *gomp3.Decoder) {
	go s.run(st, dec)
}

func (s *session) run(st *stream.Stream, dec *gomp3.Decoder) {
	defer close(s.done)
	defer s.mgr.clearCurrent(s)
	defer func() { _ = st.Close() }()

	rate := dec.SampleRate()
	if rate < 8000 || rate > 192000 {
		s.finalFailed(fmt.Sprintf("stream sample rate %d Hz is out of range", rate))
		return
	}
	s.rate = uint32(rate)
	chunkBytes := rate * playerChannels * 2 * chunkMS / 1000
	if chunkBytes <= 0 || chunkBytes > 256<<10 {
		s.finalFailed("stream chunk size is out of range")
		return
	}
	pcm := make([]byte, chunkBytes)
	log.Printf("radio-tuner: session %s playing station=%q rate=%d chunk=%dms", s.id, s.station, rate, chunkMS)

	var (
		firstFrame  uint64
		failures    int
		lastFailure time.Time
		discont     bool
		dropped     uint32
		lastArtist  string
		lastTitle   string
	)

	for {
		if s.ctx.Err() != nil {
			log.Printf("radio-tuner: session %s cancelled", s.id)
			return
		}
		n, err := io.ReadFull(dec, pcm)
		if err != nil {
			if s.ctx.Err() != nil {
				log.Printf("radio-tuner: session %s cancelled", s.id)
				return
			}
			if n > 0 {
				dropped += uint32(n / 4)
				discont = true
			}
			now := time.Now()
			if failures > 0 && now.Sub(lastFailure) > reconnectWindow {
				failures = 0
			}
			lastFailure = now
			newStream, newDecoder, lastErr := s.reconnect(&failures, err)
			if newStream == nil {
				s.finalFailed(reconnectDetail(failures, lastErr))
				return
			}
			_ = st.Close()
			st, dec = newStream, newDecoder
			rate = dec.SampleRate()
			if rate < 8000 || rate > 192000 {
				s.finalFailed(fmt.Sprintf("stream sample rate %d Hz is out of range", rate))
				return
			}
			chunkBytes = rate * playerChannels * 2 * chunkMS / 1000
			pcm = make([]byte, chunkBytes)
			discont = true
			lastArtist, lastTitle = "", ""
			continue
		}

		artist, title := st.Metadata()
		sendArtist, sendTitle := "", ""
		if artist != lastArtist || title != lastTitle {
			sendArtist, sendTitle = artist, title
			lastArtist, lastTitle = artist, title
		}

		s.seq++
		result, err := s.push(s.ctx, PushRequest{
			Sequence:      s.seq,
			SessionID:     s.id,
			PCM:           pcm[:n],
			SampleRateHz:  uint32(rate),
			Channels:      playerChannels,
			SampleFormat:  uint32(capsulad.SampleFormatPCM16LE),
			FirstFrame:    firstFrame,
			Discontinuity: discont,
			DroppedFrames: dropped,
			Title:         sendTitle,
			Artist:        sendArtist,
		})
		if err != nil {
			if s.ctx.Err() != nil {
				log.Printf("radio-tuner: session %s cancelled", s.id)
				return
			}
			log.Printf("radio-tuner: session %s push failed: %v", s.id, err)
			// A Push that cannot complete is session death; tell the player
			// explicitly instead of leaving it in a playing state.
			s.finalFailed("push failed: " + err.Error())
			return
		}
		if !result.Accepted {
			log.Printf("radio-tuner: session %s refused by player: %s", s.id, stringsOr(result.Detail, "no detail"))
			return
		}
		discont = false
		dropped = 0
		firstFrame += uint64(n / 4)
	}
}

// reconnect retries the stream a bounded number of times inside the same
// session. Every failed attempt increments failures; a stable stream resets
// the counter in run once reconnectWindow passed. A non-nil stream means the
// reconnect succeeded; otherwise cause is the last failure.
func (s *session) reconnect(failures *int, cause error) (*stream.Stream, *gomp3.Decoder, error) {
	for *failures < maxReconnects {
		(*failures)++
		log.Printf("radio-tuner: session %s stream ended (%v); reconnect %d/%d", s.id, cause, *failures, maxReconnects)
		if !s.backoff(*failures) {
			return nil, nil, cause
		}
		st, dec, err := s.reopen()
		if err == nil {
			log.Printf("radio-tuner: session %s reconnected", s.id)
			return st, dec, nil
		}
		cause = err
		log.Printf("radio-tuner: session %s reconnect failed: %v", s.id, err)
	}
	return nil, nil, cause
}

func (s *session) backoff(attempt int) bool {
	delay := time.Duration(1<<uint(attempt-1)) * time.Second
	if delay > 4*time.Second {
		delay = 4 * time.Second
	}
	select {
	case <-time.After(delay):
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *session) push(ctx context.Context, request PushRequest) (PushResult, error) {
	callCtx := ctx
	var cancel context.CancelFunc
	if s.mgr.pushTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, s.mgr.pushTimeout)
		defer cancel()
	}
	// The payload is copied into the bus frame before Call returns, so one
	// per-session buffer can carry every chunk without allocating ~150 KiB
	// per push.
	payload := request.MarshalTo(s.scratch[:0])
	s.scratch = payload
	reply, err := s.mgr.client.Call(callCtx, s.playerApp, PlayerService, "Push", payload, 0)
	if err != nil {
		return PushResult{}, err
	}
	return decodePushResult(reply)
}

// decodePushResult turns a player reply into a result. Proto3 marshals
// PushResult{accepted:false} to zero bytes, so an empty reply is a refusal
// without a detail, not a transport failure.
func decodePushResult(reply []byte) (PushResult, error) {
	if len(reply) == 0 {
		return PushResult{Detail: "player refused without detail"}, nil
	}
	result, err := DecodePushResult(reply)
	if err != nil {
		return PushResult{}, fmt.Errorf("decode PushResult: %w", err)
	}
	return result, nil
}

// finalFailed sends the last message of a session that ended without a Stop.
// It is best effort: the player may already be gone.
func (s *session) finalFailed(detail string) {
	if s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, finalPushBudget)
	defer cancel()
	s.seq++
	_, err := s.push(ctx, PushRequest{
		Sequence:     s.seq,
		SessionID:    s.id,
		SampleRateHz: s.rate,
		Channels:     playerChannels,
		SampleFormat: uint32(capsulad.SampleFormatPCM16LE),
		Failed:       true,
		Detail:       detail,
	})
	if err != nil {
		log.Printf("radio-tuner: session %s final failure push: %v", s.id, err)
		return
	}
	log.Printf("radio-tuner: session %s ended: %s", s.id, detail)
}

func reconnectDetail(attempts int, cause error) string {
	if cause == nil {
		return fmt.Sprintf("stream ended after %d reconnect attempts", attempts)
	}
	return fmt.Sprintf("stream ended after %d reconnect attempts: %v", attempts, cause)
}

func stringsOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// clearCurrent removes s from the manager if it is still the live session.
func (m *Manager) clearCurrent(s *session) {
	m.mu.Lock()
	if m.cur == s {
		m.cur = nil
	}
	m.mu.Unlock()
}
