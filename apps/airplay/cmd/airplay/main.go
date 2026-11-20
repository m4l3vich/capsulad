// Command airplay is the Capsula Mini AirPlay 2 receiver.
//
// It is a normal supervised capsulad app: the daemon allocates every inbound
// socket, the Avahi responder publishes the service, audio goes to the
// framework mixer, the ring is a presentation and transport commands arrive
// through the standard media contract. The receiver speaks the AirPlay 2 wire
// protocol itself (HAP transient pairing, encrypted RTSP, realtime ALAC over
// RTP) and the legacy RAOP setup for senders that fall back to it.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"capsulad/apps/airplay/internal/hw"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/session"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("airplay: ")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	runtime, err := hw.NewRuntime(ctx, hw.FromEnv(), log.Printf)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer runtime.Close()

	board := media.NewBoard()

	var (
		mgrMu   sync.Mutex
		manager *session.Manager
	)
	currentManager := func() *session.Manager {
		mgrMu.Lock()
		defer mgrMu.Unlock()
		return manager
	}
	setManager := func(next *session.Manager) *session.Manager {
		mgrMu.Lock()
		defer mgrMu.Unlock()
		previous := manager
		manager = next
		return previous
	}

	ready := make(chan struct{}, 1)
	settingsCh := make(chan map[string]string, 1)
	onAction := func(id string) (bool, string) {
		m := currentManager()
		if m == nil {
			return false, "the receiver is not running"
		}
		switch id {
		case "end_session":
			if m.EndSession("action: end the current session") {
				return true, ""
			}
			return false, "no sender is connected"
		case "readvertise":
			if m.Republish() {
				return true, ""
			}
			return false, "the receiver is not advertised"
		}
		return false, "unknown action"
	}
	runtime.SetHandlers(
		func() { ready <- struct{}{} },
		func(_ uint64, values map[string]string) {
			// Keep only the newest snapshot: the receiver rebuilds from it.
			for {
				select {
				case settingsCh <- values:
					return
				default:
					select {
					case <-settingsCh:
					default:
					}
				}
			}
		},
		onAction,
	)

	// Register the media contract before the bus starts so a routed call that
	// races the app becoming ready finds a handler.
	runtime.ServeMediaContract(
		func() map[string]any {
			if m := currentManager(); m != nil {
				return m.MediaSnapshot()
			}
			return map[string]any{"players": []any{}}
		},
		func(ctx context.Context, send func(map[string]any) error) error {
			snapshot, events, unsubscribe := board.Subscribe()
			defer unsubscribe()
			if err := send(snapshot); err != nil {
				return err
			}
			for {
				select {
				case <-ctx.Done():
					return nil
				case event, ok := <-events:
					if !ok {
						return nil
					}
					if err := send(event); err != nil {
						return err
					}
				}
			}
		},
		func() (map[string]any, bool) {
			if !board.Live() {
				return nil, false
			}
			return board.Snapshot(), true
		},
		func(action string) (bool, string) {
			m := currentManager()
			if m == nil {
				return false, "the receiver is not running yet"
			}
			return m.Command(action)
		},
	)

	startErr := make(chan error, 1)
	go func() { startErr <- runtime.Start() }()

	select {
	case <-ctx.Done():
		return
	case err := <-startErr:
		log.Printf("framework connection ended: %v", err)
		return
	case <-ready:
	case <-time.After(30 * time.Second):
		log.Printf("the framework did not report the app ready; exiting")
		return
	}

	var values map[string]string
	select {
	case values = <-settingsCh:
	case <-time.After(5 * time.Second):
		log.Printf("no app settings arrived; starting with defaults")
		values = map[string]string{}
	}

	fingerprint := settingsFingerprint(values)
	currentValues := values
	next, err := startReceiver(ctx, runtime, board, values)
	if err != nil {
		log.Fatalf("starting the receiver: %v", err)
	}
	setManager(next)

	// Settings changes that affect the receiver restart it: a renamed receiver
	// re-advertises immediately, and an ended session is the documented cost of
	// changing the audio configuration.
	for {
		select {
		case <-ctx.Done():
			if previous := setManager(nil); previous != nil {
				_ = previous.Close()
			}
			runtime.Release()
			return
		case err := <-startErr:
			if previous := setManager(nil); previous != nil {
				_ = previous.Close()
			}
			runtime.Release()
			log.Printf("framework connection ended: %v", err)
			return
		case latest := <-settingsCh:
			if latest == nil {
				continue
			}
			nextFingerprint := settingsFingerprint(latest)
			if nextFingerprint == fingerprint {
				continue
			}
			log.Printf("receiver settings changed; restarting the receiver")
			// Withdraw the old advertisements before publishing the new ones:
			// otherwise the retiring manager's Close can remove the record the
			// replacement just published (a rename would make the speaker
			// vanish). If the replacement cannot start, the previous
			// configuration is brought back.
			if previous := setManager(nil); previous != nil {
				_ = previous.Close()
			}
			replacement, err := startReceiver(ctx, runtime, board, latest)
			if err != nil {
				log.Printf("receiver restart failed: %v; restoring the previous configuration", err)
				restored, restoreErr := startReceiver(ctx, runtime, board, currentValues)
				if restoreErr != nil {
					log.Printf("restoring the receiver also failed: %v", restoreErr)
					fingerprint = settingsFingerprint(currentValues)
					continue
				}
				setManager(restored)
				fingerprint = settingsFingerprint(currentValues)
				continue
			}
			setManager(replacement)
			currentValues = latest
			fingerprint = nextFingerprint
		}
	}
}

// startReceiver builds and starts one receiver instance from a settings
// snapshot.
func startReceiver(ctx context.Context, runtime *hw.Runtime, board *media.Board, values map[string]string) (*session.Manager, error) {
	cfg := configFromSettings(values)
	// One master: the receiver reports the speaker's persisted level and the
	// authenticated active sender may move it. The local UI, Bluetooth and the
	// framework limiter all consume the same value.
	cfg.MasterVolumeContext = runtime.MasterVolumeContext
	cfg.SetMasterVolume = runtime.SetMasterVolume
	var ptp session.PTPFactory
	if hw.SettingBool(values, "ptp", true) {
		ptp = runtime.PTPClock
	}
	manager, err := session.NewManager(
		cfg,
		runtime,
		runtime,
		runtime,
		runtime,
		board,
		runtime.PersistSetting,
		ptp,
		values,
	)
	if err != nil {
		return nil, err
	}
	if err := manager.Start(); err != nil {
		return nil, err
	}
	return manager, nil
}

// configFromSettings maps the manifest settings onto the receiver config.
func configFromSettings(values map[string]string) session.Config {
	cfg := session.Config{
		Name:          hw.SettingString(values, "name"),
		Model:         hw.SettingString(values, "receiver_model"),
		Port:          hw.SettingInt(values, "rtsp_port", 0),
		RingEnabled:   hw.SettingBool(values, "ring_artwork", true),
		FrameTarget:   time.Duration(hw.SettingInt(values, "buffer_ms", 1500)) * time.Millisecond,
		SourceVersion: hw.SettingString(values, "source_version"),
		Logf:          log.Printf,
	}
	if features := hw.SettingString(values, "features"); features != "" {
		var parsed uint64
		if err := json.Unmarshal([]byte(features), &parsed); err == nil {
			cfg.Features = parsed
		}
	}
	if cfg.Name == "" {
		cfg.Name = "AirPlay Receiver"
	}
	if cfg.Model == "" {
		cfg.Model = session.DefaultModel
	}
	if cfg.FrameTarget < 300*time.Millisecond {
		cfg.FrameTarget = 300 * time.Millisecond
	}
	if cfg.FrameTarget > 4*time.Second {
		cfg.FrameTarget = 4 * time.Second
	}
	return cfg
}

// settingsFingerprint covers the settings that shape a receiver instance. The
// generated identity settings are excluded: the app writes them itself and
// must not restart the receiver when it does.
func settingsFingerprint(values map[string]string) string {
	relevant := []string{
		"name", "receiver_model", "rtsp_port", "ring_artwork", "buffer_ms",
		"source_version", "features", "ptp", "pairing_pin",
	}
	selected := make(map[string]string, len(relevant))
	for _, key := range relevant {
		selected[key] = values[key]
	}
	blob, err := json.Marshal(selected)
	if err != nil {
		return ""
	}
	return string(blob)
}
