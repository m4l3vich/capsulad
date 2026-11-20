// Command radio is the Capsula Mini Internet Radio app.
//
// It is one native supervised app: it owns the station settings, opens the
// selected HTTP(S) MP3 stream through the shared capsulad transport, decodes it
// in-process, writes PCM to the framework speaker, presents the station on the
// segment display and serves capsulad.contracts.media.v1.MediaPlayerService.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"capsulad/apps/radio/internal/radio"
	"capsulad/sdk/capsulad"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("radio: ")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	env := capsulad.FromEnv()
	client, err := capsulad.Connect(ctx, env)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}

	app := radio.New(ctx, client, env, log.Printf)
	ready := make(chan struct{}, 1)
	client.SetHandlers(capsulad.Handlers{
		Ready: func() {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		Settings: app.HandleSettings,
		Action: func(_ context.Context, invoked *capsulad.ActionInvoked) {
			app.HandleAction(invoked)
		},
		Invocation: func(callCtx context.Context, invocation *capsulad.LaunchInvocation) {
			app.HandleInvocation(callCtx, invocation)
		},
		Disconnected: func(err error) {
			log.Printf("app bus disconnected: %v", err)
		},
	})
	app.ServeMediaContract(ctx)

	startErr := make(chan error, 1)
	go func() { startErr <- client.Start(ctx) }()

	select {
	case <-ctx.Done():
		app.Close()
		_ = client.Close()
		return
	case <-app.Done():
		app.Close()
		_ = client.Close()
		return
	case err := <-startErr:
		if err != nil {
			log.Printf("app bus ended: %v", err)
		}
		app.Close()
		_ = client.Close()
		return
	case <-ready:
	}

	log.Printf("ready app=%s run=%s sdk=%s", client.AppID(), client.RunID(), env.SDK)
	go app.ProbeCapabilities(ctx)

	select {
	case <-ctx.Done():
	case err := <-startErr:
		if err != nil {
			log.Printf("app bus ended: %v", err)
		}
	case <-client.Done():
		if err := client.Err(); err != nil {
			log.Printf("app bus ended: %v", err)
		}
	case <-app.Done():
		// The radio exits itself when it has no audio and no live indication:
		// a bare launch that never played, a pause held for its minute, or a
		// terminal error whose display window ended. Close releases the
		// display and audio before the process leaves.
		log.Printf("idle run ended; exiting")
	}

	app.Close()
	_ = client.Close()
}
