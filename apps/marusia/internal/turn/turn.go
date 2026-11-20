// Package turn implements one Marusia voice turn: open the recovered
// Phrase/Speech stream, stream microphone PCM until the local endpointer ends
// speech, consume the server frames, and play the returned TTS URL.
//
// The framework-facing pieces (microphone, ring, speaker) and the network
// client arrive as narrow interfaces, so the ordering, cancellation and
// cleanup rules are testable without a daemon or a network.
package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"capsulad/apps/marusia/internal/redact"
	"capsulad/apps/marusia/internal/vkclient"
)

// State is the visible indication a turn asks for.
type State int

const (
	// StateListening is the required capture indication.
	StateListening State = iota
	// StateThinking covers authorization and waiting for the server.
	StateThinking
	// StateSpeaking is playback.
	StateSpeaking
	// StateError is a terminal failure; the app keeps it visible briefly.
	StateError
)

// Outcome classifies how a turn ended.
type Outcome int

const (
	// Completed means the server delivered a terminal frame.
	Completed Outcome = iota
	// Dismissed means the user or the app cancelled the turn.
	Dismissed
	// NoSpeech means the activation produced no speech before the deadline.
	NoSpeech
	// NoAudio means the turn ended without non-silent audio reaching the
	// service. It is distinct from NoAnswer: the service could not recognize
	// anything because there was nothing above the silence floor to recognize.
	NoAudio
	// Failed means authorization, transport, protocol or media failed.
	Failed
	// NoAnswer means the phrase finished cleanly but the service produced
	// neither recognized text nor a command to act on. It is not reported as
	// success: there was nothing to say or play.
	NoAnswer
)

func (o Outcome) String() string {
	switch o {
	case Completed:
		return "completed"
	case Dismissed:
		return "dismissed"
	case NoSpeech:
		return "no speech"
	case NoAudio:
		return "no audio"
	case Failed:
		return "failed"
	case NoAnswer:
		return "no answer"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// Result is the end of one turn.
type Result struct {
	Outcome Outcome
	// Detail is one actionable sentence, empty on success.
	Detail string
	// Text is the last text_so_far the server produced.
	Text string
	// PlayedURL is the TTS URL handed to the player.
	PlayedURL string
	// AuthRejected reports that the service refused the credentials during
	// this turn, so the caller must drop the session before the next one.
	AuthRejected bool
	// NoAnswer spells out the terminal frame the server did send, for the log.
	Finish string
	// Audio is this turn's capture and service acknowledgement summary.
	Audio AudioStats
}

// AudioStats is the per-turn audio evidence: how much PCM the uploader sent,
// its integrated level, capture lost before the phrase opened, and the last
// counter frame the service sent. It answers "did audio reach the service?"
// without a debug build and without touching the realtime capture path.
type AudioStats struct {
	// ChunksUploaded is the number of chunk frames the stream wrote, including
	// the zero-padded final block flushed by EOS.
	ChunksUploaded int32
	// AudioMS is ChunksUploaded * 80 ms, the duration the service should count.
	AudioMS int64
	// RMSDBFS is the integrated RMS of the uploaded PCM.
	RMSDBFS float64
	// PreOpenDropped counts capture chunks discarded before the phrase opened.
	PreOpenDropped int
	// ServiceStatus is the last SpeechResponseStatus frame; StatusSeen reports
	// whether the service sent one at all.
	ServiceStatus vkclient.StreamStatus
	StatusSeen    bool
	StatusFrames  int
}

// Silent reports whether nothing above the recognition floor was uploaded.
func (a AudioStats) Silent() bool { return a.RMSDBFS < SilenceFloorDBFS }

// Stream is one open Phrase/Speech stream.
type Stream interface {
	SendAudio(pcm []byte) error
	EOS() error
	Abort(reason string) error
	Recv() (*vkclient.Event, error)
	// Result fetches the commands stored for the phrase after the terminal
	// finish frame. The turn gives it its own deadline, separate from the
	// terminal-frame timer.
	Result(ctx context.Context) ([]*vkclient.Event, error)
	// ChunksSent reports the chunk frames written to the service, used for the
	// per-turn audio summary. It must be cheap and safe after EOS.
	ChunksSent() int32
	Close()
}

// Endpoint authorizes and opens speech streams.
type Endpoint interface {
	Authorize(ctx context.Context) error
	OpenSpeech(ctx context.Context, opts vkclient.SpeechOptions) (Stream, error)
}

// Player fetches and plays one TTS URL. It must not return until playback is
// complete or ctx is done.
type Player interface {
	Play(ctx context.Context, url string) error
}

// Indicator renders the visible turn state. Implementations own their own
// animation cadence; Show only changes the state.
type Indicator interface {
	Show(state State)
}

// Options are one turn's inputs. Zero durations use the documented defaults.
type Options struct {
	Endpoint   Endpoint
	Speech     vkclient.SpeechOptions
	Chunks     <-chan []byte
	Release    <-chan struct{}
	HoldToTalk bool
	// PhraseOpened is called when the server's phrase-init frame arrives.
	// Capture queues use this edge to distinguish opener-time audio loss from
	// ordinary upload-time backpressure.
	PhraseOpened func()
	// PreOpenDrops reports capture chunks discarded before the phrase opened.
	// It is called once when the turn ends and must not block; nil means none.
	PreOpenDrops func() int
	Player       Player
	Indicator    Indicator
	Logf         func(format string, args ...any)

	NoSpeechTimeout time.Duration
	MaxSpeech       time.Duration
	EndSilence      time.Duration
	TerminalTimeout time.Duration
	// ResultTimeout bounds the phrase-result fetch after the terminal finish.
	ResultTimeout time.Duration
}

const (
	defaultNoSpeech   = 6 * time.Second
	defaultMaxSpeech  = 15 * time.Second
	defaultEndSilence = 700 * time.Millisecond
	defaultTerminal   = 15 * time.Second
	defaultResult     = 10 * time.Second
)

// Run executes one turn. It always releases the stream before returning; the
// caller owns the microphone, presentation and conversation lifetimes.
func Run(ctx context.Context, opts Options) (result Result) {
	if opts.Endpoint == nil {
		return Result{Outcome: Failed, Detail: "internal error: no protocol endpoint"}
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	noSpeech := durationOr(opts.NoSpeechTimeout, defaultNoSpeech)
	maxSpeech := durationOr(opts.MaxSpeech, defaultMaxSpeech)
	endSilence := durationOr(opts.EndSilence, defaultEndSilence)
	terminalTimeout := durationOr(opts.TerminalTimeout, defaultTerminal)
	resultTimeout := durationOr(opts.ResultTimeout, defaultResult)

	show := func(state State) {
		if opts.Indicator != nil {
			opts.Indicator.Show(state)
		}
	}
	show(StateThinking)

	if err := opts.Endpoint.Authorize(ctx); err != nil {
		if ctx.Err() != nil {
			return Result{Outcome: Dismissed}
		}
		return Result{Outcome: Failed, Detail: describeAuth(err), AuthRejected: authRejected(err)}
	}
	if err := ctx.Err(); err != nil {
		return Result{Outcome: Dismissed}
	}

	stream, err := opts.Endpoint.OpenSpeech(ctx, opts.Speech)
	if err != nil {
		if ctx.Err() != nil {
			return Result{Outcome: Dismissed}
		}
		return Result{Outcome: Failed, Detail: describeStreamOpen(err), AuthRejected: authRejected(err)}
	}
	defer stream.Close()

	pointer := newEndpointer(endSilence)
	var (
		serviceStatus vkclient.StreamStatus
		statusSeen    bool
		statusFrames  int
	)
	defer func() {
		result = finishAudio(result, opts, logf, stream, pointer,
			serviceStatus, statusSeen, statusFrames)
	}()

	// One receiver goroutine owns Recv. Frames travel on one channel so a
	// recovered finish and a following EOF cannot be reordered: the turn sees
	// every event before the stream error that follows it.
	type recvFrame struct {
		event *vkclient.Event
		err   error
	}
	frames := make(chan recvFrame, 16)
	go func() {
		for {
			event, err := stream.Recv()
			if err != nil {
				select {
				case frames <- recvFrame{err: err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case frames <- recvFrame{event: event}:
			case <-ctx.Done():
				return
			}
		}
	}()

	show(StateListening)

	var (
		text             string
		playURL          string
		terminal         bool
		finish           string
		aborted          bool
		sending          = true
		terminalFrameErr string
		streamErr        string
		authLost         bool
		resultErr        string
		finishResult     bool
	)

	cancelTurn := func(reason string) {
		if aborted {
			return
		}
		aborted = true
		_ = stream.Abort(reason)
	}

	// finishSending sends eos once. It returns false only when the stream
	// itself failed; the caller then reports a transport failure.
	finishSending := func() bool {
		if !sending {
			return true
		}
		sending = false
		if err := stream.EOS(); err != nil {
			streamErr = fmt.Sprintf("the speech upload failed while ending capture: %v", err)
			return false
		}
		return true
	}

	handleEvent := func(event *vkclient.Event) {
		if event == nil {
			return
		}
		switch event.Kind {
		case vkclient.EventInit:
			if opts.PhraseOpened != nil {
				opts.PhraseOpened()
			}
			logf("marusia: phrase %q opened (chunk hint %d ms)", event.PhraseID, event.ChunkLengthHintMS)
			if event.ChunkLengthHintMS > 0 && event.ChunkLengthHintMS != vkclient.FrameMS {
				logf("marusia: server chunk hint %d ms differs from the fixed %d ms upload framing",
					event.ChunkLengthHintMS, vkclient.FrameMS)
			}
		case vkclient.EventTextSoFar:
			text = event.Text
		case vkclient.EventCommands:
			for _, command := range event.Commands {
				switch command.Kind {
				case vkclient.CommandPlayURL:
					if command.URL != "" && playURL == "" {
						playURL = command.URL
					}
				case vkclient.CommandShowText:
					logf("marusia: server text: %s", command.Text)
				case vkclient.CommandSendEvent:
					logf("marusia: server asked for event %q; interactive skills are not supported", command.Event)
				default:
					logf("marusia: ignoring %s command (not supported by this build)", command.Kind)
				}
			}
		case vkclient.EventFinish:
			terminal = true
			finish = event.Finish.String()
			finishResult = event.Finish == vkclient.FinishResult
			if event.Finish == vkclient.FinishAbort {
				terminalFrameErr = event.AbortReason
				if terminalFrameErr == "" {
					terminalFrameErr = "server aborted the phrase"
				}
			}
		case vkclient.EventEOS:
			// The live service sends eos before the recovered text/commands
			// and the terminal finish. It acknowledges the end of the upload,
			// not the end of the turn; stopping here would skip the result.
			logf("marusia: server end-of-stream frame")
		case vkclient.EventStatus:
			statusFrames++
			if event.Status != nil {
				serviceStatus = *event.Status
				statusSeen = true
			}
		case vkclient.EventKeywordConfirmed:
			// Confirmation only changes what the vendor server will do
			// with the audio; it is not a counter this turn needs.
		case vkclient.EventUnknown:
			logf("marusia: ignoring an unknown server frame")
		}
		if terminal {
			sending = false
		}
	}

	turnStart := time.Now()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var terminalDeadline time.Time

	for !terminal || sending {
		select {
		case <-ctx.Done():
			cancelTurn("dismissed")
			if text != "" {
				logf("marusia: turn dismissed after partial text %q", text)
			}
			return Result{Outcome: Dismissed, Text: text, PlayedURL: playURL}

		case frame := <-frames:
			if frame.err != nil {
				if terminal {
					continue
				}
				if isCancel(frame.err) {
					return Result{Outcome: Dismissed, Text: text, PlayedURL: playURL}
				}
				rejected := authRejected(frame.err)
				streamErr = fmt.Sprintf("speech stream ended before a terminal frame: %v", frame.err)
				terminal = true
				sending = false
				authLost = authLost || rejected
				continue
			}
			handleEvent(frame.event)

		case <-opts.Release:
			// A release is one edge. Clearing the local copy disarms the arm:
			// the closed channel would otherwise stay ready and the select
			// would spin on it for the rest of the turn.
			opts.Release = nil
			if !opts.HoldToTalk || terminal || !sending {
				continue
			}
			if !finishSending() {
				return Result{Outcome: Failed, Detail: streamErr, Text: text, PlayedURL: playURL}
			}
			terminalDeadline = time.Now().Add(terminalTimeout)
			logf("marusia: hold-to-talk released; end of speech sent")

		case chunk, ok := <-opts.Chunks:
			if !ok {
				// The capture stream closing is end of audio, not a failure: in
				// hold-to-talk the framework closes it when the key is released,
				// which is exactly the end-of-speech edge the service needs. Send
				// eos and keep waiting for the terminal frame.
				if terminal || !sending {
					opts.Chunks = nil
					continue
				}
				if !finishSending() {
					return Result{Outcome: Failed, Detail: streamErr, Text: text, PlayedURL: playURL}
				}
				terminalDeadline = time.Now().Add(terminalTimeout)
				logf("marusia: microphone ended; end of speech sent")
				opts.Chunks = nil
				continue
			}
			if len(chunk) == 0 {
				continue
			}
			if !sending {
				// The server already ended the phrase; drain without uploading.
				continue
			}
			if err := stream.SendAudio(chunk); err != nil {
				return Result{Outcome: Failed,
					Detail: fmt.Sprintf("microphone upload failed: %v", err), Text: text, PlayedURL: playURL}
			}
			pointer.frame(chunk, time.Now())

		case <-ticker.C:
			now := time.Now()
			if sending && !terminal {
				state := pointer.state(now)
				switch {
				case state.endOfSpeech:
					if !finishSending() {
						return Result{Outcome: Failed, Detail: streamErr, Text: text, PlayedURL: playURL}
					}
					terminalDeadline = now.Add(terminalTimeout)
					logf("marusia: end of speech after %s of audio", state.elapsed)
				case !state.speechSeen && now.Sub(turnStart) >= noSpeech:
					cancelTurn("no speech")
					return Result{Outcome: NoSpeech,
						Detail: fmt.Sprintf("no speech detected within %s", noSpeech),
						Text:   text, PlayedURL: playURL}
				case now.Sub(turnStart) >= maxSpeech:
					if !finishSending() {
						return Result{Outcome: Failed, Detail: streamErr, Text: text, PlayedURL: playURL}
					}
					terminalDeadline = now.Add(terminalTimeout)
					logf("marusia: speech reached the %s capture limit; end of speech sent", maxSpeech)
				}
			}
			if !terminalDeadline.IsZero() && !terminal && now.After(terminalDeadline) {
				cancelTurn("terminal timeout")
				return Result{Outcome: Failed,
					Detail: fmt.Sprintf("server sent no terminal frame within %s", terminalTimeout),
					Text:   text, PlayedURL: playURL}
			}
		}
	}

	// The phrase is over: release the gRPC stream before touching media so a
	// slow download cannot keep the speech connection alive.
	stream.Close()

	// A result finish only announces that the phrase is complete. Fetch the
	// stored commands under a deadline of their own, tied to turn
	// cancellation, so a slow result request cannot be mistaken for a missing
	// terminal frame.
	if finishResult && !aborted {
		if ctx.Err() != nil {
			return Result{Outcome: Dismissed, Text: text, PlayedURL: playURL}
		}
		resultCtx, cancelResult := context.WithTimeout(ctx, resultTimeout)
		recovered, err := stream.Result(resultCtx)
		cancelResult()
		if err != nil {
			if ctx.Err() != nil {
				return Result{Outcome: Dismissed, Text: text, PlayedURL: playURL}
			}
			resultErr = err.Error()
			logf("marusia: phrase result unavailable: %s", resultErr)
		}
		for _, event := range recovered {
			handleEvent(event)
		}
	}

	if streamErr != "" {
		return Result{Outcome: Failed, Detail: streamErr, Text: text, PlayedURL: playURL, AuthRejected: authLost}
	}
	if terminalFrameErr != "" {
		return Result{Outcome: Failed, Detail: "server aborted the phrase: " + terminalFrameErr,
			Text: text, PlayedURL: playURL, Finish: finish}
	}
	if playURL == "" {
		if text == "" {
			// A clean terminal frame with no text and no command is the
			// service declining to answer. Reporting it as success would
			// claim a turn that said nothing. The init facts are included
			// because "no text" is the symptom of an init the service would
			// not recognize: the keyword policy and the capability payload
			// this turn actually sent. A result-fetch failure is reported as
			// such: without the result body the turn cannot claim recognition
			// produced nothing.
			detail := fmt.Sprintf("the service returned no answer: recognition produced no text and no command "+
				"(keyword_mode=%s, capabilities=%dB)", opts.Speech.KeywordMode(), effectiveCapabilitiesLen(opts.Speech))
			if resultErr != "" {
				detail = fmt.Sprintf("the service returned no answer: the phrase result could not be fetched "+
					"(keyword_mode=%s, capabilities=%dB; result fetch: %s)",
					opts.Speech.KeywordMode(), effectiveCapabilitiesLen(opts.Speech), resultErr)
			}
			if finish != "" {
				detail += " (terminal frame: " + finish + ")"
			}
			logf("marusia: %s", detail)
			return Result{Outcome: NoAnswer, Detail: detail, Finish: finish}
		}
		logf("marusia: phrase completed with text %q but no play_url; nothing to play", text)
		return Result{Outcome: Completed, Text: text, Finish: finish}
	}
	if opts.Player == nil {
		return Result{Outcome: Failed, Detail: "internal error: no media player", Text: text, PlayedURL: playURL}
	}
	show(StateSpeaking)
	logf("marusia: playing the reply URL")
	if err := opts.Player.Play(ctx, playURL); err != nil {
		if ctx.Err() != nil {
			// Dismissal during playback is a clean terminal path, not a
			// media failure: the caller cancelled the turn.
			return Result{Outcome: Dismissed, Text: text, PlayedURL: playURL}
		}
		return Result{Outcome: Failed,
			// The reply URL is signed in its query string, and the Detail is
			// logged, so neither the URL nor an error that quotes it may
			// carry the signature here.
			Detail: fmt.Sprintf("reply audio from %s could not be played: %s",
				redact.URL(playURL), redact.ErrorText(err, playURL)),
			Text: text, PlayedURL: playURL}
	}
	return Result{Outcome: Completed, Text: text, PlayedURL: playURL}
}

// finishAudio assembles the one-line-per-turn audio evidence and converts a
// clean no-answer into no-audio when nothing above the silence floor was
// uploaded. It runs in the turn goroutine after the stream has stopped sending,
// so the counters are stable; ChunksSent takes the stream's own lock.
func finishAudio(result Result, opts Options, logf func(string, ...any),
	stream Stream, pointer *endpointer,
	status vkclient.StreamStatus, statusSeen bool, statusFrames int) Result {
	preOpen := 0
	if opts.PreOpenDrops != nil {
		preOpen = opts.PreOpenDrops()
	}
	stats := AudioStats{
		ChunksUploaded: stream.ChunksSent(),
		RMSDBFS:        pointer.rmsDBFS(),
		PreOpenDropped: preOpen,
		ServiceStatus:  status,
		StatusSeen:     statusSeen,
		StatusFrames:   statusFrames,
	}
	stats.AudioMS = int64(stats.ChunksUploaded) * vkclient.FrameMS
	result.Audio = stats

	chunksReceived, audioReceived, statusNum := int32(-1), int32(-1), int32(-1)
	if statusSeen {
		chunksReceived, audioReceived, statusNum = status.ChunksReceived, status.AudioReceivedMS, status.StatusNum
	}
	logf("marusia: turn audio uploaded_chunks=%d uploaded_ms=%d rms_dbfs=%.1f pre_open_dropped=%d "+
		"service_chunks_received=%d service_audio_received_ms=%d service_status_num=%d status_frames=%d",
		stats.ChunksUploaded, stats.AudioMS, stats.RMSDBFS, stats.PreOpenDropped,
		chunksReceived, audioReceived, statusNum, stats.StatusFrames)

	if !stats.Silent() || result.Outcome == Dismissed {
		return result
	}
	if stats.ChunksUploaded == 0 {
		logf("marusia: WARN no audio was uploaded before the turn ended: 0 chunk(s) "+
			"reached the service (pre-open dropped %d)", stats.PreOpenDropped)
	} else {
		logf("marusia: WARN the turn captured silence: rms %.1f dBFS is below the %.0f dBFS floor "+
			"across %d uploaded chunk(s) (pre-open dropped %d)",
			stats.RMSDBFS, SilenceFloorDBFS, stats.ChunksUploaded, stats.PreOpenDropped)
	}
	if result.Outcome == NoAnswer {
		result.Outcome = NoAudio
		if stats.ChunksUploaded == 0 {
			result.Detail = "no audio reached the service: no capture chunk was uploaded before the turn ended"
		} else {
			result.Detail = fmt.Sprintf("the microphone captured only silence: integrated RMS %.1f dBFS "+
				"is below the %.0f dBFS recognition floor", stats.RMSDBFS, SilenceFloorDBFS)
		}
	}
	return result
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func describeAuth(err error) string {
	var attachErr *vkclient.AttachError
	switch {
	case vkclient.ProvisioningRequired(err):
		return "this device holds no provisioning credential: the service answered 4002 " +
			"(xtoken_id is required). Supply the session_id and session_secret settings " +
			"(see the README) or an xtoken_id; attaching again will fail the same way"
	case errors.As(err, &attachErr):
		detail := fmt.Sprintf("the service rejected the session attach with HTTP %d", attachErr.Status)
		if attachErr.Body != "" {
			detail += ": " + attachErr.Body
		}
		return detail + "; correct xtoken_id/endpoint and retry with the reauth action"
	case vkclient.IsCode(err, vkclient.CodeUnauthenticated), vkclient.IsCode(err, vkclient.CodePermissionDenied):
		return "the speech service rejected the session credential: " +
			"reload the vendor session file or configure session_id/session_secret"
	case vkclient.IsCode(err, vkclient.CodeInvalidArgument) && strings.Contains(err.Error(), "Invalid 'x-device-sn'"):
		return "the device serial is missing or invalid; configure the serial setting (or make /proc/cpuinfo readable)"
	default:
		return "could not authorize with the voice service: " + err.Error()
	}
}

func describeStreamOpen(err error) string {
	if errors.Is(err, vkclient.ErrUnsupportedFormat) {
		return err.Error() + "; set audio_format=wave"
	}
	return "could not open the speech stream: " + err.Error()
}

func isCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// authRejected reports whether the service refused the installed session: gRPC
// UNAUTHENTICATED/PERMISSION_DENIED (HTTP 401/403 on the wire). Only that means
// the session is dead and must be re-resolved. A provisioning refusal (4002) is
// deliberately not included: the client caches it for the current settings
// revision, so the turn fails once per revision instead of retrying the attach.
func authRejected(err error) bool {
	return vkclient.IsCode(err, vkclient.CodeUnauthenticated) || vkclient.IsCode(err, vkclient.CodePermissionDenied)
}

// effectiveCapabilitiesLen reports the capability bytes the init will carry.
// BuildInit substitutes the model default for an empty payload, so an empty
// option set still sends one; the log must not claim zero when it does not.
func effectiveCapabilitiesLen(speech vkclient.SpeechOptions) int {
	if len(speech.CapabilitiesJSON) > 0 {
		return len(speech.CapabilitiesJSON)
	}
	return len(vkclient.DefaultCapabilitiesJSON)
}
