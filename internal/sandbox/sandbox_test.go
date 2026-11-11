package sandbox

import (
	"strings"
	"testing"
)

func TestSandboxSocketEnvUsesInNamespacePath(t *testing.T) {
	const host = "/data/capsulad/run/hal.sock"
	for _, mode := range []string{"namespaces", "mountns"} {
		if got := sandboxSocketEnv(mode, host); got != sandboxSocketPath {
			t.Fatalf("mode %s: socket env = %q, want %q (host path does not resolve inside the mount namespace)", mode, got, sandboxSocketPath)
		}
	}
	for _, mode := range []string{"uid", "none"} {
		if got := sandboxSocketEnv(mode, host); got != host {
			t.Fatalf("mode %s: socket env = %q, want host path %q", mode, got, host)
		}
	}
}

func TestBaseEnvDoesNotForceUTC(t *testing.T) {
	t.Setenv("TZ", "")
	for _, entry := range baseEnv() {
		if strings.HasPrefix(entry, "TZ=") {
			t.Fatalf("baseEnv forces %q; sandboxed apps must read the device timezone", entry)
		}
	}
	t.Setenv("TZ", "Europe/Moscow")
	found := false
	for _, entry := range baseEnv() {
		if entry == "TZ=Europe/Moscow" {
			found = true
		}
	}
	if !found {
		t.Fatal("baseEnv does not propagate the daemon TZ")
	}
}
