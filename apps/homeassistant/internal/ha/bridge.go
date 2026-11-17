package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// ErrNoSession reports a publish attempted while no broker session is live.
// The bridge treats it as "not news": every state is republished on connect, so
// a disconnected bridge stays quiet instead of logging a failure per entity.
var ErrNoSession = errors.New("ha: no broker session")

// Publisher is the MQTT write surface the bridge needs. The session layer
// implements it over the mqtt client; tests implement it with a recorder.
type Publisher interface {
	Publish(ctx context.Context, topic string, payload []byte, retain bool, qos byte) error
}

// Hardware is the device-control surface the bridge drives. Every method
// acquires or releases whatever ownership the operation needs; the bridge
// never touches a presentation or capture itself.
type Hardware interface {
	// RingSetColor lights the whole ring at one colour and intensity (0..1).
	RingSetColor(color RGB, intensity float64) error
	// RingSetEffect shows a built-in ring effect.
	RingSetEffect(presetID string) error
	// RingRelease hands the ring back to the device's configured baseline.
	RingRelease()
	// DisplaySetFace shows a built-in segment face.
	DisplaySetFace(presetID string) error
	// DisplayRelease hands the display back to the configured baseline.
	DisplayRelease()
	// ReleaseRingAndDisplay hands both surfaces back at once, which is what the
	// release action and the MQTT release button mean.
	ReleaseRingAndDisplay()
	// MediaCommand sends PLAY, PAUSE, NEXT or PREVIOUS to the connected
	// Bluetooth source.
	MediaCommand(cmd string) error
}

// Actions are the bridge's out-of-band requests: they are delivered to the
// session owner, which reconnects or republishes on its own goroutine.
type Actions struct {
	Reconnect func(reason string)
	Republish func(reason string)
}

// RingState is the bridge's view of the ring it currently drives.
type RingState struct {
	On         bool
	Color      RGB
	HasColor   bool
	Brightness uint8
	Effect     string
}

// DisplayState is the bridge's view of the segment display it currently drives.
type DisplayState struct {
	On   bool
	Face string
}

// Counters reports the bridge's traffic and failures.
type Counters struct {
	Published   uint64
	Commands    uint64
	CommandErrs uint64
	PublishErrs uint64
	States      uint64
	Events      uint64
}

// Snapshot is the bridge's externally visible status.
type Snapshot struct {
	Status      string
	LastError   string
	Entities    int
	RingOn      bool
	RingEffect  string
	DisplayOn   bool
	DisplayFace string
	Counters    Counters
}

// Bridge maps framework state and Home Assistant commands onto each other. It
// is safe for concurrent use: watcher goroutines publish state while the MQTT
// reader delivers commands.
type Bridge struct {
	topics  Topics
	catalog Catalog
	pub     Publisher
	hw      Hardware
	actions Actions
	logf    func(format string, args ...any)

	mu          sync.Mutex
	available   bool
	ringRGB     bool
	ring        RingState
	display     DisplayState
	media       MediaState
	status      string
	lastError   string
	counters    Counters
	ringPresets []Preset
	facePresets []Preset
}

// NewBridge returns a bridge that publishes through pub and drives hw. A nil
// logger is replaced by a no-op.
func NewBridge(topics Topics, catalog Catalog, pub Publisher, hw Hardware, actions Actions,
	logf func(string, ...any)) *Bridge {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Bridge{
		topics:  topics,
		catalog: catalog,
		pub:     pub,
		hw:      hw,
		actions: actions,
		logf:    logf,
		status:  BridgeStatusConnecting,
		ringRGB: catalogPublishesRGB(catalog),
		ring:    RingState{Brightness: 255, Color: RGB{R: 255, G: 255, B: 255}},
	}
}

// SetAvailable records whether a broker session is currently able to carry
// publishes. State publication tolerates an unavailable session (everything is
// republished on connect); an explicit republish request does not.
func (b *Bridge) SetAvailable(available bool) {
	b.mu.Lock()
	b.available = available
	b.mu.Unlock()
}

// Available reports whether a broker session can carry publishes.
func (b *Bridge) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.available
}

// Topics returns the bridge's topic layout.
func (b *Bridge) Topics() Topics { return b.topics }

// Catalog returns the current entity catalog.
func (b *Bridge) Catalog() Catalog {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.catalog
}

// catalogPublishesRGB reports whether the ring's discovery payload offers the
// rgb colour mode, so the state payload names the same mode the entity
// advertises.
func catalogPublishesRGB(catalog Catalog) bool {
	for _, entity := range catalog.Entities {
		if entity.Component != "light" || entity.Slug != SlugRing {
			continue
		}
		modes, _ := entity.Config["supported_color_modes"].([]string)
		for _, mode := range modes {
			if mode == "rgb" {
				return true
			}
		}
	}
	return false
}

// SetPresets records the discovered preset lists, which validate inbound
// effect and face commands.
func (b *Bridge) SetPresets(ring, faces []Preset) {
	b.mu.Lock()
	b.ringPresets = append([]Preset(nil), ring...)
	b.facePresets = append([]Preset(nil), faces...)
	b.mu.Unlock()
}

// Snapshot reports the current status for the app interface and the bridge
// status sensor.
func (b *Bridge) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{
		Status:      b.status,
		LastError:   b.lastError,
		Entities:    len(b.catalog.Entities),
		RingOn:      b.ring.On,
		RingEffect:  b.ring.Effect,
		DisplayOn:   b.display.On,
		DisplayFace: b.display.Face,
		Counters:    b.counters,
	}
}

// SetStatus records a status transition and publishes the bridge status
// sensor when the session is up.
func (b *Bridge) SetStatus(ctx context.Context, status string) {
	b.mu.Lock()
	b.status = status
	b.mu.Unlock()
	if err := b.publishStatus(ctx); err != nil {
		b.noteError("publish bridge status", err)
	}
}

// SetError records a failure and surfaces it on the bridge status sensor.
func (b *Bridge) SetError(ctx context.Context, detail string) {
	b.mu.Lock()
	b.lastError = detail
	b.status = BridgeStatusErrorPrefix
	b.mu.Unlock()
	if err := b.publishStatus(ctx); err != nil {
		b.noteError("publish bridge status", err)
	}
}

// SetLastError records a failure without publishing. It is the path used while
// no broker session exists: publishing would only fail again, and the next
// session publishes the recorded error with its status.
func (b *Bridge) SetLastError(detail string) {
	b.mu.Lock()
	b.lastError = detail
	b.mu.Unlock()
}

// SetLocalStatus records a status transition without publishing. It is used
// while the session is being torn down.
func (b *Bridge) SetLocalStatus(status string) {
	b.mu.Lock()
	b.status = status
	b.mu.Unlock()
}

// PublishAvailability publishes the retained availability payload. It is the
// first message after a reconnect and the last one before a clean shutdown;
// the broker's last will covers a crash.
func (b *Bridge) PublishAvailability(ctx context.Context, online bool) error {
	payload := PayloadOffline
	if online {
		payload = PayloadOnline
	}
	return b.publish(ctx, b.topics.StatusTopic(), []byte(payload), true, 1)
}

// PublishDiscovery publishes every entity's retained discovery configuration
// and then the online availability marker. It returns the number of discovery
// payloads written.
func (b *Bridge) PublishDiscovery(ctx context.Context) (int, error) {
	if !b.Available() {
		return 0, ErrNoSession
	}
	catalog := b.Catalog()
	count := 0
	for _, entity := range catalog.Entities {
		payload, err := catalog.DiscoveryPayload(entity)
		if err != nil {
			b.noteError("encode discovery", err)
			continue
		}
		topic := b.topics.DiscoveryTopic(entity.Component, entity.Slug)
		if err := b.publish(ctx, topic, payload, true, 1); err != nil {
			return count, err
		}
		count++
	}
	if err := b.PublishAvailability(ctx, true); err != nil {
		return count, err
	}
	return count, nil
}

// PublishStates republishes every entity state the bridge knows. Home
// Assistant treats a state topic as the current value, so a reconnect must
// refresh all of them; the discovery payloads are retained but states are not
// assumed to survive a broker restart.
func (b *Bridge) PublishStates(ctx context.Context) error {
	b.mu.Lock()
	ring, display, media := b.ring, b.display, b.media
	b.mu.Unlock()

	var errs []error
	if err := b.publishRingState(ctx, ring); err != nil {
		errs = append(errs, err)
	}
	if err := b.publishDisplayState(ctx, display); err != nil {
		errs = append(errs, err)
	}
	if err := b.publishMediaState(ctx, media); err != nil {
		errs = append(errs, err)
	}
	if err := b.publishStatus(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// PublishUptime publishes the device uptime in seconds.
func (b *Bridge) PublishUptime(ctx context.Context, seconds uint64) error {
	return b.publish(ctx, b.topics.StateTopic(SlugUptime), []byte(strconv.FormatUint(seconds, 10)), false, 0)
}

// PublishAudio publishes the mixer state: master volume as a percentage and
// the microphone mute flag.
func (b *Bridge) PublishAudio(ctx context.Context, volume float64, muted bool) error {
	percent := volume * 100
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	volumePayload := []byte(strconv.FormatFloat(percent, 'f', 1, 64))
	mutedPayload := []byte(PayloadOff)
	if muted {
		mutedPayload = []byte(PayloadOn)
	}
	var errs []error
	if err := b.publish(ctx, b.topics.StateTopic(SlugVolume), volumePayload, false, 0); err != nil {
		errs = append(errs, err)
	}
	if err := b.publish(ctx, b.topics.StateTopic(SlugMicrophoneMute), mutedPayload, false, 0); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// PublishSensor publishes one framework sensor reading.
func (b *Bridge) PublishSensor(ctx context.Context, sensor SensorInfo, value float64) error {
	payload := []byte(strconv.FormatFloat(value, 'f', -1, 64))
	return b.publish(ctx, b.topics.StateTopic(sensor.Slug), payload, false, 0)
}

// PublishMedia publishes the Bluetooth source state, its title and its
// attributes. Home Assistant has no MQTT media player, so this is one enum
// sensor plus one title sensor.
func (b *Bridge) PublishMedia(ctx context.Context, state MediaState) error {
	b.mu.Lock()
	b.media = state
	b.mu.Unlock()
	return b.publishMediaState(ctx, state)
}

func (b *Bridge) publishMediaState(ctx context.Context, state MediaState) error {
	attributes, err := json.Marshal(MediaAttributes(state))
	if err != nil {
		return Errorf("encode media attributes: %w", err)
	}
	var errs []error
	if err := b.publish(ctx, b.topics.StateTopic(SlugBluetoothState), []byte(MediaStatePayload(state)), false, 0); err != nil {
		errs = append(errs, err)
	}
	if err := b.publish(ctx, b.topics.StateTopic(SlugBluetoothTitle), []byte(MediaTitlePayload(state)), false, 0); err != nil {
		errs = append(errs, err)
	}
	if err := b.publish(ctx, b.topics.AttributesTopic(SlugBluetoothState), attributes, false, 0); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// PublishTouchEvent publishes one panel event for the touch event entity. It is
// an explicit request, so it reports failure while no session can carry it,
// unlike the periodic state publications.
func (b *Bridge) PublishTouchEvent(ctx context.Context, eventType string, extra map[string]any) error {
	if !b.Available() {
		return ErrNoSession
	}
	payload := map[string]any{"event_type": eventType}
	for key, value := range extra {
		payload[key] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Errorf("encode touch event: %w", err)
	}
	b.mu.Lock()
	b.counters.Events++
	b.mu.Unlock()
	return b.publish(ctx, b.topics.StateTopic(SlugTouch), encoded, false, 0)
}

// HandleMessage routes one inbound MQTT publication. An unknown topic and a
// malformed payload are reported, never applied: the device must not change
// state because of a message the bridge did not understand.
func (b *Bridge) HandleMessage(ctx context.Context, topic string, payload []byte) error {
	entity, ok := b.Catalog().EntityByTopic(topic)
	if !ok {
		return Errorf("no entity subscribes to topic %q", topic)
	}
	b.mu.Lock()
	b.counters.Commands++
	b.mu.Unlock()

	var err error
	switch entity.Slug {
	case SlugRing:
		err = b.applyLight(ctx, payload)
	case SlugReconnect:
		err = b.applyButton(payload, func() { b.requestReconnect("mqtt button") })
	case SlugRepublish:
		err = b.applyButton(payload, func() { b.requestRepublish("mqtt button") })
	case SlugRelease:
		err = b.applyButton(payload, b.releaseHardware)
		err = errors.Join(err, b.publishStateAfterRelease(ctx))
	case SlugDisplay:
		err = b.applyDisplaySwitch(ctx, payload)
	case SlugDisplayFace:
		err = b.applyDisplayFace(ctx, payload)
	case SlugBluetoothPlay, SlugBluetoothPause, SlugBluetoothNext, SlugBluetoothPrev:
		err = b.applyMediaCommand(ctx, entity, payload)
	default:
		err = Errorf("entity %s.%s has no command handler", entity.Component, entity.Slug)
	}
	if err != nil {
		b.mu.Lock()
		b.counters.CommandErrs++
		b.lastError = err.Error()
		b.mu.Unlock()
		b.logf("bridge: command on %s rejected: %v", topic, err)
		if statusErr := b.publishStatus(ctx); statusErr != nil {
			b.noteError("publish bridge status", statusErr)
		}
	}
	return err
}

func (b *Bridge) applyButton(payload []byte, action func()) error {
	if err := ParseButtonCommand(payload); err != nil {
		return err
	}
	action()
	return nil
}

// applyLight applies one light command. OFF releases the ring so the device's
// configured baseline resumes: the bridge never forces a black ring over a
// baseline the operator chose.
func (b *Bridge) applyLight(ctx context.Context, payload []byte) error {
	command, err := ParseLightCommand(payload)
	if err != nil {
		return err
	}
	b.mu.Lock()
	ring := b.ring
	b.mu.Unlock()

	effectCleared := false
	if command.HasEffect {
		id, clear, err := ParseEffectCommand([]byte(command.Effect), b.presets(KindRingEffect))
		if err != nil {
			return err
		}
		if clear {
			// Home Assistant sends the literal "None" to clear an effect; the
			// ring then falls back to its static colour.
			effectCleared = true
		} else {
			if err := b.hw.RingSetEffect(id); err != nil {
				return fmt.Errorf("ring effect %s: %w", id, err)
			}
			ring.On = true
			ring.Effect = id
			b.storeRing(ring)
			return b.publishRingState(ctx, ring)
		}
	}

	if command.State == PayloadOff {
		b.hw.RingRelease()
		ring = RingState{On: false, Color: ring.Color, HasColor: ring.HasColor, Brightness: ring.Brightness}
		b.storeRing(ring)
		return b.publishRingState(ctx, ring)
	}

	if command.HasBrightness {
		ring.Brightness = command.Brightness
		if ring.Brightness < 1 {
			// A zero brightness is not a state Home Assistant can hold, and the
			// ring has no separate dark mode: OFF releases it instead.
			ring.Brightness = 1
		}
	}
	if command.HasColor {
		ring.Color = command.Color
		ring.HasColor = true
	}
	if command.State != PayloadOn && !command.HasColor && !command.HasBrightness && !effectCleared {
		return nil
	}
	intensity := float64(ring.Brightness) / 255
	if err := b.hw.RingSetColor(ring.Color, intensity); err != nil {
		return fmt.Errorf("ring colour: %w", err)
	}
	ring.On = true
	ring.Effect = ""
	b.storeRing(ring)
	return b.publishRingState(ctx, ring)
}

func (b *Bridge) applyDisplaySwitch(ctx context.Context, payload []byte) error {
	on, err := ParseOnOff(payload)
	if err != nil {
		return err
	}
	b.mu.Lock()
	display := b.display
	b.mu.Unlock()
	if !on {
		b.hw.DisplayRelease()
		display.On = false
		b.storeDisplay(display)
		return b.publishDisplayState(ctx, display)
	}
	face := display.Face
	if face == "" {
		faces := b.presets(KindSegmentFace)
		if len(faces) == 0 {
			return Errorf("no segment face preset is available")
		}
		face = faces[0].ID
	}
	if err := b.hw.DisplaySetFace(face); err != nil {
		return fmt.Errorf("display face %s: %w", face, err)
	}
	display.On = true
	display.Face = face
	b.storeDisplay(display)
	return b.publishDisplayState(ctx, display)
}

func (b *Bridge) applyDisplayFace(ctx context.Context, payload []byte) error {
	faces := b.presets(KindSegmentFace)
	id, err := ParseFaceCommand(payload, faces)
	if err != nil {
		return err
	}
	if err := b.hw.DisplaySetFace(id); err != nil {
		return fmt.Errorf("display face %s: %w", id, err)
	}
	display := DisplayState{On: true, Face: id}
	b.storeDisplay(display)
	return b.publishDisplayState(ctx, display)
}

// applyMediaCommand validates one transport button press and forwards it. The
// framework's TOGGLE is deliberately not exposed: separate play and pause
// buttons are unambiguous in an automation, and a button press cannot express
// a toggle.
func (b *Bridge) applyMediaCommand(ctx context.Context, entity Entity, payload []byte) error {
	command, err := ParseMediaCommand(payload)
	if err != nil {
		return err
	}
	want, ok := mediaCommandForSlug(entity.Slug)
	if !ok {
		return Errorf("entity %s has no media command", entity.Slug)
	}
	if command != want {
		return Errorf("button %s accepts %s, not %s", entity.Slug, want, command)
	}
	if err := b.hw.MediaCommand(command); err != nil {
		return fmt.Errorf("media command %s: %w", command, err)
	}
	return nil
}

func mediaCommandForSlug(slug string) (string, bool) {
	switch slug {
	case SlugBluetoothPlay:
		return PayloadPlay, true
	case SlugBluetoothPause:
		return PayloadPause, true
	case SlugBluetoothNext:
		return PayloadNext, true
	case SlugBluetoothPrev:
		return PayloadPrevious, true
	default:
		return "", false
	}
}

// releaseHardware hands the two operator-facing surfaces back to the device in
// one step. The touch capture is a configured exposure, not an ad-hoc
// acquisition, so it stays governed by the setting that enabled it.
func (b *Bridge) releaseHardware() {
	b.hw.ReleaseRingAndDisplay()
	b.mu.Lock()
	b.ring = RingState{Brightness: b.ring.Brightness, Color: b.ring.Color, HasColor: b.ring.HasColor}
	b.display = DisplayState{}
	b.mu.Unlock()
	b.logf("bridge: hardware released; the device's configured baseline owns the ring and display again")
}

// publishStateAfterRelease republishes the states a release changed.
func (b *Bridge) publishStateAfterRelease(ctx context.Context) error {
	b.mu.Lock()
	ring, display := b.ring, b.display
	b.mu.Unlock()
	var errs []error
	if err := b.publishRingState(ctx, ring); err != nil {
		errs = append(errs, err)
	}
	if err := b.publishDisplayState(ctx, display); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (b *Bridge) publishRingState(ctx context.Context, ring RingState) error {
	payload := map[string]any{"state": PayloadOff}
	if ring.On {
		payload["state"] = PayloadOn
		// Home Assistant ignores a zero brightness in a state payload, which
		// would leave a stale value in its UI; the ring has no separate dark
		// state, so the floor is one step.
		brightness := int(ring.Brightness)
		if brightness < 1 {
			brightness = 1
		}
		payload["brightness"] = brightness
		if b.ringRGB {
			// The colour is only applied by the modern JSON schema when the
			// payload names the colour mode it is expressing.
			payload["color_mode"] = "rgb"
			payload["color"] = map[string]int{"r": int(ring.Color.R), "g": int(ring.Color.G), "b": int(ring.Color.B)}
		}
		if ring.Effect != "" {
			payload["effect"] = ring.Effect
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Errorf("encode ring state: %w", err)
	}
	return b.publish(ctx, b.topics.StateTopic(SlugRing), encoded, false, 0)
}

func (b *Bridge) publishDisplayState(ctx context.Context, display DisplayState) error {
	value := PayloadOff
	if display.On {
		value = PayloadOn
	}
	var errs []error
	if err := b.publish(ctx, b.topics.StateTopic(SlugDisplay), []byte(value), false, 0); err != nil {
		errs = append(errs, err)
	}
	if display.Face != "" {
		if err := b.publish(ctx, b.topics.StateTopic(SlugDisplayFace), []byte(display.Face), false, 0); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *Bridge) publishStatus(ctx context.Context) error {
	b.mu.Lock()
	status, lastError := b.status, b.lastError
	counters := b.counters
	ringOn, ringEffect := b.ring.On, b.ring.Effect
	displayOn, displayFace := b.display.On, b.display.Face
	entities := len(b.catalog.Entities)
	b.mu.Unlock()

	attributes := map[string]any{
		"entities":       entities,
		"published":      counters.Published,
		"commands":       counters.Commands,
		"command_errors": counters.CommandErrs,
		"publish_errors": counters.PublishErrs,
		"states":         counters.States,
		"events":         counters.Events,
		"ring_on":        ringOn,
		"display_on":     displayOn,
	}
	if ringEffect != "" {
		attributes["ring_effect"] = ringEffect
	}
	if displayFace != "" {
		attributes["display_face"] = displayFace
	}
	if lastError != "" {
		attributes["last_error"] = lastError
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return Errorf("encode bridge attributes: %w", err)
	}
	var errs []error
	if err := b.publish(ctx, b.topics.StateTopic(SlugBridgeStatus), []byte(status), false, 0); err != nil {
		errs = append(errs, err)
	}
	if err := b.publish(ctx, b.topics.AttributesTopic(SlugBridgeStatus), encoded, false, 0); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (b *Bridge) presets(kind string) []Preset {
	b.mu.Lock()
	defer b.mu.Unlock()
	if kind == KindRingEffect {
		return append([]Preset(nil), b.ringPresets...)
	}
	return append([]Preset(nil), b.facePresets...)
}

func (b *Bridge) storeRing(ring RingState) {
	b.mu.Lock()
	b.ring = ring
	b.mu.Unlock()
}

func (b *Bridge) storeDisplay(display DisplayState) {
	b.mu.Lock()
	b.display = display
	b.mu.Unlock()
}

func (b *Bridge) requestReconnect(reason string) {
	if b.actions.Reconnect != nil {
		b.actions.Reconnect(reason)
	}
}

func (b *Bridge) requestRepublish(reason string) {
	if b.actions.Republish != nil {
		b.actions.Republish(reason)
	}
}

// publish writes one message and keeps the counters and the last-error field
// honest. A disconnected publisher is not an error: the next session republishes
// everything, so the bridge does not fill the log with one failure per entity
// while it is offline.
func (b *Bridge) publish(ctx context.Context, topic string, payload []byte, retain bool, qos byte) error {
	if err := b.pub.Publish(ctx, topic, payload, retain, qos); err != nil {
		if errors.Is(err, ErrNoSession) {
			return nil
		}
		b.noteError("publish "+topic, err)
		return err
	}
	b.mu.Lock()
	b.counters.Published++
	if !retain {
		b.counters.States++
	}
	b.mu.Unlock()
	return nil
}

func (b *Bridge) noteError(what string, err error) {
	b.mu.Lock()
	b.counters.PublishErrs++
	b.lastError = what + ": " + err.Error()
	b.mu.Unlock()
	b.logf("bridge: %s: %v", what, err)
}
