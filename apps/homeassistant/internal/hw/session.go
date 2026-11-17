package hw

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"capsulad/apps/homeassistant/internal/appcfg"
	"capsulad/apps/homeassistant/internal/ha"
	"capsulad/apps/homeassistant/internal/mqtt"
	"capsulad/sdk/capsulad"
)

// ErrReconnectRequested ends a session because the operator asked for a fresh
// connection (the MQTT button or an app action).
var ErrReconnectRequested = errors.New("hw: reconnect requested")

// Reconnect backoff bounds.
const (
	minBackoff = time.Second
	maxBackoff = time.Minute
	// backoffResetAfter is how long a session must have been usable before the
	// backoff is reset. A session that never became usable, or that keeps
	// dropping moments after it did, therefore decays towards maxBackoff instead
	// of reconnecting once a second: a broker that answers CONNACK and then
	// refuses or ignores SUBSCRIBE must not make the device flap online/offline
	// (and republish the whole retained discovery set) forever.
	backoffResetAfter = 30 * time.Second
	// publishTimeout bounds one outbound MQTT write.
	publishTimeout = 5 * time.Second
	// defaultSubscribeTimeout bounds the SUBACK wait. The subscription is what
	// makes Home Assistant's commands arrive, so a broker that accepts the
	// SUBSCRIBE and never answers must fail the session and be retried instead
	// of leaving a connection that looks online and answers nothing.
	defaultSubscribeTimeout = 10 * time.Second
	// shutdownTimeout bounds the offline publish on a clean stop.
	shutdownTimeout = 3 * time.Second
)

// dialFunc opens one broker byte stream. dialCtx bounds DNS/TCP/TLS, ioCtx
// bounds every later read and write (and therefore outlives dialCtx).
// Production uses dialBroker, the shared SDK transport; tests substitute an
// in-memory stream.
type dialFunc func(dialCtx, ioCtx context.Context, env capsulad.Env, cfg appcfg.Config) (mqtt.Conn, error)

// Session owns the MQTT connection: it dials through the SDK, publishes the
// discovery and state snapshot, subscribes to the command topics, pumps
// inbound commands into the bridge and reconnects with bounded backoff.
//
// It is also the bridge's Publisher. While it is disconnected a publish fails
// visibly; the bridge keeps the last desired state so the next successful
// session republishes everything.
type Session struct {
	env      capsulad.Env
	cfg      appcfg.Config
	bridge   *ha.Bridge
	discover func(ctx context.Context) (string, int, error)
	dial     dialFunc
	logf     func(format string, args ...any)
	// subscribeTimeout bounds the SUBACK wait for this session.
	subscribeTimeout time.Duration

	mu        sync.RWMutex
	client    *mqtt.Client
	connected bool
	closing   bool
	endpoint  string
	lastError string

	reconnect chan string

	// Stats
	published uint64
	commands  uint64
	failures  uint64
}

// NewSession returns a session for one validated configuration.
func NewSession(env capsulad.Env, cfg appcfg.Config, bridge *ha.Bridge,
	discover func(context.Context) (string, int, error), logf func(string, ...any)) *Session {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Session{
		env:              env,
		cfg:              cfg,
		bridge:           bridge,
		discover:         discover,
		dial:             dialBroker,
		logf:             logf,
		subscribeTimeout: defaultSubscribeTimeout,
		reconnect:        make(chan string, 1),
	}
}

// Endpoint is the "host:port" of the session in use, including an endpoint
// found by mDNS discovery; empty while no session has been established.
func (s *Session) Endpoint() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.endpoint
}

// Connected reports whether a broker session is live.
func (s *Session) Connected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connected
}

// LastError is the last session failure, for the app interface and the bridge
// status attributes.
func (s *Session) LastError() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastError
}

// Stats reports session counters.
func (s *Session) Stats() (published, commands, failures uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.published, s.commands, s.failures
}

// Reconnect asks the session loop to drop the current connection and dial
// again. It never blocks: a request while already connecting replaces the
// pending one.
func (s *Session) Reconnect(reason string) {
	select {
	case s.reconnect <- reason:
	default:
	}
	s.logf("session: reconnect requested (%s)", reason)
}

// Publish implements ha.Publisher.
func (s *Session) Publish(ctx context.Context, topic string, payload []byte, retain bool, qos byte) error {
	s.mu.RLock()
	client, closing := s.client, s.closing
	s.mu.RUnlock()
	if client == nil || closing {
		// The session is gone or already publishing its last message; a caller
		// gets the same explicit "no session" answer as before the connect.
		return ha.ErrNoSession
	}
	writeCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	if err := client.Publish(writeCtx, mqtt.Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain}); err != nil {
		s.mu.Lock()
		s.failures++
		s.lastError = err.Error()
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.published++
	s.mu.Unlock()
	return nil
}

// Run keeps one broker session alive until ctx ends. It is the only goroutine
// that dials; a failed or lost session is retried with exponential backoff up
// to one minute.
func (s *Session) Run(ctx context.Context) {
	// Zero seeds the sequence: the first retry follows minBackoff.
	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		usable, err := s.runOnce(ctx)
		ranFor := time.Since(started)
		if ctx.Err() != nil {
			return
		}
		backoff = advanceBackoff(backoff, usable, ranFor)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.mu.Lock()
			s.lastError = err.Error()
			s.connected = false
			s.mu.Unlock()
			s.bridge.SetLastError(err.Error())
			s.logf("session: %v", err)
		}

		// A pending reconnect request is satisfied by the next attempt, so do
		// not wait out a backoff because of one; a deliberate reconnect does
		// not wait at all.
		drainReconnect(s.reconnect)
		if errors.Is(err, ErrReconnectRequested) {
			backoff = minBackoff
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func drainReconnect(ch chan string) {
	select {
	case <-ch:
	default:
	}
}

// advanceBackoff decides the delay before the next attempt, given the delay used
// for the attempt that just ended. A session that never became usable, or that
// was usable only briefly, backs off exponentially (1 s, 2 s, 4 s, … up to
// maxBackoff); one that held up for backoffResetAfter starts again promptly.
//
// The seed is zero, so the first retry after a failed start still comes after
// minBackoff rather than twice it.
func advanceBackoff(previous time.Duration, usable bool, ranFor time.Duration) time.Duration {
	if usable && ranFor >= backoffResetAfter {
		return minBackoff
	}
	switch {
	case previous < minBackoff:
		return minBackoff
	case previous >= maxBackoff:
		return maxBackoff
	default:
		next := previous * 2
		if next > maxBackoff {
			next = maxBackoff
		}
		return next
	}
}

// runOnce performs one connect → publish → subscribe → pump cycle. It returns
// whether the connection became *usable* — dialled, discovery published and the
// command subscriptions acknowledged. A failure before that point reports false
// so the reconnect backoff keeps decaying; an established session that later
// drops reports true.
func (s *Session) runOnce(ctx context.Context) (bool, error) {
	host, port := s.cfg.Host, s.cfg.Port
	if host == "" {
		if !s.cfg.DiscoverBroker || s.discover == nil {
			return false, fmt.Errorf("no broker host is configured and broker discovery is off")
		}
		discovered, discoveredPort, err := s.discover(ctx)
		if err != nil {
			return false, fmt.Errorf("discover broker: %w", err)
		}
		host, port = discovered, discoveredPort
		s.logf("session: discovered broker at %s (port %d)", host, port)
	}
	endpoint := s.cfg
	endpoint.Host, endpoint.Port = host, port

	// The session's I/O context is detached from the runtime context: the
	// retained offline marker and the DISCONNECT are written after the runtime
	// context is cancelled, and the SDK stream aborts an operation whose context
	// is done. Only endSession cancels it, once those writes have gone out.
	ioCtx, cancelIO := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelIO()

	dial := s.dial
	if dial == nil {
		dial = dialBroker
	}
	conn, err := dial(ctx, ioCtx, s.env, endpoint)
	if err != nil {
		return false, err
	}
	client := mqtt.NewClient(conn, mqtt.Config{
		ClientID:    s.clientID(),
		Username:    endpoint.Username,
		HasUsername: endpoint.Username != "",
		Password:    endpoint.Password,
		HasPassword: endpoint.Password != "",
		KeepAlive:   endpoint.KeepAlive,
		// The last will is the crash path: a broker that notices the dead
		// connection marks the device unavailable without the app's help.
		Will: &mqtt.Message{
			Topic:   s.bridge.Topics().StatusTopic(),
			Payload: []byte(ha.PayloadOffline),
			QoS:     1,
			Retain:  true,
		},
	})
	if err := client.Connect(); err != nil {
		_ = conn.Close()
		return false, err
	}

	s.mu.Lock()
	s.client = client
	s.connected = true
	s.closing = false
	s.endpoint = HostPort(host, port)
	s.lastError = ""
	s.mu.Unlock()
	s.bridge.SetAvailable(true)
	defer func() {
		s.mu.Lock()
		s.client = nil
		s.connected = false
		s.endpoint = ""
		s.mu.Unlock()
		s.bridge.SetAvailable(false)
	}()

	// The reader must run before the first publish: a QoS 1 publish waits for
	// its PUBACK, and only the session's reader consumes acknowledgements. It
	// shares the session's I/O context, so it ends exactly when endSession has
	// finished the final writes.
	runDone := make(chan error, 1)
	go func() { runDone <- client.Run(ioCtx) }()

	// From here the device is reachable from Home Assistant: announce it,
	// publish the retained entity set, then the current values, then listen.
	s.bridge.SetStatus(ctx, ha.BridgeStatusOnline)
	count, err := s.bridge.PublishDiscovery(ctx)
	if err != nil {
		s.endSession(client, cancelIO, runDone)
		return false, fmt.Errorf("publish discovery: %w", err)
	}
	s.logf("session: connected to %s (port %d); %d discovery payloads published", host, port, count)

	commands := s.bridge.Catalog().CommandTopics()
	if len(commands) > 0 {
		timeout := s.subscribeTimeout
		if timeout <= 0 {
			timeout = defaultSubscribeTimeout
		}
		subscribeCtx, cancelSubscribe := context.WithTimeout(ctx, timeout)
		err := client.Subscribe(subscribeCtx, commands...)
		cancelSubscribe()
		if err != nil {
			// Connected but not subscribed: the bridge would look online while
			// Home Assistant's commands went nowhere, so this is not a usable
			// session and must back off like any other failure to start.
			s.endSession(client, cancelIO, runDone)
			return false, fmt.Errorf("subscribe within %s: %w", timeout, err)
		}
	}
	if err := s.bridge.PublishStates(ctx); err != nil {
		s.logf("session: initial state publish: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			s.endSession(client, cancelIO, runDone)
			return true, ctx.Err()
		case reason := <-s.reconnect:
			s.logf("session: reconnecting (%s)", reason)
			s.endSession(client, cancelIO, runDone)
			return true, ErrReconnectRequested
		case err := <-runDone:
			// A broker or transport failure: the broker already published the
			// last will, so Home Assistant sees the device offline without us.
			return true, err
		case msg, ok := <-client.Messages():
			if !ok {
				return true, client.Err()
			}
			s.handle(ctx, msg)
		}
	}
}

// endSession tears one established session down in the only order that lets the
// broker learn about it: the retained offline marker, then DISCONNECT, then the
// session's I/O context, then the reader.
func (s *Session) endSession(client *mqtt.Client, cancelIO context.CancelFunc, runDone <-chan error) {
	s.shutdown(client)
	cancelIO()
	<-runDone
}

func (s *Session) handle(ctx context.Context, msg mqtt.Message) {
	s.mu.Lock()
	s.commands++
	s.mu.Unlock()
	handleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.bridge.HandleMessage(handleCtx, msg.Topic, msg.Payload); err != nil {
		// HandleMessage already logged and published the bridge status; the
		// session stays up because one bad command is not a connection fault.
		s.logf("session: command on %s: %v", msg.Topic, err)
	}
}

// shutdown publishes the retained offline marker and disconnects cleanly. The
// offline publish happens before DISCONNECT because a clean DISCONNECT makes a
// broker discard the last will.
//
// The session is marked closing between the two so a state publication from a
// watcher that is still winding down cannot report a failure for a session that
// is deliberately going away.
func (s *Session) shutdown(client *mqtt.Client) {
	offCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := s.bridge.PublishAvailability(offCtx, false); err != nil {
		s.logf("session: publish offline: %v", err)
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	if err := client.Disconnect(); err != nil && !errors.Is(err, mqtt.ErrDisconnected) {
		s.logf("session: disconnect: %v", err)
	}
	s.bridge.SetLocalStatus(ha.BridgeStatusConnecting)
}

// clientID derives a stable broker client id. Home Assistant's broker keeps a
// client id reserved for a session; a stable id means a reconnect replaces the
// previous connection instead of leaving a ghost.
func (s *Session) clientID() string {
	if s.cfg.ClientID != "" {
		return s.cfg.ClientID
	}
	return "capsulad-ha-" + s.bridge.Topics().NodeID
}
