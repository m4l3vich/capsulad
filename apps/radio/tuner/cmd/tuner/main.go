// Command tuner is the native companion for org.capsulad.radio. It opens one
// outbound MP3 stream at a time through the shared capsulad transport, strips
// ICY metadata, decodes it and pushes bounded PCM chunks to the radio player
// with one routed Push call per chunk.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"capsulad/apps/radio/tuner/internal/tuner"
	"capsulad/sdk/capsulad"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsulad.FromEnv()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	client, err := capsulad.Connect(ctx, env)
	if err != nil {
		log.Fatalf("radio-tuner: connect: %v", err)
	}
	defer client.Close()

	manager := tuner.NewManager(ctx, client, env)
	defer manager.Close()

	client.Handle(tuner.ServiceName, "Tune", manager.ServeTune)
	client.Handle(tuner.ServiceName, "Stop", manager.ServeStop)
	client.SetHandlers(capsulad.Handlers{
		Ready: func() {
			log.Printf("radio-tuner: ready app=%s run=%s sdk=%s player=%s",
				env.AppID, env.RunID, env.SDK, manager.PlayerApp())
		},
		Settings: func(event *capsulad.SettingsChanged) {
			manager.ApplySettings(event)
		},
		Disconnected: func(err error) {
			log.Printf("radio-tuner: app bus disconnected: %v", err)
		},
	})

	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		log.Printf("radio-tuner: app bus ended: %v", err)
		manager.Close()
		_ = client.Close()
		os.Exit(1)
	}
	if ctx.Err() == nil {
		// The bus ended without a stop request or a signal: that is a failure,
		// not a clean exit, and the supervisor's exit code should say so.
		log.Printf("radio-tuner: app bus ended unexpectedly")
		manager.Close()
		_ = client.Close()
		os.Exit(1)
	}
}
