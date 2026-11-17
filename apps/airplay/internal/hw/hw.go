// Package hw binds the AirPlay receiver to the capsulad framework: it is the
// only place in this app that touches the native SDK.
//
// Everything here goes through framework services. Sockets come from
// daemon-mediated listeners (the app never binds one itself), advertisements
// from the daemon's Avahi responder, output through presentations and the
// framework mixer, and state through the app's own typed settings.
package hw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"capsulad/sdk/capsulad"

	"capsulad/apps/airplay/internal/ptprecv"
	"capsulad/apps/airplay/internal/session"
)

// Framework services used here.
const (
	configService = "capsulad.framework.v1.ApplicationConfigurationService"
	mediaService  = "capsulad.contracts.media.v1.MediaPlayerService"
)

// BlockingIOTimeout is the timeout passed to native calls that are meant to
// wait until data arrives or the handle is closed.
const BlockingIOTimeout = 0

// Env is the injected runtime identity.
type Env struct {
	Library      string
	Socket       string
	RunID        string
	AppID        string
	SDK          string
	DeviceSerial string
}

// FromEnv reads the framework-injected environment.
func FromEnv() Env {
	e := capsulad.FromEnv()
	return Env{Library: e.Library, Socket: e.Socket, RunID: e.RunID, AppID: e.AppID, SDK: e.SDK, DeviceSerial: e.DeviceSerial}
}

// Runtime is the SDK-backed implementation of the session transport seams plus
// the framework surfaces the receiver needs.
type Runtime struct {
	ctx    context.Context
	client *capsulad.Client
	env    capsulad.Env
	logf   func(string, ...any)

	mu         sync.Mutex
	writeMu    sync.Mutex
	settings   map[string]string
	revision   uint64
	listeners  map[uint64]closer
	nextListen uint64
	mdnsIDs    []string
	ptp        *ptprecv.Receiver
	ptpOnce    sync.Once
}

// NewRuntime connects to the framework socket.
func NewRuntime(ctx context.Context, env Env, logf func(string, ...any)) (*Runtime, error) {
	if logf == nil {
		logf = log.Printf
	}
	client, err := capsulad.Connect(ctx, capsulad.Env{
		Library:      env.Library,
		Socket:       env.Socket,
		RunID:        env.RunID,
		AppID:        env.AppID,
		SDK:          env.SDK,
		DeviceSerial: env.DeviceSerial,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to the framework: %w", err)
	}
	rt := &Runtime{
		ctx:       ctx,
		client:    client,
		listeners: map[uint64]closer{},
		env:       capsulad.Env{Library: env.Library, Socket: env.Socket, RunID: env.RunID, AppID: env.AppID, SDK: env.SDK, DeviceSerial: env.DeviceSerial},
		logf:      logf,
		settings:  map[string]string{},
	}
	return rt, nil
}

// Client exposes the bus client for the media-contract handlers.
func (rt *Runtime) Client() *capsulad.Client { return rt.client }

// Log writes one line to the app log.
func (rt *Runtime) Log(format string, args ...any) { rt.logf(format, args...) }

// SetHandlers installs the framework callbacks. Ready fires once the app is
// ready; Settings delivers every app-settings snapshot (including the first).
func (rt *Runtime) SetHandlers(
	onReady func(),
	onSettings func(revision uint64, values map[string]string),
	onAction func(id string) (bool, string),
) {
	rt.client.SetHandlers(capsulad.Handlers{
		Ready: func() {
			if onReady != nil {
				onReady()
			}
		},
		Settings: func(changed *capsulad.SettingsChanged) {
			if changed == nil {
				return
			}
			values := make(map[string]string, len(changed.Values))
			for _, v := range changed.Values {
				values[v.Key] = v.ValueJSON
			}
			rt.mu.Lock()
			rt.settings = values
			rt.revision = changed.Revision
			rt.mu.Unlock()
			if onSettings != nil {
				onSettings(changed.Revision, values)
			}
		},
		Action: func(_ context.Context, invoked *capsulad.ActionInvoked) {
			if invoked == nil || onAction == nil {
				return
			}
			accepted, detail := onAction(invoked.ActionID)
			if err := rt.client.NotifyActionResult(invoked.ID, accepted, detail); err != nil {
				rt.logf("reporting the %q action result: %v", invoked.ActionID, err)
			}
		},
		Disconnected: func(err error) {
			rt.logf("framework connection ended: %v", err)
		},
	})
}

// Start runs the app bus until the context ends.
func (rt *Runtime) Start() error { return rt.client.Start(rt.ctx) }

// Close releases the framework connection.
func (rt *Runtime) Close() error { return rt.client.Close() }

// Settings returns the newest app-settings snapshot and its revision.
func (rt *Runtime) Settings() (uint64, map[string]string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make(map[string]string, len(rt.settings))
	for k, v := range rt.settings {
		out[k] = v
	}
	return rt.revision, out
}

// Setting returns one decoded JSON setting value ("" when unset or on a type
// mismatch).
func SettingString(values map[string]string, key string) string {
	raw, ok := values[key]
	if !ok || raw == "" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ""
	}
	return s
}

// SettingBool decodes a boolean setting.
func SettingBool(values map[string]string, key string, fallback bool) bool {
	raw, ok := values[key]
	if !ok || raw == "" {
		return fallback
	}
	var b bool
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return fallback
	}
	return b
}

// SettingInt decodes an integer setting.
func SettingInt(values map[string]string, key string, fallback int) int {
	raw, ok := values[key]
	if !ok || raw == "" {
		return fallback
	}
	var n int
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		return fallback
	}
	return n
}

// PersistSetting writes one setting through the compare-and-swap app
// configuration API. The receiver uses it only for its own generated state
// (device identity, pairing records, the RAOP key); user-facing settings stay
// under the administrator's control.
func (rt *Runtime) PersistSetting(key, value string) error {
	// Generated state is written two or three times in a row at startup. Each
	// compare-and-swap invalidates the revision the next write would use, so
	// writes are serialized and the revision carried by the update response is
	// adopted; a refused write is retried once with whatever revision arrived
	// in the meantime (the settings push also updates it).
	rt.writeMu.Lock()
	defer rt.writeMu.Unlock()
	valueJSON, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		revision, _ := rt.Settings()
		request := map[string]any{
			"appId":            rt.env.AppID,
			"expectedRevision": formatUint(revision),
			"changes":          []any{map[string]any{"key": key, "valueJson": string(valueJSON)}},
		}
		var response map[string]any
		err = rt.client.CallJSON(rt.ctx, "", configService, "Update", request, &response, 10*time.Second)
		if err == nil {
			if raw, ok := response["revision"]; ok {
				if updated, ok := parseUintJSON(raw); ok {
					rt.mu.Lock()
					if updated > rt.revision {
						rt.revision = updated
					}
					rt.mu.Unlock()
				}
			}
			return nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("persist setting %q: %w", key, lastErr)
}

// parseUintJSON reads a uint64 that canonical protobuf JSON encodes as a
// string (the daemon also accepts a number here).
func parseUintJSON(value any) (uint64, bool) {
	switch v := value.(type) {
	case string:
		parsed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case float64:
		if v < 0 {
			return 0, false
		}
		return uint64(v), true
	}
	return 0, false
}

func formatUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// --- daemon-mediated listeners -------------------------------------------

// OpenTCPListener asks the daemon for a bound TCP listener. port 0 requests an
// ephemeral port; a requested port may be refused and must then be retried
// with 0 by the caller.
func (rt *Runtime) OpenTCPListener(address string, port int) (session.Listener, error) {
	if address == "" {
		address = "0.0.0.0"
	}
	opened, err := capsulad.OpenListener(rt.ctx, rt.env, capsulad.ListenerOptions{
		Protocol: "tcp", Bind: address, Port: port, Backlog: 64,
	})
	if err != nil {
		return nil, err
	}
	tracked := rt.track(&tcpListener{listener: opened, ctx: rt.ctx})
	return tracked, nil
}

// OpenUDPListener asks the daemon for a bound UDP socket. A non-empty group
// requests a multicast receive socket, which also needs the group join to be
// permitted by the daemon.
func (rt *Runtime) OpenUDPListener(address string, port int, group string) (session.UDPListener, error) {
	if address == "" {
		address = "0.0.0.0"
	}
	opened, err := capsulad.OpenListener(rt.ctx, rt.env, capsulad.ListenerOptions{
		Protocol: "udp", Bind: address, Port: port, JoinGroup: group,
	})
	if err != nil {
		return nil, err
	}
	tracked := rt.track(&udpListener{listener: opened, ctx: rt.ctx, group: group})
	return tracked, nil
}

// closer is either listener kind: both release their descriptor on Close.
type closer interface {
	Close() error
}

// track registers a listener so Release can close it at shutdown, while the
// wrapper removes itself as soon as the owner closes it. A settings change
// restarts the receiver and would otherwise grow this list forever.
func (rt *Runtime) track(inner closer) *trackedListener {
	rt.mu.Lock()
	rt.nextListen++
	key := rt.nextListen
	rt.listeners[key] = inner
	rt.mu.Unlock()
	return &trackedListener{rt: rt, key: key, inner: inner}
}

func (rt *Runtime) forget(key uint64) {
	rt.mu.Lock()
	delete(rt.listeners, key)
	rt.mu.Unlock()
}

// trackedListener is a session.Listener whose Close releases its registry slot.
type trackedListener struct {
	rt    *Runtime
	key   uint64
	inner closer
}

func (l *trackedListener) Accept() (session.Stream, error) {
	acceptor, ok := l.inner.(session.Listener)
	if !ok {
		return nil, errors.New("this listener does not accept connections")
	}
	return acceptor.Accept()
}

func (l *trackedListener) Addr() (string, int) {
	addresser, ok := l.inner.(session.Listener)
	if !ok {
		return "", 0
	}
	return addresser.Addr()
}

func (l *trackedListener) Receive() ([]byte, session.UDPAddr, error) {
	receiver, ok := l.inner.(session.UDPListener)
	if !ok {
		return nil, session.UDPAddr{}, errors.New("this listener does not receive datagrams")
	}
	return receiver.Receive()
}

func (l *trackedListener) Send(peer session.UDPAddr, data []byte) error {
	sender, ok := l.inner.(session.UDPListener)
	if !ok {
		return errors.New("this listener does not send datagrams")
	}
	return sender.Send(peer, data)
}

func (l *trackedListener) Group() string {
	grouper, ok := l.inner.(session.UDPListener)
	if !ok {
		return ""
	}
	return grouper.Group()
}

func (l *trackedListener) Close() error {
	l.rt.forget(l.key)
	return l.inner.Close()
}

type tcpListener struct {
	listener *capsulad.Listener
	ctx      context.Context
}

func (l *tcpListener) Accept() (session.Stream, error) {
	stream, peer, err := l.listener.Accept(l.ctx)
	if err != nil {
		return nil, err
	}
	return &tcpStream{stream: stream, peer: session.Peer{Address: peer.Address, Port: peer.Port}}, nil
}

func (l *tcpListener) Addr() (string, int) {
	info, err := l.listener.Info()
	if err != nil {
		return "", 0
	}
	return info.Address, info.Port
}

func (l *tcpListener) Close() error { return l.listener.Close() }

type udpListener struct {
	listener *capsulad.Listener
	ctx      context.Context
	group    string
}

func (l *udpListener) Receive() ([]byte, session.UDPAddr, error) {
	data, peer, err := l.listener.Receive(l.ctx, 0)
	if err != nil {
		return nil, session.UDPAddr{}, err
	}
	return data, session.UDPAddr{Address: peer.Address, Port: peer.Port}, nil
}

func (l *udpListener) Send(peer session.UDPAddr, data []byte) error {
	_, err := l.listener.Send(l.ctx, capsulad.UDPAddr{Address: peer.Address, Port: peer.Port}, data)
	return err
}

func (l *udpListener) Addr() (string, int) {
	info, err := l.listener.Info()
	if err != nil {
		return "", 0
	}
	return info.Address, info.Port
}

func (l *udpListener) Group() string { return l.group }

func (l *udpListener) Close() error { return l.listener.Close() }

type tcpStream struct {
	stream *capsulad.TCPStream
	peer   session.Peer
}

func (s *tcpStream) Read(p []byte) (int, error) { return s.stream.Read(context.Background(), p) }

func (s *tcpStream) Write(p []byte) (int, error) { return s.stream.Write(context.Background(), p) }

func (s *tcpStream) Close() error { return s.stream.Close() }

func (s *tcpStream) Peer() session.Peer { return s.peer }

// PTPClock returns the sender's PTP clock, joining the multicast group on first
// use. A nil clock means the runtime refused the multicast listener; the
// receiver then advertises no PTP capability and paces from arrival time.
func (rt *Runtime) PTPClock() session.Clock {
	rt.ptpOnce.Do(func() {
		receiver, err := ptprecv.Start(rt, rt.logf)
		if err != nil {
			rt.logf("PTP unavailable: %v; playback will pace from packet arrival", err)
			rt.ptp = nil
			return
		}
		rt.ptp = receiver
	})
	if rt.ptp == nil {
		return nil
	}
	return rt.ptp
}

// --- presentations and playback ------------------------------------------

// Ring opens the ring presentation. Ownership is by presentation: the frame
// written here is the device output until the presentation closes.
type Ring struct {
	presentation *capsulad.Presentation
}

// OpenRing acquires the LED ring presentation.
func (rt *Runtime) OpenRing() (session.Ring, error) {
	presentation, err := rt.client.OpenPresentation(rt.ctx, "light.ring.main")
	if err != nil {
		return nil, err
	}
	return &Ring{presentation: presentation}, nil
}

// Pixels writes one 12-pixel frame.
func (r *Ring) Pixels(pixels []byte) error {
	if len(pixels) < 36 {
		return fmt.Errorf("ring frame needs 36 bytes, got %d", len(pixels))
	}
	frame := make([]capsulad.RGB, 12)
	for i := range 12 {
		frame[i] = capsulad.RGB{R: pixels[i*3], G: pixels[i*3+1], B: pixels[i*3+2]}
	}
	return r.presentation.Ring(frame)
}

// Close releases the ring presentation.
func (r *Ring) Close() error { return r.presentation.Close() }

// Sink is a framework playback stream on audio.output.speaker.
type Sink struct {
	playback *capsulad.Playback
}

// OpenSink opens playback in the given raw PCM format.
func (rt *Runtime) OpenSink(rate, channels int) (session.Sink, error) {
	playback, err := rt.client.OpenPlayback(rt.ctx, "audio.output.speaker", capsulad.AudioFormat{
		SampleFormat: capsulad.SampleFormatPCM16LE,
		SampleRateHz: uint32(rate),
		Channels:     uint32(channels),
	}, "")
	if err != nil {
		return nil, err
	}
	return &Sink{playback: playback}, nil
}

func (s *Sink) Write(pcm []byte) error { return s.playback.Write(pcm) }
func (s *Sink) Flush() error           { return s.playback.Control(capsulad.PlaybackFlush) }
func (s *Sink) Close() error           { return s.playback.Close() }
func (s *Sink) Abort() error           { return s.playback.Abort() }

// --- mDNS ------------------------------------------------------------------

// MasterVolume reads the framework mixer's master level (0..1). A supervised
// app may read it but never write it: the level a person set on the device is
// a ceiling, and the receiver only reports it to the sender.
func (rt *Runtime) MasterVolume() (float64, bool) {
	state, err := rt.client.AudioState(rt.ctx)
	if err != nil {
		return 0, false
	}
	return float64(state.MasterVolume), true
}

// Publish advertises one service through the daemon's responder and returns
// its publication id.
func (rt *Runtime) Publish(service session.Advert) (string, error) {
	publication, err := rt.client.PublishMDNS(rt.ctx, capsulad.MDNSService{
		ServiceType:  service.Type,
		InstanceName: service.Instance,
		Port:         uint32(service.Port),
		TXT:          service.TXT,
	}, 10*time.Second)
	if err != nil {
		return "", err
	}
	rt.mu.Lock()
	rt.mdnsIDs = append(rt.mdnsIDs, publication.PublicationID)
	rt.mu.Unlock()
	return publication.PublicationID, nil
}

// Unpublish removes one advertisement.
func (rt *Runtime) Unpublish(id string) error {
	err := rt.client.UnpublishMDNS(rt.ctx, id, 10*time.Second)
	rt.mu.Lock()
	remaining := rt.mdnsIDs[:0]
	for _, existing := range rt.mdnsIDs {
		if existing != id {
			remaining = append(remaining, existing)
		}
	}
	rt.mdnsIDs = remaining
	rt.mu.Unlock()
	return err
}

// Release closes every listener and advertisement this runtime owns. Run exit
// also removes publications, so this is the orderly path, not the only one.
func (rt *Runtime) Release() {
	rt.mu.Lock()
	listeners := make([]closer, 0, len(rt.listeners))
	for _, l := range rt.listeners {
		listeners = append(listeners, l)
	}
	rt.listeners = map[uint64]closer{}
	ids := rt.mdnsIDs
	rt.mdnsIDs = nil
	rt.mu.Unlock()
	for _, l := range listeners {
		if err := l.Close(); err != nil && !errors.Is(err, capsulad.ErrClosed) {
			rt.logf("closing listener: %v", err)
		}
	}
	for _, id := range ids {
		if err := rt.Unpublish(id); err != nil {
			rt.logf("unpublishing %s: %v", id, err)
		}
	}
	if rt.ptp != nil {
		if err := rt.ptp.Close(); err != nil {
			rt.logf("closing the PTP listeners: %v", err)
		}
		rt.ptp = nil
	}
}

// --- media contract -------------------------------------------------------

// ServeMediaContract registers the media contract handlers. Registration is
// per (service, method) on the app's single bus connection; the daemon routes
// routed calls here without any listening socket.
func (rt *Runtime) ServeMediaContract(
	list func() map[string]any,
	watch func(ctx context.Context, send func(map[string]any) error) error,
	status func() (map[string]any, bool),
	command func(action string) (bool, string),
) {
	rt.client.Handle(mediaService, "ListPlayers", func(ctx context.Context, call *capsulad.ServerCall) {
		if err := call.SendJSON(list()); err != nil {
			_ = call.End(13, err.Error())
			return
		}
		_ = call.End(0, "")
	})
	rt.client.Handle(mediaService, "WatchPlayers", func(ctx context.Context, call *capsulad.ServerCall) {
		_ = watch(call.Context(), func(event map[string]any) error { return call.SendJSON(event) })
		_ = call.End(0, "")
	})
	rt.client.Handle(mediaService, "GetStatus", func(ctx context.Context, call *capsulad.ServerCall) {
		player, ok := status()
		if !ok {
			_ = call.End(5, "no such player")
			return
		}
		if err := call.SendJSON(player); err != nil {
			_ = call.End(13, err.Error())
			return
		}
		_ = call.End(0, "")
	})
	for _, entry := range []struct {
		method string
		action string
	}{
		{"Play", "play"},
		{"Pause", "pause"},
		{"SkipNext", "next"},
		{"SkipPrevious", "previous"},
	} {
		method, action := entry.method, entry.action
		rt.client.Handle(mediaService, method, func(ctx context.Context, call *capsulad.ServerCall) {
			var request struct {
				PlayerID string `json:"playerId"`
			}
			if err := call.RecvJSON(&request); err != nil {
				_ = call.End(3, err.Error())
				return
			}
			if request.PlayerID != "" && request.PlayerID != "airplay" {
				_ = call.End(5, "unknown player")
				return
			}
			accepted, detail := command(action)
			if err := call.SendJSON(map[string]any{"accepted": accepted, "detail": detail}); err != nil {
				_ = call.End(13, err.Error())
				return
			}
			_ = call.End(0, "")
		})
	}
}
