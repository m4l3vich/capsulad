package tuner

import (
	"bytes"
	"testing"
)

func TestDecodeTuneRequest(t *testing.T) {
	payload := appendStringField(nil, 1, "session-1")
	payload = appendStringField(payload, 2, "https://example.test/live.mp3")
	payload = appendStringField(payload, 3, "Example Radio")
	payload = appendStringField(payload, 4, "org.capsulad.radio")
	payload = appendVarintField(payload, 99, 7) // unknown field must be ignored

	got, err := DecodeTuneRequest(payload)
	if err != nil {
		t.Fatalf("DecodeTuneRequest: %v", err)
	}
	if got.SessionID != "session-1" || got.URL != "https://example.test/live.mp3" ||
		got.StationName != "Example Radio" || got.PlayerAppID != "org.capsulad.radio" {
		t.Fatalf("unexpected request: %+v", got)
	}
}

func TestDecodeTuneRequestRejectsTruncated(t *testing.T) {
	payload := appendStringField(nil, 2, "https://example.test/live.mp3")
	if _, err := DecodeTuneRequest(payload[:len(payload)-1]); err == nil {
		t.Fatal("truncated message decoded without an error")
	}
}

func TestPushRequestMarshal(t *testing.T) {
	request := PushRequest{
		Sequence:      4,
		SessionID:     "s",
		PCM:           []byte{1, 2, 3, 4},
		SampleRateHz:  44100,
		Channels:      2,
		SampleFormat:  1,
		FirstFrame:    100,
		Discontinuity: true,
		DroppedFrames: 9,
		Title:         "Song",
		Artist:        "Artist",
	}
	fields := map[int]bool{}
	err := walk(request.Marshal(), func(f field) error {
		fields[f.num] = true
		switch f.num {
		case 2:
			if f.text() != "s" {
				t.Fatalf("session id = %q", f.text())
			}
		case 3:
			if len(f.data) != 4 || f.data[3] != 4 {
				t.Fatalf("pcm = %v", f.data)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, num := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		if !fields[num] {
			t.Fatalf("field %d missing from %v", num, fields)
		}
	}
}

func TestPushResultDecode(t *testing.T) {
	payload := appendVarintField(nil, 1, 1)
	payload = appendStringField(payload, 2, "paused")
	got, err := DecodePushResult(payload)
	if err != nil {
		t.Fatalf("DecodePushResult: %v", err)
	}
	if !got.Accepted || got.Detail != "paused" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestTuneResultMarshal(t *testing.T) {
	payload := (TuneResult{Accepted: true, SessionID: "s", ContentType: "audio/mpeg"}).Marshal()
	var accepted bool
	var session, contentType string
	err := walk(payload, func(f field) error {
		switch f.num {
		case 1:
			accepted = f.uvarint() != 0
		case 3:
			session = f.text()
		case 4:
			contentType = f.text()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !accepted || session != "s" || contentType != "audio/mpeg" {
		t.Fatalf("unexpected result: accepted=%v session=%q type=%q", accepted, session, contentType)
	}
}

func TestDecodePushResultEmptyIsRefusal(t *testing.T) {
	// proto3 marshals PushResult{accepted:false} to zero bytes; that is a
	// refusal without a detail, not a missing reply.
	result, err := decodePushResult(nil)
	if err != nil {
		t.Fatalf("decodePushResult(nil): %v", err)
	}
	if result.Accepted || result.Detail == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestPushRequestMarshalToReusesBuffer(t *testing.T) {
	request := PushRequest{Sequence: 1, SessionID: "s", PCM: []byte{1, 2, 3, 4}, SampleRateHz: 44100}
	want := request.Marshal()
	scratch := make([]byte, 0, 64)
	got := request.MarshalTo(scratch[:0])
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalTo = %v, want %v", got, want)
	}
}

func TestValidAppID(t *testing.T) {
	valid := []string{"org.capsulad.radio", "a", "0abc", "a-b_c.d"}
	for _, id := range valid {
		if !validAppID(id) {
			t.Fatalf("%q rejected", id)
		}
	}
	invalid := []string{"", "Org.capsulad", ".radio", "-radio", "a/b", "a b"}
	for _, id := range invalid {
		if validAppID(id) {
			t.Fatalf("%q accepted", id)
		}
	}
}
