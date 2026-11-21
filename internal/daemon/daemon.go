// Package daemon wires the framework together: backend, routing, app
// supervision, buses, audio, input, settings, gRPC and the Web UI.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/assets"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/backend/rockchip"
	"capsulad/internal/bluetooth"
	"capsulad/internal/bus"
	"capsulad/internal/discovery"
	"capsulad/internal/grpcpeer"
	"capsulad/internal/ids"
	"capsulad/internal/input"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/presentations"
	"capsulad/internal/presets"
	"capsulad/internal/provenance"
	"capsulad/internal/registry"
	"capsulad/internal/routing"
	"capsulad/internal/sandbox"
	"capsulad/internal/settings"
	"capsulad/internal/supervisor"
	"capsulad/internal/uichanges"
	"capsulad/internal/version"
	"capsulad/internal/wakeword"
	"capsulad/internal/websecurity"
	"capsulad/internal/webui"
	"capsulad/internal/wifi"
)

type Config struct {
	Platform        string
	DataDir         string
	RunDir          string
	SocketPath      string
	WebUIAddr       string
	AppsDir         string
	AssetsDir       string
	SandboxMode     string
	SandboxUIDBase  int
	TrustedRootPath string
	DeviceModel     string
	DeviceID        string
	LogFile         string
	DevAdminSocket  string
	// WakewordModel is the trained classifier path; empty means
	// <data-dir>/wakeword/model.json, and a missing file simply means no
	// keyword is installed.
	WakewordModel string
	// WakewordNegatives overrides the license-clean negative-training corpus.
	// Empty means <data-dir>/wakeword/negatives.bin. The corpus is training
	// input only, never an inference dependency, so it can live outside the
	// device's 78.5 MiB /data partition.
	WakewordNegatives string
	// WakewordTrace logs the detector's peak score continuously.
	WakewordTrace bool
	// OutputTrace samples successful rendered hardware writes in the bounded
	// daemon log. Ownership and brightness transitions are always logged.
	OutputTrace bool
	// MemLimitMB bounds the daemon's Go heap. The RK3308 has 240 MiB in total
	// and the stock vendor stack coexists with framework apps, so an unbounded
	// heap lets the kernel OOM-kill the supervisor (which resets the board).
	MemLimitMB int
	// Watchdog selects the hardware liveness timer owner: "auto" arms
	// /dev/watchdog when the node exists, "off" leaves it to the vendor agent,
	// any other value is an explicit device path.
	Watchdog string
	// Supersede selects the vendor processes the framework replaces: "auto"
	// stops the stock assistant stack on stock platforms, "off" leaves it
	// running, any other value is a comma-separated process list.
	Supersede string
	// NativeLibDir is the host directory holding the shared libcapsula runtime
	// and capsula-native.json. Empty means the launcher resolves it next to the
	// daemon binary (<daemon-dir>/lib, else <daemon-dir>/../lib).
	NativeLibDir string
}

func DefaultConfig() Config {
	base := ".capsula"
	run := filepath.Join(base, "run")
	return Config{
		Platform:       "simulator",
		DataDir:        base,
		RunDir:         run,
		SocketPath:     filepath.Join(run, "hal.sock"),
		WebUIAddr:      "127.0.0.1:8443",
		AppsDir:        filepath.Join(base, "apps"),
		AssetsDir:      filepath.Join(base, "assets"),
		SandboxMode:    "auto",
		SandboxUIDBase: 20000,
		MemLimitMB:     80,
		Watchdog:       "auto",
		DeviceModel:    "Capsula Mini (simulator)",
	}
}

type Daemon struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc

	clock   *ids.Clock
	logs    *logbus.Bus
	bootID  string
	pidPath string
	dev     backend.Backend
	bt      bluetooth.Manager
	wifiMgr wifi.Manager

	resources map[string]*frameworkv1.ResourceInfo
	resOrder  []string

	pres     *presentations.Manager
	builtins *builtinProviders
	router   *routing.Router

	systemMu         sync.Mutex
	systemApps       map[string]*SystemApp
	systemRunMu      sync.Mutex
	systemFailMu     sync.Mutex
	systemFailures   map[string]int
	systemRestarting map[string]bool
	appValidatorMu   sync.RWMutex
	appValidators    map[string]func(values map[string]json.RawMessage) error

	user       *settings.UserStore
	autoMu     sync.Mutex
	autoLvl    map[string]float64
	appCfg     *settings.AppStore
	reg        *registry.Registry
	sup        *supervisor.Supervisor
	launcher   *sandbox.Launcher
	bus        *bus.Bus
	discovery  discovery.Manager
	audio      *audio.Manager
	input      *input.Manager
	sysMedia   *systemMediaController
	wake       *wakeword.Manager
	wd         *watchdogSupervisor
	wakeCancel func()
	wkSampleMu sync.Mutex
	recording  bool
	assets     *assets.Catalog
	websec     *websecurity.Store

	provMu sync.Mutex
	prov   map[string]provenance.Result

	grpcSrv *grpc.Server
	grpcLis net.Listener
	web     *webui.Server
	// uiChanges invalidates Web UI topics that have no framework watch stream.
	uiChanges *uichanges.Hub

	// internalToken proves a caller is a connection the daemon itself opened
	// (Web UI backend and compiled-in system apps). It is minted per boot and
	// never leaves this process, so supervised apps cannot reach private
	// system-app services over the app bus.
	internalToken string

	invMu       sync.Mutex
	invocations map[string]*invocationRecord
	pending     map[string][]*invocationRecord
	lastSel     map[string]string

	// activations records framework-issued assistant activations so microphone
	// pre-roll is attached only to a real activation delivered to that app, not
	// to an arbitrary ID supplied by any caller.
	actMu       sync.Mutex
	activations map[string]activationGrant

	runMu   sync.Mutex
	runSubs map[int]chan supervisor.Run
	nextSub int

	indicatorMu  sync.Mutex
	recIndMu     sync.Mutex
	selMu        sync.Mutex
	micCaptureID string
	micIndicator *presentations.Presentation
	recCaptureID string
	recIndicator *presentations.Presentation
	micParams    presets.Params
	osdMu        sync.Mutex
	osdPres      *presentations.Presentation
	osdCapture   string
	osdTimer     *time.Timer
	cueMu        sync.Mutex
	lastCue      time.Time
	micGen       uint64
	micTimer     *time.Timer
	lastMuted    bool
	lastApplied  *settings.UserSettings

	btMu           sync.Mutex
	btInit         bool
	btIncoming     bool
	btDiscoverable bool
	btApplyGen     uint64
	btApplyMu      sync.Mutex
	btVolume       float64
	btVolSet       bool
	btRouteMu      sync.Mutex
	btRoutes       map[string]string
	btNative       bool
	btStreamMu     sync.Mutex
	btStream       *a2dpStream
	btStreamGen    uint64
	btStreamStart  bool

	touchActivation string
	touchAssistant  string
	touchActMu      sync.Mutex

	adminSrv  *httpAdmin
	closing   chan struct{}
	closeOnce sync.Once
}

type invocationRecord struct {
	ID         string
	AppID      string
	RunID      string
	Arguments  map[string]string
	QueuedAtNS uint64
	Delivered  bool
	Accepted   *bool
	Detail     string
}

// New constructs the daemon and probes hardware. Nothing is started until Run.
// applyMemoryBudget keeps the Go heap inside the device budget: the soft limit
// makes the GC work harder instead of letting the kernel kill the daemon.
func applyMemoryBudget(cfg Config) {
	if cfg.MemLimitMB > 0 {
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	}
	debug.SetGCPercent(50)
}

func New(parent context.Context, cfg Config) (*Daemon, error) {
	applyMemoryBudget(cfg)
	if cfg.Platform == "" {
		cfg.Platform = "simulator"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultConfig().DataDir
	}
	if cfg.RunDir == "" {
		cfg.RunDir = filepath.Join(cfg.DataDir, "run")
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(cfg.RunDir, "hal.sock")
	}
	if cfg.AppsDir == "" {
		cfg.AppsDir = filepath.Join(cfg.DataDir, "apps")
	}
	if cfg.AssetsDir == "" {
		cfg.AssetsDir = filepath.Join(cfg.DataDir, "assets")
	}
	if cfg.SandboxMode == "" {
		cfg.SandboxMode = "auto"
	}
	if cfg.SandboxUIDBase == 0 {
		cfg.SandboxUIDBase = 20000
	}
	abs := func(v string) string {
		if v == "" {
			return v
		}
		if a, err := filepath.Abs(v); err == nil {
			return a
		}
		return v
	}
	cfg.DataDir, cfg.RunDir = abs(cfg.DataDir), abs(cfg.RunDir)
	cfg.SocketPath, cfg.AppsDir, cfg.AssetsDir = abs(cfg.SocketPath), abs(cfg.AppsDir), abs(cfg.AssetsDir)
	cfg.LogFile, cfg.TrustedRootPath = abs(cfg.LogFile), abs(cfg.TrustedRootPath)
	ctx, cancel := context.WithCancel(parent)
	clock := ids.NewClock()
	for _, dir := range []string{cfg.DataDir, cfg.RunDir, cfg.AppsDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			cancel()
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(cfg.AssetsDir, 0o755); err != nil {
		cancel()
		return nil, fmt.Errorf("create %s: %w", cfg.AssetsDir, err)
	}
	if err := os.Chmod(cfg.AssetsDir, 0o755); err != nil {
		cancel()
		return nil, fmt.Errorf("set %s mode: %w", cfg.AssetsDir, err)
	}
	logPath := cfg.LogFile
	if logPath == "" {
		logPath = filepath.Join(cfg.DataDir, "capsulad.log")
	}
	logs, err := logbus.New(clock, 6000, logPath)
	if err != nil {
		cancel()
		return nil, err
	}
	internalToken, err := newInternalToken()
	if err != nil {
		_ = logs.Close()
		cancel()
		return nil, err
	}
	d := &Daemon{
		cfg: cfg, ctx: ctx, cancel: cancel, clock: clock, logs: logs, bootID: ids.New("boot"),
		internalToken: internalToken,
		prov:          map[string]provenance.Result{},
		invocations:   map[string]*invocationRecord{}, pending: map[string][]*invocationRecord{},
		activations: map[string]activationGrant{},
		lastSel:     map[string]string{},
		runSubs:     map[int]chan supervisor.Run{},
		closing:     make(chan struct{}),
		uiChanges:   uichanges.New(),
	}

	switch cfg.Platform {
	case "simulator", "":
		d.dev = backend.NewSimulator(backend.SimulatorOptions{DeviceID: cfg.DeviceID, Model: cfg.DeviceModel})
	case "rockchip-stock", "rockchip":
		dev, err := rockchip.New(rockchip.DefaultConfig())
		if err != nil {
			logs.Close()
			cancel()
			return nil, err
		}
		d.dev = dev
	case "amlogic-stock", "amlogic-alpine":
		// Amlogic targets are in scope for the framework but this build has no
		// probed backend for them. Refuse to pretend a simulator is hardware.
		logs.Close()
		cancel()
		return nil, fmt.Errorf("platform %q is not implemented yet: no Amlogic stock/Alpine backend has been probed on hardware", cfg.Platform)
	default:
		logs.Close()
		cancel()
		return nil, fmt.Errorf("unknown platform %q (simulator, rockchip-stock, amlogic-stock, amlogic-alpine)", cfg.Platform)
	}

	if cfg.Platform == "simulator" {
		d.bt = bluetooth.NewMemory()
	} else {
		btCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		bt, err := bluetooth.Open(btCtx)
		cancel()
		if err != nil {
			logs.Warnf("bluetooth", "", "unavailable: %v", err)
		} else {
			d.bt = bt
		}
	}

	d.buildResources()
	logs.Infof("daemon", "", "platform=%s model=%s backends=%d", d.dev.Platform(), d.dev.Info().Model, len(d.dev.Capabilities()))

	if d.reg, err = registry.Open(cfg.AppsDir, logs); err != nil {
		return nil, err
	}
	d.user, err = settings.OpenUser(filepath.Join(cfg.DataDir, "settings.json"), logs, d.validateUserSettings)
	if err != nil {
		return nil, err
	}
	if cfg.Platform == "simulator" {
		d.wifiMgr = wifi.NewSimulator(wifi.SimulatorOptions{Detail: "the simulator has no Wi-Fi radio"})
	} else if manager, err := wifi.Open(wifi.Config{
		DataDir: cfg.DataDir, RunDir: cfg.RunDir, Logs: logs, APName: d.wifiAPName,
		OnChange: func() { d.uiChanges.Publish(uichanges.Wifi) },
	}); err != nil {
		logs.Warnf("wifi", "", "unavailable: %v", err)
		d.wifiMgr = wifi.NewSimulator(wifi.SimulatorOptions{Detail: err.Error()})
	} else {
		d.wifiMgr = manager
	}
	d.appCfg, err = settings.OpenApps(filepath.Join(cfg.DataDir, "app-settings"), logs, d.manifestSchema)
	if err != nil {
		return nil, err
	}
	d.assets, err = assets.Open(cfg.AssetsDir)
	if err != nil {
		return nil, err
	}
	d.websec = websecurity.Open(filepath.Join(cfg.DataDir, "security"), websecurity.DefaultOptions())
	if err := d.websec.Load(); err != nil {
		return nil, err
	}
	d.watchWebCredentials()
	d.pres = presentations.NewManager(clock, logs, func(resourceID string) {
		d.router.Recompute(resourceID, "presentation changed")
	})
	d.router = routing.New(clock, logs, d.pres)
	d.router.SetOnChange(d.applyRouting)
	for resourceID, info := range d.resources {
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			d.router.SetResourceClass(resourceID, "display")
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			d.router.SetResourceClass(resourceID, "ring")
		case frameworkv1.ResourceKind_RESOURCE_TOUCH:
			d.router.SetResourceClass(resourceID, "touch")
		case frameworkv1.ResourceKind_RESOURCE_VOICE_ASSISTANT_ROLE:
			d.router.SetResourceClass(resourceID, "assistant")
		case frameworkv1.ResourceKind_RESOURCE_AUDIO_INPUT, frameworkv1.ResourceKind_RESOURCE_AUDIO_OUTPUT:
			d.router.SetResourceClass(resourceID, "audio")
		default:
			d.router.SetResourceClass(resourceID, "sensor")
		}
	}
	d.launcher = sandbox.New(sandbox.Config{
		Mode: cfg.SandboxMode, SocketPath: cfg.SocketPath, AssetsDir: cfg.AssetsDir,
		UIDBase: cfg.SandboxUIDBase, Seccomp: true, LibraryDir: cfg.NativeLibDir,
	})
	d.sup = supervisor.New(ctx, logs, clock, cfg.SocketPath, d.launcher)
	d.bus = bus.New(logs, func(runID string) (string, int, bool) {
		run, ok := d.sup.Get(runID)
		if !ok {
			return "", 0, false
		}
		return run.AppID, run.PID, true
	}, bus.Hooks{
		OnReady:            d.onAppReady,
		OnDisconnect:       d.onAppDisconnect,
		OnInvocationResult: d.onInvocationResult,
		OnActionResult:     d.onActionResult,
	})
	d.bus.SetInternalAuthorizer(d.authorizeInternalBusCall)
	d.sup.SetCallbacks(d.onRunState, d.onRunExit)
	d.sup.SetRestartPolicy(d.isSelectedDefault)
	// System apps share the registry, supervisor and app-bus routing table with
	// supervised packages. They are constructed here, after those exist, and
	// started by Run before external boot applications.
	d.builtins = newBuiltins(d)
	d.appCfg.SetValidator(d.validateAppSettings)
	d.registerAssistantSettingsValidator()

	d.audio = audio.NewManager(ctx, clock, logs, d.dev.Audio(), audio.Options{
		IndicationActive: d.indicationActive,
		IndicationCue:    d.audibleMicIndication,
		WatchdogGraceMS:  5000,
		// Delivery is also checked per chunk; the 250 ms poll bounds how long a
		// lost indication can keep a capture alive before termination.
		WatchdogPollMS: 250,
	})
	d.sysMedia = newSystemMediaController(d)
	d.input = input.NewManager(ctx, clock, logs, touchCaps(d.dev.Touch()), d.touchState, input.Handlers{
		VolumeUp:         func() bool { return d.adjustVolume(5) },
		VolumeDown:       func() bool { return d.adjustVolume(-5) },
		MicToggle:        func() { d.setMuted(!d.user.Get().Values.MicrophoneMuted) },
		Activation:       func(origin string) { d.triggerWakeword(origin, "") },
		Release:          d.sendTouchRelease,
		HoldToTalk:       func() bool { return d.user.Get().Values.TouchHoldMode == settings.HoldToTalk },
		DismissAlert:     d.dismissFrameworkAlerts,
		DismissAssistant: func() bool { return d.dismissAssistantTurn() },
		PrimaryActive:    d.clockPrimaryActive,
		PrimaryGesture:   d.clockPrimaryGesture,
		MediaToggle:      d.sysMedia.Toggle,
		MediaPrevious:    d.sysMedia.Previous,
		MediaNext:        d.sysMedia.Next,
	})
	go d.sysMedia.Run(ctx)
	d.wake = wakeword.NewManager(ctx, clock, logs, d.wakeModels())
	d.wake.SetOnFire(d.deliverActivation)
	d.startWakewordEngine()

	d.autoLvl = map[string]float64{}
	d.wd = newWatchdogSupervisor(cfg.RunDir, logs, cfg.Watchdog)

	if err := d.audio.Start(); err != nil {
		logs.Warnf("daemon", "", "audio service unavailable: %v", err)
	}
	return d, nil
}

// dismissFrameworkAlerts silences a sounding framework alert (clock timer or
// alarm) from a physical control and reports whether one was showing. The
// input manager uses that result to consume the press instead of summoning the
// assistant, so one button press cannot both dismiss and activate.
func (d *Daemon) dismissFrameworkAlerts() bool {
	if d.builtins == nil || d.builtins.clock == nil {
		return false
	}
	return d.builtins.clock.dismissAlert("")
}

// dismissAssistantTurn ends the selected assistant's in-flight turn from the
// circle/play-pause key. The turn is identified by the resources the assistant
// currently owns: the ring presentation it holds for the whole turn (including
// the reply delay and playback) and its audio streams. Dismissal goes through
// the app's declared "dismiss" action, so the app stops playback, closes its
// ring/microphone streams and returns to idle without being killed. A turn is
// always consumed even when the app does not implement the action, so the same
// press cannot start a second turn on top of it.
func (d *Daemon) dismissAssistantTurn() bool {
	appID := d.assistantAppID()
	if appID == "" || !d.assistantTurnOwned(appID) {
		return false
	}
	if _, err := d.TriggerAction(appID, "dismiss"); err != nil {
		// Consume the press anyway: a second activation on top of a turn the
		// app cannot dismiss is worse than an unresponsive provider.
		d.logs.Warnf("input", appID, "dismiss action: %v", err)
		return true
	}
	d.logs.Infof("input", appID, "assistant turn dismissed by the circle key")
	return true
}

// assistantTurnOwned reports whether the assistant currently owns a visible or
// audible resource. The ring presentation is the stable marker for the whole
// turn; streams cover a turn whose presentation failed to open.
func (d *Daemon) assistantTurnOwned(appID string) bool {
	if appID == "" || d.router == nil {
		return false
	}
	if d.audio != nil {
		for _, st := range d.audio.ActiveStreams() {
			if st.AppID == appID {
				return true
			}
		}
	}
	for resourceID, info := range d.resources {
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY,
			frameworkv1.ResourceKind_RESOURCE_LED_RING:
		default:
			continue
		}
		if sel, _ := d.router.State(resourceID); sel.SelectedAppID == appID {
			return true
		}
	}
	return false
}

// clockPrimaryActive reports whether a running stopwatch owns the primary
// button, so a tap pauses it instead of summoning the assistant.
func (d *Daemon) clockPrimaryActive() bool {
	if d.builtins == nil || d.builtins.clock == nil {
		return false
	}
	return d.builtins.clock.stopwatchEngaged()
}

// clockPrimaryGesture routes a framework-owned primary gesture to the clock.
func (d *Daemon) clockPrimaryGesture(kind string) bool {
	if d.builtins == nil || d.builtins.clock == nil {
		return false
	}
	return d.builtins.clock.primaryGesture(kind)
}

func touchCaps(t backend.TouchDevice) backend.TouchCaps {
	if t == nil {
		return backend.TouchCaps{}
	}
	return t.Caps()
}

func (d *Daemon) wakeModels() []wakeword.Model {
	if d.dev.Platform() == backend.PlatformSimulator {
		return []wakeword.Model{{
			ID: "sim.wakeword.manual", DisplayName: "Manual trigger (simulator)",
			Language: "any", EngineID: "sim.manual", Available: true,
		}}
	}
	return []wakeword.Model{{
		ID: "word.personal", DisplayName: "Record a personal wakeword",
		EngineID: "framework.embedding-mlp", Available: false,
	}}
}

// pidFileName is written into the run directory so the boot service and the
// upgrade CLI stop exactly the daemon they started rather than any same-named
// process. A crashed daemon leaves a stale file; readers must check the PID.
const pidFileName = "capsulad.pid"

func (d *Daemon) writePIDFile() {
	path := filepath.Join(d.cfg.RunDir, pidFileName)
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		d.logs.Warnf("daemon", "", "pid file: %v", err)
		return
	}
	d.pidPath = path
}

// Run starts servers, boot applications and background loops; it blocks until
// the context is cancelled or a fatal server error occurs.
func (d *Daemon) Run() error {
	d.writePIDFile()
	if err := d.launcher.Probe(d.ctx); err != nil {
		d.logs.Warnf("sandbox", "", "probe: %v", err)
	}
	d.logs.Infof("sandbox", "", "mode=%s (%s)", d.launcher.Mode(), d.launcher.Detail())

	if t := d.dev.Touch(); t != nil {
		go func() {
			if err := t.Start(d.ctx, d.input.Handle); err != nil {
				d.logs.Warnf("input", "", "touch backend stopped: %v", err)
			}
		}()
	}
	// Register the A2DP sink endpoint before the API or Web UI can expose a
	// connectable adapter. Pairing settings are applied later, after this
	// registration, so no source observes a half-initialized adapter.
	d.initBluetoothAudio()
	if err := d.startGRPC(); err != nil {
		return err
	}
	d.discovery = discovery.Open(d.logs, string(d.dev.Platform()))
	if err := d.discovery.Start(d.ctx); err != nil {
		d.logs.Warnf("mdns", "", "start responder monitor: %v", err)
	}
	if d.cfg.WebUIAddr != "" && d.websec.Ready() {
		tlsCfg, err := d.websec.TLSConfig()
		if err != nil {
			d.logs.Errorf("webui", "", "TLS configuration failed: %v", err)
		} else if web, err := webui.New(d.cfg.WebUIAddr, tlsCfg, d.localClientConn(), d.logs, string(d.dev.Platform()), d.websec, d.discovery, d.uiChanges); err != nil {
			d.logs.Errorf("webui", "", "init failed: %v", err)
		} else {
			web.Apps = d.reg
			web.UserSettings = d.user
			web.Assets = d.assets
			web.Wifi = d.wifiMgr
			web.Wakeword = d.wake
			d.web = web
			go func() {
				if err := web.Run(d.ctx); err != nil {
					d.logs.Errorf("webui", "", "server stopped: %v", err)
				}
			}()
		}
	} else if d.cfg.WebUIAddr != "" {
		d.logs.Warnf("webui", "", "HTTPS disabled: run `capsulad-cli auth init` (self-signed certificate + admin password)")
	}
	if d.cfg.DevAdminSocket != "" {
		d.adminSrv = d.startAdmin(d.cfg.DevAdminSocket)
	}

	d.seedDefaults()
	d.applySettings(d.user.Get())
	d.maybePlayStartupCue()
	d.startSystemApps()
	d.startBootApps()
	go d.runWifi()
	go d.watchBluetooth()
	go d.watchSettings()
	go d.autoBrightnessLoop()
	go d.outputLoop()
	// Arm the hardware timer only after the daemon is serving: an armed timer
	// cannot be disarmed on this kernel, so a startup failure must not leave
	// the board in a reset loop.
	// Start the retry loop first: it cannot open /dev/watchdog while the
	// vendor still holds it, and the moment the vendor exits it takes over.
	d.wd.Start(d.ctx)
	d.supersedeVendor()

	<-d.ctx.Done()
	d.shutdown()
	return nil
}

func (d *Daemon) startGRPC() error {
	if err := os.MkdirAll(filepath.Dir(d.cfg.SocketPath), 0o750); err != nil {
		return err
	}
	_ = os.Remove(d.cfg.SocketPath)
	lis, err := net.Listen("unix", d.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.cfg.SocketPath, err)
	}
	// The cooperative threat model relies on unguessable run IDs plus peer
	// credentials, not socket-group ACLs; sandboxed apps use distinct UIDs and
	// would otherwise be unable to connect.
	if err := os.Chmod(d.cfg.SocketPath, 0o666); err != nil {
		d.logs.Warnf("daemon", "", "chmod socket: %v", err)
	}
	d.grpcLis = &grpcpeer.Listener{Listener: lis}
	d.grpcSrv = grpc.NewServer(
		grpc.MaxRecvMsgSize(8<<20),
		grpc.MaxSendMsgSize(8<<20),
	)
	d.registerServices(d.grpcSrv)
	go func() {
		if err := d.grpcSrv.Serve(d.grpcLis); err != nil {
			d.logs.Infof("daemon", "", "gRPC server stopped: %v", err)
		}
	}()
	d.logs.Infof("daemon", "", "framework socket %s", d.cfg.SocketPath)
	return nil
}

// localClientConn dials the daemon's own Unix socket for the Web UI, keeping
// the browser backend on the same API surface as every other client.
func (d *Daemon) localClientConn() *grpc.ClientConn {
	conn, err := grpc.NewClient("passthrough:///capsulad",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(internalTokenAuth{token: d.internalToken}),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", d.cfg.SocketPath)
		}),
	)
	if err != nil {
		panic(err)
	}
	return conn
}

// internalTokenKey carries the daemon's per-boot internal-caller proof. Only
// conns created by localClientConn attach it; it is not a user credential and
// is never written to disk or logs.
const internalTokenKey = "capsula-internal-token"

type internalTokenAuth struct{ token string }

func (a internalTokenAuth) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{internalTokenKey: a.token}, nil
}

// RequireTransportSecurity is false because the framework socket is a local
// Unix socket with filesystem permissions, not a network transport.
func (internalTokenAuth) RequireTransportSecurity() bool { return false }

func newInternalToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate internal caller token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// isInternalPeer reports whether ctx carries the current per-boot token. The
// comparison is constant time; a stale token from a previous daemon process is
// rejected even if the caller somehow learned it.
func (d *Daemon) isInternalPeer(ctx context.Context) bool {
	if d.internalToken == "" {
		return false
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	want := []byte(d.internalToken)
	for _, v := range md.Get(internalTokenKey) {
		if subtle.ConstantTimeCompare([]byte(v), want) == 1 {
			return true
		}
	}
	return false
}

// authorizeInternalBusCall gates routed calls to compiled-in system-app
// targets. A service the target app publicly declares through its manifest
// equivalent (registry interfaces) is reachable exactly like an installed
// app's declared interface; anyone can discover it through
// AppInterfaceService. Undeclared services remain denied, and caller identity
// is still resolved by the bus from the run ID and socket peer credentials.
func (d *Daemon) authorizeInternalBusCall(ctx context.Context, targetAppID, service string) error {
	if d.isInternalPeer(ctx) {
		return nil
	}
	if app, ok := d.reg.Get(targetAppID); ok {
		if _, declared := app.InterfacesFor(service); declared {
			return nil
		}
	}
	d.logs.Warnf("bus", "", "denied undeclared app-bus target %s/%s from an application connection", targetAppID, service)
	return status.Error(codes.PermissionDenied, "this app-bus service is not exposed to application callers")
}

// isSupervisedPID reports whether pid belongs to an external supervised run.
// Compiled-in runs share the daemon's PID and are therefore never matched;
// their callers prove identity with the internal token instead.
func (d *Daemon) isSupervisedPID(pid int) bool {
	if pid <= 0 || pid == os.Getpid() || d.sup == nil {
		return false
	}
	for _, run := range d.sup.List("") {
		if !run.Virtual && run.PID == pid {
			return true
		}
	}
	return false
}

func (d *Daemon) shutdown() {
	d.closeOnce.Do(func() {
		close(d.closing)
		d.logs.Infof("daemon", "", "shutting down")
		if d.adminSrv != nil {
			d.adminSrv.Close()
		}
		if d.grpcSrv != nil {
			stopped := make(chan struct{})
			go func() { d.grpcSrv.GracefulStop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				d.grpcSrv.Stop()
			}
		}
		d.sup.Shutdown(5 * time.Second)
		d.pres.CloseAll()
		d.audio.Close()
		d.wake.Close()
		d.input.Close()
		d.sysMedia.Close()
		if d.discovery != nil {
			_ = d.discovery.Close()
		}
		d.bus.Close()
		if d.wifiMgr != nil {
			_ = d.wifiMgr.Close()
		}
		d.stopBluetoothStream()
		d.resetBluetoothRoutes()
		if d.bt != nil {
			_ = d.bt.Close()
		}
		if d.wd != nil {
			d.wd.disarm()
		}
		if d.pidPath != "" {
			_ = os.Remove(d.pidPath)
		}
		_ = d.dev.Close()
		d.cancel()
		_ = d.logs.Close()
	})
}

// --- resource catalog ------------------------------------------------------

func (d *Daemon) buildResources() {
	add := func(info *frameworkv1.ResourceInfo) {
		d.resources[info.ResourceId] = info
		d.resOrder = append(d.resOrder, info.ResourceId)
	}
	if d.resources == nil {
		d.resources = map[string]*frameworkv1.ResourceInfo{}
	}
	if seg := d.dev.Segment(); seg != nil {
		caps := seg.Caps()
		add(&frameworkv1.ResourceInfo{
			ResourceId: seg.ResourceID(), Kind: frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY,
			Routing:   frameworkv1.ResourceRouting_ROUTING_DEFAULT_WITH_CAPTURE,
			Available: true, BackendName: "backend.segment",
			Details: &frameworkv1.ResourceInfo_Segment{Segment: &frameworkv1.SegmentCapabilities{
				DigitCount: uint32(caps.DigitCount), SupportsColon: caps.SupportsColon,
				SupportsDecimalPoints: caps.SupportsDecimal,
			}},
		})
	}
	if ring := d.dev.Ring(); ring != nil {
		caps := ring.Caps()
		add(&frameworkv1.ResourceInfo{
			ResourceId: ring.ResourceID(), Kind: frameworkv1.ResourceKind_RESOURCE_LED_RING,
			Routing:   frameworkv1.ResourceRouting_ROUTING_DEFAULT_WITH_CAPTURE,
			Available: true, BackendName: "backend.ring",
			Details: &frameworkv1.ResourceInfo_Ring{Ring: &frameworkv1.RingCapabilities{
				PixelCount: uint32(caps.PixelCount), SupportsRgb: caps.SupportsRGB,
			}},
		})
	}
	if touch := d.dev.Touch(); touch != nil {
		caps := touch.Caps()
		add(&frameworkv1.ResourceInfo{
			ResourceId: "touch.panel.main", Kind: frameworkv1.ResourceKind_RESOURCE_TOUCH,
			Routing:   frameworkv1.ResourceRouting_ROUTING_DEFAULT_WITH_CAPTURE,
			Available: true, BackendName: "backend.touch",
			Details: &frameworkv1.ResourceInfo_Touch{Touch: &frameworkv1.TouchCapabilities{
				PartialGesturesAvailable: caps.PartialGestures,
				FullRawAvailable:         caps.FullRaw,
				FullPanelAvailable:       caps.FullPanel,
				CoordinatesAvailable:     caps.Coordinates,
				PressureAvailable:        caps.Pressure,
				SemanticGestures:         append([]string(nil), caps.SemanticGestures...),
			}},
		})
	}
	if d.dev.Audio() != nil {
		add(&frameworkv1.ResourceInfo{
			ResourceId: "audio.input.microphone", Kind: frameworkv1.ResourceKind_RESOURCE_AUDIO_INPUT,
			Routing:   frameworkv1.ResourceRouting_ROUTING_SHARED_STREAMS,
			Available: true, BackendName: "backend.audio",
			Features: []string{"fanout", "preroll"},
		})
		add(&frameworkv1.ResourceInfo{
			ResourceId: "audio.output.speaker", Kind: frameworkv1.ResourceKind_RESOURCE_AUDIO_OUTPUT,
			Routing:   frameworkv1.ResourceRouting_ROUTING_SHARED_STREAMS,
			Available: true, BackendName: "backend.audio",
			Features: []string{"mixing", "ducking"},
		})
	}
	add(&frameworkv1.ResourceInfo{
		ResourceId: "role.voice_assistant", Kind: frameworkv1.ResourceKind_RESOURCE_VOICE_ASSISTANT_ROLE,
		Routing:   frameworkv1.ResourceRouting_ROUTING_DEFAULT_WITH_CAPTURE,
		Available: true, BackendName: "framework",
	})
	if sensors := d.dev.Sensors(); sensors != nil {
		for _, s := range sensors.List() {
			add(&frameworkv1.ResourceInfo{
				ResourceId: s.ID, Kind: frameworkv1.ResourceKind_RESOURCE_SENSOR,
				Routing:   frameworkv1.ResourceRouting_ROUTING_READ_ONLY,
				Available: true, BackendName: "backend.sensor", Features: []string{"unit:" + s.Unit},
			})
		}
	}
	for _, cap := range d.dev.Capabilities() {
		if cap.Status == backend.Unverified || cap.Status == backend.Partial {
			if info, ok := d.resources[resourceForSubsystem(cap.Subsystem)]; ok {
				info.Features = append(info.Features, "backend_"+strings.ToLower(string(cap.Status)))
			}
		}
	}
}

func resourceForSubsystem(subsystem string) string {
	switch subsystem {
	case "segment":
		return "display.segment.front"
	case "ring":
		return "light.ring.main"
	case "touch":
		return "touch.panel.main"
	case "audio":
		return "audio.input.microphone"
	default:
		return ""
	}
}

// --- helpers used by service handlers -------------------------------------

func (d *Daemon) platformProto() frameworkv1.TargetPlatform {
	switch d.dev.Platform() {
	case backend.PlatformSimulator:
		return frameworkv1.TargetPlatform_TARGET_PLATFORM_UNSPECIFIED
	case backend.PlatformRockchipStock:
		return frameworkv1.TargetPlatform_CAPSULA_MINI_ROCKCHIP_STOCK
	case backend.PlatformAmlogicStock:
		return frameworkv1.TargetPlatform_CAPSULA_MINI_AMLOGIC_STOCK
	case backend.PlatformAmlogicAlpine:
		return frameworkv1.TargetPlatform_CAPSULA_MINI_AMLOGIC_ALPINE
	default:
		return frameworkv1.TargetPlatform_TARGET_PLATFORM_UNSPECIFIED
	}
}

func (d *Daemon) deviceInfo(bootID string) *frameworkv1.DeviceInfo {
	info := d.dev.Info()
	out := &frameworkv1.DeviceInfo{
		DeviceId: info.DeviceID, Model: info.Model, BoardRevision: info.BoardRevision,
		Soc: info.SoC, Kernel: info.Kernel, Firmware: info.Firmware,
		DaemonVersion: version.Version, BootId: bootID, TargetPlatform: d.platformProto(),
	}
	for _, cap := range d.dev.Capabilities() {
		out.SubsystemBackends = append(out.SubsystemBackends, &frameworkv1.BackendAvailability{
			Subsystem: cap.Subsystem, BackendId: cap.BackendID,
			Status: backendStatusProto(cap.Status), Detail: cap.Detail,
		})
	}
	if d.wd != nil {
		st := d.wd.State()
		status := backend.Unavailable
		switch {
		case st.armed:
			status = backend.Available
		case st.enabled:
			status = backend.Partial
		}
		detail := st.detail
		if st.armed && st.timeout > 0 {
			detail = fmt.Sprintf("%s armed, timeout %s; framework keeps the hardware timer alive instead of the vendor agent", st.path, st.timeout)
		}
		out.SubsystemBackends = append(out.SubsystemBackends, &frameworkv1.BackendAvailability{
			Subsystem: "watchdog", BackendId: "rockchip.syswatchdog",
			Status: backendStatusProto(status), Detail: detail,
		})
	}
	return out
}

func backendStatusProto(s backend.Status) frameworkv1.BackendStatus {
	switch s {
	case backend.Available:
		return frameworkv1.BackendStatus_BACKEND_AVAILABLE
	case backend.Partial:
		return frameworkv1.BackendStatus_BACKEND_PARTIAL
	case backend.Unverified:
		return frameworkv1.BackendStatus_BACKEND_UNVERIFIED
	default:
		return frameworkv1.BackendStatus_BACKEND_UNAVAILABLE
	}
}

func (d *Daemon) resourceList() []*frameworkv1.ResourceInfo {
	out := make([]*frameworkv1.ResourceInfo, 0, len(d.resOrder))
	for _, id := range d.resOrder {
		out = append(out, d.resources[id])
	}
	return out
}

func (d *Daemon) installedApp(appID string) (*registry.App, bool) {
	return d.reg.Get(appID)
}

func (d *Daemon) manifestSchema(appID string) (*settings.ManifestSchema, bool) {
	app, ok := d.reg.Get(appID)
	if !ok {
		return nil, false
	}
	schema := &settings.ManifestSchema{AppID: appID}
	for _, s := range app.Manifest.Settings {
		def := settings.SchemaSetting{Key: s.Key, Type: s.Type, Required: s.Required}
		if s.Default != nil {
			if raw, err := manifest.ValueJSON(s.Default); err == nil {
				def.DefaultJSON = raw
			}
		}
		schema.Settings = append(schema.Settings, def)
	}
	return schema, true
}

func (d *Daemon) validateUserSettings(v settings.UserSettings) error {
	for resourceID, appID := range v.DefaultAppByResource {
		info, ok := d.resources[resourceID]
		if !ok {
			return fmt.Errorf("unknown resource %s", resourceID)
		}
		if info.Kind != frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY && info.Kind != frameworkv1.ResourceKind_RESOURCE_LED_RING {
			return fmt.Errorf("resource %s does not accept a default application", resourceID)
		}
		if appID == "" {
			continue
		}
		if _, ok := d.reg.Get(appID); !ok {
			return fmt.Errorf("application %s is not installed", appID)
		}
	}
	if v.DefaultAssistantAppID != "" {
		if _, ok := d.reg.Get(v.DefaultAssistantAppID); !ok {
			return fmt.Errorf("assistant application %s is not installed", v.DefaultAssistantAppID)
		}
	}
	return nil
}

func (d *Daemon) wakeModelsProto() []*frameworkv1.WakewordModelInfo {
	models := d.wake.Models()
	out := make([]*frameworkv1.WakewordModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, &frameworkv1.WakewordModelInfo{
			ModelId: m.ID, DisplayName: m.DisplayName, Language: m.Language,
			EngineId: m.EngineID, Available: m.Available,
		})
	}
	return out
}
