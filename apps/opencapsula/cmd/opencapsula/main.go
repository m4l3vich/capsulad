// OpenCapsula is a first-party voice-assistant client for an OpenCapsule
// server. The framework owns the microphone, the speaker, the ring and the
// device settings; this app turns a framework activation into one OpenCapsule
// conversation and executes the commands the server asks for.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"capsulad/apps/opencapsula/internal/assistant"
	"capsulad/sdk/capsulad"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	env := capsulad.FromEnv()
	if env.RunID == "" {
		log.Fatalf("opencapsula: CAPSULAD_RUN_ID is missing; launch the app through the framework")
	}
	client, err := capsulad.Connect(runCtx, env)
	if err != nil {
		log.Fatalf("opencapsula: connect: %v", err)
	}
	defer client.Close()

	app := assistant.New(runCtx, env, client, log.Default())
	app.ResolveIdentity(runCtx)
	client.SetHandlers(app.Handlers())

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.Run()
	}()

	log.Printf("opencapsula: starting bus")
	err = client.Start(runCtx)
	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		log.Printf("opencapsula: assistant did not stop promptly")
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("opencapsula: bus stopped: %v", err)
		os.Exit(1)
	}
}
