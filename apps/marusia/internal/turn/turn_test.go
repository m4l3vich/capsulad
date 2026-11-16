package turn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"capsula/apps/marusia/internal/vkclient"
)

type fakeEndpoint struct {
	stream  *fakeStream
	authErr error
	openErr error
}

func (e *fakeEndpoint) Authorize(context.Context) error { return e.authErr }

func (e *fakeEndpoint) OpenSpeech(context.Context, vkclient.SpeechOptions) (Stream, error) {
	if e.openErr != nil {
		return nil, e.openErr
	}
	return e.stream, nil
}

type fakeStream struct {
	mu     sync.Mutex
	sends  []string
	aborts []string
	chunks int32
	onEOS  func()
	onSend func(n int32)

	events chan *vkclient.Event
	done   chan struct{}
	once   sync.Once
}

func newFakeStream(onEOS func()) *fakeStream {
	return &fakeStream{
		events: make(chan *vkclient.Event, 32),
		done:   make(chan struct{}),
		onEOS:  onEOS,
	}
}

func (s *fakeStream) SendAudio(pcm []byte) error {
	s.mu.Lock()
	s.sends = append(s.sends, "audio")
	s.chunks++
	n, onSend := s.chunks, s.onSend
	s.mu.Unlock()
	if onSend != nil {
		onSend(n)
	}
	return nil
}

func (s *fakeStream) ChunksSent() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunks
}

func (s *fakeStream) EOS() error {
	s.mu.Lock()
	s.sends = append(s.sends, "eos")
	s.mu.Unlock()
	if s.onEOS != nil {
		s.onEOS()
	}
	return nil
}

func (s *fakeStream) Abort(reason string) error {
	s.mu.Lock()
	s.aborts = append(s.aborts, reason)
	s.mu.Unlock()
	return nil
}

func (s *fakeStream) Recv() (*vkclient.Event, error) {
	select {
	case event := <-s.events:
		return event, nil
	case <-s.done:
		return nil, io.EOF
	}
}

func (s *fakeStream) Close() { s.once.Do(func() { close(s.done) }) }

func (s *fakeStream) pushes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sends...)
}

type fakePlayer struct {
	mu     sync.Mutex
	url    string
	err    error
	called bool
}

func (p *fakePlayer) Play(_ context.Context, url string) error {
	p.mu.Lock()
	p.url = url
	p.called = true
	p.mu.Unlock()
	return p.err
}

type fakeIndicator struct {
	mu     sync.Mutex
	states []State
}

func (i *fakeIndicator) Show(state State) {
	i.mu.Lock()
	i.states = append(i.states, state)
	i.mu.Unlock()
}

func (i *fakeIndicator) seen() []State {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]State(nil), i.states...)
}

// loudFrame is a 20 ms 16 kHz mono square wave well above the voice threshold.
func loudFrame() []byte {
	pcm := make([]byte, vkclient.FrameBytes)
	for i := 0; i < vkclient.FrameSamples; i++ {
		value := int16(12000)
		if i%2 == 0 {
			value = -12000
		}
		pcm[i*2] = byte(uint16(value))
		pcm[i*2+1] = byte(uint16(value) >> 8)
	}
	return pcm
}

func playingFrames(playURL string) []*vkclient.Event {
	return []*vkclient.Event{
		{Kind: vkclient.EventInit, PhraseID: "p-1", ChunkLengthHintMS: 20},
		{Kind: vkclient.EventTextSoFar, Text: "погода"},
		{Kind: vkclient.EventCommands, Commands: []vkclient.Command{
			{Kind: vkclient.CommandPlayURL, URL: playURL},
		}},
		{Kind: vkclient.EventFinish, Finish: vkclient.FinishResult},
		{Kind: vkclient.EventEOS},
	}
}

func TestTurnStreamsAudioThenPlaysReply(t *testing.T) {
	stream := newFakeStream(nil)
	stream.onEOS = func() {
		for _, event := range playingFrames("https://tts.example/reply.wav") {
			stream.events <- event
		}
	}
	player := &fakePlayer{}
	indicator := &fakeIndicator{}
	chunks := make(chan []byte, 8)
	chunks <- loudFrame()
	chunks <- loudFrame()
	chunks <- loudFrame()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := Run(ctx, Options{
		Endpoint:   &fakeEndpoint{stream: stream},
		Chunks:     chunks,
		Player:     player,
		Indicator:  indicator,
		EndSilence: 40 * time.Millisecond,
		MaxSpeech:  2 * time.Second,
	})
	if result.Outcome != Completed {
		t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
	}
	if result.PlayedURL != "https://tts.example/reply.wav" {
		t.Fatalf("played url = %q", result.PlayedURL)
	}
	if result.Text != "погода" {
		t.Fatalf("text = %q", result.Text)
	}
	if !player.called || !strings.HasSuffix(player.url, "reply.wav") {
		t.Fatalf("player = %+v", player)
	}

	pushes := stream.pushes()
	if len(pushes) < 4 || pushes[len(pushes)-1] != "eos" {
		t.Fatalf("pushes = %v", pushes)
	}
	for _, push := range pushes[:len(pushes)-1] {
		if push != "audio" {
			t.Fatalf("pushes = %v", pushes)
		}
	}

	states := indicator.seen()
	if len(states) < 3 || states[0] != StateThinking || states[1] != StateListening || states[len(states)-1] != StateSpeaking {
		t.Fatalf("states = %v", states)
	}
}

func TestTurnDismissSendsAbortAndReleases(t *testing.T) {
	stream := newFakeStream(nil)
	indicator := &fakeIndicator{}
	chunks := make(chan []byte)
	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct{ result Result }
	done := make(chan outcome, 1)
	go func() {
		done <- outcome{Run(ctx, Options{
			Endpoint:   &fakeEndpoint{stream: stream},
			Chunks:     chunks,
			Player:     &fakePlayer{},
			Indicator:  indicator,
			EndSilence: 40 * time.Millisecond,
		})}
	}()

	// Wait until capture is actually being read, then dismiss.
	deadline := time.Now().Add(2 * time.Second)
	for {
		states := indicator.seen()
		if len(states) > 0 && states[len(states)-1] == StateListening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn never reached listening; states=%v", states)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case got := <-done:
		if got.result.Outcome != Dismissed {
			t.Fatalf("outcome = %v (%s)", got.result.Outcome, got.result.Detail)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not return after dismissal")
	}
	if aborts := stream.aborts; len(aborts) != 1 || aborts[0] != "dismissed" {
		t.Fatalf("aborts = %v", aborts)
	}
}

func TestTurnServerAbortFails(t *testing.T) {
	var stream *fakeStream
	stream = newFakeStream(func() {
		stream.events <- &vkclient.Event{Kind: vkclient.EventInit, PhraseID: "p-2"}
		stream.events <- &vkclient.Event{Kind: vkclient.EventFinish,
			Finish: vkclient.FinishAbort, AbortReason: "keyword not confirmed"}
		stream.events <- &vkclient.Event{Kind: vkclient.EventEOS}
	})
	player := &fakePlayer{}
	chunks := make(chan []byte, 8)
	for range 8 {
		chunks <- loudFrame()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := Run(ctx, Options{
		Endpoint:   &fakeEndpoint{stream: stream},
		Chunks:     chunks,
		Player:     player,
		EndSilence: 40 * time.Millisecond,
	})
	if result.Outcome != Failed || !strings.Contains(result.Detail, "keyword not confirmed") {
		t.Fatalf("result = %+v", result)
	}
	if player.called {
		t.Fatal("player must not run after a server abort")
	}
}

func TestTurnReportsNoSpeech(t *testing.T) {
	stream := newFakeStream(nil)
	chunks := make(chan []byte, 4)
	silence := make([]byte, vkclient.FrameBytes)
	for range 4 {
		chunks <- silence
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	result := Run(ctx, Options{
		Endpoint:        &fakeEndpoint{stream: stream},
		Chunks:          chunks,
		Player:          &fakePlayer{},
		NoSpeechTimeout: 60 * time.Millisecond,
	})
	if result.Outcome != NoSpeech {
		t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("no-speech deadline took %s", time.Since(start))
	}
	if aborts := stream.aborts; len(aborts) != 1 || aborts[0] != "no speech" {
		t.Fatalf("aborts = %v", aborts)
	}
}

func TestTurnPlayerFailureIsActionable(t *testing.T) {
	var stream *fakeStream
	stream = newFakeStream(func() {
		for _, event := range playingFrames("https://tts.example/broken.ogg") {
			stream.events <- event
		}
	})
	chunks := make(chan []byte, 4)
	chunks <- loudFrame()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := Run(ctx, Options{
		Endpoint:   &fakeEndpoint{stream: stream},
		Chunks:     chunks,
		Player:     &fakePlayer{err: errors.New("the body is Ogg (Opus), which this build cannot decode")},
		EndSilence: 40 * time.Millisecond,
	})
	if result.Outcome != Failed {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	if !strings.Contains(result.Detail, "broken.ogg") || !strings.Contains(result.Detail, "Ogg") {
		t.Fatalf("detail = %q", result.Detail)
	}
}

func TestTurnHoldToTalkReleaseEndsCapture(t *testing.T) {
	var stream *fakeStream
	stream = newFakeStream(func() {
		for _, event := range playingFrames("https://tts.example/a.wav") {
			stream.events <- event
		}
	})
	chunks := make(chan []byte, 4)
	chunks <- loudFrame()
	release := make(chan struct{})
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := Run(ctx, Options{
		Endpoint:   &fakeEndpoint{stream: stream},
		Chunks:     chunks,
		Release:    release,
		HoldToTalk: true,
		Player:     &fakePlayer{},
		EndSilence: time.Second,
	})
	if result.Outcome != Completed {
		t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
	}
	pushes := stream.pushes()
	if pushes[len(pushes)-1] != "eos" {
		t.Fatalf("pushes = %v", pushes)
	}
}

// A capture stream that closes mid-turn is end of audio, not a failure: the
// framework closes it when a hold-to-talk key is released, and the service still
// needs the eos edge and its terminal frame.
func TestTurnMicCloseEndsSpeechInsteadOfFailing(t *testing.T) {
	var stream *fakeStream
	stream = newFakeStream(func() {
		for _, event := range playingFrames("https://tts.example/a.wav") {
			stream.events <- event
		}
	})
	chunks := make(chan []byte, 2)
	chunks <- loudFrame()
	close(chunks)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := Run(ctx, Options{
		Endpoint:   &fakeEndpoint{stream: stream},
		Chunks:     chunks,
		HoldToTalk: true,
		Player:     &fakePlayer{},
		EndSilence: time.Second,
	})
	if result.Outcome != Completed {
		t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
	}
	pushes := stream.pushes()
	if len(pushes) == 0 || pushes[len(pushes)-1] != "eos" {
		t.Fatalf("pushes = %v, want the last push to be eos", pushes)
	}
}

func TestTurnSurfacesAttachRejection(t *testing.T) {
	result := Run(context.Background(), Options{
		Endpoint: &fakeEndpoint{authErr: &vkclient.AttachError{
			Status: 403, Body: `{"error":"unknown device"}`,
		}},
		Chunks: make(chan []byte),
	})
	if result.Outcome != Failed || !strings.Contains(result.Detail, "HTTP 403") ||
		!strings.Contains(result.Detail, "reauth") {
		t.Fatalf("result = %+v", result)
	}
}

func TestTurnRejectsUnimplementedFormat(t *testing.T) {
	result := Run(context.Background(), Options{
		Endpoint: &fakeEndpoint{openErr: vkclient.ErrUnsupportedFormat},
		Chunks:   make(chan []byte),
	})
	if result.Outcome != Failed || !strings.Contains(result.Detail, "wave") {
		t.Fatalf("result = %+v", result)
	}
}

func TestEndpointerDetectsSpeechAndSilence(t *testing.T) {
	gate := newEndpointer(40 * time.Millisecond)
	start := time.Now()
	gate.frame(loudFrame(), start)
	if !gate.state(start).speechSeen {
		t.Fatal("loud frame was not detected as speech")
	}
	if gate.state(start.Add(10 * time.Millisecond)).endOfSpeech {
		t.Fatal("end of speech before the hangover window")
	}
	if !gate.state(start.Add(50 * time.Millisecond)).endOfSpeech {
		t.Fatal("end of speech was not detected after the hangover window")
	}

	quiet := newEndpointer(time.Millisecond)
	silence := make([]byte, vkclient.FrameBytes)
	for range 10 {
		quiet.frame(silence, start)
	}
	if quiet.state(start.Add(time.Second)).speechSeen {
		t.Fatal("silence was detected as speech")
	}
	if math.Abs(quiet.noiseDB) > 1000 {
		t.Fatalf("noise floor diverged: %v", quiet.noiseDB)
	}
}

// blockingPlayer stays in playback until the turn is cancelled, which is what
// a dismiss during reply playback looks like to the player.
type blockingPlayer struct {
	started chan struct{}
	url     string
}

func (p *blockingPlayer) Play(ctx context.Context, url string) error {
	p.url = url
	close(p.started)
	<-ctx.Done()
	return ctx.Err()
}

// cpuTime is the process CPU time, the measure of a spinning select.
func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// TestTurnReleaseDoesNotSpin keeps the stream open after the hold-to-talk
// release. The release channel is closed and can never fire again, so the
// turn must block waiting for the server instead of re-entering the select on
// a permanently ready arm.
func TestTurnReleaseDoesNotSpin(t *testing.T) {
	stream := newFakeStream(nil) // never sends a terminal frame
	chunks := make(chan []byte)
	release := make(chan struct{})
	close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan Result, 1)
	go func() {
		done <- Run(ctx, Options{
			Endpoint:        &fakeEndpoint{stream: stream},
			Chunks:          chunks,
			Release:         release,
			HoldToTalk:      true,
			Player:          &fakePlayer{},
			EndSilence:      time.Minute,
			MaxSpeech:       time.Minute,
			TerminalTimeout: time.Minute,
		})
	}()

	// Wait until end of speech has been sent; from here the turn only waits
	// for the server's terminal frame.
	deadline := time.Now().Add(2 * time.Second)
	for {
		pushes := stream.pushes()
		if len(pushes) > 0 && pushes[len(pushes)-1] == "eos" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the release never sent eos; pushes = %v", pushes)
		}
		time.Sleep(5 * time.Millisecond)
	}

	const window = 400 * time.Millisecond
	before := cpuTime(t)
	time.Sleep(window)
	used := cpuTime(t) - before

	cancel()
	select {
	case result := <-done:
		if result.Outcome != Dismissed {
			t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the turn did not return after dismissal")
	}
	// A blocking select costs a ticker wake-up every 20 ms; a busy one costs a
	// full core, i.e. the whole window.
	t.Logf("CPU while waiting after the release: %s in %s", used, window)
	if used > window/4 {
		t.Fatalf("the turn burned %s of CPU in a %s wait; the release arm is spinning", used, window)
	}
}

// TestTurnDismissDuringPlaybackIsDismissed cancels between StateSpeaking and
// the end of the turn: a cancelled playback is a dismissal, not a media error.
func TestTurnDismissDuringPlaybackIsDismissed(t *testing.T) {
	const playURL = "https://tts.example/long-reply.wav"
	var stream *fakeStream
	stream = newFakeStream(func() {
		for _, event := range playingFrames(playURL) {
			stream.events <- event
		}
	})
	player := &blockingPlayer{started: make(chan struct{})}
	indicator := &fakeIndicator{}
	chunks := make(chan []byte, 4)
	chunks <- loudFrame()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan Result, 1)
	go func() {
		done <- Run(ctx, Options{
			Endpoint:   &fakeEndpoint{stream: stream},
			Chunks:     chunks,
			Player:     player,
			Indicator:  indicator,
			EndSilence: 40 * time.Millisecond,
		})
	}()

	select {
	case <-player.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never reached playback")
	}
	if states := indicator.seen(); len(states) == 0 || states[len(states)-1] != StateSpeaking {
		t.Fatalf("states = %v; playback must be visible before dismissal", states)
	}
	cancel()

	select {
	case result := <-done:
		if result.Outcome != Dismissed {
			t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
		}
		if result.Text != "погода" {
			t.Fatalf("text = %q; a dismissal keeps the partial reply", result.Text)
		}
		if result.PlayedURL != playURL {
			t.Fatalf("played url = %q", result.PlayedURL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the turn did not return after dismissal")
	}
}

func TestTurnReportsNoAnswerInsteadOfSuccess(t *testing.T) {
	var stream *fakeStream
	stream = newFakeStream(func() {
		stream.events <- &vkclient.Event{Kind: vkclient.EventInit, PhraseID: "p-3"}
		stream.events <- &vkclient.Event{Kind: vkclient.EventStatus, Status: &vkclient.StreamStatus{
			ChunksReceived: 13, AudioReceivedMS: 1040,
		}}
		stream.events <- &vkclient.Event{Kind: vkclient.EventFinish, Finish: vkclient.FinishResult}
		stream.events <- &vkclient.Event{Kind: vkclient.EventEOS}
	})
	player := &fakePlayer{}
	chunks := make(chan []byte, 8)
	for range 3 {
		chunks <- loudFrame()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := Run(ctx, Options{
		Endpoint:   &fakeEndpoint{stream: stream},
		Chunks:     chunks,
		Player:     player,
		EndSilence: 40 * time.Millisecond,
	})
	if result.Outcome != NoAnswer {
		t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
	}
	if !strings.Contains(result.Detail, "no answer") || !strings.Contains(result.Detail, "result") {
		t.Fatalf("detail = %q; it must name the terminal frame", result.Detail)
	}
	if player.called {
		t.Fatal("a turn without a play_url must not open playback")
	}
}

func TestTurnMarksRejectedCredentials(t *testing.T) {
	result := Run(context.Background(), Options{
		Endpoint: &fakeEndpoint{authErr: &vkclient.StatusError{
			Op: "probe", Code: codes.Unauthenticated, Message: "session expired",
		}},
		Chunks: make(chan []byte),
	})
	if result.Outcome != Failed {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	if !result.AuthRejected {
		t.Fatalf("a rejected session must be reported so the caller can drop it: %+v", result)
	}
	if !strings.Contains(result.Detail, "session_id/session_secret") {
		t.Fatalf("detail = %q; it must be actionable", result.Detail)
	}
}

func TestTurnMapsProvisioningRefusal(t *testing.T) {
	result := Run(context.Background(), Options{
		Endpoint: &fakeEndpoint{authErr: &vkclient.AttachError{
			Status: 400, Code: 4002,
			Body: `{"error":"4002:Bad param: xtoken_id is required","code":4002}`,
		}},
		Chunks: make(chan []byte),
	})
	if result.Outcome != Failed {
		t.Fatalf("outcome = %v", result.Outcome)
	}
	if !strings.Contains(result.Detail, "4002") || !strings.Contains(result.Detail, "session_id") {
		t.Fatalf("detail = %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "will fail the same way") {
		t.Fatalf("detail = %q; it must say the attach is not worth retrying", result.Detail)
	}
	if result.AuthRejected {
		t.Fatal("a provisioning refusal must not drop the client: it is cached per settings revision")
	}
}

// --- audio evidence ---------------------------------------------------------

// logSink captures the turn's log lines for assertions.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logSink) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// TestEndpointerAccumulatesIntegratedRMS pins that the gate that already reads
// every sample also reports the turn's integrated level: a full-scale square
// wave at 12000 computes analytically, and mixing in silence lowers the
// integrated level by 3 dB instead of resetting it.
func TestEndpointerAccumulatesIntegratedRMS(t *testing.T) {
	loudDB := 20 * math.Log10(12000.0/32768.0)

	loud := newEndpointer(time.Second)
	loud.frame(loudFrame(), time.Now())
	if got := loud.rmsDBFS(); math.Abs(got-loudDB) > 0.05 {
		t.Fatalf("loud rms = %.2f dBFS, want %.2f", got, loudDB)
	}

	silent := newEndpointer(time.Second)
	silent.frame(make([]byte, vkclient.FrameBytes), time.Now())
	if got := silent.rmsDBFS(); got != -120 {
		t.Fatalf("digital silence rms = %.2f dBFS, want the -120 floor", got)
	}

	mixed := newEndpointer(time.Second)
	mixed.frame(loudFrame(), time.Now())
	mixed.frame(make([]byte, vkclient.FrameBytes), time.Now())
	if got, want := mixed.rmsDBFS(), loudDB-3.0103; math.Abs(got-want) > 0.05 {
		t.Fatalf("loud+silence rms = %.2f dBFS, want %.2f; energy must integrate, not reset", got, want)
	}

	empty := newEndpointer(time.Second)
	if got := empty.rmsDBFS(); got != -120 {
		t.Fatalf("no samples rms = %.2f dBFS, want the -120 floor", got)
	}
}

// TestTurnLogsAudioSummary pins the one line that answers "did audio reach the
// service?": uploaded chunk/ms accounting from the stream, the integrated RMS,
// the pre-open loss count and the service's own status counters.
func TestTurnLogsAudioSummary(t *testing.T) {
	var stream *fakeStream
	stream = newFakeStream(func() {
		stream.events <- &vkclient.Event{Kind: vkclient.EventInit, PhraseID: "p-4"}
		stream.events <- &vkclient.Event{Kind: vkclient.EventStatus, Status: &vkclient.StreamStatus{
			ChunksReceived: 13, AudioReceivedMS: 1040, StatusNum: 7,
		}}
		stream.events <- &vkclient.Event{Kind: vkclient.EventFinish, Finish: vkclient.FinishResult}
		stream.events <- &vkclient.Event{Kind: vkclient.EventEOS}
	})
	logs := &logSink{}
	chunks := make(chan []byte, 8)
	for range 3 {
		chunks <- loudFrame()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := Run(ctx, Options{
		Endpoint:     &fakeEndpoint{stream: stream},
		Chunks:       chunks,
		Player:       &fakePlayer{},
		Logf:         logs.logf,
		EndSilence:   40 * time.Millisecond,
		PreOpenDrops: func() int { return 7 },
	})
	if result.Outcome != NoAnswer {
		t.Fatalf("outcome = %v (%s)", result.Outcome, result.Detail)
	}
	if result.Audio.ChunksUploaded != 3 || result.Audio.AudioMS != 240 {
		t.Fatalf("uploaded = %d chunks / %d ms, want 3 / 240", result.Audio.ChunksUploaded, result.Audio.AudioMS)
	}
	if result.Audio.PreOpenDropped != 7 {
		t.Fatalf("pre-open dropped = %d, want 7", result.Audio.PreOpenDropped)
	}
	if !result.Audio.StatusSeen || result.Audio.StatusFrames != 1 {
		t.Fatalf("status = %+v, want one counter frame", result.Audio)
	}
	if got := result.Audio.ServiceStatus; got.ChunksReceived != 13 ||
		got.AudioReceivedMS != 1040 || got.StatusNum != 7 {
		t.Fatalf("service status = %+v", got)
	}
	if math.Abs(result.Audio.RMSDBFS+8.72) > 0.1 {
		t.Fatalf("rms = %.2f dBFS, want about -8.72", result.Audio.RMSDBFS)
	}
	output := logs.text()
	want := "turn audio uploaded_chunks=3 uploaded_ms=240 rms_dbfs=-8.7 pre_open_dropped=7 " +
		"service_chunks_received=13 service_audio_received_ms=1040 service_status_num=7 status_frames=1"
	if !strings.Contains(output, want) {
		t.Fatalf("summary line missing %q:\n%s", want, output)
	}
	if strings.Contains(output, "WARN") {
		t.Fatalf("a loud turn must not warn:\n%s", output)
	}
}

// TestTurnNoAudioOutcome covers both ways a clean terminal frame can mean "no
// audio": captured chunks that are digital silence, and a turn that never
// uploaded a chunk at all. Neither may be reported as a recognition no-answer.
func TestTurnNoAudioOutcome(t *testing.T) {
	t.Run("silent capture", func(t *testing.T) {
		stream := newFakeStream(nil)
		stream.onSend = func(n int32) {
			if n != 3 {
				return
			}
			stream.events <- &vkclient.Event{Kind: vkclient.EventFinish, Finish: vkclient.FinishResult}
			stream.events <- &vkclient.Event{Kind: vkclient.EventEOS}
		}
		logs := &logSink{}
		chunks := make(chan []byte, 4)
		for range 3 {
			chunks <- make([]byte, vkclient.FrameBytes)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := Run(ctx, Options{
			Endpoint: &fakeEndpoint{stream: stream},
			Chunks:   chunks,
			Player:   &fakePlayer{},
			Logf:     logs.logf,
		})
		if result.Outcome != NoAudio {
			t.Fatalf("outcome = %v (%s), want no audio", result.Outcome, result.Detail)
		}
		if !strings.Contains(result.Detail, "captured only silence") {
			t.Fatalf("detail = %q; it must name silent capture", result.Detail)
		}
		if !strings.Contains(logs.text(), "WARN the turn captured silence") {
			t.Fatalf("missing the silence warning:\n%s", logs.text())
		}
	})

	t.Run("nothing uploaded", func(t *testing.T) {
		stream := newFakeStream(nil)
		stream.events <- &vkclient.Event{Kind: vkclient.EventFinish, Finish: vkclient.FinishResult}
		stream.events <- &vkclient.Event{Kind: vkclient.EventEOS}
		logs := &logSink{}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := Run(ctx, Options{
			Endpoint: &fakeEndpoint{stream: stream},
			Chunks:   make(chan []byte),
			Player:   &fakePlayer{},
			Logf:     logs.logf,
		})
		if result.Outcome != NoAudio {
			t.Fatalf("outcome = %v (%s), want no audio", result.Outcome, result.Detail)
		}
		if !strings.Contains(result.Detail, "no audio reached the service") {
			t.Fatalf("detail = %q; it must name missing upload", result.Detail)
		}
		if !strings.Contains(logs.text(), "WARN no audio was uploaded") {
			t.Fatalf("missing the no-upload warning:\n%s", logs.text())
		}
	})
}
