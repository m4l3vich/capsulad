//go:build !linux

package discovery

func openBackend(string) (backend, Status) {
	return unsupported("mDNS advertising needs Avahi on the device; this build has no responder")
}
