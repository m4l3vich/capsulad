package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"capsulad/sdk/capsulad"

	"capsulad/apps/cadence/internal/link"
	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/redact"
	"capsulad/apps/cadence/internal/settings"
	"capsulad/apps/cadence/internal/turn"
)

// appVersion is advertised in Hello and shown in logs. It is not a protocol
// version: the link speaks cadence.v2's protocol 2.
const appVersion = "0.1.0"

// deviceService is the framework's app-readable identity service.
const deviceService = "capsulad.framework.v1.DeviceService"

func main() {
	redactor := &redact.Redactor{}
	logger := log.New(redactWriter{r: redactor}, "cadence: ", log.LstdFlags|log.Lmicroseconds)

	env := capsulad.FromEnv()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	client, err := capsulad.Connect(ctx, env)
	if err != nil {
		logger.Printf("connect: %v", err)
		os.Exit(1)
	}
	defer client.Close()

	var current atomic.Pointer[settings.Config]
	current.Store(&settings.Config{HoldTail: time.Second, FollowUp: true})

	board := identity(ctx, client, logger)

	// The link's handler needs the controller, and the controller sends on the
	// link; the holder breaks the cycle. The link is not reading envelopes
	// before Run, which starts after both are built.
	holder := &linkHandler{}
	dialer := &link.SDKDialer{Env: env, Config: func() settings.Config { return *current.Load() }}
	linkClient, err := link.New(dialer, logger, holder, "capsulad-cadence/"+appVersion, board)
	if err != nil {
		logger.Printf("link: %v", err)
		os.Exit(1)
	}
	controller := turn.New(turn.Deps{
		Link:     linkClient,
		Audio:    frameworkAudio{client: client},
		Ring:     newRingIndicator(sdkRing{client}, logger),
		Codecs:   opusCodecs{},
		Log:      logger,
		Settings: func() settings.Config { return *current.Load() },
		NewID:    capsulad.UUIDv4,
	})
	holder.ctrl = controller

	client.SetHandlers(capsulad.Handlers{
		Ready: func() { logger.Printf("ready board=%s", board) },
		Settings: func(sc *capsulad.SettingsChanged) {
			applySettings(sc, &current, redactor, linkClient, logger)
		},
		Activation: func(_ context.Context, a *capsulad.Activation) {
			enqueue(controller, a, logger)
		},
		TouchRelease: func(rel *capsulad.InputReleased) {
			if rel != nil && rel.ActivationID != "" {
				controller.Release(rel.ActivationID)
			}
		},
		Action: func(_ context.Context, a *capsulad.ActionInvoked) {
			if a == nil || a.ActionID != "dismiss" {
				return
			}
			logger.Printf("dismiss")
			controller.Dismiss()
		},
		Invocation: func(_ context.Context, inv *capsulad.LaunchInvocation) {
			_ = client.NotifyInvocationResult(inv.ID, true, "cadence client is ready")
		},
		Disconnected: func(err error) {
			logger.Printf("bus ended: %v", err)
		},
	})

	go controller.Run(ctx)
	go func() { _ = linkClient.Run(ctx) }()

	logger.Printf("starting bus")
	if err := client.Start(ctx); err != nil && ctx.Err() == nil {
		logger.Printf("bus stopped: %v", err)
	}
}

// linkHandler forwards the link's lifecycle and envelopes to the controller.
type linkHandler struct{ ctrl *turn.Controller }

func (h *linkHandler) LinkChanged(up bool) {
	if h.ctrl == nil {
		return
	}
	if !up {
		h.ctrl.LinkDown()
	}
}

func (h *linkHandler) Message(env *cadencev2.Envelope) {
	if h.ctrl == nil {
		return
	}
	h.ctrl.Message(env)
}

// applySettings validates a settings snapshot and installs it. A malformed
// value keeps the previous configuration and logs why; the token is never
// logged, and the redactor scrubs it from anything else.
func applySettings(sc *capsulad.SettingsChanged, current *atomic.Pointer[settings.Config],
	redactor *redact.Redactor, linkClient *link.Client, logger *log.Logger) {
	values := make(settings.Values, len(sc.Values))
	for _, v := range sc.Values {
		values[v.Key] = v.ValueJSON
	}
	next, err := settings.Parse(values, os.ReadFile)
	if err != nil {
		logger.Printf("settings rejected: %v (keeping the previous configuration)", err)
		return
	}
	current.Store(&next)
	redactor.Set(next.Token)
	for _, warning := range next.Warnings {
		logger.Printf("%s", warning)
	}
	if next.Configured {
		logger.Printf("settings applied server=%s", redact.URL(next.BaseURL))
	} else {
		logger.Printf("settings applied: not configured; set server_url and device_token (or device_token_file)")
	}
	// Re-dial so a changed server or credential takes effect now.
	linkClient.Wake()
}

// enqueue turns one framework activation into a queued turn. An activation
// without an id cannot be matched to its hold-to-talk release, so it is
// dropped rather than risking a mis-release.
func enqueue(controller *turn.Controller, a *capsulad.Activation, logger *log.Logger) {
	if a == nil {
		return
	}
	if a.ID == "" {
		logger.Printf("activation without an id dropped origin=%s", a.Origin)
		return
	}
	trig := turn.Trigger{
		ActivationID: a.ID,
		Origin:       a.Origin,
		ModelID:      a.MatchedModelID,
		HoldToTalk:   a.HoldToTalk,
		QueuedAt:     time.Now(),
	}
	if a.ExpiresMonotonicNS > a.MonotonicTimeNS {
		trig.Expires = time.Duration(a.ExpiresMonotonicNS - a.MonotonicTimeNS)
	}
	if !controller.Offer(trig) {
		logger.Printf("activation=%s dropped; queue is full", a.ID)
	}
}

// identity reads the board name from the framework. A failure is not fatal: the
// client still speaks, and the server only uses the board for display.
func identity(ctx context.Context, client *capsulad.Client, logger *log.Logger) string {
	const board = "capsula-mini"
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info struct {
		Model string `json:"model"`
	}
	if err := client.CallJSON(cctx, "", deviceService, "GetInfo", nil, &info, 5*time.Second); err != nil {
		logger.Printf("identity: %v", err)
		return board
	}
	if info.Model == "" {
		return board
	}
	return info.Model
}

// redactWriter scrubs registered secrets from every log line. The device token
// is a bearer credential; it never reaches the log even if a library error
// quotes it.
type redactWriter struct{ r *redact.Redactor }

func (w redactWriter) Write(p []byte) (int, error) {
	if _, err := os.Stderr.WriteString(w.r.Text(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
