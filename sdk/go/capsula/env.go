package capsula

import "os"

// platformLibraryName returns the lazily loaded platform shared-library name.
func platformLibraryName() string {
	return soname
}

// FromEnv reads the launcher-assigned identity and library override.
func FromEnv() Env {
	return Env{
		Library: os.Getenv("CAPSULA_LIBRARY"),
		Socket:  envOr("CAPSULA_SOCKET", "/run/capsulad/hal.sock"),
		RunID:   os.Getenv("CAPSULA_RUN_ID"),
		AppID:   os.Getenv("CAPSULA_APP_ID"),
		SDK:     os.Getenv("CAPSULA_SDK"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
