package capsula

import (
	"context"
	"time"
)

// Service and app identities for the built-in Clock and Ring system apps.
// Calls use the ordinary routed app-bus path; discovery uses
// Framework AppInterfaceService.
const (
	ClockAppID   = "capsula.clock"
	ClockService = "capsula.contracts.clock.v1.ClockService"
	RingAppID    = "capsula.ring"
	RingService  = "capsula.contracts.ring.v1.RingService"
)

const clockCallTimeout = 10 * time.Second

type ClockState struct {
	LocalTimeRfc3339 string        `json:"local_time_rfc3339"`
	Timezone         string        `json:"timezone"`
	Stopwatch        StopwatchInfo `json:"stopwatch"`
	Timers           []TimerInfo   `json:"timers"`
	Alarms           []AlarmInfo   `json:"alarms"`
	ActiveAlerts     []ActiveAlert `json:"active_alerts"`
}

type StopwatchInfo struct {
	State         string `json:"state"`
	ElapsedMs     uint64 `json:"elapsed_ms"`
	StartedUnixMs uint64 `json:"started_unix_ms"`
}

type TimerInfo struct {
	TimerID        string `json:"timer_id"`
	Label          string `json:"label"`
	DurationMs     uint64 `json:"duration_ms"`
	RemainingMs    uint64 `json:"remaining_ms"`
	Status         string `json:"status"`
	StartedUnixMs  uint64 `json:"started_unix_ms"`
	FinishedUnixMs uint64 `json:"finished_unix_ms"`
}

type AlarmInfo struct {
	AlarmID             string `json:"alarm_id"`
	Label               string `json:"label"`
	Hour                uint32 `json:"hour"`
	Minute              uint32 `json:"minute"`
	DaysOfWeek          uint32 `json:"days_of_week"`
	Enabled             bool   `json:"enabled"`
	Timezone            string `json:"timezone"`
	EffectiveTimezone   string `json:"effective_timezone"`
	NextTriggerUnixMs   uint64 `json:"next_trigger_unix_ms"`
	NextTriggerRFC3339  string `json:"next_trigger_rfc3339"`
	LastTriggeredUnixMs uint64 `json:"last_triggered_unix_ms"`
}

type ActiveAlert struct {
	AlertID       string `json:"alert_id"`
	Kind          string `json:"kind"`
	SourceID      string `json:"source_id"`
	Label         string `json:"label"`
	StartedUnixMs uint64 `json:"started_unix_ms"`
}

type AlarmSpec struct {
	AlarmID    string `json:"alarm_id,omitempty"`
	Label      string `json:"label,omitempty"`
	Hour       uint32 `json:"hour,omitempty"`
	Minute     uint32 `json:"minute,omitempty"`
	DaysOfWeek uint32 `json:"days_of_week,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
	Enabled    bool   `json:"enabled,omitempty"`
}

type RingState struct {
	Effect         string  `json:"effect"`
	Palette        string  `json:"palette"`
	Brightness     float32 `json:"brightness"`
	Speed          float32 `json:"speed"`
	PresentationID string  `json:"presentation_id"`
}

func (c *Client) clockCall(ctx context.Context, rpc string, request, response any) error {
	return c.CallJSON(ctx, ClockAppID, ClockService, rpc, request, response, clockCallTimeout)
}

// ClockState reads the complete clock, stopwatch, timer and alarm state.
func (c *Client) ClockState(ctx context.Context) (ClockState, error) {
	var out ClockState
	err := c.clockCall(ctx, "GetState", struct{}{}, &out)
	return out, err
}

// StopwatchStart starts at zero when idle and is a no-op while running.
func (c *Client) StopwatchStart(ctx context.Context) (StopwatchInfo, error) {
	var out StopwatchInfo
	err := c.clockCall(ctx, "StopwatchStart", struct{}{}, &out)
	return out, err
}

// StopwatchPause preserves the accumulated monotonic elapsed time.
func (c *Client) StopwatchPause(ctx context.Context) (StopwatchInfo, error) {
	var out StopwatchInfo
	err := c.clockCall(ctx, "StopwatchPause", struct{}{}, &out)
	return out, err
}

// StopwatchResume continues a paused stopwatch or starts an idle one.
func (c *Client) StopwatchResume(ctx context.Context) (StopwatchInfo, error) {
	var out StopwatchInfo
	err := c.clockCall(ctx, "StopwatchResume", struct{}{}, &out)
	return out, err
}

// StopwatchReset stops the stopwatch and clears elapsed time.
func (c *Client) StopwatchReset(ctx context.Context) (StopwatchInfo, error) {
	var out StopwatchInfo
	err := c.clockCall(ctx, "StopwatchReset", struct{}{}, &out)
	return out, err
}

// StartTimer starts a countdown. Completion is signalled visibly and audibly
// by the clock app through normal presentation captures and audio playback.
func (c *Client) StartTimer(ctx context.Context, duration time.Duration, label string) (TimerInfo, error) {
	var out TimerInfo
	request := struct {
		DurationMs uint64 `json:"duration_ms"`
		Label      string `json:"label,omitempty"`
	}{DurationMs: uint64(duration / time.Millisecond), Label: label}
	err := c.clockCall(ctx, "StartTimer", request, &out)
	return out, err
}

// CancelTimer cancels a running timer.
func (c *Client) CancelTimer(ctx context.Context, timerID string) (TimerInfo, error) {
	var out TimerInfo
	err := c.clockCall(ctx, "CancelTimer", struct {
		TimerID string `json:"timer_id"`
	}{TimerID: timerID}, &out)
	return out, err
}

// ListTimers lists running timers and completed timers whose alert is active.
func (c *Client) ListTimers(ctx context.Context) ([]TimerInfo, error) {
	var out struct {
		Timers []TimerInfo `json:"timers"`
	}
	err := c.clockCall(ctx, "ListTimers", struct{}{}, &out)
	return out.Timers, err
}

// SetAlarm creates an alarm when AlarmID is empty, otherwise upserts it.
func (c *Client) SetAlarm(ctx context.Context, spec AlarmSpec) (AlarmInfo, error) {
	var out AlarmInfo
	err := c.clockCall(ctx, "SetAlarm", spec, &out)
	return out, err
}

// SetAlarmEnabled enables or disables an alarm and recomputes its trigger.
func (c *Client) SetAlarmEnabled(ctx context.Context, alarmID string, enabled bool) (AlarmInfo, error) {
	var out AlarmInfo
	err := c.clockCall(ctx, "SetAlarmEnabled", struct {
		AlarmID string `json:"alarm_id"`
		Enabled bool   `json:"enabled"`
	}{AlarmID: alarmID, Enabled: enabled}, &out)
	return out, err
}

// DeleteAlarm removes a service-created alarm.
func (c *Client) DeleteAlarm(ctx context.Context, alarmID string) (bool, error) {
	var out struct {
		Deleted bool `json:"deleted"`
	}
	err := c.clockCall(ctx, "DeleteAlarm", struct {
		AlarmID string `json:"alarm_id"`
	}{AlarmID: alarmID}, &out)
	return out.Deleted, err
}

// ListAlarms lists alarms with effective timezone and next trigger.
func (c *Client) ListAlarms(ctx context.Context) ([]AlarmInfo, error) {
	var out struct {
		Alarms []AlarmInfo `json:"alarms"`
	}
	err := c.clockCall(ctx, "ListAlarms", struct{}{}, &out)
	return out.Alarms, err
}

// DismissAlert stops a visible/audible timer or alarm alert.
func (c *Client) DismissAlert(ctx context.Context, alertID string) (bool, error) {
	var out struct {
		Dismissed bool `json:"dismissed"`
	}
	err := c.clockCall(ctx, "DismissAlert", struct {
		AlertID string `json:"alert_id"`
	}{AlertID: alertID}, &out)
	return out.Dismissed, err
}

// RingState reads the effective state rendered by the ring system app.
func (c *Client) RingState(ctx context.Context) (RingState, error) {
	var out RingState
	err := c.CallJSON(ctx, RingAppID, RingService, "GetState", struct{}{}, &out, clockCallTimeout)
	return out, err
}
