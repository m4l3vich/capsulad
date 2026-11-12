package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

// The clock app publishes capsula.contracts.clock.v1.ClockService. The Web UI
// discovers the exact service name from the application interface registry and
// talks to it through AppBusService.Call, exactly like a third-party caller.
// The wire format is protobuf JSON with proto field names (snake_case); 64-bit
// integers may arrive as JSON strings, hence clockInt64.

type clockInt64 int64

func (n *clockInt64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" || s == `""` {
		*n = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = clockInt64(v)
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("clock: invalid integer %q", s)
	}
	*n = clockInt64(v)
	return nil
}

type clockStopwatch struct {
	State         string     `json:"state"`
	ElapsedMS     clockInt64 `json:"elapsed_ms"`
	StartedUnixMS clockInt64 `json:"started_unix_ms"`
}

type clockTimer struct {
	TimerID        string     `json:"timer_id"`
	Label          string     `json:"label"`
	DurationMS     clockInt64 `json:"duration_ms"`
	RemainingMS    clockInt64 `json:"remaining_ms"`
	Status         string     `json:"status"`
	StartedUnixMS  clockInt64 `json:"started_unix_ms"`
	FinishedUnixMS clockInt64 `json:"finished_unix_ms"`
}

type clockAlarm struct {
	AlarmID             string     `json:"alarm_id"`
	Label               string     `json:"label"`
	Hour                clockInt64 `json:"hour"`
	Minute              clockInt64 `json:"minute"`
	DaysOfWeek          clockInt64 `json:"days_of_week"`
	Enabled             bool       `json:"enabled"`
	Timezone            string     `json:"timezone"`
	EffectiveTimezone   string     `json:"effective_timezone"`
	NextTriggerUnixMS   clockInt64 `json:"next_trigger_unix_ms"`
	NextTriggerRFC3339  string     `json:"next_trigger_rfc3339"`
	LastTriggeredUnixMS clockInt64 `json:"last_triggered_unix_ms"`
}

type clockAlert struct {
	AlertID       string     `json:"alert_id"`
	Kind          string     `json:"kind"`
	SourceID      string     `json:"source_id"`
	Label         string     `json:"label"`
	StartedUnixMS clockInt64 `json:"started_unix_ms"`
}

type clockState struct {
	LocalTimeRFC3339 string          `json:"local_time_rfc3339"`
	Timezone         string          `json:"timezone"`
	Stopwatch        *clockStopwatch `json:"stopwatch"`
	Timers           []clockTimer    `json:"timers"`
	Alarms           []clockAlarm    `json:"alarms"`
	ActiveAlerts     []clockAlert    `json:"active_alerts"`
}

// clockAlarmForm carries create/edit defaults. An empty AlarmID creates a new
// alarm; otherwise the same fields replace that alarm.
type clockAlarmForm struct {
	AlarmID  string
	Label    string
	Hour     clockInt64
	Minute   clockInt64
	Days     clockInt64
	Timezone string
	Enabled  bool
}

func defaultClockAlarmForm() clockAlarmForm {
	return clockAlarmForm{Label: "Alarm", Hour: 7, Enabled: true}
}

// findClockService identifies the clock app by server-published interface
// metadata, never by a hardcoded service string or app ID.
func (s *Server) findClockService(ctx context.Context, appID string) (string, error) {
	ifaces, err := frameworkv1.NewAppInterfaceServiceClient(s.Conn).ListInterfaces(ctx,
		&frameworkv1.ListAppInterfacesRequest{AppId: appID})
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces.GetInterfaces() {
		desc, err := frameworkv1.NewAppInterfaceServiceClient(s.Conn).DescribeInterface(ctx,
			&frameworkv1.DescribeAppInterfaceRequest{AppId: appID, GrpcService: iface.GrpcService})
		if err != nil {
			continue
		}
		methods := map[string]bool{}
		for _, m := range desc.GetMethods() {
			methods[m.RpcName] = true
		}
		if methods["GetState"] && methods["StopwatchStart"] && methods["SetAlarm"] &&
			methods["StartTimer"] && methods["CancelTimer"] {
			return iface.GrpcService, nil
		}
	}
	return "", fmt.Errorf("no clock service is published by this application")
}

// routedJSONCall performs one unary app RPC using protobuf JSON. The bus is
// opaque to both sides; the clock service decodes proto JSON with proto names.
func (s *Server) routedJSONCall(ctx context.Context, appID, service, rpc string, request any) ([]byte, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	msgs, _, end, err := s.routedCall(ctx, appID, service, rpc, body)
	if err != nil {
		return nil, err
	}
	if end != nil && end.GrpcStatusCode != 0 {
		detail := strings.TrimSpace(end.Detail)
		if detail == "" {
			detail = "application returned an error"
		}
		return nil, fmt.Errorf("%s: %s", codes.Code(end.GrpcStatusCode), detail)
	}
	if len(msgs) != 1 {
		return nil, fmt.Errorf("expected one response message, got %d", len(msgs))
	}
	return msgs[0], nil
}

func (s *Server) clockSnapshot(ctx context.Context, appID, service string) (*clockState, error) {
	raw, err := s.routedJSONCall(ctx, appID, service, "GetState", struct{}{})
	if err != nil {
		return nil, err
	}
	return decodeClockState(raw)
}

func decodeClockState(raw []byte) (*clockState, error) {
	var state clockState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode clock state: %w", err)
	}
	// Proto JSON omits zero-valued fields. The zero enum values are real
	// states, not "unknown", so restore them before the template sees them.
	if state.Stopwatch != nil && state.Stopwatch.State == "" {
		state.Stopwatch.State = "STOPWATCH_STATUS_IDLE"
	}
	for i := range state.Timers {
		if state.Timers[i].Status == "" {
			state.Timers[i].Status = "TIMER_STATUS_RUNNING"
		}
	}
	for i := range state.ActiveAlerts {
		if state.ActiveAlerts[i].Kind == "" {
			state.ActiveAlerts[i].Kind = "ALERT_KIND_TIMER"
		}
	}
	return &state, nil
}

func (s *Server) clockAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r)
	defer cancel()
	appID := r.PathValue("id")
	back := "/applications/" + appID
	service, err := s.findClockService(ctx, appID)
	if err != nil {
		s.redirect(w, r, back, "", rpcMessage(err))
		return
	}
	action := r.PostFormValue("action")
	rpc := ""
	var request any = struct{}{}
	msg := ""
	switch action {
	case "stopwatch_start":
		rpc, msg = "StopwatchStart", "stopwatch started"
	case "stopwatch_pause":
		rpc, msg = "StopwatchPause", "stopwatch paused"
	case "stopwatch_resume":
		rpc, msg = "StopwatchResume", "stopwatch resumed"
	case "stopwatch_reset":
		rpc, msg = "StopwatchReset", "stopwatch reset"
	case "timer_start":
		seconds, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("duration_s")))
		if err != nil || seconds < 1 || seconds > 2592000 {
			s.redirect(w, r, back, "", "timer duration must be a whole number of seconds between 1 and 2592000 (30 days)")
			return
		}
		rpc = "StartTimer"
		msg = "timer started"
		request = map[string]any{
			"duration_ms": int64(seconds) * 1000,
			"label":       strings.TrimSpace(r.PostFormValue("label")),
		}
	case "timer_cancel":
		id := strings.TrimSpace(r.PostFormValue("timer_id"))
		if id == "" {
			s.redirect(w, r, back, "", "timer id is required")
			return
		}
		rpc, msg = "CancelTimer", "timer cancelled"
		request = map[string]any{"timer_id": id}
	case "alarm_set":
		form, err := alarmFormFromRequest(r)
		if err != nil {
			s.redirect(w, r, back, "", err.Error())
			return
		}
		rpc = "SetAlarm"
		if form.AlarmID == "" {
			msg = "alarm created"
		} else {
			msg = "alarm updated"
		}
		request = map[string]any{
			"alarm_id":     form.AlarmID,
			"label":        form.Label,
			"hour":         int(form.Hour),
			"minute":       int(form.Minute),
			"days_of_week": int(form.Days),
			"timezone":     form.Timezone,
			"enabled":      form.Enabled,
		}
	case "alarm_enable", "alarm_disable":
		id := strings.TrimSpace(r.PostFormValue("alarm_id"))
		if id == "" {
			s.redirect(w, r, back, "", "alarm id is required")
			return
		}
		rpc = "SetAlarmEnabled"
		enabled := action == "alarm_enable"
		if enabled {
			msg = "alarm enabled"
		} else {
			msg = "alarm disabled"
		}
		request = map[string]any{"alarm_id": id, "enabled": enabled}
	case "alarm_delete":
		id := strings.TrimSpace(r.PostFormValue("alarm_id"))
		if id == "" {
			s.redirect(w, r, back, "", "alarm id is required")
			return
		}
		rpc, msg = "DeleteAlarm", "alarm deleted"
		request = map[string]any{"alarm_id": id}
	case "alert_dismiss":
		id := strings.TrimSpace(r.PostFormValue("alert_id"))
		if id == "" {
			s.redirect(w, r, back, "", "alert id is required")
			return
		}
		rpc, msg = "DismissAlert", "alert dismissed"
		request = map[string]any{"alert_id": id}
	default:
		s.redirect(w, r, back, "", "unknown clock action")
		return
	}
	if _, err := s.routedJSONCall(ctx, appID, service, rpc, request); err != nil {
		s.redirect(w, r, back, "", rpcMessage(err))
		return
	}
	s.redirect(w, r, back, msg, "")
}

func alarmFormFromRequest(r *http.Request) (clockAlarmForm, error) {
	hour, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("hour")))
	if err != nil || hour < 0 || hour > 23 {
		return clockAlarmForm{}, fmt.Errorf("alarm hour must be a whole number between 0 and 23")
	}
	minute, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("minute")))
	if err != nil || minute < 0 || minute > 59 {
		return clockAlarmForm{}, fmt.Errorf("alarm minute must be a whole number between 0 and 59")
	}
	var days clockInt64
	for day := range 7 {
		if r.PostFormValue(fmt.Sprintf("day_%d", day)) == "1" {
			days |= 1 << day
		}
	}
	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" {
		label = "Alarm"
	}
	return clockAlarmForm{
		AlarmID:  strings.TrimSpace(r.PostFormValue("alarm_id")),
		Label:    label,
		Hour:     clockInt64(hour),
		Minute:   clockInt64(minute),
		Days:     days,
		Timezone: strings.TrimSpace(r.PostFormValue("alarm_timezone")),
		Enabled:  r.PostFormValue("enabled") == "on",
	}, nil
}

// clockStatus trims proto enum prefixes for display without changing the
// underlying stored value.
func clockStatus(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	for _, prefix := range []string{"stopwatch_status_", "timer_status_", "alert_kind_", "stopwatch_", "timer_", "alert_"} {
		s = strings.TrimPrefix(s, prefix)
	}
	return s
}

func clockDuration(ms clockInt64) string {
	if ms <= 0 {
		return "0s"
	}
	total := int64(math.Round(float64(ms) / 1000))
	h, m, s := total/3600, (total%3600)/60, total%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func clockDays(mask clockInt64) string {
	if mask == 0 {
		return "one-shot"
	}
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	var out []string
	for i, name := range names {
		if mask&(1<<i) != 0 {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return "one-shot"
	}
	return strings.Join(out, ", ")
}

// rpcMessage extracts the server-authored message from a gRPC error so the
// browser shows the app's validation text instead of the transport wrapper.
func rpcMessage(err error) string {
	if err == nil {
		return ""
	}
	if msg := status.Convert(err).Message(); msg != "" {
		return msg
	}
	return err.Error()
}

func clockHasDay(mask clockInt64, day int) bool {
	return day >= 0 && day < 7 && mask&(1<<day) != 0
}
