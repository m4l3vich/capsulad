package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"capsulad/apps/airplay/internal/hapcrypt"
)

// logCollector serialises the receiver's log lines for assertions.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCollector) logf(format string, args ...any) {
	c.mu.Lock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
	c.mu.Unlock()
}

func (c *logCollector) matching(needle string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range c.lines {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

// TestCandidateSummaryLogsStageOnce pins the field diagnostic: a connection
// that sends requests but never starts a session leaves exactly one bounded
// line naming only the allowlisted last method and status, while a connection
// that sends nothing (a dual-stack loser or port probe) stays silent.
func TestCandidateSummaryLogsStageOnce(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	waitUntil(t, "the candidate to register", func() bool { return pendingCount(mgr) == 1 })
	if _, err := client.do("OPTIONS", "*", "", nil); err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	mgr.sessionWG.Wait()

	lines := logs.matching("ended before starting a session")
	if len(lines) != 1 {
		t.Fatalf("%d candidate summary lines, want 1: %v", len(lines), lines)
	}
	for _, want := range []string{"1 request(s)", "last OPTIONS -> 200"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("summary %q does not contain %q", lines[0], want)
		}
	}

	silent, _ := dialSender(t, mgr, transport)
	waitUntil(t, "the silent candidate to register", func() bool { return pendingCount(mgr) == 1 })
	if err := silent.Close(); err != nil {
		t.Fatalf("close silent connection: %v", err)
	}
	mgr.sessionWG.Wait()
	if lines := logs.matching("ended before starting a session"); len(lines) != 1 {
		t.Fatalf("a silent candidate logged a summary: %v", lines)
	}

	ownerStream, ownerClient := dialSender(t, mgr, transport)
	announce(t, ownerClient)
	waitUntil(t, "the live session", func() bool { return mgr.Active() != nil })
	if err := ownerStream.Close(); err != nil {
		t.Fatalf("close owner: %v", err)
	}
	mgr.sessionWG.Wait()
	if lines := logs.matching("ended before starting a session"); len(lines) != 1 {
		t.Fatalf("a live session logged a candidate summary: %v", lines)
	}
}

// TestCandidateSummaryUsesStaticStage pins that a sender-supplied path never
// reaches the summary: an unlisted path is reported as "other", not echoed.
func TestCandidateSummaryUsesStaticStage(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	waitUntil(t, "the candidate to register", func() bool { return pendingCount(mgr) == 1 })
	if _, err := client.do("POST", "/unknown-secret", "application/x-apple-binary-plist", []byte("x")); err != nil {
		t.Fatalf("POST: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	mgr.sessionWG.Wait()

	lines := logs.matching("ended before starting a session")
	if len(lines) != 1 {
		t.Fatalf("%d candidate summary lines, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "last other -> 404") {
		t.Fatalf("summary %q does not report the static stage", lines[0])
	}
	if strings.Contains(lines[0], "unknown-secret") {
		t.Fatalf("summary echoes the sender-supplied path: %q", lines[0])
	}
}

// TestCandidateStageKeepsBothAnchorSpellings pins the sanitized method
// vocabulary: the reference literal and the field client's full spelling are
// both reported verbatim, while an unlisted spelling stays "other" and is
// never echoed into the summary.
func TestCandidateStageKeepsBothAnchorSpellings(t *testing.T) {
	for _, method := range []string{"SETRATEANCHORTI", "SETRATEANCHORTIME"} {
		if got := candidateStage(method, "/1"); got != method {
			t.Errorf("candidateStage(%q) = %q, want the allowlisted literal", method, got)
		}
	}
	if got := candidateStage("SETRATEANCHORTISECRET", "/1"); got != "other" {
		t.Errorf("candidateStage(unlisted anchor spelling) = %q, want other", got)
	}
}

// TestCandidateSummaryCountsReservationRefusal covers the request that is
// parsed but answered with the explicit 503 refusal instead of its computed
// status: the summary still counts it as the last request and reports 503.
func TestCandidateSummaryCountsReservationRefusal(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
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
	waitUntil(t, "the blocked candidate", func() bool { return pendingCount(mgr) == 1 })
	response, err := blockedClient.do("ANNOUNCE", "/1", "", legacyAnnounce())
	if err != nil {
		t.Fatalf("blocked ANNOUNCE: %v", err)
	}
	if response.status != 503 {
		t.Fatalf("blocked ANNOUNCE = %d, want 503", response.status)
	}
	waitUntil(t, "the refusal summary", func() bool {
		return len(logs.matching("last ANNOUNCE -> 503")) == 1
	})
}

// TestCandidateSummaryIsRateLimited pins the receiver-wide bound: two
// request-sending candidates that end inside one interval produce one summary
// line, not two.
func TestCandidateSummaryIsRateLimited(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	for i := range 2 {
		stream, client := dialSender(t, mgr, transport)
		waitUntil(t, "the candidate to register", func() bool { return pendingCount(mgr) == 1 })
		if _, err := client.do("OPTIONS", "*", "", nil); err != nil {
			t.Fatalf("OPTIONS %d: %v", i, err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
		mgr.sessionWG.Wait()
	}
	if lines := logs.matching("ended before starting a session"); len(lines) != 1 {
		t.Fatalf("%d summaries inside one interval, want 1: %v", len(lines), lines)
	}
}

// TestSupersededSessionIsNotLoggedAsCandidate pins that a sender that did own
// the receiver and was then taken over is not reported as a candidate; its own
// close line is the record.
func TestSupersededSessionIsNotLoggedAsCandidate(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	firstStream, firstClient := dialSender(t, mgr, transport)
	defer firstStream.Close()
	announce(t, firstClient)
	waitUntil(t, "the first live session", func() bool { return mgr.Active() != nil })
	first := mgr.Active()

	secondStream, secondClient := dialSender(t, mgr, transport)
	defer secondStream.Close()
	waitUntil(t, "the second candidate", func() bool { return pendingCount(mgr) == 1 })
	announce(t, secondClient)
	waitUntil(t, "the takeover", func() bool { return mgr.Active() != nil && mgr.Active() != first })

	if err := secondStream.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
	mgr.sessionWG.Wait()
	if lines := logs.matching("ended before starting a session"); len(lines) != 0 {
		t.Fatalf("a superseded session was logged as a candidate: %v", lines)
	}
}

// stalledStream accepts one request and then stalls every write until Close.
// It simulates a peer that never drains the socket, the case the bounded
// refusal write exists for.
type stalledStream struct {
	reader *bytes.Reader
	done   chan struct{}
	once   sync.Once
}

func (s *stalledStream) Read(p []byte) (int, error) {
	if s.reader.Len() > 0 {
		return s.reader.Read(p)
	}
	<-s.done
	return 0, io.EOF
}

func (s *stalledStream) Write(p []byte) (int, error) {
	<-s.done
	return 0, errors.New("write stalled")
}

func (s *stalledStream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func (s *stalledStream) Peer() Peer { return Peer{Address: "sender.test", Port: 50000} }

// TestCandidateSummaryMarksTimedOutRefusal covers the P2: when the explicit
// 503 refusal cannot be written before its bound, the serving goroutine must
// record the failed write itself so the summary never claims a delivered 503.
func TestCandidateSummaryMarksTimedOutRefusal(t *testing.T) {
	mgr, transport, _, _, _, _, _ := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	oldTimeout := refusalWriteTimeout
	refusalWriteTimeout = 50 * time.Millisecond
	defer func() { refusalWriteTimeout = oldTimeout }()

	holderStream, _ := dialSender(t, mgr, transport)
	defer holderStream.Close()
	waitUntil(t, "the holder candidate", func() bool { return pendingCount(mgr) == 1 })
	holder := candidateForTest(t, mgr)
	if !mgr.reserve(holder) {
		t.Fatal("holder could not reserve")
	}

	announceBody := legacyAnnounce()
	request := fmt.Sprintf("ANNOUNCE /1 RTSP/1.0\r\nCSeq: 1\r\nUser-Agent: test\r\nContent-Length: %d\r\n\r\n", len(announceBody))
	stalled := &stalledStream{
		reader: bytes.NewReader(append([]byte(request), announceBody...)),
		done:   make(chan struct{}),
	}
	port := mgr.advertisedPort()
	transport.mu.Lock()
	listener := transport.listeners[port]
	transport.mu.Unlock()
	if listener == nil {
		t.Fatalf("no listener on port %d", port)
	}
	listener.conns <- stalled

	waitUntil(t, "the timed-out refusal summary", func() bool {
		return len(logs.matching("last ANNOUNCE -> 503, last write failed")) == 1
	})
}

// TestEncryptedCandidateSummaryReportsReadBoundary is the discriminating
// diagnostic for a paired connection that ends before media starts: it must
// show the post-upgrade request count and first/last allowlisted stages, plus
// the fixed HAP outcome/blocks and RTSP parse class. No path, header, body or
// raw error is present.
func TestEncryptedCandidateSummaryReportsReadBoundary(t *testing.T) {
	mgr, transport, _, _, _, clientKey, pairingID := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	waitUntil(t, "the encrypted candidate", func() bool { return pendingCount(mgr) == 1 })
	shared := pairVerify(t, client, pairingID, clientKey)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(stream, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("client channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})
	if response, err := client.do("OPTIONS", "*", "", nil); err != nil || response.status != 200 {
		t.Fatalf("encrypted OPTIONS = %v %v", response, err)
	}
	if response, err := client.do("GET", "/info", "", nil); err != nil || response.status != 200 {
		t.Fatalf("encrypted GET /info = %v %v", response, err)
	}
	if mgr.Active() != nil {
		t.Fatal("an encrypted pairing/probe candidate became the media owner")
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	mgr.sessionWG.Wait()

	lines := logs.matching("ended before starting a session")
	if len(lines) != 1 {
		t.Fatalf("%d candidate summary lines, want 1: %v", len(lines), lines)
	}
	for _, want := range []string{
		"post-upgrade 2",
		"first OPTIONS -> 200",
		"last GET /info -> 200",
		"hap=eof/2",
		"rtsp=none",
	} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("summary %q does not contain %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "/pair-verify") || strings.Contains(lines[0], "RTSP/") {
		t.Fatalf("summary leaked a raw path or request line: %q", lines[0])
	}
}

// TestRTSPParseClassSeparatesHAPFromRTSP pins the layer classification used by
// the encrypted-candidate summary. A HAP tag/truncation error wrapped by the
// RTSP body reader must not also claim an RTSP parse failure, while a bare
// partial request at a clean HAP EOF is reported as truncated.
func TestRTSPParseClassSeparatesHAPFromRTSP(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"hap wrapped by body", fmt.Errorf("rtsp: reading body: %w", errors.New("hapcrypt: authentication tag mismatch")), ""},
		{"hap partial length", fmt.Errorf("hapcrypt: reading block length: %w", io.ErrUnexpectedEOF), ""},
		{"rtsp body", fmt.Errorf("rtsp: truncated body (3 of 9 bytes): %w", io.ErrUnexpectedEOF), "body"},
		{"bare truncated line", io.ErrUnexpectedEOF, "truncated"},
		{"invalid content length quotes header text", errors.New("rtsp: invalid Content-Length \"folded header\": syntax error"), "content-length"},
		{"rtsp request line", errors.New("rtsp: malformed request line"), "request-line"},
		{"clean eof", io.EOF, ""},
	} {
		if got := rtspParseClass(tc.err); got != tc.want {
			t.Errorf("%s: rtspParseClass = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEncryptedCandidateSummaryAttributesTruncatedBlockToHAP sends a HAP
// length prefix with an incomplete ciphertext/tag. RTSP may replace the
// hapcrypt error with a bare header/body EOF while reading; the recorded HAP
// outcome must keep the summary's RTSP class empty.
func TestEncryptedCandidateSummaryAttributesTruncatedBlockToHAP(t *testing.T) {
	mgr, transport, _, _, _, clientKey, pairingID := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	waitUntil(t, "the candidate", func() bool { return pendingCount(mgr) == 1 })
	shared := pairVerify(t, client, pairingID, clientKey)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(stream, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("client channel: %v", err)
	}
	// One complete HAP block carries RTSP headers that declare a body. The
	// body is then cut off by an incomplete raw HAP frame, so the RTSP body
	// read sees a partial encrypted block rather than a clean EOF or a first
	// frame truncation in Peek.
	if _, err := encrypted.Write([]byte("SETUP /1 RTSP/1.0\r\nCSeq: 3\r\nContent-Length: 10\r\n\r\n")); err != nil {
		t.Fatalf("write header block: %v", err)
	}
	if _, err := stream.Write([]byte{0x10, 0x00, 0x01}); err != nil {
		t.Fatalf("write truncated HAP block: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitUntil(t, "the truncated-block summary", func() bool {
		return len(logs.matching("ended before starting a session")) == 1
	})
	line := logs.matching("ended before starting a session")[0]
	for _, want := range []string{"hap=partial-body/1", "rtsp=none"} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q does not contain %q", line, want)
		}
	}
	if strings.Contains(line, "rtsp=body") || strings.Contains(line, "rtsp=headers") {
		t.Fatalf("HAP truncation was mislabeled as an RTSP parse failure: %q", line)
	}
}

// TestEncryptedCandidateSummaryReportsTruncatedRTSPBody sends a complete HAP
// block carrying RTSP headers whose declared body never arrives. That is a real
// RTSP body failure at a clean HAP EOF, so both classes may be reported.
func TestEncryptedCandidateSummaryReportsTruncatedRTSPBody(t *testing.T) {
	mgr, transport, _, _, _, clientKey, pairingID := newTestManager(t)
	logs := &logCollector{}
	mgr.logf = logs.logf
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	stream, client := dialSender(t, mgr, transport)
	waitUntil(t, "the candidate", func() bool { return pendingCount(mgr) == 1 })
	shared := pairVerify(t, client, pairingID, clientKey)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(stream, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("client channel: %v", err)
	}
	if _, err := encrypted.Write([]byte("SETUP /1 RTSP/1.0\r\nCSeq: 3\r\nContent-Length: 10\r\n\r\n")); err != nil {
		t.Fatalf("write encrypted truncated body: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitUntil(t, "the truncated-body summary", func() bool {
		return len(logs.matching("ended before starting a session")) == 1
	})
	line := logs.matching("ended before starting a session")[0]
	for _, want := range []string{"hap=eof/1", "rtsp=body"} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary %q does not contain %q", line, want)
		}
	}
}
