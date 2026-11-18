//go:build linux

package discovery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"capsulad/internal/ids"
	"capsulad/internal/logbus"
)

// Avahi's public EntryGroup, ServiceBrowser and ServiceResolver interfaces.
// The daemon never opens the mDNS port: Avahi owns it on the stock image.
const (
	avahiEntryGroupIface             = avahiService + ".EntryGroup"
	avahiServiceBrowserIface         = avahiService + ".ServiceBrowser"
	avahiServerIface                 = avahiService + ".Server"
	avahiEntryGroupEstablished int32 = 2
	avahiEntryGroupCollision   int32 = 3
	avahiEntryGroupFailure     int32 = 4
)

// avahiEntryGroupWait bounds how long a publish waits for the group to finish
// probing. A variable so tests need no wall-clock wait.
var avahiEntryGroupWait = 3 * time.Second

// avahiBrowserQueue bounds one browse stream. D-Bus delivery must not block on
// a slow gRPC consumer, so a full queue drops the oldest event and records the
// drop in the daemon log; the newest state is the useful one.
const avahiBrowserQueue = 64

func openServiceBackend(logs *logbus.Bus, platform string) serviceBackend {
	if platform == "" || platform == "simulator" {
		return unsupportedServices{reason: "mDNS service discovery is disabled in the simulator"}
	}
	return &avahiServiceBackend{logs: logs}
}

type avahiServiceBackend struct {
	logs *logbus.Bus

	mu sync.Mutex
	// freeGroupFn releases one published entry group on the bus. It is a field
	// so the retry ordering in unpublish can be exercised without a live
	// Avahi responder.
	freeGroupFn func(context.Context, dbus.ObjectPath) error
	conn        *dbus.Conn
	pubs        map[string]avahiPublication
	browsers    map[*avahiBrowser]struct{}
	closed      bool
}

type avahiPublication struct {
	path dbus.ObjectPath
}

func (b *avahiServiceBackend) bus(ctx context.Context) (*dbus.Conn, dbus.BusObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, ErrResponderLost
	}
	if b.conn == nil {
		conn, err := dbus.SystemBus()
		if err != nil {
			return nil, nil, fmt.Errorf("%w: system D-Bus is unavailable: %v", ErrUnsupported, err)
		}
		b.conn = conn
		b.freeGroupFn = freeEntryGroupOn(conn)
		if b.pubs == nil {
			b.pubs = map[string]avahiPublication{}
		}
		if b.browsers == nil {
			b.browsers = map[*avahiBrowser]struct{}{}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return b.conn, b.conn.Object(avahiService, avahiPath), nil
}

// requireRunning returns the server object only when Avahi reports RUNNING.
// REGISTERING is waited out for the bounded boot window; COLLISION and FAILURE
// are reported as an explicit not-ready error and never as success.
func (b *avahiServiceBackend) requireRunning(ctx context.Context) (dbus.BusObject, error) {
	_, obj, err := b.bus(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := avahiState(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("%w: Avahi is not reachable: %v", ErrUnsupported, err)
	}
	state, err = awaitAvahiRunning(ctx, dbusAvahiServer{obj: obj}, state)
	if err != nil {
		return nil, err
	}
	if state != stateRunning {
		return nil, fmt.Errorf("%w: Avahi is %s", ErrResponderNotReady, avahiStateName(state))
	}
	return obj, nil
}

func (b *avahiServiceBackend) publish(ctx context.Context, svc Service) (Publication, error) {
	obj, err := b.requireRunning(ctx)
	if err != nil {
		return Publication{}, err
	}
	var groupPath dbus.ObjectPath
	call := obj.CallWithContext(ctx, avahiServerIface+".EntryGroupNew", 0)
	if call.Err != nil {
		return Publication{}, fmt.Errorf("%w: create entry group: %v", ErrResponderNotReady, call.Err)
	}
	if err := call.Store(&groupPath); err != nil {
		return Publication{}, fmt.Errorf("decode entry group: %w", err)
	}
	group := b.groupObject(groupPath)
	txt := avahiTXT(svc.TXT)
	add := group.CallWithContext(ctx, avahiEntryGroupIface+".AddService", 0,
		int32(-1), int32(-1), uint32(0),
		svc.Instance, svc.Type, svc.Domain, "", uint16(svc.Port), txt)
	if add.Err != nil {
		b.freeGroup(groupPath)
		return Publication{}, avahiServiceError("add service", add.Err)
	}
	if err := group.CallWithContext(ctx, avahiEntryGroupIface+".Commit", 0).Err; err != nil {
		b.freeGroup(groupPath)
		return Publication{}, avahiServiceError("commit service", err)
	}
	if err := b.awaitEstablished(ctx, group, svc.Instance); err != nil {
		// A group that never reached ESTABLISHED is not published, so the
		// caller must not receive a publication ID.
		b.freeGroup(groupPath)
		return Publication{}, err
	}
	host := ""
	if fqdn, fqdnErr := avahiStringCall(ctx, obj, avahiServerIface+".GetHostNameFqdn"); fqdnErr == nil {
		host = strings.TrimSuffix(fqdn, ".")
	}
	pub := Publication{ID: ids.New("pub"), Service: svc, Host: host}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.freeGroup(groupPath)
		return Publication{}, ErrResponderLost
	}
	if b.pubs == nil {
		b.pubs = map[string]avahiPublication{}
	}
	b.pubs[pub.ID] = avahiPublication{path: groupPath}
	b.mu.Unlock()
	return pub, nil
}

// awaitEstablished waits for Avahi's probing to finish. Commit success alone
// does not prove the registration is live: a colliding instance ends in
// COLLISION and a failed registration in FAILURE.
func (b *avahiServiceBackend) awaitEstablished(ctx context.Context, group dbus.BusObject, instance string) error {
	deadline := time.Now().Add(avahiEntryGroupWait)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := avahiEntryGroupState(ctx, group)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("%w: read entry group state: %v", ErrResponderNotReady, err)
		}
		switch state {
		case avahiEntryGroupEstablished:
			return nil
		case avahiEntryGroupCollision:
			return fmt.Errorf("%w: instance %q collides with another registration", ErrNameInUse, instance)
		case avahiEntryGroupFailure:
			return fmt.Errorf("%w: Avahi failed to register the service", ErrResponderNotReady)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: service registration did not become established", ErrResponderNotReady)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *avahiServiceBackend) groupObject(path dbus.ObjectPath) dbus.BusObject {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	return conn.Object(avahiService, path)
}

// freeEntryGroupOn returns the entry-group Free call for one bus connection.
// Callers hold the connection, so it is never nil here.
func freeEntryGroupOn(conn *dbus.Conn) func(context.Context, dbus.ObjectPath) error {
	return func(ctx context.Context, path dbus.ObjectPath) error {
		return conn.Object(avahiService, path).CallWithContext(ctx, avahiEntryGroupIface+".Free", 0).Err
	}
}

func (b *avahiServiceBackend) freeGroup(path dbus.ObjectPath) {
	b.mu.Lock()
	free := b.freeGroupFn
	b.mu.Unlock()
	if free == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = free(ctx, path)
}

func (b *avahiServiceBackend) unpublish(ctx context.Context, id string) (bool, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false, nil
	}
	pub, ok := b.pubs[id]
	free := b.freeGroupFn
	b.mu.Unlock()
	if !ok {
		return false, nil
	}
	if free == nil {
		return false, ErrResponderLost
	}
	err := free(ctx, pub.path)
	if err != nil && !avahiObjectGone(err) {
		// Keep the tracker: a transient D-Bus failure leaves the entry group
		// live, and a daemon retry must still be able to free it. Forgetting
		// the publication here would strand the advertisement until the
		// daemon exits and answer every retry with (false, nil).
		return false, fmt.Errorf("free mDNS entry group: %w", err)
	}
	// Freed, or definitively gone: Avahi restarted or the group was already
	// released, so the registration the caller asked to remove no longer
	// exists. Only now may the tracker go.
	b.mu.Lock()
	delete(b.pubs, id)
	b.mu.Unlock()
	return true, nil
}

func (b *avahiServiceBackend) browse(ctx context.Context, q Query) (Browser, error) {
	obj, err := b.requireRunning(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	conn := b.conn
	closed := b.closed
	b.mu.Unlock()
	if closed || conn == nil {
		return nil, ErrResponderLost
	}

	br := &avahiBrowser{
		logs: b.logs, conn: conn, owner: b, events: make(chan Event, avahiBrowserQueue),
		sigch: make(chan *dbus.Signal, avahiBrowserQueue),
		done:  make(chan struct{}), query: q,
	}
	br.ctx, br.cancel = context.WithCancel(ctx)
	conn.Signal(br.sigch)
	// Install the matches before creating the browser so an ItemNew emitted
	// immediately after creation cannot be missed. Signals for other browsers
	// are filtered by object path.
	if err := br.addMatches(ctx); err != nil {
		br.removeMatches()
		br.cancel()
		return nil, err
	}
	call := obj.CallWithContext(ctx, avahiServerIface+".ServiceBrowserNew", 0,
		int32(-1), int32(-1), q.Type, q.Domain, uint32(0))
	if call.Err != nil {
		br.removeMatches()
		br.cancel()
		return nil, fmt.Errorf("%w: create service browser: %v", ErrResponderNotReady, call.Err)
	}
	if err := call.Store(&br.path); err != nil {
		br.removeMatches()
		br.cancel()
		return nil, fmt.Errorf("decode service browser: %w", err)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		br.removeMatches()
		br.cancel()
		br.free()
		return nil, ErrResponderLost
	}
	b.browsers[br] = struct{}{}
	b.mu.Unlock()
	go br.run()
	return br, nil
}

func (b *avahiServiceBackend) close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	pubs := b.pubs
	browsers := make([]*avahiBrowser, 0, len(b.browsers))
	for br := range b.browsers {
		browsers = append(browsers, br)
	}
	b.pubs = nil
	b.browsers = nil
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for id, pub := range pubs {
		if err := b.unpublishPath(ctx, pub.path); err != nil {
			if b.logs != nil {
				b.logs.Warnf("mdns", "", "free publication %s: %v", id, err)
			}
		}
	}
	for _, br := range browsers {
		_ = br.Close()
	}
	return nil
}

func (b *avahiServiceBackend) unpublishPath(ctx context.Context, path dbus.ObjectPath) error {
	b.mu.Lock()
	free := b.freeGroupFn
	b.mu.Unlock()
	if free == nil {
		return nil
	}
	return free(ctx, path)
}

func (b *avahiServiceBackend) forgetBrowser(br *avahiBrowser) {
	b.mu.Lock()
	delete(b.browsers, br)
	b.mu.Unlock()
}

// --- browser ---------------------------------------------------------------

type avahiBrowser struct {
	owner    *avahiServiceBackend
	logs     *logbus.Bus
	conn     *dbus.Conn
	path     dbus.ObjectPath
	query    Query
	ctx      context.Context
	cancel   context.CancelFunc
	events   chan Event
	sigch    chan *dbus.Signal
	done     chan struct{}
	closeOne sync.Once
	drops    int

	mu  sync.Mutex
	err error
}

func (br *avahiBrowser) Events() <-chan Event { return br.events }

func (br *avahiBrowser) Err() error {
	br.mu.Lock()
	defer br.mu.Unlock()
	return br.err
}

func (br *avahiBrowser) addMatches(ctx context.Context) error {
	options := []dbus.MatchOption{
		dbus.WithMatchSender(avahiService),
		dbus.WithMatchInterface(avahiServiceBrowserIface),
		dbus.WithMatchMember("ItemNew"),
	}
	if err := br.conn.AddMatchSignalContext(ctx, options...); err != nil {
		return fmt.Errorf("%w: subscribe to mDNS browse events: %v", ErrResponderNotReady, err)
	}
	if err := br.conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(avahiService),
		dbus.WithMatchInterface(avahiServiceBrowserIface), dbus.WithMatchMember("ItemRemove")); err != nil {
		_ = br.conn.RemoveMatchSignalContext(ctx, options...)
		return fmt.Errorf("%w: subscribe to mDNS browse events: %v", ErrResponderNotReady, err)
	}
	if err := br.conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(avahiService),
		dbus.WithMatchObjectPath(avahiPath), dbus.WithMatchInterface(avahiServerIface),
		dbus.WithMatchMember("StateChanged")); err != nil {
		_ = br.conn.RemoveMatchSignalContext(ctx, options...)
		return fmt.Errorf("%w: subscribe to mDNS responder state: %v", ErrResponderNotReady, err)
	}
	return nil
}

// removeMatches removes every match addMatches installed. It is safe to call
// before a browser path exists; path filtering happens in run.
func (br *avahiBrowser) removeMatches() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	options := []dbus.MatchOption{
		dbus.WithMatchSender(avahiService),
		dbus.WithMatchInterface(avahiServiceBrowserIface),
		dbus.WithMatchMember("ItemNew"),
	}
	_ = br.conn.RemoveMatchSignalContext(ctx, options...)
	_ = br.conn.RemoveMatchSignalContext(ctx,
		dbus.WithMatchSender(avahiService),
		dbus.WithMatchInterface(avahiServiceBrowserIface),
		dbus.WithMatchMember("ItemRemove"))
	_ = br.conn.RemoveMatchSignalContext(ctx,
		dbus.WithMatchSender(avahiService),
		dbus.WithMatchObjectPath(avahiPath),
		dbus.WithMatchInterface(avahiServerIface),
		dbus.WithMatchMember("StateChanged"))
	br.conn.RemoveSignal(br.signals())
}

// signals needs the same channel object the run loop uses; it is stored before
// matching so removal always refers to it.
func (br *avahiBrowser) signals() chan *dbus.Signal { return br.sigch }

func (br *avahiBrowser) free() {
	if br.path == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = br.conn.Object(avahiService, br.path).CallWithContext(ctx, avahiServiceBrowserIface+".Free", 0).Err
}

func (br *avahiBrowser) run() {
	defer close(br.done)
	defer close(br.events)
	sigch := br.signals()
	for {
		select {
		case <-br.ctx.Done():
			return
		case sig, ok := <-sigch:
			if !ok {
				br.setErr(ErrResponderLost)
				return
			}
			if sig == nil {
				continue
			}
			switch sig.Name {
			case avahiServerIface + ".StateChanged":
				// Only a terminal state ends the stream. Avahi goes
				// RUNNING → REGISTERING → RUNNING when the host name is
				// re-applied, so a settings rename must not kill a live
				// browse stream or tear down its matches. A real bus loss
				// surfaces as a closed signal channel or a failed call
				// instead.
				if state, ok := avahiStateFromSignal(sig); ok && avahiResponderLostState(state) {
					br.setErr(fmt.Errorf("%w: Avahi is %s", ErrResponderLost, avahiStateName(state)))
					return
				}
			case avahiServiceBrowserIface + ".ItemNew":
				if !br.forPath(sig) {
					continue
				}
				if name, ok := avahiBrowserName(sig); ok {
					br.resolve(name)
				}
			case avahiServiceBrowserIface + ".ItemRemove":
				if !br.forPath(sig) {
					continue
				}
				if name, ok := avahiBrowserName(sig); ok {
					br.push(Event{Kind: EventRemoved, Name: name, Type: br.query.Type, Domain: br.query.Domain})
				}
			}
		}
	}
}

func (br *avahiBrowser) forPath(sig *dbus.Signal) bool {
	return sig.Path == br.path
}

func avahiBrowserName(sig *dbus.Signal) (string, bool) {
	// (i interface, i protocol, s name, s type, s domain, u flags)
	if len(sig.Body) < 3 {
		return "", false
	}
	name, ok := sig.Body[2].(string)
	return name, ok && name != ""
}

func (br *avahiBrowser) resolve(name string) {
	obj := br.conn.Object(avahiService, avahiPath)
	call := obj.CallWithContext(br.ctx, avahiServerIface+".ResolveService", 0,
		int32(-1), int32(-1), name, br.query.Type, br.query.Domain, int32(-1), uint32(0))
	if call.Err != nil {
		if br.logs != nil && br.ctx.Err() == nil {
			br.logs.Warnf("mdns", "", "resolve %s: %v", name, call.Err)
		}
		return
	}
	var (
		iface, proto, aproto, flags int32
		resolvedName, typ, domain   string
		host, address               string
		port                        uint16
		txt                         [][]byte
	)
	if err := call.Store(&iface, &proto, &resolvedName, &typ, &domain, &host, &aproto, &address, &port, &txt, &flags); err != nil {
		if br.logs != nil {
			br.logs.Warnf("mdns", "", "decode resolved service %s: %v", name, err)
		}
		return
	}
	if resolvedName == "" {
		resolvedName = name
	}
	event := Event{
		Kind: EventAdded, Name: resolvedName, Type: typ, Domain: domain,
		Host: strings.TrimSuffix(host, "."), Port: uint32(port), TXT: decodeAvahiTXT(txt),
	}
	if address != "" {
		event.Addresses = []string{address}
	}
	br.push(event)
}

// push enqueues an event without blocking the D-Bus signal loop. A full queue
// drops its oldest entry: browse streams are state notifications, and the
// newest event is the one a UI can act on.
func (br *avahiBrowser) push(ev Event) {
	select {
	case br.events <- ev:
		return
	default:
	}
	select {
	case <-br.events:
		br.drops++
	default:
	}
	select {
	case br.events <- ev:
	default:
	}
}

func (br *avahiBrowser) setErr(err error) {
	br.mu.Lock()
	if br.err == nil {
		br.err = err
	}
	br.mu.Unlock()
}

func (br *avahiBrowser) Close() error {
	br.closeOne.Do(func() {
		br.cancel()
		br.free()
		br.removeMatches()
		<-br.done
		if br.owner != nil {
			br.owner.forgetBrowser(br)
		}
		if br.drops > 0 && br.logs != nil {
			br.logs.Warnf("mdns", "", "browse %s dropped %d events: consumer too slow", br.query.Type, br.drops)
		}
	})
	return br.Err()
}

// avahiEntryGroupState reads the public GetState property method.
func avahiEntryGroupState(ctx context.Context, group dbus.BusObject) (int32, error) {
	call := group.CallWithContext(ctx, avahiEntryGroupIface+".GetState", 0)
	if call.Err != nil {
		return 0, call.Err
	}
	var state int32
	if err := call.Store(&state); err != nil {
		return 0, err
	}
	return state, nil
}

func avahiTXT(txt map[string]string) [][]byte {
	if len(txt) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(txt))
	for key, value := range txt {
		out = append(out, []byte(key+"="+value))
	}
	return out
}

func decodeAvahiTXT(records [][]byte) map[string]string {
	if len(records) == 0 {
		return nil
	}
	out := make(map[string]string, len(records))
	for _, record := range records {
		key, value, found := strings.Cut(string(record), "=")
		if !found {
			if len(record) > 0 {
				out[key] = ""
			}
			continue
		}
		if key == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// avahiServiceError classifies a D-Bus error from a publish call. A rejected
// record or instance is the request's fault and will fail again; saying the
// responder is "not running" for it hid a TXT-size defect in the field. A name
// collision is its own condition so the caller can choose a new instance.
// Other failures stay retryable as an unready responder.
func avahiServiceError(op string, err error) error {
	// AddService/Commit return the raw D-Bus error, not the avahiDMSError the
	// host-name setter wraps, so read the name from the D-Bus type directly.
	switch dbusErrorName(err) {
	case "org.freedesktop.Avahi.InvalidRecordError",
		"org.freedesktop.Avahi.InvalidServiceNameError",
		"org.freedesktop.Avahi.InvalidServiceTypeError",
		"org.freedesktop.Avahi.InvalidPortError",
		"org.freedesktop.Avahi.InvalidKeyError",
		"org.freedesktop.Avahi.InvalidRDataError",
		"org.freedesktop.Avahi.InvalidArgumentError":
		return fmt.Errorf("%w: %s: %v", ErrInvalidRequest, op, err)
	case "org.freedesktop.Avahi.CollisionError":
		return fmt.Errorf("%w: %s: the instance name is already registered on the link", ErrNameInUse, op)
	}
	return fmt.Errorf("%w: %s: %v", ErrResponderNotReady, op, err)
}

func avahiObjectGone(err error) bool {
	switch dbusErrorName(err) {
	case "org.freedesktop.DBus.Error.UnknownObject",
		"org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.Avahi.InvalidObjectPathError":
		return true
	}
	return false
}
