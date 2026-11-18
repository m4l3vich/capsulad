package session

// End-to-end session test over in-memory transports.
//
// It acts as a sender: it completes the HomeKit pair-verify exchange, switches
// to the encrypted control channel, SETUPs an AirPlay 2 realtime PCM stream,
// pushes encrypted RTP audio, and checks what a household would observe — the
// framework sink receiving scaled PCM, ring frames, the media-contract status,
// and teardown releasing everything. No daemon, no device and no network are
// involved; the only production code bypassed is the SDK transport, which is
// exactly the seam the app is built around.

import (
	"bufio"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"capsulad/apps/airplay/internal/hapcrypt"
	"capsulad/apps/airplay/internal/media"
	"capsulad/apps/airplay/internal/plist"
	"capsulad/apps/airplay/internal/tlv8"
)

// --- fake transport -------------------------------------------------------

type fakeTransport struct {
	mu        sync.Mutex
	listeners map[int]*pipeListener
	udp       map[int]*fakeUDPListener
	nextPort  int
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{listeners: map[int]*pipeListener{}, udp: map[int]*fakeUDPListener{}, nextPort: 41000}
}

func (t *fakeTransport) OpenTCPListener(_ string, port int) (Listener, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if port == 0 {
		port = t.nextPort
		t.nextPort++
	}
	if _, exists := t.listeners[port]; exists {
		return nil, fmt.Errorf("port %d is already bound", port)
	}
	listener := &pipeListener{port: port, conns: make(chan Stream, 8), closed: make(chan struct{})}
	t.listeners[port] = listener
	return listener, nil
}

func (t *fakeTransport) OpenUDPListener(_ string, port int, group string) (UDPListener, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if port == 0 {
		port = t.nextPort
		t.nextPort++
	}
	if _, exists := t.udp[port]; exists {
		return nil, fmt.Errorf("port %d is already bound", port)
	}
	listener := &fakeUDPListener{
		port: port, group: group,
		in:     make(chan udpDatagram, 64),
		out:    make(chan udpDatagram, 64),
		closed: make(chan struct{}),
	}
	if t.udp == nil {
		t.udp = map[int]*fakeUDPListener{}
	}
	t.udp[port] = listener
	return listener, nil
}

// sendUDP returns a sender for the fake UDP socket bound to port: each call
// injects one datagram as the sender would.
func (t *fakeTransport) sendUDP(port int) (func([]byte) error, error) {
	t.mu.Lock()
	listener := t.udp[port]
	t.mu.Unlock()
	if listener == nil {
		return nil, fmt.Errorf("no UDP socket on port %d", port)
	}
	return func(data []byte) error {
		select {
		case listener.in <- udpDatagram{data: append([]byte(nil), data...), peer: UDPAddr{Address: "sender.test", Port: 50000}}:
			return nil
		case <-listener.closed:
			return errors.New("socket closed")
		}
	}, nil
}

// sentUDP drains what the receiver sent from a fake UDP socket.
func (t *fakeTransport) sentUDP(port int) [][]byte {
	t.mu.Lock()
	listener := t.udp[port]
	t.mu.Unlock()
	if listener == nil {
		return nil
	}
	var packets [][]byte
	for {
		select {
		case datagram := <-listener.out:
			packets = append(packets, datagram.data)
		default:
			return packets
		}
	}
}

type udpDatagram struct {
	data []byte
	peer UDPAddr
}

type fakeUDPListener struct {
	port   int
	group  string
	in     chan udpDatagram
	out    chan udpDatagram
	closed chan struct{}
	once   sync.Once
}

func (l *fakeUDPListener) Receive() ([]byte, UDPAddr, error) {
	select {
	case datagram := <-l.in:
		return datagram.data, datagram.peer, nil
	case <-l.closed:
		return nil, UDPAddr{}, errors.New("socket closed")
	}
}

func (l *fakeUDPListener) Send(peer UDPAddr, data []byte) error {
	select {
	case l.out <- udpDatagram{data: append([]byte(nil), data...), peer: peer}:
		return nil
	default:
		return nil
	}
}

func (l *fakeUDPListener) Addr() (string, int) { return "0.0.0.0", l.port }
func (l *fakeUDPListener) Group() string       { return l.group }

func (l *fakeUDPListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (t *fakeTransport) dial(port int) (Stream, error) {
	t.mu.Lock()
	listener := t.listeners[port]
	t.mu.Unlock()
	if listener == nil {
		return nil, fmt.Errorf("nothing listens on port %d", port)
	}
	local, remote := net.Pipe()
	select {
	case listener.conns <- &pipeStream{conn: remote, peer: Peer{Address: "receiver.test", Port: port}}:
	default:
		local.Close()
		remote.Close()
		return nil, errors.New("accept queue full")
	}
	return &pipeStream{conn: local, peer: Peer{Address: "sender.test", Port: 50000}}, nil
}

type pipeListener struct {
	port   int
	conns  chan Stream
	closed chan struct{}
	once   sync.Once
}

func (l *pipeListener) Accept() (Stream, error) {
	select {
	case stream := <-l.conns:
		return stream, nil
	case <-l.closed:
		return nil, errors.New("listener closed")
	}
}

func (l *pipeListener) Addr() (string, int) { return "0.0.0.0", l.port }

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

type pipeStream struct {
	conn net.Conn
	peer Peer
	once sync.Once
}

func (s *pipeStream) Read(p []byte) (int, error)  { return s.conn.Read(p) }
func (s *pipeStream) Write(p []byte) (int, error) { return s.conn.Write(p) }
func (s *pipeStream) Peer() Peer                  { return s.peer }

func (s *pipeStream) Close() error {
	var err error
	s.once.Do(func() { err = s.conn.Close() })
	return err
}

// --- other fakes ----------------------------------------------------------

type fakeAdvertiser struct {
	mu  sync.Mutex
	all []Advert
}

func (a *fakeAdvertiser) Publish(advert Advert) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.all = append(a.all, advert)
	return fmt.Sprintf("%s-%d", advert.Type, len(a.all)), nil
}

func (a *fakeAdvertiser) Unpublish(string) error { return nil }

func (a *fakeAdvertiser) find(serviceType string) (Advert, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.all) - 1; i >= 0; i-- {
		if a.all[i].Type == serviceType {
			return a.all[i], true
		}
	}
	return Advert{}, false
}

func (a *fakeAdvertiser) count(serviceType string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, advert := range a.all {
		if advert.Type == serviceType {
			n++
		}
	}
	return n
}

type fakeRing struct {
	mu     sync.Mutex
	frames [][]byte
	closed bool
}

func (r *fakeRing) Pixels(pixels []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(pixels) != 36 {
		return fmt.Errorf("ring frame is %d bytes", len(pixels))
	}
	r.frames = append(r.frames, append([]byte(nil), pixels...))
	return nil
}

func (r *fakeRing) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *fakeRing) snapshot() (frames int, closed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames), r.closed
}

func (r *fakeRing) lastFrame() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frames) == 0 {
		return nil
	}
	return r.frames[len(r.frames)-1]
}

type fakeSink struct {
	mu      sync.Mutex
	pcm     []byte
	flushes int
	closed  bool
	aborted bool
}

func (s *fakeSink) Write(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.aborted {
		return errors.New("playback closed")
	}
	s.pcm = append(s.pcm, pcm...)
	return nil
}

func (s *fakeSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes++
	return nil
}

func (s *fakeSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeSink) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = true
	return nil
}

func (s *fakeSink) firstSample() (int16, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pcm) < 2 {
		return 0, false
	}
	return int16(binary.LittleEndian.Uint16(s.pcm[0:2])), true
}

func (s *fakeSink) state() (pcm int, aborted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pcm), s.aborted
}

type fakeRings struct{ ring *fakeRing }

func (f fakeRings) OpenRing() (Ring, error) { return f.ring, nil }

type fakeSinks struct{ sink *fakeSink }

func (f fakeSinks) OpenSink(int, int) (Sink, error) { return f.sink, nil }

// --- RTSP client helpers --------------------------------------------------

type rtspClient struct {
	conn   Stream
	reader *bufio.Reader
	cseq   int
}

func newRTSPClient(conn Stream) *rtspClient {
	return &rtspClient{conn: conn, reader: bufio.NewReaderSize(conn, 16*1024)}
}

func (c *rtspClient) setStream(conn Stream) {
	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 16*1024)
}

type clientResponse struct {
	status  int
	headers map[string]string
	body    []byte
}

func (c *rtspClient) do(method, uri, contentType string, body []byte) (*clientResponse, error) {
	c.cseq++
	header := fmt.Sprintf("%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: AirPlay/600.1 (Test iPhone; iOS 18)\r\n", method, uri, c.cseq)
	if contentType != "" {
		header += "Content-Type: " + contentType + "\r\n"
	}
	if body != nil {
		header += fmt.Sprintf("Content-Length: %d\r\n", len(body))
	}
	header += "\r\n"
	if _, err := c.conn.Write(append([]byte(header), body...)); err != nil {
		return nil, err
	}
	return c.readResponse()
}

func (c *rtspClient) readResponse() (*clientResponse, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "RTSP/") {
		return nil, fmt.Errorf("unexpected status line %q", strings.TrimSpace(line))
	}
	response := &clientResponse{headers: map[string]string{}}
	if _, err := fmt.Sscanf(fields[1], "%d", &response.status); err != nil {
		return nil, err
	}
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("malformed header %q", line)
		}
		response.headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	if raw := response.headers["content-length"]; raw != "" {
		length := 0
		if _, err := fmt.Sscanf(raw, "%d", &length); err != nil {
			return nil, err
		}
		response.body = make([]byte, length)
		if _, err := io.ReadFull(c.reader, response.body); err != nil {
			return nil, err
		}
	}
	return response, nil
}

// --- pairing client -------------------------------------------------------

func hkdfKey(shared []byte, salt, info string) []byte {
	reader := hkdf.New(sha512.New, shared, []byte(salt), []byte(info))
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		panic(err)
	}
	return key
}

func sealNonce(key []byte, label string, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	copy(nonce[4:], label)
	return aead.Seal(nil, nonce, plaintext, nil), nil
}

func openNonce(key []byte, label string, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	copy(nonce[4:], label)
	return aead.Open(nil, nonce, ciphertext, nil)
}

// pairVerify drives M1..M4 against the receiver and returns the shared secret
// and the decrypted accessory identity.
func pairVerify(t *testing.T, client *rtspClient, pairingID string, clientKey ed25519.PrivateKey) []byte {
	t.Helper()
	curveKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("curve key: %v", err)
	}
	m1 := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM1}},
		tlv8.Item{Tag: tlv8.TagPublicKey, Value: curveKey.PublicKey().Bytes()},
	)
	response, err := client.do("POST", "/pair-verify", "application/pairing+tlv8", m1)
	if err != nil {
		t.Fatalf("pair-verify M1: %v", err)
	}
	items, err := tlv8.Decode(response.body)
	if err != nil {
		t.Fatalf("pair-verify M2: %v", err)
	}
	if got := tlv8.GetString(items, tlv8.TagState); len(got) != 1 || got[0] != tlv8.StateM2 {
		t.Fatalf("pair-verify M2 state = %x", got)
	}
	accessoryPub, _ := tlv8.Get(items, tlv8.TagPublicKey)
	encrypted, _ := tlv8.Get(items, tlv8.TagEncryptedData)
	if len(accessoryPub) != 32 || len(encrypted) == 0 {
		t.Fatalf("pair-verify M2 payload lengths pub=%d enc=%d", len(accessoryPub), len(encrypted))
	}
	peer, err := ecdh.X25519().NewPublicKey(accessoryPub)
	if err != nil {
		t.Fatalf("accessory curve key: %v", err)
	}
	shared, err := curveKey.ECDH(peer)
	if err != nil {
		t.Fatalf("ECDH: %v", err)
	}
	verifyKey := hkdfKey(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info")
	sub, err := openNonce(verifyKey, "PV-Msg02", encrypted)
	if err != nil {
		t.Fatalf("decrypt M2: %v", err)
	}
	subItems, err := tlv8.Decode(sub)
	if err != nil {
		t.Fatalf("M2 sub-TLV: %v", err)
	}
	if _, ok := tlv8.Get(subItems, tlv8.TagSignature); !ok {
		t.Fatalf("M2 sub-TLV has no signature")
	}

	deviceInfo := append(append(append([]byte{}, curveKey.PublicKey().Bytes()...), []byte(pairingID)...), accessoryPub...)
	message := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagIdentifier, Value: []byte(pairingID)},
		tlv8.Item{Tag: tlv8.TagSignature, Value: ed25519.Sign(clientKey, deviceInfo)},
	)
	sealed, err := sealNonce(verifyKey, "PV-Msg03", message)
	if err != nil {
		t.Fatalf("seal M3: %v", err)
	}
	m3 := tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{tlv8.StateM3}},
		tlv8.Item{Tag: tlv8.TagEncryptedData, Value: sealed},
	)
	response, err = client.do("POST", "/pair-verify", "application/pairing+tlv8", m3)
	if err != nil {
		t.Fatalf("pair-verify M3: %v", err)
	}
	items, err = tlv8.Decode(response.body)
	if err != nil {
		t.Fatalf("pair-verify M4: %v", err)
	}
	if got := tlv8.GetString(items, tlv8.TagState); len(got) != 1 || got[0] != tlv8.StateM4 {
		t.Fatalf("pair-verify M4 state = %x", got)
	}
	if _, ok := tlv8.Get(items, tlv8.TagError); ok {
		t.Fatalf("pair-verify M4 reported an error")
	}
	return shared
}

// --- the test -------------------------------------------------------------

func newTestManager(t *testing.T) (*Manager, *fakeTransport, *fakeAdvertiser, *fakeRing, *fakeSink, ed25519.PrivateKey, string) {
	t.Helper()
	transport := newFakeTransport()
	advertiser := &fakeAdvertiser{}
	ring := &fakeRing{}
	sink := &fakeSink{}
	board := media.NewBoard()
	mgr, err := NewManager(
		Config{
			DeviceID:    "aabbccddeeff",
			Name:        "Test Speaker",
			Port:        7000,
			FrameTarget: 20 * time.Millisecond,
			RingEnabled: true,
			Logf:        t.Logf,
		},
		transport,
		advertiser,
		fakeRings{ring: ring},
		fakeSinks{sink: sink},
		board,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	pairingID := "sender-pairing-id"
	mgr.Store().SetControllerLTPK(pairingID, public)
	return mgr, transport, advertiser, ring, sink, private, pairingID
}

// TestRAOPInstanceNameFitsLabel pins the DNS-SD single-label projection: a
// name that is valid for _airplay (63 bytes) leaves only 50 bytes after the
// 12-hex device id and "@" in the _raop instance. The projection must stay a
// valid UTF-8 prefix of the saved name rather than fail the advertisement.
func TestRAOPInstanceNameFitsLabel(t *testing.T) {
	names := []string{
		"short",
		strings.Repeat("a", 63),
		strings.Repeat("ж", 31) + "a",  // 63 bytes, two-byte runes
		strings.Repeat("🎛", 15) + "abc", // 63 bytes, four-byte runes
	}
	for _, name := range names {
		got := raopInstanceName("aabbccddeeff", name)
		if len(got) > maxServiceInstanceBytes {
			t.Fatalf("instance %q is %d bytes, limit %d", got, len(got), maxServiceInstanceBytes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("instance %q is not valid UTF-8", got)
		}
		const prefix = "AABBCCDDEEFF@"
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("instance %q lost the device-id prefix", got)
		}
		display := strings.TrimPrefix(got, prefix)
		if !strings.HasPrefix(name, display) {
			t.Fatalf("projected display %q is not a prefix of %q", display, name)
		}
	}
}

func TestSessionPairingSetupAudioAndTeardown(t *testing.T) {
	mgr, transport, advertiser, ring, sink, clientKey, pairingID := newTestManager(t)
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Close()

	advert, ok := advertiser.find("_airplay._tcp")
	if !ok {
		t.Fatal("no _airplay._tcp advertisement")
	}
	if advert.Port != 7000 {
		t.Fatalf("advertised port = %d, want 7000", advert.Port)
	}
	if advert.TXT["deviceid"] != "aabbccddeeff" || advert.TXT["features"] == "" {
		t.Fatalf("advertisement TXT = %v", advert.TXT)
	}
	raop, ok := advertiser.find("_raop._tcp")
	if !ok {
		t.Fatal("no _raop._tcp advertisement")
	}
	// The field failure: _raop carried the ~360-byte base64 RSA key, which is
	// larger than one DNS TXT string, so Avahi rejected the whole record. It
	// now carries the same 32-byte ed25519 key as _airplay, like the reference.
	if raop.TXT["pk"] != advert.TXT["pk"] || len(raop.TXT["pk"]) != 64 {
		t.Fatalf("_raop pk = %q, want the 64-hex _airplay key %q", raop.TXT["pk"], advert.TXT["pk"])
	}
	if raop.TXT["am"] != DefaultModel || raop.TXT["ft"] == "" || raop.TXT["sf"] == "" {
		t.Fatalf("_raop TXT = %v", raop.TXT)
	}
	for _, record := range []Advert{advert, raop} {
		if len(record.Instance) > maxServiceInstanceBytes {
			t.Fatalf("%s instance is %d bytes, limit %d", record.Type, len(record.Instance), maxServiceInstanceBytes)
		}
		for key, value := range record.TXT {
			if item := len(key) + 1 + len(value); item > 255 {
				t.Fatalf("%s TXT %q is %d bytes, limit 255", record.Type, key, item)
			}
		}
	}

	conn, err := transport.dial(7000)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := newRTSPClient(conn)

	// GET /info: the device description a sender reads before pairing.
	info, err := client.do("GET", "/info", "", nil)
	if err != nil {
		t.Fatalf("GET /info: %v", err)
	}
	if info.status != 200 || !strings.Contains(info.headers["content-type"], "binary-plist") {
		t.Fatalf("GET /info = %d %v", info.status, info.headers)
	}
	decoded, err := plist.Decode(info.body)
	if err != nil {
		t.Fatalf("decode /info: %v", err)
	}
	infoDict, _ := decoded.(map[string]any)
	if infoDict["deviceID"] != "aabbccddeeff" {
		t.Fatalf("/info deviceID = %v", infoDict["deviceID"])
	}

	// Pair-verify, then switch this connection to the encrypted control channel.
	shared := pairVerify(t, client, pairingID, clientKey)
	control := hapcrypt.ControlChannelKeys(shared)
	encrypted, err := hapcrypt.NewSession(conn, control.Inbound, control.Outbound)
	if err != nil {
		t.Fatalf("client channel: %v", err)
	}
	client.setStream(&rwStream{rw: encrypted})

	// A sender volume of -6 dB is a linear gain of about 0.5 on our PCM.
	volume := []byte("volume: -6.0206\r\n")
	response, err := client.do("SET_PARAMETER", "/1", "text/parameters", volume)
	if err != nil || response.status != 200 {
		t.Fatalf("SET_PARAMETER volume = %v %v", response, err)
	}

	// SETUP: one realtime PCM stream with a sender-supplied stream key.
	shk := make([]byte, 32)
	if _, err := rand.Read(shk); err != nil {
		t.Fatalf("shk: %v", err)
	}
	setupBody, err := plist.EncodeBinary(map[string]any{
		"streams": []any{map[string]any{
			"type":        int64(streamTypeRealtime),
			"audioFormat": int64(1 << 11), // PCM 44100 16-bit stereo
			"spf":         int64(352),
			"shk":         shk,
		}},
	})
	if err != nil {
		t.Fatalf("encode SETUP: %v", err)
	}
	setup, err := client.do("SETUP", "/1", "application/x-apple-binary-plist", setupBody)
	if err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	if setup.status != 200 {
		t.Fatalf("SETUP status = %d", setup.status)
	}
	decoded, err = plist.Decode(setup.body)
	if err != nil {
		t.Fatalf("decode SETUP response: %v", err)
	}
	setupDict, _ := decoded.(map[string]any)
	streams, _ := setupDict["streams"].([]any)
	if len(streams) != 1 {
		t.Fatalf("SETUP streams = %v", setupDict["streams"])
	}
	streamDict, _ := streams[0].(map[string]any)
	dataPort := int(plistUint(streamDict["dataPort"]))
	if dataPort == 0 {
		t.Fatalf("SETUP dataPort = %v", streamDict["dataPort"])
	}
	if int(plistUint(streamDict["controlPort"])) == 0 {
		t.Fatalf("SETUP controlPort = %v", streamDict["controlPort"])
	}

	// RECORD starts playout.
	record, err := client.do("RECORD", "/1", "", nil)
	if err != nil || record.status != 200 {
		t.Fatalf("RECORD = %v %v", record, err)
	}
	if record.headers["audio-latency"] == "" {
		t.Fatalf("RECORD has no Audio-Latency header")
	}

	// Audio: 30 RTP packets of 352 stereo frames, encrypted exactly as the
	// sender does it (nonce at the end, RTP header 4..12 as AAD).
	sendAudio, err := transport.sendUDP(dataPort)
	if err != nil {
		t.Fatalf("audio socket: %v", err)
	}
	samples := make([]byte, 352*2*2)
	for i := 0; i < 352*2; i++ {
		binary.LittleEndian.PutUint16(samples[i*2:], 1000)
	}
	aead, err := chacha20poly1305.New(shk)
	if err != nil {
		t.Fatalf("aead: %v", err)
	}
	var timestamp uint32 = 1000
	for seq := 0; seq < 30; seq++ {
		header := make([]byte, 12)
		header[0] = 0x80
		header[1] = byte(streamTypeRealtime)
		binary.BigEndian.PutUint16(header[2:4], uint16(seq))
		binary.BigEndian.PutUint32(header[4:8], timestamp)
		binary.BigEndian.PutUint32(header[8:12], 0x11223344)
		nonce := make([]byte, 12)
		binary.LittleEndian.PutUint64(nonce[4:], uint64(seq)+1)
		ciphertext := aead.Seal(nil, nonce, samples, header[4:12])
		packet := append(append(append([]byte{}, header...), ciphertext...), nonce[4:]...)
		if err := sendAudio(packet); err != nil {
			t.Fatalf("audio datagram: %v", err)
		}
		timestamp += 352
	}

	// Metadata arrives as a DMAP blob; the media contract must reflect it.
	title, artist := "Test Title", "Test Artist"
	metadata := append([]byte("mlit"), 0, 0, 0, 0)
	metadata = append(metadata, dmapItem("minm", title)...)
	metadata = append(metadata, dmapItem("asar", artist)...)
	binary.BigEndian.PutUint32(metadata[4:8], uint32(len(metadata)-8))
	if response, err := client.do("SET_PARAMETER", "/1", "application/x-dmap-tagged", metadata); err != nil || response.status != 200 {
		t.Fatalf("SET_PARAMETER metadata = %v %v", response, err)
	}

	// The sink receives the PCM with the sender's gain applied.
	deadline := time.Now().Add(4 * time.Second)
	var received int
	for time.Now().Before(deadline) {
		received, _ = sink.state()
		if received >= 20*len(samples) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if received < 10*len(samples) {
		t.Fatalf("sink received %d bytes, want at least %d", received, 10*len(samples))
	}
	// 1000 * gain(0.5) = 500, allowing for the float path's rounding.
	first, ok := sink.firstSample()
	if !ok {
		t.Fatal("the sink has no complete sample")
	}
	if first < 495 || first > 505 {
		t.Fatalf("first sample = %d, want about 500 (sender volume applied)", first)
	}

	// The media contract shows the player and its metadata.
	status := mgr.MediaSnapshot()
	players, _ := status["players"].([]any)
	if len(players) != 1 {
		t.Fatalf("players = %v", status["players"])
	}
	player, _ := players[0].(map[string]any)
	if player["playerId"] != "airplay" || player["appName"] != "AirPlay" {
		t.Fatalf("player = %v", player)
	}
	if player["title"] != title || player["artist"] != artist {
		t.Fatalf("player metadata = %v / %v", player["title"], player["artist"])
	}
	if player["playbackState"] != string(media.StatePlaying) {
		t.Fatalf("playbackState = %v", player["playbackState"])
	}

	// The ring is presented and released with the session.
	ringDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(ringDeadline) {
		if frames, _ := ring.snapshot(); frames > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if frames, _ := ring.snapshot(); frames == 0 {
		t.Fatal("the ring never received a frame")
	}
	if len(ring.lastFrame()) != 36 {
		t.Fatalf("ring frame length = %d", len(ring.lastFrame()))
	}

	// TEARDOWN releases the session: player gone, ring closed, audio aborted.
	if response, err := client.do("TEARDOWN", "/1", "", nil); err != nil || response.status != 200 {
		t.Fatalf("TEARDOWN = %v %v", response, err)
	}
	endDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(endDeadline) {
		if _, closed := ring.snapshot(); closed && !mgr.board.Live() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mgr.board.Live() {
		t.Fatal("the player is still live after TEARDOWN")
	}
	if _, closed := ring.snapshot(); !closed {
		t.Fatal("the ring presentation was not released")
	}
	if _, aborted := sink.state(); !aborted {
		t.Fatal("the playback stream was not aborted on teardown")
	}
	if status := mgr.MediaSnapshot(); len(status["players"].([]any)) != 0 {
		// An idle receiver offers no player at all.
		t.Fatalf("unexpected player list %v", status["players"])
	}
}

func dmapItem(tag, value string) []byte {
	item := append([]byte(tag), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(item[4:8], uint32(len(value)))
	return append(item, []byte(value)...)
}

// rwStream adapts an io.ReadWriter to the session Stream seam.
type rwStream struct{ rw io.ReadWriter }

func (s *rwStream) Read(p []byte) (int, error)  { return s.rw.Read(p) }
func (s *rwStream) Write(p []byte) (int, error) { return s.rw.Write(p) }
func (s *rwStream) Close() error                { return nil }
func (s *rwStream) Peer() Peer                  { return Peer{Address: "sender.test", Port: 50000} }
