package session

// Session ownership regression: a TCP accept is not a sender. Dual-stack
// senders race IPv4 and IPv6 connections and abandon the loser immediately,
// and reachability probes connect and close without a request. None of those
// may take the live session, the player or the advertisements away from a
// working sender. A completed HAP exchange protects and keys its connection
// but is still not a session; promotion happens only after an accepted
// ANNOUNCE or SETUP, as in the reference receiver.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/pair"
	"capsulad/apps/airplay/internal/plist"
)

// legacyAnnounce is a valid unencrypted RAOP announcement. It parses as PCM
// (L16) and needs no RSA key, so it promotes a session without audio setup.
func legacyAnnounce() []byte {
	return []byte(strings.Join([]string{
		"v=0",
		"o=iTunes 1 0 IN IP4 192.168.1.99",
		"s=iTunes",
		"c=IN IP4 192.168.1.99",
		"t=0 0",
		"m=audio 0 RTP/AVP 96",
		"a=rtpmap:96 L16/44100/2",
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100",
		"",
	}, "\r\n"))
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitAdvertGID(t *testing.T, advertiser *fakeAdvertiser, gid string) {
	t.Helper()
	waitUntil(t, "the advertised "+gid+" group id", func() bool {
		advert, ok := advertiser.find("_airplay._tcp")
		return ok && advert.TXT["gid"] == gid
	})
}

func dialSender(t *testing.T, mgr *Manager, transport *fakeTransport) (Stream, *rtspClient) {
	t.Helper()
	stream, err := transport.dial(mgr.advertisedPort())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return stream, newRTSPClient(stream)
}

// announce sends a valid session-starting request. Promotion is asynchronous
// with the response, so callers wait for the active session they expect.
func announce(t *testing.T, client *rtspClient) {
	t.Helper()
	response, err := client.do("ANNOUNCE", "/1", "", legacyAnnounce())
	if err != nil {
		t.Fatalf("ANNOUNCE: %v", err)
	}
	if response.status != 200 {
		t.Fatalf("ANNOUNCE = %d", response.status)
	}
}

// doHeaders writes one request with extra RTSP headers, which the shared test
// client does not expose.
func (c *rtspClient) doHeaders(method, uri string, body []byte, extra ...string) (*clientResponse, error) {
	c.cseq++
	header := fmt.Sprintf("%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: AirPlay/600.1 (Test iPhone; iOS 18)\r\n", method, uri, c.cseq)
	for _, line := range extra {
		header += line + "\r\n"
	}
	header += fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := c.conn.Write(append([]byte(header), body...)); err != nil {
		return nil, err
	}
	return c.readResponse()
}

func pendingCount(mgr *Manager) int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return len(mgr.pending)
}

// TestZeroPayloadAndProbesDoNotTakeOverLiveSession pins the field failure: the
// iPhone's real session is the IPv6 connection, while its IPv4 dual-stack
// loser connects, sends nothing and closes. The live sender must survive both
// the silent connection and OPTIONS//info probes.
func TestZeroPayloadAndProbesDoNotTakeOverLiveSession(t *testing.T) {
	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	live, liveClient := dialSender(t, mgr, transport)
	defer live.Close()
	announce(t, liveClient)
	waitUntil(t, "the announced session to become active", func() bool { return mgr.Active() != nil })
	liveSession := mgr.Active()
	if liveSession == nil {
		t.Fatal("no active session after ANNOUNCE")
	}
	waitUntil(t, "the active advertisement", func() bool { return advertiser.count("_airplay._tcp") == 2 })
	published := advertiser.count("_airplay._tcp")

	// The dual-stack loser: accepted, no bytes, closed immediately.
	loser, _ := dialSender(t, mgr, transport)
	time.Sleep(50 * time.Millisecond)
	if got := mgr.Active(); got != liveSession {
		t.Fatal("a silent TCP connection took over the live session")
	}
	loser.Close()
	waitUntil(t, "the silent candidate to leave the pending set", func() bool { return pendingCount(mgr) == 0 })
	if got := mgr.Active(); got != liveSession {
		t.Fatal("closing the silent candidate ended the live session")
	}

	// A reachability probe that sends OPTIONS and GET /info is still not a
	// sender: it may be answered, but it must not evict the live session.
	probe, probeClient := dialSender(t, mgr, transport)
	response, err := probeClient.do("OPTIONS", "/1", "", nil)
	if err != nil || response.status != 200 {
		t.Fatalf("OPTIONS = %v %v", response, err)
	}
	response, err = probeClient.do("GET", "/info", "", nil)
	if err != nil || response.status != 200 {
		t.Fatalf("GET /info = %v %v", response, err)
	}
	// A refused SETUP is not a session start either.
	response, err = probeClient.do("SETUP", "/1", "application/x-apple-binary-plist", []byte("not a plist"))
	if err != nil || response.status == 200 {
		t.Fatalf("malformed SETUP = %v %v, want a refusal", response, err)
	}
	if got := mgr.Active(); got != liveSession {
		t.Fatal("a probe or refused SETUP took over the live session")
	}
	if got := advertiser.count("_airplay._tcp"); got != published {
		t.Fatalf("probe republished the advertisement: %d records, want %d", got, published)
	}
	if !mgr.board.Live() {
		t.Fatal("the player was released by a probe")
	}

	// The same connection becomes a sender only when it announces.
	announce(t, probeClient)
	waitUntil(t, "the probe connection to replace the live session", func() bool { return mgr.Active() != liveSession })
	if mgr.Active() == nil {
		t.Fatal("ANNOUNCE did not promote the connection")
	}
	if got := advertiser.count("_airplay._tcp"); got != published {
		t.Fatalf("takeover republished an already-active advertisement: %d, want %d", got, published)
	}
	probe.Close()
}

// TestTakeoverOnSessionRequestReleasesOldResources proves the genuine path:
// once a candidate sends a valid session-starting request, the old sender is
// closed and its audio stream is aborted, while only the new sender owns the
// player.
func TestTakeoverOnSessionRequestReleasesOldResources(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	oldStream, oldClient := dialSender(t, mgr, transport)
	announce(t, oldClient)
	waitUntil(t, "the old session to become active", func() bool { return mgr.Active() != nil })
	old := mgr.Active()
	if old == nil {
		t.Fatal("no active session after ANNOUNCE")
	}
	setup, err := oldClient.doHeaders("SETUP", "/1", nil,
		"Transport: RTP/AVP/TCP;unicast;interleaved=0-1")
	if err != nil || setup.status != 200 {
		t.Fatalf("SETUP = %v %v", setup, err)
	}
	waitUntil(t, "the old session's audio stream", func() bool { return len(old.activeStreams()) == 1 })

	newStream, newClient := dialSender(t, mgr, transport)
	defer newStream.Close()
	announce(t, newClient)
	waitUntil(t, "the new sender to take the session over", func() bool { return mgr.Active() != nil && mgr.Active() != old })
	next := mgr.Active()
	if next == nil || next == old {
		t.Fatal("ANNOUNCE did not take the session over")
	}
	waitUntil(t, "the superseded session to close", func() bool {
		select {
		case <-old.done:
			return true
		default:
			return false
		}
	})
	waitUntil(t, "the old audio stream to be released", func() bool {
		streams := old.activeStreams()
		return len(streams) == 1 && streams[0].isClosed()
	})
	if streams := old.activeStreams(); len(streams) == 0 || !streams[0].isClosed() {
		t.Fatal("the superseded session's audio stream was not released")
	}
	waitUntil(t, "the player to name the new generation", func() bool {
		return mgr.board.CurrentSession() == next.boardGen
	})
	oldStream.Close()
}

// TestCandidateTimeoutIsSilent bounds a connection that never sends a
// session-initiating request. It closes without ending the player, republishing
// or leaving a pending session behind.
func TestCandidateTimeoutIsSilent(t *testing.T) {
	oldTimeout := candidateRequestTimeout
	candidateRequestTimeout = 40 * time.Millisecond
	defer func() { candidateRequestTimeout = oldTimeout }()

	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	published := advertiser.count("_airplay._tcp")

	stream, _ := dialSender(t, mgr, transport)
	// The fake dial queues the connection and returns before the accept loop
	// registers it; wait for registration before treating an empty pending set
	// as expiry.
	waitUntil(t, "the candidate to register", func() bool { return pendingCount(mgr) == 1 })
	waitUntil(t, "the candidate to expire", func() bool { return pendingCount(mgr) == 0 })
	if mgr.Active() != nil {
		t.Fatal("an expired candidate became the live session")
	}
	if mgr.board.Live() {
		t.Fatal("an expired candidate started the player")
	}
	if got := advertiser.count("_airplay._tcp"); got != published {
		t.Fatalf("an expired candidate republished the advertisement: %d, want %d", got, published)
	}
	// The receiver closed its end; the sender observes EOF rather than a live
	// session. Bound the read so a timeout regression fails the test instead
	// of hanging it.
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := stream.Read(buf)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("the expired candidate connection stayed open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the expired candidate connection was not closed")
	}
	stream.Close()
}

// TestPendingCandidatesAreBounded keeps silent connections from accumulating.
func TestPendingCandidatesAreBounded(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	streams := make([]Stream, 0, maxPendingSessions+2)
	for range maxPendingSessions + 2 {
		stream, _ := dialSender(t, mgr, transport)
		streams = append(streams, stream)
	}
	waitUntil(t, "pending candidates to reach the cap", func() bool {
		return pendingCount(mgr) == maxPendingSessions
	})
	if mgr.Active() != nil {
		t.Fatal("a silent candidate became the live session")
	}
	for _, stream := range streams {
		stream.Close()
	}
}

// TestFinishedCandidateCannotPromote covers the narrow race where a candidate
// ends before promotion starts the player. It must be refused without starting
// the player or republishing, so a finished connection cannot own the sender.
func TestFinishedCandidateCannotPromote(t *testing.T) {
	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	published := advertiser.count("_airplay._tcp")

	stream, _ := dialSender(t, mgr, transport)
	waitUntil(t, "the candidate to register", func() bool { return pendingCount(mgr) == 1 })

	mgr.mu.Lock()
	candidate := mgr.pending[0]
	mgr.mu.Unlock()

	// Simulate close winning the lifecycle race without ending the connection,
	// so the manager still sees the candidate in its pending set.
	candidate.lifecycleMu.Lock()
	candidate.ended = true
	candidate.lifecycleMu.Unlock()

	if mgr.reserve(candidate) {
		t.Fatal("reservation succeeded after the connection ended")
	}
	if mgr.Active() != nil {
		t.Fatal("a finished candidate became the live session")
	}
	if mgr.board.Live() {
		t.Fatal("a finished candidate started the player")
	}
	if got := advertiser.count("_airplay._tcp"); got != published {
		t.Fatalf("refused reservation republished: %d, want %d", got, published)
	}
	if got := pendingCount(mgr); got != 0 {
		t.Fatalf("%d pending sessions after the refused promotion, want 0", got)
	}
	candidate.close("test cleanup")
	stream.Close()
}

// --- SETUP transaction, group id and reservation regressions ---------------

func setupBody(t *testing.T, gid string, streams ...map[string]any) []byte {
	t.Helper()
	dict := map[string]any{}
	if gid != "" {
		dict["gid"] = gid
	}
	list := make([]any, 0, len(streams))
	for _, stream := range streams {
		list = append(list, stream)
	}
	dict["streams"] = list
	body, err := plist.EncodeBinary(dict)
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	return body
}

func pcmStream() map[string]any {
	return map[string]any{"type": int64(streamTypeRealtime), "audioFormat": int64(1 << 11), "spf": int64(352)}
}

func managerGroupID(mgr *Manager) string {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.groupID
}

func candidateForTest(t *testing.T, mgr *Manager) *Session {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.pending) != 1 {
		t.Fatalf("%d pending candidates, want 1", len(mgr.pending))
	}
	return mgr.pending[0]
}

func sessionDone(s *Session) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// TestRefusedPartialSetupLeavesOwnerAndGIDUntouched covers a multi-stream
// SETUP that fails after one stream was staged: the live sender, its group id,
// its advertisement and its audio must be unchanged, and the candidate must
// not retain a reader or a staged stream.
func TestRefusedPartialSetupLeavesOwnerAndGIDUntouched(t *testing.T) {
	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	ownerStream, ownerClient := dialSender(t, mgr, transport)
	defer ownerStream.Close()
	announce(t, ownerClient)
	waitUntil(t, "the owner session", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()
	// Let the activation publish finish before the SETUP adds a gid publish;
	// otherwise a delayed activation could land after the baseline count.
	waitUntil(t, "the active advertisement", func() bool { return advertiser.count("_airplay._tcp") == 2 })

	setup, err := ownerClient.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody(t, "owner-gid", pcmStream()))
	if err != nil || setup.status != 200 {
		t.Fatalf("owner SETUP = %v %v", setup, err)
	}
	waitUntil(t, "the owner audio stream", func() bool { return len(owner.activeStreams()) == 1 })
	waitUntil(t, "the owner group id", func() bool { return managerGroupID(mgr) == "owner-gid" })
	waitAdvertGID(t, advertiser, "owner-gid")
	published := advertiser.count("_airplay._tcp")

	candidateStream, candidateClient := dialSender(t, mgr, transport)
	defer candidateStream.Close()
	waitUntil(t, "the candidate to register", func() bool { return pendingCount(mgr) == 1 })
	candidate := candidateForTest(t, mgr)

	// The first stream is valid and gets staged; the second is not decodable.
	bad := map[string]any{"type": int64(999), "audioFormat": int64(1 << 11)}
	response, err := candidateClient.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody(t, "candidate-gid", pcmStream(), bad))
	if err != nil || response.status == 200 {
		t.Fatalf("partial SETUP = %v %v, want a refusal", response, err)
	}
	if mgr.Active() != owner {
		t.Fatal("a refused partial SETUP took the live session")
	}
	if got := managerGroupID(mgr); got != "owner-gid" {
		t.Fatalf("refused SETUP changed gid to %q", got)
	}
	if got := advertiser.count("_airplay._tcp"); got != published {
		t.Fatalf("refused SETUP republished: %d records, want %d", got, published)
	}
	if got := len(owner.activeStreams()); got != 1 {
		t.Fatalf("owner has %d streams after a refused SETUP, want 1", got)
	}
	if staged := candidate.stagedStreams(); len(staged) != 0 {
		t.Fatalf("refused SETUP left %d staged streams", len(staged))
	}
	if active := candidate.activeStreams(); len(active) != 0 {
		t.Fatalf("refused SETUP left %d active streams", len(active))
	}
}

// TestCandidateSetupCommitsGroupOnlyOnTakeover proves a candidate's gid is
// staged until the session owns the receiver, and a superseded owner cannot
// apply a later gid update.
func TestCandidateSetupCommitsGroupOnlyOnTakeover(t *testing.T) {
	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	ownerStream, ownerClient := dialSender(t, mgr, transport)
	defer ownerStream.Close()
	announce(t, ownerClient)
	waitUntil(t, "the owner session", func() bool { return mgr.Active() != nil })
	old := mgr.Active()

	candidateStream, candidateClient := dialSender(t, mgr, transport)
	defer candidateStream.Close()
	response, err := candidateClient.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody(t, "new-gid", pcmStream()))
	if err != nil || response.status != 200 {
		t.Fatalf("candidate SETUP = %v %v", response, err)
	}
	waitUntil(t, "the candidate takeover", func() bool { return mgr.Active() != nil && mgr.Active() != old })
	next := mgr.Active()
	waitUntil(t, "the candidate stream", func() bool { return len(next.activeStreams()) == 1 })
	waitUntil(t, "the candidate group id", func() bool { return managerGroupID(mgr) == "new-gid" })
	waitAdvertGID(t, advertiser, "new-gid")
	waitUntil(t, "the old session to close", func() bool { return sessionDone(old) })

	// A late gid from the superseded owner must not overwrite the live one.
	mgr.setGroupID(old, "stale-gid")
	if got := managerGroupID(mgr); got != "new-gid" {
		t.Fatalf("superseded owner applied gid %q", got)
	}
}

// TestReservedCandidateSurvivesEvictionDuringResponse covers a success response
// blocked in write while the accept loop fills the pending cap: the reserved
// candidate must not be evicted, and its later commit must succeed.
func TestReservedCandidateSurvivesEvictionDuringResponse(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	ownerStream, ownerClient := dialSender(t, mgr, transport)
	defer ownerStream.Close()
	announce(t, ownerClient)
	waitUntil(t, "the owner session", func() bool { return mgr.Active() != nil })

	// Write ANNOUNCE without reading the reply, so the server blocks in
	// writeResponse after reserving ownership.
	candidateStream, candidateClient := dialSender(t, mgr, transport)
	defer candidateStream.Close()
	body := legacyAnnounce()
	request := fmt.Sprintf("ANNOUNCE /1 RTSP/1.0\r\nCSeq: 1\r\nUser-Agent: AirPlay/600.1\r\nContent-Length: %d\r\n\r\n", len(body))
	if _, err := candidateClient.conn.Write(append([]byte(request), body...)); err != nil {
		t.Fatalf("ANNOUNCE write: %v", err)
	}
	waitUntil(t, "the candidate to reserve ownership", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return mgr.reserved != nil
	})
	mgr.mu.Lock()
	reserved := mgr.reserved
	mgr.mu.Unlock()

	var fillers []Stream
	for range maxPendingSessions + 1 {
		stream, _ := dialSender(t, mgr, transport)
		fillers = append(fillers, stream)
	}
	waitUntil(t, "silent candidates to fill the cap", func() bool { return pendingCount(mgr) == maxPendingSessions })
	if sessionDone(reserved) {
		t.Fatal("the accept loop evicted a reserved candidate during its success write")
	}

	// Drain the blocked response and let the candidate commit.
	if response, err := candidateClient.readResponse(); err != nil || response.status != 200 {
		t.Fatalf("reserved ANNOUNCE response = %v %v", response, err)
	}
	waitUntil(t, "the reserved candidate to take over", func() bool { return mgr.Active() == reserved })
	for _, stream := range fillers {
		stream.Close()
	}
}

// TestReservationTimeoutFreesABlockedStart bounds a success write that never
// completes: the candidate closes, the live sender stays, and a later session
// can still start.
func TestReservationTimeoutFreesABlockedStart(t *testing.T) {
	oldTimeout := reservationTimeout
	reservationTimeout = 50 * time.Millisecond
	defer func() { reservationTimeout = oldTimeout }()

	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	ownerStream, ownerClient := dialSender(t, mgr, transport)
	defer ownerStream.Close()
	announce(t, ownerClient)
	waitUntil(t, "the owner session", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()
	gen := mgr.board.CurrentSession()
	waitUntil(t, "the owner advertisement", func() bool { return advertiser.count("_airplay._tcp") == 2 })
	published := advertiser.count("_airplay._tcp")

	blockedStream, blockedClient := dialSender(t, mgr, transport)
	defer blockedStream.Close()
	body := legacyAnnounce()
	request := fmt.Sprintf("ANNOUNCE /1 RTSP/1.0\r\nCSeq: 1\r\nUser-Agent: AirPlay/600.1\r\nContent-Length: %d\r\n\r\n", len(body))
	if _, err := blockedClient.conn.Write(append([]byte(request), body...)); err != nil {
		t.Fatalf("ANNOUNCE write: %v", err)
	}
	waitUntil(t, "the candidate to reserve ownership", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return mgr.reserved != nil
	})
	waitUntil(t, "the stalled reservation to expire", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return mgr.reserved == nil
	})
	if mgr.Active() != owner || mgr.board.CurrentSession() != gen {
		t.Fatal("an expired reservation disturbed the live sender")
	}
	if got := advertiser.count("_airplay._tcp"); got != published {
		t.Fatalf("an expired reservation republished: %d, want %d", got, published)
	}

	// The reservation slot is free again.
	nextStream, nextClient := dialSender(t, mgr, transport)
	defer nextStream.Close()
	announce(t, nextClient)
	waitUntil(t, "a later session to start", func() bool { return mgr.Active() != nil && mgr.Active() != owner })
}

// TestReservationPreventsCandidateExpiry proves the candidate timeout cannot
// close a connection that reserved or committed ownership.
func TestReservationPreventsCandidateExpiry(t *testing.T) {
	oldCandidate := candidateRequestTimeout
	oldReservation := reservationTimeout
	candidateRequestTimeout = 40 * time.Millisecond
	reservationTimeout = time.Second
	defer func() {
		candidateRequestTimeout = oldCandidate
		reservationTimeout = oldReservation
	}()

	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, _ := dialSender(t, mgr, transport)
	defer stream.Close()
	waitUntil(t, "the candidate to register", func() bool { return pendingCount(mgr) == 1 })
	candidate := candidateForTest(t, mgr)
	if !mgr.reserve(candidate) {
		t.Fatal("reserve failed")
	}
	time.Sleep(80 * time.Millisecond)
	if sessionDone(candidate) {
		t.Fatal("candidate timeout closed a reserved connection")
	}
	if !mgr.commit(candidate) {
		t.Fatal("commit failed")
	}
	time.Sleep(80 * time.Millisecond)
	if sessionDone(candidate) || mgr.Active() != candidate {
		t.Fatal("candidate timeout closed a committed owner")
	}
}

// TestSameOwnerSetupWithoutGIDPreservesGroup covers the ordinary streams SETUP
// that omits gid: the same sender's previously established group id must stay
// advertised, not be cleared by an absent key.
func TestSameOwnerSetupWithoutGIDPreservesGroup(t *testing.T) {
	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()
	announce(t, client)
	waitUntil(t, "the owner session", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()

	first, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody(t, "owner-gid", pcmStream()))
	if err != nil || first.status != 200 {
		t.Fatalf("first SETUP = %v %v", first, err)
	}
	waitUntil(t, "the first stream", func() bool { return len(owner.activeStreams()) == 1 })
	waitUntil(t, "the owner group id", func() bool { return managerGroupID(mgr) == "owner-gid" })
	waitAdvertGID(t, advertiser, "owner-gid")

	// A second stream without gid must not clear the same owner's group.
	second, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody(t, "", pcmStream()))
	if err != nil || second.status != 200 {
		t.Fatalf("second SETUP = %v %v", second, err)
	}
	waitUntil(t, "the second stream", func() bool { return len(owner.activeStreams()) == 2 })
	if got := managerGroupID(mgr); got != "owner-gid" {
		t.Fatalf("SETUP without gid changed the owner gid to %q", got)
	}
	waitAdvertGID(t, advertiser, "owner-gid")

	// A different gid on the same owner is accepted.
	third, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody(t, "new-gid", pcmStream()))
	if err != nil || third.status != 200 {
		t.Fatalf("third SETUP = %v %v", third, err)
	}
	waitUntil(t, "the changed group id", func() bool { return managerGroupID(mgr) == "new-gid" })
	waitAdvertGID(t, advertiser, "new-gid")
}

// TestWrongTypeStreamsIsRefusedWithoutPromotion distinguishes a malformed
// stream SETUP (the key is present but not a list) from the event-channel SETUP
// that omits it: the malformed request must be refused before ownership.
func TestWrongTypeStreamsIsRefusedWithoutPromotion(t *testing.T) {
	mgr, transport, advertiser, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	ownerStream, ownerClient := dialSender(t, mgr, transport)
	defer ownerStream.Close()
	announce(t, ownerClient)
	waitUntil(t, "the owner session", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()
	waitUntil(t, "the active advertisement", func() bool { return advertiser.count("_airplay._tcp") == 2 })
	published := advertiser.count("_airplay._tcp")

	candidateStream, candidateClient := dialSender(t, mgr, transport)
	defer candidateStream.Close()
	body, err := plist.EncodeBinary(map[string]any{"gid": "candidate-gid", "streams": "not-a-list"})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	response, err := candidateClient.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err != nil || response.status != 400 {
		t.Fatalf("wrong-type SETUP = %v %v, want 400", response, err)
	}
	if mgr.Active() != owner {
		t.Fatal("a wrong-type SETUP took the live session")
	}
	if got := managerGroupID(mgr); got != "" {
		t.Fatalf("wrong-type SETUP changed gid to %q", got)
	}
	if got := advertiser.count("_airplay._tcp"); got != published {
		t.Fatalf("wrong-type SETUP republished: %d records, want %d", got, published)
	}
	waitUntil(t, "the candidate to remain pending", func() bool { return pendingCount(mgr) == 1 })
	candidate := candidateForTest(t, mgr)
	if len(candidate.stagedStreams()) != 0 || len(candidate.activeStreams()) != 0 {
		t.Fatal("wrong-type SETUP created a stream")
	}
}

// TestReserveFailureRefusesAndCloses proves a candidate that cannot secure
// ownership never receives a success and does not linger: it gets an explicit
// 503 and the control connection closes.
func TestReserveFailureRefusesAndCloses(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	holderStream, _ := dialSender(t, mgr, transport)
	defer holderStream.Close()
	waitUntil(t, "the holder candidate", func() bool { return pendingCount(mgr) == 1 })
	holder := candidateForTest(t, mgr)
	if !mgr.reserve(holder) {
		t.Fatal("holder could not reserve")
	}

	blockedStream, blockedClient := dialSender(t, mgr, transport)
	defer blockedStream.Close()
	response, err := blockedClient.do("ANNOUNCE", "/1", "", legacyAnnounce())
	if err != nil {
		t.Fatalf("blocked ANNOUNCE: %v", err)
	}
	if response.status != 503 {
		t.Fatalf("blocked ANNOUNCE = %d, want an explicit 503", response.status)
	}
	if mgr.Active() != nil {
		t.Fatal("a candidate without ownership became the live session")
	}
	// The refusal is followed by a closed control connection.
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := blockedClient.reader.Read(buf)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("the refused connection stayed open")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refused connection was not closed")
	}
}

// TestCleanupClosesControlConnectionOnReadError proves the cleanup path that
// never went through close still releases the TCP socket: a malformed request
// ends the session and the sender observes EOF.
func TestCleanupClosesControlConnectionOnReadError(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	defer stream.Close()
	if _, err := client.conn.Write([]byte("BOGUS\r\n\r\n")); err != nil {
		t.Fatalf("write malformed request: %v", err)
	}
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := client.reader.Read(buf)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("the control connection stayed open after a read error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the control connection was not closed after a read error")
	}
	waitUntil(t, "the failed session to leave the pending set", func() bool { return pendingCount(mgr) == 0 })
	if mgr.Active() != nil {
		t.Fatal("a malformed request became the live session")
	}
}

// TestPairingAndEncryptedProbeDoNotOwnMedia is the lifecycle regression for a
// sender that completes transient HAP pairing and sends an encrypted benign
// request but never starts a stream. The connection is authenticated, yet it
// must not start the board, change the selected media state, evict the live
// sender or withdraw/replace the advertisements. A later accepted SETUP on
// that same connection promotes it; a malformed SETUP before that leaves the
// prior owner in place. This uses the real loopback transport, not net.Pipe.
func TestPairingAndEncryptedProbeDoNotOwnMedia(t *testing.T) {
	transport := &realTransport{}
	t.Cleanup(transport.Close)
	advertiser := &fakeAdvertiser{}
	ring := &fakeRing{}
	sink := &fakeSink{}
	board := media.NewBoard()
	mgr, err := NewManager(Config{
		DeviceID: "aabbccddeeff", Name: "Ownership Loopback", Port: 0,
		FrameTarget: 20 * time.Millisecond, RingEnabled: true, Logf: t.Logf,
	}, transport, advertiser, fakeRings{ring: ring}, fakeSinks{sink: sink},
		board, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()
	port := mgr.advertisedPort()

	// Establish the live media owner with the reference's accepted ANNOUNCE
	// path. Pairing is deliberately not used to create this owner.
	ownerConn := dialTCPStream(t, port)
	defer ownerConn.Close()
	ownerClient := newRTSPClient(ownerConn)
	announce(t, ownerClient)
	waitUntil(t, "the announced owner", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()
	ownerGen := board.CurrentSession()
	waitUntil(t, "the owner advertisement", func() bool {
		return advertiser.count("_airplay._tcp") == 2 && advertiser.count("_raop._tcp") == 2
	})
	beforeAirplay := advertiser.count("_airplay._tcp")
	beforeRAOP := advertiser.count("_raop._tcp")
	beforeStatus := mgr.advertisedStatus()
	beforeBoard := board.Snapshot()
	beforeRing, _ := ring.snapshot()
	beforeSink, _ := sink.state()

	// A second connection completes transient pairing and receives an
	// encrypted /info. It is authenticated but has not started a session.
	candidateConn := dialTCPStream(t, port)
	defer candidateConn.Close()
	client := newRTSPClient(candidateConn)
	sessionKey, err := pairSetupTransient(t, client, pair.DefaultPIN)
	if err != nil {
		t.Fatalf("transient pair-setup: %v", err)
	}
	control := hapcrypt.ControlChannelKeys(sessionKey)
	encrypted, err := hapcrypt.NewSession(candidateConn, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("candidate encrypted channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})
	probe, err := client.do("GET", "/info", "", nil)
	if err != nil || probe.status != 200 {
		t.Fatalf("encrypted GET /info = %v %v", probe, err)
	}
	time.Sleep(50 * time.Millisecond)

	if got := mgr.Active(); got != owner {
		t.Fatal("encrypted pairing/probe took media ownership from the live sender")
	}
	if got := board.CurrentSession(); got != ownerGen {
		t.Fatalf("selected media generation = %d, want %d", got, ownerGen)
	}
	if !reflect.DeepEqual(board.Snapshot(), beforeBoard) {
		t.Fatalf("selected media state changed: got %#v, want %#v", board.Snapshot(), beforeBoard)
	}
	if frames, _ := ring.snapshot(); frames != beforeRing {
		t.Fatalf("ring frames changed from %d to %d", beforeRing, frames)
	}
	if written, _ := sink.state(); written != beforeSink {
		t.Fatalf("sink bytes changed from %d to %d", beforeSink, written)
	}
	if got := advertiser.count("_airplay._tcp"); got != beforeAirplay {
		t.Fatalf("pairing/probe republished _airplay: %d, want %d", got, beforeAirplay)
	}
	if got := advertiser.count("_raop._tcp"); got != beforeRAOP {
		t.Fatalf("pairing/probe republished _raop: %d, want %d", got, beforeRAOP)
	}
	if got := mgr.advertisedStatus(); got != beforeStatus {
		t.Fatalf("pairing/probe changed statusFlags from %#x to %#x", beforeStatus, got)
	}

	// The encrypted event-only SETUP configures the event channel but is not a
	// media start. It must leave the owner and advertisements alone even after
	// the sender connects the event peer and receives updateInfo.
	eventBody, err := plist.EncodeBinary(map[string]any{
		"gid":            "candidate-gid",
		"timingProtocol": "PTP",
		"timingPeerInfo": map[string]any{
			"ID":        "candidate-loopback",
			"Addresses": []any{"127.0.0.1"},
		},
	})
	if err != nil {
		t.Fatalf("encode event-only SETUP: %v", err)
	}
	eventSetup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", eventBody)
	if err != nil || eventSetup.status != 200 {
		t.Fatalf("event-only SETUP = %v %v", eventSetup, err)
	}
	eventDecoded, err := plist.Decode(eventSetup.body)
	if err != nil {
		t.Fatalf("decode event-only SETUP: %v", err)
	}
	eventDict, _ := eventDecoded.(map[string]any)
	eventPort := int(plistUint(eventDict["eventPort"]))
	if eventPort == 0 {
		t.Fatalf("event-only SETUP eventPort = %v", eventDict["eventPort"])
	}
	if got := mgr.Active(); got != owner {
		t.Fatal("event-only SETUP took media ownership from the live sender")
	}
	if got := board.CurrentSession(); got != ownerGen {
		t.Fatalf("event-only SETUP changed the media generation to %d", got)
	}
	if got := advertiser.count("_airplay._tcp"); got != beforeAirplay {
		t.Fatalf("event-only SETUP republished _airplay: %d, want %d", got, beforeAirplay)
	}

	eventConn := dialTCPStream(t, eventPort)
	defer eventConn.Close()
	eventKeys := hapcrypt.EventChannelKeys(sessionKey)
	eventSession, err := hapcrypt.NewSession(eventConn, eventKeys.Inbound, eventKeys.Outbound)
	if err != nil {
		t.Fatalf("event channel keys: %v", err)
	}
	firstEventBlock, err := eventSession.ReadOne()
	if err != nil {
		t.Fatalf("read event updateInfo: %v", err)
	}
	if !bytes.HasPrefix(firstEventBlock, []byte("POST /command")) {
		t.Fatalf("event channel did not carry updateInfo: %q", firstEventBlock[:minInt(len(firstEventBlock), 32)])
	}
	if got := mgr.Active(); got != owner {
		t.Fatal("connected event peer took media ownership")
	}
	// Metadata arriving on the candidate's event channel is stored but must not
	// mutate the owner's board; promotion replays it into the new generation.
	eventMetadata, err := plist.EncodeBinary(map[string]any{
		"deviceName": "Event iPhone",
		"params": map[string]any{
			"kMRMediaRemoteNowPlayingInfoTitle":  "Pending Title",
			"kMRMediaRemoteNowPlayingInfoArtist": "Pending Artist",
		},
	})
	if err != nil {
		t.Fatalf("encode event metadata: %v", err)
	}
	eventRequest := fmt.Sprintf("POST /command RTSP/1.0\r\nCSeq: 1\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: %d\r\n\r\n", len(eventMetadata))
	if _, err := eventSession.Write(append([]byte(eventRequest), eventMetadata...)); err != nil {
		t.Fatalf("write event metadata: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := board.Snapshot()["title"]; got != nil {
		t.Fatalf("candidate event metadata reached the live board: %v", got)
	}

	// A type-130 control-only stream SETUP is acknowledged but still is not a
	// media start.
	controlBody, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{"type": int64(streamTypeRemoteControl)}},
	})
	if err != nil {
		t.Fatalf("encode control-only SETUP: %v", err)
	}
	controlSetup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", controlBody)
	if err != nil || controlSetup.status != 200 {
		t.Fatalf("control-only SETUP = %v %v", controlSetup, err)
	}
	if got := mgr.Active(); got != owner {
		t.Fatal("type-130 SETUP took media ownership from the live sender")
	}
	if got := board.CurrentSession(); got != ownerGen {
		t.Fatalf("type-130 SETUP changed the media generation to %d", got)
	}

	// A malformed SETUP on the authenticated candidate is refused before
	// ownership; the prior owner keeps the session and the advertisements.
	bad, err := plist.EncodeBinary(map[string]any{"streams": "not-a-list"})
	if err != nil {
		t.Fatalf("encode malformed SETUP: %v", err)
	}
	refused, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", bad)
	if err != nil || refused.status != 400 {
		t.Fatalf("malformed SETUP = %v %v, want 400", refused, err)
	}
	if got := mgr.Active(); got != owner {
		t.Fatal("malformed SETUP replaced the live sender")
	}
	if got := board.CurrentSession(); got != ownerGen {
		t.Fatalf("malformed SETUP changed the media generation to %d", got)
	}
	if got := advertiser.count("_airplay._tcp"); got != beforeAirplay {
		t.Fatalf("malformed SETUP republished _airplay: %d, want %d", got, beforeAirplay)
	}

	// A successful SETUP on the same authenticated connection is the first
	// media/session initiation and does promote, taking over from the prior
	// owner as an accepted SETUP is allowed to.
	setupBody, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeRealtime),
			"audioFormat": int64(1 << 11), // PCM 44.1 kHz stereo
			"spf":         int64(352),
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody)
	if err != nil || setup.status != 200 {
		t.Fatalf("accepted SETUP = %v %v", setup, err)
	}
	waitUntil(t, "the accepted SETUP to promote", func() bool { return mgr.Active() != owner })
	if mgr.Active() == nil {
		t.Fatal("accepted SETUP did not promote the authenticated connection")
	}
	if !mgr.Active().encrypted {
		t.Fatal("promoted connection is not the encrypted candidate")
	}
	if got := board.CurrentSession(); got == ownerGen {
		t.Fatal("accepted SETUP did not start a new media generation")
	}
	if !board.Live() {
		t.Fatal("accepted SETUP left the board without a live session")
	}
	snapshot := board.Snapshot()
	if snapshot["title"] != "Pending Title" || snapshot["artist"] != "Pending Artist" {
		t.Fatalf("promotion lost pending event metadata: %#v", snapshot)
	}
	if snapshot["sourceName"] != "Event iPhone" {
		t.Fatalf("promotion lost the pending event device name: %#v", snapshot)
	}
	if got := managerGroupID(mgr); got != "candidate-gid" {
		t.Fatalf("promotion lost the candidate group id: got %q", got)
	}
}

// failingWriteStream injects a deterministic write failure after pairing so the
// successful SETUP response cannot be delivered. The serving goroutine must
// release the reservation instead of committing media ownership.
type failingWriteStream struct {
	Stream
	fail atomic.Bool
}

func (s *failingWriteStream) Write(p []byte) (int, error) {
	if s.fail.Load() {
		return 0, io.ErrShortWrite
	}
	return s.Stream.Write(p)
}

// TestShortSetupResponseDoesNotCommitOwnership pins the write-before-commit
// boundary: the candidate reserves and stages its SETUP, but a failed success
// response must leave the old owner, its media generation and the ADS
// untouched, and the connection must close without a 200 claim.
func TestShortSetupResponseDoesNotCommitOwnership(t *testing.T) {
	mgr, transport, advertiser, _, sink, clientKey, pairingID := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	ownerStream, ownerClient := dialSender(t, mgr, transport)
	defer ownerStream.Close()
	announce(t, ownerClient)
	waitUntil(t, "the live owner", func() bool { return mgr.Active() != nil })
	owner := mgr.Active()
	ownerGen := mgr.board.CurrentSession()
	waitUntil(t, "the owner advertisement", func() bool {
		return advertiser.count("_airplay._tcp") == 2
	})
	beforeAirplay := advertiser.count("_airplay._tcp")
	beforeSink, _ := sink.state()

	local, remote := net.Pipe()
	serverStream := &failingWriteStream{Stream: &pipeStream{
		conn: remote, peer: Peer{Address: "short-write.test", Port: 50001},
	}}
	port := mgr.advertisedPort()
	transport.mu.Lock()
	listener := transport.listeners[port]
	transport.mu.Unlock()
	if listener == nil {
		t.Fatalf("no listener on port %d", port)
	}
	listener.conns <- serverStream
	candidateStream := &pipeStream{conn: local, peer: Peer{Address: "short-write.test", Port: 50001}}
	candidate := newRTSPClient(candidateStream)

	// The pairing responses are written normally. Only the SETUP success
	// response fails, after the reservation and staged streams exist.
	shared := pairVerify(t, candidate, pairingID, clientKey)
	serverStream.fail.Store(true)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(candidateStream, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("candidate channel: %v", err)
	}
	candidate.setStream(&rwStream{rw: encrypted})
	body, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeRealtime),
			"audioFormat": int64(1 << 11),
			"spf":         int64(352),
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	response, err := candidate.do("SETUP", "/1", "application/x-apple-binary-plist", body)
	if err == nil && response != nil && response.status == 200 {
		t.Fatal("the failed SETUP response was delivered as a 200")
	}
	waitUntil(t, "the failed candidate to leave pending", func() bool { return pendingCount(mgr) == 0 })
	if got := mgr.Active(); got != owner {
		t.Fatal("a failed SETUP response committed ownership")
	}
	if got := mgr.board.CurrentSession(); got != ownerGen {
		t.Fatalf("a failed SETUP response changed the media generation to %d", got)
	}
	if got := advertiser.count("_airplay._tcp"); got != beforeAirplay {
		t.Fatalf("a failed SETUP response republished _airplay: %d, want %d", got, beforeAirplay)
	}
	if written, _ := sink.state(); written != beforeSink {
		t.Fatalf("a failed SETUP response changed the sink from %d to %d", beforeSink, written)
	}
	mgr.mu.Lock()
	reserved := mgr.reserved
	mgr.mu.Unlock()
	if reserved != nil {
		t.Fatal("a failed SETUP response left the reservation held")
	}
}

// lateEventListener accepts one peer after Close, modeling a transport whose
// Accept completes late. The session must reject and close it rather than
// publishing an event channel after teardown.
type lateEventListener struct {
	peers   chan Stream
	closed  chan struct{}
	served  atomic.Bool
	started chan struct{}
}

func (l *lateEventListener) Accept() (Stream, error) {
	if !l.served.Swap(true) {
		close(l.started)
		return <-l.peers, nil
	}
	<-l.closed
	return nil, io.EOF
}

func (l *lateEventListener) Addr() (string, int) { return "late.event", 7002 }
func (l *lateEventListener) Close() error        { close(l.closed); return nil }

type trackedPeerStream struct {
	peer   Peer
	closed atomic.Bool
	writes atomic.Int64
}

func (s *trackedPeerStream) Read([]byte) (int, error)  { return 0, io.EOF }
func (s *trackedPeerStream) Write([]byte) (int, error) { s.writes.Add(1); return 0, io.ErrClosedPipe }
func (s *trackedPeerStream) Close() error              { s.closed.Store(true); return nil }
func (s *trackedPeerStream) Peer() Peer                { return s.peer }

// TestLateEventAcceptAfterCleanupIsClosed proves the accept/cleanup race is
// closed: a peer returned by Accept after cleanup has closed the listener is
// rejected, closed, and never receives updateInfo or starts a reader.
func TestLateEventAcceptAfterCleanupIsClosed(t *testing.T) {
	mgr, _, _, _, _, _, _ := newTestManager(t)
	listener := &lateEventListener{
		peers:   make(chan Stream, 1),
		closed:  make(chan struct{}),
		started: make(chan struct{}),
	}
	s := &Session{
		m:             mgr,
		done:          make(chan struct{}),
		peer:          Peer{Address: "late.event", Port: 50000},
		encrypted:     true,
		eventListener: listener,
	}
	go s.acceptEventConnections(listener)
	select {
	case <-listener.started:
	case <-time.After(2 * time.Second):
		t.Fatal("event Accept did not start")
	}
	s.cleanup("teardown")
	peer := &trackedPeerStream{peer: Peer{Address: "late.peer", Port: 50001}}
	listener.peers <- peer
	waitUntil(t, "the late event peer to close", func() bool { return peer.closed.Load() })
	if peer.writes.Load() != 0 {
		t.Fatalf("late event peer received %d write(s) after teardown", peer.writes.Load())
	}
	s.eventMu.Lock()
	if s.eventConn != nil || s.eventOut != nil || s.eventIn != nil {
		s.eventMu.Unlock()
		t.Fatal("late event peer was published after teardown")
	}
	s.eventMu.Unlock()
}
