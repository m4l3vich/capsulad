// Package uichanges is the daemon's in-process invalidation signal for the
// authenticated Web UI. It carries no data: a subscriber rebuilds the topic
// view from the authoritative store when it receives a topic change.
//
// It exists because the wakeword catalog and the local security store have no
// framework watch RPC. Browser sockets are the only subscribers; publishing is
// non-blocking and a saturated subscriber coalesces to the newest signal, so a
// mutation burst can never stall an RPC or grow memory without bound.
package uichanges

import "sync"

type Topic uint8

const (
	Wakeword Topic = iota + 1
	Access
	// AppSettings invalidates per-application settings views. It carries no
	// app id: a subscriber rebuilds only the page it currently shows from the
	// authoritative application-configuration service.
	AppSettings
	// Wifi invalidates the Wi-Fi status view after a backend state change.
	Wifi
	// Bluetooth invalidates observed adapter status (incoming/discoverable/
	// powered/name) after the backend or hardware changes it.
	Bluetooth
	// Apps invalidates the installed-application listing after a package was
	// installed, replaced or removed. It carries no app id: a subscriber
	// rebuilds the application views it currently shows from the authoritative
	// registry through the framework API. This is the only invalidation a
	// package mutation publishes; run transitions keep their own watch stream.
	Apps
)

type Hub struct {
	mu   sync.Mutex
	subs map[int]chan Topic
	next int
}

func New() *Hub {
	return &Hub{subs: map[int]chan Topic{}}
}

// Subscribe returns a bounded signal stream and an idempotent cancel. The
// channel is closed by cancel; a slow subscriber keeps only recent signals.
func (h *Hub) Subscribe() (<-chan Topic, func()) {
	ch := make(chan Topic, 8)
	h.mu.Lock()
	h.next++
	id := h.next
	h.subs[id] = ch
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if current, ok := h.subs[id]; ok {
				delete(h.subs, id)
				close(current)
			}
			h.mu.Unlock()
		})
	}
}

// Publish invalidates topic for every current subscriber. If a subscriber is
// full, the oldest pending signal is discarded so the newest mutation is
// always represented; the subscriber's next snapshot is authoritative.
func (h *Hub) Publish(topic Topic) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- topic:
			continue
		default:
		}
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- topic:
		default:
		}
	}
}
