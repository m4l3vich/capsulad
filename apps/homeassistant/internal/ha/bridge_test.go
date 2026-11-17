package ha

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// recorder is a Publisher that records every message.
type recorder struct {
	mu       sync.Mutex
	messages []recordedMessage
	failOn   string
	err      error
}

type recordedMessage struct {
	Topic   string
	Payload string
	Retain  bool
	QoS     byte
}

func (r *recorder) Publish(_ context.Context, topic string, payload []byte, retain bool, qos byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn != "" && strings.Contains(topic, r.failOn) {
		return r.err
	}
	r.messages = append(r.messages, recordedMessage{Topic: topic, Payload: string(payload), Retain: retain, QoS: qos})
	return nil
}

func (r *recorder) find(topic string) (recordedMessage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, msg := range r.messages {
		if msg.Topic == topic {
			return msg, true
		}
	}
	return recordedMessage{}, false
}

func (r *recorder) last(topic string) (recordedMessage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found recordedMessage
	ok := false
	for _, msg := range r.messages {
		if msg.Topic == topic {
			found, ok = msg, true
		}
	}
	return found, ok
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

// fakeHardware records the control calls the bridge makes.
type fakeHardware struct {
	mu       sync.Mutex
	calls    []string
	ringErr  error
	dispErr  error
	mediaErr error
}

func (f *fakeHardware) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeHardware) RingSetColor(color RGB, intensity float64) error {
	f.record("ring-color " + string(rune('0'+color.R%10)) + "/" + formatFloat(intensity))
	return f.ringErr
}

func (f *fakeHardware) RingSetEffect(presetID string) error {
	f.record("ring-effect " + presetID)
	return f.ringErr
}

func (f *fakeHardware) RingRelease() { f.record("ring-release") }

func (f *fakeHardware) DisplaySetFace(presetID string) error {
	f.record("display-face " + presetID)
	return f.dispErr
}

func (f *fakeHardware) DisplayRelease() { f.record("display-release") }

func (f *fakeHardware) ReleaseRingAndDisplay() {
	f.record("ring-release")
	f.record("display-release")
}

func (f *fakeHardware) MediaCommand(cmd string) error {
	f.record("media-" + cmd)
	return f.mediaErr
}

func (f *fakeHardware) callsList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func formatFloat(v float64) string {
	encoded, _ := json.Marshal(v)
	return string(encoded)
}

func newTestBridge(t *testing.T, opts BuildOptions) (*Bridge, *recorder, *fakeHardware, Topics) {
	t.Helper()
	topics := testTopics(t)
	caps := Capabilities{SegmentDisplay: true, Ring: true, RingRGB: true, Media: true, Touch: true, TouchPartial: true}
	catalog := BuildCatalog(topics, testDevice(), caps,
		[]SensorInfo{{ID: "sensor.ambient_light", Slug: "sensor_ambient_light", Name: "Ambient light",
			FrameworkUnit: "lux", Unit: "lx", DeviceClass: "illuminance", StateClass: true}},
		ringPresets(), facePresets(), opts)
	pub := &recorder{}
	hw := &fakeHardware{}
	bridge := NewBridge(topics, catalog, pub, hw, Actions{}, t.Logf)
	bridge.SetPresets(ringPresets(), facePresets())
	// The fixture stands in for a live session: discovery publication requires
	// one, and TestPublishWithoutSessionIsSilent covers the other case.
	bridge.SetAvailable(true)
	return bridge, pub, hw, topics
}

func allOptions() BuildOptions {
	return BuildOptions{Ring: true, Display: true, Sensors: true, Bluetooth: true, Touch: true, Diagnostics: true}
}

func TestPublishDiscoveryRetainsEveryConfig(t *testing.T) {
	bridge, pub, _, topics := newTestBridge(t, allOptions())
	ctx := context.Background()
	count, err := bridge.PublishDiscovery(ctx)
	if err != nil {
		t.Fatalf("PublishDiscovery: %v", err)
	}
	if count != len(bridge.Catalog().Entities) {
		t.Fatalf("published %d configs for %d entities", count, len(bridge.Catalog().Entities))
	}
	for _, entity := range bridge.Catalog().Entities {
		msg, ok := pub.find(topics.DiscoveryTopic(entity.Component, entity.Slug))
		if !ok {
			t.Fatalf("no discovery payload for %s.%s", entity.Component, entity.Slug)
		}
		if !msg.Retain || msg.QoS != 1 {
			t.Fatalf("%s discovery is retain=%v qos=%d, want retained QoS 1", entity.Slug, msg.Retain, msg.QoS)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(msg.Payload), &decoded); err != nil {
			t.Fatalf("%s discovery payload is not JSON: %v", entity.Slug, err)
		}
	}
	status, ok := pub.last(topics.StatusTopic())
	if !ok || status.Payload != PayloadOnline || !status.Retain || status.QoS != 1 {
		t.Fatalf("availability message %+v", status)
	}
}

func TestPublishStatesCoversEveryStateEntity(t *testing.T) {
	bridge, pub, _, topics := newTestBridge(t, allOptions())
	ctx := context.Background()
	if err := bridge.PublishStates(ctx); err != nil {
		t.Fatalf("PublishStates: %v", err)
	}
	for _, topic := range []string{
		topics.StateTopic(SlugRing),
		topics.StateTopic(SlugDisplay),
		topics.StateTopic(SlugBluetoothState),
		topics.StateTopic(SlugBluetoothTitle),
		topics.StateTopic(SlugBridgeStatus),
		topics.AttributesTopic(SlugBluetoothState),
		topics.AttributesTopic(SlugBridgeStatus),
	} {
		if _, ok := pub.last(topic); !ok {
			t.Fatalf("no state published on %s", topic)
		}
	}
}

func TestLightCommands(t *testing.T) {
	bridge, pub, hw, topics := newTestBridge(t, allOptions())
	ctx := context.Background()

	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRing),
		[]byte(`{"state":"ON","brightness":128,"color":{"r":10,"g":20,"b":30}}`)); err != nil {
		t.Fatalf("light on: %v", err)
	}
	calls := hw.callsList()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "ring-color ") {
		t.Fatalf("hardware calls %v", calls)
	}
	state, ok := pub.last(topics.StateTopic(SlugRing))
	if !ok {
		t.Fatal("no ring state published")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(state.Payload), &decoded); err != nil {
		t.Fatalf("ring state is not JSON: %v", err)
	}
	if decoded["state"] != PayloadOn {
		t.Fatalf("ring state %v", decoded)
	}
	if decoded["brightness"] != float64(128) {
		t.Fatalf("brightness %v", decoded["brightness"])
	}
	color, _ := decoded["color"].(map[string]any)
	if color["r"] != float64(10) || color["g"] != float64(20) || color["b"] != float64(30) {
		t.Fatalf("colour %v", decoded["color"])
	}
	if _, present := decoded["effect"]; present {
		t.Fatalf("an effect is reported although none was applied: %v", decoded)
	}

	// A brightness-only command keeps the last colour and implies ON.
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRing), []byte(`{"brightness":255}`)); err != nil {
		t.Fatalf("brightness only: %v", err)
	}
	state, _ = pub.last(topics.StateTopic(SlugRing))
	decoded = map[string]any{}
	if err := json.Unmarshal([]byte(state.Payload), &decoded); err != nil {
		t.Fatalf("ring state is not JSON: %v", err)
	}
	if decoded["brightness"] != float64(255) || decoded["state"] != PayloadOn {
		t.Fatalf("brightness-only command not applied: %v", decoded)
	}
	color, _ = decoded["color"].(map[string]any)
	if color["r"] != float64(10) {
		t.Fatalf("brightness-only command lost the colour: %v", decoded)
	}

	// OFF releases ownership: the device's configured baseline resumes.
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRing), []byte(`{"state":"OFF"}`)); err != nil {
		t.Fatalf("light off: %v", err)
	}
	calls = hw.callsList()
	if calls[len(calls)-1] != "ring-release" {
		t.Fatalf("OFF did not release the ring: %v", calls)
	}
	state, _ = pub.last(topics.StateTopic(SlugRing))
	if !strings.Contains(state.Payload, `"OFF"`) {
		t.Fatalf("ring state after OFF: %s", state.Payload)
	}
	if snapshot := bridge.Snapshot(); snapshot.RingOn {
		t.Fatalf("ring state still ON: %+v", snapshot)
	}
}

func TestLightEffectCommands(t *testing.T) {
	bridge, pub, hw, topics := newTestBridge(t, allOptions())
	ctx := context.Background()

	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRing),
		[]byte(`{"state":"ON","effect":"system.ring.pulse"}`)); err != nil {
		t.Fatalf("effect: %v", err)
	}
	if calls := hw.callsList(); calls[len(calls)-1] != "ring-effect system.ring.pulse" {
		t.Fatalf("hardware calls %v", calls)
	}
	state, _ := pub.last(topics.StateTopic(SlugRing))
	if !strings.Contains(state.Payload, `"effect":"system.ring.pulse"`) {
		t.Fatalf("effect not published: %s", state.Payload)
	}

	// An unknown effect is refused before the device is touched.
	before := len(hw.callsList())
	err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRing), []byte(`{"state":"ON","effect":"system.ring.nope"}`))
	if err == nil {
		t.Fatal("unknown effect accepted")
	}
	if len(hw.callsList()) != before {
		t.Fatal("a rejected effect still reached the device")
	}
	if snapshot := bridge.Snapshot(); snapshot.Counters.CommandErrs == 0 {
		t.Fatal("command error not counted")
	}

	// "None" clears the effect and returns to the static colour.
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRing), []byte(`{"effect":"None"}`)); err != nil {
		t.Fatalf("clear effect: %v", err)
	}
	if calls := hw.callsList(); !strings.HasPrefix(calls[len(calls)-1], "ring-color ") {
		t.Fatalf("clearing the effect did not restore the colour: %v", calls)
	}
	state, _ = pub.last(topics.StateTopic(SlugRing))
	if strings.Contains(state.Payload, "effect") {
		t.Fatalf("cleared effect still reported: %s", state.Payload)
	}
}

func TestDisplayCommands(t *testing.T) {
	bridge, pub, hw, topics := newTestBridge(t, allOptions())
	ctx := context.Background()

	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugDisplay), []byte(PayloadOn)); err != nil {
		t.Fatalf("display on: %v", err)
	}
	calls := hw.callsList()
	if calls[len(calls)-1] != "display-face system.display.face.neutral" {
		t.Fatalf("display on did not apply the default face: %v", calls)
	}
	if state, _ := pub.last(topics.StateTopic(SlugDisplay)); state.Payload != PayloadOn {
		t.Fatalf("switch state %q", state.Payload)
	}
	if state, _ := pub.last(topics.StateTopic(SlugDisplayFace)); state.Payload != "system.display.face.neutral" {
		t.Fatalf("face state %q", state.Payload)
	}

	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugDisplay), []byte(PayloadOff)); err != nil {
		t.Fatalf("display off: %v", err)
	}
	calls = hw.callsList()
	if calls[len(calls)-1] != "display-release" {
		t.Fatalf("display off did not release the display: %v", calls)
	}
	if state, _ := pub.last(topics.StateTopic(SlugDisplay)); state.Payload != PayloadOff {
		t.Fatalf("switch state after off %q", state.Payload)
	}

	// A face selected while the overlay is off turns it on implicitly.
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugDisplayFace), []byte("system.display.face.neutral")); err != nil {
		t.Fatalf("face select: %v", err)
	}
	if state, _ := pub.last(topics.StateTopic(SlugDisplay)); state.Payload != PayloadOn {
		t.Fatalf("face select did not turn the overlay on: %q", state.Payload)
	}

	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugDisplayFace), []byte("system.display.face.nope")); err == nil {
		t.Fatal("unknown face accepted")
	}
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugDisplay), []byte("yes")); err == nil {
		t.Fatal("malformed switch payload accepted")
	}
}

func TestMediaCommands(t *testing.T) {
	bridge, _, hw, topics := newTestBridge(t, allOptions())
	ctx := context.Background()
	buttons := []struct {
		slug    string
		payload string
	}{
		{SlugBluetoothPlay, PayloadPlay},
		{SlugBluetoothPause, PayloadPause},
		{SlugBluetoothNext, PayloadNext},
		{SlugBluetoothPrev, PayloadPrevious},
	}
	for _, button := range buttons {
		if err := bridge.HandleMessage(ctx, topics.SetTopic(button.slug), []byte(button.payload)); err != nil {
			t.Fatalf("button %s: %v", button.slug, err)
		}
	}
	calls := hw.callsList()
	want := []string{"media-PLAY", "media-PAUSE", "media-NEXT", "media-PREVIOUS"}
	if len(calls) != len(want) {
		t.Fatalf("hardware calls %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
	// A button only accepts its own command: a payload swap would make the
	// entity's advertised action untrue.
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugBluetoothPlay), []byte(PayloadPause)); err == nil {
		t.Fatal("the play button accepted a pause payload")
	}
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugBluetoothPause), []byte("STOP")); err == nil {
		t.Fatal("unsupported media command accepted")
	}
}

func TestButtonCommands(t *testing.T) {
	var (
		mu        sync.Mutex
		reconnect int
		republish int
	)
	topics := testTopics(t)
	caps := Capabilities{Ring: true, RingRGB: true, SegmentDisplay: true}
	catalog := BuildCatalog(topics, testDevice(), caps, nil, nil, nil, BuildOptions{Ring: true, Display: true, Diagnostics: true})
	pub := &recorder{}
	hw := &fakeHardware{}
	bridge := NewBridge(topics, catalog, pub, hw, Actions{
		Reconnect: func(string) { mu.Lock(); reconnect++; mu.Unlock() },
		Republish: func(string) { mu.Lock(); republish++; mu.Unlock() },
	}, t.Logf)
	ctx := context.Background()

	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugReconnect), []byte(PayloadPress)); err != nil {
		t.Fatalf("reconnect button: %v", err)
	}
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRepublish), []byte(PayloadPress)); err != nil {
		t.Fatalf("republish button: %v", err)
	}
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugRelease), []byte(PayloadPress)); err != nil {
		t.Fatalf("release button: %v", err)
	}
	mu.Lock()
	gotReconnect, gotRepublish := reconnect, republish
	mu.Unlock()
	if gotReconnect != 1 || gotRepublish != 1 {
		t.Fatalf("callbacks reconnect=%d republish=%d", gotReconnect, gotRepublish)
	}
	calls := hw.callsList()
	if len(calls) != 2 || calls[0] != "ring-release" || calls[1] != "display-release" {
		t.Fatalf("release button did not release hardware: %v", calls)
	}
	if state, ok := pub.last(topics.StateTopic(SlugRing)); !ok || !strings.Contains(state.Payload, `"OFF"`) {
		t.Fatalf("release did not publish the ring state: %q", state.Payload)
	}
	if err := bridge.HandleMessage(ctx, topics.SetTopic(SlugReconnect), []byte("nope")); err == nil {
		t.Fatal("malformed button payload accepted")
	}
}

func TestUnknownTopicIsRefused(t *testing.T) {
	bridge, _, hw, topics := newTestBridge(t, allOptions())
	err := bridge.HandleMessage(context.Background(), topics.BaseTopic+"/capsula/nope/set", []byte("ON"))
	if err == nil {
		t.Fatal("unknown topic accepted")
	}
	if len(hw.callsList()) != 0 {
		t.Fatal("an unknown topic reached the hardware")
	}
	if err := bridge.HandleMessage(context.Background(), topics.DiscoveryTopic("light", SlugRing), []byte("{}")); err == nil {
		t.Fatal("a discovery topic was treated as a command topic")
	}
}

func TestStatePublishers(t *testing.T) {
	bridge, pub, _, topics := newTestBridge(t, allOptions())
	ctx := context.Background()

	if err := bridge.PublishUptime(ctx, 12345); err != nil {
		t.Fatalf("PublishUptime: %v", err)
	}
	if msg, _ := pub.last(topics.StateTopic(SlugUptime)); msg.Payload != "12345" {
		t.Fatalf("uptime payload %q", msg.Payload)
	}

	if err := bridge.PublishAudio(ctx, 0.4, true); err != nil {
		t.Fatalf("PublishAudio: %v", err)
	}
	if msg, _ := pub.last(topics.StateTopic(SlugVolume)); msg.Payload != "40.0" {
		t.Fatalf("volume payload %q", msg.Payload)
	}
	if msg, _ := pub.last(topics.StateTopic(SlugMicrophoneMute)); msg.Payload != PayloadOn {
		t.Fatalf("mute payload %q", msg.Payload)
	}
	// Clamping keeps a nonsense mixer value out of Home Assistant.
	if err := bridge.PublishAudio(ctx, 3, false); err != nil {
		t.Fatalf("PublishAudio: %v", err)
	}
	if msg, _ := pub.last(topics.StateTopic(SlugVolume)); msg.Payload != "100.0" {
		t.Fatalf("clamped volume payload %q", msg.Payload)
	}

	sensor := SensorInfo{ID: "sensor.ambient_light", Slug: "sensor_ambient_light", Name: "Ambient light",
		FrameworkUnit: "lux", Unit: "lx", DeviceClass: "illuminance", StateClass: true}
	if err := bridge.PublishSensor(ctx, sensor, 42.5); err != nil {
		t.Fatalf("PublishSensor: %v", err)
	}
	if msg, _ := pub.last(topics.StateTopic("sensor_ambient_light")); msg.Payload != "42.5" {
		t.Fatalf("sensor payload %q", msg.Payload)
	}

	if err := bridge.PublishMedia(ctx, MediaState{Present: true, Playing: true, Title: "Song", Position: 9000}); err != nil {
		t.Fatalf("PublishMedia: %v", err)
	}
	if msg, _ := pub.last(topics.StateTopic(SlugBluetoothState)); msg.Payload != MediaStatePlaying {
		t.Fatalf("media state %q", msg.Payload)
	}
	if msg, _ := pub.last(topics.StateTopic(SlugBluetoothTitle)); msg.Payload != "Song" {
		t.Fatalf("media title %q", msg.Payload)
	}
	attributes, _ := pub.last(topics.AttributesTopic(SlugBluetoothState))
	if !strings.Contains(attributes.Payload, `"title":"Song"`) ||
		!strings.Contains(attributes.Payload, `"position_s":9`) {
		t.Fatalf("media attributes %s", attributes.Payload)
	}
	if err := bridge.PublishMedia(ctx, MediaState{}); err != nil {
		t.Fatalf("PublishMedia: %v", err)
	}
	if msg, _ := pub.last(topics.StateTopic(SlugBluetoothTitle)); msg.Payload != TitlesNone {
		t.Fatalf("absent title payload %q", msg.Payload)
	}
	if snapshot := bridge.Snapshot(); snapshot.Entities == 0 {
		t.Fatal("snapshot reports no entities")
	}

	if err := bridge.PublishTouchEvent(ctx, "play_pause_press", map[string]any{"pressed": true}); err != nil {
		t.Fatalf("PublishTouchEvent: %v", err)
	}
	msg, _ := pub.last(topics.StateTopic(SlugTouch))
	var event map[string]any
	if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		t.Fatalf("touch event is not JSON: %v", err)
	}
	if event["event_type"] != "play_pause_press" || event["pressed"] != true {
		t.Fatalf("touch event %v", event)
	}
}

func TestPublishFailureIsCountedAndReported(t *testing.T) {
	bridge, pub, _, topics := newTestBridge(t, allOptions())
	pub.failOn = topics.StatusTopic()
	pub.err = errors.New("session down")
	if err := bridge.PublishAvailability(context.Background(), true); err == nil {
		t.Fatal("a failed publish was reported as success")
	}
	snapshot := bridge.Snapshot()
	if snapshot.Counters.PublishErrs == 0 || snapshot.LastError == "" {
		t.Fatalf("failure not recorded: %+v", snapshot)
	}
}

func TestHardwareFailureIsReportedAndNotRetried(t *testing.T) {
	bridge, _, hw, topics := newTestBridge(t, allOptions())
	hw.ringErr = errors.New("capture refused")
	err := bridge.HandleMessage(context.Background(), topics.SetTopic(SlugRing), []byte(`{"state":"ON"}`))
	if err == nil {
		t.Fatal("a hardware failure was reported as success")
	}
	if snapshot := bridge.Snapshot(); snapshot.RingOn {
		t.Fatal("ring reported ON although the hardware refused")
	}
}

func TestSetStatusPublishesDiagnostics(t *testing.T) {
	bridge, pub, _, topics := newTestBridge(t, allOptions())
	ctx := context.Background()
	bridge.SetStatus(ctx, BridgeStatusOnline)
	state, ok := pub.last(topics.StateTopic(SlugBridgeStatus))
	if !ok || state.Payload != BridgeStatusOnline {
		t.Fatalf("bridge status %+v", state)
	}
	attributes, ok := pub.last(topics.AttributesTopic(SlugBridgeStatus))
	if !ok {
		t.Fatal("no bridge attributes published")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(attributes.Payload), &decoded); err != nil {
		t.Fatalf("attributes are not JSON: %v", err)
	}
	if decoded["entities"] == nil {
		t.Fatalf("attributes lack the entity count: %v", decoded)
	}
	bridge.SetError(ctx, "broker unreachable")
	state, _ = pub.last(topics.StateTopic(SlugBridgeStatus))
	if state.Payload != BridgeStatusErrorPrefix {
		t.Fatalf("error status %q", state.Payload)
	}
}

// offlinePublisher always reports that no broker session exists.
type offlinePublisher struct{ calls int }

func (p *offlinePublisher) Publish(context.Context, string, []byte, bool, byte) error {
	p.calls++
	return ErrNoSession
}

func TestPublishWithoutSessionIsSilent(t *testing.T) {
	topics := testTopics(t)
	caps := Capabilities{Ring: true, RingRGB: true, SegmentDisplay: true}
	catalog := BuildCatalog(topics, testDevice(), caps, nil, nil, nil,
		BuildOptions{Ring: true, Display: true, Diagnostics: true})
	pub := &offlinePublisher{}
	bridge := NewBridge(topics, catalog, pub, &fakeHardware{}, Actions{}, t.Logf)

	if err := bridge.PublishStates(context.Background()); err != nil {
		t.Fatalf("PublishStates without a session = %v, want nil", err)
	}
	if err := bridge.PublishAvailability(context.Background(), false); err != nil {
		t.Fatalf("PublishAvailability without a session = %v, want nil", err)
	}
	// An explicit republish request must not claim success while nothing can be
	// published; state publication above is the tolerant path.
	if _, err := bridge.PublishDiscovery(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("PublishDiscovery without a session = %v, want ErrNoSession", err)
	}
	bridge.SetAvailable(true)
	if _, err := bridge.PublishDiscovery(context.Background()); err != nil {
		t.Fatalf("PublishDiscovery with a session = %v, want nil", err)
	}
	if pub.calls == 0 {
		t.Fatal("the bridge never attempted a publish")
	}
	snapshot := bridge.Snapshot()
	if snapshot.Counters.PublishErrs != 0 {
		t.Fatalf("a disconnected publish was counted as a failure: %+v", snapshot.Counters)
	}
	if snapshot.LastError != "" {
		t.Fatalf("a disconnected publish set last_error: %q", snapshot.LastError)
	}
}
