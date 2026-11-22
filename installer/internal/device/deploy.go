package device

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// RemoteInstallDir is the tmpfs staging area the installer owns on the device.
// The deploy script, the pushed CLI, the bundle and its log live here.
const RemoteInstallDir = "/tmp/capsulad-installer"

// DeployTimeout bounds the deployer's own health gate. The script passes it to
// `capsulad-cli deploy -timeout`.
const DeployTimeout = 180 * time.Second

// DeployPlan holds the exact remote paths one deployment uses.
type DeployPlan struct {
	RemoteDir  string
	ScriptPath string
	LogPath    string
	CLIPath    string
	BundlePath string
	DataDir    string
	Platform   Platform
	Timeout    time.Duration
	Jingle     bool
}

// NewDeployPlan fills the conventional layout.
func NewDeployPlan(dataDir string, platform Platform, bundleName string, jingle bool) DeployPlan {
	if bundleName == "" {
		bundleName = "deploy-bundle.tar.gz"
	}
	return DeployPlan{
		RemoteDir:  RemoteInstallDir,
		ScriptPath: path.Join(RemoteInstallDir, "deploy.sh"),
		LogPath:    path.Join(RemoteInstallDir, "deploy.log"),
		CLIPath:    path.Join(RemoteInstallDir, "capsulad-cli"),
		BundlePath: path.Join(RemoteInstallDir, bundleName),
		DataDir:    dataDir,
		Platform:   platform,
		Timeout:    DeployTimeout,
		Jingle:     jingle,
	}
}

// DeployScript is the detached deployment: the stock daemon owns Wi-Fi, so the
// transport dies while the daemon stops and restarts. The script keeps the
// process alive, writes the exact result to deploy.log and keeps umask 022 so
// app-visible directory components stay traversable.
//
// No secret appears here; the deployment has none.
func DeployScript(p DeployPlan) string {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DeployTimeout
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Written by capsulad-installer. Detached because the daemon owns Wi-Fi.\n")
	b.WriteString("umask 022\n")
	fmt.Fprintf(&b, "%s -data-dir %s deploy %s -platform %s -staging /run -timeout %ds > %s 2>&1\n",
		ShellQuote(p.CLIPath), ShellQuote(p.DataDir), ShellQuote(p.BundlePath),
		ShellQuote(string(p.Platform)), int(timeout.Seconds()), ShellQuote(p.LogPath))
	fmt.Fprintf(&b, "echo exit=$? >> %s\n", ShellQuote(p.LogPath))
	return b.String()
}

// DetachedLaunch returns the launch attempts in order. The first uses the
// image's init helper; the second is the portable fallback.
func DetachedLaunch(script string) [][]string {
	return [][]string{
		{"start-stop-daemon", "-S", "-b", "-x", script},
		{"setsid", "sh", script},
	}
}

// StartupMarker returns the command that writes or removes
// <data>/startup-sound.played. The daemon plays its one-shot cue on the first
// boot only; writing the marker before the deploy keeps the jingle off.
//
// The parent is created first: a fresh image may not have the data directory
// until the deploy runs, and a silently failed touch would turn the jingle on.
func StartupMarker(p DeployPlan) Command {
	marker := path.Join(p.DataDir, "startup-sound.played")
	if p.Jingle {
		return Command{Args: []string{"rm", "-f", marker}}
	}
	script := "mkdir -p " + ShellQuote(p.DataDir) + " && touch " + ShellQuote(marker)
	return Command{Args: []string{"sh", "-c", script}}
}

// ParseDeployExit finds the final `exit=N` line written by DeployScript.
func ParseDeployExit(log string) (int, bool) {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "exit=") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(line, "exit="))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

// LogTail returns the last n non-empty lines, for an error report.
func LogTail(log string, n int) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// RecoveryAdvice is the unreachable-device guidance, parameterised by the
// setup AP details when the Wi-Fi status exposed them.
func RecoveryAdvice(address, apSSID, apHint string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The device stopped answering at %s. The daemon owns the Wi-Fi interface, so the link drops while it restarts.\n", address)
	b.WriteString("Wait for the device to come back on the same IP, or connect to its setup access point:\n")
	if apSSID != "" {
		fmt.Fprintf(&b, "  network: %s\n", apSSID)
	}
	if apHint != "" {
		fmt.Fprintf(&b, "  password hint: %s\n", apHint)
	} else {
		b.WriteString("  password: see the device label or your saved Wi-Fi password\n")
	}
	b.WriteString("  portal: http://192.168.4.1\n")
	b.WriteString("Then re-enter the device address and retry.")
	return b.String()
}
