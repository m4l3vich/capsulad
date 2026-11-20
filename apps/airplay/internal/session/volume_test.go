package session

// The device master is reported to the sender with the reference's dvlc
// notification. The notification must survive a startup where the media
// session exists before the sender's event channel; a receiver-side change is
// one authoritative value, never an artificial ramp. These tests exercise that
// state machine without a live ticker or a device.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/plist"
	"capsulad/apps/airplay/internal/rtsp"
)

// volumeSession returns a session whose master reads the supplied level.
func volumeSession(reader func() (float64, bool)) *Session {
	s := &Session{done: make(chan struct{})}
	s.m = &Manager{cfg: Config{MasterVolumeContext: func(context.Context) (float64, bool) {
		return reader()
	}}, logf: func(string, ...any) {}}
	return s
}

// seedMaster simulates updateInfo for the current event channel: it reads the
// master and records it only in this session's delivered/display state. It
// deliberately leaves the manager cache alone so tests can see the difference
// between a sender-facing seed and an authoritative read.
func seedMaster(s *Session) {
	if unit, ok := s.m.readMasterVolume(s.masterVolumeContext()); ok {
		s.noteSeedMaster(unit)
	}
}

// idleStream is a Stream for sessions that never read or write a socket.
type idleStream struct{}

func (idleStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (idleStream) Write(p []byte) (int, error) { return len(p), nil }
func (idleStream) Close() error                { return nil }
func (idleStream) Peer() Peer                  { return Peer{} }

// blockingEventWriter holds one outbound dvlc inside Write so a control-socket
// SET can arrive before the write returns.
type blockingEventWriter struct {
	buf     bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingEventWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.buf.Write(p)
}

func attachEventWriter(s *Session) *captureEventWriter {
	writer := &captureEventWriter{}
	attachWriter(s, writer)
	return writer
}

func attachWriter(s *Session, writer io.Writer) {
	s.eventMu.Lock()
	s.eventOut = writer
	s.eventReady = true
	s.eventGen++
	s.eventMu.Unlock()
}

// failingEventStream is an encrypted event peer whose underlying write can be
// made to fail after a configured number of successful calls.
type failingEventStream struct {
	failAt   int
	attempts int
	closed   bool
	session  *Session
	swapTo   *captureEventWriter
}

func (f *failingEventStream) Write(p []byte) (int, error) {
	f.attempts++
	if f.swapTo != nil {
		// Simulate a replacement publish landing while this write is in
		// progress; retirement must leave the replacement alone.
		replacement := f.swapTo
		f.swapTo = nil
		f.session.eventMu.Lock()
		f.session.eventOut = replacement
		f.session.eventReady = true
		f.session.eventGen++
		f.session.eventMu.Unlock()
	}
	if f.attempts > f.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (f *failingEventStream) Read([]byte) (int, error) { return 0, io.EOF }
func (f *failingEventStream) Close() error             { f.closed = true; return nil }
func (f *failingEventStream) Peer() Peer               { return Peer{} }

// updateInfoSession has the minimum manager state the event update document
// reads: identity, name/model/source version, and a 32-byte pairing key.
func updateInfoSession(t *testing.T) *Session {
	t.Helper()
	store := &pair.Store{Device: &pair.Device{PairingID: "11111111-2222-3333-4444-555555555555"}}
	for i := range store.Device.LTPK {
		store.Device.LTPK[i] = byte(i + 1)
	}
	m := &Manager{
		cfg: Config{
			DeviceID:            "aabbccddeeff",
			Name:                "Test Speaker",
			Model:               "Test1,1",
			SourceVersion:       "366.0",
			MasterVolumeContext: func(context.Context) (float64, bool) { return 0.05, true },
		},
		store: store,
		logf:  func(string, ...any) {},
	}
	return &Session{m: m, done: make(chan struct{})}
}

func attachEncryptedEventChannel(t *testing.T, s *Session, stream Stream) (*hapcrypt.Session, uint64) {
	t.Helper()
	key := make([]byte, 32)
	hap, err := hapcrypt.NewSession(stream, key, key)
	if err != nil {
		t.Fatalf("hapcrypt.NewSession: %v", err)
	}
	s.eventMu.Lock()
	s.eventOut = hap
	s.eventConn = stream
	s.eventReady = true
	s.eventGen++
	generation := s.eventGen
	s.eventMu.Unlock()
	return hap, generation
}

// swappingEventWriter publishes a replacement channel during its first write,
// simulating a reply that is already being written when the channel changes.
type swappingEventWriter struct {
	session     *Session
	replacement *captureEventWriter
	buf         bytes.Buffer
	writes      int
}

func (w *swappingEventWriter) Write(p []byte) (int, error) {
	w.writes++
	n, err := w.buf.Write(p)
	if w.replacement != nil {
		replacement := w.replacement
		w.replacement = nil
		w.session.eventMu.Lock()
		w.session.eventOut = replacement
		w.session.eventReady = true
		w.session.eventGen++
		w.session.eventMu.Unlock()
	}
	return n, err
}

// postedUnitVolumes decodes every dvlc POST /command body written to the
// capture writer.
func postedUnitVolumes(t *testing.T, data []byte) []float64 {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(data))
	var volumes []float64
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read request line: %v", err)
		}
		if line != "POST /command RTSP/1.0\r\n" {
			t.Fatalf("request line = %q", line)
		}
		length := 0
		for {
			line, err = reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read header: %v", err)
			}
			if line == "\r\n" {
				break
			}
			name, value, found := strings.Cut(line, ":")
			if found && strings.EqualFold(name, "Content-Length") {
				if _, err := fmtSscan(strings.TrimSpace(value), &length); err != nil {
					t.Fatalf("content length %q: %v", value, err)
				}
			}
		}
		if length == 0 {
			t.Fatal("dvlc request has no body")
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		value, err := plist.Decode(body)
		if err != nil {
			t.Fatalf("decode body: %v", err)
		}
		dict, _ := value.(map[string]any)
		if dict["type"] != "sendMediaRemoteCommand" || dict["value"] != "dvlc" {
			t.Fatalf("notification = %v", dict)
		}
		volume, ok := dict["volume"].(float64)
		if !ok {
			t.Fatalf("volume = %v (%T)", dict["volume"], dict["volume"])
		}
		volumes = append(volumes, volume)
	}
	return volumes
}

// Without an event channel there is nowhere to deliver a notification, and
// nothing is recorded as sent. When the channel arrives, its updateInfo reports
// the current master; the watcher then sends no duplicate.
func TestMasterVolumeNotificationWaitsForEventChannel(t *testing.T) {
	s := volumeSession(func() (float64, bool) { return 0.05, true })
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped without an event channel")
	}
	writer := attachEventWriter(s)
	seedMaster(s)
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped with an event channel")
	}
	if len(writer.buf.Bytes()) != 0 {
		t.Fatalf("startup master was echoed after updateInfo: %v", postedUnitVolumes(t, writer.buf.Bytes()))
	}
}

// The event channel exists before its updateInfo is written; a dvlc must wait
// so the document that sets the sender's slider is not preceded by a volume
// notification it would overwrite. Once updateInfo has run, the same value is
// not repeated.
func TestMasterVolumeNotificationWaitsForReadyChannel(t *testing.T) {
	s := volumeSession(func() (float64, bool) { return 0.05, true })
	writer := &captureEventWriter{}
	s.eventMu.Lock()
	s.eventOut = writer
	s.eventMu.Unlock()
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped before updateInfo")
	}
	if len(writer.buf.Bytes()) != 0 {
		t.Fatal("volume notification was sent before updateInfo")
	}
	s.eventMu.Lock()
	s.eventReady = true
	s.eventGen++
	s.eventMu.Unlock()
	seedMaster(s)
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped after updateInfo")
	}
	if len(writer.buf.Bytes()) != 0 {
		t.Fatalf("post-updateInfo pass sent %v, want no echo", postedUnitVolumes(t, writer.buf.Bytes()))
	}
}

// A receiver-side change is mirrored as one authoritative dvlc at the new
// master, not as artificial intermediate slider values.
func TestMasterVolumeNotificationSendsOneAuthoritativeValue(t *testing.T) {
	master := 0.05
	s := volumeSession(func() (float64, bool) { return master, true })
	writer := attachEventWriter(s)
	seedMaster(s)
	master = 0.5
	if !s.pollMasterVolume() {
		t.Fatalf("pollMasterVolume stopped")
	}
	volumes := postedUnitVolumes(t, writer.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.5) > 1e-9 {
		t.Fatalf("local change sent %v, want exactly [0.5]", volumes)
	}
	// The same level is not re-sent on the next poll.
	before := len(writer.buf.Bytes())
	if !s.pollMasterVolume() {
		t.Fatal("repeat pollMasterVolume stopped")
	}
	if after := len(writer.buf.Bytes()); after != before {
		t.Fatalf("unchanged master was re-sent (%d -> %d bytes)", before, after)
	}
}

// A replacement event channel has just been given the current master by its
// updateInfo, so it must not receive an unsolicited snapshot; a later local
// change is mirrored on that channel.
func TestMasterVolumeNotificationSeedsOnReconnect(t *testing.T) {
	master := 0.05
	s := volumeSession(func() (float64, bool) { return master, true })
	first := attachEventWriter(s)
	seedMaster(s)
	if !s.pollMasterVolume() {
		t.Fatal("first poll stopped")
	}
	if len(first.buf.Bytes()) != 0 {
		t.Fatalf("first channel received %v, want no startup echo", postedUnitVolumes(t, first.buf.Bytes()))
	}

	second := attachEventWriter(s)
	seedMaster(s)
	if !s.pollMasterVolume() {
		t.Fatal("reconnect poll stopped")
	}
	if len(second.buf.Bytes()) != 0 {
		t.Fatalf("reconnect received %v, want no unsolicited snapshot", postedUnitVolumes(t, second.buf.Bytes()))
	}

	master = 0.3
	if !s.pollMasterVolume() {
		t.Fatal("local-change poll stopped")
	}
	volumes := postedUnitVolumes(t, second.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.3) > 1e-9 {
		t.Fatalf("local change volumes = %v, want exactly [0.3]", volumes)
	}
}

// A notification captured for one channel must never fall through to a
// replacement: the bound helper validates writer and generation under the
// event write lock.
func TestMasterVolumeNotificationBindsCapturedWriter(t *testing.T) {
	s := volumeSession(func() (float64, bool) { return 0.05, true })
	old := attachEventWriter(s)
	out, generation, ready := s.readyEventWriter()
	if !ready {
		t.Fatal("first channel is not ready")
	}
	replacement := attachEventWriter(s)

	if s.sendUnitVolumeTo(out, generation, 0.42) {
		t.Fatal("a notification bound to the replaced channel was accepted")
	}
	if len(old.buf.Bytes()) != 0 {
		t.Fatal("the replaced channel was written")
	}
	if len(replacement.buf.Bytes()) != 0 {
		t.Fatal("the replacement channel received a stale notification")
	}
	if !s.sendUnitVolumeTo(replacement, generation+1, 0.42) {
		t.Fatal("the replacement channel's own notification was refused")
	}
	if len(replacement.buf.Bytes()) == 0 {
		t.Fatal("the replacement channel received no notification")
	}
}

// A held reply from an old reader must not land on a replacement channel,
// including when the replacement is published during the reply write itself.
func TestStaleEventReplyCannotHitReplacement(t *testing.T) {
	s := volumeSession(func() (float64, bool) { return 0.05, true })
	replacement := &captureEventWriter{}
	old := &swappingEventWriter{session: s, replacement: replacement}
	s.eventMu.Lock()
	s.eventOut = old
	s.eventReady = true
	s.eventGen = 1
	s.eventMu.Unlock()

	reply := &rtsp.Response{Status: 200}
	s.writeEventTo(old, 1, reply)
	if old.writes != 1 {
		t.Fatalf("old writer got %d writes, want its own reply", old.writes)
	}
	if replacement.buf.Len() != 0 {
		t.Fatal("old reply leaked into the replacement channel")
	}
	if _, generation, ready := s.readyEventWriter(); !ready || generation != 2 {
		t.Fatalf("replacement state = (ready %v, gen %d), want (true, 2)", ready, generation)
	}

	// A delayed duplicate from the old reader is stale and must be dropped.
	before := old.writes
	s.writeEventTo(old, 1, reply)
	if old.writes != before {
		t.Fatal("stale reply was written to the old peer")
	}
	if replacement.buf.Len() != 0 {
		t.Fatal("stale reply leaked into the replacement channel")
	}
	// The replacement's own reply still works.
	s.writeEventTo(replacement, 2, reply)
	if replacement.buf.Len() == 0 {
		t.Fatal("replacement reply was refused")
	}
}

// A reply that would queue behind eventWriteMu must validate after it acquires
// the lock: a replacement published while it waited makes the reply stale.
func TestQueuedEventReplyCannotHitReplacement(t *testing.T) {
	s := volumeSession(func() (float64, bool) { return 0.05, true })
	old := attachEventWriter(s)
	out, generation, ready := s.readyEventWriter()
	if !ready {
		t.Fatal("old channel is not ready")
	}

	// Simulate the reply waiting on the write lock while a replacement lands.
	s.eventWriteMu.Lock()
	replacement := &captureEventWriter{}
	s.eventMu.Lock()
	s.eventOut = replacement
	s.eventReady = true
	s.eventGen++
	s.eventMu.Unlock()
	s.eventWriteMu.Unlock()

	s.writeEventTo(out, generation, &rtsp.Response{Status: 200})
	if len(old.buf.Bytes()) != 0 || len(replacement.buf.Bytes()) != 0 {
		t.Fatal("queued stale reply reached an old or replacement channel")
	}
}

// State application and replacement publication share eventApplyMu: a body
// that starts first finishes before the replacement, while a body that arrives
// after the replacement is dropped. TryLock proves the lock is held across the
// whole application, not released after validation.
func TestEventStateApplicationSerializesReplacement(t *testing.T) {
	s := volumeSession(func() (float64, bool) { return 0.05, true })
	attachEventWriter(s)
	out, generation, ready := s.readyEventWriter()
	if !ready {
		t.Fatal("old channel is not ready")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	applied := make(chan bool, 1)
	go func() {
		applied <- s.applyEventState(out, generation, func() {
			close(started)
			<-release
		})
	}()
	<-started
	if s.eventApplyMu.TryLock() {
		s.eventApplyMu.Unlock()
		t.Fatal("eventApplyMu was not held across state application")
	}

	replacement := &captureEventWriter{}
	published := make(chan bool, 1)
	go func() {
		_, ok := s.replaceEventChannel(nil, nil, replacement, nil)
		published <- ok
	}()
	select {
	case <-published:
		t.Fatal("the replacement was published while a body was being applied")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if !<-applied {
		t.Fatal("a body that started before the replacement was dropped")
	}
	if !<-published {
		t.Fatal("the replacement was not published")
	}

	called := false
	if s.applyEventState(out, generation, func() { called = true }) {
		t.Fatal("a late body from the retired generation was applied")
	}
	if called {
		t.Fatal("the late body's state callback ran")
	}
	s.eventMu.Lock()
	current := s.eventOut
	s.eventMu.Unlock()
	if current != replacement {
		t.Fatal("the replacement is not the current writer")
	}
}

// The loop must read the reader captured at accept, not whatever s.eventIn
// points at after a replacement. An old EOF reader must leave the replacement
// reader and writer untouched.
func TestEventReadLoopUsesAcceptedReader(t *testing.T) {
	s := &Session{done: make(chan struct{}), m: &Manager{}}
	replacementReader := bufio.NewReader(strings.NewReader("OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n"))
	s.eventIn = replacementReader
	writer := attachEventWriter(s)

	s.eventReadLoop(bufio.NewReader(strings.NewReader("")), writer, s.eventGen)
	if _, err := s.eventIn.Peek(1); err != nil {
		t.Fatalf("replacement reader was consumed: %v", err)
	}
	if len(writer.buf.Bytes()) != 0 {
		t.Fatal("the old loop replied through the replacement writer")
	}
}

// A request already read from an old reader must not mutate the replacement
// session or produce a reply.
func TestStaleEventRequestIsDropped(t *testing.T) {
	s := volumeSession(func() (float64, bool) { return 0.05, true })
	body, err := plist.EncodeBinary(map[string]any{
		"params": map[string]any{"kMRMediaRemoteNowPlayingInfoPlaybackRate": int64(1)},
	})
	if err != nil {
		t.Fatalf("encode request body: %v", err)
	}
	request := "POST /command RTSP/1.0\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\nCSeq: 1\r\n\r\n" + string(body)
	writer := attachEventWriter(s)

	s.eventReadLoop(bufio.NewReader(strings.NewReader(request)), &captureEventWriter{}, s.eventGen-1)
	if s.rate != 0 {
		t.Fatalf("stale request changed the session rate to %v", s.rate)
	}
	if len(writer.buf.Bytes()) != 0 {
		t.Fatal("stale request produced a reply on the replacement channel")
	}
}

// HAP advances its outbound nonce before the underlying write, so a failed
// write must retire the exact peer. A replacement channel must still work, and
// no other event write may touch the retired peer.
func TestMasterVolumeNotificationRetiresFailedEncryptedChannel(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		master := 0.05
		s := volumeSession(func() (float64, bool) { return master, true })
		stream := &failingEventStream{failAt: 0}
		hap, generation := attachEncryptedEventChannel(t, s, stream)
		seedMaster(s)
		master = 0.3

		if !s.pollMasterVolume() {
			t.Fatal("watcher stopped")
		}
		if stream.attempts != 1 || !stream.closed {
			t.Fatalf("failed notification attempts = %d, closed = %v", stream.attempts, stream.closed)
		}
		if _, _, ready := s.readyEventWriter(); ready {
			t.Fatal("failed channel is still published")
		}

		attempts := stream.attempts
		if s.sendUnitVolumeTo(hap, generation, 0.42) {
			t.Fatal("the retired writer accepted another notification")
		}
		if s.postEventRequest([]byte("retired")) {
			t.Fatal("postEventRequest used the retired writer")
		}
		if ok, _ := s.sendRemoteCommand("play"); ok {
			t.Fatal("sendRemoteCommand used the retired writer")
		}
		s.writeEventTo(hap, generation, &rtsp.Response{Status: 200})
		if stream.attempts != attempts {
			t.Fatalf("retired writer was used again (%d -> %d attempts)", attempts, stream.attempts)
		}

		replacement := attachEventWriter(s)
		seedMaster(s)
		if !s.pollMasterVolume() {
			t.Fatal("replacement poll stopped")
		}
		if len(replacement.buf.Bytes()) != 0 {
			t.Fatalf("replacement received %v, want no echo after its updateInfo",
				postedUnitVolumes(t, replacement.buf.Bytes()))
		}
	})

	t.Run("local-change", func(t *testing.T) {
		master := 0.05
		s := volumeSession(func() (float64, bool) { return master, true })
		stream := &failingEventStream{failAt: 0}
		hap, generation := attachEncryptedEventChannel(t, s, stream)
		seedMaster(s)
		master = 0.3

		if !s.pollMasterVolume() {
			t.Fatalf("watcher stopped")
		}
		if stream.attempts != 1 || !stream.closed {
			t.Fatalf("failed notification attempts = %d, closed = %v", stream.attempts, stream.closed)
		}
		if _, _, ready := s.readyEventWriter(); ready {
			t.Fatal("failed channel is still published")
		}
		attempts := stream.attempts
		if s.sendUnitVolumeTo(hap, generation, 0.42) {
			t.Fatal("the retired writer accepted another notification")
		}
		if stream.attempts != attempts {
			t.Fatal("the failed notification was retried on the same peer")
		}

		replacement := attachEventWriter(s)
		seedMaster(s)
		if !s.pollMasterVolume() {
			t.Fatal("replacement poll stopped")
		}
		if len(replacement.buf.Bytes()) != 0 {
			t.Fatalf("replacement received %v, want no unsolicited snapshot",
				postedUnitVolumes(t, replacement.buf.Bytes()))
		}
	})
}

// A replacement published while the old HAP write fails must remain ready:
// retirement is identity-checked to the failed writer.
func TestFailedEncryptedWriteLeavesReplacementChannel(t *testing.T) {
	master := 0.05
	s := volumeSession(func() (float64, bool) { return master, true })
	replacement := &captureEventWriter{}
	stream := &failingEventStream{failAt: 0, session: s, swapTo: replacement}
	_, generation := attachEncryptedEventChannel(t, s, stream)
	seedMaster(s)
	master = 0.3

	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped")
	}
	if stream.attempts != 1 {
		t.Fatalf("failed write attempts = %d, want one", stream.attempts)
	}
	out, current, ready := s.readyEventWriter()
	if !ready || out != replacement || current == generation {
		t.Fatalf("replacement state = (ready %v, gen %d), want the replacement ready with a new generation", ready, current)
	}
	if !s.sendUnitVolumeTo(out, current, 0.42) {
		t.Fatal("the replacement channel was retired with the failed writer")
	}
	if len(replacement.buf.Bytes()) == 0 {
		t.Fatal("the replacement channel received no notification")
	}
}

// The event channel is published only after its updateInfo write succeeded.
// A failure retires the encrypted peer without making it ready; a successful
// write enables the first dvlc on that same channel.
func TestEventChannelPublishesOnlyAfterUpdateInfo(t *testing.T) {
	newChannel := func(t *testing.T, s *Session, stream *failingEventStream) *hapcrypt.Session {
		t.Helper()
		key := make([]byte, 32)
		hap, err := hapcrypt.NewSession(stream, key, key)
		if err != nil {
			t.Fatalf("hapcrypt.NewSession: %v", err)
		}
		s.eventMu.Lock()
		s.eventOut = hap
		s.eventConn = stream
		s.eventMu.Unlock()
		return hap
	}

	t.Run("failure", func(t *testing.T) {
		s := updateInfoSession(t)
		stream := &failingEventStream{failAt: 0}
		hap := newChannel(t, s, stream)

		if _, published := s.publishEventChannel(hap, stream); published {
			t.Fatal("a channel whose updateInfo failed was published")
		}
		if stream.attempts != 1 || !stream.closed {
			t.Fatalf("failed updateInfo attempts = %d, closed = %v", stream.attempts, stream.closed)
		}
		if _, _, ready := s.readyEventWriter(); ready {
			t.Fatal("failed updateInfo channel is ready")
		}
	})

	t.Run("success", func(t *testing.T) {
		s := updateInfoSession(t)
		stream := &failingEventStream{failAt: 100}
		hap := newChannel(t, s, stream)

		generation, published := s.publishEventChannel(hap, stream)
		if !published {
			t.Fatal("updateInfo did not publish a working encrypted channel")
		}
		if _, current, ready := s.readyEventWriter(); !ready || current != generation {
			t.Fatalf("channel state = (ready %v, generation %d), want (true, %d)", ready, current, generation)
		}
		attempts := stream.attempts
		if !s.pollMasterVolume() {
			t.Fatal("poll after updateInfo stopped")
		}
		if stream.attempts != attempts {
			t.Fatal("updateInfo master was echoed as an unsolicited dvlc")
		}
	})
}

// activeVolumeSession returns a promoted HAP-keyed session that owns the
// receiver, with the supplied master reader/writer. It uses the /info fixture
// (identity, store, pairing key) so real request handlers can be exercised.
func activeVolumeSession(t *testing.T, read func() (float64, bool), set func(context.Context, float64) (float64, error)) *Session {
	t.Helper()
	s := updateInfoSession(t)
	s.m.cfg.MasterVolumeContext = func(context.Context) (float64, bool) { return read() }
	s.m.cfg.SetMasterVolume = set
	masterCtx, masterCancel := context.WithCancel(context.Background())
	s.masterCtx = masterCtx
	s.masterCancel = masterCancel
	s.promoted.Store(true)
	s.mu.Lock()
	s.encrypted = true
	s.mu.Unlock()
	s.m.mu.Lock()
	s.m.active = s
	s.m.sessionActive = true
	s.m.mu.Unlock()
	return s
}

func textVolumeRequest(t *testing.T, s *Session, value string) *rtsp.Response {
	t.Helper()
	response, _, err := s.handleSetParameter(&rtsp.Request{
		Method:  "SET_PARAMETER",
		URI:     "rtsp://receiver/1",
		Proto:   "RTSP/1.0",
		Headers: []rtsp.Header{{Name: "Content-Type", Value: "text/parameters"}},
		Body:    []byte("volume: " + value + "\r\n"),
	})
	if err != nil {
		t.Fatalf("SET_PARAMETER volume: %v", err)
	}
	return response
}

func plistVolumeRequest(t *testing.T, s *Session, dB float64) *rtsp.Response {
	t.Helper()
	body, err := plist.EncodeBinary(map[string]any{"volume": dB})
	if err != nil {
		t.Fatalf("encode volume plist: %v", err)
	}
	response, _, err := s.handleSetParameter(&rtsp.Request{
		Method:  "SET_PARAMETER",
		URI:     "rtsp://receiver/1",
		Proto:   "RTSP/1.0",
		Headers: []rtsp.Header{{Name: "Content-Type", Value: "application/x-apple-binary-plist"}},
		Body:    body,
	})
	if err != nil {
		t.Fatalf("SET_PARAMETER plist volume: %v", err)
	}
	return response
}

func postedRequestBodies(t *testing.T, data []byte) [][]byte {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(data))
	var bodies [][]byte
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read request line: %v", err)
		}
		if line != "POST /command RTSP/1.0\r\n" {
			t.Fatalf("request line = %q", line)
		}
		length := 0
		for {
			line, err = reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read header: %v", err)
			}
			if line == "\r\n" {
				break
			}
			name, value, found := strings.Cut(line, ":")
			if found && strings.EqualFold(name, "Content-Length") {
				if _, err := fmtSscan(strings.TrimSpace(value), &length); err != nil {
					t.Fatalf("content length %q: %v", value, err)
				}
			}
		}
		if length == 0 {
			t.Fatal("event request has no body")
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

func postedDvlcVolumes(t *testing.T, data []byte) []float64 {
	t.Helper()
	bodies := postedRequestBodies(t, data)
	var volumes []float64
	for _, body := range bodies {
		value, err := plist.Decode(body)
		if err != nil {
			t.Fatalf("decode event body: %v", err)
		}
		dict, _ := value.(map[string]any)
		if dict["type"] != "sendMediaRemoteCommand" || dict["value"] != "dvlc" {
			continue
		}
		volume, ok := dict["volume"].(float64)
		if !ok {
			t.Fatalf("dvlc volume = %v (%T)", dict["volume"], dict["volume"])
		}
		volumes = append(volumes, volume)
	}
	return volumes
}

func infoInitialVolume(t *testing.T, s *Session) float64 {
	t.Helper()
	response, _, err := s.handleInfo(&rtsp.Request{
		Method: "GET", URI: "/info", Proto: "RTSP/1.0",
	})
	if err != nil {
		t.Fatalf("GET /info: %v", err)
	}
	value, err := plist.Decode(response.Body)
	if err != nil {
		t.Fatalf("decode /info: %v", err)
	}
	document, _ := value.(map[string]any)
	volume, ok := document["initialVolume"].(float64)
	if !ok {
		t.Fatalf("/info initialVolume = %v (%T)", document["initialVolume"], document["initialVolume"])
	}
	return volume
}

func getParameterVolume(t *testing.T, s *Session) string {
	t.Helper()
	response, _, err := s.handleGetParameter(&rtsp.Request{
		Method: "GET_PARAMETER", URI: "rtsp://receiver/1", Proto: "RTSP/1.0",
		Body: []byte("volume"),
	})
	if err != nil {
		t.Fatalf("GET_PARAMETER: %v", err)
	}
	return string(response.Body)
}

// A sender's SET_PARAMETER moves the one persisted master. Both wire forms
// (text and binary plist) take the same path; /info, updateInfo and
// GET_PARAMETER then report that same level in the AirPlay dB domain.
func TestSenderVolumeControlsOneMaster(t *testing.T) {
	master := 0.05
	var writes []float64
	read := func() (float64, bool) { return master, true }
	set := func(_ context.Context, level float64) (float64, error) {
		master = level
		writes = append(writes, level)
		return level, nil
	}
	s := activeVolumeSession(t, read, set)

	if got := infoInitialVolume(t, s); math.Abs(got-(-28.5)) > 1e-9 {
		t.Fatalf("/info initialVolume = %v, want -28.5 for a 5%% master", got)
	}
	if got := getParameterVolume(t, s); got != "\r\nvolume: -28.500000\r\n" {
		t.Fatalf("GET_PARAMETER body = %q", got)
	}

	if response := textVolumeRequest(t, s, "-15"); response.Status != 200 {
		t.Fatalf("text SET_PARAMETER status = %d", response.Status)
	}
	if len(writes) != 1 || math.Abs(writes[0]-0.5) > 1e-9 || math.Abs(master-0.5) > 1e-9 {
		t.Fatalf("text SET wrote %v, master = %v, want one 0.5", writes, master)
	}
	if got := getParameterVolume(t, s); got != "\r\nvolume: -15.000000\r\n" {
		t.Fatalf("GET_PARAMETER after text SET = %q", got)
	}
	if got := infoInitialVolume(t, s); math.Abs(got-(-15)) > 1e-9 {
		t.Fatalf("/info after text SET = %v, want -15", got)
	}

	if response := plistVolumeRequest(t, s, -12.5); response.Status != 200 {
		t.Fatalf("plist SET_PARAMETER status = %d", response.Status)
	}
	if len(writes) != 2 || math.Abs(writes[1]-(1-12.5/30)) > 1e-9 {
		t.Fatalf("plist SET wrote %v, want a second value 0.58333...", writes)
	}
	if got := getParameterVolume(t, s); got != "\r\nvolume: -12.500000\r\n" {
		t.Fatalf("GET_PARAMETER after plist SET = %q", got)
	}

	writer := attachEventWriter(s)
	seedMaster(s)
	if _, ok := s.sendUpdateInfo(); !ok {
		t.Fatal("updateInfo was not posted")
	}
	bodies := postedRequestBodies(t, writer.buf.Bytes())
	if len(bodies) != 1 {
		t.Fatalf("updateInfo bodies = %d, want one", len(bodies))
	}
	value, err := plist.Decode(bodies[0])
	if err != nil {
		t.Fatalf("decode updateInfo: %v", err)
	}
	document, _ := value.(map[string]any)
	info, _ := document["value"].(map[string]any)
	if got, _ := info["initialVolume"].(float64); math.Abs(got-(-12.5)) > 1e-9 {
		t.Fatalf("updateInfo initialVolume = %v, want -12.5", got)
	}
}

// A sender SET records the applied level; the watcher must not echo it back as
// a dvlc, while a later receiver-side change mirrors as one authoritative value.
func TestSenderSetDoesNotEchoDvlc(t *testing.T) {
	master := 0.05
	read := func() (float64, bool) { return master, true }
	set := func(_ context.Context, level float64) (float64, error) {
		master = level
		return level, nil
	}
	s := activeVolumeSession(t, read, set)
	writer := attachEventWriter(s)
	seedMaster(s)

	if response := textVolumeRequest(t, s, "-15"); response.Status != 200 {
		t.Fatalf("SET_PARAMETER status = %d", response.Status)
	}
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped")
	}
	if len(writer.buf.Bytes()) != 0 {
		t.Fatalf("sender SET was echoed as %v", postedUnitVolumes(t, writer.buf.Bytes()))
	}

	master = 0.3
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped on the local change")
	}
	volumes := postedUnitVolumes(t, writer.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.3) > 1e-9 {
		t.Fatalf("local change from 0.5 to 0.3 mirrored %v, want exactly [0.3]", volumes)
	}
}

// Reads on a candidate connection and on the owner's own control channel must
// not consume the owner's pending receiver-side mirror.
func TestVolumeReadsDoNotConsumeOwnerMirror(t *testing.T) {
	master := 0.05
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			return level, nil
		})
	writer := attachEventWriter(s)
	seedMaster(s)
	candidate := &Session{m: s.m, done: make(chan struct{}), peer: Peer{Address: "candidate"}}

	master = 0.3
	if got := infoInitialVolume(t, candidate); math.Abs(got-(-21)) > 1e-9 {
		t.Fatalf("candidate /info = %v, want -21", got)
	}
	if got := getParameterVolume(t, candidate); got != "\r\nvolume: -21.000000\r\n" {
		t.Fatalf("candidate GET_PARAMETER = %q", got)
	}
	if got := getParameterVolume(t, s); got != "\r\nvolume: -21.000000\r\n" {
		t.Fatalf("owner GET_PARAMETER = %q", got)
	}
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped")
	}
	volumes := postedUnitVolumes(t, writer.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.3) > 1e-9 {
		t.Fatalf("owner mirror after reads = %v, want exactly [0.3]", volumes)
	}
}

// A takeover before the new sender opens its event channel must let updateInfo
// seed the delivered value; only later local changes are mirrored.
func TestTakeoverEventSeedsBeforeMirror(t *testing.T) {
	master := 0.05
	first := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			return level, nil
		})
	second := newSession(first.m, idleStream{})
	second.promoted.Store(true)
	second.mu.Lock()
	second.encrypted = true
	second.mu.Unlock()
	first.m.mu.Lock()
	first.m.active = second
	first.m.mu.Unlock()

	master = 0.3
	writer := &captureEventWriter{}
	second.eventMu.Lock()
	second.eventOut = writer
	second.eventMu.Unlock()
	if _, ok := second.publishEventChannel(writer, idleStream{}); !ok {
		t.Fatal("updateInfo was not published")
	}
	afterUpdate := len(writer.buf.Bytes())
	if !second.pollMasterVolume() {
		t.Fatal("watcher stopped")
	}
	if len(writer.buf.Bytes()) != afterUpdate {
		t.Fatalf("takeover echoed its updateInfo master: %v", postedDvlcVolumes(t, writer.buf.Bytes()))
	}

	master = 0.5
	if !second.pollMasterVolume() {
		t.Fatal("watcher stopped on the local change")
	}
	volumes := postedDvlcVolumes(t, writer.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.5) > 1e-9 {
		t.Fatalf("takeover mirror = %v, want exactly [0.5]", volumes)
	}
}

// A delayed SET echoing a dvlc value must not revert a newer local master; the
// echo token is one-shot, so the next divergent drag still applies. A fresh
// session with no outbound dvlc never suppresses its first drag.
func TestDelayedMirrorEchoDoesNotRevertNewerMaster(t *testing.T) {
	master := 0.05
	writes := 0
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			writes++
			return level, nil
		})
	writer := attachEventWriter(s)
	seedMaster(s)
	master = 0.3
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped")
	}
	if got := postedUnitVolumes(t, writer.buf.Bytes()); len(got) != 1 || math.Abs(got[0]-0.3) > 1e-9 {
		t.Fatalf("mirror = %v, want [0.3]", got)
	}

	// A newer local level is not yet mirrored when the sender echoes the old
	// dvlc. It must win, not be overwritten by the stale value.
	master = 0.5
	textVolumeRequest(t, s, "-21") // unit 0.3, the just-mirrored value
	if writes != 0 || master != 0.5 {
		t.Fatalf("stale echo applied %d write(s), master %v; want none and 0.5", writes, master)
	}
	// The token is consumed; a later legitimate drag applies.
	textVolumeRequest(t, s, "-18") // unit 0.4
	if writes != 1 || math.Abs(master-0.4) > 1e-9 {
		t.Fatalf("post-echo drag wrote %d, master %v; want one 0.4", writes, master)
	}

	fresh := activeVolumeSession(t, func() (float64, bool) { return 0.05, true },
		func(_ context.Context, level float64) (float64, error) {
			return level, nil
		})
	textVolumeRequest(t, fresh, "-15")
}

// A slow master read never holds the ownership mutex: close and takeover do not
// wait for it, and the read itself is bounded.
func TestSlowMasterReadDoesNotBlockClose(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	entered := make(chan struct{})
	var once sync.Once
	mgr.cfg.MasterVolumeContext = func(ctx context.Context) (float64, bool) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, false
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	conn, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := newRTSPClient(conn)
	requestDone := make(chan error, 1)
	go func() {
		_, err := client.do("GET", "/info", "", nil)
		requestDone <- err
	}()
	<-entered

	closeDone := make(chan error, 1)
	go func() { closeDone <- mgr.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close could not join a session parked in a master read")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("the parked RTSP handler did not return after Close")
	}
}

// A slow master read in the old owner's real RTSP handler also must not delay a
// takeover.
func TestSlowMasterReadDoesNotBlockTakeover(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	s1 := activeVolumeSession(t, func() (float64, bool) { return 0.05, true },
		func(_ context.Context, level float64) (float64, error) { return level, nil })
	s1.m.cfg.MasterVolumeContext = func(ctx context.Context) (float64, bool) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, false
	}
	s2 := newSession(s1.m, idleStream{})
	s1.m.mu.Lock()
	s1.m.reserved = s2
	s1.m.mu.Unlock()

	requestDone := make(chan error, 1)
	go func() {
		_, _, err := s1.handleInfo(&rtsp.Request{Method: "GET", URI: "/info", Proto: "RTSP/1.0"})
		requestDone <- err
	}()
	<-entered

	commitDone := make(chan bool, 1)
	go func() { commitDone <- s1.m.commit(s2) }()
	select {
	case ok := <-commitDone:
		if !ok {
			t.Fatal("takeover did not commit")
		}
	case <-time.After(time.Second):
		t.Fatal("takeover waited for the slow master read")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("the parked old-owner handler did not return after takeover")
	}
	s2.close("test over")
}

// Close signals the session cancellation before draining the write scope, so a
// daemon-queued setter wakes instead of blocking shutdown.
func TestCloseCancelsInFlightMasterWrite(t *testing.T) {
	entered := make(chan struct{})
	set := func(ctx context.Context, _ float64) (float64, error) {
		close(entered)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	s := activeVolumeSession(t, func() (float64, bool) { return 0.05, true }, set)
	writeDone := make(chan error, 1)
	go func() {
		_, err := s.m.setMasterVolume(s, 0.5)
		writeDone <- err
	}()
	<-entered

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.m.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close waited for the cancelled write")
	}
	select {
	case err := <-writeDone:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("in-flight write error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight write was not cancelled")
	}
}

// A SET can arrive on the independent control socket before the outbound dvlc
// Write returns. The token is staged before the bytes, so a divergent genuine
// SET B wins, a delayed echo of A is ignored, and A's late completion does not
// overwrite B's display state.
func TestInFlightDvlcEchoAndDivergentSet(t *testing.T) {
	master := 0.05
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			return level, nil
		})
	blocking := &blockingEventWriter{entered: make(chan struct{}), release: make(chan struct{})}
	attachWriter(s, blocking)
	seedMaster(s)

	master = 0.3
	pollDone := make(chan bool, 1)
	go func() { pollDone <- s.pollMasterVolume() }()
	<-blocking.entered

	// Genuine divergent SET B applies while A's Write is parked.
	textVolumeRequest(t, s, "-18") // unit 0.4
	close(blocking.release)
	if !<-pollDone {
		t.Fatal("watcher stopped during the in-flight write")
	}
	if master != 0.4 {
		t.Fatalf("master = %v, want the genuine B 0.4", master)
	}
	delivered, known := s.deliveredMaster()
	if !known || math.Abs(delivered-0.3) > 1e-9 {
		t.Fatalf("delivered display = %v/%v, want the wire value A 0.3", delivered, known)
	}
	volumes := postedDvlcVolumes(t, blocking.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.3) > 1e-9 {
		t.Fatalf("outbound dvlc = %v, want the staged [0.3]", volumes)
	}
	// A is still armed, so the repair attempt defers.
	if !s.pollMasterVolume() {
		t.Fatal("deferred repair poll stopped")
	}
	if got := postedDvlcVolumes(t, blocking.buf.Bytes()); len(got) != 1 {
		t.Fatalf("repair overwrote an armed token: %v", got)
	}
	// The delayed echo of A consumes the token without undoing B.
	textVolumeRequest(t, s, "-21") // unit 0.3
	if master != 0.4 {
		t.Fatalf("delayed echo A reverted master to %v", master)
	}
	// The next regular pass repairs presentation to authoritative B.
	if !s.pollMasterVolume() {
		t.Fatal("repair poll stopped")
	}
	if master != 0.4 {
		t.Fatalf("presentation repair mutated master to %v", master)
	}
	delivered, known = s.deliveredMaster()
	if !known || math.Abs(delivered-0.4) > 1e-9 {
		t.Fatalf("repaired display = %v/%v, want 0.4", delivered, known)
	}
	volumes = postedDvlcVolumes(t, blocking.buf.Bytes())
	if len(volumes) != 2 || math.Abs(volumes[0]-0.3) > 1e-9 || math.Abs(volumes[1]-0.4) > 1e-9 {
		t.Fatalf("wire sequence = %v, want [0.3, 0.4]", volumes)
	}
}

// Without an echo, the bounded expiry lets the next regular pass repair a
// superseded presentation; before expiry the armed token defers it.
func TestMirrorExpiryAllowsRepairWithoutEcho(t *testing.T) {
	master := 0.05
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			return level, nil
		})
	writer := attachEventWriter(s)
	seedMaster(s)

	master = 0.3
	if !s.pollMasterVolume() {
		t.Fatal("first poll stopped")
	}
	master = 0.5
	if !s.pollMasterVolume() {
		t.Fatal("deferred poll stopped")
	}
	if got := postedDvlcVolumes(t, writer.buf.Bytes()); len(got) != 1 || math.Abs(got[0]-0.3) > 1e-9 {
		t.Fatalf("armed token was overwritten: %v", got)
	}

	s.displayMu.Lock()
	s.mirrorSentAt = time.Now().Add(-masterVolumeEchoWindow - time.Millisecond)
	s.displayMu.Unlock()
	if !s.pollMasterVolume() {
		t.Fatal("repair poll stopped")
	}
	if master != 0.5 {
		t.Fatalf("repair mutated master to %v", master)
	}
	delivered, known := s.deliveredMaster()
	if !known || math.Abs(delivered-0.5) > 1e-9 {
		t.Fatalf("repaired display = %v/%v, want 0.5", delivered, known)
	}
	volumes := postedDvlcVolumes(t, writer.buf.Bytes())
	if len(volumes) != 2 || math.Abs(volumes[0]-0.3) > 1e-9 || math.Abs(volumes[1]-0.5) > 1e-9 {
		t.Fatalf("wire sequence = %v, want [0.3, 0.5]", volumes)
	}
}

// A replacement channel can publish while an old dvlc Write is finishing. The
// old completion must not record delivery for the new channel; its updateInfo
// owns the seed/token.
func TestReplacementWhileDvlcWriteFinishes(t *testing.T) {
	master := 0.05
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			return level, nil
		})
	old := &blockingEventWriter{entered: make(chan struct{}), release: make(chan struct{})}
	attachWriter(s, old)
	seedMaster(s)

	master = 0.3
	pollDone := make(chan bool, 1)
	go func() { pollDone <- s.pollMasterVolume() }()
	<-old.entered

	replacement := &captureEventWriter{}
	if _, ok := s.replaceEventChannel(nil, nil, replacement, nil); !ok {
		t.Fatal("replacement was not installed")
	}
	publishDone := make(chan bool, 1)
	go func() {
		_, ok := s.publishEventChannel(replacement, idleStream{})
		publishDone <- ok
	}()
	close(old.release)
	if !<-pollDone {
		t.Fatal("old watcher stopped")
	}
	if !<-publishDone {
		t.Fatal("replacement updateInfo did not publish")
	}
	if volumes := postedDvlcVolumes(t, replacement.buf.Bytes()); len(volumes) != 0 {
		t.Fatalf("replacement received an old dvlc: %v", volumes)
	}

	master = 0.5
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped on the new channel")
	}
	volumes := postedDvlcVolumes(t, replacement.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.5) > 1e-9 {
		t.Fatalf("replacement mirror = %v, want exactly [0.5]", volumes)
	}
}

// A read parked on an old master sample returns the newer accepted sender
// write after it completes, and the old sample is never mirrored.
func TestParkedReadDoesNotReturnOrMirrorOldSample(t *testing.T) {
	current := 0.05
	release := make(chan struct{})
	started := make(chan struct{})
	var first sync.Once
	s := activeVolumeSession(t, func() (float64, bool) { return current, true },
		func(_ context.Context, level float64) (float64, error) {
			current = level
			return level, nil
		})
	s.m.cfg.MasterVolumeContext = func(ctx context.Context) (float64, bool) {
		parked := false
		first.Do(func() {
			parked = true
			close(started)
		})
		if !parked {
			return current, true
		}
		select {
		case <-ctx.Done():
			return 0, false
		case <-release:
			return 0.05, true
		}
	}

	type readResult struct {
		unit float64
		ok   bool
	}
	readDone := make(chan readResult, 1)
	go func() {
		unit, ok := s.m.readMasterVolume(s.masterVolumeContext())
		readDone <- readResult{unit, ok}
	}()
	<-started
	if response := textVolumeRequest(t, s, "-18"); response.Status != 200 { // genuine 0.4
		t.Fatalf("sender write status = %d", response.Status)
	}
	close(release)
	got := <-readDone
	if !got.ok || math.Abs(got.unit-0.4) > 1e-9 {
		t.Fatalf("parked read returned %v/%v, want the newer 0.4", got.unit, got.ok)
	}
	writer := attachEventWriter(s)
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped")
	}
	if volumes := postedDvlcVolumes(t, writer.buf.Bytes()); len(volumes) != 0 {
		t.Fatalf("parked old sample was mirrored: %v", volumes)
	}
}

// An exact in-flight echo consumes the echo token but must not invalidate the
// delivery reservation: after Write succeeds the value is recorded once and is
// not mirrored again.
func TestEchoOnlyDuringWriteStillRecordsDelivery(t *testing.T) {
	master := 0.05
	writes := 0
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			writes++
			return level, nil
		})
	blocking := &blockingEventWriter{entered: make(chan struct{}), release: make(chan struct{})}
	attachWriter(s, blocking)
	seedMaster(s)

	master = 0.3
	pollDone := make(chan bool, 1)
	go func() { pollDone <- s.pollMasterVolume() }()
	<-blocking.entered
	if response := textVolumeRequest(t, s, "-21"); response.Status != 200 { // exact echo of 0.3
		t.Fatalf("echo SET status = %d", response.Status)
	}
	if writes != 0 {
		t.Fatalf("echo SET applied %d write(s), want none", writes)
	}
	close(blocking.release)
	if !<-pollDone {
		t.Fatal("watcher stopped")
	}
	delivered, known := s.deliveredMaster()
	if !known || math.Abs(delivered-0.3) > 1e-9 {
		t.Fatalf("delivered display = %v/%v, want 0.3 after the successful write", delivered, known)
	}
	before := len(blocking.buf.Bytes())
	if !s.pollMasterVolume() {
		t.Fatal("follow-up poll stopped")
	}
	if len(blocking.buf.Bytes()) != before {
		t.Fatalf("delivery was mirrored again: %v", postedDvlcVolumes(t, blocking.buf.Bytes()))
	}
}

// A poll queued behind a blocked dvlc write and a genuine SET must not send the
// stale value or erase the older valid echo token.
func TestStaleReservationCannotEraseArmedEcho(t *testing.T) {
	master := 0.05
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			return level, nil
		})
	writer := attachEventWriter(s)
	seedMaster(s)
	out, generation, ready := s.readyEventWriter()
	if !ready {
		t.Fatal("event channel is not ready")
	}

	master = 0.3
	if !s.pollMasterVolume() {
		t.Fatal("first poll stopped")
	}
	staleEpoch := s.senderDeliveryEpoch()
	s.displayMu.Lock()
	priorToken, priorUnit, priorArmed := s.mirrorTokenID, s.mirrorUnit, s.mirrorArmed
	s.displayMu.Unlock()

	// Genuine B advances the accepted-write epoch.
	textVolumeRequest(t, s, "-18") // unit 0.4

	// A stale C reservation is attempted directly under the event-writer order
	// with an unequal value; it must fail and leave A's token intact.
	s.eventWriteMu.Lock()
	_, _, ok := s.stageMirrorIfEpoch(staleEpoch, 0.2, generation, out)
	s.eventWriteMu.Unlock()
	if ok {
		t.Fatal("stale reservation was accepted")
	}
	s.displayMu.Lock()
	gotToken, gotUnit, gotArmed := s.mirrorTokenID, s.mirrorUnit, s.mirrorArmed
	s.displayMu.Unlock()
	if gotToken != priorToken || gotUnit != priorUnit || gotArmed != priorArmed {
		t.Fatalf("stale reservation mutated token: token %d->%d unit %v->%v armed %v",
			priorToken, gotToken, priorUnit, gotUnit, gotArmed)
	}

	// The armed A echo is still correlated and cannot undo B.
	textVolumeRequest(t, s, "-21") // unit 0.3
	if master != 0.4 {
		t.Fatalf("echo A reverted master to %v, want 0.4", master)
	}
	volumes := postedDvlcVolumes(t, writer.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.3) > 1e-9 {
		t.Fatalf("outbound dvlc = %v, want only the first staged [0.3]", volumes)
	}
}

// A poll started while a genuine SET is parked must not post the old master
// after the SET applies.
func TestPollDuringParkedSetterDoesNotPostOldMaster(t *testing.T) {
	master := 0.05
	entered := make(chan struct{})
	release := make(chan struct{})
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			close(entered)
			<-release
			master = level
			return level, nil
		})
	writer := attachEventWriter(s)
	seedMaster(s)

	master = 0.3
	setDone := make(chan *rtsp.Response, 1)
	go func() { setDone <- textVolumeRequest(t, s, "-18") }() // genuine B 0.4
	<-entered
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped")
	}
	if volumes := postedDvlcVolumes(t, writer.buf.Bytes()); len(volumes) != 0 {
		t.Fatalf("poll during parked SET posted %v, want none", volumes)
	}
	close(release)
	if response := <-setDone; response.Status != 200 {
		t.Fatalf("parked SET status = %d", response.Status)
	}
	if master != 0.4 {
		t.Fatalf("master = %v, want 0.4", master)
	}
	if volumes := postedDvlcVolumes(t, writer.buf.Bytes()); len(volumes) != 0 {
		t.Fatalf("old master was posted after SET completion: %v", volumes)
	}
	master = 0.5
	if !s.pollMasterVolume() {
		t.Fatal("watcher stopped on later local change")
	}
	volumes := postedDvlcVolumes(t, writer.buf.Bytes())
	if len(volumes) != 1 || math.Abs(volumes[0]-0.5) > 1e-9 {
		t.Fatalf("later local change mirror = %v, want [0.5]", volumes)
	}
}

// A non-owner never reaches the setter, and the request is acknowledged with
// 200 like the reference's failed principal check. Pause keeps ownership.
func TestSenderVolumeRequiresPromotedHAPOwner(t *testing.T) {
	type ownerState struct {
		name      string
		promoted  bool
		encrypted bool
		owned     bool
	}
	for _, state := range []ownerState{
		{name: "candidate", promoted: false, encrypted: true, owned: false},
		{name: "event-only", promoted: false, encrypted: true, owned: true},
		{name: "legacy plaintext", promoted: true, encrypted: false, owned: true},
		{name: "retired owner", promoted: true, encrypted: true, owned: false},
		{name: "paused owner", promoted: true, encrypted: true, owned: true},
	} {
		t.Run(state.name, func(t *testing.T) {
			master := 0.05
			writes := 0
			s := activeVolumeSession(t, func() (float64, bool) { return master, true },
				func(_ context.Context, level float64) (float64, error) {
					master = level
					writes++
					return level, nil
				})
			s.promoted.Store(state.promoted)
			s.mu.Lock()
			s.encrypted = state.encrypted
			s.mu.Unlock()
			if state.name == "paused owner" {
				s.rate = 0
			}
			if !state.owned {
				s.m.mu.Lock()
				s.m.active = nil
				s.m.mu.Unlock()
			}
			response := textVolumeRequest(t, s, "-15")
			if response.Status != 200 {
				t.Fatalf("status = %d, want the reference's 200", response.Status)
			}
			wantWrites := 0
			if state.name == "paused owner" {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("setter calls = %d, want %d", writes, wantWrites)
			}
			if wantWrites == 0 && master != 0.05 {
				t.Fatalf("master changed to %v, want 0.05", master)
			}
		})
	}
}

// A takeover is serialized with a master write: the commit waits until the
// in-flight framework call returns, and every request from the old session
// afterwards fails the ownership check instead of changing the new state.
func TestMasterWriteDoesNotOutliveTakeover(t *testing.T) {
	master := 0.05
	entered := make(chan struct{})
	release := make(chan struct{})
	set := func(_ context.Context, level float64) (float64, error) {
		close(entered)
		<-release
		return level, nil
	}
	s1 := activeVolumeSession(t, func() (float64, bool) { return master, true }, set)
	m := s1.m
	s2 := newSession(m, idleStream{})
	m.mu.Lock()
	m.reserved = s2
	m.mu.Unlock()

	writeDone := make(chan error, 1)
	go func() {
		_, err := m.setMasterVolume(s1, 0.5)
		writeDone <- err
	}()
	<-entered

	commitDone := make(chan bool, 1)
	go func() { commitDone <- m.commit(s2) }()
	select {
	case <-commitDone:
		t.Fatal("the takeover committed while a master write was still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatalf("in-flight master write: %v", err)
	}
	if !<-commitDone {
		t.Fatal("the takeover did not commit after the write finished")
	}
	if m.Active() != s2 {
		t.Fatal("the new sender is not the active session")
	}
	if _, err := m.setMasterVolume(s1, 0.6); !errors.Is(err, errMasterVolumeNotOwner) {
		t.Fatalf("retired sender master write error = %v, want errMasterVolumeNotOwner", err)
	}
	s2.close("test over")
}

// Non-finite mixer data never becomes a dvlc body and never changes the
// recorded master.
func TestMasterVolumeNotificationRejectsNonFinite(t *testing.T) {
	for _, level := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		s := volumeSession(func() (float64, bool) { return level, true })
		writer := attachEventWriter(s)
		if !s.pollMasterVolume() {
			t.Fatalf("watcher stopped on master %v", level)
		}
		if len(writer.buf.Bytes()) != 0 {
			t.Fatalf("master %v produced a notification", level)
		}
		if _, known := s.m.masterSnapshot(); known {
			t.Fatalf("master %v changed the recorded level", level)
		}
	}
}

// Out-of-contract values change nothing; mute, the near-zero bottom and the
// near-full top remain distinct legitimate settings.
func TestSenderVolumeValidationAndEndpoints(t *testing.T) {
	master := 0.25
	var writes []float64
	s := activeVolumeSession(t, func() (float64, bool) { return master, true },
		func(_ context.Context, level float64) (float64, error) {
			master = level
			writes = append(writes, level)
			return level, nil
		})
	for _, value := range []string{"abc", "NaN", "+Inf", "3", "100"} {
		if response := textVolumeRequest(t, s, value); response.Status != 200 {
			t.Fatalf("value %q status = %d", value, response.Status)
		}
	}
	if len(writes) != 0 || master != 0.25 {
		t.Fatalf("invalid values wrote %v (master %v)", writes, master)
	}

	textVolumeRequest(t, s, "-144")
	if len(writes) != 1 || writes[0] != 0 {
		t.Fatalf("mute wrote %v, want 0", writes)
	}
	textVolumeRequest(t, s, "-30")
	if len(writes) != 2 || writes[1] != 0 {
		t.Fatalf("-30 dB wrote %v, want 0", writes)
	}
	textVolumeRequest(t, s, "-0.0001")
	if len(writes) != 3 {
		t.Fatalf("near-zero dB did not write: %v", writes)
	}
	if writes[2] < 0.999 || writes[2] > 1 {
		t.Fatalf("near-zero dB wrote %v, want near full scale, not mute", writes[2])
	}
	textVolumeRequest(t, s, "0")
	if len(writes) != 4 || writes[3] != 1 {
		t.Fatalf("0 dB wrote %v, want 1", writes)
	}
	if got := getParameterVolume(t, s); got != "\r\nvolume: 0.000000\r\n" {
		t.Fatalf("GET_PARAMETER at full = %q", got)
	}
}

// With no readable master and no recorded level, /info and GET_PARAMETER report
// the safe -144 mute value; they never report 0 dB/full scale and never write
// the master. A previously read level is preferred.
func TestUnreadableMasterReportsMuteWithoutWriting(t *testing.T) {
	writes := 0
	s := updateInfoSession(t)
	s.m.cfg.MasterVolumeContext = func(context.Context) (float64, bool) { return 0, false }
	s.m.cfg.SetMasterVolume = func(_ context.Context, level float64) (float64, error) {
		writes++
		return level, nil
	}
	if got := infoInitialVolume(t, s); got != -144 {
		t.Fatalf("/info initialVolume = %v, want -144 mute", got)
	}
	if got := getParameterVolume(t, s); got != "\r\nvolume: -144.000000\r\n" {
		t.Fatalf("GET_PARAMETER body = %q, want -144 mute", got)
	}
	if writes != 0 {
		t.Fatalf("reporting fallback wrote the master %d times", writes)
	}

	s.m.storeMasterWrite(0.3)
	if got := infoInitialVolume(t, s); math.Abs(got-(-21)) > 1e-9 {
		t.Fatalf("/info with a recorded 0.3 master = %v, want -21", got)
	}
}

func TestUnitVolumeFromDB(t *testing.T) {
	for _, tc := range []struct {
		dB    float64
		want  float64
		valid bool
	}{
		{0, 1, true},
		{-15, 0.5, true},
		{-29.999, 0.0000333333333333, true},
		{-30, 0, true},
		{-144, 0, true},
		{-200, 0, true},
		{0.0001, 0, false},
		{3, 0, false},
		{math.NaN(), 0, false},
		{math.Inf(1), 0, false},
		{math.Inf(-1), 0, false},
	} {
		got, valid := unitVolumeFromDB(tc.dB)
		if valid != tc.valid || (tc.valid && math.Abs(got-tc.want) > 1e-9) || (!tc.valid && got != 0) {
			t.Errorf("unitVolumeFromDB(%v) = (%v, %v), want (%v, %v)", tc.dB, got, valid, tc.want, tc.valid)
		}
	}
}

func TestVolumeDBFromUnit(t *testing.T) {
	for _, tc := range []struct {
		unit  float64
		want  float64
		valid bool
	}{
		{0, -144, true},
		{0.05, -28.5, true},
		{0.5, -15, true},
		{1, 0, true},
		{math.NaN(), 0, false},
		{1.1, 0, false},
	} {
		got, valid := volumeDBFromUnit(tc.unit)
		if valid != tc.valid || (tc.valid && math.Abs(got-tc.want) > 1e-9) {
			t.Errorf("volumeDBFromUnit(%v) = (%v, %v), want (%v, %v)", tc.unit, got, valid, tc.want, tc.valid)
		}
	}
}
