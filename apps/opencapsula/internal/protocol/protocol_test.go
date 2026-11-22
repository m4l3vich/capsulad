package protocol

import (
	"strings"
	"testing"
)

func TestHelloRoundTrip(t *testing.T) {
	hello := &Hello{
		ProtocolVersion: Version,
		DeviceID:        "dev-1",
		InputFormat:     DeviceInputFormat,
		Capabilities:    []string{"volume.get", "conversation.end"},
		Language:        "ru-RU",
		Location:        "Ashdod",
		TimeZone:        "Asia/Jerusalem",
		AgentVersion:    "0.1.0",
	}
	payload, err := Encode(&Message{Type: TypeHello, Hello: hello})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(payload), `"protocol_version":1`) ||
		!strings.Contains(string(payload), `"sample_rate_hertz":16000`) {
		t.Fatalf("wire shape = %s", payload)
	}
	decoded, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Type != TypeHello || decoded.Hello == nil || decoded.Hello.DeviceID != "dev-1" ||
		decoded.Hello.InputFormat != DeviceInputFormat {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestReadyRoundTrip(t *testing.T) {
	payload := []byte(`{"type":"ready","ready":{"session_id":"s-1","server_vad":true,
		"output_format":{"encoding":"LINEAR16","sample_rate_hertz":24000,"channels":1}}}`)
	message, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if message.Ready == nil || message.Ready.SessionID != "s-1" || !message.Ready.ServerVAD {
		t.Fatalf("ready = %+v", message.Ready)
	}
	if err := message.Ready.OutputFormat.Validate(); err != nil {
		t.Fatalf("output format: %v", err)
	}
}

func TestTurnMessagesRoundTrip(t *testing.T) {
	for _, message := range []*Message{
		{Type: TypeTurnStart, TurnID: "t-1"},
		{Type: TypeTurnEnd, TurnID: "t-1"},
		{Type: TypeTurnCancel, TurnID: "t-1"},
		{Type: TypeListenEnd, TurnID: "t-1"},
		{Type: TypeInterrupted},
		{Type: TypeTurnComplete, TurnID: "t-1"},
	} {
		payload, err := Encode(message)
		if err != nil {
			t.Fatalf("encode %s: %v", message.Type, err)
		}
		decoded, err := Decode(payload)
		if err != nil {
			t.Fatalf("decode %s: %v", message.Type, err)
		}
		if decoded.Type != message.Type || decoded.TurnID != message.TurnID {
			t.Fatalf("decoded %+v, want %+v", decoded, message)
		}
	}
}

func TestCommandAndResultRoundTrip(t *testing.T) {
	inbound := []byte(`{"type":"command","turn_id":"t-2","command":{"id":"c-1","action":"media.control","args":{"operation":"toggle"}}}`)
	message, err := Decode(inbound)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if message.Command == nil {
		t.Fatal("command missing")
	}
	if operation, ok := message.Command.String("operation"); !ok || operation != "toggle" {
		t.Fatalf("operation = %q, %v", operation, ok)
	}
	result := &Message{Type: TypeCommandResult, TurnID: "t-2", CommandResult: &CommandResult{
		ID: "c-1", OK: true, Data: map[string]any{"present": true, "playing": true},
	}}
	payload, err := Encode(result)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	decoded, err := Decode(payload)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if decoded.CommandResult == nil || !decoded.CommandResult.OK ||
		decoded.CommandResult.Data["present"] != true ||
		decoded.CommandResult.Data["playing"] != true {
		t.Fatalf("result = %+v", decoded.CommandResult)
	}
}

func TestDecodeRejectsMalformedMessages(t *testing.T) {
	for _, payload := range []string{
		`{"type":"command"}`,
		`{"type":"nonsense"}`,
		`{"type":"command_result","command_result":{"ok":true}}`,
		`not json`,
	} {
		if _, err := Decode([]byte(payload)); err == nil {
			t.Errorf("accepted %s", payload)
		}
	}
}

func TestAudioFormatValidation(t *testing.T) {
	for _, format := range []AudioFormat{
		{Encoding: EncodingLinear16, SampleRateHertz: 16000, Channels: 1},
		{Encoding: EncodingLinear16, SampleRateHertz: 24000, Channels: 1},
	} {
		if err := format.Validate(); err != nil {
			t.Errorf("%+v: %v", format, err)
		}
	}
	for _, format := range []AudioFormat{
		{Encoding: "OPUS", SampleRateHertz: 16000, Channels: 1},
		{Encoding: EncodingLinear16, SampleRateHertz: 48000, Channels: 1},
		{Encoding: EncodingLinear16, SampleRateHertz: 16000, Channels: 2},
	} {
		if err := format.Validate(); err == nil {
			t.Errorf("accepted %+v", format)
		}
	}
}
