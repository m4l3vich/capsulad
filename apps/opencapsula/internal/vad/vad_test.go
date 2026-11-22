package vad

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

const frameMS = 20

func tone(ms int, frequency, amplitude float64) []byte {
	samples := 16000 * ms / 1000
	pcm := make([]byte, samples*2)
	for i := range samples {
		value := amplitude * math.Sin(2*math.Pi*frequency*float64(i)/16000)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(value*32767)))
	}
	return pcm
}

func silence(ms int) []byte {
	return make([]byte, 16000*ms/1000*2)
}

func feed(e *Endpointer, pcm []byte) []Decision {
	var decisions []Decision
	for offset := 0; offset+320*2 <= len(pcm); offset += 320 * 2 {
		decisions = append(decisions, e.Process(pcm[offset:offset+320*2]))
	}
	return decisions
}

func count(decisions []Decision, want Decision) int {
	total := 0
	for _, decision := range decisions {
		if decision == want {
			total++
		}
	}
	return total
}

func TestOnsetAfterSpeechRun(t *testing.T) {
	e := New(DefaultConfig())
	e.Reset(0)
	decisions := feed(e, append(silence(400), tone(400, 220, 0.25)...))
	started := -1
	for i, decision := range decisions {
		if decision == SpeechStarted {
			started = i
			break
		}
	}
	if started < 0 {
		t.Fatalf("no onset in %v", decisions)
	}
	// 400 ms of silence is 20 frames; the onset follows SpeechStart
	// (100 ms) of confirmed speech, with a five-frame smoothing window.
	if started < 20 || started > 28 {
		t.Fatalf("onset at frame %d, want 20..28", started)
	}
	if count(decisions, SpeechStarted) != 1 {
		t.Fatalf("SpeechStarted repeated: %v", decisions)
	}
}

func TestEndAfterTrailingSilence(t *testing.T) {
	e := New(DefaultConfig())
	e.Reset(0)
	decisions := feed(e, append(tone(500, 220, 0.25), silence(1200)...))
	if count(decisions, EndOfUtterance) == 0 {
		t.Fatalf("no end in %v", decisions)
	}
	lastEnd := -1
	for i, decision := range decisions {
		if decision == EndOfUtterance {
			lastEnd = i
		}
	}
	if lastEnd < 30 {
		t.Fatalf("ended at frame %d, before the silence window", lastEnd)
	}
	// Once completed, the decision is stable so a caller that drains one more
	// chunk cannot mistake the end for a new turn.
	if got := e.Process(silence(frameMS)); got != EndOfUtterance {
		t.Fatalf("after completion = %v", got)
	}
}

func first(decisions []Decision, want Decision) int {
	for i, decision := range decisions {
		if decision == want {
			return i
		}
	}
	return -1
}

func TestNoSpeechInProlongedSilence(t *testing.T) {
	e := New(DefaultConfig())
	e.Reset(0)
	decisions := feed(e, silence(7000))
	at := first(decisions, NoSpeech)
	if at < 295 || at > 305 {
		t.Fatalf("no-speech at frame %d, want the 6 s window (frame 300)", at)
	}
	if count(decisions, SpeechStarted) != 0 {
		t.Fatalf("silence started speech: %v", decisions)
	}
	if got := e.Process(silence(frameMS)); got != NoSpeech {
		t.Fatalf("after completion = %v", got)
	}
}

func TestFollowUpWindowOverridesNoSpeechTimeout(t *testing.T) {
	e := New(DefaultConfig())
	e.Reset(1500 * time.Millisecond)
	if got := feed(e, silence(1000)); count(got, NoSpeech) != 0 {
		t.Fatalf("ended before the follow-up window: %v", got)
	}
	got := feed(e, silence(800))
	at := first(got, NoSpeech)
	if at < 20 || at > 30 {
		t.Fatalf("follow-up no-speech at frame %d, want about frame 25", at)
	}
}

func TestResetStartsANewUtterance(t *testing.T) {
	e := New(DefaultConfig())
	e.Reset(0)
	feed(e, append(tone(400, 220, 0.25), silence(1000)...))
	e.Reset(0)
	got := feed(e, tone(400, 220, 0.25))
	if count(got, SpeechStarted) != 1 {
		t.Fatalf("second utterance did not start: %v", got)
	}
}
