//go:build linux

package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestAvahiTXTRoundTrip(t *testing.T) {
	in := map[string]string{"v": "1", "path": "/media"}
	records := avahiTXT(in)
	if len(records) != len(in) {
		t.Fatalf("records = %q", records)
	}
	got := decodeAvahiTXT(records)
	for key, value := range in {
		if got[key] != value {
			t.Fatalf("decoded %q = %q, want %q", key, got[key], value)
		}
	}
	if decodeAvahiTXT(nil) != nil {
		t.Fatal("nil TXT decoded to a map")
	}
}

func TestAvahiBrowserPushDropsOldest(t *testing.T) {
	br := &avahiBrowser{events: make(chan Event, 1)}
	br.push(Event{Kind: EventAdded, Name: "one"})
	br.push(Event{Kind: EventAdded, Name: "two"})
	if got := (<-br.events).Name; got != "two" {
		t.Fatalf("kept %q, want the newest event", got)
	}
	if br.drops != 1 {
		t.Fatalf("drops = %d, want 1", br.drops)
	}
}

// TestAvahiUnpublishKeepsTrackerUntilFreed covers the ordering a retry depends
// on: a transient Free failure must leave the publication tracked and return the
// error, so a daemon retry can still free the group and the advertisement
// cannot survive a later shutdown unnoticed.
func TestAvahiUnpublishKeepsTrackerUntilFreed(t *testing.T) {
	transient := errors.New("org.freedesktop.DBus.Error.NoReply")
	id := "pub_test"
	path := dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1")
	var (
		calls int
		fail  = true
		freed []dbus.ObjectPath
	)
	b := &avahiServiceBackend{
		pubs: map[string]avahiPublication{id: {path: path}},
		freeGroupFn: func(ctx context.Context, p dbus.ObjectPath) error {
			calls++
			if fail {
				return transient
			}
			freed = append(freed, p)
			return nil
		},
	}

	removed, err := b.unpublish(context.Background(), id)
	if removed || !errors.Is(err, transient) {
		t.Fatalf("failed Free returned (%v, %v), want (false, wrapped error)", removed, err)
	}
	if _, tracked := b.pubs[id]; !tracked {
		t.Fatal("tracker dropped the publication after a failed Free")
	}

	// The retry frees the same group and only then forgets the publication.
	fail = false
	removed, err = b.unpublish(context.Background(), id)
	if !removed || err != nil {
		t.Fatalf("retry returned (%v, %v), want (true, nil)", removed, err)
	}
	if _, tracked := b.pubs[id]; tracked {
		t.Fatal("publication still tracked after a successful Free")
	}
	if calls != 2 || len(freed) != 1 || freed[0] != path {
		t.Fatalf("Free calls = %d (%v), want the retried group freed once", calls, freed)
	}
	// A publication that is already gone is not an error, and it must not be
	// retried forever.
	removed, err = b.unpublish(context.Background(), id)
	if removed || err != nil {
		t.Fatalf("unknown publication returned (%v, %v), want (false, nil)", removed, err)
	}
}

// TestAvahiUnpublishDropsTrackerOnObjectGone proves a definitive object-gone
// error counts as removed: Avahi restarted or the group was already freed, so
// the registration the caller asked to drop no longer exists.
func TestAvahiUnpublishDropsTrackerOnObjectGone(t *testing.T) {
	id := "pub_gone"
	b := &avahiServiceBackend{
		pubs: map[string]avahiPublication{id: {path: "/org/freedesktop/Avahi/EntryGroup2"}},
		freeGroupFn: func(context.Context, dbus.ObjectPath) error {
			return dbus.Error{Name: "org.freedesktop.DBus.Error.UnknownObject", Body: []any{"no such object"}}
		},
	}
	removed, err := b.unpublish(context.Background(), id)
	if !removed || err != nil {
		t.Fatalf("object-gone Free returned (%v, %v), want (true, nil)", removed, err)
	}
	if _, tracked := b.pubs[id]; tracked {
		t.Fatal("publication still tracked after the group was definitively gone")
	}
}

// TestAvahiUnpublishWithoutResponder reports the responder as lost instead of
// silently claiming the publication was removed.
func TestAvahiUnpublishWithoutResponder(t *testing.T) {
	b := &avahiServiceBackend{pubs: map[string]avahiPublication{"pub_x": {path: "/x"}}}
	removed, err := b.unpublish(context.Background(), "pub_x")
	if removed || !errors.Is(err, ErrResponderLost) {
		t.Fatalf("unpublish without a responder = (%v, %v), want (false, ErrResponderLost)", removed, err)
	}
	if _, tracked := b.pubs["pub_x"]; !tracked {
		t.Fatal("tracker dropped the publication without a responder")
	}
}

// TestAvahiBrowserRunSurvivesReRegistering feeds the run loop the signal
// sequence a host-name re-apply produces. REGISTERING must be waited through
// with the browser's matches intact — item notifications still resolve — and
// only a terminal server state may end the stream.
func TestAvahiBrowserRunSurvivesReRegistering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	br := &avahiBrowser{
		ctx: ctx, cancel: cancel, path: "/fixture/ServiceBrowser1",
		query:  Query{Type: "_test._tcp", Domain: "local"},
		events: make(chan Event, 4),
		sigch:  make(chan *dbus.Signal, 8),
		done:   make(chan struct{}),
	}
	go br.run()

	stateSignal := func(state int32) *dbus.Signal {
		return &dbus.Signal{Name: avahiServerIface + ".StateChanged", Path: avahiPath, Body: []any{state}}
	}
	itemSignal := func(member string) *dbus.Signal {
		return &dbus.Signal{
			Name: avahiServiceBrowserIface + "." + member, Path: br.path,
			Body: []any{int32(0), int32(0), "capsula-speaker", "_test._tcp", "local", uint32(0)},
		}
	}

	br.sigch <- stateSignal(avahiServerRegistering)
	br.sigch <- stateSignal(stateRunning)
	br.sigch <- itemSignal("ItemRemove")

	select {
	case ev, ok := <-br.events:
		if !ok {
			t.Fatal("REGISTERING closed the browse stream")
		}
		if ev.Kind != EventRemoved || ev.Name != "capsula-speaker" {
			t.Fatalf("event = %+v, want the removal after re-registration", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived after RUNNING → REGISTERING → RUNNING")
	}
	if err := br.Err(); err != nil {
		t.Fatalf("browse error after re-registration = %v, want nil", err)
	}

	// A terminal state still ends the stream with the responder-lost error.
	br.sigch <- stateSignal(avahiServerCollision)
	select {
	case _, ok := <-br.events:
		if ok {
			t.Fatal("collision left the browse event channel open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collision did not end the browse stream")
	}
	if err := br.Err(); !errors.Is(err, ErrResponderLost) {
		t.Fatalf("browse error = %v, want ErrResponderLost", err)
	}
	<-br.done
}
