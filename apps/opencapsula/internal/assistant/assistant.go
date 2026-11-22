// Package assistant runs the OpenCapsula voice client: one websocket device
// link, a supervisor-friendly reconnect loop, an energy-endpointed microphone
// turn and the framework-side commands the server can invoke.
package assistant

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"capsulad/apps/opencapsula/internal/commands"
	"capsulad/apps/opencapsula/internal/config"
	"capsulad/apps/opencapsula/internal/protocol"
	"capsulad/apps/opencapsula/internal/remote"
	"capsulad/apps/opencapsula/internal/ring"
	"capsulad/sdk/capsulad"
	"capsulad/sdk/websocket"
)

const (
	// Bounds on the reconnect loop. A successful ready resets the delay.
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
	// disconnectedDwell bounds the amber disconnected indication. The ring is
	// released afterward instead of staying claimed during reconnect backoff.
	disconnectedDwell = 1750 * time.Millisecond

	// activationQueue keeps a short burst of presses; each is matched to a
	// turn in order.
	activationQueue = 8
	actionQueue     = 4

	// dismissAction is the manifest-declared action the framework invokes.
	dismissAction = "dismiss"
)

// agentVersion is the build identity sent as agent_version. It is a build
// constant, not a protocol version.
const agentVersion = "0.1.0"

// Assistant is the process-wide state. One instance serves one supervised run.
type Assistant struct {
	ctx    context.Context
	env    capsulad.Env
	client *capsulad.Client
	log    *log.Logger
	ring   *ring.View

	activations chan activationItem
	actions     chan actionItem
	wake        chan struct{}

	settingsReady chan struct{}
	settingsOnce  sync.Once

	mu          sync.Mutex
	cfg         config.Config
	revision    uint64
	frameworkID string
	fallbackID  string
	lastRingLog time.Time
	// disconnected marks that this outage already showed the amber indication,
	// so repeated dial failures during backoff do not re-claim the ring.
	disconnected bool

	seq atomic.Uint64
}

// activationItem is a framework activation plus its local expiry, so a turn
// that waits behind a reconnect cannot answer a stale press.
type activationItem struct {
	activation   *capsulad.Activation
	queuedAt     time.Time
	expiresAfter time.Duration
}

func (a activationItem) expired() bool {
	return a.expiresAfter > 0 && time.Since(a.queuedAt) >= a.expiresAfter
}

// actionItem is one manifest action press.
type actionItem struct {
	invocation *capsulad.ActionInvoked
}

// New builds the assistant. ResolveIdentity should be called before Run; it
// fills the framework device identity used when the setting is empty.
func New(ctx context.Context, env capsulad.Env, client *capsulad.Client, logger *log.Logger) *Assistant {
	if logger == nil {
		logger = log.Default()
	}
	a := &Assistant{
		ctx:           ctx,
		env:           env,
		client:        client,
		log:           logger,
		ring:          ring.New(client, logger),
		activations:   make(chan activationItem, activationQueue),
		actions:       make(chan actionItem, actionQueue),
		wake:          make(chan struct{}, 1),
		settingsReady: make(chan struct{}),
		cfg:           config.Default(),
	}
	a.fallbackID = fallbackIdentity(env)
	return a
}

// ResolveIdentity asks the framework for the stable device id. Failure is not
// fatal: the app logs it and uses a local fallback for this process.
func (a *Assistant) ResolveIdentity(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info struct {
		DeviceID string `json:"deviceId"`
	}
	err := a.client.CallJSON(callCtx, "", "capsulad.framework.v1.DeviceService", "GetInfo",
		struct{}{}, &info, 5*time.Second)
	if err != nil {
		a.log.Printf("opencapsula: device identity unavailable: %v; using %s", err, a.fallbackID)
		return
	}
	id := strings.TrimSpace(info.DeviceID)
	if id == "" {
		a.log.Printf("opencapsula: framework reported no device id; using %s", a.fallbackID)
		return
	}
	a.mu.Lock()
	a.frameworkID = id
	a.mu.Unlock()
	a.log.Printf("opencapsula: device id %s", id)
}

// Handlers returns the bus handlers the main function installs. Callbacks run
// on the SDK's bus thread and must never block, so they only enqueue.
func (a *Assistant) Handlers() capsulad.Handlers {
	return capsulad.Handlers{
		Activation: func(_ context.Context, activation *capsulad.Activation) {
			a.onActivation(activation)
		},
		Settings: a.onSettings,
		Action: func(_ context.Context, invocation *capsulad.ActionInvoked) {
			a.onAction(invocation)
		},
		Disconnected: func(err error) {
			a.log.Printf("opencapsula: framework bus disconnected: %v", err)
		},
	}
}

func (a *Assistant) onActivation(activation *capsulad.Activation) {
	if activation == nil {
		return
	}
	if activation.ID == "" {
		a.log.Printf("opencapsula: activation without an id dropped origin=%s", activation.Origin.String())
		return
	}
	item := activationItem{activation: activation, queuedAt: time.Now()}
	if activation.ExpiresMonotonicNS > activation.MonotonicTimeNS {
		item.expiresAfter = time.Duration(activation.ExpiresMonotonicNS-activation.MonotonicTimeNS) * time.Nanosecond
	}
	select {
	case a.activations <- item:
	default:
		a.log.Printf("opencapsula: activation %s dropped; queue is full", activation.ID)
		return
	}
	// Wake a backoff sleep so a press is not ignored while the link recovers.
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *Assistant) onAction(invocation *capsulad.ActionInvoked) {
	if invocation == nil || invocation.ActionID != dismissAction {
		return
	}
	select {
	case a.actions <- actionItem{invocation: invocation}:
	default:
		a.log.Printf("opencapsula: dismiss action dropped; queue is full")
	}
}

func (a *Assistant) onSettings(changed *capsulad.SettingsChanged) {
	if changed == nil {
		return
	}
	a.settingsOnce.Do(func() { close(a.settingsReady) })
	a.mu.Lock()
	if changed.Revision != 0 && changed.Revision < a.revision {
		current := a.revision
		a.mu.Unlock()
		a.log.Printf("opencapsula: settings revision %d ignored; current revision is %d", changed.Revision, current)
		return
	}
	old := a.cfg
	next, rejected := config.Apply(a.cfg, changed)
	a.cfg = next
	if changed.Revision > a.revision {
		a.revision = changed.Revision
	}
	a.mu.Unlock()
	for _, reason := range rejected {
		a.log.Printf("opencapsula: setting rejected: %s", reason)
	}
	if old.ServerURL != next.ServerURL || old.Token != next.Token {
		a.log.Printf("opencapsula: server URL or token changed; the next connection uses it")
	}
	if old.FollowUp != next.FollowUp || old.Earcons != next.Earcons {
		a.log.Printf("opencapsula: follow_up=%t earcons=%t", next.FollowUp, next.Earcons)
	}
}

// Config returns a copy of the current validated configuration.
func (a *Assistant) Config() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *Assistant) deviceID(cfg config.Config) string {
	if cfg.DeviceID != "" {
		return cfg.DeviceID
	}
	a.mu.Lock()
	id := a.frameworkID
	a.mu.Unlock()
	if id != "" {
		return id
	}
	return a.fallbackID
}

func (a *Assistant) nextTurnID(cfg config.Config) string {
	seq := a.seq.Add(1)
	return fmt.Sprintf("%s-%d-%d", a.deviceID(cfg), time.Now().Unix(), seq)
}

// Run keeps one link up until ctx ends. A failed attempt waits with a capped
// exponential backoff; a framework activation wakes the wait but an
// immediately failing dial still backs off before the next attempt.
func (a *Assistant) Run() {
	defer a.ring.Release()
	// The framework sends the initial settings snapshot right after AppReady.
	// Waiting briefly avoids a first connection to the manifest default when
	// the operator configured another server. A missing snapshot still starts
	// with the defaults.
	select {
	case <-a.settingsReady:
	case <-time.After(2 * time.Second):
		a.log.Printf("opencapsula: no settings snapshot yet; connecting with current defaults")
	case <-a.ctx.Done():
		return
	}
	backoff := minBackoff
	for {
		if a.ctx.Err() != nil {
			return
		}
		cfg := a.Config()
		hello := a.hello(cfg)
		link, err := remote.Dial(a.ctx, a.env, cfg, hello)
		if err != nil {
			if a.ctx.Err() != nil {
				return
			}
			if IsAuthError(err) {
				a.log.Printf("opencapsula: the server refused the device token; check the token setting")
			}
			a.log.Printf("opencapsula: connect failed: %v", err)
			a.showDisconnected()
			if !a.wait(backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = minBackoff
		a.mu.Lock()
		a.disconnected = false
		a.mu.Unlock()
		a.log.Printf("opencapsula: ready session=%s server_vad=%t output=%d Hz/%d ch",
			link.Ready().SessionID, link.Ready().ServerVAD,
			link.Ready().OutputFormat.SampleRateHertz, link.Ready().OutputFormat.Channels)
		// The link is up and no turn has claimed the ring yet.
		a.ring.Release()

		session := newSession(a, link)
		session.run()
		link.CloseNow()
		if a.ctx.Err() != nil {
			return
		}
		a.showDisconnected()
		a.log.Printf("opencapsula: link closed; reconnecting in %s", backoff)
		if !a.wait(backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func (a *Assistant) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-a.ctx.Done():
		return false
	case <-timer.C:
		return true
	case <-a.wake:
		return true
	}
}

func (a *Assistant) showDisconnected() {
	a.mu.Lock()
	if a.disconnected {
		a.mu.Unlock()
		return
	}
	a.disconnected = true
	a.mu.Unlock()

	if err := a.ring.Show(a.ctx, ring.StateDisconnected); err != nil {
		a.mu.Lock()
		a.disconnected = false
		show := time.Since(a.lastRingLog) > 30*time.Second
		if show {
			a.lastRingLog = time.Now()
		}
		a.mu.Unlock()
		if show {
			a.log.Printf("opencapsula: disconnected ring unavailable: %v", err)
		}
		return
	}
	generation := a.ring.Generation()
	go func() {
		timer := time.NewTimer(disconnectedDwell)
		defer timer.Stop()
		select {
		case <-timer.C:
			if a.ring.Generation() == generation {
				a.ring.Release()
			}
		case <-a.ctx.Done():
		}
	}()
}

func (a *Assistant) hello(cfg config.Config) *protocol.Message {
	capabilities := make([]string, len(commands.Capabilities))
	copy(capabilities, commands.Capabilities)
	return &protocol.Message{Type: protocol.TypeHello, Hello: &protocol.Hello{
		ProtocolVersion: protocol.Version,
		DeviceID:        a.deviceID(cfg),
		InputFormat:     protocol.DeviceInputFormat,
		Capabilities:    capabilities,
		Language:        cfg.Language,
		Location:        cfg.Location,
		TimeZone:        cfg.Timezone,
		AgentVersion:    agentVersion,
	}}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

// IsAuthError reports a refused token so main can log a directly useful hint.
func IsAuthError(err error) bool {
	return errors.Is(err, websocket.ErrUnauthorized)
}

func fallbackIdentity(env capsulad.Env) string {
	if host, err := os.Hostname(); err == nil {
		host = strings.TrimSpace(host)
		if host != "" {
			return host
		}
	}
	if env.RunID != "" {
		return env.RunID
	}
	var random [4]byte
	for i := range random {
		random[i] = byte(rand.Uint32())
	}
	return "opencapsula-" + hex.EncodeToString(random[:])
}
