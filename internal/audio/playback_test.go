package audio

import (
	"encoding/binary"
	"testing"
	"time"

	"capsulad/internal/backend"
)

// s16Constant builds mono 16-bit PCM holding one constant level.
func s16Constant(level int, frames int) []byte {
	pcm := make([]byte, frames*2)
	for i := range frames {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(level))
	}
	return pcm
}

// TestFlushKeepsAudioPublishedAfterIt pins the Simon cue failure: the app
// flushed the sounding backlog and wrote the next cue immediately afterwards,
// as the audio contract allows. The mixer applied commands on a later tick, so
// the flush also discarded the cue published after it; the first tone of every
// replay, every key confirmation and the error cue were silently dropped.
func TestFlushKeepsAudioPublishedAfterIt(t *testing.T) {
	in := backend.AudioFormat{Format: backend.S16LE, RateHz: 24000, Channels: 1}
	out := backend.AudioFormat{Format: backend.S16LE, RateHz: 48000, Channels: 2}
	p := &Playback{
		format:     in,
		feedFrames: (in.RateHz*mixFrameMS+999)/1000 + 2,
		events:     make(chan PlaybackEvent, 4),
		in:         make(chan playbackChunk, playbackQueue),
		ctl:        make(chan playbackCommand, 4),
		done:       make(chan struct{}),
		conv:       newResampler(in, out),
		gain:       1,
		trim:       1,
	}
	const backlogLevel, cueLevel = 0x1000, 0x2000
	if err := p.Write(s16Constant(backlogLevel, 24)); err != nil {
		t.Fatalf("write backlog: %v", err)
	}
	// The producer publishes the cue after the command; both are waiting when
	// the mixer's next tick applies the flush.
	p.Control("FLUSH")
	if err := p.Write(s16Constant(cueLevel, 48)); err != nil {
		t.Fatalf("write cue: %v", err)
	}

	p.drainControl()
	p.feed()

	if got, want := p.conv.srcFrames(), 48; got != want {
		t.Fatalf("resampler holds %d frames after the flush, want the %d-frame cue only", got, want)
	}
	want := float32(cueLevel) / 32768
	for i, sample := range p.conv.src {
		if sample != want {
			t.Fatalf("resampler sample %d = %v, want the cue at %v", i, sample, want)
		}
	}
	p.mu.Lock()
	queued := p.queuedBytes
	p.mu.Unlock()
	if queued != 0 {
		t.Fatalf("queued bytes = %d after the cue was consumed, want 0", queued)
	}
}

func TestFiniteResampledPlaybackDrains(t *testing.T) {
	m := newAudioManager(t, &fakeDevice{pace: time.Millisecond})
	defer m.Close()
	p, err := m.OpenPlayback("test.audio", "run", "audio.output.speaker", backend.AudioFormat{Format: backend.S16LE, RateHz: 24000, Channels: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	// A 100 ms clip at an exact 1:2 rate ratio used to strand one sample
	// forever, keeping playback and its owner alive after the clip ended.
	if err := p.Write(make([]byte, 2400*2)); err != nil {
		t.Fatal(err)
	}
	p.CloseInput()
	deadline := time.After(time.Second)
	for {
		select {
		case ev, ok := <-p.Events():
			if !ok {
				return
			}
			if ev.Notice != nil && ev.Notice.Code == "STREAM_CLOSED" {
				return
			}
		case <-deadline:
			t.Fatal("finite resampled clip never completed")
		}
	}
}

func TestPausedPlaybackBackpressuresAndCloseUnblocksWriter(t *testing.T) {
	m := newAudioManager(t, &fakeDevice{pace: time.Millisecond})
	defer m.Close()
	p, err := m.OpenPlayback("test.audio", "run", "audio.output.speaker", m.PlaybackFormat(), "")
	if err != nil {
		t.Fatal(err)
	}
	p.Control("PAUSE")
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		paused := p.paused
		p.mu.Unlock()
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("playback did not pause")
		}
		time.Sleep(time.Millisecond)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		pcm := make([]byte, 480*2*2)
		if err := p.Write(pcm); err != nil {
			done <- err
			return
		}
		close(started)
		for range 63 {
			if err := p.Write(pcm); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("writer did not start: %v", err)
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("paused playback absorbed a whole clip instead of applying backpressure: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	p.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed playback accepted the remaining clip")
		}
	case <-time.After(time.Second):
		t.Fatal("closing playback did not unblock its writer")
	}
}
