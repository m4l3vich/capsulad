// Command homeassistant exposes a Capsula Mini to Home Assistant.
//
// The bridge talks to Home Assistant the way Home Assistant expects a local
// device to: MQTT discovery. It publishes retained discovery payloads for the
// surfaces the device actually advertises, publishes their state, and applies
// the commands Home Assistant sends back. Nothing about Home Assistant is
// hard-coded into the framework: the app is an ordinary supervised app that
// reads framework services and writes output through presentations.
//
// # Interaction contract
//
// Trigger: the app is installed, started and configured with a broker. It is a
// background-capable app that runs as one instance, so a launch while it is
// running is delivered as an invocation and forces a discovery/state republish.
//
// Feedback: availability is a retained "online"/"offline" topic with a broker
// last will for the crash path, a diagnostic "Bridge status" sensor with JSON
// attributes, and one state topic per entity. Every applied command republishes
// the state it changed. A refused command is logged and reflected in the status
// sensor; the device never changes state because of a message the bridge did not
// understand.
//
// Ownership: the bridge owns the ring and the segment display only while it is
// driving them. Turning a light off or switching the display overlay off
// releases the capture, so the device's configured default app resumes. The
// optional touch capture is partial: the framework keeps the volume and
// microphone gestures.
//
// Interruption and dismissal: a settings change stops the runtime, releases
// every presentation and capture, and starts again with the new configuration.
// The "Release hardware" action or MQTT button hands the ring and display back
// immediately. Stopping the app publishes offline before its clean DISCONNECT so
// Home Assistant does not keep acting on a device that is gone.
//
// Final state: on stop the bridge publishes offline, disconnects, releases every
// presentation, capture and stream, and exits. The device is back on its
// configured baseline.
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
	"strings"
	"sync"
	"syscall"
	"time"

	"capsulad/apps/homeassistant/internal/appcfg"
	"capsulad/apps/homeassistant/internal/ha"
	"capsulad/apps/homeassistant/internal/hw"
	haproto "capsulad/apps/homeassistant/proto"
	"capsulad/sdk/capsulad"
)

const (
	// interfaceService is the app interface published on the app bus.
	interfaceService = "org.capsulad.homeassistant.v1.HomeAssistantService"

	// Action ids, declared in manifest.toml.
	actionRepublish = "republish"
	actionReconnect = "reconnect"
	actionRelease   = "release_hardware"

	// probeTimeout bounds the startup device probe.
	probeTimeout = 10 * time.Second
	// setupRetry is the pause before retrying a failed device probe.
	setupRetry = 15 * time.Second
	// actionTimeout bounds one action or interface call.
	actionTimeout = 10 * time.Second
	// stopTimeout bounds how long a stop waits for the runtime to wind down.
	stopTimeout = 10 * time.Second
	// maxEventPayload bounds a PublishEvent payload.
	maxEventPayload = 4096
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsulad.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsulad.Connect(ctx, env)
	if err != nil {
		log.Fatalf("homeassistant: connect: %v", err)
	}
	defer client.Close()

	a := newApp(ctx, env, client)
	client.SetHandlers(capsulad.Handlers{
		Ready: func() {
			log.Printf("homeassistant: ready app=%s run=%s sdk=%s", env.AppID, env.RunID, env.SDK)
		},
		Settings: a.applySettings,
		Action:   a.action,
		Invocation: func(_ context.Context, invocation *capsulad.LaunchInvocation) {
			a.invocation(invocation)
		},
		Disconnected: func(err error) {
			if a.ctx.Err() != nil {
				// A deliberate stop cancels the signal context first; the bus
				// then ends with the cancellation, not with a fault.
				log.Printf("homeassistant: framework connection closed during shutdown: %v", err)
				return
			}
			log.Printf("homeassistant: framework connection lost: %v", err)
		},
	})
	a.registerInterface()

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		a.loop()
	}()

	log.Printf("homeassistant: starting bus app=%s run=%s", env.AppID, env.RunID)
	busErr := client.Start(ctx)
	unexpected := busErr != nil && ctx.Err() == nil
	if unexpected {
		log.Printf("homeassistant: bus stopped unexpectedly: %v", busErr)
	}
	a.stopRuntime()
	select {
	case <-loopDone:
	case <-time.After(stopTimeout):
		log.Printf("homeassistant: shutdown timed out waiting for the configuration loop")
	}
	a.shutdown()
	_ = client.Close()
	log.Printf("homeassistant: stopped")
	if unexpected {
		// The framework connection ended while the app was meant to be running.
		// Exiting non-zero lets restart_on_failure surface the fault instead of
		// reporting a quiet, deliberate stop.
		os.Exit(1)
	}
}

// app is the process-wide state: the last accepted configuration and the single
// live runtime that serves it.
type app struct {
	ctx    context.Context
	env    capsulad.Env
	client *capsulad.Client

	mu               sync.Mutex
	cfg              appcfg.Config
	haveConfig       bool
	configError      string
	configGeneration uint64

	reconfigure chan struct{}

	runtimeMu sync.Mutex
	runtime   *runtime
}

func newApp(ctx context.Context, env capsulad.Env, client *capsulad.Client) *app {
	return &app{
		ctx:         ctx,
		env:         env,
		client:      client,
		reconfigure: make(chan struct{}, 1),
	}
}

// runtime is one live bridge: the session, the watchers and the hardware it
// holds.
type runtime struct {
	cancel  context.CancelFunc
	done    chan struct{}
	cfg     appcfg.Config
	started time.Time

	mu         sync.Mutex
	bridge     *ha.Bridge
	session    *hw.Session
	controller *hw.Controller
	setupError string
}

func (r *runtime) set(bridge *ha.Bridge, session *hw.Session, controller *hw.Controller) {
	r.mu.Lock()
	r.bridge, r.session, r.controller = bridge, session, controller
	r.mu.Unlock()
}

func (r *runtime) parts() (*ha.Bridge, *hw.Session, *hw.Controller) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bridge, r.session, r.controller
}

func (r *runtime) setUpError(detail string) {
	r.mu.Lock()
	r.setupError = detail
	r.mu.Unlock()
}

// applySettings decodes the app-private settings snapshot and requests a
// runtime restart when the effective configuration changed. A rejected snapshot
// keeps the previous configuration: a half-valid broker configuration is worse
// than a stale one.
func (a *app) applySettings(changed *capsulad.SettingsChanged) {
	if changed == nil {
		return
	}
	values, err := appcfg.Read(convertSettings(changed))
	if err != nil {
		a.recordConfigError(err)
		return
	}
	cfg, warnings, err := appcfg.Build(values)
	if err != nil {
		a.recordConfigError(err)
		return
	}
	for _, warning := range warnings {
		log.Printf("homeassistant: %s", warning)
	}

	a.mu.Lock()
	previous, hadConfig := a.cfg, a.haveConfig
	a.cfg, a.haveConfig, a.configError = cfg, true, ""
	a.configGeneration++
	a.mu.Unlock()

	log.Printf("homeassistant: settings revision=%d generation=%d %s", changed.Revision, a.configGeneration, cfg.Redacted())
	if hadConfig && previous == cfg {
		// A re-delivered snapshot with identical values must not disturb a
		// working session.
		return
	}
	a.requestReconfigure()
}

func (a *app) recordConfigError(err error) {
	a.mu.Lock()
	a.configError = err.Error()
	a.mu.Unlock()
	log.Printf("homeassistant: settings rejected: %v (keeping the previous configuration)", err)
}

func convertSettings(changed *capsulad.SettingsChanged) []appcfg.SettingValue {
	values := make([]appcfg.SettingValue, 0, len(changed.Values))
	for _, value := range changed.Values {
		values = append(values, appcfg.SettingValue{Key: value.Key, ValueJSON: value.ValueJSON})
	}
	return values
}

// config returns the accepted configuration.
func (a *app) config() (appcfg.Config, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg, a.haveConfig
}

func (a *app) requestReconfigure() {
	select {
	case a.reconfigure <- struct{}{}:
	default:
	}
}

// loop serializes runtime restarts. Every settings change stops the previous
// runtime completely (offline publish, disconnect, release) before the next one
// starts, so two runtimes never own the same presentation.
func (a *app) loop() {
	for {
		select {
		case <-a.ctx.Done():
			a.stopRuntime()
			return
		case <-a.reconfigure:
			a.stopRuntime()
			cfg, ok := a.config()
			if !ok {
				continue
			}
			a.startRuntime(cfg)
		}
	}
}

func (a *app) startRuntime(cfg appcfg.Config) {
	ctx, cancel := context.WithCancel(a.ctx)
	rt := &runtime{cancel: cancel, done: make(chan struct{}), cfg: cfg, started: time.Now()}
	a.runtimeMu.Lock()
	a.runtime = rt
	a.runtimeMu.Unlock()
	go func() {
		defer close(rt.done)
		a.runRuntime(ctx, rt)
	}()
}

func (a *app) stopRuntime() {
	a.runtimeMu.Lock()
	rt := a.runtime
	a.runtime = nil
	a.runtimeMu.Unlock()
	if rt == nil {
		return
	}
	rt.cancel()
	select {
	case <-rt.done:
	case <-time.After(stopTimeout):
		log.Printf("homeassistant: runtime did not stop within %s", stopTimeout)
	}
}

func (a *app) currentRuntime() *runtime {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	return a.runtime
}

// runRuntime probes the device and runs one bridge until ctx ends. A failed
// probe is retried: a device whose backend is still warming up must not leave
// the bridge down until someone restarts the app.
func (a *app) runRuntime(ctx context.Context, rt *runtime) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := a.serve(ctx, rt)
		if ctx.Err() != nil {
			return
		}
		rt.setUpError(err.Error())
		log.Printf("homeassistant: bridge setup failed: %v; retrying in %s", err, setupRetry)
		if !sleepCtx(ctx, setupRetry) {
			return
		}
	}
}

// serve runs one bridge until ctx is cancelled, then releases everything it
// held.
func (a *app) serve(ctx context.Context, rt *runtime) error {
	cfg := rt.cfg
	framework := hw.NewFramework(a.client)

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	info, err := framework.DeviceInfo(probeCtx)
	if err != nil {
		cancel()
		return err
	}
	caps, err := framework.Capabilities(probeCtx)
	if err != nil {
		cancel()
		return err
	}
	sensors, err := framework.Sensors(probeCtx)
	if err != nil {
		cancel()
		return err
	}
	ringResource, ringPixels := framework.RingResourceID(probeCtx)
	segmentResource, _ := framework.SegmentResourceID(probeCtx)
	touchResource := framework.TouchResourceID(probeCtx)
	ringPresets, err := framework.Presets(probeCtx, ringResource, ha.KindRingEffect)
	if err != nil {
		cancel()
		return err
	}
	facePresets, err := framework.Presets(probeCtx, segmentResource, ha.KindSegmentFace)
	if err != nil {
		cancel()
		return err
	}
	cancel()

	nodeID := cfg.NodeID
	if nodeID == "" {
		nodeID = ha.Slug(info.DeviceID)
	}
	if nodeID == "" {
		nodeID = "capsula"
	}
	topics, err := cfg.TopicLayout(nodeID)
	if err != nil {
		return err
	}

	device := describeDevice(info, nodeID)
	catalog := ha.BuildCatalog(topics, device, caps, sensors, ringPresets, facePresets, cfg.BuildOptions())
	if len(catalog.Entities) == 0 {
		return fmt.Errorf("the device advertises no surface to publish (capabilities %+v)", caps)
	}

	controller := hw.NewController(a.client, ringResource, ringPixels, segmentResource, touchResource, log.Printf)
	proxy := &publisherProxy{}
	bridge := ha.NewBridge(topics, catalog, proxy, controller, ha.Actions{
		Reconnect: func(reason string) {
			if _, session, _ := rt.parts(); session != nil {
				session.Reconnect(reason)
			}
		},
		Republish: func(reason string) {
			a.republish("mqtt button: " + reason)
		},
	}, log.Printf)
	bridge.SetPresets(ringPresets, facePresets)

	discoverer := hw.NewBrokerDiscoverer(a.client, log.Printf)
	session := hw.NewSession(a.env, cfg, bridge, discoverer.Discover, log.Printf)
	proxy.set(session)
	touchPublished := cfg.ExposeTouch && caps.Touch && caps.TouchPartial
	watchers := hw.NewWatchers(framework, bridge, cfg, sensors, controller, catalog, log.Printf)
	rt.set(bridge, session, controller)

	log.Printf("homeassistant: bridge node=%q entities=%d surfaces=ring:%v display:%v sensors:%d bluetooth:%v touch:%v",
		nodeID, len(catalog.Entities), caps.Ring, caps.SegmentDisplay, len(sensors), caps.Media, touchPublished)

	runtimeCtx, runtimeCancel := context.WithCancel(ctx)
	defer runtimeCancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		session.Run(runtimeCtx)
	}()
	go func() {
		defer wg.Done()
		watchers.Run(runtimeCtx)
	}()

	<-runtimeCtx.Done()
	wg.Wait()
	// Every terminal path releases the presentations, the capture and the
	// touch subscription; the shutdown publish already happened inside the
	// session.
	controller.ReleaseAll()
	log.Printf("homeassistant: bridge stopped; hardware released")
	return nil
}

func describeDevice(info hw.DeviceInfo, nodeID string) ha.Device {
	name := strings.TrimSpace(info.Model)
	if name == "" {
		name = "Capsula Mini"
	}
	sw := "capsulad " + strings.TrimSpace(info.DaemonVersion)
	if fw := strings.TrimSpace(info.Firmware); fw != "" {
		sw += ", firmware " + fw
	}
	hwVersion := strings.TrimSpace(info.BoardRevision + " " + info.SOC)
	return ha.Device{
		NodeID:       nodeID,
		Name:         name,
		Manufacturer: "capsulad",
		Model:        strings.TrimSpace(info.Model),
		SWVersion:    sw,
		HWVersion:    hwVersion,
	}
}

// publisherProxy lets the bridge exist before the session that publishes for it.
type publisherProxy struct {
	mu     sync.RWMutex
	target ha.Publisher
}

func (p *publisherProxy) set(target ha.Publisher) {
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

func (p *publisherProxy) Publish(ctx context.Context, topic string, payload []byte, retain bool, qos byte) error {
	p.mu.RLock()
	target := p.target
	p.mu.RUnlock()
	if target == nil {
		return ha.ErrNoSession
	}
	return target.Publish(ctx, topic, payload, retain, qos)
}

// --- actions and invocations ------------------------------------------------

func (a *app) action(_ context.Context, invoked *capsulad.ActionInvoked) {
	if invoked == nil {
		return
	}
	switch invoked.ActionID {
	case actionRepublish:
		count, err := a.republish("app action")
		detail := fmt.Sprintf("republished %d discovery payloads", count)
		a.reportAction(invoked, err, detail)
	case actionReconnect:
		rt := a.currentRuntime()
		if rt == nil {
			a.reportAction(invoked, errors.New("the bridge is not running"), "")
			return
		}
		_, session, _ := rt.parts()
		if session == nil {
			a.reportAction(invoked, errors.New("the bridge is not connected"), "")
			return
		}
		session.Reconnect("app action")
		a.reportAction(invoked, nil, "broker connection will be re-established")
	case actionRelease:
		rt := a.currentRuntime()
		if rt == nil {
			a.reportAction(invoked, errors.New("the bridge is not running"), "")
			return
		}
		_, _, controller := rt.parts()
		if controller == nil {
			a.reportAction(invoked, errors.New("the bridge owns no hardware"), "")
			return
		}
		controller.ReleaseRingAndDisplay()
		a.reportAction(invoked, nil, "ring and display released; the device baseline owns them again")
	default:
		log.Printf("homeassistant: unknown action %q", invoked.ActionID)
		a.reportAction(invoked, fmt.Errorf("unknown action %q", invoked.ActionID), "")
	}
}

func (a *app) reportAction(invoked *capsulad.ActionInvoked, err error, detail string) {
	if err != nil {
		log.Printf("homeassistant: action %s failed: %v", invoked.ActionID, err)
		_ = a.client.NotifyActionResult(invoked.ID, false, err.Error())
		return
	}
	log.Printf("homeassistant: action %s: %s", invoked.ActionID, detail)
	_ = a.client.NotifyActionResult(invoked.ID, true, detail)
}

// republish rewrites every retained discovery payload and the current state.
func (a *app) republish(reason string) (int, error) {
	rt := a.currentRuntime()
	if rt == nil {
		return 0, errors.New("the bridge is not running yet")
	}
	bridge, _, _ := rt.parts()
	if bridge == nil {
		return 0, errors.New("the bridge is not running yet")
	}
	ctx, cancel := context.WithTimeout(a.ctx, actionTimeout)
	defer cancel()
	count, err := bridge.PublishDiscovery(ctx)
	if err != nil {
		return 0, fmt.Errorf("publish discovery: %w", err)
	}
	if err := bridge.PublishStates(ctx); err != nil {
		return count, fmt.Errorf("publish state: %w", err)
	}
	log.Printf("homeassistant: republished %d discovery payloads (%s)", count, reason)
	return count, nil
}

// invocation reports the bridge state and refreshes Home Assistant. A launch
// while the bridge is running is a request to make sure Home Assistant is
// current, not a request to start a second bridge.
func (a *app) invocation(invocation *capsulad.LaunchInvocation) {
	if invocation == nil {
		return
	}
	detail := a.describe()
	_ = a.client.NotifyInvocationResult(invocation.ID, true, detail)
	log.Printf("homeassistant: invocation %s: %s", invocation.ID, detail)
	go func() {
		if _, err := a.republish("launch invocation"); err != nil {
			log.Printf("homeassistant: invocation republish: %v", err)
		}
	}()
}

// describe renders one human-readable status line.
func (a *app) describe() string {
	cfg, ok := a.config()
	if !ok {
		return "no settings have been received yet"
	}
	rt := a.currentRuntime()
	if rt == nil {
		if a.configErrorText() != "" {
			return "bridge is stopped: " + a.configErrorText()
		}
		return "bridge is stopped"
	}
	bridge, session, _ := rt.parts()
	if bridge == nil || session == nil {
		rt.mu.Lock()
		setupError := rt.setupError
		rt.mu.Unlock()
		if setupError != "" {
			return "bridge setup failed: " + setupError
		}
		return "bridge is starting"
	}
	snapshot := bridge.Snapshot()
	state := "disconnected"
	if session.Connected() {
		state = "connected"
	}
	// The endpoint in use when there is one, so this line and Status.broker tell
	// the same story about a broker found by discovery.
	endpoint := "no broker"
	switch {
	case session.Endpoint() != "":
		endpoint = session.Endpoint()
	case cfg.Host != "":
		endpoint = hw.HostPort(cfg.Host, cfg.Port)
	case cfg.DiscoverBroker:
		endpoint = "mDNS discovery"
	}
	return fmt.Sprintf("%s from %s as node %q; %d entities; %d commands applied; status %s",
		state, endpoint, bridge.Topics().NodeID, snapshot.Entities,
		snapshot.Counters.Commands, snapshot.Status)
}

func (a *app) configErrorText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.configError
}

func (a *app) shutdown() {
	a.stopRuntime()
	log.Printf("homeassistant: shutdown complete")
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// --- published app interface ------------------------------------------------

// The interface's wire shapes are named types so a test can hold them against
// proto/homeassistant.proto: a field name that drifts from the descriptor makes
// the native JSON conversion fail at runtime, and the daemon would report it
// only as a refused call.

type getStatusRequest struct{}

type publishDiscoveryRequest struct{}

type publishDiscoveryResponse struct {
	Published uint32 `json:"published"`
}

type publishEventRequest struct {
	EventType   string `json:"eventType"`
	PayloadJSON string `json:"payloadJson"`
}

type publishEventResponse struct {
	Published bool `json:"published"`
}

// statusJSON is the wire shape of the interface's Status message. Field names
// are canonical protobuf JSON so the native engine can convert them with the
// registered descriptor.
type statusJSON struct {
	Configured      bool   `json:"configured"`
	Connected       bool   `json:"connected"`
	Broker          string `json:"broker"`
	NodeID          string `json:"nodeId"`
	DiscoveryPrefix string `json:"discoveryPrefix"`
	BaseTopic       string `json:"baseTopic"`
	Entities        uint32 `json:"entities"`
	RingOn          bool   `json:"ringOn"`
	RingEffect      string `json:"ringEffect"`
	DisplayOn       bool   `json:"displayOn"`
	DisplayFace     string `json:"displayFace"`
	BridgeStatus    string `json:"bridgeStatus"`
	LastError       string `json:"lastError"`
	Published       uint64 `json:"published"`
	Commands        uint64 `json:"commands"`
	Failures        uint64 `json:"failures"`
}

func (a *app) registerInterface() {
	if len(haproto.DescriptorSet) == 0 {
		log.Printf("homeassistant: interface descriptor is empty; JSON callers cannot resolve %s", interfaceService)
	} else if err := a.client.RegisterDescriptors(haproto.DescriptorSet); err != nil {
		log.Printf("homeassistant: register interface descriptor: %v", err)
	}
	a.client.Handle(interfaceService, "GetStatus", a.handleGetStatus)
	a.client.Handle(interfaceService, "PublishDiscovery", a.handlePublishDiscovery)
	a.client.Handle(interfaceService, "PublishEvent", a.handlePublishEvent)
}

func (a *app) handleGetStatus(_ context.Context, call *capsulad.ServerCall) {
	var request getStatusRequest
	if err := call.RecvJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		_ = call.End(3, "decode request: "+err.Error())
		return
	}
	if err := call.SendJSON(a.status()); err != nil {
		_ = call.End(13, "encode status: "+err.Error())
		return
	}
	_ = call.End(0, "")
}

func (a *app) handlePublishDiscovery(_ context.Context, call *capsulad.ServerCall) {
	var request publishDiscoveryRequest
	if err := call.RecvJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		_ = call.End(3, "decode request: "+err.Error())
		return
	}
	count, err := a.republish("interface call")
	if err != nil {
		_ = call.End(9, err.Error())
		return
	}
	if err := call.SendJSON(publishDiscoveryResponse{Published: uint32(count)}); err != nil {
		_ = call.End(13, "encode response: "+err.Error())
		return
	}
	_ = call.End(0, "")
}

func (a *app) handlePublishEvent(_ context.Context, call *capsulad.ServerCall) {
	var request publishEventRequest
	if err := call.RecvJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		_ = call.End(3, "decode request: "+err.Error())
		return
	}
	if err := a.publishEvent(request.EventType, request.PayloadJSON); err != nil {
		var refused *refusal
		if errors.As(err, &refused) {
			_ = call.End(int32(refused.code), refused.Error())
			return
		}
		_ = call.End(13, err.Error())
		return
	}
	if err := call.SendJSON(publishEventResponse{Published: true}); err != nil {
		_ = call.End(13, "encode response: "+err.Error())
		return
	}
	_ = call.End(0, "")
}

// refusal carries the canonical gRPC code for a rejected interface request.
//
// Codes in use: INVALID_ARGUMENT (3) for a malformed request and
// FAILED_PRECONDITION (9) for a request the running bridge cannot serve, which
// is what docs/bridge.md documents.
type refusal struct {
	code int
	err  error
}

func (r *refusal) Error() string { return r.err.Error() }
func (r *refusal) Unwrap() error { return r.err }

func invalidArgument(format string, args ...any) error {
	return &refusal{code: 3, err: fmt.Errorf(format, args...)}
}

func failedPrecondition(format string, args ...any) error {
	return &refusal{code: 9, err: fmt.Errorf(format, args...)}
}

// publishEvent raises one declared panel event. The type list is fixed by the
// published event entity, so an unknown type is refused instead of being sent
// where Home Assistant would discard it.
func (a *app) publishEvent(eventType, payloadJSON string) error {
	if eventType == "" {
		return invalidArgument("event_type is required")
	}
	declared := false
	for _, candidate := range ha.TouchEventTypes() {
		if candidate == eventType {
			declared = true
			break
		}
	}
	if !declared {
		return invalidArgument("event_type %q is not published by the event entity", eventType)
	}
	extra := map[string]any{}
	if strings.TrimSpace(payloadJSON) != "" {
		if len(payloadJSON) > maxEventPayload {
			return invalidArgument("payload_json is larger than %d bytes", maxEventPayload)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payloadJSON), &decoded); err != nil {
			return invalidArgument("payload_json is not a JSON object: %v", err)
		}
		extra = decoded
	}
	rt := a.currentRuntime()
	if rt == nil {
		return failedPrecondition("the bridge is not running")
	}
	bridge, _, _ := rt.parts()
	if bridge == nil {
		return failedPrecondition("the bridge is not running")
	}
	if !hasEntity(bridge.Catalog(), "event", ha.SlugTouch) {
		return failedPrecondition("no event entity is published: enable expose_touch, or the panel does not advertise partial gestures")
	}
	ctx, cancel := context.WithTimeout(a.ctx, actionTimeout)
	defer cancel()
	if err := bridge.PublishTouchEvent(ctx, eventType, extra); err != nil {
		if errors.Is(err, ha.ErrNoSession) {
			return failedPrecondition("no broker session is connected")
		}
		return fmt.Errorf("publish event: %w", err)
	}
	return nil
}

// hasEntity reports whether the catalog publishes one component's entity.
func hasEntity(catalog ha.Catalog, component, slug string) bool {
	for _, entity := range catalog.Entities {
		if entity.Component == component && entity.Slug == slug {
			return true
		}
	}
	return false
}

func (a *app) status() statusJSON {
	cfg, configured := a.config()
	status := statusJSON{
		Configured:      configured,
		DiscoveryPrefix: cfg.DiscoveryPrefix,
		BaseTopic:       cfg.BaseTopic,
		BridgeStatus:    ha.BridgeStatusStopped,
	}
	if configured && cfg.Host != "" {
		status.Broker = hw.HostPort(cfg.Host, cfg.Port)
	}
	rt := a.currentRuntime()
	if rt == nil {
		return status
	}
	bridge, session, _ := rt.parts()
	if bridge != nil && session != nil {
		// The endpoint in use wins: an mDNS-discovered broker has no configured
		// host, and the status must still name what the bridge dialled.
		if endpoint := session.Endpoint(); endpoint != "" {
			status.Broker = endpoint
		}
	}
	if bridge == nil || session == nil {
		rt.mu.Lock()
		setupError := rt.setupError
		rt.mu.Unlock()
		status.LastError = setupError
		return status
	}
	snapshot := bridge.Snapshot()
	topics := bridge.Topics()
	published, commands, failures := session.Stats()
	status.Connected = session.Connected()
	status.NodeID = topics.NodeID
	status.Entities = uint32(snapshot.Entities)
	status.RingOn = snapshot.RingOn
	status.RingEffect = snapshot.RingEffect
	status.DisplayOn = snapshot.DisplayOn
	status.DisplayFace = snapshot.DisplayFace
	status.BridgeStatus = snapshot.Status
	status.LastError = snapshot.LastError
	status.Published = published
	status.Commands = commands
	status.Failures = failures
	return status
}
