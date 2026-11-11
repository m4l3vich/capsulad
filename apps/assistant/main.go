// Assistant is a minimal always-running voice assistant for framework tests
// and demos. It receives framework wakeword/touch activations, opens a
// conversation lifetime (which ducks other playback in the mixer), applies a
// visible ring indication, and opens the microphone.
//
// It deliberately does not perform speech recognition; it demonstrates the
// supervisor, activation, capture, watchdog-visible indication and explicit
// release contracts.
package main

import (
	"context"
	"log"
	"os/signal"
	"sync"
	"syscall"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/sdk/go/capsula"
)

const (
	ringResource = "light.ring.main"
	listenFor    = 4 * time.Second
)

type assistant struct {
	client *capsula.Client
	ctx    context.Context

	mu      sync.Mutex
	stop    chan struct{}
	active  bool
	pending string
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsula.Connect(ctx, env.Socket, env.RunID)
	if err != nil {
		log.Fatalf("assistant: connect: %v", err)
	}
	defer client.Close()
	a := &assistant{client: client, ctx: ctx}

	client.SetHandlers(capsula.Handlers{
		Ready: func() { log.Printf("assistant: ready and idle") },
		Activation: func(_ context.Context, act *frameworkv1.AssistantActivation) {
			log.Printf("assistant: activation=%s origin=%s model=%s", act.ActivationId, act.Origin, act.MatchedModelId)
			a.start(act.ActivationId)
		},
		TouchRelease: func(rel *frameworkv1.AssistantInputReleased) {
			log.Printf("assistant: input released activation=%s", rel.ActivationId)
			a.halt()
		},
		Settings: func(s *frameworkv1.AppSettingsChanged) {
			log.Printf("assistant: settings revision %d", s.Revision)
		},
		Invocation: func(ctx context.Context, inv *frameworkv1.AppLaunchInvocation) {
			log.Printf("assistant: invocation %s", inv.InvocationId)
			_ = client.NotifyInvocationResult(inv.InvocationId, true, "assistant already running")
		},
	})
	log.Printf("assistant: starting bus")
	if err := client.Start(ctx); err != nil {
		log.Fatalf("assistant: bus: %v", err)
	}
}

func (a *assistant) start(activationID string) {
	a.mu.Lock()
	if a.active {
		a.mu.Unlock()
		log.Printf("assistant: activation ignored; already listening")
		return
	}
	a.active = true
	stop := make(chan struct{})
	a.stop = stop
	a.mu.Unlock()

	go a.converse(activationID, stop)
}

func (a *assistant) halt() {
	a.mu.Lock()
	if a.stop != nil {
		close(a.stop)
		a.stop = nil
	}
	a.mu.Unlock()
}

func (a *assistant) converse(activationID string, stop chan struct{}) {
	defer func() {
		a.mu.Lock()
		a.active = false
		a.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	conv, err := a.client.BeginConversation(ctx, activationID)
	if err != nil {
		log.Printf("assistant: conversation: %v", err)
		return
	}
	defer conv.Close()
	log.Printf("assistant: conversation=%s begun", conv.ID)

	ring, err := a.client.OpenPresentation(ctx, ringResource)
	var capture *capsula.Capture
	if err == nil {
		if err := ring.Effect("system.ring.spinner", map[string]any{
			"speed": 1.4, "color": capsula.RGB{R: 0, G: 136, B: 255}, "intensity": 1.0, "tail": 4,
		}); err != nil {
			log.Printf("assistant: ring effect: %v", err)
		}
		capture, err = a.client.OpenCapture(ctx, ringResource, ring.ID(), frameworkv1.TouchCaptureMode_TOUCH_CAPTURE_MODE_UNSPECIFIED)
		if err != nil {
			log.Printf("assistant: ring capture: %v", err)
		}
	} else {
		log.Printf("assistant: ring unavailable: %v", err)
	}
	if capture != nil {
		defer capture.Close()
	}
	if ring != nil {
		defer ring.Close()
	}

	mic, err := a.client.OpenMic(ctx, activationID, conv.ID)
	if err != nil {
		log.Printf("assistant: microphone: %v", err)
		return
	}
	defer func() {
		cancel()
		<-mic.Done()
	}()
	log.Printf("assistant: listening (visible indication active, watchdog should be satisfied)")

	ctxListen, cancelListen := context.WithTimeout(ctx, listenFor)
	defer cancelListen()
	for {
		select {
		case <-ctxListen.Done():
			log.Printf("assistant: listening finished")
			return
		case chunk, ok := <-mic.Chunks():
			if !ok {
				return
			}
			if chunk.Notice != nil {
				log.Printf("assistant: audio notice %s: %s", chunk.Notice.Code, chunk.Notice.Detail)
			}
		}
	}
}
