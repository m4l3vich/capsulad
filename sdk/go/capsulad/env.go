package capsulad

import "os"

// DeviceSerialEnv names the launcher's descriptor-free SoC serial variable.
const DeviceSerialEnv = "CAPSULAD_DEVICE_SERIAL"

// platformLibraryName returns the lazily loaded platform shared-library name.
func platformLibraryName() string {
	return soname
}

// FromEnv reads the launcher-assigned identity and library override.
func FromEnv() Env {
	return Env{
		Library:      os.Getenv("CAPSULAD_LIBRARY"),
		Socket:       envOr("CAPSULAD_SOCKET", "/run/capsulad/hal.sock"),
		RunID:        os.Getenv("CAPSULAD_RUN_ID"),
		AppID:        os.Getenv("CAPSULAD_APP_ID"),
		SDK:          os.Getenv("CAPSULAD_SDK"),
		DeviceSerial: os.Getenv(DeviceSerialEnv),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
