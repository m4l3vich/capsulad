package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

	// The framework's model is a human name ("VK Capsula Mini (A113X)"), not
	// an upstream board id. BoardID maps it to the id the dashboard enrolls
	// and the link reports; an explicit board setting wins when the operator
	// enrolled the device under another upstream model.
	model := deviceModel(ctx, client, logger)
	board := settings.BoardID(settings.BoardAuto, model)

	// The link's handler needs the controller, and the controller sends on the
	// link; the holder breaks the cycle. The link is not reading envelopes
	// before Run, which starts after both are built.
	holder := &linkHandler{}
	dialer := &link.SDKDialer{Env: env, Config: func() settings.Config { return currentConfig(holder) }}
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
		Settings: func() settings.Config { return currentConfig(holder) },
		NewID:    capsulad.UUIDv4,
	})
	holder.ctrl = controller

	runtime := newRuntime(ctx, client, env, logger, redactor, linkClient, model, assetStoreRoot())
	holder.runtime = runtime

	client.SetHandlers(capsulad.Handlers{
		Ready: func() { logger.Printf("ready board=%s", linkClient.Board()) },
		Settings: func(sc *capsulad.SettingsChanged) {
			runtime.applySettings(sc)
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
			if a == nil {
				return
			}
			switch a.ActionID {
			case "dismiss":
				logger.Printf("dismiss")
				controller.Dismiss()
			case "test_connection":
				go func() {
					ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
					defer cancel()
					if err := runtime.testConnection(ctx); err != nil {
						_ = client.NotifyActionResult(a.ID, false, err.Error())
						return
					}
					_ = client.NotifyActionResult(a.ID, true, "the Cadence server is reachable")
				}()
			}
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
type linkHandler struct {
	ctrl    *turn.Controller
	runtime *runtime
}

func (h *linkHandler) LinkChanged(up bool, generation uint64) {
	if h.ctrl == nil {
		return
	}
	h.ctrl.LinkChanged(up, generation)
}

func (h *linkHandler) Message(generation uint64, env *cadencev2.Envelope) {
	if h.ctrl == nil {
		return
	}
	h.ctrl.Message(generation, env)
}

func currentConfig(h *linkHandler) settings.Config {
	if h.runtime == nil {
		return settings.Config{HoldTail: time.Second, FollowUp: true}
	}
	return h.runtime.config()
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

// deviceModel reads the framework's board model. A failure is not fatal: the
// link still speaks, and BoardID falls back to vk-capsula-mini rather than
// inventing a board name.
func deviceModel(ctx context.Context, client *capsulad.Client, logger *log.Logger) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info struct {
		Model string `json:"model"`
	}
	if err := client.CallJSON(cctx, "", deviceService, "GetInfo", nil, &info, 5*time.Second); err != nil {
		logger.Printf("identity: %v; the board id falls back to vk-capsula-mini (set board to override)", err)
		return ""
	}
	return info.Model
}

// assetStoreRoot is the framework's shared asset store. The sandbox always
// exports CAPSULAD_ASSETS; an unsandboxed run gets the canonical mount, like
// the framework's other assistant.
func assetStoreRoot() string {
	root := strings.TrimSpace(os.Getenv("CAPSULAD_ASSETS"))
	if root == "" || !filepath.IsAbs(root) {
		return "/assets"
	}
	return filepath.Clean(root)
}

// redactWriter scrubs registered secrets from every log line. The device token
// and the one-time enrollment code are bearer credentials; they never reach
// the log even if a library error quotes them.
type redactWriter struct{ r *redact.Redactor }

func (w redactWriter) Write(p []byte) (int, error) {
	if _, err := os.Stderr.WriteString(w.r.Text(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
