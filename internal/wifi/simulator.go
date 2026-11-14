package wifi

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// SimulatorOptions describes the deterministic in-memory manager used by the
// simulator daemon and focused tests. The daemon leaves Available false: a
// simulator has no radio, and pretending otherwise would be a false success.
// Tests that need UI interaction opt in explicitly.
type SimulatorOptions struct {
	Available   bool
	Detail      string
	Interface   string
	APName      string
	APAddress   string
	PortalPort  int
	Known       []KnownNetwork
	ScanResults []ScanResult
}

type Simulator struct {
	mu       sync.Mutex
	status   Status
	known    []KnownNetwork
	scans    []ScanResult
	scanHits int
	closed   bool
	revision uint64
}

func NewSimulator(opts SimulatorOptions) *Simulator {
	detail := opts.Detail
	if detail == "" {
		if opts.Available {
			detail = "simulated Wi-Fi radio"
		} else {
			detail = "the simulator has no Wi-Fi radio"
		}
	}
	iface := opts.Interface
	if iface == "" {
		iface = "sim-wlan0"
	}
	state := StateUnavailable
	if opts.Available {
		state = StateDisconnected
	}
	known := append([]KnownNetwork(nil), opts.Known...)
	sim := &Simulator{
		status: Status{
			Available:     opts.Available,
			State:         state,
			Interface:     iface,
			Detail:        detail,
			APSSID:        opts.APName,
			APAddress:     opts.APAddress,
			PortalPort:    opts.PortalPort,
			ScanSupported: opts.Available,
			Revision:      1,
		},
		known:    known,
		scans:    append([]ScanResult(nil), opts.ScanResults...),
		revision: 1,
	}
	return sim
}

func (s *Simulator) Start(context.Context) error { return nil }

func (s *Simulator) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *Simulator) Status(context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.status
	out.Revision = s.revision
	return out, nil
}

func (s *Simulator) KnownNetworks(context.Context) ([]KnownNetwork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]KnownNetwork, len(s.known))
	copy(out, s.known)
	return out, nil
}

func (s *Simulator) Scan(context.Context) ([]ScanResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.status.Available {
		return nil, ErrUnavailable
	}
	s.scanHits++
	out := make([]ScanResult, len(s.scans))
	copy(out, s.scans)
	return out, nil
}

// ScanCalls exists for regression tests that prove a status view never scans.
func (s *Simulator) ScanCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanHits
}

func (s *Simulator) Connect(_ context.Context, req ConnectRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.status.Available {
		return ErrUnavailable
	}
	security, err := NormalizeSecurity(req.Security)
	if err != nil {
		return err
	}
	if !validSSID(req.SSID) {
		return fmt.Errorf("%w: SSID must be 1-32 bytes", ErrInvalidNetwork)
	}
	if security != SecurityOpen && strings.TrimSpace(req.Passphrase) == "" {
		return fmt.Errorf("%w: passphrase is required", ErrInvalidNetwork)
	}
	if security == SecurityWEP {
		return fmt.Errorf("%w: WEP is not supported", ErrUnsupported)
	}
	for i := range s.known {
		if s.known[i].SSID == req.SSID {
			s.known[i].Security = security
			s.known[i].HasPassword = security != SecurityOpen
			s.known[i].Connected = true
			s.known[i].Priority = int(s.revision) + 1
			s.status.SSID = req.SSID
			s.status.Security = security
			s.status.State = StateConnected
			s.status.Detail = "connected to simulated network"
			s.revision++
			for j := range s.known {
				if j != i {
					s.known[j].Connected = false
				}
			}
			return nil
		}
	}
	s.known = append([]KnownNetwork{{
		SSID: req.SSID, Security: security, HasPassword: security != SecurityOpen,
		Connected: true, Priority: int(s.revision) + 1,
	}}, s.known...)
	s.status.SSID = req.SSID
	s.status.Security = security
	s.status.State = StateConnected
	s.status.Detail = "connected to simulated network"
	s.revision++
	return nil
}

func (s *Simulator) Forget(_ context.Context, ssid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.status.Available {
		return ErrUnavailable
	}
	out := s.known[:0]
	found := false
	for _, n := range s.known {
		if n.SSID == ssid {
			found = true
			continue
		}
		out = append(out, n)
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrNotFound, ssid)
	}
	s.known = out
	if s.status.SSID == ssid {
		s.status.SSID = ""
		s.status.Security = ""
		s.status.State = StateDisconnected
		s.status.Detail = "network forgotten"
	}
	s.revision++
	return nil
}
