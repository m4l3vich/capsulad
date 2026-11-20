package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/discovery"
	"capsulad/internal/ids"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
	"capsulad/internal/supervisor"
)

type fakeMDNS struct {
	mu       sync.Mutex
	err      error
	pubs     []discovery.Publication
	unpubs   []string
	browser  discovery.Browser
	browseQ  discovery.Query
	closed   bool
	pubCalls int
}

func (f *fakeMDNS) Publish(_ context.Context, svc discovery.Service) (discovery.Publication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pubCalls++
	if f.err != nil {
		return discovery.Publication{}, f.err
	}
	normalized, err := discovery.NormalizeService(svc)
	if err != nil {
		return discovery.Publication{}, err
	}
	pub := discovery.Publication{ID: fmt.Sprintf("pub_%d", f.pubCalls), Service: normalized, Host: "capsula.local"}
	f.pubs = append(f.pubs, pub)
	return pub, nil
}

func (f *fakeMDNS) Unpublish(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	f.unpubs = append(f.unpubs, id)
	return true, nil
}

func (f *fakeMDNS) Browse(_ context.Context, q discovery.Query) (discovery.Browser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	normalized, err := discovery.NormalizeQuery(q)
	if err != nil {
		return nil, err
	}
	f.browseQ = normalized
	return f.browser, nil
}

func (f *fakeMDNS) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

type fakeGenericBrowser struct {
	events chan discovery.Event
	err    error
	closed bool
}

func (b *fakeGenericBrowser) Events() <-chan discovery.Event { return b.events }
func (b *fakeGenericBrowser) Err() error                     { return b.err }
func (b *fakeGenericBrowser) Close() error                   { b.closed = true; return nil }

type sleepSpawner struct{}

func (sleepSpawner) Command(*manifest.Manifest, string, string, string) (*exec.Cmd, error) {
	return exec.Command("/bin/sleep", "30"), nil
}
func (sleepSpawner) Mode() string { return "test" }

// discoveryFixture returns the daemon, its supervisor, and the contexts of the
// app's current live run and the run it replaced. A supervised app is a
// singleton, so two runs of test.mdns are never live together; the stale
// context is the previous generation's run ID, which is what a publication
// recorded while that run was exiting (the responder call was in flight when
// cleanup ran) still carries. That stale owner is the identity a current run
// must never be able to unpublish.
func discoveryFixture(t *testing.T) (*Daemon, *supervisor.Supervisor, context.Context, context.Context) {
	t.Helper()
	logs, err := logbus.New(ids.NewClock(), 32, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup := supervisor.New(ctx, logs, ids.NewClock(), "", sleepSpawner{})
	t.Cleanup(func() { sup.Shutdown(time.Second) })

	m := &manifest.Manifest{
		ID: "test.mdns", Name: "mDNS test", Version: "1.0.0",
		Entrypoint: manifest.Entrypoint{Runtime: manifest.RuntimeNative},
	}
	stale, spawned, err := sup.StartIfAbsent(m, "inv-1")
	if err != nil || !spawned {
		t.Fatalf("start stale fixture: spawned=%v err=%v", spawned, err)
	}
	if !sup.Stop(stale.RunID) {
		t.Fatal("stop stale fixture")
	}
	waitFor(t, 5*time.Second, "stale fixture removed", func() bool {
		_, live := sup.Get(stale.RunID)
		return !live
	})

	live, spawned, err := sup.StartIfAbsent(m, "inv-2")
	if err != nil || !spawned {
		t.Fatalf("start live fixture: spawned=%v err=%v", spawned, err)
	}
	d := &Daemon{logs: logs, sup: sup, mdnsPubs: map[string]mdnsPublication{}}
	runCtx := func(runID string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("capsulad-run-id", runID))
	}
	return d, sup, runCtx(live.RunID), runCtx(stale.RunID)
}

func TestDiscoveryPublishIsScopedToRun(t *testing.T) {
	d, _, liveCtx, staleCtx := discoveryFixture(t)
	fake := &fakeMDNS{}
	d.mdns = fake
	svc := &discoveryService{d: d}

	resp, err := svc.Publish(liveCtx, &frameworkv1.PublishServiceRequest{
		ServiceType: "_snapcast._tcp", InstanceName: "Kitchen", Port: 1704,
		Txt: map[string]string{"v": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPublicationId() != "pub_1" || resp.GetDomain() != "local" || resp.GetHost() != "capsula.local" {
		t.Fatalf("publish response = %+v", resp)
	}

	// A registration recorded for the app's previous run (the responder call
	// completed after that run's cleanup) still belongs to that run. The
	// current live run has the same app ID but must not remove it, and the
	// responder must not be asked to.
	_, staleRunID := callerRunID(t, staleCtx)
	d.mdnsMu.Lock()
	d.mdnsPubs["pub_stale"] = mdnsPublication{appID: "test.mdns", runID: staleRunID, pub: discovery.Publication{ID: "pub_stale"}}
	d.mdnsMu.Unlock()
	if _, err := svc.Unpublish(liveCtx, &frameworkv1.UnpublishServiceRequest{PublicationId: "pub_stale"}); status.Code(err) != codes.NotFound {
		t.Fatalf("foreign unpublish of a stale run's registration = %v, want NotFound", err)
	}
	if len(fake.unpubs) != 0 {
		t.Fatalf("foreign unpublish reached the responder: %v", fake.unpubs)
	}
	d.mdnsMu.Lock()
	_, staleKept := d.mdnsPubs["pub_stale"]
	d.mdnsMu.Unlock()
	if !staleKept {
		t.Fatal("foreign unpublish removed the stale run's registration")
	}

	out, err := svc.Unpublish(liveCtx, &frameworkv1.UnpublishServiceRequest{PublicationId: resp.GetPublicationId()})
	if err != nil || !out.GetRemoved() {
		t.Fatalf("owner unpublish = %+v, %v", out, err)
	}
	if len(fake.unpubs) != 1 || fake.unpubs[0] != "pub_1" {
		t.Fatalf("unpublish calls = %v", fake.unpubs)
	}
	if _, err := svc.Unpublish(liveCtx, &frameworkv1.UnpublishServiceRequest{PublicationId: resp.GetPublicationId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("second unpublish = %v, want NotFound", err)
	}
}

func TestDiscoveryAuthAndFailureMapping(t *testing.T) {
	d, _, liveCtx, _ := discoveryFixture(t)
	fake := &fakeMDNS{}
	d.mdns = fake
	svc := &discoveryService{d: d}

	if _, err := svc.Publish(context.Background(), &frameworkv1.PublishServiceRequest{ServiceType: "_x._tcp", InstanceName: "x", Port: 1}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous publish = %v, want PermissionDenied", err)
	}
	if err := svc.Browse(&frameworkv1.BrowseServicesRequest{ServiceType: "_x._tcp"}, &fakeBrowseStream{ctx: context.Background()}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("anonymous browse = %v, want PermissionDenied", err)
	}

	fake.err = fmt.Errorf("%w: no responder", discovery.ErrUnsupported)
	if _, err := svc.Publish(liveCtx, &frameworkv1.PublishServiceRequest{ServiceType: "_x._tcp", InstanceName: "x", Port: 1}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unsupported publish = %v, want FailedPrecondition", err)
	}
	if err := svc.Browse(&frameworkv1.BrowseServicesRequest{ServiceType: "_x._tcp"}, &fakeBrowseStream{ctx: liveCtx}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unsupported browse = %v, want FailedPrecondition", err)
	}

	fake.err = fmt.Errorf("%w: instance name is required", discovery.ErrInvalidRequest)
	if _, err := svc.Publish(liveCtx, &frameworkv1.PublishServiceRequest{ServiceType: "_x._tcp", Port: 1}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid publish = %v, want InvalidArgument", err)
	}

	fake.err = fmt.Errorf("%w: speaker", discovery.ErrNameInUse)
	if _, err := svc.Publish(liveCtx, &frameworkv1.PublishServiceRequest{ServiceType: "_x._tcp", InstanceName: "x", Port: 1}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("colliding publish = %v, want AlreadyExists", err)
	}
}

func TestDiscoveryBrowseStreamsEvents(t *testing.T) {
	d, _, liveCtx, _ := discoveryFixture(t)
	browser := &fakeGenericBrowser{events: make(chan discovery.Event, 2)}
	browser.events <- discovery.Event{
		Kind: discovery.EventAdded, Name: "tv", Type: "_googlecast._tcp", Domain: "local",
		Host: "cast.local", Addresses: []string{"192.0.2.10"}, Port: 8009, TXT: map[string]string{"id": "abc"},
	}
	browser.events <- discovery.Event{Kind: discovery.EventRemoved, Name: "tv", Type: "_googlecast._tcp", Domain: "local"}
	close(browser.events)
	fake := &fakeMDNS{browser: browser}
	d.mdns = fake
	stream := &fakeBrowseStream{ctx: liveCtx}
	err := (&discoveryService{d: d}).Browse(&frameworkv1.BrowseServicesRequest{ServiceType: "_googlecast._tcp"}, stream)
	if err != nil {
		t.Fatal(err)
	}
	if fake.browseQ.Domain != "local" || fake.browseQ.Type != "_googlecast._tcp" {
		t.Fatalf("browse query = %+v", fake.browseQ)
	}
	if len(stream.sent) != 2 {
		t.Fatalf("sent %d events, want 2", len(stream.sent))
	}
	if got := stream.sent[0]; got.GetKind() != frameworkv1.DiscoveredService_EVENT_KIND_ADDED ||
		got.GetName() != "tv" || got.GetPort() != 8009 || got.GetAddresses()[0] != "192.0.2.10" {
		t.Fatalf("added event = %+v", got)
	}
	if got := stream.sent[1]; got.GetKind() != frameworkv1.DiscoveredService_EVENT_KIND_REMOVED {
		t.Fatalf("removed event = %+v", got)
	}
	if !browser.closed {
		t.Fatal("browser was not closed when the stream ended")
	}
}

func TestRemoveRunPublicationsOnlyRunOwned(t *testing.T) {
	d, _, liveCtx, staleCtx := discoveryFixture(t)
	fake := &fakeMDNS{}
	d.mdns = fake
	svc := &discoveryService{d: d}

	livePub, err := svc.Publish(liveCtx, &frameworkv1.PublishServiceRequest{
		ServiceType: "_airplay._tcp", InstanceName: "box", Port: 7000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A registration from the app's previous run survives that run's cleanup
	// when the responder call was still in flight. Its cleanup path must
	// release exactly that registration and leave the current run's alone.
	_, staleRunID := callerRunID(t, staleCtx)
	d.mdnsMu.Lock()
	d.mdnsPubs["pub_stale"] = mdnsPublication{appID: "test.mdns", runID: staleRunID, pub: discovery.Publication{ID: "pub_stale"}}
	d.mdnsMu.Unlock()

	d.removeRunPublications(staleRunID)
	if len(fake.unpubs) != 1 || fake.unpubs[0] != "pub_stale" {
		t.Fatalf("unpublish calls = %v, want the stale registration only", fake.unpubs)
	}
	d.mdnsMu.Lock()
	_, liveKept := d.mdnsPubs[livePub.GetPublicationId()]
	remaining := len(d.mdnsPubs)
	d.mdnsMu.Unlock()
	if !liveKept || remaining != 1 {
		t.Fatalf("remaining publications = %d, live registration kept=%v, want only the live one", remaining, liveKept)
	}

	_, liveRunID := callerRunID(t, liveCtx)
	d.removeRunPublications(liveRunID)
	if len(fake.unpubs) != 2 || fake.unpubs[1] != livePub.GetPublicationId() {
		t.Fatalf("unpublish calls after the live run exited = %v", fake.unpubs)
	}
	d.mdnsMu.Lock()
	remaining = len(d.mdnsPubs)
	d.mdnsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("remaining publications = %d, want none", remaining)
	}
}

func callerRunID(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get("capsulad-run-id")) == 0 {
		t.Fatal("no run metadata")
	}
	return "", md.Get("capsulad-run-id")[0]
}

type fakeBrowseStream struct {
	ctx  context.Context
	sent []*frameworkv1.DiscoveredService
}

func (s *fakeBrowseStream) Context() context.Context { return s.ctx }
func (s *fakeBrowseStream) Send(m *frameworkv1.DiscoveredService) error {
	s.sent = append(s.sent, m)
	return nil
}
func (s *fakeBrowseStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeBrowseStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeBrowseStream) SetTrailer(metadata.MD)       {}
func (s *fakeBrowseStream) SendMsg(any) error            { return nil }
func (s *fakeBrowseStream) RecvMsg(any) error            { return errors.New("not implemented") }
