package discovery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeService(t *testing.T) {
	valid := []struct {
		in   Service
		want Service
	}{
		{
			in:   Service{Type: "_snapcast._tcp", Instance: "Living Room", Port: 1704, TXT: map[string]string{"v": "1"}},
			want: Service{Type: "_snapcast._tcp", Instance: "Living Room", Domain: "local", Port: 1704, TXT: map[string]string{"v": "1"}},
		},
		{
			in:   Service{Type: "_googlecast._udp", Instance: "cast", Port: 8009, Domain: "lan."},
			want: Service{Type: "_googlecast._udp", Instance: "cast", Domain: "lan", Port: 8009},
		},
		{
			in:   Service{Type: "_airplay._tcp.local.", Instance: "box", Port: 7000},
			want: Service{Type: "_airplay._tcp", Instance: "box", Domain: "local", Port: 7000},
		},
	}
	for _, tc := range valid {
		got, err := NormalizeService(tc.in)
		if err != nil {
			t.Fatalf("NormalizeService(%+v) = %v", tc.in, err)
		}
		if got.Type != tc.want.Type || got.Instance != tc.want.Instance || got.Domain != tc.want.Domain || got.Port != tc.want.Port {
			t.Fatalf("NormalizeService(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}

	invalid := []Service{
		{Type: "", Instance: "x", Port: 1},
		{Type: "_tcp", Instance: "x", Port: 1},
		{Type: "snap._tcp", Instance: "x", Port: 1},
		{Type: "_snap._sctp", Instance: "x", Port: 1},
		{Type: "_snap._tcp", Instance: "", Port: 1},
		{Type: "_snap._tcp", Instance: "x\x00y", Port: 1},
		{Type: "_snap._tcp", Instance: strings.Repeat("a", 64), Port: 1},
		{Type: "_snap._tcp", Instance: "x", Port: 0},
		{Type: "_snap._tcp", Instance: "x", Port: 65536},
		{Type: "_snap._tcp", Instance: "x", Port: 1, Domain: "bad_domain"},
		{Type: "_snap._tcp", Instance: "x", Port: 1, TXT: map[string]string{"a": "b", "": "c"}},
		{Type: "_snap._tcp", Instance: "x", Port: 1, TXT: map[string]string{"k": strings.Repeat("v", maxTXTBytes)}},
	}
	for _, svc := range invalid {
		if _, err := NormalizeService(svc); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("NormalizeService(%+v) = %v, want ErrInvalidRequest", svc, err)
		}
	}

	txt := map[string]string{}
	for i := range maxTXTRecords + 1 {
		txt[string(rune('a'+i))] = "v"
	}
	if _, err := NormalizeService(Service{Type: "_x._tcp", Instance: "i", Port: 1, TXT: txt}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("too many TXT records = %v", err)
	}
}

func TestNormalizeQuery(t *testing.T) {
	got, err := NormalizeQuery(Query{Type: "_snapcast._tcp"})
	if err != nil || got.Domain != "local" || got.Type != "_snapcast._tcp" {
		t.Fatalf("query = %+v, %v", got, err)
	}
	if _, err := NormalizeQuery(Query{Type: "nope"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid query = %v", err)
	}
}

type fakeServiceBackend struct {
	mu          sync.Mutex
	published   []Service
	unpublished []string
	browseQ     Query
	err         error
	browser     Browser
	closed      bool
}

func (f *fakeServiceBackend) publish(_ context.Context, svc Service) (Publication, error) {
	if f.err != nil {
		return Publication{}, f.err
	}
	f.mu.Lock()
	f.published = append(f.published, svc)
	f.mu.Unlock()
	return Publication{ID: "pub_fake", Service: svc, Host: "capsula.local"}, nil
}

func (f *fakeServiceBackend) unpublish(_ context.Context, id string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpublished = append(f.unpublished, id)
	return true, nil
}

func (f *fakeServiceBackend) browse(_ context.Context, q Query) (Browser, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	f.browseQ = q
	f.mu.Unlock()
	return f.browser, nil
}

func (f *fakeServiceBackend) close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

type fakeBrowser struct {
	events chan Event
	err    error
	closed bool
}

func (b *fakeBrowser) Events() <-chan Event { return b.events }
func (b *fakeBrowser) Err() error           { return b.err }
func (b *fakeBrowser) Close() error         { b.closed = true; return nil }

func TestServiceManagerPublishesAndBrowses(t *testing.T) {
	backend := &fakeServiceBackend{browser: &fakeBrowser{events: make(chan Event, 1)}}
	m := newServiceManager(nil, backend)

	pub, err := m.Publish(context.Background(), Service{
		Type: "_snapcast._tcp", Instance: "Kitchen", Port: 1704, TXT: map[string]string{"v": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pub.ID != "pub_fake" || pub.Service.Domain != "local" || pub.Host != "capsula.local" {
		t.Fatalf("publication = %+v", pub)
	}
	if len(backend.published) != 1 || backend.published[0].Instance != "Kitchen" {
		t.Fatalf("backend saw %+v", backend.published)
	}

	removed, err := m.Unpublish(context.Background(), pub.ID)
	if err != nil || !removed {
		t.Fatalf("unpublish = %v, %v", removed, err)
	}
	if len(backend.unpublished) != 1 || backend.unpublished[0] != pub.ID {
		t.Fatalf("backend unpublish = %v", backend.unpublished)
	}

	browser, err := m.Browse(context.Background(), Query{Type: "_googlecast._tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if backend.browseQ.Domain != "local" || backend.browseQ.Type != "_googlecast._tcp" {
		t.Fatalf("browse query = %+v", backend.browseQ)
	}
	browser.(*fakeBrowser).events <- Event{Kind: EventAdded, Name: "tv", Addresses: []string{"192.0.2.4"}}
	if got := (<-browser.Events()).Name; got != "tv" {
		t.Fatalf("event name = %q", got)
	}

	if _, err := m.Publish(context.Background(), Service{Type: "bad", Instance: "x", Port: 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid publish = %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if !backend.closed {
		t.Fatal("backend not closed")
	}
	if _, err := m.Publish(context.Background(), Service{Type: "_x._tcp", Instance: "i", Port: 1}); err == nil {
		t.Fatal("publish after Close succeeded")
	}
}

func TestUnsupportedServicesAreExplicit(t *testing.T) {
	m := OpenServices(nil, "simulator")
	if _, err := m.Publish(context.Background(), Service{Type: "_x._tcp", Instance: "i", Port: 1}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("publish = %v, want ErrUnsupported", err)
	}
	if _, err := m.Browse(context.Background(), Query{Type: "_x._tcp"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("browse = %v, want ErrUnsupported", err)
	}
	if removed, err := m.Unpublish(context.Background(), "pub"); err == nil || removed {
		t.Fatalf("unpublish = %v, %v, want an explicit unsupported error", removed, err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}
