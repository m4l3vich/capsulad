// Test probe app used by capsulad's end-to-end tests. It selects behavior from
// its launcher-assigned app ID and invocation arguments.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/sdk/go/capsula"
)

func main() {
	log.SetFlags(0)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	client, err := capsula.Connect(ctx, env.Socket, env.RunID)
	if err != nil {
		log.Fatalf("probe: connect: %v", err)
	}
	defer client.Close()

	var touchCount atomic.Int64
	serve := env.AppID == "test.probe"
	if serve {
		client.Handle("test.probe.v1.ProbeService", "TouchCount", func(ctx context.Context, call *capsula.ServerCall) {
			for {
				_, err := call.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return
				}
			}
			_ = call.Send([]byte(fmt.Sprintf("%d", touchCount.Load())))
			_ = call.End(0, "")
		})
		client.Handle("test.probe.v1.ProbeService", "Echo", func(ctx context.Context, call *capsula.ServerCall) {
			for {
				data, err := call.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return
				}
				_ = call.Send(append([]byte("echo:"), data...))
			}
			_ = call.End(0, "")
		})
		client.Handle("test.probe.v1.ProbeService", "Stream", func(ctx context.Context, call *capsula.ServerCall) {
			for {
				data, err := call.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return
				}
				if err := call.Send(append([]byte("s:"), data...)); err != nil {
					return
				}
			}
			_ = call.End(0, "")
		})
	}

	ready := make(chan struct{}, 1)
	client.SetHandlers(capsula.Handlers{
		Ready: func() { ready <- struct{}{} },
		Invocation: func(ctx context.Context, inv *frameworkv1.AppLaunchInvocation) {
			mode := ""
			for _, a := range inv.Arguments {
				if a.Name == "mode" {
					mode = a.Value
				}
			}
			log.Printf("invocation mode=%s", mode)
			switch mode {
			case "crash":
				go func() {
					time.Sleep(800 * time.Millisecond)
					log.Printf("simulating crash while holding a ring capture")
					os.Exit(3)
				}()
				holdRing(ctx, client, 10*time.Second)
			case "capture":
				holdRing(ctx, client, time.Hour)
			case "mic":
				openMic(ctx, client)
			case "touchpartial":
				watchTouch(ctx, client, frameworkv1.TouchCaptureMode_TOUCH_PARTIAL_GESTURES, &touchCount)
			case "touchfull":
				watchTouch(ctx, client, frameworkv1.TouchCaptureMode_TOUCH_FULL_RAW, &touchCount)
			}
			_ = client.NotifyInvocationResult(inv.InvocationId, true, "probe "+mode)
		},
		Activation: func(ctx context.Context, act *frameworkv1.AssistantActivation) {
			log.Printf("activation %s", act.ActivationId)
			assistantTurn(ctx, client, act.ActivationId)
		},
		TouchRelease: func(rel *frameworkv1.AssistantInputReleased) {},
	})

	if env.AppID == "test.clock" {
		go func() {
			<-ready
			p, err := client.OpenPresentation(ctx, "display.segment.front")
			if err != nil {
				log.Printf("clock presentation: %v", err)
				return
			}
			t := time.NewTicker(200 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := p.Segment([]uint16{1, 2, 3, 4}, true); err != nil {
						return
					}
				}
			}
		}()
	}
	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("probe: bus: %v", err)
	}
}

func watchTouch(ctx context.Context, client *capsula.Client, mode frameworkv1.TouchCaptureMode, count *atomic.Int64) {
	cap, err := client.OpenCapture(ctx, "touch.panel.main", "", mode)
	if err != nil {
		log.Printf("touch capture: %v", err)
		return
	}
	in, err := client.WatchInput(ctx, cap.ID())
	if err != nil {
		log.Printf("watch input: %v", err)
		return
	}
	log.Printf("touch capture %s mode=%s", cap.ID(), mode)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-in.Events():
			if !ok {
				return
			}
			count.Add(1)
			_ = ev
		}
	}
}

func holdRing(ctx context.Context, client *capsula.Client, d time.Duration) {
	p, err := client.OpenPresentation(ctx, "light.ring.main")
	if err != nil {
		log.Printf("ring: %v", err)
		return
	}
	if err := p.Effect("system.ring.spinner", map[string]any{"speed": 1.0}); err != nil {
		log.Printf("effect: %v", err)
	}
	cap, err := client.OpenCapture(ctx, "light.ring.main", p.ID(), frameworkv1.TouchCaptureMode_TOUCH_CAPTURE_MODE_UNSPECIFIED)
	if err != nil {
		log.Printf("capture: %v", err)
		return
	}
	log.Printf("holding ring capture %s", cap.ID())
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
	cap.Close()
	p.Close()
}

func openMic(ctx context.Context, client *capsula.Client) {
	mic, err := client.OpenMic(ctx, "", "")
	if err != nil {
		log.Printf("mic: %v", err)
		return
	}
	log.Printf("microphone open without a visible indication")
	for chunk := range mic.Chunks() {
		if chunk.Notice != nil {
			log.Printf("notice %s: %s", chunk.Notice.Code, chunk.Notice.Detail)
		}
	}
	log.Printf("microphone stream ended")
}

func assistantTurn(ctx context.Context, client *capsula.Client, activationID string) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	conv, err := client.BeginConversation(turnCtx, activationID)
	if err != nil {
		log.Printf("conversation: %v", err)
		return
	}
	defer conv.Close()
	p, err := client.OpenPresentation(turnCtx, "light.ring.main")
	if err != nil {
		return
	}
	defer p.Close()
	_ = p.Effect("system.ring.spinner", map[string]any{"speed": 1.2})
	cap, err := client.OpenCapture(turnCtx, "light.ring.main", p.ID(), frameworkv1.TouchCaptureMode_TOUCH_CAPTURE_MODE_UNSPECIFIED)
	if err != nil {
		return
	}
	defer cap.Close()
	mic, err := client.OpenMic(turnCtx, activationID, conv.ID)
	if err != nil {
		return
	}
	defer func() {
		cancel()
		<-mic.Done()
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-turnCtx.Done():
			return
		case <-timer.C:
			return
		case chunk, ok := <-mic.Chunks():
			if !ok {
				return
			}
			if chunk.Notice != nil {
				log.Printf("assistant notice %s", chunk.Notice.Code)
			}
		}
	}
}
