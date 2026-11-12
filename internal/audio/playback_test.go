package audio

import (
	"testing"
	"time"

	"capsulad/internal/backend"
)

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
