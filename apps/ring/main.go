// Ring controller is an example app that owns the ring baseline and exposes
// its own custom interface. It demonstrates the reverse-RPC bus: the daemon
// relays RingControllerService calls over this app's Connect stream; the app
// never listens on an IPC socket.
package main

import (
	"context"
	"io"
	"log"
	"os/signal"
	"sync"
	"syscall"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	ringv1 "capsulad/gen/go/org/example/ring/v1"
	"capsulad/sdk/go/capsula"
	"google.golang.org/protobuf/proto"
)

const (
	service      = "org.example.ring.v1.RingControllerService"
	ringResource = "light.ring.main"
)

type controller struct {
	mu     sync.Mutex
	mode   string
	active bool
	pres   *capsula.Presentation
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsula.Connect(ctx, env.Socket, env.RunID)
	if err != nil {
		log.Fatalf("ring: connect: %v", err)
	}
	defer client.Close()

	c := &controller{mode: "ambient"}
	ready := make(chan struct{}, 1)

	client.SetHandlers(capsula.Handlers{
		Ready: func() { ready <- struct{}{} },
		Settings: func(s *frameworkv1.AppSettingsChanged) {
			log.Printf("ring: settings revision %d", s.Revision)
		},
		Action: func(ctx context.Context, a *frameworkv1.AppActionInvoked) {
			log.Printf("ring: action %s", a.ActionId)
			c.apply()
			_ = client.NotifyActionResult(a.ActionInvocationId, true, "effects reapplied")
		},
		Invocation: func(ctx context.Context, inv *frameworkv1.AppLaunchInvocation) {
			_ = client.NotifyInvocationResult(inv.InvocationId, true, "ring controller already running")
		},
	})

	client.Handle(service, "ListModes", unary(func() proto.Message { return &ringv1.ListModesRequest{} }, func(req proto.Message) proto.Message {
		return &ringv1.ListModesResponse{Modes: []*ringv1.RingMode{
			{Id: "ambient", Name: "Ambient", Description: "Slow breathing green light"},
			{Id: "spinner", Name: "Spinner", Description: "Rotating blue head"},
			{Id: "rainbow", Name: "Rainbow", Description: "Hue rotation around the ring"},
		}}
	}))
	client.Handle(service, "GetState", unary(func() proto.Message { return &ringv1.GetStateRequest{} }, func(req proto.Message) proto.Message {
		c.mu.Lock()
		defer c.mu.Unlock()
		return &ringv1.RingState{CurrentModeId: c.mode, PresentationActive: c.active}
	}))
	client.Handle(service, "SetMode", unary(func() proto.Message { return &ringv1.SetModeRequest{} }, func(req proto.Message) proto.Message {
		in := req.(*ringv1.SetModeRequest)
		switch in.ModeId {
		case "ambient", "spinner", "rainbow":
		default:
			return &ringv1.RingState{}
		}
		c.mu.Lock()
		c.mode = in.ModeId
		c.mu.Unlock()
		c.apply()
		c.mu.Lock()
		defer c.mu.Unlock()
		return &ringv1.RingState{CurrentModeId: c.mode, PresentationActive: c.active}
	}))
	go func() {
		if err := client.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("ring: bus stopped: %v", err)
			cancel()
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}
	pres, err := client.OpenPresentation(ctx, ringResource)
	if err != nil {
		log.Printf("ring: unavailable: %v", err)
		<-ctx.Done()
		return
	}
	defer pres.Close()
	c.mu.Lock()
	c.pres = pres
	c.active = true
	c.mu.Unlock()
	c.apply()
	log.Printf("ring: presentation %s mode=%s", pres.ID(), c.mode)
	<-ctx.Done()
	c.mu.Lock()
	c.active = false
	c.mu.Unlock()
}

func (c *controller) apply() {
	c.mu.Lock()
	pres := c.pres
	mode := c.mode
	c.mu.Unlock()
	if pres == nil {
		return
	}
	var err error
	switch mode {
	case "spinner":
		err = pres.Effect("system.ring.spinner", map[string]any{
			"speed": 0.8, "color": capsula.RGB{R: 0, G: 136, B: 255}, "intensity": 1.0, "tail": 5,
		})
	case "rainbow":
		err = pres.Effect("system.ring.rainbow", map[string]any{"speed": 0.3, "saturation": 1.0})
	default:
		err = pres.Effect("system.ring.breathe", map[string]any{
			"period_s": 4.0, "color": capsula.RGB{R: 0, G: 200, B: 120},
		})
	}
	if err != nil {
		log.Printf("ring: apply %s: %v", mode, err)
	}
}

// unary adapts a one-request/one-response protobuf method to a CallHandler.
func unary(newRequest func() proto.Message, handle func(proto.Message) proto.Message) capsula.CallHandler {
	return func(ctx context.Context, call *capsula.ServerCall) {
		req := newRequest()
		for {
			data, err := call.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}
			if err := proto.Unmarshal(data, req); err != nil {
				_ = call.End(3, err.Error())
				return
			}
		}
		resp := handle(req)
		raw, err := proto.Marshal(resp)
		if err != nil {
			_ = call.End(13, err.Error())
			return
		}
		if err := call.Send(raw); err != nil {
			return
		}
		_ = call.End(0, "")
	}
}
