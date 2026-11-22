package device

import "fmt"

// Platform is the firmware/board combination the daemon is deployed for. The
// string value is exactly what `capsulad-cli deploy -platform` accepts.
type Platform string

const (
	PlatformRockchipStock Platform = "rockchip-stock"
	PlatformAmlogicStock  Platform = "amlogic-stock"
	PlatformAmlogicAlpine Platform = "amlogic-alpine"
)

// Firmware is the user-facing firmware choice; the board is discovered.
type Firmware string

const (
	FirmwareStock  Firmware = "stock"
	FirmwareAlpine Firmware = "alpine"
)

// DataDir is the framework data directory for this platform.
func (p Platform) DataDir() string {
	switch p {
	case PlatformRockchipStock:
		return "/data/capsdemo"
	case PlatformAmlogicStock:
		return "/data/local/capsulad"
	case PlatformAmlogicAlpine:
		return "/var/lib/capsulad"
	}
	return ""
}

// Stock reports whether the platform is a jailbroken vendor image.
func (p Platform) Stock() bool {
	return p == PlatformRockchipStock || p == PlatformAmlogicStock
}

// Valid reports whether p is a supported target.
func (p Platform) Valid() bool {
	switch p {
	case PlatformRockchipStock, PlatformAmlogicStock, PlatformAmlogicAlpine:
		return true
	}
	return false
}

// Label is the human name shown in the installer.
func (p Platform) Label() string {
	switch p {
	case PlatformRockchipStock:
		return "Jailbroken stock (Rockchip RK3308)"
	case PlatformAmlogicStock:
		return "Jailbroken stock (Amlogic A113X)"
	case PlatformAmlogicAlpine:
		return "Alpine Linux (Amlogic A113X)"
	}
	return string(p)
}

// InitScript is the boot hook the deployer installs.
func (p Platform) InitScript() string {
	switch p {
	case PlatformAmlogicStock:
		return "/etc/init.d/S99sysmon.sh"
	case PlatformAmlogicAlpine:
		return "/etc/init.d/capsulad"
	default:
		return "/etc/init.d/S99btctl.sh"
	}
}

// Rollback is the exact command that returns boot ownership to the image.
// Retiring the hook is the rollback: no vendor file is deleted.
func (p Platform) Rollback() string {
	switch p {
	case PlatformAmlogicStock:
		return "/etc/init.d/S99sysmon.sh disable"
	case PlatformAmlogicAlpine:
		return "rc-service capsulad stop && rc-update del capsulad"
	default:
		return "/etc/init.d/S99btctl.sh disable"
	}
}

// SessionFile is the stock voice-agent session file Marusia can migrate.
func (p Platform) SessionFile() string {
	switch p {
	case PlatformRockchipStock, PlatformAmlogicStock:
		return "/data/voice-agent/session.json"
	}
	return ""
}

// CheckFirmware refuses a selection that contradicts the discovered platform.
func CheckFirmware(selected Firmware, detected Platform) error {
	switch selected {
	case FirmwareAlpine:
		if detected != PlatformAmlogicAlpine {
			return fmt.Errorf("you selected Alpine Linux, but the device reports %s (%s); go back and choose jailbroken stock", detected.Label(), detected)
		}
	case FirmwareStock:
		if !detected.Stock() {
			return fmt.Errorf("you selected jailbroken stock, but the device reports %s (%s); go back and choose Alpine Linux", detected.Label(), detected)
		}
	default:
		return fmt.Errorf("unknown firmware selection %q", selected)
	}
	return nil
}
