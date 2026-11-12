package webui

import "testing"

// Proto JSON omits zero-valued fields. The zero enum values are real states:
// an idle stopwatch, a running timer and a timer alert must not render blank.
func TestDecodeClockStateRestoresZeroEnumDefaults(t *testing.T) {
	state, err := decodeClockState([]byte(`{
		"local_time_rfc3339":"2026-11-12T12:00:00Z",
		"stopwatch":{"elapsed_ms":"0"},
		"timers":[{"timer_id":"t1","status":"TIMER_STATUS_COMPLETED"},{"timer_id":"t2"}],
		"active_alerts":[{"alert_id":"a1","label":"timer"},{"alert_id":"a2","kind":"ALERT_KIND_ALARM"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if state.Stopwatch == nil || state.Stopwatch.State != "STOPWATCH_STATUS_IDLE" {
		t.Fatalf("stopwatch state = %+v, want IDLE default", state.Stopwatch)
	}
	if got := state.Timers[0].Status; got != "TIMER_STATUS_COMPLETED" {
		t.Fatalf("completed timer status = %q, want preserved", got)
	}
	if got := state.Timers[1].Status; got != "TIMER_STATUS_RUNNING" {
		t.Fatalf("omitted timer status = %q, want RUNNING default", got)
	}
	if got := state.ActiveAlerts[0].Kind; got != "ALERT_KIND_TIMER" {
		t.Fatalf("omitted alert kind = %q, want TIMER default", got)
	}
	if got := state.ActiveAlerts[1].Kind; got != "ALERT_KIND_ALARM" {
		t.Fatalf("alarm alert kind = %q, want preserved", got)
	}
}
