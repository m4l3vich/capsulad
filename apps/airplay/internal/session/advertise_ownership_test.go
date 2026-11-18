package session

// Advertisement ownership regression: session start/end, the group-id update
// and the Re-advertise action can request a republish concurrently. The field
// log showed an Unpublish that returned removed=true followed by an AddService
// collision, then a successful publish from the other caller. These tests pin
// the serialization that keeps one caller from withdrawing another's fresh
// record and racing the same instance, and that makes Close wait for an
// in-flight publish before withdrawing.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/media"
)

type ownershipAdvertiser struct {
	mu   sync.Mutex
	live map[string]string // "type|instance" -> publication id
	next int

	// arm makes the first Publish wait briefly for a second concurrent
	// publisher, so an unserialized pair reliably overlaps instead of relying
	// on scheduler luck.
	arm      atomic.Bool
	barrier  sync.Once
	entered  chan struct{}
	inflight atomic.Int32
	overlaps atomic.Bool

	// hold blocks every Publish until release, so Close can race an in-flight
	// publish deterministically.
	hold        atomic.Bool
	release     chan struct{}
	releaseOnce sync.Once
}

func (a *ownershipAdvertiser) open() { a.releaseOnce.Do(func() { close(a.release) }) }

func newOwnershipAdvertiser() *ownershipAdvertiser {
	return &ownershipAdvertiser{
		live:    map[string]string{},
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (a *ownershipAdvertiser) Publish(advert Advert) (string, error) {
	if a.inflight.Add(1) > 1 {
		a.overlaps.Store(true)
	}
	defer a.inflight.Add(-1)
	if a.hold.Load() {
		select {
		case a.entered <- struct{}{}:
		default:
		}
		<-a.release
	}
	if a.arm.Load() {
		// Hold the first caller long enough for an unserialized second caller
		// to enter Publish; inflight then proves the overlap.
		a.barrier.Do(func() { time.Sleep(200 * time.Millisecond) })
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := advert.Type + "|" + advert.Instance
	if _, exists := a.live[key]; exists {
		return "", fmt.Errorf("instance %q is already registered", advert.Instance)
	}
	a.next++
	id := fmt.Sprintf("pub-test-%d", a.next)
	a.live[key] = id
	return id, nil
}

func (a *ownershipAdvertiser) Unpublish(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, existing := range a.live {
		if existing == id {
			delete(a.live, key)
		}
	}
	return nil
}

func (a *ownershipAdvertiser) liveCount(serviceType string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for key := range a.live {
		if len(key) > len(serviceType) && key[:len(serviceType)] == serviceType {
			count++
		}
	}
	return count
}

func (a *ownershipAdvertiser) liveTotal() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.live)
}

func newManagerWithAdvertiser(t *testing.T, advertiser Advertiser) *Manager {
	t.Helper()
	mgr, err := NewManager(
		Config{
			DeviceID: "aabbccddeeff", Name: "Test Speaker", Port: 7000,
			FrameTarget: 20 * time.Millisecond, Logf: t.Logf,
		},
		newFakeTransport(),
		advertiser,
		fakeRings{ring: &fakeRing{}},
		fakeSinks{sink: &fakeSink{}},
		media.NewBoard(),
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func TestConcurrentPublishIsSerialized(t *testing.T) {
	advertiser := newOwnershipAdvertiser()
	mgr := newManagerWithAdvertiser(t, advertiser)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	advertiser.arm.Store(true)

	port := mgr.advertisedPort()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- mgr.publish(port)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	if advertiser.overlaps.Load() {
		t.Fatal("two advertisement mutations ran concurrently")
	}
	if got := advertiser.liveCount("_airplay._tcp"); got != 1 {
		t.Fatalf("%d live _airplay records, want 1", got)
	}
	if got := advertiser.liveCount("_raop._tcp"); got != 1 {
		t.Fatalf("%d live _raop records, want 1", got)
	}
}

// TestDelayedRepublishUsesCurrentSessionState covers the ordering nuance that
// survives serialization: a caller delayed before it reads lifecycle state must
// publish the state current at publication time, not a bool captured when its
// event happened. Otherwise a late session-end republish could overwrite a
// newer active record with sf=inactive.
func TestDelayedRepublishUsesCurrentSessionState(t *testing.T) {
	advertiser := &fakeAdvertiser{}
	mgr := newManagerWithAdvertiser(t, advertiser)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	port := mgr.advertisedPort()

	gate := make(chan struct{})
	delayed := make(chan error, 1)
	go func() { <-gate; delayed <- mgr.publish(port) }()

	// A newer session is active before the delayed caller reaches publish.
	mgr.mu.Lock()
	mgr.sessionActive = true
	mgr.mu.Unlock()
	if err := mgr.publish(port); err != nil {
		t.Fatalf("active publish: %v", err)
	}

	close(gate)
	if err := <-delayed; err != nil {
		t.Fatalf("delayed publish: %v", err)
	}

	want := hexStatus(StatusReceiverSessionIsOn)
	for _, serviceType := range []string{"_airplay._tcp", "_raop._tcp"} {
		advert, ok := advertiser.find(serviceType)
		if !ok {
			t.Fatalf("no %s advertisement", serviceType)
		}
		if got := advert.TXT["sf"]; got != want {
			t.Fatalf("%s sf after delayed republish = %q, want %q", serviceType, got, want)
		}
	}
}

func TestCloseWaitsForInFlightPublish(t *testing.T) {
	advertiser := newOwnershipAdvertiser()
	mgr := newManagerWithAdvertiser(t, advertiser)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	advertiser.hold.Store(true)
	defer advertiser.open()

	publishDone := make(chan error, 1)
	go func() { publishDone <- mgr.publish(mgr.advertisedPort()) }()
	select {
	case <-advertiser.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("publish never reached the advertiser")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- mgr.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while a publish was in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	advertiser.open()
	if err := <-publishDone; err != nil {
		t.Fatalf("in-flight publish: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := advertiser.liveTotal(); got != 0 {
		t.Fatalf("Close left %d live advertisement(s)", got)
	}
}
