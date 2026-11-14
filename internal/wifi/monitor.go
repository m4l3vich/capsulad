package wifi

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"time"
)

// eventMonitor is the wpa_supplicant control-interface event stream. It uses
// the control socket's ATTACH protocol directly so no polling or helper script
// is needed; the manager reconvenes one coalesced snapshot per burst.
type eventMonitor struct {
	conn *net.UnixConn
	done chan struct{}
}

var supplicantEvents = []string{
	"CTRL-EVENT-CONNECTED",
	"CTRL-EVENT-DISCONNECTED",
	"CTRL-EVENT-STATE-CHANGE",
	"CTRL-EVENT-ASSOC-REJECT",
	"CTRL-EVENT-SSID-TEMP-DISABLED",
	"CTRL-EVENT-NETWORK-NOT-FOUND",
	"CTRL-EVENT-TERMINATING",
	"CTRL-EVENT-SCAN-RESULTS",
}

func relevantSupplicantEvent(text string) bool {
	for _, prefix := range supplicantEvents {
		if strings.Contains(text, prefix) {
			return true
		}
	}
	return false
}

func (m *linuxManager) startSupplicantMonitor() {
	m.stopMonitor()
	path := filepath.Join(m.ctrlDir(), m.iface)
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte("ATTACH")); err != nil {
		_ = conn.Close()
		return
	}
	mon := &eventMonitor{conn: conn, done: make(chan struct{})}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = conn.Close()
		return
	}
	m.monitor = mon
	m.mu.Unlock()
	go func() {
		select {
		case <-m.ctx.Done():
			_ = conn.Close()
		case <-mon.done:
		}
	}()
	go m.readSupplicantEvents(mon)
}

func (m *linuxManager) readSupplicantEvents(mon *eventMonitor) {
	defer close(mon.done)
	buf := make([]byte, 4096)
	for {
		n, err := mon.conn.Read(buf)
		if err != nil {
			m.wakeReconcile()
			return
		}
		if n == 0 {
			continue
		}
		if relevantSupplicantEvent(string(buf[:n])) {
			m.wakeReconcile()
		}
	}
}

func (m *linuxManager) stopMonitor() {
	m.mu.Lock()
	mon := m.monitor
	m.monitor = nil
	m.mu.Unlock()
	if mon == nil {
		return
	}
	_ = mon.conn.Close()
	select {
	case <-mon.done:
	case <-time.After(time.Second):
	}
}

func (m *linuxManager) wakeReconcile() {
	select {
	case m.eventWake <- struct{}{}:
	default:
	}
}

func (m *linuxManager) runReconciler() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.eventWake:
			m.reconcile(context.Background())
		}
	}
}

// reconcile copies a live supplicant observation into the authoritative
// snapshot. It bumps the revision and publishes only when observable fields
// actually change, so a page, known-network list and explicit scan all agree
// after a drop, roam or DHCP change.
func (m *linuxManager) reconcile(ctx context.Context) {
	m.reconcileMu.Lock()
	changed := false
	defer func() {
		m.reconcileMu.Unlock()
		if changed {
			m.notify()
		}
	}()

	m.mu.Lock()
	if m.closed || m.ap != nil || m.station == nil {
		m.mu.Unlock()
		return
	}
	station := m.station
	connecting := m.connecting
	before := m.status
	m.mu.Unlock()

	live, err := m.queryStatus(ctx)
	if ctx.Err() != nil {
		return
	}
	ip := ""
	if err == nil && live.State == "COMPLETED" {
		ip = m.queryIP(ctx)
	}
	if ctx.Err() != nil {
		return
	}
	security := ""
	if live.SSID != "" {
		security = m.securityFor(live.SSID)
	}

	m.mu.Lock()
	if m.closed || m.ap != nil || m.station != station {
		m.mu.Unlock()
		return
	}
	next := m.status
	switch {
	case err != nil && !station.running():
		next.State = StateError
		next.SSID = ""
		next.Security = ""
		next.IPAddress = ""
		next.Detail = "wpa_supplicant is no longer running"
	case err != nil:
		next.State = StateError
		next.Detail = "could not read the supplicant state"
	case live.State == "COMPLETED":
		next.State = StateConnected
		next.SSID = live.SSID
		next.Security = security
		next.IPAddress = ip
		if ip == "" {
			next.Detail = "associated; waiting for an IPv4 lease"
		} else {
			next.Detail = "connected"
		}
	case connecting:
		next.State = StateConnecting
	default:
		next.State = StateDisconnected
		next.SSID = ""
		next.Security = ""
		next.IPAddress = ""
		next.Detail = "not connected to a Wi-Fi network"
	}
	changed = statusChanged(before, next)
	if changed {
		next.Revision = m.revision + 1
		m.revision++
		m.status = next
	} else {
		next.Revision = m.revision
	}
	m.mu.Unlock()
}

func statusChanged(a, b Status) bool {
	return a.State != b.State ||
		a.SSID != b.SSID ||
		a.Security != b.Security ||
		a.IPAddress != b.IPAddress ||
		a.Detail != b.Detail ||
		a.APActive != b.APActive ||
		a.APSSID != b.APSSID ||
		a.APPortalURL != b.APPortalURL
}
