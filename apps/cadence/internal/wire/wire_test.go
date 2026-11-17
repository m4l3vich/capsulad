package wire_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	cadencev2 "capsulad/apps/cadence/internal/proto/cadencev2"
	"capsulad/apps/cadence/internal/wire"
)

// TestUpstreamGoldenFrames decodes the upstream golden fixtures for
// cadence/v2 (proto/fixtures/v2/*.binpb, copied verbatim from the Cadence
// repository's `go test ./pkg/protofixtures -update` output). They are the
// authoritative proof that this client's field numbers, wire types and enum
// values are the upstream wire, not a guess. A failure here means the client
// would not interoperate.
func TestUpstreamGoldenFrames(t *testing.T) {
	dir := filepath.Join("..", "proto", "testdata", "upstream")
	read := func(name string) *cadencev2.Envelope {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name+".binpb"))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		env, err := wire.Decode(b)
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return env
	}

	t.Run("hello", func(t *testing.T) {
		h := read("hello").GetHello()
		if h.GetProtocol() != 2 || h.GetDaemonVersion() != "0.0.0-dev" || h.GetAudiodVersion() != "0.0.0-dev" ||
			h.GetBoard() != "sim" || string(h.GetCapabilitiesHash()) != "\x00\x01\x02\x03" {
			t.Fatalf("hello = %v", h)
		}
	})

	t.Run("welcome_device_info", func(t *testing.T) {
		w := read("welcome_device_info").GetWelcome()
		if w.GetServerVersion() == "" || !w.GetSendFullReport() {
			t.Fatalf("welcome = %v", w)
		}
		if w.GetDeviceInfo().GetName() == "" || w.GetDeviceInfo().GetRoomName() == "" {
			t.Fatalf("device_info = %v", w.GetDeviceInfo())
		}
	})

	t.Run("session_start", func(t *testing.T) {
		s := read("session_start").GetSessionStart()
		if s.GetSessionId() != "s-1" || s.GetTrigger() != cadencev2.Trigger_TRIGGER_BUTTON {
			t.Fatalf("session_start = %v", s)
		}
		if s.GetWake().GetEngine() != "stock" || s.GetWake().GetScore() != 0.75 {
			t.Fatalf("wake = %v", s.GetWake())
		}
	})

	t.Run("mic_audio", func(t *testing.T) {
		m := read("mic_audio").GetMicAudio()
		if m.GetSessionId() != "s-1" || m.GetSeq() != 5 || string(m.GetOpus()) != "\x01\x02\x03\x04" {
			t.Fatalf("mic_audio = %v", m)
		}
	})

	t.Run("session_cancel", func(t *testing.T) {
		c := read("session_cancel").GetSessionCancel()
		if c.GetSessionId() != "s-1" || c.GetReason() == "" {
			t.Fatalf("session_cancel = %v", c)
		}
	})

	t.Run("session_event", func(t *testing.T) {
		e := read("session_event").GetSessionEvent()
		if e.GetSessionId() != "s-1" || e.GetKind() == cadencev2.SessionEventKind_SESSION_EVENT_KIND_UNSPECIFIED {
			t.Fatalf("session_event = %v", e)
		}
	})

	t.Run("session_close", func(t *testing.T) {
		c := read("session_close").GetSessionClose()
		if c.GetSessionId() != "s-1" || !c.GetFollowUpAllowed() || c.GetCode() != "tts_failed" {
			t.Fatalf("session_close = %v", c)
		}
		f := c.GetFailure()
		if f.GetCode() != "tts_failed" || f.GetStage() != "tts" || f.GetProvider() != "tts-main" || f.GetWaitedMs() != 120 {
			t.Fatalf("failure = %v", f)
		}
	})

	t.Run("session_close_follow_up", func(t *testing.T) {
		c := read("session_close_follow_up").GetSessionClose()
		if c.GetSessionId() == "" || c.GetFollowUpWindowMs() == 0 {
			t.Fatalf("session_close_follow_up = %v", c)
		}
	})

	t.Run("stream_start", func(t *testing.T) {
		s := read("stream_start").GetStreamStart()
		if s.GetStreamId() != "st-1" || s.GetSessionId() != "s-1" ||
			s.GetPurpose() != cadencev2.StreamPurpose_STREAM_PURPOSE_REPLY ||
			s.GetCodec() != cadencev2.Codec_CODEC_OPUS || s.GetChannels() != 2 || !s.GetDuck() {
			t.Fatalf("stream_start = %v", s)
		}
	})

	t.Run("audio_down", func(t *testing.T) {
		a := read("audio_down").GetAudioDown()
		if a.GetStreamId() != "st-1" || a.GetSeq() != 9 || string(a.GetOpus()) != "\x05\x06\x07" {
			t.Fatalf("audio_down = %v", a)
		}
	})

	t.Run("stream_end", func(t *testing.T) {
		if e := read("stream_end").GetStreamEnd(); e.GetStreamId() != "st-1" {
			t.Fatalf("stream_end = %v", e)
		}
	})

	t.Run("capability_report", func(t *testing.T) {
		r := read("capability_report").GetCapabilityReport()
		if len(r.GetDescriptors()) != 1 {
			t.Fatalf("descriptors = %v", r.GetDescriptors())
		}
		d := r.GetDescriptors()[0]
		if d.GetId() != "audio.output" || d.GetVersion() != 3 ||
			d.GetConfigSchema() != `{"type":"object"}` || d.GetStateSchema() != `{"type":"object"}` {
			t.Fatalf("descriptor = %v", d)
		}
		if len(d.GetSecrets()) != 1 || d.GetSecrets()[0].GetName() != "api_key" {
			t.Fatalf("secrets = %v", d.GetSecrets())
		}
		if string(r.GetHash()) != "\x0a\x0b\x0c\x0d" {
			t.Fatalf("hash = %x", r.GetHash())
		}
	})

	t.Run("config_set", func(t *testing.T) {
		c := read("config_set").GetConfigSet()
		if c.GetCapability() != "audio.output" || c.GetGeneration() != 7 || c.GetJson() != `{"level":40}` {
			t.Fatalf("config_set = %v", c)
		}
		if c.GetSecrets()["api_key"] != "s3cret" {
			t.Fatalf("secrets = %v", c.GetSecrets())
		}
	})

	t.Run("command_result", func(t *testing.T) {
		r := read("command_result").GetCommandResult()
		if r.GetRequestId() == "" || r.GetCode() == "" {
			t.Fatalf("command_result = %v", r)
		}
	})

	t.Run("duck", func(t *testing.T) {
		d := read("duck").GetDuck()
		if d.GetLevel() == 0 || d.GetRampMs() == 0 || d.GetReason() == cadencev2.DuckReason_DUCK_REASON_UNSPECIFIED {
			t.Fatalf("duck = %v", d)
		}
	})

	t.Run("mic_open", func(t *testing.T) {
		m := read("mic_open").GetMicOpen()
		if m.GetSessionId() == "" || m.GetOriginDeviceId() == "" {
			t.Fatalf("mic_open = %v", m)
		}
	})

	t.Run("voice_level", func(t *testing.T) {
		v := read("voice_level").GetVoiceLevel()
		if v.GetTurnId() == "" || v.GetMic() == 0 {
			t.Fatalf("voice_level = %v", v)
		}
	})

	// Every fixture must also encode back to itself: deterministic marshalling
	// of the parsed message has to reproduce the upstream bytes, which proves
	// no field the client declares is misplaced.
	for _, name := range []string{"hello", "session_start", "mic_audio", "stream_start", "audio_down",
		"session_close", "capability_report", "config_set", "duck", "mic_open", "voice_level",
		"command_result", "session_cancel", "session_close_follow_up", "session_event", "stream_end",
		"welcome_device_info"} {
		t.Run("roundtrip/"+name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(dir, name+".binpb"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := wire.Encode(read(name))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("re-encode differs from upstream fixture:\n got %x\nwant %x", got, want)
			}
		})
	}
}

// TestEncodeRejectsLargeFrame and TestDecodeUnknownBody mirror the upstream
// pkg/wire tests: the same bounds and the same skip-not-close rule.
func TestEncodeRejectsLargeFrame(t *testing.T) {
	env := &cadencev2.Envelope{Body: &cadencev2.Envelope_AudioDown{
		AudioDown: &cadencev2.AudioDown{StreamId: "s", Opus: make([]byte, wire.MaxFrameBytes+1)},
	}}
	if _, err := wire.Encode(env); !errors.Is(err, wire.ErrFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := wire.Decode([]byte{0xff, 0xff}); !errors.Is(err, wire.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestDecodeUnknownBody(t *testing.T) {
	newer := protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1)
	if _, err := wire.Decode(newer); !errors.Is(err, wire.ErrUnknown) {
		t.Fatalf("got %v", err)
	}
}

// TestUnknownUpstreamArmIsSkipped proves an arm this client does not declare —
// upstream's Cards (27) — decodes as ErrUnknown rather than a hard failure, so
// a newer server does not close the link.
func TestUnknownUpstreamArmIsSkipped(t *testing.T) {
	b := protowire.AppendTag(nil, 27, protowire.BytesType)
	b = protowire.AppendBytes(b, []byte{0x08, 0x01})
	if _, err := wire.Decode(b); !errors.Is(err, wire.ErrUnknown) {
		t.Fatalf("got %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	env := &cadencev2.Envelope{Body: &cadencev2.Envelope_Hello{
		Hello: &cadencev2.Hello{Protocol: 2, Board: "capsula-mini"},
	}}
	b, err := wire.Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := wire.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, env) {
		t.Fatalf("got %v", got)
	}
}
