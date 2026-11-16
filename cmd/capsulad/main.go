// capsulad is the appliance daemon for the Capsula Mini speaker.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"capsulad/internal/backend"
	"capsulad/internal/backend/profile"
	"capsulad/internal/backend/rockchip"
	"capsulad/internal/daemon"
	"capsulad/internal/sandbox"
	"capsulad/internal/version"
)

func main() {
	ignoreHangup()
	if handled, code := sandbox.HandleHelper(os.Args); handled {
		os.Exit(code)
	}
	cfg := daemon.Config{}
	rk := profile.RK3308Rev4Stock
	fs := flag.NewFlagSet("capsulad", flag.ExitOnError)
	fs.StringVar(&cfg.Platform, "platform", "simulator", "target platform: simulator, rockchip-stock")
	fs.StringVar(&cfg.DataDir, "data-dir", "", "durable state directory (default .capsulad; production: /var/lib/capsulad)")
	fs.StringVar(&cfg.RunDir, "run-dir", "", "runtime directory (default <data-dir>/run)")
	fs.StringVar(&cfg.SocketPath, "socket", "", "framework Unix socket (default <run-dir>/hal.sock)")
	fs.StringVar(&cfg.WebUIAddr, "web-addr", "127.0.0.1:8443", "HTTPS Web UI listen address; empty disables it")
	fs.StringVar(&cfg.AppsDir, "apps-dir", "", "installed applications root (default <data-dir>/apps)")
	fs.StringVar(&cfg.AssetsDir, "assets-dir", "", "shared read-only assets root (default <data-dir>/assets)")
	fs.StringVar(&cfg.NativeLibDir, "native-lib-dir", "", "shared native SDK runtime directory (default <daemon-dir>/lib, else <daemon-dir>/../lib)")
	fs.StringVar(&cfg.SandboxMode, "sandbox", "auto", "sandbox mode: auto, namespaces, mountns, uid, none")
	fs.IntVar(&cfg.SandboxUIDBase, "sandbox-uid-base", 20000, "first host UID assigned to sandboxed apps")
	fs.StringVar(&cfg.TrustedRootPath, "trusted-root", "", "offline Sigstore trusted_root.json for provenance verification")
	fs.StringVar(&cfg.LogFile, "log-file", "", "daemon log file (default <data-dir>/capsulad.log)")
	fs.StringVar(&cfg.DevAdminSocket, "dev-admin", "", "development-only admin Unix socket (simulator); empty disables")
	fs.IntVar(&cfg.MemLimitMB, "mem-limit-mb", rk.Memory.HeapLimitMB, fmt.Sprintf("soft Go heap limit in MiB (0 disables); keeps the OOM killer away on a %d MiB board", rk.Memory.PhysicalMiB))
	fs.StringVar(&cfg.WakewordModel, "wakeword-model", "", "trained keyword classifier (default <data-dir>/wakeword/model.json)")
	fs.StringVar(&cfg.WakewordNegatives, "wakeword-negatives", "", "negative-training corpus (default <data-dir>/wakeword/negatives.bin; training input only)")
	fs.StringVar(&cfg.Supersede, "supersede", "auto", "vendor processes to replace: auto, off, or a comma-separated list")
	fs.BoolVar(&cfg.WakewordTrace, "wakeword-trace", false, "log the wakeword detector score twice a second")
	fs.BoolVar(&cfg.OutputTrace, "output-trace", os.Getenv("CAPSULAD_OUTPUT_TRACE") == "1", "sample rendered LED/display writes up to four times a second per resource")
	fs.StringVar(&cfg.Watchdog, "watchdog", "auto", fmt.Sprintf("hardware watchdog owner: auto (arm %s when present), off, or an explicit device path", rk.Watchdog.Path))
	showVersion := fs.Bool("version", false, "print version and exit")
	health := fs.Bool("health", false, "check a running daemon and exit 0 when it serves the framework API")
	fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Printf("capsulad %s (sdk %s)\n", version.Version, version.SDKVersion)
		return
	}
	if *health {
		os.Exit(checkHealth(cfg))
	}
	selected := profile.Simulator
	switch cfg.Platform {
	case "rockchip-stock", "rockchip":
		selected = rk
		cfg.WatchdogOpen = func(path string) (backend.Watchdog, error) { return rockchip.OpenWatchdog(path) }
		cfg.NewBackend = func(p profile.Profile, warnf, infof func(string, ...any)) (backend.Backend, error) {
			rc := rockchip.ConfigFor(p)
			rc.Warnf = warnf
			rc.Infof = infof
			return rockchip.New(rc)
		}
	}
	cfg.Profile = &selected
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	d, err := daemon.New(ctx, cfg)
	if err != nil {
		log.Fatalf("capsulad: %v", err)
	}
	if err := d.Run(); err != nil {
		log.Fatalf("capsulad: %v", err)
	}
}
