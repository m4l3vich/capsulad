package device

import (
	"context"
	"strings"
)

// Identity is the best-effort, pre-deploy identity of a stock device. ADB
// itself does not authenticate the peer, so this is a continuity check across a
// reconnect, not a cryptographic attestation.
type Identity struct {
	Serial   string
	Source   string
	Hostname string
}

// Available reports whether a stable serial was found. Without one the caller
// must warn and require an explicit acknowledgement.
func (id Identity) Available() bool { return id.Serial != "" }

// String is the human-readable confirmation value.
func (id Identity) String() string {
	if id.Serial == "" {
		if id.Hostname != "" {
			return "serial unavailable; hostname " + id.Hostname
		}
		return "serial unavailable"
	}
	out := id.Serial
	if id.Source != "" {
		out += " (" + id.Source + ")"
	}
	if id.Hostname != "" {
		out += " · " + id.Hostname
	}
	return out
}

// ReadIdentity tries the device-tree serial, Android's ro.serialno prop, and
// the vendor devinfo command, then adds the hostname. Missing identity is not
// an error unless every read also failed at the transport level.
func ReadIdentity(ctx context.Context, dev Device) (Identity, error) {
	var id Identity
	sources := []struct {
		name string
		argv []string
	}{
		{"device-tree", []string{"cat", "/proc/device-tree/serial-number"}},
		{"getprop", []string{"getprop", "ro.serialno"}},
		{"devinfo", []string{"devinfo"}},
	}
	var lastErr error
	for _, src := range sources {
		res, err := dev.Run(ctx, Command{Args: src.argv, MaxStdout: MaxSessionRead})
		if err != nil {
			lastErr = err
			continue
		}
		if res.ExitCode != 0 {
			continue
		}
		if value := identityFromSource(src.name, string(res.Stdout)); value != "" &&
			!strings.EqualFold(value, "unknown") && value != "0" {
			id.Serial = value
			id.Source = src.name
			break
		}
	}
	if res, err := dev.Run(ctx, Command{Args: []string{"hostname"}, MaxStdout: MaxSessionRead}); err == nil && res.ExitCode == 0 {
		id.Hostname = identityValue(string(res.Stdout))
	}
	if id.Serial == "" && id.Hostname == "" && lastErr != nil {
		return id, lastErr
	}
	return id, nil
}

// identityFromSource extracts a serial from one source. `devinfo` prints
// KEY=VALUE diagnostics, with SERIAL and DEVICE_ID as the useful keys.
func identityFromSource(source, raw string) string {
	if source != "devinfo" {
		return identityValue(raw)
	}
	var serial, deviceID string
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "SERIAL":
			serial = strings.TrimSpace(value)
		case "DEVICE_ID":
			deviceID = strings.TrimSpace(value)
		}
	}
	if serial != "" {
		return serial
	}
	return deviceID
}

// identityValue trims NUL padding (device-tree strings) and returns the first
// non-empty line.
func identityValue(raw string) string {
	raw = strings.ReplaceAll(raw, "\x00", "")
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
