// Package install builds the device commands for one Capsula Mini installation
// and drives them through a device.Device. Every command constructor here is
// pure: it returns an argv, so tests assert exactly what the device would run.
package install

import (
	"capsulad/installer/internal/device"
)

// DeviceCLI is the pushed device CLI. Global flags come before the subcommand.
func DeviceCLI(cli, dataDir string, args ...string) []string {
	out := []string{cli, "-data-dir", dataDir}
	return append(out, args...)
}

// AuthInitArgs creates the server certificate and admin password. The password
// arrives on stdin: `-password-stdin` never puts it in argv.
func AuthInitArgs(cli, dataDir, hostname, ip string) []string {
	return DeviceCLI(cli, dataDir, "auth", "init", "-password-stdin", "-hostname", hostname, "-ip", ip)
}

// AuthSetPasswordArgs changes an existing password from stdin.
func AuthSetPasswordArgs(cli, dataDir string) []string {
	return DeviceCLI(cli, dataDir, "auth", "set-password", "-password-stdin")
}

// AuthStatusArgs reads whether an admin password is configured.
func AuthStatusArgs(cli, dataDir string) []string {
	return DeviceCLI(cli, dataDir, "auth", "status")
}

// SettingsStdinArgs sets one setting from stdin using the CLI's KEY@- form.
// The key name is the only value in argv; the value is one line on stdin.
func SettingsStdinArgs(cli, dataDir, appID, key string) []string {
	return DeviceCLI(cli, dataDir, "settings", "app", appID, key+"@-")
}

// AppListArgs lists installed apps. The output is one line per app and is not
// JSON-formatted; the first field is the app ID.
func AppListArgs(cli, dataDir string) []string {
	return DeviceCLI(cli, dataDir, "app", "list")
}

// AppInstallArgs installs a pushed package.
func AppInstallArgs(cli, dataDir, remotePath string) []string {
	return DeviceCLI(cli, dataDir, "app", "install", remotePath)
}

// AppRunsArgs inspects a single app's runs.
func AppRunsArgs(cli, dataDir, appID string) []string {
	return DeviceCLI(cli, dataDir, "app", "runs", appID)
}

// WifiStatusArgs reads Wi-Fi state as JSON.
func WifiStatusArgs(cli, dataDir, webURL string) []string {
	return DeviceCLI(cli, dataDir, "-json", "wifi", "status", "-web-url", webURL)
}

// WifiScanArgs scans for networks as JSON.
func WifiScanArgs(cli, dataDir, webURL string) []string {
	return DeviceCLI(cli, dataDir, "-json", "wifi", "scan", "-web-url", webURL)
}

// WifiConnectArgs connects to SSID. The passphrase arrives on stdin only for a
// secured network; `-security` is passed explicitly for an open network so the
// backend cannot fall back to its WPA2 default and demand a passphrase.
func WifiConnectArgs(cli, dataDir, webURL, ssid, security string, passwordStdin bool) []string {
	args := []string{cli, "-data-dir", dataDir, "-json", "wifi", "connect", ssid}
	if passwordStdin {
		args = append(args, "-password-stdin")
	}
	if security != "" {
		args = append(args, "-security", security)
	}
	return append(args, "-web-url", webURL)
}

// ChmodArgs marks a pushed file executable.
func ChmodArgs(mode, path string) []string {
	return []string{"chmod", mode, path}
}

// EnsureStagingArgs creates or validates the installer's remote staging
// directory. It must be a real directory owned by the current (root) user and
// mode 0700; a symlink or foreign-owned directory is refused before any
// artifact or secret is written there. Every refusal names the failed check
// and the observed value, because the host only sees this output when the
// device-side step exits non-zero.
func EnsureStagingArgs(dir string) []string {
	d := device.ShellQuote(dir)
	script := "set -e\n" +
		"fail() { echo \"capsulad-installer: $*\" >&2; exit 1; }\n" +
		// Some stock BusyBox builds ship no `id`/`whoami`; /proc/self/status
		// carries the effective uid using shell builtins only.
		"current_uid() {\n" +
		"  id -u 2>/dev/null && return 0\n" +
		"  [ -r /proc/self/status ] || return 1\n" +
		"  while read -r key value; do\n" +
		"    if [ \"$key\" = \"Uid:\" ]; then\n" +
		"      set -- $value\n" +
		"      if [ -n \"${2:-}\" ]; then echo \"$2\"; return 0; fi\n" +
		"      if [ -n \"${1:-}\" ]; then echo \"$1\"; return 0; fi\n" +
		"      return 1\n" +
		"    fi\n" +
		"  done < /proc/self/status\n" +
		"  return 1\n" +
		"}\n" +
		"u=$(current_uid) || u=\n" +
		"if [ -z \"$u\" ]; then\n" +
		"  [ -O / ] || fail \"the device shell must be root (cannot read the uid and / is not owned by this user)\"\n" +
		"elif [ \"$u\" != \"0\" ]; then\n" +
		"  fail \"the device shell must be root (uid $u)\"\n" +
		"fi\n" +
		"d=" + d + "\n" +
		"if [ -L \"$d\" ]; then fail \"$d is a symlink\"; fi\n" +
		"if [ -e \"$d\" ] && [ ! -d \"$d\" ]; then fail \"$d exists and is not a directory\"; fi\n" +
		"mkdir -p \"$d\" || fail \"cannot create $d\"\n" +
		"chmod 0700 \"$d\" || fail \"cannot chmod 0700 $d\"\n" +
		"[ -O \"$d\" ] || fail \"$d is not owned by the current user: $(ls -ld \"$d\" 2>/dev/null)\"\n" +
		"[ -d \"$d\" ] && [ ! -L \"$d\" ] || fail \"$d is not a plain directory after creation\"\n" +
		"a=\"$d/apps\"\n" +
		"if [ -L \"$a\" ]; then fail \"$a is a symlink\"; fi\n" +
		"mkdir -p \"$a\" || fail \"cannot create $a\"\n" +
		"chmod 0700 \"$a\" || fail \"cannot chmod 0700 $a\"\n" +
		"[ -O \"$a\" ] || fail \"$a is not owned by the current user: $(ls -ld \"$a\" 2>/dev/null)\"\n" +
		"[ -d \"$a\" ] && [ ! -L \"$a\" ] || fail \"$a is not a plain directory after creation\"\n"
	return []string{"sh", "-c", script}
}
