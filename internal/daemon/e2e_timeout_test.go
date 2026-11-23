//go:build !race

package daemon

// e2eTimeoutScale keeps the daemon journeys' observation deadlines unchanged
// in a normal build. See waitFor.
const e2eTimeoutScale = 1
