package capsula

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	presentationService = "capsula.framework.v1.PresentationService"
	audioService        = "capsula.framework.v1.AudioService"
	assistantService    = "capsula.framework.v1.AssistantService"
	settingsService     = "capsula.framework.v1.SettingsService"

	presentationOpenTimeout = 5 * time.Second
	playbackDrainTimeout    = 30 * time.Second
)

// --- presentations ---------------------------------------------------------

type presentationReadyJSON struct {
	PresentationID string `json:"presentationId"`
	ResourceID     string `json:"resourceId"`
	HasValidFrame  bool   `json:"hasValidFrame"`
}

type presentationVisibilityJSON struct {
	Visible bool   `json:"visible"`
	Reason  string `json:"reason"`
}

type presentationOpenJSON struct {
	ResourceID string `json:"resourceId,omitempty"`
}

type presentationEventJSON struct {
	Ready      *presentationReadyJSON      `json:"ready"`
	Visibility *presentationVisibilityJSON `json:"visibility"`
}

// Presentation is one dynamic display/ring presentation.
type Presentation struct {
	resource string

	mu      sync.Mutex
	id      string
	visible bool

	call      *callHandle
	done      chan struct{}
	closeOnce sync.Once
}

// OpenPresentation creates a presentation on resourceID and waits for the
// framework's ready event.
func (c *Client) OpenPresentation(ctx context.Context, resourceID string) (*Presentation, error) {
	ctx = orBackground(ctx)
	call, err := c.startCall(ctx, "", presentationService, "Open", callKindBidi, 0)
	if err != nil {
		return nil, err
	}
	if err := call.sendJSON(presentationRequest{
		Open: &presentationOpenJSON{ResourceID: resourceID},
	}); err != nil {
		call.release()
		return nil, err
	}

	p := &Presentation{resource: resourceID, call: call, done: make(chan struct{})}
	deadline := time.Now().Add(presentationOpenTimeout)
	if ctx != nil {
		if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
			deadline = dl
		}
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			call.release()
			return nil, context.DeadlineExceeded
		}
		payload, err := call.recvJSONTimeout(timeoutMillis(context.Background(), remaining))
		if err != nil {
			call.release()
			return nil, err
		}
		var event presentationEventJSON
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			call.release()
			return nil, err
		}
		if event.Visibility != nil {
			p.mu.Lock()
			p.visible = event.Visibility.Visible
			p.mu.Unlock()
		}
		if event.Ready != nil {
			p.mu.Lock()
			p.id = event.Ready.PresentationID
			p.mu.Unlock()
			break
		}
	}
	go p.readLoop()
	return p, nil
}

func (p *Presentation) readLoop() {
	defer close(p.done)
	defer p.call.release()
	for {
		payload, err := p.call.recvJSON()
		if err != nil {
			return
		}
		var event presentationEventJSON
		if err := unmarshalProtoJSON(payload, &event); err != nil {
			continue
		}
		if event.Visibility != nil {
			p.mu.Lock()
			p.visible = event.Visibility.Visible
			p.mu.Unlock()
		}
	}
}

// ID is the runtime-assigned presentation ID.
func (p *Presentation) ID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.id
}

// Visible reports the last visibility event.
func (p *Presentation) Visible() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.visible
}

// Done closes when the presentation stream ends.
func (p *Presentation) Done() <-chan struct{} { return p.done }

func (p *Presentation) send(frame *presentationFrame) error {
	p.mu.Lock()
	call := p.call
	p.mu.Unlock()
	if call == nil {
		return ErrClosed
	}
	return call.sendJSON(presentationRequest{Frame: frame})
}

// Segment sends a segment frame. cells are per-digit masks; colon controls the
// separator.
func (p *Presentation) Segment(cells []uint16, colon bool) error {
	frame := &presentationFrame{Segment: &segmentFrame{Colon: colon}}
	for _, cell := range cells {
		frame.Segment.Cells = append(frame.Segment.Cells, segmentCell{Mask: uint32(cell)})
	}
	return p.send(frame)
}

// Ring sends one ring frame.
func (p *Presentation) Ring(pixels []RGB) error {
	return p.send(&presentationFrame{Ring: &ringFrame{Pixels: pixels}})
}

// Effect shows a built-in ring effect with named parameters.
func (p *Presentation) Effect(effectID string, params map[string]any) error {
	arguments, err := presetArgs(params)
	if err != nil {
		return err
	}
	return p.send(&presentationFrame{RingEffect: &ringEffectFrame{
		EffectID: effectID, Arguments: arguments,
	}})
}

// Face shows a built-in segment face with named parameters.
func (p *Presentation) Face(faceID string, params map[string]any) error {
	arguments, err := presetArgs(params)
	if err != nil {
		return err
	}
	return p.send(&presentationFrame{SegmentFace: &segmentFaceFrame{
		FaceID: faceID, Arguments: arguments,
	}})
}

// Blank explicitly blanks the presentation, which counts as valid content.
func (p *Presentation) Blank() error {
	return p.send(&presentationFrame{Blank: &blankFrame{}})
}

// Close asks the framework to close the presentation and half-closes the
// stream. It is idempotent.
func (p *Presentation) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if sendErr := p.call.sendJSON(presentationRequest{Close: &blankFrame{}}); sendErr != nil &&
			!errors.Is(sendErr, ErrClosed) {
			err = sendErr
		}
		if closeErr := p.call.halfClose(); closeErr != nil && err == nil &&
			!errors.Is(closeErr, ErrClosed) {
			err = closeErr
		}
	})
	return err
}

// --- preset discovery ------------------------------------------------------

// ListPresets discovers built-in effects and faces for a resource.
func (c *Client) ListPresets(ctx context.Context, resourceID string) ([]PresetInfo, error) {
	var response struct {
		Presets []PresetInfo `json:"presets"`
	}
	if err := c.unaryJSON(ctx, presentationService, "ListPresets", struct {
		ResourceID string `json:"resourceId,omitempty"`
	}{ResourceID: resourceID}, &response); err != nil {
		return nil, err
	}
	return response.Presets, nil
}

// --- playback --------------------------------------------------------------

// Playback is one independent PCM playback stream.
type Playback struct {
	call *callHandle
	done chan struct{}

	mu     sync.Mutex
	closed bool
	err    error
}

// OpenPlayback opens a playback stream. A zero AudioFormat selects the
// framework default (PCM s16le, 48000 Hz, stereo).
func (c *Client) OpenPlayback(ctx context.Context, endpoint string, format AudioFormat,
	conversationID string) (*Playback, error) {
	ctx = orBackground(ctx)
	client, err := c.clientHandle()
	if err != nil {
		return nil, err
	}
	if format.isZero() {
		format = DefaultAudioFormat()
	}
	handle, err := audioPlaybackOpenNative(client, endpoint, format, 0, conversationID)
	if err != nil {
		return nil, err
	}
	call := newCallHandle(handle, ctx, "playback")
	p := &Playback{call: call, done: make(chan struct{})}
	go p.readLoop()
	return p, nil
}

func (p *Playback) readLoop() {
	defer close(p.done)
	defer p.call.release()
	for {
		_, err := p.call.recvProto()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, ErrClosed) {
				p.mu.Lock()
				p.err = err
				p.mu.Unlock()
			}
			return
		}
	}
}

// Write enqueues one PCM chunk. The native engine copies the bytes before the
// call returns, so the caller may reuse pcm immediately. Frame count and
// timestamp are derived natively.
func (p *Playback) Write(pcm []byte) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return audioPlaybackWriteNative(p.call.value(), pcm, 0, 0, false, 0)
}

// Control sends FLUSH, PAUSE or RESUME.
func (p *Playback) Control(cmd PlaybackCommand) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return audioPlaybackControlNative(p.call.value(), cmd)
}

// Close half-closes the request side and waits for the framework to drain and
// end the stream, then releases the native handle. It is bounded and
// context-aware.
func (p *Playback) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	_ = p.call.halfClose()

	timer := time.NewTimer(playbackDrainTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		return err
	case <-timer.C:
		_ = p.call.release()
		return &StatusError{Op: "playback close", Code: statusTimeout,
			Message: "timed out waiting for queued audio to drain"}
	case <-p.call.ctx.Done():
		_ = p.call.release()
		return p.call.ctx.Err()
	}
}

// Abort cancels playback immediately, discarding queued audio. Use Close for
// graceful drain.
func (p *Playback) Abort() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return p.call.release()
}

// --- microphone capture ----------------------------------------------------

type captureAudioStartedJSON struct {
	StreamID             string       `json:"streamId"`
	ActualFormat         *AudioFormat `json:"actualFormat"`
	IndicationWatchdogMS uint32       `json:"indicationWatchdogMs"`
}

// Mic is one fan-out microphone capture.
type Mic struct {
	chunks chan MicChunk
	call   *callHandle
	done   chan struct{}
	stop   chan struct{}

	closeOnce sync.Once
}

// OpenMic opens microphone capture for an activation.
func (c *Client) OpenMic(ctx context.Context, activationID, conversationID string) (*Mic, error) {
	ctx = orBackground(ctx)
	client, err := c.clientHandle()
	if err != nil {
		return nil, err
	}
	format := DefaultAudioFormat()
	handle, err := audioCaptureOpenNative(client, "audio.input.microphone", format,
		0, activationID, conversationID)
	if err != nil {
		return nil, err
	}
	m := &Mic{
		chunks: make(chan MicChunk, 8),
		call:   newCallHandle(handle, ctx, "microphone"),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
	}
	go m.readLoop()
	return m, nil
}

func (m *Mic) readLoop() {
	defer close(m.chunks)
	defer close(m.done)
	defer m.call.release()

	var format *AudioFormat
	for {
		event, status, _ := audioCaptureRecvNative(m.call.value(), waitForever)
		if status != statusOK {
			return
		}

		switch event.kind {
		case 1: // started
			var started captureAudioStartedJSON
			if err := unmarshalProtoJSON(event.json, &started); err != nil {
				continue
			}
			if started.ActualFormat != nil {
				copied := *started.ActualFormat
				format = &copied
			}
			select {
			case m.chunks <- MicChunk{Format: format, StreamID: started.StreamID}:
			case <-m.stop:
				return
			}
		case 2: // chunk
			chunk := &AudioChunk{
				Sequence:            event.sequence,
				FirstFrame:          event.firstFrame,
				MonotonicTimeNS:     event.monotonic,
				FrameCount:          event.frameCount,
				PCM:                 event.pcm,
				Discontinuity:       event.discontinuity,
				DroppedFramesBefore: event.dropped,
			}
			select {
			case m.chunks <- MicChunk{Chunk: chunk, Format: format}:
			case <-m.stop:
				return
			}
		case 3: // notice
			var notice AudioNotice
			if err := unmarshalProtoJSON(event.json, &notice); err != nil {
				continue
			}
			select {
			case m.chunks <- MicChunk{Notice: &notice}:
			case <-m.stop:
				return
			}
		}
	}
}

// Chunks streams capture events; it closes when the capture ends.
func (m *Mic) Chunks() <-chan MicChunk { return m.chunks }

// Done closes when the capture stream ends.
func (m *Mic) Done() <-chan struct{} { return m.done }

// Close cancels the capture and releases the native handle.
func (m *Mic) Close() error {
	var err error
	m.closeOnce.Do(func() {
		close(m.stop)
		err = m.call.release()
	})
	return err
}

// --- assistant conversations ----------------------------------------------

type conversationEventJSON struct {
	Started *struct {
		ConversationID string `json:"conversationId"`
	} `json:"started"`
	Notice *AudioNotice `json:"notice"`
}

// Conversation is a ducking lifetime marker for one assistant activation.
type Conversation struct {
	id   string
	call *callHandle
	once sync.Once
}

// BeginConversation starts the conversation lifetime for activationID.
func (c *Client) BeginConversation(ctx context.Context, activationID string) (*Conversation, error) {
	ctx = orBackground(ctx)
	call, err := c.startCall(ctx, "", assistantService, "BeginConversation", callKindServerStream, 0)
	if err != nil {
		return nil, err
	}
	if err := call.sendJSON(struct {
		ActivationID string `json:"activationId,omitempty"`
	}{ActivationID: activationID}); err != nil {
		call.release()
		return nil, err
	}
	if err := call.halfClose(); err != nil {
		call.release()
		return nil, err
	}
	payload, err := call.recvJSONTimeout(timeoutMillis(ctx, 10*time.Second))
	if err != nil {
		call.release()
		return nil, err
	}
	var event conversationEventJSON
	if err := unmarshalProtoJSON(payload, &event); err != nil {
		call.release()
		return nil, err
	}
	if event.Started == nil || event.Started.ConversationID == "" {
		call.release()
		if event.Notice != nil && event.Notice.Detail != "" {
			return nil, fmt.Errorf("capsula: conversation did not start: %s", event.Notice.Detail)
		}
		return nil, fmt.Errorf("capsula: conversation did not start")
	}
	conversation := &Conversation{id: event.Started.ConversationID, call: call}
	go func() {
		// Drain notices so the stream does not back up.
		for {
			if _, err := call.recvProto(); err != nil {
				call.release()
				return
			}
		}
	}()
	return conversation, nil
}

// ID is the framework conversation ID.
func (k *Conversation) ID() string { return k.id }

// Close ends the conversation marker. It is idempotent.
func (k *Conversation) Close() {
	k.once.Do(func() {
		_ = k.call.release()
	})
}

// --- settings and mixer state ---------------------------------------------

// UserSettings returns the device-wide settings snapshot and its revision.
func (c *Client) UserSettings(ctx context.Context) (UserSettings, uint64, error) {
	var snapshot struct {
		Revision uint64        `json:"revision"`
		Values   *UserSettings `json:"values"`
	}
	if err := c.unaryJSON(ctx, settingsService, "GetSettings", struct{}{}, &snapshot); err != nil {
		return UserSettings{}, 0, err
	}
	if snapshot.Values == nil {
		return UserSettings{}, snapshot.Revision, nil
	}
	return *snapshot.Values, snapshot.Revision, nil
}

// AudioState returns the current system mixer and mute state.
func (c *Client) AudioState(ctx context.Context) (AudioState, error) {
	var state AudioState
	err := c.unaryJSON(ctx, audioService, "GetAudioState", struct{}{}, &state)
	return state, err
}

// ListAudioEndpoints discovers available capture and playback endpoints.
func (c *Client) ListAudioEndpoints(ctx context.Context) ([]AudioEndpoint, error) {
	var response struct {
		Endpoints []AudioEndpoint `json:"endpoints"`
	}
	if err := c.unaryJSON(ctx, audioService, "ListEndpoints", struct{}{}, &response); err != nil {
		return nil, err
	}
	return response.Endpoints, nil
}

// unaryJSON performs a bounded unary framework call and decodes the response.
func (c *Client) unaryJSON(ctx context.Context, service, method string, request, response any) error {
	ctx = orBackground(ctx)
	call, err := c.startCall(ctx, "", service, method, callKindUnary, 10*time.Second)
	if err != nil {
		return err
	}
	defer call.release()

	payload, err := marshalProtoJSON(request)
	if err != nil {
		return err
	}
	if err := call.sendJSONNative(payload); err != nil {
		return err
	}
	if err := call.halfClose(); err != nil {
		return err
	}
	var first []byte
	for {
		message, err := call.recvJSON()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if first == nil {
			first = message
		}
	}
	if response != nil && first != nil {
		return unmarshalProtoJSON(first, response)
	}
	return nil
}
