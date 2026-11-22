package device

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Detection is what the device says about itself.
type Detection struct {
	OSRelease  string
	Compatible string
	Arch       string
	Platform   Platform
}

// ReadText runs a simple reader and returns its trimmed output, capped at
// MaxTextRead.
func ReadText(ctx context.Context, dev Device, path string) (string, error) {
	res, err := dev.Run(ctx, Command{Args: []string{"cat", path}, MaxStdout: MaxTextRead})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("read %s: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// Detect reads /etc/os-release, the device-tree compatible string and the
// architecture, then maps them to a deployment platform.
func Detect(ctx context.Context, dev Device) (Detection, error) {
	var d Detection
	osr, err := ReadText(ctx, dev, "/etc/os-release")
	if err != nil {
		return d, fmt.Errorf("identify firmware: %w (is this a Capsula Mini image?)", err)
	}
	compat, err := ReadText(ctx, dev, "/proc/device-tree/compatible")
	if err != nil {
		return d, fmt.Errorf("identify board: %w", err)
	}
	var arch string
	if res, err := dev.Run(ctx, Command{Args: []string{"uname", "-m"}, MaxStdout: MaxSessionRead}); err == nil && res.ExitCode == 0 {
		arch = strings.TrimSpace(string(res.Stdout))
	}
	d.OSRelease = osr
	d.Compatible = strings.ReplaceAll(compat, "\x00", " ")
	d.Compatible = strings.TrimSpace(d.Compatible)
	d.Arch = arch
	p, err := MapPlatform(d.OSRelease, d.Compatible)
	if err != nil {
		return d, err
	}
	d.Platform = p
	return d, nil
}

// MapPlatform is the pure firmware mapping. It refuses an unsupported board
// rather than guessing a data directory or boot hook.
func MapPlatform(osRelease, compatible string) (Platform, error) {
	osr := strings.ToLower(osRelease)
	compat := strings.ToLower(compatible)
	alpine := osReleaseField(osr, "id") == "alpine" || strings.Contains(osr, "alpine")
	switch {
	case strings.Contains(compat, "rockchip") || strings.Contains(compat, "rk3308"):
		if alpine {
			return "", fmt.Errorf("device reports a Rockchip board running Alpine; only the jailbroken stock image is supported on RK3308")
		}
		return PlatformRockchipStock, nil
	case strings.Contains(compat, "amlogic") || strings.Contains(compat, "a113x") || strings.Contains(compat, "meson"):
		if alpine {
			return PlatformAmlogicAlpine, nil
		}
		return PlatformAmlogicStock, nil
	case compat == "":
		return "", errors.New("empty device-tree compatible string; this does not look like a Capsula Mini")
	default:
		return "", fmt.Errorf("unsupported Capsula Mini board %q (expected Rockchip RK3308 or Amlogic A113X)", compatible)
	}
}

func osReleaseField(osr, key string) string {
	for _, line := range strings.Split(osr, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.EqualFold(k, key) {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// ConnectAndDetect is the single pre-change step: attach, identify, map.
func ConnectAndDetect(ctx context.Context, dev Device) (Detection, error) {
	if err := dev.Connect(ctx); err != nil {
		return Detection{}, err
	}
	return Detect(ctx, dev)
}
