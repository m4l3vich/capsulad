// Package ptprecv feeds the AirPlay sender's IEEE 1588 clock to a PTP slave
// clock.
//
// The sender is the PTP grandmaster: its Sync and Follow_Up messages are
// addressed to the PTP event and general ports at 224.0.0.1, so the receiver
// needs a daemon-allocated multicast listener (see
// docs/app_development/network.md#multicast-receive-for-ptp). A socket bound to
// the port without joining the group never sees those datagrams, which is why
// this package can fail and the session then runs without a master clock.
package ptprecv

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"capsulad/apps/airplay/internal/ptp"
	"capsulad/apps/airplay/internal/session"
)

// Group is the AirPlay/802.1AS PTP multicast group.
const Group = "224.0.0.1"

// Ports are the PTP event and general ports the sender transmits on.
var Ports = []int{319, 320}

// Receiver owns the multicast listeners and the disciplined clock.
type Receiver struct {
	clock  *ptp.Clock
	logf   func(string, ...any)
	mu     sync.Mutex
	closed bool
	stop   chan struct{}
	wg     sync.WaitGroup
	groups []session.UDPListener
}

// Start joins the PTP group on the event and general ports. It returns an error
// naming the first port the runtime refused, so the caller can report that PTP
// is unavailable instead of silently pacing from arrival time.
func Start(transport session.Transport, logf func(string, ...any)) (*Receiver, error) {
	if transport == nil {
		return nil, errors.New("no transport for the PTP listeners")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r := &Receiver{clock: ptp.NewClock(), logf: logf, stop: make(chan struct{})}
	for _, port := range Ports {
		listener, err := transport.OpenUDPListener("0.0.0.0", port, Group)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("PTP port %d: %w", port, err)
		}
		r.mu.Lock()
		r.groups = append(r.groups, listener)
		r.mu.Unlock()
		r.wg.Add(1)
		go r.serve(listener)
	}
	logf("PTP: joined %s on ports %v", Group, Ports)
	return r, nil
}

func (r *Receiver) serve(listener session.UDPListener) {
	defer r.wg.Done()
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		datagram, _, err := listener.Receive()
		if err != nil {
			select {
			case <-r.stop:
			default:
				r.logf("PTP: receive on port %d: %v", portOf(listener), err)
			}
			return
		}
		message, err := ptp.Parse(datagram)
		if err != nil {
			// The PTP ports carry other senders' traffic; a malformed or
			// foreign message is not an error worth a log line per packet.
			continue
		}
		ingress := time.Now()
		switch message.Header.Type {
		case ptp.MsgSync:
			r.clock.AddSync(message, ingress)
		case ptp.MsgFollowUp:
			r.clock.AddFollowUp(message, ingress)
		}
	}
}

func portOf(listener session.UDPListener) int {
	_, port := listener.Addr()
	return port
}

// Offset reports (master - local) in nanoseconds and whether the clock is
// disciplined. A receiver that never saw a complete Sync/Follow_Up pair keeps
// reporting false.
func (r *Receiver) Offset() (int64, bool) {
	if r == nil {
		return 0, false
	}
	return r.clock.OffsetNanos()
}

// Close releases the listeners and stops the readers.
func (r *Receiver) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	groups := r.groups
	r.groups = nil
	close(r.stop)
	r.mu.Unlock()
	var first error
	for _, listener := range groups {
		if err := listener.Close(); err != nil && first == nil {
			first = err
		}
	}
	r.wg.Wait()
	return first
}
