// Command devtools is a development-only supervised app that exercises the
// Capsula Mini's hardware and framework surfaces through the public app API.
//
// Steps are serialized by one worker. The SDK's event handlers only validate
// an invocation/action and enqueue a step; presentations, captures, audio
// handles and streams are owned and released inside the step that opened them,
// so no callback outlives a step and no private socket is ever used.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"capsula/sdk/capsula"
)

const (
	deviceService     = "capsula.framework.v1.DeviceService"
	configService     = "capsula.framework.v1.ApplicationConfigurationService"
	echoService       = "dev.devtools.v1.DevToolsService"
	scriptEchoService = "dev.devtools.v1.ScriptEcho"
	segmentResource   = "display.segment.front"
	ringResource      = "light.ring.main"
	touchResource     = "touch.panel.main"
	outputEndpoint    = "audio.output.speaker"
	callTimeout       = 5 * time.Second
	peerTimeout       = 2 * time.Second
	queueDepth        = 4
)

// A simulator or a muted device may legitimately lack a surface; that is a
// first-class "unsupported" result, not an app error.
const (
	statusOK          = "ok"
	statusUnsupported = "unsupported"
	statusError       = "error"
)

var stepNames = []string{"selftest", "info", "display", "ring", "audio", "input", "settings", "echo"}

type stepRequest struct {
	step, source, invocationID, actionID string
}

type stepResult struct {
	status, detail string
	ms             int64
}

func okResult(format string, args ...any) stepResult {
	return stepResult{status: statusOK, detail: fmt.Sprintf(format, args...)}
}
func unsupported(format string, args ...any) stepResult {
	return stepResult{status: statusUnsupported, detail: fmt.Sprintf(format, args...)}
}
func errorResult(format string, args ...any) stepResult {
	return stepResult{status: statusError, detail: fmt.Sprintf(format, args...)}
}

type devApp struct {
	ctx            context.Context
	client         *capsula.Client
	appID, runID   string
	echoJSON       bool
	queue          chan stepRequest
	mu             sync.Mutex
	gen            uint64
	settingsWrites int
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	client, err := capsula.Connect(ctx, env)
	if err != nil {
		log.Fatalf("devtools: connect: %v", err)
	}
	defer client.Close()
	a := &devApp{
		ctx: ctx, client: client, appID: env.AppID, runID: env.RunID,
		queue: make(chan stepRequest, queueDepth),
	}
	a.registerEcho()
	client.SetHandlers(capsula.Handlers{
		Ready: func() {
			log.Printf("devtools: ready app=%s run=%s sdk=%s echo=%s",
				env.AppID, env.RunID, env.SDK, a.echoMode())
		},
		Invocation: func(_ context.Context, inv *capsula.LaunchInvocation) {
			step := invokeStep(inv)
			log.Printf("devtools: invocation id=%s step=%s", inv.ID, step)
			if !validStep(step) {
				_ = client.NotifyInvocationResult(inv.ID, false,
					fmt.Sprintf("unknown step %q; use %s", step, strings.Join(stepNames, ", ")))
				return
			}
			if !a.enqueue(stepRequest{step: step, source: "invocation " + inv.ID, invocationID: inv.ID}) {
				_ = client.NotifyInvocationResult(inv.ID, false, "step queue is full")
			}
		},
		Action: func(_ context.Context, act *capsula.ActionInvoked) {
			log.Printf("devtools: action id=%s action=%s", act.ID, act.ActionID)
			if !validStep(act.ActionID) {
				_ = client.NotifyActionResult(act.ID, false, "unknown action")
				return
			}
			if !a.enqueue(stepRequest{step: act.ActionID, source: "action " + act.ID, actionID: act.ID}) {
				_ = client.NotifyActionResult(act.ID, false, "step queue is full")
			}
		},
		Settings: func(s *capsula.SettingsChanged) {
			log.Printf("devtools: settings-changed revision=%d note=%q", s.Revision, settingFromEvent(s, "note"))
		},
		Disconnected: func(err error) { log.Printf("devtools: disconnected: %v", err) },
	})
	go a.worker()
	log.Printf("devtools: starting bus app=%s run=%s", env.AppID, env.RunID)
	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Printf("devtools: bus stopped: %v", err)
	}
}

func (a *devApp) echoMode() string {
	if a.echoJSON {
		return "json"
	}
	return "raw-proto"
}

func (a *devApp) enqueue(req stepRequest) bool {
	select {
	case a.queue <- req:
		return true
	default:
		log.Printf("devtools: step=%s source=%s rejected; queue is full", req.step, req.source)
		return false
	}
}

func (a *devApp) worker() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case req := <-a.queue:
			a.execute(req)
		}
	}
}

func (a *devApp) execute(req stepRequest) {
	a.mu.Lock()
	a.gen++
	gen := a.gen
	a.mu.Unlock()
	started := time.Now()
	ctx, cancel := context.WithTimeout(a.ctx, stepDeadline(req.step))
	defer cancel()
	res := a.report(req.step, gen, started, a.runStep(ctx, req.step, gen))
	accepted, detail := res.status != statusError, res.status+": "+res.detail
	if req.invocationID != "" {
		_ = a.client.NotifyInvocationResult(req.invocationID, accepted, detail)
	}
	if req.actionID != "" {
		_ = a.client.NotifyActionResult(req.actionID, accepted, detail)
	}
}

// report logs one structured step result and returns it with its duration.
func (a *devApp) report(step string, gen uint64, started time.Time, res stepResult) stepResult {
	res.ms = time.Since(started).Milliseconds()
	log.Printf("devtools: result step=%s gen=%d status=%s duration_ms=%d detail=%q",
		step, gen, res.status, res.ms, res.detail)
	return res
}

func (a *devApp) runStep(ctx context.Context, step string, gen uint64) stepResult {
	switch step {
	case "selftest":
		return a.selftest(ctx, gen)
	case "info":
		return a.stepInfo(ctx, gen)
	case "display":
		return a.stepDisplay(ctx, gen)
	case "ring":
		return a.stepRing(ctx, gen)
	case "audio":
		return a.stepAudio(ctx, gen)
	case "input":
		return a.stepInput(ctx, gen)
	case "settings":
		return a.stepSettings(ctx, gen)
	case "echo":
		return a.stepEcho(ctx, gen)
	}
	return errorResult("unknown step %q", step)
}

// selftest runs every step in order; one unsupported or failed surface does not
// abort the rest, and each step releases its own resources on return.
func (a *devApp) selftest(ctx context.Context, gen uint64) stepResult {
	steps := []string{"info", "display", "ring", "input", "audio", "settings", "echo"}
	counts := map[string]int{}
	var failed []string
	for _, step := range steps {
		res := a.report(step, gen, time.Now(), a.runStep(ctx, step, gen))
		counts[res.status]++
		if res.status == statusError {
			failed = append(failed, step)
		}
		if ctx.Err() != nil {
			break
		}
	}
	detail := fmt.Sprintf("steps=%d ok=%d unsupported=%d failed=%d",
		len(steps), counts[statusOK], counts[statusUnsupported], counts[statusError])
	if len(failed) > 0 {
		return errorResult("%s failed_steps=%s", detail, strings.Join(failed, ","))
	}
	if counts[statusOK] == 0 {
		return unsupported("%s (no step could use this backend)", detail)
	}
	return okResult("%s", detail)
}

func stepDeadline(step string) time.Duration {
	switch step {
	case "selftest":
		return 90 * time.Second
	case "input", "audio":
		return 20 * time.Second
	}
	return 15 * time.Second
}

func validStep(step string) bool {
	for _, name := range stepNames {
		if name == step {
			return true
		}
	}
	return false
}

// invokeStep reads the manifest-typed "step" argument; empty runs the full
// self-test, so `capsulad-cli app start dev.hardware` is useful on its own.
func invokeStep(inv *capsula.LaunchInvocation) string {
	if inv != nil {
		for _, arg := range inv.Arguments {
			if arg.Name == "step" {
				if step := strings.ToLower(strings.TrimSpace(arg.Value)); step != "" {
					return step
				}
			}
		}
	}
	return "selftest"
}

// classify maps framework statuses to step results. Unimplemented,
// precondition, not-found and unavailable mean "this backend cannot do it".
func classify(op string, err error) stepResult {
	if err == nil {
		return okResult("%s", op)
	}
	var status *capsula.StatusError
	if errors.As(err, &status) {
		switch status.Code {
		case 5, 9, 12, 14: // NotFound, FailedPrecondition, Unimplemented, Unavailable
			return unsupported("%s: %s", op, status.Message)
		}
		return errorResult("%s: %v", op, err)
	}
	if errors.Is(err, context.Canceled) {
		return errorResult("%s: cancelled", op)
	}
	return errorResult("%s: %v", op, err)
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func settingFromEvent(s *capsula.SettingsChanged, key string) string {
	for _, value := range s.Values {
		if value.Key == key {
			return value.ValueJSON
		}
	}
	return ""
}

// --- echo --------------------------------------------------------------

type echoRequest struct {
	Text string `json:"text,omitempty"`
}
type echoResponse struct {
	Text  string `json:"text,omitempty"`
	AppID string `json:"appId,omitempty"`
	RunID string `json:"runId,omitempty"`
}

// registerEcho installs the AppBus handler. With proto/devtools.protoset both
// sides use canonical protobuf JSON; without it the handler and self-call use
// the wire format of a single length-delimited string field.
func (a *devApp) registerEcho() {
	path := filepath.Join(packageRoot(), "proto", "devtools.protoset")
	raw, err := os.ReadFile(path)
	if err == nil {
		err = a.client.RegisterDescriptors(raw)
	}
	if err != nil {
		log.Printf("devtools: descriptor %s: %v; using raw protobuf echo", path, err)
		a.client.Handle(echoService, "Echo", a.handleEchoRaw)
		return
	}
	a.echoJSON = true
	a.client.Handle(echoService, "Echo", a.handleEchoJSON)
}

func (a *devApp) handleEchoJSON(_ context.Context, call *capsula.ServerCall) {
	for {
		var req echoRequest
		if err := call.RecvJSON(&req); err != nil {
			if err == io.EOF {
				_ = call.End(0, "")
			} else {
				_ = call.End(13, "decode echo request: "+err.Error())
			}
			return
		}
		if err := call.SendJSON(echoResponse{Text: "echo:" + req.Text, AppID: a.appID, RunID: a.runID}); err != nil {
			_ = call.End(13, "encode echo response: "+err.Error())
			return
		}
	}
}

func (a *devApp) handleEchoRaw(_ context.Context, call *capsula.ServerCall) {
	for {
		payload, err := call.Recv()
		if err == io.EOF {
			_ = call.End(0, "")
			return
		}
		if err != nil {
			return
		}
		text, err := decodeProtoString(payload)
		if err != nil {
			_ = call.End(3, "invalid echo request: "+err.Error())
			return
		}
		if err := call.Send(encodeProtoString("echo:" + text)); err != nil {
			return
		}
	}
}

// encodeProtoString writes protobuf field 1 as a length-delimited UTF-8 string.
func encodeProtoString(text string) []byte {
	buf := []byte{0x0a}
	for n := uint64(len(text)); ; n >>= 7 {
		if n < 0x80 {
			buf = append(buf, byte(n))
			break
		}
		buf = append(buf, byte(n)|0x80)
	}
	return append(buf, text...)
}

func decodeProtoString(payload []byte) (string, error) {
	if len(payload) < 2 || payload[0] != 0x0a {
		return "", errors.New("expected string field 1")
	}
	var length uint64
	var used int
	for i, b := range payload[1:] {
		if i == 10 {
			return "", errors.New("varint is too long")
		}
		length |= uint64(b&0x7f) << (7 * uint(i))
		used = i + 1
		if b < 0x80 {
			break
		}
	}
	start := 1 + used
	if start > len(payload) || length > uint64(len(payload)-start) {
		return "", errors.New("declared length exceeds payload")
	}
	text := payload[start : start+int(length)]
	if !utf8.Valid(text) {
		return "", errors.New("field is not valid UTF-8")
	}
	return string(text), nil
}

func (a *devApp) selfEcho(ctx context.Context) (string, string, error) {
	if a.echoJSON {
		var resp echoResponse
		err := a.client.CallJSON(ctx, a.appID, echoService, "Echo",
			echoRequest{Text: "selftest"}, &resp, callTimeout)
		return resp.Text, "json", err
	}
	reply, err := a.client.Call(ctx, a.appID, echoService, "Echo",
		encodeProtoString("selftest"), callTimeout)
	if err != nil {
		return "", "raw-proto", err
	}
	text, err := decodeProtoString(reply)
	return text, "raw-proto", err
}

func (a *devApp) peerEcho(ctx context.Context, peer, text string) (string, error) {
	reply, err := a.client.Call(ctx, peer, scriptEchoService, "Echo",
		encodeProtoString(text), peerTimeout)
	if err != nil {
		return "", err
	}
	return decodeProtoString(reply)
}

// --- settings ----------------------------------------------------------

type settingState struct {
	Key       string `json:"key"`
	ValueJSON string `json:"valueJson"`
	HasValue  bool   `json:"hasValue"`
}
type configSnapshot struct {
	AppID    string         `json:"appId"`
	Revision uint64         `json:"revision"`
	Values   []settingState `json:"values"`
}
type configChange struct {
	Key       string `json:"key"`
	ValueJSON string `json:"valueJson"`
}
type updateConfigRequest struct {
	AppID            string         `json:"appId"`
	ExpectedRevision uint64         `json:"expectedRevision"`
	Changes          []configChange `json:"changes"`
}

func (a *devApp) configGet(ctx context.Context) (configSnapshot, error) {
	var snap configSnapshot
	err := a.client.CallJSON(ctx, "", configService, "Get",
		struct {
			AppID string `json:"appId"`
		}{a.appID}, &snap, callTimeout)
	return snap, err
}

func (a *devApp) configUpdateNote(ctx context.Context, revision uint64, value string) (configSnapshot, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return configSnapshot{}, err
	}
	var snap configSnapshot
	err = a.client.CallJSON(ctx, "", configService, "Update", updateConfigRequest{
		AppID: a.appID, ExpectedRevision: revision,
		Changes: []configChange{{Key: "note", ValueJSON: string(encoded)}},
	}, &snap, callTimeout)
	return snap, err
}

func settingValue(snap configSnapshot, key string) (string, bool) {
	for _, value := range snap.Values {
		if value.Key == key {
			return value.ValueJSON, value.HasValue
		}
	}
	return "", false
}

// --- resources ---------------------------------------------------------

type segmentCaps struct {
	DigitCount    uint32 `json:"digitCount"`
	SupportsColon bool   `json:"supportsColon"`
}
type ringCaps struct {
	PixelCount uint32 `json:"pixelCount"`
}
type touchCaps struct {
	PartialGestures bool     `json:"partialGesturesAvailable"`
	FullRaw         bool     `json:"fullRawAvailable"`
	FullPanel       bool     `json:"fullPanelAvailable"`
	Semantic        []string `json:"semanticGestures"`
}
type resourceInfo struct {
	ResourceID string       `json:"resourceId"`
	Kind       string       `json:"kind"`
	Available  bool         `json:"available"`
	Segment    *segmentCaps `json:"segment,omitempty"`
	Ring       *ringCaps    `json:"ring,omitempty"`
	Touch      *touchCaps   `json:"touch,omitempty"`
}
type resourceList struct {
	Revision  uint64         `json:"revision"`
	Resources []resourceInfo `json:"resources"`
}

func (a *devApp) resources(ctx context.Context) ([]resourceInfo, error) {
	var list resourceList
	err := a.client.CallJSON(ctx, "", deviceService, "ListResources", struct{}{}, &list, callTimeout)
	return list.Resources, err
}

func findResource(resources []resourceInfo, resourceID, kind string) *resourceInfo {
	for i := range resources {
		if resources[i].ResourceID == resourceID && resources[i].Kind == kind {
			return &resources[i]
		}
	}
	return nil
}

func packageRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
