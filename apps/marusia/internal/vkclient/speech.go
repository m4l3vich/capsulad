package vkclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"capsulad/apps/marusia/internal/vkproto/clientpb"
	commandspb "capsulad/apps/marusia/internal/vkproto/commandspb"
	"capsulad/apps/marusia/internal/vkproto/optionspb"
	"capsulad/apps/marusia/internal/vkproto/phrasepb"
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

// DefaultCapabilitiesHex is the capability token the shipped voice agent sent
// on every vendored HTTP control call, recovered from the device userdata
// image (_capsula-mini-rockchip-backup/11_userdata.img, the rt_logs entries for
// /connect):
//
//	capabilities_hex=31c0255a004201d1408a235de880400000
//
// The vendor builds it with sprintf("%llx%016llx%016llx", ...) over a
// capability bitmask (voice-agent offset 0x12fbb0), so the value is a
// per-firmware feature set, not a per-unit credential. This is the token of the
// Capsula Mini firmware 44.91 this app targets; another firmware can override
// it through the capabilities_json setting.
const DefaultCapabilitiesHex = "31c0255a004201d1408a235de880400000"

// DefaultCapabilitiesJSON is the model default payload for
// RequestClientOptions.capabilities.json.
//
// The recovered field is `bytes json = 1`; the vendor sets it present but the
// exact JSON schema was not recoverable from the binary. The only capability
// evidence the device leaks is the hex token above, so the default carries it
// under the same key the vendor uses on /connect. It is a documented model
// default, not a proof of the vendor's schema: an operator with the exact
// payload can override it with the capabilities_json setting.
var DefaultCapabilitiesJSON = []byte(`{"capabilities_hex":"` + DefaultCapabilitiesHex + `"}`)

// ValidateCapabilitiesJSON rejects a capabilities payload the app must not
// send: the init field is JSON, and the recovered token (when present) is hex.
// A non-object JSON document is accepted so an operator can paste the vendor's
// exact payload even if it is not shaped like the default.
func ValidateCapabilitiesJSON(payload []byte) error {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return errors.New("vkclient: capabilities payload is empty; the phrase init needs " +
			"RequestClientOptions.capabilities or the service has nothing to gate recognition on")
	}
	if !json.Valid(payload) {
		return errors.New("vkclient: capabilities payload is not valid JSON")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(payload, &object) != nil {
		return nil
	}
	raw, ok := object["capabilities_hex"]
	if !ok {
		return nil
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil {
		return errors.New("vkclient: capabilities_hex must be a JSON string")
	}
	if len(token)%2 != 0 {
		return errors.New("vkclient: capabilities_hex must have an even number of hex digits")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return errors.New("vkclient: capabilities_hex is not hex")
	}
	return nil
}

// ValidateClientDataJSON checks the optional RequestClientOptions.client_data
// payload. The message has the same shape as capabilities (bytes json = 1) but
// no recovered token of its own, so only its JSON syntax is checked. Empty means
// "send nothing".
func ValidateClientDataJSON(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if !json.Valid(payload) {
		return errors.New("vkclient: client_data payload is not valid JSON")
	}
	return nil
}

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
	// Keyword is the wake word name sent inside keyword_options.kw_params.
	Keyword string
	// KeywordInAudio states that the uploaded phrase begins with the wake
	// word. kw_params is sent only when it is true: the service confirms the
	// keyword from the phrase audio it receives, so a capture that starts at
	// the command — a touch or API activation, whose pre-roll is room audio
	// ahead of it, or a wake-word capture whose pre-roll never arrived — can
	// never be confirmed and the service ends the phrase with "keyword not
	// confirmed".
	KeywordInAudio bool
	HWID           string
	// MediaState is required inside keyword_params: the service rejects an init
	// whose kw_params omits it with "unknown media state".
	MediaState clientpb.MediaState
	Brand      string
	// CapabilitiesJSON is the payload of RequestClientOptions.capabilities
	// (clientpb.ClientCapabilities.json). The field is present in the vendor's
	// own init; a zero value falls back to DefaultCapabilitiesJSON so a caller
	// cannot silently open a phrase without it.
	CapabilitiesJSON []byte
	// ClientDataJSON is the payload of RequestClientOptions.client_data. Empty
	// leaves the field unset, which is what the vendor's own init does unless a
	// control-connection parameter supplies client data.
	ClientDataJSON []byte
	// StatusIntervalMS is the requested status cadence. The vendor sets the
	// field but its constant was not recovered, so this is an inference.
	StatusIntervalMS int32
}

// BuildInit assembles the SpeechRequestInit the vendor's packed-init builder
// produces: audio framing, the capability payload, device info, the Capsula
// client state with the microphone on, GENERAL mode and the keyword choice.
func BuildInit(opts SpeechOptions, serial, version string) *phrasepb.SpeechRequestInit {
	capabilities := opts.CapabilitiesJSON
	if len(capabilities) == 0 {
		capabilities = DefaultCapabilitiesJSON
	}
	clientOptions := &optionspb.RequestClientOptions{
		Capabilities: &clientpb.ClientCapabilities{Json: capabilities},
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
	}
	if len(opts.ClientDataJSON) > 0 {
		clientOptions.ClientData = &clientpb.ClientData{Json: opts.ClientDataJSON}
	}
	init := &phrasepb.SpeechRequestInit{
		AudioOptions:  &optionspb.RequestAudioOptions{Format: opts.Format.proto()},
		ClientOptions: clientOptions,
		Mode:          phrasepb.SpeechRequestInit_SPEECH_MODE_GENERAL,
	}
	if opts.StatusIntervalMS > 0 {
		init.QosOptions = &phrasepb.SpeechRequestQoSOptions{StatusIntervalMs: opts.StatusIntervalMS}
	}
	// The variant is always present: the service answers an absent one with
	// "Invalid 'kw_type' param: unknown keyword type" and drops the phrase
	// before any audio is transcribed. A turn whose audio does not contain the
	// wake word states no_kw explicitly.
	init.KeywordOptions = &optionspb.RequestKeywordOptions{}
	if opts.Keyword == "" || !opts.KeywordInAudio {
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

// KeywordMode names the keyword_options variant these options produce.
func (o SpeechOptions) KeywordMode() string {
	if o.Keyword == "" || !o.KeywordInAudio {
		return "no_kw"
	}
	return "kw_params"
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
	CommandListen
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
	case CommandListen:
		return "listen"
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
// stream reads are owned by the caller's Recv loop.
type Speech struct {
	stream Stream
	cancel context.CancelFunc
	op     string
	client *Client
	opts   SpeechOptions

	mu        sync.Mutex
	sentInit  bool
	sentEOS   bool
	sendErr   error
	chunkNum  int32
	upload    *frameUploader
	closeOnce sync.Once

	// phraseID arrives in SpeechResponseInit. The commands live behind
	// GET /phrase/result and are fetched by Result after the finish is
	// delivered to the turn.
	phraseID      string
	resultFetched bool
}

// OpenSpeech opens the stream and sends init. The stream lifetime is the
// returned Speech plus ctx; Close tears it down.
func (c *Client) OpenSpeech(ctx context.Context, opts SpeechOptions) (*Speech, error) {
	if opts.Format == FormatOggOpus {
		return nil, fmt.Errorf("%w: this build has no Opus encoder", ErrUnsupportedFormat)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.transport.NewStream(streamCtx, phraseService, "Speech",
		c.callMetadata(), CallBidi, 0)
	if err != nil {
		cancel()
		return nil, err
	}

	s := &Speech{
		stream: stream,
		cancel: cancel,
		op:     "speech",
		client: c,
		opts:   opts,
		upload: newFrameUploader(opts.Format),
	}
	init := BuildInit(opts, c.cfg.Serial, c.cfg.ClientVersion)
	if err := s.sendMessage(&phrasepb.SpeechRequest{
		Kind: &phrasepb.SpeechRequest_Init{Init: init},
	}); err != nil {
		cancel()
		_ = stream.Close()
		return nil, err
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
		if err := s.sendMessage(req); err != nil {
			s.sendErr = err
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
		if err := s.sendMessage(req); err != nil {
			s.sendErr = err
			return s.sendErr
		}
	}
	if err := s.sendMessage(&phrasepb.SpeechRequest{
		Kind: &phrasepb.SpeechRequest_Eos{Eos: &phrasepb.SpeechRequestEos{}},
	}); err != nil {
		s.sendErr = err
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
	err := s.sendMessage(&phrasepb.SpeechRequest{
		Kind: &phrasepb.SpeechRequest_Abort{Abort: &phrasepb.SpeechRequestAbort{Reason: reason}},
	})
	_ = s.stream.CloseSend()
	if err != nil {
		s.sendErr = err
		return s.sendErr
	}
	return nil
}

// Recv reads and decodes one server frame. It returns io.EOF at a clean end of
// stream and a *StatusError for a non-OK status. A canceled context returns the
// context error.
func (s *Speech) Recv() (*Event, error) {
	payload, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	resp := &phrasepb.SpeechResponse{}
	if err := proto.Unmarshal(payload, resp); err != nil {
		return nil, fmt.Errorf("vkclient: decode SpeechResponse: %w", err)
	}
	event, err := decodeResponse(resp)
	if err != nil {
		return nil, err
	}
	if event.Kind == EventInit {
		s.phraseID = event.PhraseID
	}
	return event, nil
}

// sendMessage marshals one request and writes it to the stream.
func (s *Speech) sendMessage(message *phrasepb.SpeechRequest) error {
	payload, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("vkclient: encode SpeechRequest: %w", err)
	}
	return s.stream.Send(payload)
}

// Cancel tears the stream down immediately, without sending an abort frame.
// Use Abort first when the server should observe the cancellation.
func (s *Speech) Cancel() {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.stream.Close()
	})
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
