package hw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"capsulad/sdk/capsulad"
)

// Broker discovery.
const (
	// mqttServiceType is the service type Home Assistant's Mosquitto add-on and
	// most home brokers advertise, including via Avahi's nss-mdns bridge.
	mqttServiceType = "_mqtt._tcp"
	// discoveryTimeout bounds one browse. A browse that finds nothing is
	// retried on the next reconnect attempt.
	discoveryTimeout = 20 * time.Second
)

// BrokerDiscoverer finds a local MQTT broker through the daemon's Avahi
// responder. Browsing is the only mDNS path an app has; the app never runs a
// responder and never binds port 5353.
type BrokerDiscoverer struct {
	client *capsulad.Client
	logf   func(format string, args ...any)
}

// NewBrokerDiscoverer returns a discoverer over a connected client.
func NewBrokerDiscoverer(client *capsulad.Client, logf func(string, ...any)) *BrokerDiscoverer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &BrokerDiscoverer{client: client, logf: logf}
}

// Discover returns the first usable address and port of an advertised
// `_mqtt._tcp` service. IPv4 addresses are preferred: a link-local IPv6 address
// is frequently not the one a broker listens on.
//
// Browse events are untrusted network input; only the address and port are
// used, and nothing from the TXT record is trusted or logged.
func (d *BrokerDiscoverer) Discover(ctx context.Context) (string, int, error) {
	browseCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	browse, err := d.client.BrowseMDNS(browseCtx, mqttServiceType, "")
	if err != nil {
		return "", 0, fmt.Errorf("browse %s: %w", mqttServiceType, err)
	}
	defer browse.Close()

	fallback := ""
	var fallbackPort int
	for {
		event, err := browse.Next(browseCtx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if fallback != "" {
				break
			}
			return "", 0, fmt.Errorf("browse %s: %w", mqttServiceType, err)
		}
		if event.Kind != capsulad.MDNSEventKindAdded || event.Port == 0 {
			continue
		}
		d.logf("discovery: %s at %v:%d", event.Name, event.Addresses, event.Port)
		for _, address := range event.Addresses {
			if address == "" {
				continue
			}
			if !strings.Contains(address, ":") {
				return address, int(event.Port), nil
			}
			if fallback == "" {
				fallback, fallbackPort = address, int(event.Port)
			}
		}
	}
	if fallback != "" {
		return fallback, fallbackPort, nil
	}
	return "", 0, fmt.Errorf("no %s service is advertised on the local link", mqttServiceType)
}
