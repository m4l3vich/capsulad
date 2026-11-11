// Package daemon wires the framework together: backend, routing, app
// supervision, buses, audio, input, settings, gRPC and the Web UI.
package daemon

import (
	"context"
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
	"google.golang.org/grpc/credentials/insecure"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/assets"
	"capsulad/internal/audio"
	"capsulad/internal/backend"
	"capsulad/internal/backend/rockchip"
	"capsulad/internal/bluetooth"
	"capsulad/internal/bus"
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
	"capsulad/internal/version"
	"capsulad/internal/wakeword"
	"capsulad/internal/websecurity"
	"capsulad/internal/webui"
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

	resources map[string]*frameworkv1.ResourceInfo
	resOrder  []string

	pres     *presentations.Manager
	fallback map[string]*presentations.Presentation
	router   *routing.Router

	user       *settings.UserStore
	autoMu     sync.Mutex
	autoLvl    map[string]float64
	appCfg     *settings.AppStore
	reg        *registry.Registry
	sup        *supervisor.Supervisor
	launcher   *sandbox.Launcher
	bus        *bus.Bus
	audio      *audio.Manager
	input      *input.Manager
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

	invMu       sync.Mutex
	invocations map[string]*invocationRecord
	pending     map[string][]*invocationRecord
	lastSel     map[string]string

	runMu   sync.Mutex
	runSubs map[int]chan supervisor.Run
	nextSub int

	indicatorMu  sync.Mutex
	recIndMu     sync.Mutex
	selMu        sync.Mutex
	sysRingMu    sync.Mutex
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

	btMu         sync.Mutex
	btInit       bool
	btPairable   bool
	btVolume     float64
	btVolSet     bool
	btVolumeAt   time.Time
	btPulseMu    sync.Mutex
	btPulseCap   string
	btPulseGen   uint64
	btPulseTmr   *time.Timer
	btRouteMu    sync.Mutex
	btRoutes     map[string]string
	sysRingState string

	lastActivation string
	lastActMu      sync.Mutex

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
	for _, dir := range []string{cfg.DataDir, cfg.RunDir, cfg.AppsDir, cfg.AssetsDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			cancel()
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
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
	d := &Daemon{
		cfg: cfg, ctx: ctx, cancel: cancel, clock: clock, logs: logs, bootID: ids.New("boot"),
		fallback:    map[string]*presentations.Presentation{},
		prov:        map[string]provenance.Result{},
		invocations: map[string]*invocationRecord{}, pending: map[string][]*invocationRecord{},
		lastSel: map[string]string{},
		runSubs: map[int]chan supervisor.Run{},
		closing: make(chan struct{}),
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
	d.seedDefaults()
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
	d.createFallbacks()

	d.launcher = sandbox.New(sandbox.Config{
		Mode: cfg.SandboxMode, SocketPath: cfg.SocketPath, AssetsDir: cfg.AssetsDir,
		UIDBase: cfg.SandboxUIDBase, Seccomp: true,
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
	d.sup.SetCallbacks(d.onRunState, d.onRunExit)

	d.audio = audio.NewManager(ctx, clock, logs, d.dev.Audio(), audio.Options{
		IndicationActive: d.indicationActive,
		WatchdogGraceMS:  5000,
		WatchdogPollMS:   1000,
	})
	d.input = input.NewManager(ctx, clock, logs, touchCaps(d.dev.Touch()), d.touchState, input.Handlers{
		VolumeUp:   func() bool { return d.adjustVolume(5) },
		VolumeDown: func() bool { return d.adjustVolume(-5) },
		MicToggle:  func() { d.setMuted(!d.user.Get().Values.MicrophoneMuted) },
		Activation: func(origin string) { d.triggerWakeword(origin, "") },
		Release:    d.sendTouchRelease,
		HoldToTalk: func() bool { return d.user.Get().Values.TouchHoldMode == settings.HoldToTalk },
	})
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
	if err := d.startGRPC(); err != nil {
		return err
	}
	if d.cfg.WebUIAddr != "" && d.websec.Ready() {
		tlsCfg, err := d.websec.TLSConfig()
		if err != nil {
			d.logs.Errorf("webui", "", "TLS configuration failed: %v", err)
		} else if web, err := webui.New(d.cfg.WebUIAddr, tlsCfg, d.localClientConn(), d.logs, string(d.dev.Platform()), d.websec); err != nil {
			d.logs.Errorf("webui", "", "init failed: %v", err)
		} else {
			d.web = web
			go func() {
				if err := web.Run(d.ctx); err != nil {
					d.logs.Errorf("webui", "", "server stopped: %v", err)
				}
			}()
		}
	} else if d.cfg.WebUIAddr != "" {
		d.logs.Warnf("webui", "", "HTTPS disabled: run `capsula auth init` (self-signed certificate + admin password)")
	}
	if d.cfg.DevAdminSocket != "" {
		d.adminSrv = d.startAdmin(d.cfg.DevAdminSocket)
	}

	d.applySettings(d.user.Get())
	d.startBootApps()
	d.ensurePulseBluetoothModules()
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
		d.bus.Close()
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
				PartialGesturesAvailable: caps.PartialGestures, FullRawAvailable: caps.FullRaw,
				CoordinatesAvailable: caps.Coordinates, PressureAvailable: caps.Pressure,
				SemanticGestures: append([]string(nil), caps.SemanticGestures...),
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

func (d *Daemon) createFallbacks() {
	for _, info := range d.resources {
		var kind presentations.Kind
		switch info.Kind {
		case frameworkv1.ResourceKind_RESOURCE_SEGMENT_DISPLAY:
			kind = presentations.Segment
		case frameworkv1.ResourceKind_RESOURCE_LED_RING:
			kind = presentations.Ring
		default:
			continue
		}
		p := d.pres.Open(info.ResourceId, routing.SystemAppID, "system", kind)
		if err := d.pres.Update(p, presentations.Blank()); err != nil {
			d.logs.Errorf("daemon", "", "fallback presentation: %v", err)
		}
		d.fallback[info.ResourceId] = p
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
