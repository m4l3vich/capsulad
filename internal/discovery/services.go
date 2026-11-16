package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"capsulad/internal/logbus"
)

// ErrInvalidRequest marks a service publication or browse query the responder
// would reject. The daemon maps it to InvalidArgument.
var ErrInvalidRequest = errors.New("invalid mDNS service request")

// ErrResponderNotReady means an mDNS responder exists but is not answering
// right now (for example Avahi is registering after a restart). Callers must
// not treat it as a successful publication.
var ErrResponderNotReady = errors.New("mDNS responder is not running")

// ErrResponderLost means the responder disappeared while a browse stream was
// active; the registration or browser no longer exists.
var ErrResponderLost = errors.New("mDNS responder stopped")

const (
	maxInstanceBytes = 63
	maxTXTRecords    = 16
	maxTXTBytes      = 1024
	defaultDomain    = "local"
)

// Service describes one service instance to advertise.
type Service struct {
	Type     string
	Instance string
	Domain   string
	Port     uint32
	TXT      map[string]string
}

// Publication is a live registration. ID is opaque to callers except for
// Unpublish. Host is the responder's fully-qualified host name.
type Publication struct {
	ID      string
	Service Service
	Host    string
}

// EventKind is a browse event type.
type EventKind int

const (
	EventAdded EventKind = iota + 1
	EventRemoved
)

// Event is one browse notification. An ADDED event carries one resolved
// address per event; a REMOVED event names the instance that disappeared and
// carries no address.
type Event struct {
	Kind      EventKind
	Name      string
	Type      string
	Domain    string
	Host      string
	Addresses []string
	Port      uint32
	TXT       map[string]string
}

// Query selects the service type to browse.
type Query struct {
	Type   string
	Domain string
}

// Browser is a live browse stream. Events is closed when the browser stops;
// Err reports why it stopped (nil for a caller-requested Close).
type Browser interface {
	Events() <-chan Event
	Err() error
	Close() error
}

// ServiceManager publishes and browses local services through the host's mDNS
// responder. It never opens the mDNS port.
type ServiceManager interface {
	Publish(ctx context.Context, svc Service) (Publication, error)
	Unpublish(ctx context.Context, id string) (bool, error)
	Browse(ctx context.Context, q Query) (Browser, error)
	Close() error
}

// serviceBackend is the responder adapter below validation. Linux forwards it
// to Avahi over the system D-Bus; tests substitute a fake.
type serviceBackend interface {
	publish(ctx context.Context, svc Service) (Publication, error)
	unpublish(ctx context.Context, id string) (bool, error)
	browse(ctx context.Context, q Query) (Browser, error)
	close() error
}

// OpenServices returns the platform service manager. A build or platform with
// no responder gets an explicit unsupported manager, so callers can report
// FailedPrecondition/Unimplemented instead of a false success.
func OpenServices(logs *logbus.Bus, platform string) ServiceManager {
	return newServiceManager(logs, openServiceBackend(logs, platform))
}

func newServiceManager(logs *logbus.Bus, b serviceBackend) ServiceManager {
	return &serviceManager{logs: logs, backend: b}
}

type serviceManager struct {
	logs    *logbus.Bus
	backend serviceBackend

	mu     sync.Mutex
	closed bool
}

func (m *serviceManager) Publish(ctx context.Context, svc Service) (Publication, error) {
	normalized, err := NormalizeService(svc)
	if err != nil {
		return Publication{}, err
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return Publication{}, ErrResponderLost
	}
	pub, err := m.backend.publish(ctx, normalized)
	if err != nil {
		return Publication{}, err
	}
	if m.logs != nil {
		m.logs.Infof("mdns", "", "published %s instance=%q domain=%s port=%d",
			normalized.Type, normalized.Instance, normalized.Domain, normalized.Port)
	}
	return pub, nil
}

func (m *serviceManager) Unpublish(ctx context.Context, id string) (bool, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return false, nil
	}
	removed, err := m.backend.unpublish(ctx, id)
	if err == nil && removed && m.logs != nil {
		m.logs.Infof("mdns", "", "unpublished %s", id)
	}
	return removed, err
}

func (m *serviceManager) Browse(ctx context.Context, q Query) (Browser, error) {
	normalized, err := NormalizeQuery(q)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, ErrResponderLost
	}
	return m.backend.browse(ctx, normalized)
}

func (m *serviceManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	return m.backend.close()
}

// NormalizeService validates a service advertisement and fills the default
// domain. Service types are "_name._tcp" or "_name._udp", optionally carrying
// a domain suffix ("_name._tcp.local.").
func NormalizeService(svc Service) (Service, error) {
	serviceType, embeddedDomain, err := splitServiceType(svc.Type)
	if err != nil {
		return Service{}, err
	}
	domain := svc.Domain
	if domain == "" {
		domain = embeddedDomain
	}
	if domain == "" {
		domain = defaultDomain
	}
	if embeddedDomain != "" && svc.Domain != "" && !strings.EqualFold(strings.TrimSuffix(svc.Domain, "."), embeddedDomain) {
		return Service{}, fmt.Errorf("%w: service_type domain %q disagrees with domain %q", ErrInvalidRequest, embeddedDomain, svc.Domain)
	}
	domain, err = normalizeDomain(domain)
	if err != nil {
		return Service{}, err
	}
	if err := validateInstance(svc.Instance); err != nil {
		return Service{}, err
	}
	if svc.Port == 0 || svc.Port > 65535 {
		return Service{}, fmt.Errorf("%w: port %d is outside 1..65535", ErrInvalidRequest, svc.Port)
	}
	txt, err := normalizeTXT(svc.TXT)
	if err != nil {
		return Service{}, err
	}
	return Service{Type: serviceType, Instance: svc.Instance, Domain: domain, Port: svc.Port, TXT: txt}, nil
}

// NormalizeQuery validates a browse query.
func NormalizeQuery(q Query) (Query, error) {
	serviceType, embeddedDomain, err := splitServiceType(q.Type)
	if err != nil {
		return Query{}, err
	}
	domain := q.Domain
	if domain == "" {
		domain = embeddedDomain
	}
	if domain == "" {
		domain = defaultDomain
	}
	domain, err = normalizeDomain(domain)
	if err != nil {
		return Query{}, err
	}
	return Query{Type: serviceType, Domain: domain}, nil
}

// splitServiceType accepts "_name._tcp", "_name._udp" and the DNS form with a
// trailing domain. The returned domain is lower case without a trailing dot.
func splitServiceType(raw string) (serviceType, domain string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", fmt.Errorf("%w: service_type is required", ErrInvalidRequest)
	}
	labels := strings.Split(strings.TrimSuffix(s, "."), ".")
	if len(labels) < 2 {
		return "", "", fmt.Errorf("%w: service_type must look like _name._tcp or _name._udp", ErrInvalidRequest)
	}
	name, proto := labels[0], strings.ToLower(labels[1])
	if len(name) < 2 || name[0] != '_' {
		return "", "", fmt.Errorf("%w: service name %q must start with _", ErrInvalidRequest, name)
	}
	if len(name)-1 > MaxLabelLength {
		return "", "", fmt.Errorf("%w: service name exceeds %d bytes", ErrInvalidRequest, MaxLabelLength)
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		digit := c >= '0' && c <= '9'
		if !letter && !digit && c != '-' {
			return "", "", fmt.Errorf("%w: service name %q may contain only letters, digits and hyphens", ErrInvalidRequest, name)
		}
	}
	if proto != "_tcp" && proto != "_udp" {
		return "", "", fmt.Errorf("%w: service_type protocol must be _tcp or _udp", ErrInvalidRequest)
	}
	serviceType = name + "." + proto
	if len(labels) > 2 {
		domain = strings.ToLower(strings.Join(labels[2:], "."))
	}
	return serviceType, domain, nil
}

func normalizeDomain(raw string) (string, error) {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if domain == "" {
		return "", fmt.Errorf("%w: domain is empty", ErrInvalidRequest)
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > MaxLabelLength {
			return "", fmt.Errorf("%w: domain %q has an invalid label", ErrInvalidRequest, raw)
		}
		for i := range len(label) {
			c := label[i]
			letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
			digit := c >= '0' && c <= '9'
			if !letter && !digit && c != '-' {
				return "", fmt.Errorf("%w: domain %q may contain only letters, digits, hyphens and dots", ErrInvalidRequest, raw)
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("%w: domain %q has a label starting or ending with a hyphen", ErrInvalidRequest, raw)
		}
	}
	return domain, nil
}

func validateInstance(instance string) error {
	if instance == "" {
		return fmt.Errorf("%w: instance name is required", ErrInvalidRequest)
	}
	if !utf8.ValidString(instance) {
		return fmt.Errorf("%w: instance name is not valid UTF-8", ErrInvalidRequest)
	}
	if strings.ContainsRune(instance, 0) {
		return fmt.Errorf("%w: instance name contains NUL", ErrInvalidRequest)
	}
	if len(instance) > maxInstanceBytes {
		return fmt.Errorf("%w: instance name is %d bytes, limit %d", ErrInvalidRequest, len(instance), maxInstanceBytes)
	}
	return nil
}

func normalizeTXT(txt map[string]string) (map[string]string, error) {
	if len(txt) == 0 {
		return nil, nil
	}
	if len(txt) > maxTXTRecords {
		return nil, fmt.Errorf("%w: %d TXT records, limit %d", ErrInvalidRequest, len(txt), maxTXTRecords)
	}
	total := 0
	out := make(map[string]string, len(txt))
	for key, value := range txt {
		if key == "" {
			return nil, fmt.Errorf("%w: TXT key is empty", ErrInvalidRequest)
		}
		if strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("%w: TXT record %q contains NUL", ErrInvalidRequest, key)
		}
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return nil, fmt.Errorf("%w: TXT record %q is not valid UTF-8", ErrInvalidRequest, key)
		}
		total += len(key) + len(value)
		out[key] = value
	}
	if total > maxTXTBytes {
		return nil, fmt.Errorf("%w: TXT records total %d bytes, limit %d", ErrInvalidRequest, total, maxTXTBytes)
	}
	return out, nil
}

// unsupportedServices reports a missing responder explicitly.
type unsupportedServices struct{ reason string }

func (u unsupportedServices) publish(context.Context, Service) (Publication, error) {
	return Publication{}, fmt.Errorf("%w: %s", ErrUnsupported, u.reason)
}

func (u unsupportedServices) unpublish(context.Context, string) (bool, error) {
	return false, fmt.Errorf("%w: %s", ErrUnsupported, u.reason)
}

func (u unsupportedServices) browse(context.Context, Query) (Browser, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, u.reason)
}

func (u unsupportedServices) close() error { return nil }
