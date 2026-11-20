package vkclient

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"capsulad/apps/marusia/internal/vkproto/phrasepb"
)

type resultTransport struct {
	request HTTPRequest
	body    []byte
	status  int
	err     error
}

func (t *resultTransport) Do(_ context.Context, req HTTPRequest, _ time.Duration) (*HTTPResponse, error) {
	t.request = req
	if t.err != nil {
		return nil, t.err
	}
	status := t.status
	if status == 0 {
		status = 200
	}
	return &HTTPResponse{StatusCode: status, Body: t.body, Header: map[string]string{"content-type": "application/json"}}, nil
}

func (t *resultTransport) Unary(context.Context, string, string, map[string]string, []byte, time.Duration) ([]byte, error) {
	return nil, io.EOF
}

func (t *resultTransport) NewStream(context.Context, string, string, map[string]string, CallKind, time.Duration) (Stream, error) {
	return nil, io.EOF
}

func (t *resultTransport) Close() error { return nil }

type cannedStream struct {
	messages [][]byte
	index    int
}

func (s *cannedStream) Send([]byte) error { return nil }

func (s *cannedStream) Recv() ([]byte, error) {
	if s.index >= len(s.messages) {
		return nil, io.EOF
	}
	message := s.messages[s.index]
	s.index++
	return message, nil
}

func (s *cannedStream) CloseSend() error   { return nil }
func (s *cannedStream) Close() error       { return nil }
func (s *cannedStream) Abort(string) error { return nil }
func (s *cannedStream) EOS() error         { return nil }
func (s *cannedStream) ChunksSent() int32  { return 0 }

func newResultSpeech(t *testing.T, body []byte) (*Speech, *resultTransport) {
	t.Helper()
	base, err := url.Parse("https://vc.example")
	if err != nil {
		t.Fatal(err)
	}
	transport := &resultTransport{body: body}
	client := &Client{
		base:      base,
		cfg:       Config{DeviceID: "device-1"},
		transport: transport,
		session:   Session{SessionID: "session-1", SessionSecret: "secret-1"},
	}
	return &Speech{
		client:   client,
		opts:     SpeechOptions{CapabilitiesJSON: DefaultCapabilitiesJSON},
		phraseID: "phrase-1",
	}, transport
}

func TestFetchPhraseResultMapsCommands(t *testing.T) {
	body := []byte(`{"result":{"commands":[
		{"type":"text","text":"hello"},
		{"type":"tts","text":"hello","stream_id":"stream-1"},
		{"type":"listen"}
	]}}`)
	speech, transport := newResultSpeech(t, body)
	events, err := speech.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Kind != EventTextSoFar || events[0].Text != "hello" {
		t.Fatalf("text event = %+v", events[0])
	}
	if events[1].Kind != EventCommands || len(events[1].Commands) != 3 {
		t.Fatalf("commands = %+v", events[1])
	}
	commands := events[1].Commands
	if commands[0].Kind != CommandShowText || commands[0].Text != "hello" {
		t.Fatalf("show_text = %+v", commands[0])
	}
	if commands[1].Kind != CommandPlayURL || !strings.Contains(commands[1].URL, "/stream?stream_id=stream-1") {
		t.Fatalf("tts = %+v", commands[1])
	}
	if commands[2].Kind != CommandListen {
		t.Fatalf("listen = %+v", commands[2])
	}
	if transport.request.Method != "GET" || !strings.Contains(transport.request.URL, "/phrase/result?") {
		t.Fatalf("request = %s %s", transport.request.Method, transport.request.URL)
	}
	if !transport.request.NoRedirects {
		t.Fatal("phrase result request must disable redirects")
	}
	parsed, err := url.Parse(transport.request.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"phrase_id":        "phrase-1",
		"capabilities_hex": DefaultCapabilitiesHex,
		"device_id":        "device-1",
		"session_id":       "session-1",
	} {
		if got := query.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if got := transport.request.Header["Authorization"]; got != "Bearer secret-1" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestSpeechResultAfterFinish(t *testing.T) {
	initPayload, err := proto.Marshal(&phrasepb.SpeechResponse{Kind: &phrasepb.SpeechResponse_Init{
		Init: &phrasepb.SpeechResponseInit{PhraseId: "phrase-1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	finishPayload, err := proto.Marshal(&phrasepb.SpeechResponse{Kind: &phrasepb.SpeechResponse_Finish{
		Finish: &phrasepb.SpeechResponseFinish{Continuation: &phrasepb.SpeechResponseFinish_Result{
			Result: &phrasepb.SpeechResponseFinish_ContinuationResult{},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"result":{"commands":[{"type":"text","text":"hello"},{"type":"tts","stream_id":"stream-1"}]}}`)
	speech, _ := newResultSpeech(t, body)
	speech.stream = &cannedStream{messages: [][]byte{initPayload, finishPayload}}

	event, err := speech.Recv()
	if err != nil || event.Kind != EventInit || event.PhraseID != "phrase-1" {
		t.Fatalf("init = %+v, %v", event, err)
	}
	event, err = speech.Recv()
	if err != nil || event.Kind != EventFinish {
		t.Fatalf("finish = %+v, %v", event, err)
	}
	events, err := speech.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if len(events) != 2 || events[0].Kind != EventTextSoFar || events[1].Kind != EventCommands {
		t.Fatalf("result events = %+v", events)
	}
	if events[1].Commands[1].Kind != CommandPlayURL || !strings.Contains(events[1].Commands[1].URL, "/stream?stream_id=stream-1") {
		t.Fatalf("tts = %+v", events[1].Commands[1])
	}
}

func TestResultErrorsAreRedacted(t *testing.T) {
	const query = "https://vc.example/phrase/result?phrase_id=phrase-1&session_id=session-1&device_id=device-1"
	speech, _ := newResultSpeech(t, nil)
	speech.client.transport = &resultTransport{err: errors.New("redirect Location: " + query)}
	_, err := speech.Result(context.Background())
	if err == nil {
		t.Fatal("Result must fail")
	}
	for _, secret := range []string{"phrase-1", "session-1", "device-1", "https://"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("result error leaked %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "phrase result request failed") {
		t.Fatalf("result error class = %v", err)
	}

	httpSpeech, _ := newResultSpeech(t, nil)
	httpSpeech.client.transport = &resultTransport{status: 400, body: []byte(`{"reason":"session_id=session-1"}`)}
	_, err = httpSpeech.Result(context.Background())
	if err == nil {
		t.Fatal("Result must fail on HTTP 400")
	}
	if err.Error() != "the phrase result request returned HTTP 400" {
		t.Fatalf("HTTP error = %q", err)
	}
}
