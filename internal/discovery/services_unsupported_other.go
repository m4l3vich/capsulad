//go:build !linux

package discovery

import "capsulad/internal/logbus"

func openServiceBackend(_ *logbus.Bus, _ string) serviceBackend {
	return unsupportedServices{reason: "mDNS service discovery needs Avahi on the device; this build has no responder"}
}
