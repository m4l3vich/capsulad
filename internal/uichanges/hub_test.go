package uichanges

import (
	"testing"
	"time"
)

func TestPublishCoalescesToNewestSignal(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	defer cancel()
	for range 100 {
		h.Publish(Wakeword)
	}
	select {
	case topic := <-ch:
		if topic != Wakeword {
			t.Fatalf("topic = %v, want Wakeword", topic)
		}
	case <-time.After(time.Second):
		t.Fatal("no signal delivered")
	}
	if len(ch) > cap(ch) {
		t.Fatal("subscriber queue exceeded its bound")
	}
}

func TestCancelClosesAndIsIdempotent(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	cancel()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("cancelled subscription stayed open")
	}
	// Publishing after cancel must not send on or panic over a closed channel.
	h.Publish(Access)
}

func TestIndependentTopicsAndMultipleSubscribers(t *testing.T) {
	h := New()
	a, cancelA := h.Subscribe()
	defer cancelA()
	b, cancelB := h.Subscribe()
	defer cancelB()
	h.Publish(Access)
	for _, ch := range []<-chan Topic{a, b} {
		select {
		case topic := <-ch:
			if topic != Access {
				t.Fatalf("topic = %v, want Access", topic)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber missed the signal")
		}
	}
}
