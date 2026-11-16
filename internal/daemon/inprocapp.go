package daemon

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	"capsulad/internal/backend"
	"capsulad/internal/presets"
)

const appStreamTimeout = 5 * time.Second

// builtinAppClient is the app-facing path for a compiled-in system app. It
// dials the daemon's own Unix socket and uses the generated public service
// clients, exactly like a supervised app's SDK. Presentation ownership,
// capture validation, audio stream rules and cleanup therefore run in the
// shared service handlers, never through direct manager access.
type builtinAppClient struct {
	d     *Daemon
	appID string
	runID string
	conn  *grpc.ClientConn

	presentations frameworkv1.PresentationServiceClient
	routing       frameworkv1.RoutingServiceClient
	audio         frameworkv1.AudioServiceClient

	failMu    sync.Mutex
	onFailure func(error)
}

// SetFailureHandler registers a callback for an unexpected internal stream
// failure. Normal presentation/capture Close calls never invoke it.
func (c *builtinAppClient) SetFailureHandler(fn func(error)) {
	c.failMu.Lock()
	c.onFailure = fn
	c.failMu.Unlock()
}

func (c *builtinAppClient) streamFailed(err error) {
	c.failMu.Lock()
	fn := c.onFailure
	c.failMu.Unlock()
	if fn != nil {
		fn(err)
	}
}

func newBuiltinAppClient(d *Daemon, appID string) *builtinAppClient {
	conn := d.localClientConn()
	return &builtinAppClient{
		d: d, appID: appID, runID: d.SystemAppRunID(appID), conn: conn,
		presentations: frameworkv1.NewPresentationServiceClient(conn),
		routing:       frameworkv1.NewRoutingServiceClient(conn),
		audio:         frameworkv1.NewAudioServiceClient(conn),
	}
}

func (c *builtinAppClient) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

func (c *builtinAppClient) context(parent context.Context) context.Context {
	return metadata.AppendToOutgoingContext(parent, "capsulad-run-id", c.runID)
}

// --- presentation service ---------------------------------------------------

type appPresentation struct {
	cancel context.CancelFunc
	stream frameworkv1.PresentationService_OpenClient
	done   chan struct{}
	ready  chan struct{}

	sendMu  sync.Mutex
	mu      sync.Mutex
	id      string
	err     error
	once    sync.Once
	client  *builtinAppClient
	closing atomic.Bool
}

func (c *builtinAppClient) OpenPresentation(parent context.Context, resourceID string) (*appPresentation, error) {
	ctx, cancel := context.WithCancel(c.context(parent))
	stream, err := c.presentations.Open(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := stream.Send(&frameworkv1.PresentationRequest{Request: &frameworkv1.PresentationRequest_Open{
		Open: &frameworkv1.OpenPresentation{ResourceId: resourceID},
	}}); err != nil {
		cancel()
		return nil, err
	}
	p := &appPresentation{
		cancel: cancel, stream: stream, client: c,
		done: make(chan struct{}), ready: make(chan struct{}),
	}
	go p.recvLoop()
	select {
	case <-p.ready:
		return p, nil
	case <-p.done:
		cancel()
		return nil, p.result()
	case <-time.After(appStreamTimeout):
		cancel()
		return nil, fmt.Errorf("presentation %s did not become ready", resourceID)
	}
}

func (p *appPresentation) recvLoop() {
	defer close(p.done)
	defer p.cancel()
	for {
		ev, err := p.stream.Recv()
		if err != nil {
			if !p.closing.Load() && p.client != nil {
				p.client.streamFailed(err)
			}
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			return
		}
		if ready := ev.GetReady(); ready != nil {
			p.mu.Lock()
			p.id = ready.PresentationId
			p.mu.Unlock()
			p.once.Do(func() { close(p.ready) })
		}
	}
}

func (p *appPresentation) ID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.id
}

func (p *appPresentation) result() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		return fmt.Errorf("presentation stream closed")
	}
	return p.err
}

func (p *appPresentation) UpdateFrame(frame *frameworkv1.PresentationFrame) error {
	if frame == nil {
		return fmt.Errorf("presentation frame is required")
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	return p.stream.Send(&frameworkv1.PresentationRequest{Request: &frameworkv1.PresentationRequest_Frame{Frame: frame}})
}

func (p *appPresentation) Close() {
	p.closing.Store(true)
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(time.Second):
	}
}

// --- routing service --------------------------------------------------------

type appCapture struct {
	client *builtinAppClient
	cancel context.CancelFunc
	stream frameworkv1.RoutingService_CaptureClient
	done   chan struct{}
	ready  chan struct{}

	mu      sync.Mutex
	id      string
	err     error
	release sync.Once
	once    sync.Once
	closing atomic.Bool
}

func (c *builtinAppClient) Capture(parent context.Context, resourceID, presentationID string) (*appCapture, error) {
	ctx, cancel := context.WithCancel(c.context(parent))
	stream, err := c.routing.Capture(ctx, &frameworkv1.CaptureRequest{
		ResourceId: resourceID, PresentationId: presentationID,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	cap := &appCapture{
		client: c, cancel: cancel, stream: stream,
		done: make(chan struct{}), ready: make(chan struct{}),
	}
	go cap.recvLoop()
	select {
	case <-cap.ready:
		return cap, nil
	case <-cap.done:
		cancel()
		return nil, cap.result()
	case <-time.After(appStreamTimeout):
		cancel()
		return nil, fmt.Errorf("capture on %s was not granted", resourceID)
	}
}

func (c *appCapture) recvLoop() {
	defer close(c.done)
	defer c.cancel()
	for {
		ev, err := c.stream.Recv()
		if err != nil {
			if !c.closing.Load() && c.client != nil {
				c.client.streamFailed(err)
			}
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
		if granted := ev.GetGranted(); granted != nil && granted.Capture != nil {
			c.mu.Lock()
			c.id = granted.Capture.CaptureId
			c.mu.Unlock()
			c.once.Do(func() { close(c.ready) })
		}
	}
}

func (c *appCapture) ID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

func (c *appCapture) result() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		return fmt.Errorf("capture stream closed")
	}
	return c.err
}

func (c *appCapture) Release() {
	c.closing.Store(true)
	c.release.Do(func() {
		if id := c.ID(); id != "" {
			ctx, cancel := context.WithTimeout(c.client.context(context.Background()), time.Second)
			_, _ = c.client.routing.ReleaseCapture(ctx, &frameworkv1.ReleaseCaptureRequest{CaptureId: id})
			cancel()
		}
		c.cancel()
		select {
		case <-c.done:
		case <-time.After(time.Second):
		}
	})
}

// --- audio service ----------------------------------------------------------

// PlaybackFormat reads the speaker format through AudioService.ListEndpoints.
func (c *builtinAppClient) PlaybackFormat(parent context.Context) (backend.AudioFormat, error) {
	ctx, cancel := context.WithTimeout(parent, appStreamTimeout)
	defer cancel()
	resp, err := c.audio.ListEndpoints(c.context(ctx), &frameworkv1.ListAudioEndpointsRequest{})
	if err != nil {
		return backend.AudioFormat{}, err
	}
	for _, endpoint := range resp.GetEndpoints() {
		if endpoint.GetDirection() != frameworkv1.AudioDirection_AUDIO_PLAYBACK {
			continue
		}
		if formats := endpoint.GetSupportedFormats(); len(formats) > 0 {
			return formatFromProto(formats[0]), nil
		}
	}
	return backend.AudioFormat{}, fmt.Errorf("no playback endpoint format advertised")
}

// Playback sends one finite PCM clip through AudioService.Playback. The shared
// handler resolves the caller run and feeds the normal mixer.
func (c *builtinAppClient) Playback(parent context.Context, format backend.AudioFormat, pcm []byte) error {
	ctx, cancel := context.WithCancel(c.context(parent))
	defer cancel()
	stream, err := c.audio.Playback(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Open{
		Open: &frameworkv1.OpenPlayback{
			EndpointId: "audio.output.speaker", Format: formatProto(format),
		},
	}}); err != nil {
		return err
	}
	const chunk = 16 * 1024
	for offset := 0; offset < len(pcm); offset += chunk {
		end := offset + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		req := &frameworkv1.PlaybackRequest{Request: &frameworkv1.PlaybackRequest_Chunk{
			Chunk: &frameworkv1.AudioChunk{Pcm: pcm[offset:end]},
		}}
		if err := stream.Send(req); err != nil {
			return err
		}
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				if err == io.EOF {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-parent.Done():
		return parent.Err()
	}
}

// --- frame builders ---------------------------------------------------------

func segmentPresentationFrame(cells []uint16, colon bool) *frameworkv1.PresentationFrame {
	protoCells := make([]*frameworkv1.SegmentCell, 0, len(cells))
	for _, mask := range cells {
		protoCells = append(protoCells, &frameworkv1.SegmentCell{Mask: uint32(mask)})
	}
	return &frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_Segment{
		Segment: &frameworkv1.SegmentFrame{Cells: protoCells, Colon: colon},
	}}
}

func ringEffectPresentationFrame(id string, params presets.Params) *frameworkv1.PresentationFrame {
	return &frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_RingEffect{
		RingEffect: &frameworkv1.RingEffect{EffectId: id, Arguments: presetArguments(params)},
	}}
}

// blankPresentationFrame is explicit zero content, not an absent update: it
// keeps ownership and eligibility while the backend writes nothing lit.
func blankPresentationFrame() *frameworkv1.PresentationFrame {
	return &frameworkv1.PresentationFrame{Payload: &frameworkv1.PresentationFrame_Blank{
		Blank: &frameworkv1.BlankPresentation{},
	}}
}

func presetArguments(params presets.Params) []*frameworkv1.PresetArgument {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*frameworkv1.PresetArgument, 0, len(keys))
	for _, key := range keys {
		arg := &frameworkv1.PresetArgument{Name: key}
		switch value := params[key].(type) {
		case string:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Text{Text: value}}
		case bool:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Boolean{Boolean: value}}
		case int:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Integer{Integer: int64(value)}}
		case int32:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Integer{Integer: int64(value)}}
		case int64:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Integer{Integer: value}}
		case float32:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Number{Number: float64(value)}}
		case float64:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Number{Number: value}}
		case backend.RGB:
			arg.Value = &frameworkv1.PresetValue{Value: &frameworkv1.PresetValue_Color{Color: &frameworkv1.RgbPixel{
				Red: uint32(value.R), Green: uint32(value.G), Blue: uint32(value.B),
			}}}
		default:
			continue
		}
		out = append(out, arg)
	}
	return out
}
