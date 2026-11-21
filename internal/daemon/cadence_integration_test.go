package daemon

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"capsulad/internal/backend"
	"capsulad/internal/settings"
)

// cadenceIntegrationServer serves both sides a real Cadence Client needs on
// the host: POST /v2/enroll for a dashboard code, and the websocket device
// link. It is a loopback fake, not a Cadence server; it records the enrollment
// fingerprint and the bearer token the app presents.
type cadenceIntegrationServer struct {
	baseURL  string
	token    string
	announce bool
	server   *httptest.Server

	enroll   chan enrollSeen
	auth     chan string
	reportCh chan []byte
	stateCh  chan []byte
}

type enrollSeen struct {
	Code  string
	Board string
}

func startCadenceIntegrationServer(t *testing.T, token string, announce bool) *cadenceIntegrationServer {
	t.Helper()
	s := &cadenceIntegrationServer{
		token: token, announce: announce,
		enroll: make(chan enrollSeen, 8), auth: make(chan string, 8),
		reportCh: make(chan []byte, 8), stateCh: make(chan []byte, 8),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	s.baseURL = s.server.URL
	t.Cleanup(s.server.Close)
	return s
}

func startCadenceTLSIntegrationServer(t *testing.T, token string, announce bool) (*cadenceIntegrationServer, []byte) {
	t.Helper()
	caPEM, leaf := cadenceTestCA(t)
	s := &cadenceIntegrationServer{
		token: token, announce: announce,
		enroll: make(chan enrollSeen, 8), auth: make(chan string, 8),
		reportCh: make(chan []byte, 8), stateCh: make(chan []byte, 8),
	}
	s.server = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	s.server.StartTLS()
	s.baseURL = s.server.URL
	t.Cleanup(s.server.Close)
	return s, caPEM
}

// cadenceTestCA makes a private CA and a server leaf for 127.0.0.1. The Go
// test server's built-in certificate is a CA used as an end entity, which
// rustls rejects; this exercises the same split a real deployment has.
func cadenceTestCA(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cadence test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "cadence-test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf := tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), leaf
}

func writeCadenceCAAsset(t *testing.T, d *Daemon, ca []byte) string {
	t.Helper()
	dir := filepath.Join(d.assets.Root(), "cadence")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	return "cadence/ca.pem"
}

func (s *cadenceIntegrationServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && r.URL.Path == "/v2/enroll" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		var req struct {
			Code        string `json:"code"`
			Fingerprint struct {
				Board string `json:"board"`
			} `json:"fingerprint"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			select {
			case s.enroll <- enrollSeen{Code: req.Code, Board: req.Fingerprint.Board}:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"deviceId":"dev-enrolled","token":%q}`, s.token)
		return
	}
	if r.URL.Path != "/v2/device" {
		http.NotFound(w, r)
		return
	}
	select {
	case s.auth <- r.Header.Get("Authorization"):
	default:
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	key := r.Header.Get("Sec-WebSocket-Key")
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err := rw.Flush(); err != nil {
		return
	}
	_, _ = conn.Write(fakeCadenceWelcome)
	go s.sendAnnounce(conn)
	s.readClientFrames(rw.Reader)
}

// sendAnnounce is the short server stream used by the announcement test. The
// client is already past Welcome and is sending its capability report.
func (s *cadenceIntegrationServer) sendAnnounce(conn net.Conn) {
	if !s.announce {
		return
	}
	time.Sleep(250 * time.Millisecond)
	_, _ = conn.Write(wsBinary(cadenceAnnounceStart("an-1")))
	time.Sleep(50 * time.Millisecond)
	_, _ = conn.Write(wsBinary(cadenceAnnounceAudio("an-1", 0, cadenceAnnounceOpus)))
	time.Sleep(250 * time.Millisecond)
	_, _ = conn.Write(wsBinary(cadenceAnnounceEnd("an-1")))
}

// readClientFrames records the capability report and audio.output state so the
// host test can prove what the real app advertised, not just that it linked.
func (s *cadenceIntegrationServer) readClientFrames(br *bufio.Reader) {
	for {
		op, payload, err := readClientFrame(br)
		if err != nil {
			return
		}
		if op != 0x2 {
			continue
		}
		field, body := envelopeField(payload)
		switch field {
		case 3:
			s.reportCh <- body
		case 6:
			s.stateCh <- body
		}
	}
}

func readClientFrame(br *bufio.Reader) (byte, []byte, error) {
	first, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	second, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	op := first & 0x0f
	length := uint64(second & 0x7f)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(b[:])
	}
	var mask [4]byte
	if second&0x80 != 0 {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if second&0x80 != 0 {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func envelopeField(payload []byte) (int, []byte) {
	tag, n := binary.Uvarint(payload)
	if n <= 0 {
		return 0, nil
	}
	field := int(tag >> 3)
	if tag&7 != 2 {
		return field, nil
	}
	length, m := binary.Uvarint(payload[n:])
	if m <= 0 {
		return field, nil
	}
	start := n + m
	end := start + int(length)
	if end > len(payload) {
		return field, nil
	}
	return field, payload[start:end]
}

// cadenceAnnounceOpus is one valid 20 ms Opus packet (16 kHz mono) produced by
// the app's own encoder; only its playback path is under test here.
var cadenceAnnounceOpus = mustBase64("SIJbA2o5MpAAAAo/sHa2OVobaVGEt8ILfTX134yEhUMBTjMksbTxgOeE5E8GXsMqkPuww+CjPJY3RSLHGqxpGJX08aJiABbV1E6SyiSWDKJ+tCEbbvMy")

func mustBase64(value string) []byte {
	out, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return out
}

func wsBinary(payload []byte) []byte {
	switch {
	case len(payload) < 126:
		return append([]byte{0x82, byte(len(payload))}, payload...)
	case len(payload) <= 0xffff:
		return append([]byte{0x82, 126, byte(len(payload) >> 8), byte(len(payload))}, payload...)
	default:
		out := []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(out[2:], uint64(len(payload)))
		return append(out, payload...)
	}
}

func pbTag(field int, wire int) byte { return byte(field<<3 | wire) }

func pbVarint(field int, value uint64) []byte {
	out := []byte{pbTag(field, 0)}
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func pbBytes(field int, value []byte) []byte {
	out := []byte{pbTag(field, 2)}
	n := uint64(len(value))
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	out = append(out, byte(n))
	return append(out, value...)
}

func cadenceEnvelope(field int, message []byte) []byte { return pbBytes(field, message) }

func cadenceAnnounceStart(id string) []byte {
	start := pbBytes(1, []byte(id))
	start = append(start, pbVarint(3, 2)...) // StreamPurpose.ANNOUNCE
	start = append(start, pbVarint(4, 1)...) // Codec.OPUS
	start = append(start, pbVarint(5, 1)...) // channels
	start = append(start, pbVarint(6, 1)...) // duck
	return cadenceEnvelope(12, start)        // Envelope.stream_start
}

func cadenceAnnounceAudio(id string, seq uint64, opus []byte) []byte {
	audio := pbBytes(1, []byte(id))
	audio = append(audio, pbVarint(2, seq)...)
	audio = append(audio, pbBytes(3, opus)...)
	return cadenceEnvelope(13, audio) // Envelope.audio_down
}

func cadenceAnnounceEnd(id string) []byte {
	return cadenceEnvelope(14, pbBytes(1, []byte(id))) // Envelope.stream_end
}

func cadenceApp(t *testing.T) *Daemon {
	t.Helper()
	binary := buildCadenceApp(t)
	root := moduleRoot(t)
	manifest, err := os.ReadFile(filepath.Join(root, "apps", "cadence", "manifest.toml"))
	if err != nil {
		t.Fatal(err)
	}
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: cadenceAppID, binaryName: "cadence", binaryPath: binary, manifest: manifest,
	})
	return d
}

func selectCadence(t *testing.T, d *Daemon) {
	t.Helper()
	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = cadenceAppID
	}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, d, cadenceAppID, 20*time.Second)
}

func waitCadenceLink(t *testing.T, d *Daemon) {
	t.Helper()
	waitFor(t, 10*time.Second, "the app's link to the fake server", func() bool {
		return strings.Contains(logText(d), "link_up")
	})
}

func receiveEnrollment(t *testing.T, s *cadenceIntegrationServer) enrollSeen {
	t.Helper()
	select {
	case seen := <-s.enroll:
		return seen
	case <-time.After(10 * time.Second):
		t.Fatal("the app did not POST /v2/enroll")
		return enrollSeen{}
	}
}

func receiveAuth(t *testing.T, s *cadenceIntegrationServer) string {
	t.Helper()
	select {
	case auth := <-s.auth:
		return auth
	case <-time.After(10 * time.Second):
		t.Fatal("the app did not open the device link")
		return ""
	}
}

func assertAudioOutputAdvertisement(t *testing.T, s *cadenceIntegrationServer) {
	t.Helper()
	select {
	case report := <-s.reportCh:
		if !bytes.Contains(report, []byte("audio.output")) {
			t.Fatalf("capability report has no audio.output: %q", report)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the app sent no capability report")
	}
	select {
	case state := <-s.stateCh:
		if !bytes.Contains(state, []byte("audio.output")) ||
			!bytes.Contains(state, []byte(`"channels":2`)) ||
			!bytes.Contains(state, []byte(`"maxBitrate":128000`)) ||
			bytes.Contains(state, []byte(`"level"`)) {
			t.Fatalf("audio.output state is not the expected advertisement: %q", state)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the app sent no audio.output state")
	}
}

func drainHandshakeFrames(s *cadenceIntegrationServer) {
	for {
		select {
		case <-s.reportCh:
			continue
		default:
		}
		break
	}
	for {
		select {
		case <-s.stateCh:
			continue
		default:
		}
		break
	}
}

func cadenceAppValue(t *testing.T, d *Daemon, key string) (string, bool) {
	t.Helper()
	snap, err := d.appCfg.Snapshot(cadenceAppID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range snap.Values {
		if v.Key == key {
			return v.Value, v.HasValue
		}
	}
	t.Fatalf("setting %s is not declared", key)
	return "", false
}

// TestCadenceEnrollmentFromDashboardCode is the integrated enrollment proof:
// the real app spends a dashboard code at a loopback /v2/enroll, persists the
// returned token through the real application-configuration service, clears
// the spent code, and links with that token. No device and no remote server
// are involved.
func TestCadenceEnrollmentFromDashboardCode(t *testing.T) {
	server := startCadenceIntegrationServer(t, "tok-enrolled", false)
	d := cadenceApp(t)
	defer func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", logText(d))
		}
	}()

	_, rev, private, err := d.appCfg.Update(cadenceAppID, 0, []settings.Change{
		{Key: "server_url", Value: server.baseURL},
		{Key: "enroll_code", Value: "K7M4QX"},
	})
	if err != nil {
		t.Fatalf("app settings: %v", err)
	}
	d.pushAppSettings(cadenceAppID, rev, private)
	selectCadence(t, d)

	seen := receiveEnrollment(t, server)
	if seen.Code != "K7M4QX" || seen.Board != "sim" {
		t.Fatalf("enrollment = %+v, want code K7M4QX and board sim", seen)
	}
	if auth := receiveAuth(t, server); auth != "Bearer tok-enrolled" {
		t.Fatalf("device link authorization = %q, want the enrolled token", auth)
	}
	waitFor(t, 10*time.Second, "stored token and cleared code", func() bool {
		_, tokenSet := cadenceAppValue(t, d, "device_token")
		_, codeSet := cadenceAppValue(t, d, "enroll_code")
		return tokenSet && !codeSet
	})
	waitCadenceLink(t, d)
	assertAudioOutputAdvertisement(t, server)
	waitFor(t, 10*time.Second, "enrollment success logs", func() bool {
		logs := logText(d)
		return strings.Contains(logs, "enroll_ok board=sim") &&
			strings.Contains(logs, "enroll_stored")
	})
}

// TestCadenceAnnouncementReachesSimulatorPlayback is the integrated
// announcement proof: the real app receives a server announce stream outside
// any session, decodes it, and plays it on the simulator's speaker sink. It
// opens no microphone and takes no ring capture; the announcement ends and its
// playback is released.
func TestCadenceAnnouncementReachesSimulatorPlayback(t *testing.T) {
	server := startCadenceIntegrationServer(t, "sim-token", true)
	d := cadenceApp(t)
	defer func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", logText(d))
		}
	}()

	_, rev, private, err := d.appCfg.Update(cadenceAppID, 0, []settings.Change{
		{Key: "server_url", Value: server.baseURL},
		{Key: "device_token", Value: "sim-token"},
	})
	if err != nil {
		t.Fatalf("app settings: %v", err)
	}
	d.pushAppSettings(cadenceAppID, rev, private)
	selectCadence(t, d)
	waitCadenceLink(t, d)

	sim, ok := d.dev.(*backend.Simulator)
	if !ok {
		t.Fatal("the simulator backend is not active")
	}
	sink, ok := sim.Audio().(interface{ LastOutput() []byte })
	if !ok {
		t.Fatal("the simulator audio sink does not expose LastOutput")
	}
	before := len(sink.LastOutput())

	waitFor(t, 10*time.Second, "announcement start", func() bool {
		return strings.Contains(logText(d), "announce_start stream=an-1")
	})
	if !playbackOpen(d, cadenceAppID) {
		t.Fatal("the announcement did not open speaker playback")
	}
	if inputOpen(d, cadenceAppID) {
		t.Fatal("the announcement opened a microphone")
	}
	if got := d.router.Current("light.ring.main").SelectedAppID; got == cadenceAppID {
		t.Fatal("the announcement took the ring capture")
	}
	waitFor(t, 10*time.Second, "announcement playback in the simulator sink", func() bool {
		out := sink.LastOutput()
		return len(out) > before && outputPeak(out) > 0
	})
	waitFor(t, 10*time.Second, "announcement end", func() bool {
		return strings.Contains(logText(d), "announce_end stream=an-1 code=ok")
	})
	waitFor(t, 10*time.Second, "announcement playback released", func() bool {
		return !playbackOpen(d, cadenceAppID)
	})
	logs := logText(d)
	if strings.Contains(logs, "announce_refused") {
		t.Fatalf("the announcement was refused:\n%s", logs)
	}
}

// A CA asset with a CERTIFICATE label but unusable bytes is rejected by the
// SDK's trust-root parser before the working configuration is replaced. The
// link stays up on its previous CA.
func TestCadenceInvalidCAKeepsTheWorkingLink(t *testing.T) {
	server := startCadenceIntegrationServer(t, "sim-token", false)
	d := cadenceApp(t)
	defer func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", logText(d))
		}
	}()

	_, rev, private, err := d.appCfg.Update(cadenceAppID, 0, []settings.Change{
		{Key: "server_url", Value: server.baseURL},
		{Key: "device_token", Value: "sim-token"},
	})
	if err != nil {
		t.Fatalf("app settings: %v", err)
	}
	d.pushAppSettings(cadenceAppID, rev, private)
	selectCadence(t, d)
	waitCadenceLink(t, d)

	_, rev, private, err = d.appCfg.Update(cadenceAppID, 0, []settings.Change{
		{Key: "ca_pem", Value: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"},
	})
	if err != nil {
		t.Fatalf("CA settings: %v", err)
	}
	d.pushAppSettings(cadenceAppID, rev, private)
	waitFor(t, 10*time.Second, "invalid CA rejection", func() bool {
		return strings.Contains(logText(d), "ca_pem is not a usable trust root")
	})
	time.Sleep(200 * time.Millisecond)
	logs := logText(d)
	if strings.Count(logs, "link_up") == 0 {
		t.Fatalf("the working link was never up:\n%s", logs)
	}
	if strings.Contains(logs, "link_down") {
		t.Fatalf("a rejected CA dropped the working link:\n%s", logs)
	}
}

// TestCadenceTLSEnrollmentAndRestart is the private-CA host proof: HTTPS
// enrollment and the WSS device link use a CA loaded from the shared asset
// store, and a restart reuses the persisted token instead of redeeming a
// second code. No live credential or device is involved.
func TestCadenceTLSEnrollmentAndRestart(t *testing.T) {
	server, ca := startCadenceTLSIntegrationServer(t, "tok-tls", false)
	d := cadenceApp(t)
	defer func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", logText(d))
		}
	}()
	caAsset := writeCadenceCAAsset(t, d, ca)

	_, rev, private, err := d.appCfg.Update(cadenceAppID, 0, []settings.Change{
		{Key: "server_url", Value: server.baseURL},
		{Key: "ca_pem", Value: caAsset},
		{Key: "enroll_code", Value: "K7M4QX"},
	})
	if err != nil {
		t.Fatalf("app settings: %v", err)
	}
	d.pushAppSettings(cadenceAppID, rev, private)
	if err := d.writeUserSettings([]string{"default_assistant_app_id"}, func(v *settings.UserSettings) {
		v.DefaultAssistantAppID = cadenceAppID
	}); err != nil {
		t.Fatal(err)
	}
	firstRun := waitReady(t, d, cadenceAppID, 20*time.Second)

	if seen := receiveEnrollment(t, server); seen.Code != "K7M4QX" || seen.Board != "sim" {
		t.Fatalf("enrollment = %+v, want code K7M4QX and board sim", seen)
	}
	if auth := receiveAuth(t, server); auth != "Bearer tok-tls" {
		t.Fatalf("first WSS authorization = %q", auth)
	}
	waitFor(t, 10*time.Second, "stored token and cleared code", func() bool {
		_, tokenSet := cadenceAppValue(t, d, "device_token")
		_, codeSet := cadenceAppValue(t, d, "enroll_code")
		return tokenSet && !codeSet
	})
	waitCadenceLink(t, d)
	assertAudioOutputAdvertisement(t, server)
	drainHandshakeFrames(server)
	// Discard any echo reconnect from the commit's settings event; the proof
	// below needs the link opened by the restarted process.
	for {
		select {
		case <-server.auth:
			continue
		default:
		}
		break
	}
	linksBeforeRestart := strings.Count(logText(d), "link_up")

	// A selected assistant is recovered by the supervisor. The new process
	// must read the stored token and connect without a code exchange.
	if !d.Stop(string(firstRun)) {
		t.Fatal("stop the first Cadence run")
	}
	waitFor(t, 20*time.Second, "restarted Cadence run", func() bool {
		run, ok := d.sup.ReadyRun(cadenceAppID)
		return ok && run.RunID != string(firstRun)
	})
	if auth := receiveAuth(t, server); auth != "Bearer tok-tls" {
		t.Fatalf("restart WSS authorization = %q", auth)
	}
	waitFor(t, 10*time.Second, "second link", func() bool {
		return strings.Count(logText(d), "link_up") > linksBeforeRestart
	})
	assertAudioOutputAdvertisement(t, server)
	select {
	case extra := <-server.enroll:
		t.Fatalf("restart redeemed another code: %+v", extra)
	case <-time.After(500 * time.Millisecond):
	}
}
