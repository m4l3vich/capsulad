package vkclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"capsula/apps/marusia/internal/vkproto/clientpb"
	commandspb "capsula/apps/marusia/internal/vkproto/commandspb"
	"capsula/apps/marusia/internal/vkproto/optionspb"
	"capsula/apps/marusia/internal/vkproto/phrasepb"
	"capsula/apps/marusia/internal/vkproto/servicepb"
)

const (
	// SampleRate is the capture and upload rate: 16 kHz mono s16le, proven by
	// the vendor's 16000 comparison and mono check.
	SampleRate = 16000
	// FrameMS is the server's accepted phrase chunk duration from live init QoS.
	FrameMS = 80
	// FrameSamples is 80 ms at 16 kHz.
	FrameSamples = SampleRate * FrameMS / 1000
	// FrameBytes is one mono s16le 80 ms frame.
	FrameBytes = FrameSamples * 2
)

// AudioFormat is the upload framing advertised in
// SpeechRequestInit.audio_options.format.
type AudioFormat int

const (
	// FormatWave sends one RIFF/WAVE header followed by raw s16le frames.
	FormatWave AudioFormat = iota
	// FormatOggOpus is the vendor's preferred framing. This build has no Opus
	// encoder, so OpenSpeech refuses it with ErrUnsupportedFormat.
	FormatOggOpus
)

func (f AudioFormat) String() string {
	switch f {
	case FormatWave:
		return "wave"
	case FormatOggOpus:
		return "ogg_opus"
	default:
		return fmt.Sprintf("audio_format(%d)", int(f))
	}
}

func (f AudioFormat) proto() optionspb.RequestAudioOptions_AudioFormat {
	switch f {
	case FormatOggOpus:
		return optionspb.RequestAudioOptions_AUDIO_FORMAT_OGG_OPUS
	default:
		return optionspb.RequestAudioOptions_AUDIO_FORMAT_WAVE
	}
}

// ParseAudioFormat accepts the manifest setting vocabulary.
func ParseAudioFormat(text string) (AudioFormat, error) {
	switch text {
	case "", "wave", "wav":
		return FormatWave, nil
	case "ogg_opus", "opus":
		return FormatOggOpus, nil
	default:
		return 0, fmt.Errorf("vkclient: unknown audio format %q (want wave or ogg_opus)", text)
	}
}

// SpeechOptions carries the per-turn init fields this app fills.
type SpeechOptions struct {
	Format         AudioFormat
	Timezone       string
	ActivationType string
	Keyword        string
	HWID           string
	// MediaState is required inside keyword_params: the service rejects an init
	// whose kw_params omits it with "unknown media state".
	MediaState clientpb.MediaState
	Brand      string
	// StatusIntervalMS is the requested status cadence. The vendor sets the
	// field but its constant was not recovered, so this is an inference.
	StatusIntervalMS int32
}

// BuildInit assembles the SpeechRequestInit the vendor's packed-init builder
// produces: audio framing, device info, the Capsula client state with the
// microphone on, GENERAL mode and the keyword parameters.
func BuildInit(opts SpeechOptions, serial, version string) *phrasepb.SpeechRequestInit {
	init := &phrasepb.SpeechRequestInit{
		AudioOptions: &optionspb.RequestAudioOptions{Format: opts.Format.proto()},
		ClientOptions: &optionspb.RequestClientOptions{
			DeviceInfo: &clientpb.DeviceInfo{
				Serial:  serial,
				Version: version,
				Brand:   opts.Brand,
			},
			State: &clientpb.ClientState{
				State: &clientpb.PlatformSpecific{
					PlatformState: &clientpb.PlatformSpecific_CapsulaState{
						CapsulaState: &clientpb.PlatformSpecific_Capsula{MicOn: true},
					},
				},
			},
			Timezone:       opts.Timezone,
			ActivationType: opts.ActivationType,
		},
		Mode: phrasepb.SpeechRequestInit_SPEECH_MODE_GENERAL,
	}
	if opts.StatusIntervalMS > 0 {
		init.QosOptions = &phrasepb.SpeechRequestQoSOptions{StatusIntervalMs: opts.StatusIntervalMS}
	}
	// The variant is always present: the service answers an absent one with
	// "Invalid 'kw_type' param: unknown keyword type" and drops the phrase
	// before any audio is transcribed. A turn without keyword evidence states
	// no_kw explicitly.
	init.KeywordOptions = &optionspb.RequestKeywordOptions{}
	if opts.Keyword == "" {
		init.KeywordOptions.KwVariant = &optionspb.RequestKeywordOptions_NoKw{
			NoKw: &optionspb.RequestKeywordOptions_NoKeyword{},
		}
	} else {
		state := opts.MediaState
		if state == clientpb.MediaState_MEDIA_STATE_UNSPECIFIED {
			// The service refuses an unspecified media state; "stopped" is the
			// truthful value while a phrase is being captured.
			state = clientpb.MediaState_MEDIA_STATE_STOP
		}
		init.KeywordOptions.KwVariant = &optionspb.RequestKeywordOptions_KwParams{
			KwParams: &optionspb.RequestKeywordOptions_KeywordParams{
				Keyword:    opts.Keyword,
				HwId:       opts.HWID,
				MediaState: state,
			},
		}
	}
	return init
}

// StreamStatus is one SpeechResponseStatus frame.
type StreamStatus struct {
	ChunksReceived      int32
	AudioReceivedMS     int32
	LastChunkReceivedTS int64
	StatusNum           int32
}

// FinishKind is the terminal SpeechResponseFinish continuation.
type FinishKind int

const (
	FinishUnknown FinishKind = iota
	FinishAbort
	FinishNoop
	FinishResult
)

func (f FinishKind) String() string {
	switch f {
	case FinishAbort:
		return "abort"
	case FinishNoop:
		return "noop"
	case FinishResult:
		return "result"
	default:
		return "unspecified"
	}
}

// EventKind identifies one decoded server frame.
type EventKind int

const (
	EventUnknown EventKind = iota
	EventInit
	EventEOS
	EventStatus
	EventKeywordConfirmed
	EventTextSoFar
	EventCommands
	EventFinish
)

// Event is one decoded SpeechResponse. Only the fields relevant to Kind are
// populated.
type Event struct {
	Kind EventKind

	// EventInit
	PhraseID          string
	ChunkLengthHintMS int32
	StatusIntervalMS  int32
	ChunksOverflowOK  bool
	// EventStatus
	Status *StreamStatus
	// EventTextSoFar
	Text string
	// EventCommands
	Commands []Command
	// EventFinish
	Finish      FinishKind
	AbortReason string
	// SendTSMS is the server timestamp carried by every frame.
	SendTSMS int64
}

// CommandKind identifies a decoded command payload.
type CommandKind int

const (
	CommandUnknown CommandKind = iota
	CommandPlayURL
	CommandPlayURLsPreload
	CommandBgMusic
	CommandBgMusicPreload
	CommandShowText
	CommandSendEvent
)

func (k CommandKind) String() string {
	switch k {
	case CommandPlayURL:
		return "play_url"
	case CommandPlayURLsPreload:
		return "play_urls_preload"
	case CommandBgMusic:
		return "bg_music"
	case CommandBgMusicPreload:
		return "bg_music_preload"
	case CommandShowText:
		return "show_text"
	case CommandSendEvent:
		return "send_event"
	default:
		return "unknown"
	}
}

// Command is one decoded google.protobuf.Any command payload.
type Command struct {
	Kind         CommandKind
	TypeURL      string
	URL          string
	URLs         []string
	Text         string
	Event        string
	CallbackData []byte
}

// Speech is one bidirectional Phrase/Speech stream. Send, EOS, Abort and
// Recv may be called from different goroutines: sends are serialized here and
// gRPC reads are owned by the caller's Recv loop.
type Speech struct {
	stream servicepb.Phrase_SpeechClient
	cancel context.CancelFunc
	op     string

	mu        sync.Mutex
	sentInit  bool
	sentEOS   bool
	sendErr   error
	chunkNum  int32
	upload    *frameUploader
	closeOnce sync.Once
}

// OpenSpeech opens the stream and sends init. The stream lifetime is the
// returned Speech plus ctx; Close tears it down.
func (c *Client) OpenSpeech(ctx context.Context, opts SpeechOptions) (*Speech, error) {
	if opts.Format == FormatOggOpus {
		return nil, fmt.Errorf("%w: this build has no Opus encoder", ErrUnsupportedFormat)
	}
	conn, err := c.channel()
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	streamCtx = metadata.NewOutgoingContext(streamCtx, c.callMetadata())
	stream, err := servicepb.NewPhraseClient(conn).Speech(streamCtx)
	if err != nil {
		cancel()
		return nil, wrapStatus("speech open", err)
	}

	s := &Speech{stream: stream, cancel: cancel, op: "speech", upload: newFrameUploader(opts.Format)}
	init := BuildInit(opts, c.cfg.Serial, c.cfg.ClientVersion)
	if err := stream.Send(&phrasepb.SpeechRequest{
		Kind: &phrasepb.SpeechRequest_Init{Init: init},
	}); err != nil {
		cancel()
		return nil, wrapStatus("speech init", err)
	}
	s.sentInit = true
	return s, nil
}

// SendAudio frames pcm into FrameMS (80 ms) blocks and sends one chunk per
// block. On the WAVE format every chunk is a self-contained WAV file with real
// sizes, which is what the service repacks.
// It returns an error once the stream is finished or after a failed send.
func (s *Speech) SendAudio(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	if s.sentEOS {
		return errors.New("vkclient: audio after eos")
	}
	for _, block := range s.upload.push(pcm) {
		req := &phrasepb.SpeechRequest{
			Kind: &phrasepb.SpeechRequest_Chunk{
				Chunk: &phrasepb.SpeechRequestChunk{ChunkNum: s.chunkNum, Data: block},
			},
		}
		s.chunkNum++
		if err := s.stream.Send(req); err != nil {
			s.sendErr = wrapStatus("speech chunk", err)
			return s.sendErr
		}
	}
	return nil
}

// EOS sends the end-of-stream frame. Data still buffered below one FrameMS
// block is flushed first.
//
// The request side is deliberately left open: closing it immediately after the
// eos frame makes the live service end the response stream with
// "Internal server error" instead of the clean terminal frames, and any frame
// that follows eos would be lost. The server closes the stream itself once the
// phrase result is delivered; Close still releases it on the local side.
func (s *Speech) EOS() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	if s.sentEOS {
		return nil
	}
	if tail := s.upload.flush(); tail != nil {
		req := &phrasepb.SpeechRequest{
			Kind: &phrasepb.SpeechRequest_Chunk{
				Chunk: &phrasepb.SpeechRequestChunk{ChunkNum: s.chunkNum, Data: tail},
			},
		}
		s.chunkNum++
		if err := s.stream.Send(req); err != nil {
			s.sendErr = wrapStatus("speech chunk", err)
			return s.sendErr
		}
	}
	if err := s.stream.Send(&phrasepb.SpeechRequest{
		Kind: &phrasepb.SpeechRequest_Eos{Eos: &phrasepb.SpeechRequestEos{}},
	}); err != nil {
		s.sendErr = wrapStatus("speech eos", err)
		return s.sendErr
	}
	s.sentEOS = true
	return nil
}

// Abort sends the client abort frame. The caller still owns teardown via
// Close, which cancels the stream and releases the channel resources.
func (s *Speech) Abort(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sentEOS || s.sendErr != nil {
		return s.sendErr
	}
	s.sentEOS = true
	err := s.stream.Send(&phrasepb.SpeechRequest{
		Kind: &phrasepb.SpeechRequest_Abort{Abort: &phrasepb.SpeechRequestAbort{Reason: reason}},
	})
	_ = s.stream.CloseSend()
	if err != nil {
		s.sendErr = wrapStatus("speech abort", err)
		return s.sendErr
	}
	return nil
}

// Recv reads and decodes one server frame. It returns io.EOF at a clean end of
// stream and a *StatusError for a non-OK status. A canceled context returns the
// context error.
func (s *Speech) Recv() (*Event, error) {
	resp, err := s.stream.Recv()
	if err != nil {
		return nil, wrapStatus(s.op+" recv", err)
	}
	return decodeResponse(resp)
}

// Cancel tears the stream down immediately, without sending an abort frame.
// Use Abort first when the server should observe the cancellation.
func (s *Speech) Cancel() {
	s.closeOnce.Do(func() { s.cancel() })
}

// Close cancels the stream. It is idempotent and safe after EOS.
func (s *Speech) Close() {
	s.Cancel()
}

// ChunksSent reports how many chunk frames were written. Used for diagnostics.
func (s *Speech) ChunksSent() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunkNum
}

// decodeResponse maps the recovered oneof onto Event.
func decodeResponse(resp *phrasepb.SpeechResponse) (*Event, error) {
	if resp == nil {
		return nil, errors.New("vkclient: empty SpeechResponse")
	}
	event := &Event{SendTSMS: resp.GetSendTsMs()}
	switch kind := resp.GetKind().(type) {
	case *phrasepb.SpeechResponse_Init:
		event.Kind = EventInit
		if init := kind.Init; init != nil {
			event.PhraseID = init.GetPhraseId()
			if qos := init.GetQos(); qos != nil {
				event.ChunkLengthHintMS = qos.GetChunkLengthMsHint()
				event.StatusIntervalMS = qos.GetStatusIntervalMs()
				event.ChunksOverflowOK = qos.GetChunksOverflowAcceptable()
			}
		}
	case *phrasepb.SpeechResponse_Eos:
		event.Kind = EventEOS
	case *phrasepb.SpeechResponse_Status:
		event.Kind = EventStatus
		if st := kind.Status; st != nil {
			event.Status = &StreamStatus{
				ChunksReceived:      st.GetChunksReceived(),
				AudioReceivedMS:     st.GetAudioReceivedMs(),
				LastChunkReceivedTS: st.GetLastChunkReceivedTsMs(),
				StatusNum:           st.GetStatusNum(),
			}
		}
	case *phrasepb.SpeechResponse_KeywordConfirmed:
		event.Kind = EventKeywordConfirmed
	case *phrasepb.SpeechResponse_TextSoFar:
		event.Kind = EventTextSoFar
		event.Text = kind.TextSoFar.GetText()
	case *phrasepb.SpeechResponse_Commands:
		event.Kind = EventCommands
		event.Commands = decodeCommands(kind.Commands)
	case *phrasepb.SpeechResponse_Finish:
		event.Kind = EventFinish
		if finish := kind.Finish; finish != nil {
			switch cont := finish.GetContinuation().(type) {
			case *phrasepb.SpeechResponseFinish_Abort:
				event.Finish = FinishAbort
				event.AbortReason = cont.Abort.GetReason()
			case *phrasepb.SpeechResponseFinish_Noop:
				event.Finish = FinishNoop
			case *phrasepb.SpeechResponseFinish_Result:
				event.Finish = FinishResult
			default:
				event.Finish = FinishUnknown
			}
		}
	default:
		event.Kind = EventUnknown
	}
	return event, nil
}

// decodeCommands unwraps the bursted Any payloads. The vendor labels commands
// with short names (play_url, bg_music, show_text, send_event); the shipped
// type URLs are the fully-qualified message names, so both spellings are
// accepted.
func decodeCommands(frame *phrasepb.SpeechResponseCommands) []Command {
	if frame == nil {
		return nil
	}
	var out []Command
	for _, bundle := range frame.GetBundles() {
		if bundle == nil {
			continue
		}
		for _, any := range bundle.GetCommands() {
			if any == nil {
				continue
			}
			cmd := Command{TypeURL: any.GetTypeUrl()}
			switch commandKind(any.GetTypeUrl()) {
			case CommandPlayURL:
				var msg commandspb.CommandPlayUrl
				if proto.Unmarshal(any.GetValue(), &msg) == nil {
					cmd.Kind = CommandPlayURL
					cmd.URL = msg.GetUrl()
				}
			case CommandPlayURLsPreload:
				var msg commandspb.CommandPlayUrlsPreload
				if proto.Unmarshal(any.GetValue(), &msg) == nil {
					cmd.Kind = CommandPlayURLsPreload
					cmd.URLs = msg.GetUrls()
				}
			case CommandBgMusic:
				var msg commandspb.CommandBgMusic
				if proto.Unmarshal(any.GetValue(), &msg) == nil {
					cmd.Kind = CommandBgMusic
					cmd.URL = msg.GetUrl()
				}
			case CommandBgMusicPreload:
				var msg commandspb.CommandBgMusicPreload
				if proto.Unmarshal(any.GetValue(), &msg) == nil {
					cmd.Kind = CommandBgMusicPreload
					cmd.URLs = msg.GetUrls()
				}
			case CommandShowText:
				var msg commandspb.CommandShowText
				if proto.Unmarshal(any.GetValue(), &msg) == nil {
					cmd.Kind = CommandShowText
					cmd.Text = msg.GetText()
				}
			case CommandSendEvent:
				var msg commandspb.CommandSendEvent
				if proto.Unmarshal(any.GetValue(), &msg) == nil {
					cmd.Kind = CommandSendEvent
					cmd.Event = msg.GetEvent()
					cmd.CallbackData = msg.GetCallbackData()
				}
			}
			out = append(out, cmd)
		}
	}
	return out
}

// commandKind resolves a type URL or a vendor short name. Unknown payloads are
// preserved as CommandUnknown so the turn can log them once and continue.
func commandKind(typeURL string) CommandKind {
	name := typeURL
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	// A fully-qualified type URL ends in pkg.Message; the vendor's short
	// labels ("play_url") carry no package at all.
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimPrefix(name, "Command")
	switch strings.ToLower(name) {
	case "playurl", "play_url":
		return CommandPlayURL
	case "playurlspreload", "play_urls_preload":
		return CommandPlayURLsPreload
	case "bgmusic", "bg_music":
		return CommandBgMusic
	case "bgmusicpreload", "bg_music_preload":
		return CommandBgMusicPreload
	case "showtext", "show_text":
		return CommandShowText
	case "sendevent", "send_event":
		return CommandSendEvent
	default:
		return CommandUnknown
	}
}
