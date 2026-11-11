package capsula

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
)

// RGB is a ring pixel.
type RGB struct{ R, G, B uint8 }

// --- routed calls ----------------------------------------------------------

type RoutedCall struct {
	stream frameworkv1.AppBusService_CallClient
	closed bool
}

func (c *Client) StreamCall(ctx context.Context, targetAppID, service, rpc string, timeout time.Duration) (*RoutedCall, error) {
	stream, err := c.bus.Call(ctx)
	if err != nil {
		return nil, err
	}
	ms := uint32(timeout / time.Millisecond)
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{TargetAppId: targetAppID, GrpcService: service, RpcName: rpc, TimeoutMs: ms},
	}}); err != nil {
		return nil, err
	}
	return &RoutedCall{stream: stream}, nil
}

func (r *RoutedCall) Send(request []byte) error {
	if r.closed {
		return fmt.Errorf("request side is closed")
	}
	return r.stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{RequestMessage: request}})
}

func (r *RoutedCall) HalfClose() error {
	if r.closed {
		return nil
	}
	r.closed = true
	_ = r.stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_HalfClose{HalfClose: &frameworkv1.CallHalfClose{}}})
	return r.stream.CloseSend()
}

// Recv returns one serialized response; io.EOF marks a successful end.
func (r *RoutedCall) Recv() ([]byte, error) {
	for {
		resp, err := r.stream.Recv()
		if err != nil {
			return nil, err
		}
		switch m := resp.Message.(type) {
		case *frameworkv1.RoutedCallResponse_ResponseMessage:
			return m.ResponseMessage, nil
		case *frameworkv1.RoutedCallResponse_End:
			if m.End.GrpcStatusCode != 0 {
				return nil, fmt.Errorf("routed call failed (%d): %s", m.End.GrpcStatusCode, m.End.Detail)
			}
			return nil, io.EOF
		}
	}
}

// Call performs a unary routed call and returns the first response message.
func (c *Client) Call(ctx context.Context, targetAppID, service, rpc string, request []byte, timeout time.Duration) ([]byte, error) {
	rc, err := c.StreamCall(ctx, targetAppID, service, rpc, timeout)
	if err != nil {
		return nil, err
	}
	if err := rc.Send(request); err != nil {
		return nil, err
	}
	if err := rc.HalfClose(); err != nil {
		return nil, err
	}
	var first []byte
	for {
		msg, err := rc.Recv()
		if err == io.EOF {
			return first, nil
		}
		if err != nil {
			return nil, err
		}
		if first == nil {
			first = msg
		}
	}
}

// --- presentations ---------------------------------------------------------

type Presentation struct {
	id       string
	resource string
	stream   frameworkv1.PresentationService_OpenClient

	idCh    chan string
	done    chan struct{}
	mu      sync.Mutex
	visible bool
	err     error
}

func (c *Client) OpenPresentation(ctx context.Context, resourceID string) (*Presentation, error) {
	stream, err := frameworkv1.NewPresentationServiceClient(c.conn).Open(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&frameworkv1.PresentationRequest{Request: &frameworkv1.PresentationRequest_Open{
		Open: &frameworkv1.OpenPresentation{ResourceId: resourceID},
	}}); err != nil {
		return nil, err
	}
	p := &Presentation{resource: resourceID, stream: stream, idCh: make(chan string, 1), done: make(chan struct{})}
	go p.readLoop()
	select {
	case id := <-p.idCh:
		p.id = id
		return p, nil
	case <-p.done:
		return nil, fmt.Errorf("presentation stream closed before ready: %v", p.err)
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("presentation open timed out")
	}
}

func (p *Presentation) readLoop() {
	defer close(p.done)
	for {
		ev, err := p.stream.Recv()
		if err != nil {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			return
		}
		switch e := ev.Event.(type) {
		case *frameworkv1.PresentationEvent_Ready:
			select {
			case p.idCh <- e.Ready.PresentationId:
			default:
			}
		case *frameworkv1.PresentationEvent_Visibility:
			p.mu.Lock()
			p.visible = e.Visibility.Visible
			p.mu.Unlock()
		}
	}
}

func (p *Presentation) ID() string { return p.id }
func (p *Presentation) Visible() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.visible
}

func (p *Presentation) send(frame *frameworkv1.PresentationFrame) error {
	return p.stream.Send(&frameworkv1.PresentationRequest{Request: &frameworkv1.PresentationRequest_Frame{Frame: frame}})
}

func (p *Presentation) Segment(cells []uint16, colon bool) error {
	out := &frameworkv1.SegmentFrame{Colon: colon}
	for _, c := range cells {
		out.Cells = append(out.Cells, &frameworkv1.SegmentCell{Mask: uint32(c)})
	}
	return p.send(&frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_Segment{Segment: out}})
}

func (p *Presentation) Ring(pixels []RGB) error {
	out := &frameworkv1.RingFrame{}
	for _, px := range pixels {
		out.Pixels = append(out.Pixels, &frameworkv1.RgbPixel{Red: uint32(px.R), Green: uint32(px.G), Blue: uint32(px.B)})
	}
	return p.send(&frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_Ring{Ring: out}})
}

func (p *Presentation) Effect(effectID string, params map[string]any) error {
	args, err := presetArgs(params)
	if err != nil {
		return err
	}
	return p.send(&frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_RingEffect{
		RingEffect: &frameworkv1.RingEffect{EffectId: effectID, Arguments: args},
	}})
}

func (p *Presentation) Face(faceID string, params map[string]any) error {
	args, err := presetArgs(params)
	if err != nil {
		return err
	}
	return p.send(&frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_SegmentFace{
		SegmentFace: &frameworkv1.SegmentFace{FaceId: faceID, Arguments: args},
	}})
}

func (p *Presentation) Blank() error {
	return p.send(&frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_Blank{Blank: &frameworkv1.BlankPresentation{}}})
}

func (p *Presentation) Close() error {
	_ = p.stream.Send(&frameworkv1.PresentationRequest{Request: &frameworkv1.PresentationRequest_Close{Close: &frameworkv1.ClosePresentation{}}})
	return p.stream.CloseSend()
}

func presetArgs(params map[string]any) ([]*frameworkv1.PresetArgument, error) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*frameworkv1.PresetArgument, 0, len(keys))
	for _, name := range keys {
		arg := &frameworkv1.PresetArgument{Name: name}
		switch v := params[name].(type) {
		case string:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Text{Text: v}}
		case bool:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Boolean{Boolean: v}}
		case int:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Integer{Integer: int64(v)}}
		case int64:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Integer{Integer: v}}
		case float64:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Number{Number: v}}
		case float32:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Number{Number: float64(v)}}
		case RGB:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Color{Color: &frameworkv1.RgbPixel{
				Red: uint32(v.R), Green: uint32(v.G), Blue: uint32(v.B),
			}}}
		default:
			return nil, fmt.Errorf("unsupported preset argument %s type %T", name, params[name])
		}
		out = append(out, arg)
	}
	return out, nil
}

// --- audio -----------------------------------------------------------------

type Playback struct {
	stream frameworkv1.AudioService_PlaybackClient
	done   chan struct{}
}

func (c *Client) OpenPlayback(ctx context.Context, endpointID string, format *frameworkv1.AudioFormat, conversationID string) (*Playback, error) {
	stream, err := frameworkv1.NewAudioServiceClient(c.conn).Playback(ctx)
	if err != nil {
		return nil, err
	}
	if format == nil {
		format = &frameworkv1.AudioFormat{
			SampleFormat: frameworkv1.SampleFormat_PCM_S16LE, SampleRateHz: 48000, Channels: 2,
		}
	}
	if err := stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Open{
		Open: &frameworkv1.OpenPlayback{EndpointId: endpointID, Format: format, ConversationId: conversationID},
	}}); err != nil {
		return nil, err
	}
	p := &Playback{stream: stream, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
	return p, nil
}

func (p *Playback) Write(pcm []byte) error {
	return p.stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Chunk{
		Chunk: &frameworkv1.AudioChunk{Pcm: pcm},
	}})
}

func (p *Playback) Control(cmd frameworkv1.PlaybackCommand) error {
	return p.stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Control{Control: cmd}})
}

func (p *Playback) Close() error { return p.stream.CloseSend() }

// MicChunk is one captured audio chunk.
type MicChunk struct {
	Chunk    *frameworkv1.AudioChunk
	Format   *frameworkv1.AudioFormat
	StreamID string
	Notice   *frameworkv1.AudioNotice
}

type Mic struct {
	chunks chan MicChunk
	stream frameworkv1.AudioService_CaptureClient
	done   chan struct{}
}

func (c *Client) OpenMic(ctx context.Context, activationID, conversationID string) (*Mic, error) {
	stream, err := frameworkv1.NewAudioServiceClient(c.conn).Capture(ctx, &frameworkv1.CaptureAudioRequest{
		EndpointId: "audio.input.microphone", ActivationId: activationID, ConversationId: conversationID,
	})
	if err != nil {
		return nil, err
	}
	m := &Mic{chunks: make(chan MicChunk, 8), stream: stream, done: make(chan struct{})}
	go func() {
		defer close(m.chunks)
		defer close(m.done)
		var format *frameworkv1.AudioFormat
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			switch e := ev.Event.(type) {
			case *frameworkv1.CaptureAudioEvent_Started:
				format = e.Started.ActualFormat
				m.chunks <- MicChunk{Format: format, StreamID: e.Started.StreamId}
			case *frameworkv1.CaptureAudioEvent_Chunk:
				m.chunks <- MicChunk{Chunk: e.Chunk, Format: format}
			case *frameworkv1.CaptureAudioEvent_Notice:
				m.chunks <- MicChunk{Notice: e.Notice}
			}
		}
	}()
	return m, nil
}

func (m *Mic) Chunks() <-chan MicChunk { return m.chunks }
func (m *Mic) Done() <-chan struct{}   { return m.done }

// Conversation is a ducking lifetime marker.
type Conversation struct {
	ID     string
	cancel context.CancelFunc
}

func (c *Client) BeginConversation(ctx context.Context, activationID string) (*Conversation, error) {
	runCtx, cancel := context.WithCancel(ctx)
	stream, err := frameworkv1.NewAssistantServiceClient(c.conn).BeginConversation(runCtx, &frameworkv1.BeginConversationRequest{ActivationId: activationID})
	if err != nil {
		cancel()
		return nil, err
	}
	ev, err := stream.Recv()
	if err != nil {
		cancel()
		return nil, err
	}
	started := ev.GetStarted()
	if started == nil {
		cancel()
		return nil, fmt.Errorf("conversation did not start")
	}
	return &Conversation{ID: started.ConversationId, cancel: cancel}, nil
}

func (c *Conversation) Close() { c.cancel() }
