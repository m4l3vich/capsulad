package link

import (
	"testing"
	"time"
)

// noJitter pins the random draw so the schedule is deterministic.
func noJitter() float64 { return 0.5 }

func TestDelayDoublesThenCaps(t *testing.T) {
	want := map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		6: 32 * time.Second,
		7: 60 * time.Second, // capped
		9: 60 * time.Second,
	}
	for attempt, d := range want {
		if got := delay(attempt, noJitter); got != d {
			t.Fatalf("delay(%d) = %s want %s", attempt, got, d)
		}
	}
}

func TestDelayJitterBounds(t *testing.T) {
	if got := delay(1, func() float64 { return 0 }); got != 800*time.Millisecond {
		t.Fatalf("low jitter = %s", got)
	}
	if got := delay(1, func() float64 { return 1 }); got != 1200*time.Millisecond {
		t.Fatalf("high jitter = %s", got)
	}
}

func TestNextAttemptResetsAfterAHealthyLink(t *testing.T) {
	if got := nextAttempt(4, resetAfter); got != 1 {
		t.Fatalf("after a healthy link = %d", got)
	}
	if got := nextAttempt(4, resetAfter-time.Second); got != 5 {
		t.Fatalf("after a short link = %d", got)
	}
}
