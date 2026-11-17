package hw

import "strconv"

// HostPort renders a broker address as "host:port". A host that already contains
// a colon is an IPv6 literal and is bracketed, so the string stays unambiguous.
//
// It mirrors net.JoinHostPort deliberately: importing the net package links its
// resolver, interface and poll machinery into a sandboxed appliance app whose
// only networking happens inside the shared SDK, which is a real cost for a
// single formatting call.
func HostPort(host string, port int) string {
	for i := 0; i < len(host); i++ {
		if host[i] == ':' {
			return "[" + host + "]:" + strconv.Itoa(port)
		}
	}
	return host + ":" + strconv.Itoa(port)
}
