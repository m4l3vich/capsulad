package turn

import (
	"io"
	"log"
	"testing"
	"time"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
)

// A full controller queue may drop advisory frames, but a terminal StreamEnd
// must wait for room instead of disappearing with playback and ducking open.
func TestTerminalEnvelopeWaitsForQueueRoom(t *testing.T) {
	c := New(Deps{Log: log.New(io.Discard, "", 0)})
	for i := 0; i < cap(c.events); i++ {
		c.events <- linkEvent{}
	}
	done := make(chan struct{})
	go func() {
		c.Message(1, &cadencev2.Envelope{Body: &cadencev2.Envelope_StreamEnd{
			StreamEnd: &cadencev2.StreamEnd{StreamId: "an1"},
		}})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("terminal envelope was accepted into a full queue")
	case <-time.After(50 * time.Millisecond):
	}
	<-c.events
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal envelope did not enter after room appeared")
	}
	found := false
	for {
		select {
		case ev := <-c.events:
			if ev.env.GetStreamEnd() != nil {
				found = true
			}
			continue
		default:
		}
		break
	}
	if !found {
		t.Fatal("terminal envelope was not enqueued")
	}
}

// Link-down is a terminal lifecycle event; it must not be dropped when the
// controller queue is full.
func TestLinkDownWaitsForQueueRoom(t *testing.T) {
	c := New(Deps{Log: log.New(io.Discard, "", 0)})
	for i := 0; i < cap(c.events); i++ {
		c.events <- linkEvent{}
	}
	done := make(chan struct{})
	go func() {
		c.LinkChanged(false, 1)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("link-down was accepted into a full queue")
	case <-time.After(50 * time.Millisecond):
	}
	<-c.events
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("link-down did not enter after room appeared")
	}
}
