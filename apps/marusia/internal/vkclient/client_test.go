package vkclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"capsulad/apps/marusia/internal/vkproto/clientpb"
	"capsulad/apps/marusia/internal/vkproto/clientprobepb"
	commandspb "capsulad/apps/marusia/internal/vkproto/commandspb"
	"capsulad/apps/marusia/internal/vkproto/optionspb"
	"capsulad/apps/marusia/internal/vkproto/phrasepb"
	"capsulad/apps/marusia/internal/vkproto/servicepb"
)

const (
	testSerial  = "SN-TEST-0001"
	testDevice  = "11111111-2222-4333-8444-555555555555"
	testSecret  = "secret-1"
	testSession = "session-1"
)

// --- in-process TLS plumbing ------------------------------------------------

// testCertificate issues a self-signed server certificate valid for 127.0.0.1
// and returns a pool that trusts it. All server connections are loopback.
func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "marusia-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "marusia-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}, pool
}

// startGRPC serves reg on a loopback TLS listener and returns its address.
func startGRPC(t *testing.T, cert tls.Certificate, reg func(*grpc.Server)) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})))
	reg(server)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return lis.Addr().String()
}

// rawMessage and rawCodec keep application protobuf bytes opaque on the wire,
// exactly like the native engine's RawCodec.
type rawMessage []byte

type rawCodec struct{}

func (rawCodec) Marshal(value any) ([]byte, error) {
	message, ok := value.(rawMessage)
	if !ok {
		return nil, fmt.Errorf("rawCodec: unexpected %T", value)
	}
	return message, nil
}

func (rawCodec) Unmarshal(data []byte, value any) error {
	message, ok := value.(*rawMessage)
	if !ok {
		return fmt.Errorf("rawCodec: unexpected %T", value)
	}
	*message = append((*message)[:0], data...)
	return nil
}

func (rawCodec) Name() string { return "proto" }

// testTransport is the in-process adapter behind the production seam. It uses
// grpc-go and net/http, which is why it lives in _test.go: the application
// binary does not link either.
type testTransport struct {
	http *http.Client
	conn *grpc.ClientConn
}

func dialTest(t *testing.T, addr string, pool *x509.CertPool) *grpc.ClientConn {
	t.Helper()
	var dialer net.Dialer
	conn, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs:    pool,
			ServerName: "127.0.0.1",
			MinVersion: tls.VersionTLS12,
		})),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		}))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func newTestTransport(t *testing.T, addr string, pool *x509.CertPool, httpClient *http.Client) *testTransport {
	t.Helper()
	transport := &testTransport{http: httpClient}
	if addr != "" {
		transport.conn = dialTest(t, addr, pool)
	}
	return transport
}

func (t *testTransport) Do(ctx context.Context, req HTTPRequest, timeout time.Duration) (*HTTPResponse, error) {
	if t.http == nil {
		return nil, errors.New("testTransport: no http client")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	for name, value := range req.Header {
		request.Header.Set(name, value)
	}
	response, err := t.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(response.Header))
	for name, values := range response.Header {
		if len(values) > 0 {
			headers[strings.ToLower(name)] = values[len(values)-1]
		}
	}
	return &HTTPResponse{StatusCode: response.StatusCode, Header: headers, Body: body}, nil
}

func (t *testTransport) Unary(ctx context.Context, service, method string,
	headers map[string]string, request []byte, timeout time.Duration) ([]byte, error) {
	if t.conn == nil {
		return nil, errors.New("testTransport: no grpc connection")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ctx = metadata.NewOutgoingContext(ctx, metadata.New(headers))
	var response rawMessage
	var md metadata.MD
	err := t.conn.Invoke(ctx, "/"+service+"/"+method, rawMessage(request), &response,
		grpc.ForceCodec(rawCodec{}), grpc.Header(&md))
	if err != nil {
		return nil, testStatusError(method, err)
	}
	return response, nil
}

func (t *testTransport) NewStream(ctx context.Context, service, method string,
	headers map[string]string, kind CallKind, timeout time.Duration) (Stream, error) {
	if t.conn == nil {
		return nil, errors.New("testTransport: no grpc connection")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	streamCtx, cancel := context.WithCancel(ctx)
	streamCtx = metadata.NewOutgoingContext(streamCtx, metadata.New(headers))
	stream, err := t.conn.NewStream(streamCtx, &grpc.StreamDesc{
		ServerStreams: true,
		ClientStreams: true,
	}, "/"+service+"/"+method, grpc.ForceCodec(rawCodec{}))
	if err != nil {
		cancel()
		return nil, testStatusError(method, err)
	}
	return &testStream{stream: stream, cancel: cancel, method: method}, nil
}

func (t *testTransport) Close() error {
	if t.conn != nil {
		_ = t.conn.Close()
	}
	return nil
}

type testStream struct {
	stream grpc.ClientStream
	cancel context.CancelFunc
	method string
}

func (s *testStream) Send(payload []byte) error {
	return testStatusError(s.method+" send", s.stream.SendMsg(rawMessage(payload)))
}

func (s *testStream) Recv() ([]byte, error) {
	var message rawMessage
	if err := s.stream.RecvMsg(&message); err != nil {
		return nil, testStatusError(s.method+" recv", err)
	}
	return message, nil
}

func (s *testStream) CloseSend() error {
	return testStatusError(s.method+" close send", s.stream.CloseSend())
}

func (s *testStream) Close() error {
	s.cancel()
	return nil
}

func testStatusError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if st, ok := status.FromError(err); ok {
		return &StatusError{Op: op, Code: Code(st.Code()), Message: st.Message()}
	}
	return err
}

type attachRecord struct {
	method      string
	path        string
	accept      string
	contentType string
	body        string
	authorized  string
}

type fakeAttach struct {
	mu       sync.Mutex
	calls    int
	records  []attachRecord
	status   int
	response string
}

func (f *fakeAttach) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	f.calls++
	f.records = append(f.records, attachRecord{
		method:      r.Method,
		path:        r.URL.Path,
		accept:      r.Header.Get("Accept"),
		contentType: r.Header.Get("Content-Type"),
		body:        string(body),
		authorized:  r.Header.Get("Authorization"),
	})
	status, response := f.status, f.response
	f.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
	}
	_, _ = w.Write([]byte(response))
}

func (f *fakeAttach) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func attachOK() string {
	return `{"result":{"account_id":"account-1","session_id":"` + testSession +
		`","session_secret":"` + testSecret + `"}}`
}

// newTestClient builds a client whose HTTP attach goes to ts and whose gRPC
// channel is redirected to grpcAddr, so one base URL keeps the production
// TLS/target derivation while both servers stay in process.
func newTestClient(t *testing.T, base string, pool *x509.CertPool, httpClient *http.Client, grpcAddr, xtoken string) *Client {
	t.Helper()
	client, err := New(Config{
		BaseURL:   base,
		XTokenID:  xtoken,
		Serial:    testSerial,
		DeviceID:  testDevice,
		Transport: newTestTransport(t, grpcAddr, pool, httpClient),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// --- fakes ------------------------------------------------------------------

type fakeProbe struct {
	servicepb.UnimplementedClientProbeServer

	mu       sync.Mutex
	calls    int
	metadata []metadata.MD
	// fail codes returned per call, in order; missing entries succeed.
	fail []codes.Code
	// desc overrides the status message; empty means "not ready".
	desc string
}

func (p *fakeProbe) AuthReady(ctx context.Context, _ *clientprobepb.AuthReadyRequest) (*clientprobepb.AuthReadyResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	p.mu.Lock()
	p.calls++
	p.metadata = append(p.metadata, md)
	index := p.calls - 1
	var code codes.Code
	if index < len(p.fail) {
		code = p.fail[index]
	}
	desc := p.desc
	p.mu.Unlock()
	if code != codes.OK {
		if desc == "" {
			desc = "not ready"
		}
		return nil, status.Error(code, desc)
	}
	return &clientprobepb.AuthReadyResponse{}, nil
}

func (p *fakeProbe) snapshot() (int, []metadata.MD) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, append([]metadata.MD(nil), p.metadata...)
}

type fakeSpeech struct {
	servicepb.UnimplementedPhraseServer

	mu       sync.Mutex
	requests []*phrasepb.SpeechRequest
	metadata metadata.MD
	aborts   chan string
	// failCode, when non-OK, ends the stream right after init.
	failCode codes.Code
	// waitAbort makes the server hold the stream open until abort or cancel.
	waitAbort bool
}

func (f *fakeSpeech) Speech(stream grpc.BidiStreamingServer[phrasepb.SpeechRequest, phrasepb.SpeechResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.metadata = md
	f.mu.Unlock()

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	f.record(first)
	if first.GetInit() == nil {
		return status.Error(codes.InvalidArgument, "first frame must be init")
	}
	if f.failCode != codes.OK {
		return status.Error(f.failCode, "stream rejected")
	}
	if err := stream.Send(&phrasepb.SpeechResponse{Kind: &phrasepb.SpeechResponse_Init{
		Init: &phrasepb.SpeechResponseInit{
			PhraseId: "phrase-1",
			Qos:      &phrasepb.SpeechResponseQoSOptions{ChunkLengthMsHint: 20, StatusIntervalMs: 1000},
		},
	}}); err != nil {
		return err
	}

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		f.record(req)
		switch kind := req.GetKind().(type) {
		case *phrasepb.SpeechRequest_Eos:
			return f.reply(stream)
		case *phrasepb.SpeechRequest_Abort:
			select {
			case f.aborts <- kind.Abort.GetReason():
			default:
			}
			return nil
		}
	}
}

func (f *fakeSpeech) reply(stream grpc.BidiStreamingServer[phrasepb.SpeechRequest, phrasepb.SpeechResponse]) error {
	playURL, err := anypb.New(&commandspb.CommandPlayUrl{Url: "https://tts.example/reply.wav"})
	if err != nil {
		return err
	}
	showText, err := anypb.New(&commandspb.CommandShowText{Text: "hello"})
	if err != nil {
		return err
	}
	frames := []*phrasepb.SpeechResponse{
		{Kind: &phrasepb.SpeechResponse_Status{Status: &phrasepb.SpeechResponseStatus{
			ChunksReceived: 2, AudioReceivedMs: 40, StatusNum: 1,
		}}},
		{Kind: &phrasepb.SpeechResponse_TextSoFar{TextSoFar: &phrasepb.SpeechResponseTextSoFar{Text: "привет"}}},
		{Kind: &phrasepb.SpeechResponse_Commands{Commands: &phrasepb.SpeechResponseCommands{
			Bundles: []*commandspb.CommandsBundle{
				{Commands: []*anypb.Any{playURL, showText}},
			},
		}}},
		{Kind: &phrasepb.SpeechResponse_Finish{Finish: &phrasepb.SpeechResponseFinish{
			Continuation: &phrasepb.SpeechResponseFinish_Result{Result: &phrasepb.SpeechResponseFinish_ContinuationResult{}},
		}}},
		{Kind: &phrasepb.SpeechResponse_Eos{Eos: &phrasepb.SpeechResponseEos{}}},
	}
	for _, frame := range frames {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSpeech) record(req *phrasepb.SpeechRequest) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
}

func (f *fakeSpeech) snapshot() ([]*phrasepb.SpeechRequest, metadata.MD) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*phrasepb.SpeechRequest(nil), f.requests...), f.metadata
}

// --- attach -----------------------------------------------------------------

func TestAttachRequestShape(t *testing.T) {
	fake := &fakeAttach{response: attachOK()}
	ts := httptest.NewTLSServer(fake)
	defer ts.Close()

	client, err := New(Config{
		BaseURL:   ts.URL,
		XTokenID:  "xtoken-abc",
		Serial:    testSerial,
		DeviceID:  testDevice,
		Transport: newTestTransport(t, "", nil, ts.Client()),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer client.Close()

	session, err := client.Attach(context.Background())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if session != (Session{SessionID: testSession, SessionSecret: testSecret, AccountID: "account-1"}) {
		t.Fatalf("session = %+v", session)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.records) != 1 {
		t.Fatalf("attach calls = %d", len(fake.records))
	}
	rec := fake.records[0]
	if rec.method != http.MethodPost {
		t.Errorf("method = %q", rec.method)
	}
	if rec.path != "/account/attach_xtoken" {
		t.Errorf("path = %q", rec.path)
	}
	if rec.accept != "application/json" {
		t.Errorf("Accept = %q", rec.accept)
	}
	if rec.contentType != "application/json" {
		t.Errorf("Content-Type = %q", rec.contentType)
	}
	if rec.authorized != "" {
		t.Errorf("attach must not be authorized, got %q", rec.authorized)
	}
	var body struct {
		Serial     string `json:"serial"`
		WithSecret string `json:"with_secret"`
		XTokenID   string `json:"xtoken_id"`
	}
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("body %q: %v", rec.body, err)
	}
	if body.Serial != testSerial || body.WithSecret != "1" || body.XTokenID != "xtoken-abc" {
		t.Errorf("body = %s", rec.body)
	}
	if strings.Count(rec.body, ":") != 3 {
		t.Errorf("body must contain exactly the three recovered fields: %s", rec.body)
	}
}

func TestAttachFailurePaths(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		fake := &fakeAttach{status: http.StatusForbidden, response: `{"error":"nope"}`}
		ts := httptest.NewTLSServer(fake)
		defer ts.Close()
		client, err := New(Config{BaseURL: ts.URL, XTokenID: "x", Transport: newTestTransport(t, "", nil, ts.Client())})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		defer client.Close()
		_, err = client.Attach(context.Background())
		var attachErr *AttachError
		if !asError(err, &attachErr) || attachErr.Status != http.StatusForbidden {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("empty xtoken still attempts the attach", func(t *testing.T) {
		fake := &fakeAttach{response: attachOK()}
		ts := httptest.NewTLSServer(fake)
		defer ts.Close()
		client, err := New(Config{BaseURL: ts.URL, Serial: testSerial, Transport: newTestTransport(t, "", nil, ts.Client())})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		defer client.Close()
		session, err := client.Attach(context.Background())
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
		if session.SessionSecret != testSecret {
			t.Fatalf("session = %+v", session)
		}
		if fake.callCount() != 1 {
			t.Fatalf("attach calls = %d, want 1", fake.callCount())
		}
		fake.mu.Lock()
		body := fake.records[0].body
		fake.mu.Unlock()
		var decoded struct {
			Serial   string `json:"serial"`
			XTokenID string `json:"xtoken_id"`
		}
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if decoded.Serial != testSerial {
			t.Fatalf("serial = %q", decoded.Serial)
		}
		if decoded.XTokenID != "" {
			t.Fatalf("xtoken_id = %q, want the empty configured value", decoded.XTokenID)
		}
	})
}

// --- metadata and the speech stream ----------------------------------------

func TestAuthorizeMetadataAndSpeechTurn(t *testing.T) {
	cert, pool := testCertificate(t)
	attach := &fakeAttach{response: attachOK()}
	ts := httptest.NewTLSServer(attach)
	defer ts.Close()

	probe := &fakeProbe{}
	speech := &fakeSpeech{aborts: make(chan string, 1)}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterClientProbeServer(server, probe)
		servicepb.RegisterPhraseServer(server, speech)
	})
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "xtoken-abc")

	if err := client.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if attach.callCount() != 1 {
		t.Fatalf("attach calls = %d, want 1", attach.callCount())
	}

	probeCalls, probeMDs := probe.snapshot()
	if probeCalls != 1 {
		t.Fatalf("probe calls = %d", probeCalls)
	}
	assertCallMetadata(t, probeMDs[0], false)

	session := client.Session()
	if session.SessionSecret != testSecret || session.SessionID != testSession {
		t.Fatalf("session = %+v", session)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.OpenSpeech(ctx, SpeechOptions{
		Format:           FormatWave,
		Timezone:         "Europe/Moscow",
		Keyword:          "marusya",
		KeywordInAudio:   true,
		HWID:             "9F25725B",
		StatusIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("open speech: %v", err)
	}
	defer stream.Close()

	pcm := bytes.Repeat([]byte{0x01, 0x02}, 2*FrameSamples) // exactly two frames
	if err := stream.SendAudio(pcm); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	if got := stream.ChunksSent(); got != 2 {
		t.Fatalf("chunks sent = %d, want 2", got)
	}
	if err := stream.EOS(); err != nil {
		t.Fatalf("eos: %v", err)
	}

	var events []*Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		events = append(events, event)
	}

	kinds := make([]EventKind, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	want := []EventKind{EventInit, EventStatus, EventTextSoFar, EventCommands, EventFinish, EventEOS}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if events[0].PhraseID != "phrase-1" || events[0].ChunkLengthHintMS != 20 {
		t.Errorf("init event = %+v", events[0])
	}
	if events[1].Status == nil || events[1].Status.ChunksReceived != 2 {
		t.Errorf("status event = %+v", events[1])
	}
	if events[2].Text != "привет" {
		t.Errorf("text_so_far = %q", events[2].Text)
	}
	commands := events[3].Commands
	if len(commands) != 2 {
		t.Fatalf("commands = %+v", commands)
	}
	if commands[0].Kind != CommandPlayURL || commands[0].URL != "https://tts.example/reply.wav" {
		t.Errorf("play_url = %+v", commands[0])
	}
	if commands[1].Kind != CommandShowText || commands[1].Text != "hello" {
		t.Errorf("show_text = %+v", commands[1])
	}
	if events[4].Finish != FinishResult {
		t.Errorf("finish = %v", events[4].Finish)
	}

	requests, streamMD := speech.snapshot()
	assertCallMetadata(t, streamMD, true)
	if len(requests) != 4 { // init, chunk 0, chunk 1, eos
		t.Fatalf("requests = %d", len(requests))
	}
	init := requests[0].GetInit()
	if init == nil {
		t.Fatalf("first request is not init: %v", requests[0])
	}
	if init.GetAudioOptions().GetFormat() != optionspb.RequestAudioOptions_AUDIO_FORMAT_WAVE {
		t.Errorf("audio format = %v", init.GetAudioOptions().GetFormat())
	}
	if init.GetMode() != phrasepb.SpeechRequestInit_SPEECH_MODE_GENERAL {
		t.Errorf("mode = %v", init.GetMode())
	}
	device := init.GetClientOptions().GetDeviceInfo()
	if device.GetSerial() != testSerial || device.GetVersion() != "44.91" {
		t.Errorf("device_info = %+v", device)
	}
	capsulad := init.GetClientOptions().GetState().GetState().GetCapsulaState()
	if capsulad == nil || !capsulad.GetMicOn() {
		t.Errorf("capsulad state = %+v", capsulad)
	}
	params := init.GetKeywordOptions().GetKwParams()
	if params.GetKeyword() != "marusya" || params.GetHwId() != "9F25725B" {
		t.Errorf("kw_params = %+v", params)
	}
	if params.GetMediaState() != clientpb.MediaState_MEDIA_STATE_STOP {
		t.Errorf("kw_params media_state = %v; the service refuses an unspecified state",
			params.GetMediaState())
	}

	chunk0 := requests[1].GetChunk()
	chunk1 := requests[2].GetChunk()
	if chunk0.GetChunkNum() != 0 || chunk1.GetChunkNum() != 1 {
		t.Fatalf("chunk numbers = %d, %d", chunk0.GetChunkNum(), chunk1.GetChunkNum())
	}
	for i, chunk := range []*phrasepb.SpeechRequestChunk{chunk0, chunk1} {
		data := chunk.GetData()
		if len(data) != 44+FrameBytes {
			t.Fatalf("chunk %d length = %d, want %d", i, len(data), 44+FrameBytes)
		}
		if !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WAVE")) {
			t.Fatalf("chunk %d does not start with a RIFF/WAVE header", i)
		}
		// The live service repacks each chunk and rejects a streaming size or a
		// size that disagrees with the payload, so both fields are real.
		if got := binary.LittleEndian.Uint32(data[4:8]); got != uint32(36+FrameBytes) {
			t.Errorf("chunk %d RIFF size = %d, want %d", i, got, 36+FrameBytes)
		}
		if got := binary.LittleEndian.Uint32(data[40:44]); got != uint32(FrameBytes) {
			t.Errorf("chunk %d data size = %d, want %d", i, got, FrameBytes)
		}
		if binary.LittleEndian.Uint32(data[24:28]) != SampleRate {
			t.Errorf("chunk %d header sample rate = %d", i, binary.LittleEndian.Uint32(data[24:28]))
		}
	}
	if requests[3].GetEos() == nil {
		t.Errorf("last request is not eos: %v", requests[3])
	}
}

func TestMetadataBeforeAttachHasEmptyAuthorization(t *testing.T) {
	client, err := New(Config{
		BaseURL: "https://vc.go.mail.ru", Serial: testSerial, DeviceID: testDevice,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	md := client.callMetadata()
	if value, ok := md["authorization"]; !ok || value != "" {
		t.Fatalf("authorization = %q (present=%v), want one empty value", value, ok)
	}
	if value := md["x-session-id"]; value != "" {
		t.Fatalf("x-session-id = %q", value)
	}
	if value := md["x-device-ver"]; value != defaultClientVersion {
		t.Fatalf("x-device-ver = %q", value)
	}
	if value := md["x-device-sn"]; value != testSerial {
		t.Fatalf("x-device-sn = %q, want the configured serial %q", value, testSerial)
	}
	if id := md["x-client-request-id"]; !uuidRE.MatchString(id) {
		t.Fatalf("x-client-request-id = %q", id)
	}
}

func assertCallMetadata(t *testing.T, md metadata.MD, stream bool) {
	t.Helper()
	if md == nil {
		t.Fatalf("no metadata received")
	}
	for key, want := range map[string]string{
		"x-session-id":  testSession,
		"x-device-id":   testDevice,
		"x-device-sn":   testSerial,
		"x-device-ver":  "44.91",
		"authorization": "Bearer " + testSecret,
	} {
		values := md.Get(key)
		if len(values) != 1 || values[0] != want {
			t.Errorf("%s = %v, want %q (stream=%v)", key, values, want, stream)
		}
	}
	ids := md.Get("x-client-request-id")
	if len(ids) != 1 || !uuidRE.MatchString(ids[0]) {
		t.Fatalf("x-client-request-id = %v", ids)
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestClientRequestIDIsFreshPerCall(t *testing.T) {
	cert, pool := testCertificate(t)
	probe := &fakeProbe{}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterClientProbeServer(server, probe)
	})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(attachOK()))
	}))
	defer ts.Close()
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "xtoken-abc")

	for i := range 2 {
		if err := client.Authorize(context.Background()); err != nil {
			t.Fatalf("authorize %d: %v", i, err)
		}
	}
	_, mds := probe.snapshot()
	if len(mds) != 2 {
		t.Fatalf("probes = %d", len(mds))
	}
	first := mds[0].Get("x-client-request-id")
	second := mds[1].Get("x-client-request-id")
	if len(first) != 1 || len(second) != 1 || first[0] == second[0] {
		t.Fatalf("request ids are not fresh per call: %v vs %v", first, second)
	}
}

// --- authorization retry ----------------------------------------------------

func TestAuthorizeUsesSessionWithoutAttach(t *testing.T) {
	cert, pool := testCertificate(t)
	attach := &fakeAttach{response: attachOK()}
	ts := httptest.NewTLSServer(attach)
	defer ts.Close()
	probe := &fakeProbe{}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterClientProbeServer(server, probe)
	})
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "xtoken-abc")
	client.SetSession(Session{SessionID: testSession, SessionSecret: testSecret})

	if err := client.Authorize(context.Background()); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if calls, mds := probe.snapshot(); calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	} else {
		assertCallMetadata(t, mds[0], false)
	}
	if attach.callCount() != 0 {
		t.Fatalf("attach calls = %d; a configured session must never attach", attach.callCount())
	}
}

func TestAuthorizeDoesNotReplaceRejectedSession(t *testing.T) {
	cert, pool := testCertificate(t)
	attach := &fakeAttach{response: attachOK()}
	ts := httptest.NewTLSServer(attach)
	defer ts.Close()
	probe := &fakeProbe{fail: []codes.Code{codes.Unauthenticated, codes.OK}}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterClientProbeServer(server, probe)
	})
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "xtoken-abc")
	client.SetSession(Session{SessionID: testSession, SessionSecret: testSecret})

	err := client.Authorize(context.Background())
	if err == nil {
		t.Fatal("a rejected session must not be reported as authorized")
	}
	if !IsCode(err, CodeUnauthenticated) {
		t.Fatalf("error = %v, want the UNAUTHENTICATED status", err)
	}
	if attach.callCount() != 0 {
		t.Fatalf("attach calls = %d; a rejected session must be reported, not replaced", attach.callCount())
	}
	if calls, _ := probe.snapshot(); calls != 1 {
		t.Fatalf("probe calls = %d, want exactly 1", calls)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatalf("the error leaked the session secret: %v", err)
	}
}

func TestAuthorizeInvalidDeviceSNIsLegible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serial string
		want   string
	}{
		{name: "missing", serial: "", want: "device serial is missing"},
		{name: "wrong", serial: "not-the-soc-serial", want: "device serial is wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, pool := testCertificate(t)
			attach := &fakeAttach{response: attachOK()}
			ts := httptest.NewTLSServer(attach)
			defer ts.Close()
			probe := &fakeProbe{fail: []codes.Code{codes.InvalidArgument},
				desc: "Invalid 'x-device-sn' param"}
			addr := startGRPC(t, cert, func(server *grpc.Server) {
				servicepb.RegisterClientProbeServer(server, probe)
			})
			client, err := New(Config{
				BaseURL:   ts.URL,
				Serial:    tc.serial,
				DeviceID:  testDevice,
				Transport: newTestTransport(t, addr, pool, ts.Client()),
			})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			defer client.Close()

			err = client.Authorize(context.Background())
			if err == nil {
				t.Fatal("an invalid x-device-sn must fail the turn")
			}
			if !IsInvalidDeviceSN(err) {
				t.Fatalf("error = %v, want the invalid x-device-sn classification", err)
			}
			if !IsCode(err, CodeInvalidArgument) {
				t.Fatalf("error = %v, want the InvalidArgument status preserved", err)
			}
			for _, want := range []string{tc.want, `"serial" setting`, "hardware_serial"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
				}
			}
		})
	}
}

func TestAuthorizeCachesProvisioningRefusalPerClient(t *testing.T) {
	cert, pool := testCertificate(t)
	attach := &fakeAttach{status: http.StatusBadRequest,
		response: `{"error":"4002:Bad param: xtoken_id is required","code":4002,"reason":"Bad param: xtoken_id is required"}`}
	ts := httptest.NewTLSServer(attach)
	defer ts.Close()
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterClientProbeServer(server, &fakeProbe{})
	})
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "")

	for i := range 3 {
		err := client.Authorize(context.Background())
		if err == nil {
			t.Fatalf("attempt %d: a provisioning refusal must fail", i)
		}
		if !ProvisioningRequired(err) {
			t.Fatalf("attempt %d: error = %v, want a provisioning refusal", i, err)
		}
	}
	if attach.callCount() != 1 {
		t.Fatalf("attach calls = %d, want 1 (the refusal is cached for this client)", attach.callCount())
	}
}

// --- cancellation and errors ------------------------------------------------
// --- cancellation and errors ------------------------------------------------

func TestAbortReachesServerAndCancelsStream(t *testing.T) {
	cert, pool := testCertificate(t)
	attach := &fakeAttach{response: attachOK()}
	ts := httptest.NewTLSServer(attach)
	defer ts.Close()
	speech := &fakeSpeech{aborts: make(chan string, 1), waitAbort: true}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterPhraseServer(server, speech)
	})
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "xtoken-abc")
	client.SetSession(Session{SessionID: testSession, SessionSecret: testSecret})

	ctx := context.Background()
	stream, err := client.OpenSpeech(ctx, SpeechOptions{Format: FormatWave})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := stream.SendAudio(bytes.Repeat([]byte{0x00}, FrameBytes)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := stream.Abort("dismiss"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	select {
	case reason := <-speech.aborts:
		if reason != "dismiss" {
			t.Fatalf("abort reason = %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the abort frame")
	}
	// The server queues the init frame before it reads the abort, so drain it
	// first: otherwise Recv after Close may return that buffered frame with a
	// nil error and the assertion below is a race, not a check.
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("init frame: %v", err)
	}
	stream.Close()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("recv after close returned no error")
	}
}

func TestParentCancellationEndsStream(t *testing.T) {
	cert, pool := testCertificate(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(attachOK()))
	}))
	defer ts.Close()
	speech := &fakeSpeech{aborts: make(chan string, 1), waitAbort: true}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterPhraseServer(server, speech)
	})
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "xtoken-abc")
	client.SetSession(Session{SessionID: testSession, SessionSecret: testSecret})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.OpenSpeech(ctx, SpeechOptions{Format: FormatWave})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cancel()
	if _, err := stream.Recv(); !isCanceled(err) {
		t.Fatalf("recv error = %v, want cancellation", err)
	}
	stream.Close()
}

func TestStatusErrorSurfaces(t *testing.T) {
	cert, pool := testCertificate(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(attachOK()))
	}))
	defer ts.Close()
	speech := &fakeSpeech{aborts: make(chan string, 1), failCode: codes.Unauthenticated}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterPhraseServer(server, speech)
	})
	client := newTestClient(t, ts.URL, pool, ts.Client(), addr, "xtoken-abc")
	client.SetSession(Session{SessionID: testSession, SessionSecret: testSecret})

	stream, err := client.OpenSpeech(context.Background(), SpeechOptions{Format: FormatWave})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()
	_, err = stream.Recv()
	var statusErr *StatusError
	if !asError(err, &statusErr) {
		t.Fatalf("error = %v, want *StatusError", err)
	}
	if statusErr.Code != CodeUnauthenticated {
		t.Fatalf("code = %v", statusErr.Code)
	}
	if !IsCode(err, CodeUnauthenticated) {
		t.Fatal("IsCode did not recognize the status")
	}
}

func TestOpenSpeechRefusesUnimplementedOpus(t *testing.T) {
	client, err := New(Config{BaseURL: "https://vc.go.mail.ru", XTokenID: "x"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer client.Close()
	if _, err := client.OpenSpeech(context.Background(), SpeechOptions{Format: FormatOggOpus}); !isErr(err, ErrUnsupportedFormat) {
		t.Fatalf("error = %v", err)
	}
}

// --- framing and parsing ----------------------------------------------------

func TestUploaderFraming(t *testing.T) {
	u := newFrameUploader(FormatWave)
	if blocks := u.push(make([]byte, 100)); len(blocks) != 0 {
		t.Fatalf("partial push yielded %d blocks", len(blocks))
	}
	blocks := u.push(make([]byte, FrameBytes-100))
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d", len(blocks))
	}
	if len(blocks[0]) != 44+FrameBytes {
		t.Fatalf("first block = %d bytes", len(blocks[0]))
	}
	if tail := u.flush(); tail != nil {
		t.Fatalf("flush with no pending audio returned %d bytes", len(tail))
	}
	u.push(make([]byte, 10))
	tail := u.flush()
	if tail == nil || len(tail) != 44+FrameBytes {
		t.Fatalf("tail = %d bytes, want a zero-padded %d plus its WAV header", len(tail), FrameBytes)
	}
	// The header must describe the padded payload exactly: the service rejects
	// a header whose data size disagrees with the bytes in the chunk.
	if got := binary.LittleEndian.Uint32(tail[40:44]); got != uint32(FrameBytes) {
		t.Fatalf("tail data size = %d, want %d", got, FrameBytes)
	}
	if got := binary.LittleEndian.Uint32(tail[4:8]); got != uint32(36+FrameBytes) {
		t.Fatalf("tail RIFF size = %d, want %d", got, 36+FrameBytes)
	}
}

func TestParseBaseURL(t *testing.T) {
	for _, good := range []string{
		"https://vc.go.mail.ru",
		"https://vc.go.mail.ru/",
		"http://127.0.0.1:8080",
	} {
		if _, err := New(Config{BaseURL: good}); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{
		"",
		"vc.go.mail.ru",
		"ftp://vc.go.mail.ru",
		"https://vc.go.mail.ru/api",
		"https://vc.go.mail.ru?x=1",
	} {
		if _, err := New(Config{BaseURL: bad}); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

// TestParseBaseURLErrorDropsTheSignedTail covers the rejection path that reaches
// the settings log: an operator can paste a URL with a signed query into
// endpoint_custom, and the error must not echo it back.
func TestParseBaseURLErrorDropsTheSignedTail(t *testing.T) {
	const signed = "https://vc.go.mail.ru/api?X-Amz-Signature=deadbeef&X-Amz-Credential=secret#frag"
	_, err := ParseBaseURL(signed)
	if err == nil {
		t.Fatal("a path, query and fragment must be rejected")
	}
	for _, secret := range []string{"deadbeef", "X-Amz", "secret", "#frag", "?"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error echoed %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "https://vc.go.mail.ru/api") {
		t.Fatalf("the error must still name scheme://host/path: %v", err)
	}

	// The unparseable case must not fall back to the raw string either.
	if _, err := ParseBaseURL("https://vc.go.mail.ru/%zz?X-Amz-Signature=deadbeef"); err == nil {
		t.Fatal("an invalid escape must be rejected")
	} else if strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("the parse error echoed the signed query: %v", err)
	}
}

func TestParseAudioFormat(t *testing.T) {
	if f, err := ParseAudioFormat("wave"); err != nil || f != FormatWave {
		t.Fatalf("wave = %v, %v", f, err)
	}
	if f, err := ParseAudioFormat("ogg_opus"); err != nil || f != FormatOggOpus {
		t.Fatalf("ogg_opus = %v, %v", f, err)
	}
	if _, err := ParseAudioFormat("mp3"); err == nil {
		t.Fatal("mp3 must be rejected")
	}
}

// --- tiny error helpers to keep the test free of extra imports ---------------

func isErr(err, target error) bool { return errors.Is(err, target) }

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		IsCode(err, CodeCanceled) || IsCode(err, CodeDeadlineExceeded)
}

func asError(err error, target any) bool { return errors.As(err, target) }

// --- eos framing ------------------------------------------------------------

// eosProbeSpeech reports whether the client half-closed the request side after
// the eos frame. io.EOF from the follow-up Recv means it did; a timeout means
// the request side is still open, which is what the live service needs (closing
// it turns a clean result into "Internal server error").
type eosProbeSpeech struct {
	servicepb.UnimplementedPhraseServer
	result chan error
}

func (p *eosProbeSpeech) Speech(stream grpc.BidiStreamingServer[phrasepb.SpeechRequest, phrasepb.SpeechResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetInit() == nil {
		return status.Error(codes.InvalidArgument, "first frame must be init")
	}
	if err := stream.Send(&phrasepb.SpeechResponse{Kind: &phrasepb.SpeechResponse_Init{
		Init: &phrasepb.SpeechResponseInit{PhraseId: "phrase-eos"},
	}}); err != nil {
		return err
	}
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		if req.GetEos() != nil {
			break
		}
	}
	waiting := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		waiting <- err
	}()
	select {
	case err := <-waiting:
		p.result <- err
	case <-time.After(300 * time.Millisecond):
		p.result <- nil
	}
	return nil
}

func TestEOSLeavesTheRequestSideOpen(t *testing.T) {
	cert, pool := testCertificate(t)
	probe := &eosProbeSpeech{result: make(chan error, 1)}
	addr := startGRPC(t, cert, func(server *grpc.Server) {
		servicepb.RegisterPhraseServer(server, probe)
	})
	client := newTestClient(t, "https://127.0.0.1:1", pool, &http.Client{}, addr, "xtoken-abc")
	client.SetSession(Session{SessionID: testSession, SessionSecret: testSecret})

	stream, err := client.OpenSpeech(context.Background(), SpeechOptions{Format: FormatWave})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer stream.Close()
	if err := stream.SendAudio(make([]byte, FrameBytes)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := stream.EOS(); err != nil {
		t.Fatalf("eos: %v", err)
	}
	select {
	case err := <-probe.result:
		if err != nil {
			t.Fatalf("the client half-closed the request side after eos: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server never observed eos")
	}
}

func TestBuildInitKeywordMediaState(t *testing.T) {
	init := BuildInit(SpeechOptions{Format: FormatWave, Keyword: "marusya", KeywordInAudio: true,
		HWID: "9F25725B"}, testSerial, "44.91")
	params := init.GetKeywordOptions().GetKwParams()
	if params.GetMediaState() != clientpb.MediaState_MEDIA_STATE_STOP {
		t.Fatalf("media_state = %v, want STOP (the service refuses an unspecified state)",
			params.GetMediaState())
	}

	playing := BuildInit(SpeechOptions{Format: FormatWave, Keyword: "marusya", KeywordInAudio: true,
		HWID: "9F25725B", MediaState: clientpb.MediaState_MEDIA_STATE_PLAY}, testSerial, "44.91")
	if got := playing.GetKeywordOptions().GetKwParams().GetMediaState(); got != clientpb.MediaState_MEDIA_STATE_PLAY {
		t.Fatalf("explicit media_state = %v", got)
	}
	// An absent variant is rejected by the service ("unknown keyword type"), so
	// a turn without a keyword states no_kw explicitly.
	plain := BuildInit(SpeechOptions{Format: FormatWave}, testSerial, "44.91").GetKeywordOptions()
	if plain.GetNoKw() == nil {
		t.Fatalf("no_keyword init = %+v, want no_kw", plain)
	}
	if plain.GetKwParams() != nil {
		t.Fatalf("no_keyword init carried kw_params: %+v", plain.GetKwParams())
	}
}

// The recovery token is the only capability evidence the device leaks, and the
// init field it fills was the one field the previous build omitted entirely.
// These tests pin the exact bytes so a future refactor cannot silently drop
// them back to a bare init.
func TestBuildInitSendsTheRecoveredCapabilitiesPayload(t *testing.T) {
	init := BuildInit(SpeechOptions{Format: FormatWave}, testSerial, "44.91")
	capabilities := init.GetClientOptions().GetCapabilities()
	if capabilities == nil {
		t.Fatal("init has no RequestClientOptions.capabilities")
	}
	const want = `{"capabilities_hex":"31c0255a004201d1408a235de880400000"}`
	if got := string(capabilities.GetJson()); got != want {
		t.Fatalf("capabilities.json = %q, want %q", got, want)
	}
	if got := DefaultCapabilitiesHex; got != "31c0255a004201d1408a235de880400000" {
		t.Fatalf("DefaultCapabilitiesHex = %q", got)
	}

	// The payload is also present on the wire, not just in the struct.
	payload, err := proto.Marshal(init)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded := &phrasepb.SpeechRequestInit{}
	if err := proto.Unmarshal(payload, decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := string(decoded.GetClientOptions().GetCapabilities().GetJson()); got != want {
		t.Fatalf("round-tripped capabilities.json = %q, want %q", got, want)
	}
}

func TestBuildInitUsesAnOverriddenCapabilitiesAndClientData(t *testing.T) {
	init := BuildInit(SpeechOptions{
		Format:           FormatWave,
		CapabilitiesJSON: []byte(`{"capabilities_hex":"deadbeef"}`),
		ClientDataJSON:   []byte(`{"client":"capsula"}`),
	}, testSerial, "44.91")
	if got := string(init.GetClientOptions().GetCapabilities().GetJson()); got != `{"capabilities_hex":"deadbeef"}` {
		t.Fatalf("capabilities.json = %q", got)
	}
	if got := string(init.GetClientOptions().GetClientData().GetJson()); got != `{"client":"capsula"}` {
		t.Fatalf("client_data.json = %q", got)
	}
	if got := init.GetClientOptions().GetClientData().GetJson(); got == nil {
		t.Fatal("client_data.json is unset")
	}

	// Empty client data leaves the field unset, as the vendor's own init does.
	plain := BuildInit(SpeechOptions{Format: FormatWave}, testSerial, "44.91")
	if plain.GetClientOptions().GetClientData() != nil {
		t.Fatalf("client_data = %+v, want unset", plain.GetClientOptions().GetClientData())
	}
}

// kw_params is only valid when the uploaded phrase begins with the keyword;
// the service confirms the keyword from the received audio, so a capture whose
// audio starts at the command (a touch or API activation) can only ever be
// answered with "keyword not confirmed".
func TestBuildInitKeywordRequiresTheKeywordInAudio(t *testing.T) {
	commandFirst := BuildInit(SpeechOptions{Format: FormatWave, Keyword: "marusya", HWID: "9F25725B"},
		testSerial, "44.91")
	if commandFirst.GetKeywordOptions().GetNoKw() == nil {
		t.Fatalf("command-first keyword init = %+v, want no_kw", commandFirst.GetKeywordOptions())
	}
	if params := commandFirst.GetKeywordOptions().GetKwParams(); params != nil {
		t.Fatalf("command-first init claimed kw_params: %+v", params)
	}
	if got := (SpeechOptions{Keyword: "marusya"}).KeywordMode(); got != "no_kw" {
		t.Fatalf("KeywordMode = %q, want no_kw", got)
	}

	fullUtterance := BuildInit(SpeechOptions{Format: FormatWave, Keyword: "marusya",
		KeywordInAudio: true, HWID: "9F25725B"}, testSerial, "44.91")
	if params := fullUtterance.GetKeywordOptions().GetKwParams(); params == nil || params.GetKeyword() != "marusya" {
		t.Fatalf("whole-utterance init kw_params = %+v", params)
	}
	if got := (SpeechOptions{Keyword: "marusya", KeywordInAudio: true}).KeywordMode(); got != "kw_params" {
		t.Fatalf("KeywordMode = %q, want kw_params", got)
	}
}

func TestValidateCapabilitiesJSON(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"recovered default", string(DefaultCapabilitiesJSON), false},
		{"opaque vendor payload", `{"anything":[1,2,3]}`, false},
		{"non-object JSON", `"31c0255a"`, false},
		{"empty", "", true},
		{"whitespace", "  \n", true},
		{"not JSON", "31c0255a", true},
		{"odd hex token", `{"capabilities_hex":"abc"}`, true},
		{"non-hex token", `{"capabilities_hex":"zz"}`, true},
		{"non-string token", `{"capabilities_hex":49}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCapabilitiesJSON([]byte(tc.payload))
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateCapabilitiesJSON(%q) = nil, want an error", tc.payload)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateCapabilitiesJSON(%q) = %v", tc.payload, err)
			}
		})
	}
}

// TestBuildInitWireBytes pins the complete init message, not just a
// marshal/unmarshal round trip through the same descriptor: a round trip cannot
// detect a divergence from the vendor's schema, but an exact byte comparison
// fails the moment a field number, ordering, default or payload changes.
//
// The reference message is BuildInit(SpeechOptions{Format: FormatWave,
// Timezone: "Europe/Moscow", StatusIntervalMS: 1000}, "SN-TEST-0001", "44.91"),
// decoded field by field:
//
//	0a 02 08 01              audio_options.format = AUDIO_FORMAT_WAVE (field 1)
//	12 03 08 e8 07           qos_options.status_interval_ms = 1000 (field 2)
//	1a 6b                    client_options (field 3, 107 bytes)
//	  0a 3b                    capabilities (field 1, 59 bytes)
//	    0a 39                    json (field 1, 57 bytes)
//	                              {"capabilities_hex":"31c0255a…"}
//	  12 06 0a 04 1a 02 20 01  state (field 2) → capsule mic_on
//	  22 0d Europe/Moscow      timezone (field 4)
//	  62 15 0a 0c … 12 05 …    device_info (field 12) → serial, version
//	20 01                    mode = SPEECH_MODE_GENERAL (field 4)
//	32 02 22 00              keyword_options (field 6) → no_kw (field 4)
const wantInitWireHex = "0a020801120308e8071a6b0a3b0a397b226361706162696c69746965735f686578223a2233316330323535613030343230316431343038613233356465383830343030303030227d12060a041a022001220d4575726f70652f4d6f73636f7762150a0c534e2d544553542d30303031120534342e3931200132022200"

func TestBuildInitWireBytes(t *testing.T) {
	init := BuildInit(SpeechOptions{
		Format:           FormatWave,
		Timezone:         "Europe/Moscow",
		StatusIntervalMS: 1000,
	}, "SN-TEST-0001", "44.91")
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(init)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := hex.EncodeToString(payload); got != wantInitWireHex {
		t.Fatalf("init bytes changed:\n got %s\nwant %s", got, wantInitWireHex)
	}

	// The capability payload must sit at client_options(3).capabilities(1).json(1).
	capabilitiesField := init.GetClientOptions().GetCapabilities().ProtoReflect().
		Descriptor().Fields().ByName("json")
	if capabilitiesField == nil || capabilitiesField.Number() != 1 {
		t.Fatalf("capabilities.json field = %+v, want number 1", capabilitiesField)
	}
	if got := init.ProtoReflect().Descriptor().Fields().ByName("client_options").Number(); got != 3 {
		t.Fatalf("client_options field number = %d, want 3", got)
	}
}
