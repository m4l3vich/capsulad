// Media player is a sample provider for the standard MediaPlayerService
// contract. It publishes one player, synthesizes a short local playlist through
// the framework audio mixer, and serves the contract over the reverse-RPC bus:
// the app never listens on its own socket, and the daemon never assumes media
// semantics beyond the contract.
package main

import (
	"context"
	"log"
	"math"
	"os/signal"
	"sync"
	"syscall"
	"time"

	mediav1 "capsulad/gen/go/capsula/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/sdk/go/capsula"
	"google.golang.org/protobuf/proto"
)

const (
	service    = "capsula.contracts.media.v1.MediaPlayerService"
	playerID   = "local.player"
	sampleRate = 48000
	channels   = 2
	chunkMS    = 20
)

// track is one synthesized piece: a short melody repeated on skip.
type track struct {
	id      string
	title   string
	artist  string
	noteMS  int
	notesHz []float64
}

func (t track) frames() int { return len(t.notesHz) * t.noteMS * sampleRate / 1000 }

var (
	tracks = []track{
		{
			id: "aurora", title: "Aurora", artist: "Capsula", noteMS: 320,
			notesHz: []float64{440.00, 493.88, 554.37, 659.25, 554.37, 493.88, 440.00, 392.00},
		},
		{
			id: "harbor", title: "Harbor Lights", artist: "Capsula", noteMS: 280,
			notesHz: []float64{329.63, 392.00, 440.00, 392.00, 523.25, 440.00, 392.00, 329.63},
		},
		{
			id: "drift", title: "Drift", artist: "Capsula", noteMS: 400,
			notesHz: []float64{261.63, 311.13, 349.23, 392.00, 349.23, 311.13},
		},
	}
	framesPerChunk = sampleRate * chunkMS / 1000
)

type provider struct {
	client *capsula.Client
	ctx    context.Context

	mu       sync.Mutex
	playing  bool
	started  bool
	trackIdx int
	frame    uint64
	noteIdx  int
	notePos  int
	phase    float64
	revision uint64
	changed  chan struct{}
	feedStop context.CancelFunc
	pcmBuf   []byte
}

func newProvider(ctx context.Context, client *capsula.Client) *provider {
	return &provider{ctx: ctx, client: client, changed: make(chan struct{})}
}

func (p *provider) status() *mediav1.PlayerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	tr := tracks[p.trackIdx]
	state := mediav1.PlaybackState_PLAYBACK_STOPPED
	if p.started {
		state = mediav1.PlaybackState_PLAYBACK_PAUSED
	}
	if p.playing {
		state = mediav1.PlaybackState_PLAYBACK_PLAYING
	}
	return &mediav1.PlayerStatus{
		PlayerId:      playerID,
		PlaybackState: state,
		SupportedActions: []mediav1.MediaAction{
			mediav1.MediaAction_MEDIA_PLAY,
			mediav1.MediaAction_MEDIA_PAUSE,
			mediav1.MediaAction_MEDIA_SKIP_NEXT,
			mediav1.MediaAction_MEDIA_SKIP_PREVIOUS,
		},
		AppName:    "Media",
		SourceName: "Local playlist",
		MediaName:  proto.String(tr.title + " — " + tr.artist),
		Title:      proto.String(tr.title),
		Artist:     proto.String(tr.artist),
		Album:      proto.String("Capsula samples"),
		DurationMs: proto.Uint64(uint64(tr.frames()) * 1000 / sampleRate),
		PositionMs: proto.Uint64(p.frame * 1000 / sampleRate),
		Revision:   p.revision,
	}
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
	if p.playing {
		p.mu.Unlock()
		return
	}
	p.playing = true
	p.started = true
	ctx, cancel := context.WithCancel(p.ctx)
	p.feedStop = cancel
	p.notifyLocked()
	p.mu.Unlock()
	go p.feed(ctx)
}

func (p *provider) pause() {
	p.mu.Lock()
	if !p.playing {
		p.mu.Unlock()
		return
	}
	p.playing = false
	stop := p.feedStop
	p.feedStop = nil
	p.notifyLocked()
	p.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (p *provider) skip(delta int) {
	p.mu.Lock()
	p.trackIdx = ((p.trackIdx+delta)%len(tracks) + len(tracks)) % len(tracks)
	p.frame, p.noteIdx, p.notePos, p.phase = 0, 0, 0, 0
	p.notifyLocked()
	p.mu.Unlock()
}

func (p *provider) shutdown() {
	p.mu.Lock()
	playing := p.playing
	stop := p.feedStop
	p.playing = false
	p.feedStop = nil
	p.mu.Unlock()
	if playing && stop != nil {
		stop()
	}
}

// feed streams 20 ms chunks while the player is playing. Pausing cancels the
// context, which closes the playback stream; resuming opens a new one.
func (p *provider) feed(ctx context.Context) {
	stream, err := p.client.OpenPlayback(ctx, "audio.output.speaker", nil, "")
	if err != nil {
		log.Printf("media: playback: %v", err)
		p.mu.Lock()
		p.playing = false
		p.notifyLocked()
		p.mu.Unlock()
		return
	}
	defer stream.Close()
	ticker := time.NewTicker(chunkMS * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pcm := p.nextChunk()
			if len(pcm) == 0 {
				return
			}
			if err := stream.Write(pcm); err != nil {
				log.Printf("media: write: %v", err)
				return
			}
		}
	}
}

// nextChunk renders one 20 ms stereo frame and advances the timeline. It
// returns nil once the player is no longer playing.
func (p *provider) nextChunk() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.playing {
		return nil
	}
	if p.pcmBuf == nil {
		p.pcmBuf = make([]byte, framesPerChunk*channels*2)
	}
	out := p.pcmBuf
	for i := 0; i < framesPerChunk; i++ {
		tr := tracks[p.trackIdx]
		noteFrames := tr.noteMS * sampleRate / 1000
		if p.notePos >= noteFrames {
			p.notePos = 0
			p.noteIdx = (p.noteIdx + 1) % len(tr.notesHz)
			if p.noteIdx == 0 {
				p.trackIdx = (p.trackIdx + 1) % len(tracks)
				p.frame = 0
				p.phase = 0
				tr = tracks[p.trackIdx]
				noteFrames = tr.noteMS * sampleRate / 1000
				p.notifyLocked()
			}
		}
		freq := tr.notesHz[p.noteIdx]
		p.phase += 2 * math.Pi * freq / sampleRate
		if p.phase > 2*math.Pi {
			p.phase -= 2 * math.Pi
		}
		env := 1.0
		const attack, release = sampleRate / 200, sampleRate / 50
		if p.notePos < attack {
			env = float64(p.notePos) / attack
		}
		if remaining := noteFrames - p.notePos; remaining < release {
			if r := float64(remaining) / release; r < env {
				env = r
			}
		}
		sample := int16(math.Sin(p.phase) * env * 0.18 * 32767)
		offset := i * channels * 2
		for c := 0; c < channels; c++ {
			out[offset+c*2] = byte(sample)
			out[offset+c*2+1] = byte(uint16(sample) >> 8)
		}
		p.notePos++
		p.frame++
	}
	return out
}

func (p *provider) register(client *capsula.Client) {
	client.Handle(service, "ListPlayers", func(ctx context.Context, call *capsula.ServerCall) {
		_ = call.Send(marshal(&mediav1.ListPlayersResponse{Players: []*mediav1.PlayerStatus{p.status()}}))
	})
	client.Handle(service, "GetStatus", func(ctx context.Context, call *capsula.ServerCall) {
		_ = call.Send(marshal(p.status()))
	})
	client.Handle(service, "WatchPlayers", p.watch)
	client.Handle(service, "Play", func(ctx context.Context, call *capsula.ServerCall) {
		p.play()
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "playing"}))
	})
	client.Handle(service, "Pause", func(ctx context.Context, call *capsula.ServerCall) {
		p.pause()
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "paused"}))
	})
	client.Handle(service, "SkipNext", func(ctx context.Context, call *capsula.ServerCall) {
		p.skip(1)
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "next"}))
	})
	client.Handle(service, "SkipPrevious", func(ctx context.Context, call *capsula.ServerCall) {
		p.skip(-1)
		_ = call.Send(marshal(&mediav1.PlayerCommandResult{Accepted: true, Detail: "previous"}))
	})
}

// watch emits the required snapshot first, then one update per state change.
func (p *provider) watch(ctx context.Context, call *capsula.ServerCall) {
	first := true
	for {
		changed := p.watchChan()
		event := &mediav1.PlayerEvent{Event: &mediav1.PlayerEvent_Updated{Updated: p.status()}}
		if first {
			event = &mediav1.PlayerEvent{Event: &mediav1.PlayerEvent_Snapshot{
				Snapshot: &mediav1.ListPlayersResponse{Players: []*mediav1.PlayerStatus{p.status()}},
			}}
			first = false
		}
		if err := call.Send(marshal(event)); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func marshal(msg proto.Message) []byte {
	data, err := proto.Marshal(msg)
	if err != nil {
		log.Printf("media: marshal: %v", err)
		return nil
	}
	return data
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsula.Connect(ctx, env.Socket, env.RunID)
	if err != nil {
		log.Fatalf("media: connect: %v", err)
	}
	defer client.Close()

	p := newProvider(ctx, client)
	p.register(client)
	ready := make(chan struct{}, 1)
	client.SetHandlers(capsula.Handlers{
		Ready: func() { ready <- struct{}{} },
		Invocation: func(ctx context.Context, inv *frameworkv1.AppLaunchInvocation) {
			_ = client.NotifyInvocationResult(inv.InvocationId, true, "media player is running")
		},
		Action: func(ctx context.Context, a *frameworkv1.AppActionInvoked) {
			_ = client.NotifyActionResult(a.ActionInvocationId, true, "no actions")
		},
	})
	go func() {
		if err := client.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("media: bus stopped: %v", err)
			cancel()
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}
	log.Printf("media: provider ready player=%s tracks=%d", playerID, len(tracks))
	<-ctx.Done()
	p.shutdown()
}
