package appcontrol

import (
	"net"
	"strings"
	"testing"
)

// TestMulticastInterfaceSelection pins the interface rule: up, multicast
// capable, never loopback.
func TestMulticastInterfaceSelection(t *testing.T) {
	interfaces := []net.Interface{
		{Name: "lo0", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast},
		{Name: "down0", Flags: net.FlagMulticast},
		{Name: "eth0", Flags: net.FlagUp | net.FlagMulticast | net.FlagBroadcast},
		{Name: "ppp0", Flags: net.FlagUp | net.FlagPointToPoint},
	}
	selected := selectMulticastInterfaces(interfaces)
	if len(selected) != 1 || selected[0].Name != "eth0" {
		t.Fatalf("selected %v, want only eth0", selected)
	}
	if got := selectMulticastInterfaces(nil); len(got) != 0 {
		t.Fatalf("nil input selected %v", got)
	}
	// The host's own list must never contribute loopback or a down interface.
	for _, iface := range selectMulticastInterfaces(mustInterfaces()) {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			t.Fatalf("host selection included %s (%v)", iface.Name, iface.Flags)
		}
	}
}

// TestJoinGroupPolicy pins the narrow multicast rule: a group join is a
// receive-only UDP socket on a PTP port with the wildcard bind, and nothing
// else may be requested through it.
func TestJoinGroupPolicy(t *testing.T) {
	channel := &runChannel{manager: New(nil, Policy{})}
	tests := []struct {
		name    string
		request listenRequest
		wantErr string
		want    string
	}{
		{
			name:    "ptp general port",
			request: listenRequest{Op: "listen", Protocol: "udp", Bind: "0.0.0.0", Port: 320, JoinGroup: "224.0.0.1"},
			want:    "224.0.0.1",
		},
		{
			name:    "ptp event port",
			request: listenRequest{Op: "listen", Protocol: "udp", Bind: "", Port: 319, JoinGroup: "224.0.0.1"},
			want:    "224.0.0.1",
		},
		{
			name:    "tcp group",
			request: listenRequest{Op: "listen", Protocol: "tcp", Port: 320, JoinGroup: "224.0.0.1"},
			wantErr: `requires protocol "udp"`,
		},
		{
			name:    "ordinary port",
			request: listenRequest{Op: "listen", Protocol: "udp", Port: 7000, JoinGroup: "224.0.0.1"},
			wantErr: "only allowed on the PTP ports",
		},
		{
			name:    "unicast bind",
			request: listenRequest{Op: "listen", Protocol: "udp", Bind: "127.0.0.1", Port: 320, JoinGroup: "224.0.0.1"},
			wantErr: "wildcard bind",
		},
		{
			name:    "not multicast",
			request: listenRequest{Op: "listen", Protocol: "udp", Bind: "0.0.0.0", Port: 320, JoinGroup: "192.0.2.1"},
			wantErr: "not an IPv4 multicast group",
		},
		{
			name:    "ipv6 group",
			request: listenRequest{Op: "listen", Protocol: "udp", Bind: "0.0.0.0", Port: 320, JoinGroup: "ff02::1"},
			wantErr: "not an IPv4 multicast group",
		},
		{
			name:    "a different multicast group",
			request: listenRequest{Op: "listen", Protocol: "udp", Bind: "0.0.0.0", Port: 320, JoinGroup: "239.1.2.3"},
			wantErr: "not the AirPlay PTP group",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			protocol, _, port, backlog, group, err := channel.validate(tc.request)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("validate() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate(): %v", err)
			}
			if protocol != "udp" || backlog != 0 {
				t.Fatalf("protocol/backlog = %q/%d", protocol, backlog)
			}
			if port != tc.request.Port {
				t.Fatalf("port = %d, want %d", port, tc.request.Port)
			}
			if group == nil || !group.Equal(net.ParseIP(tc.want)) {
				t.Fatalf("group = %v, want %s", group, tc.want)
			}
		})
	}
}
