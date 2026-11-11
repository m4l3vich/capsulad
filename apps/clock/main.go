// Clock is the bundled default segment display provider.
//
// It opens one implicit baseline presentation on the segment display, sends a
// first frame immediately, and keeps updating in place. It never captures the
// display: ambient updates remain invisible when another app temporarily
// captures it.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os/signal"
	"sync"
	"syscall"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/sdk/go/capsula"
)

const resourceID = "display.segment.front"

// Seven-segment masks: bit 0 = a (top), 1 = b, 2 = c, 3 = d, 4 = e, 5 = f,
// 6 = g (middle).
var digits = map[rune]uint16{
	'0': 0b0111111, '1': 0b0000110, '2': 0b1011011, '3': 0b1001111,
	'4': 0b1100110, '5': 0b1101101, '6': 0b1111101, '7': 0b0000111,
	'8': 0b1111111, '9': 0b1101111, '-': 0b1000000, ' ': 0,
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	env := capsula.FromEnv()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	client, err := capsula.Connect(ctx, env.Socket, env.RunID)
	if err != nil {
		log.Fatalf("clock: connect: %v", err)
	}
	defer client.Close()

	blink := true
	var (
		locMu   sync.Mutex
		loc     *time.Location // nil means the device timezone
		locName string
	)
	setTimezone := func(name string) {
		var resolved *time.Location
		if name != "" {
			l, err := time.LoadLocation(name)
			if err != nil {
				log.Printf("clock: timezone %q unavailable (%v); using device timezone", name, err)
				name = ""
			} else {
				resolved = l
			}
		}
		locMu.Lock()
		loc, locName = resolved, name
		locMu.Unlock()
		if name == "" {
			log.Printf("clock: using the device timezone")
		} else {
			log.Printf("clock: timezone %s", name)
		}
	}
	nowInZone := func() (time.Time, string) {
		locMu.Lock()
		l, name := loc, locName
		locMu.Unlock()
		now := time.Now()
		if l != nil {
			now = now.In(l)
		}
		return now, name
	}
	var (
		ready chan struct{} = make(chan struct{})
	)
	client.SetHandlers(capsula.Handlers{
		Ready: func() {
			select {
			case <-ready:
			default:
				close(ready)
			}
			log.Printf("clock: ready")
		},
		Invocation: func(ctx context.Context, inv *frameworkv1.AppLaunchInvocation) {
			args, _ := json.Marshal(inv.Arguments)
			log.Printf("clock: invocation %s %s", inv.InvocationId, args)
			_ = client.NotifyInvocationResult(inv.InvocationId, true, "clock is always ready")
		},
		Settings: func(s *frameworkv1.AppSettingsChanged) {
			for _, v := range s.Values {
				switch v.Key {
				case "blink":
					var b bool
					if json.Unmarshal([]byte(v.ValueJson), &b) == nil {
						blink = b
					}
				case "timezone":
					var tz string
					if json.Unmarshal([]byte(v.ValueJson), &tz) == nil {
						setTimezone(tz)
					}
				}
			}
			log.Printf("clock: settings revision %d", s.Revision)
		},
		Action: func(ctx context.Context, a *frameworkv1.AppActionInvoked) {
			log.Printf("clock: action %s", a.ActionId)
			_ = client.NotifyActionResult(a.ActionInvocationId, true, "repainted")
		},
	})

	go func() {
		if err := client.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("clock: bus stopped: %v", err)
			cancel()
		}
	}()

	pres, err := client.OpenPresentation(ctx, resourceID)
	if err != nil {
		log.Printf("clock: display unavailable (%v); idling", err)
		<-ctx.Done()
		return
	}
	defer pres.Close()
	log.Printf("clock: presentation %s", pres.ID())

	render := func() {
		now, _ := nowInZone()
		hhmm := now.Format("1504")
		cells := make([]uint16, 0, len(hhmm))
		for _, r := range hhmm {
			cells = append(cells, digits[r])
		}
		colon := true
		if blink {
			colon = now.Second()%2 == 0
		}
		if err := pres.Segment(cells, colon); err != nil {
			log.Printf("clock: frame: %v", err)
		}
	}
	// First eligible content makes this presentation the baseline.
	render()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("clock: exiting")
			return
		case <-ticker.C:
			render()
		}
	}
}
