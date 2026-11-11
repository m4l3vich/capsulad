// capsulad is the Capsula Universal Framework daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"capsulad/internal/daemon"
	"capsulad/internal/sandbox"
	"capsulad/internal/version"
)

func main() {
	ignoreHangup()
	if handled, code := sandbox.HandleHelper(os.Args); handled {
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == daemon.WatchdogFeederArg {
		os.Exit(daemon.RunWatchdogFeeder())
	}
	cfg := daemon.Config{}
	fs := flag.NewFlagSet("capsulad", flag.ExitOnError)
	fs.StringVar(&cfg.Platform, "platform", "simulator", "target platform: simulator, rockchip-stock, amlogic-stock, amlogic-alpine")
	fs.StringVar(&cfg.DataDir, "data-dir", "", "durable state directory (default .capsula; production: /var/lib/capsulad)")
	fs.StringVar(&cfg.RunDir, "run-dir", "", "runtime directory (default <data-dir>/run)")
	fs.StringVar(&cfg.SocketPath, "socket", "", "framework Unix socket (default <run-dir>/hal.sock)")
	fs.StringVar(&cfg.WebUIAddr, "web-addr", "127.0.0.1:8443", "HTTPS Web UI listen address; empty disables it")
	fs.StringVar(&cfg.AppsDir, "apps-dir", "", "installed applications root (default <data-dir>/apps)")
	fs.StringVar(&cfg.AssetsDir, "assets-dir", "", "shared read-only assets root (default <data-dir>/assets)")
	fs.StringVar(&cfg.SandboxMode, "sandbox", "auto", "sandbox mode: auto, namespaces, mountns, uid, none")
	fs.IntVar(&cfg.SandboxUIDBase, "sandbox-uid-base", 20000, "first host UID assigned to sandboxed apps")
	fs.StringVar(&cfg.TrustedRootPath, "trusted-root", "", "offline Sigstore trusted_root.json for provenance verification")
	fs.StringVar(&cfg.LogFile, "log-file", "", "daemon log file (default <data-dir>/capsulad.log)")
	fs.StringVar(&cfg.DevAdminSocket, "dev-admin", "", "development-only admin Unix socket (simulator); empty disables")
	fs.IntVar(&cfg.MemLimitMB, "mem-limit-mb", 80, "soft Go heap limit in MiB (0 disables); keeps the OOM killer away on a 240 MiB board")
	fs.StringVar(&cfg.WakewordModel, "wakeword-model", "", "trained keyword classifier (default <data-dir>/wakeword/model.json)")
	fs.StringVar(&cfg.WakewordNegatives, "wakeword-negatives", "", "negative-training corpus (default <data-dir>/wakeword/negatives.bin; training input only)")
	fs.StringVar(&cfg.Supersede, "supersede", "auto", "vendor processes to replace: auto, off, or a comma-separated list")
	fs.BoolVar(&cfg.WakewordTrace, "wakeword-trace", false, "log the wakeword detector score twice a second")
	fs.StringVar(&cfg.Watchdog, "watchdog", "auto", "hardware watchdog owner: auto (arm /dev/watchdog when present), off, or an explicit device path")
	showVersion := fs.Bool("version", false, "print version and exit")
	health := fs.Bool("health", false, "check a running daemon and exit 0 when it serves the framework API")
	fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Printf("capsulad %s (draft %s, sdk %s)\n", version.Version, version.APIDraft, version.SDKVersion)
		return
	}
	if *health {
		os.Exit(checkHealth(cfg))
	}
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
