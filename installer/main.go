// Command capsulad-installer installs capsulad on a Capsula Mini over ADB
// (jailbroken stock) or SSH (Alpine).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"capsulad/installer/internal/install"
	"capsulad/installer/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		artifacts  = flag.String("artifacts", "", "offline release tree (release.json or discoverable platform directories)")
		releaseURL = flag.String("release-url", "", "core release download base URL (default: latest GitHub release)")
		releaseTag = flag.String("release-tag", "", "GitHub core release tag (overridden by -release-url)")
		appsURL    = flag.String("apps-url", "", "app channel download base URL (default: the rolling apps release)")
		appsTag    = flag.String("apps-tag", "", "GitHub app channel tag (overridden by -apps-url)")
		adbPath    = flag.String("adb", "adb", "path to the adb binary")
		sshKey     = flag.String("ssh-key", "", "SSH private key path (prefills the Alpine key field)")
		configDir  = flag.String("config-dir", "", "state directory (default: <user config>/capsulad-installer)")
		cacheDir   = flag.String("cache-dir", "", "download cache (default: <user cache>/capsulad-installer)")
		printVer   = flag.Bool("version", false, "print the installer version and exit")
	)
	flag.Parse()

	if *printVer {
		fmt.Printf("capsulad-installer %s\n", version)
		return
	}
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "capsulad-installer: unexpected arguments: %v\n", flag.Args())
		os.Exit(2)
	}
	if *artifacts != "" {
		info, err := os.Stat(*artifacts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "capsulad-installer: -artifacts: %v\n", err)
			os.Exit(2)
		}
		if !info.IsDir() {
			fmt.Fprintf(os.Stderr, "capsulad-installer: -artifacts is not a directory: %s\n", *artifacts)
			os.Exit(2)
		}
	}
	if *releaseURL != "" && *releaseTag != "" {
		fmt.Fprintln(os.Stderr, "capsulad-installer: -release-url and -release-tag are mutually exclusive")
		os.Exit(2)
	}
	if *appsURL != "" && *appsTag != "" {
		fmt.Fprintln(os.Stderr, "capsulad-installer: -apps-url and -apps-tag are mutually exclusive")
		os.Exit(2)
	}

	cfg := *configDir
	if cfg == "" {
		var err error
		cfg, err = install.DefaultConfigDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "capsulad-installer: %v\n", err)
			os.Exit(1)
		}
	}
	cache := *cacheDir
	if cache == "" {
		var err error
		cache, err = install.DefaultCacheDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "capsulad-installer: %v\n", err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := ui.Run(ctx, ui.Options{
		Version:      version,
		ArtifactsDir: *artifacts,
		ReleaseURL:   *releaseURL,
		ReleaseTag:   *releaseTag,
		AppsURL:      *appsURL,
		AppsTag:      *appsTag,
		ADBPath:      *adbPath,
		SSHKey:       *sshKey,
		ConfigDir:    cfg,
		CacheDir:     cache,
	})
	if err != nil {
		if errors.Is(err, ui.ErrAborted) {
			fmt.Fprintln(os.Stderr, "capsulad-installer: stopped by the user; the current step was cancelled.")
			if errors.Is(err, ui.ErrAbortedDuringDeploy) {
				fmt.Fprintln(os.Stderr, "The device-side deploy is detached; reconnect and read /tmp/capsulad-installer/deploy.log, then retry or run /etc/init.d/S99btctl.sh start (stock) / rc-service capsulad start (Alpine).")
			}
		} else {
			fmt.Fprintf(os.Stderr, "capsulad-installer: %v\n", err)
		}
		os.Exit(1)
	}
}
