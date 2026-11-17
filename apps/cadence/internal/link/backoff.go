package link

import "time"

// The reconnect policy mirrors cadence-device's link: 1 s doubling to 60 s with
// ±20 % jitter, and a link that stayed up for a minute restarts at 1 s.
const (
	firstDelay = time.Second
	maxDelay   = 60 * time.Second
	resetAfter = 60 * time.Second
)

// delay is the wait before reconnect attempt n (from 1).
func delay(attempt int, rnd func() float64) time.Duration {
	d := firstDelay
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rnd()))
}

// nextAttempt numbers the attempt after a link that was up for up.
func nextAttempt(attempt int, up time.Duration) int {
	if up >= resetAfter {
		return 1
	}
	return attempt + 1
}
