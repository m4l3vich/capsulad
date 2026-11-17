package hw

import "testing"

func TestHostPortBracketsIPv6(t *testing.T) {
	cases := map[string]string{
		"broker.local": "broker.local:1883",
		"10.0.0.5":     "10.0.0.5:1883",
		"::1":          "[::1]:1883",
		"fe80::1":      "[fe80::1]:8883",
	}
	for host, want := range cases {
		port := 1883
		if want[len(want)-4:] == "8883" {
			port = 8883
		}
		if got := HostPort(host, port); got != want {
			t.Fatalf("HostPort(%q, %d) = %q, want %q", host, port, got, want)
		}
	}
}
