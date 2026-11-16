// Media provider fixture for capsulad's routed-contract end-to-end test.
//
// It is not an installable sample application. The test installs this binary
// under a synthetic app ID, publishes the standard MediaPlayerService
// descriptor, and exercises unary discovery plus server-streaming state
// through the app bus. A connected Bluetooth source is mirrored as a second
// player through the framework's app-facing AVRCP service.
package main

import (
	"context"
	"log"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	"capsulad/sdk/capsulad"
	"google.golang.org/protobuf/proto"
)

const (
	service       = "capsulad.contracts.media.v1.MediaPlayerService"
	localPlayerID = "local.player"
	btPlayerID    = "bluetooth"
)

type provider struct {
	client *capsulad.Client
	ctx    context.Context

	mu       sync.Mutex
	playing  bool
	started  bool
	revision uint64
	changed  chan struct{}
	bt       *capsulad.BluetoothMediaState

	tracks []string
	track  int
}

func newProvider(ctx context.Context, client *capsulad.Client) *provider {
	return &provider{
		ctx: ctx, client: client, changed: make(chan struct{}),
		tracks: []string{"Aurora", "Borealis", "Cirrus"},
	}
}

func (p *provider) localStatusLocked() *mediav1.PlayerStatus {
	state := mediav1.PlaybackState_PLAYBACK_STOPPED
	if p.started {
		state = mediav1.PlaybackState_PLAYBACK_PAUSED
	}
	if p.playing {
		state = mediav1.PlaybackState_PLAYBACK_PLAYING
	}
	return &mediav1.PlayerStatus{
		PlayerId:      localPlayerID,
		PlaybackState: state,
		SupportedActions: []mediav1.MediaAction{
			mediav1.MediaAction_MEDIA_PLAY,
			mediav1.MediaAction_MEDIA_PAUSE,
			mediav1.MediaAction_MEDIA_SKIP_NEXT,
			mediav1.MediaAction_MEDIA_SKIP_PREVIOUS,
		},
		AppName:    "Media",
		SourceName: "Fixture playlist",
		MediaName:  proto.String(p.localTitleLocked() + " — Capsula"),
		Title:      proto.String(p.localTitleLocked()),
		Artist:     proto.String("Capsula"),
		Album:      proto.String("Capsula samples"),
		Revision:   p.revision,
	}
}

func (p *provider) bluetoothStatusLocked() *mediav1.PlayerStatus {
	if p.bt == nil || !p.bt.Present {
		return nil
	}
	state := mediav1.PlaybackState_PLAYBACK_PAUSED
	if p.bt.Playing {
		state = mediav1.PlaybackState_PLAYBACK_PLAYING
	}
	st := &mediav1.PlayerStatus{
		PlayerId:      btPlayerID,
		PlaybackState: state,
		SupportedActions: []mediav1.MediaAction{
			mediav1.MediaAction_MEDIA_PLAY,
			mediav1.MediaAction_MEDIA_PAUSE,
			mediav1.MediaAction_MEDIA_SKIP_NEXT,
			mediav1.MediaAction_MEDIA_SKIP_PREVIOUS,
		},
		AppName:    "Bluetooth",
		SourceName: p.bt.DeviceName,
		DurationMs: proto.Uint64(p.bt.DurationMS),
		PositionMs: proto.Uint64(p.bt.PositionMS),
		Revision:   p.bt.Revision,
	}
	name := strings.TrimSpace(p.bt.Title)
	if name != "" && p.bt.Artist != "" {
		name += " — " + p.bt.Artist
	}
	if name == "" {
		name = p.bt.DeviceName
	}
	if name != "" {
		st.MediaName = proto.String(name)
	}
	if p.bt.Title != "" {
		st.Title = proto.String(p.bt.Title)
	}
	if p.bt.Artist != "" {
		st.Artist = proto.String(p.bt.Artist)
	}
	if p.bt.Album != "" {
		st.Album = proto.String(p.bt.Album)
	}
	return st
}

func (p *provider) statuses() []*mediav1.PlayerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []*mediav1.PlayerStatus{p.localStatusLocked()}
	if bt := p.bluetoothStatusLocked(); bt != nil {
		out = append(out, bt)
	}
	return out
}

func (p *provider) status(playerID string) *mediav1.PlayerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if playerID == btPlayerID {
		if st := p.bluetoothStatusLocked(); st != nil {
			return st
		}
		return &mediav1.PlayerStatus{PlayerId: btPlayerID}
	}
	return p.localStatusLocked()
}

func (p *provider) notifyLocked() {
	p.revision++
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *provider) watchChan() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

func (p *provider) play() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.playing {
		return
	}
	p.playing = true
	p.started = true
	p.notifyLocked()
}

func (p *provider) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.playing {
		return
	}
	p.playing = false
	p.notifyLocked()
}

func (p *provider) localTitleLocked() string {
	if len(p.tracks) == 0 {
		return "Aurora"
	}
	return p.tracks[p.track%len(p.tracks)]
}

// skip moves the fixture playlist so next/previous have an observable public
// Track/Title outcome, not just an accepted command.
func (p *provider) skip(delta int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tracks) == 0 {
		return
	}
	p.track = (p.track + delta + len(p.tracks)) % len(p.tracks)
	p.notifyLocked()
}

func (p *provider) startBluetooth(ctx context.Context) {
	stream, err := p.client.WatchBluetoothMedia(ctx)
	if err != nil {
		log.Printf("mediafixture: bluetooth media unavailable: %v", err)
		return
	}
	go func() {
		for state := range stream.C {
			next := state
			p.mu.Lock()
			p.bt = &next
			p.notifyLocked()
			p.mu.Unlock()
		}
	}()
}

func (p *provider) bluetoothCommand(ctx context.Context, cmd capsulad.BluetoothMediaCommand) *mediav1.PlayerCommandResult {
	p.mu.Lock()
	present := p.bt != nil && p.bt.Present
	p.mu.Unlock()
	if !present {
		return &mediav1.PlayerCommandResult{Accepted: false, Detail: "no Bluetooth source is connected"}
	}
	if _, err := p.client.BluetoothMediaCommand(ctx, cmd); err != nil {
		return &mediav1.PlayerCommandResult{Accepted: false, Detail: err.Error()}
	}
	return &mediav1.PlayerCommandResult{Accepted: true, Detail: "bluetooth"}
}

// playerRequest reads the optional unary request (player_id) from a routed
// call. An absent request defaults to the local player.
func playerRequest(call *capsulad.ServerCall) *mediav1.PlayerRequest {
	data, err := call.Recv()
	if err != nil {
		return &mediav1.PlayerRequest{}
	}
	var req mediav1.PlayerRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return &mediav1.PlayerRequest{}
	}
	return &req
}

func marshal(msg proto.Message) []byte {
	data, err := proto.Marshal(msg)
	if err != nil {
		log.Printf("mediafixture: marshal: %v", err)
		return nil
	}
	return data
}

func (p *provider) register(client *capsulad.Client) {
	client.Handle(service, "ListPlayers", func(ctx context.Context, call *capsulad.ServerCall) {
		_ = call.Send(marshal(&mediav1.ListPlayersResponse{Players: p.statuses()}))
	})
	client.Handle(service, "GetStatus", func(ctx context.Context, call *capsulad.ServerCall) {
		req := playerRequest(call)
		_ = call.Send(marshal(p.status(req.GetPlayerId())))
	})
	client.Handle(service, "WatchPlayers", p.watch)
	client.Handle(service, "Play", func(ctx context.Context, call *capsulad.ServerCall) {
		req := playerRequest(call)
		if req.GetPlayerId() == btPlayerID {
			_ = call.Send(marshal(p.bluetoothCommand(ctx, capsulad.BluetoothMediaPlay)))
			return
		}
		p.play()
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "playing"}))
	})
	client.Handle(service, "Pause", func(ctx context.Context, call *capsulad.ServerCall) {
		req := playerRequest(call)
		if req.GetPlayerId() == btPlayerID {
			_ = call.Send(marshal(p.bluetoothCommand(ctx, capsulad.BluetoothMediaPause)))
			return
		}
		p.pause()
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "paused"}))
	})
	client.Handle(service, "SkipNext", func(ctx context.Context, call *capsulad.ServerCall) {
		req := playerRequest(call)
		if req.GetPlayerId() == btPlayerID {
			_ = call.Send(marshal(p.bluetoothCommand(ctx, capsulad.BluetoothMediaNext)))
			return
		}
		p.skip(1)
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "next"}))
	})
	client.Handle(service, "SkipPrevious", func(ctx context.Context, call *capsulad.ServerCall) {
		req := playerRequest(call)
		if req.GetPlayerId() == btPlayerID {
			_ = call.Send(marshal(p.bluetoothCommand(ctx, capsulad.BluetoothMediaPrevious)))
			return
		}
		p.skip(-1)
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "previous"}))
	})
}

// watch emits the required snapshot first, then updates. A Bluetooth player
// that disappears is reported as PlayerRemoved.
func (p *provider) watch(ctx context.Context, call *capsulad.ServerCall) {
	first := true
	lastBluetooth := false
	for {
		changed := p.watchChan()
		players := p.statuses()
		bluetooth := false
		for _, player := range players {
			if player.GetPlayerId() == btPlayerID {
				bluetooth = true
			}
		}
		if first {
			event := &mediav1.PlayerEvent{Event: &mediav1.PlayerEvent_Snapshot{
				Snapshot: &mediav1.ListPlayersResponse{Players: players},
			}}
			if err := call.Send(marshal(event)); err != nil {
				return
			}
			first = false
		} else {
			if lastBluetooth && !bluetooth {
				removed := &mediav1.PlayerEvent{Event: &mediav1.PlayerEvent_Removed{
					Removed: &mediav1.PlayerRemoved{PlayerId: btPlayerID},
				}}
				if err := call.Send(marshal(removed)); err != nil {
					return
				}
			}
			for _, player := range players {
				event := &mediav1.PlayerEvent{Event: &mediav1.PlayerEvent_Updated{Updated: player}}
				if err := call.Send(marshal(event)); err != nil {
					return
				}
			}
		}
		lastBluetooth = bluetooth
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsulad.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsulad.Connect(ctx, env)
	if err != nil {
		log.Fatalf("mediafixture: connect: %v", err)
	}
	defer client.Close()

	p := newProvider(ctx, client)
	p.register(client)
	ready := make(chan struct{}, 1)
	client.SetHandlers(capsulad.Handlers{
		Ready: func() { ready <- struct{}{} },
		Invocation: func(ctx context.Context, inv *capsulad.LaunchInvocation) {
			_ = client.NotifyInvocationResult(inv.ID, true, "media fixture is running")
		},
	})
	go func() {
		if err := client.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("mediafixture: bus stopped: %v", err)
			cancel()
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}
	p.startBluetooth(ctx)
	log.Printf("mediafixture: provider ready")
	<-ctx.Done()
}
