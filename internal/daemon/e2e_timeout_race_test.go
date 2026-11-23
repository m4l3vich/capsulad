//go:build race

package daemon

// e2eTimeoutScale stretches the daemon journeys' observation deadlines under
// the race detector. See waitFor.
const e2eTimeoutScale = 3
