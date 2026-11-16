package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
)

// The physical circle key and panel swipes drive current media through public
// APIs only. A standard provider that is actually playing or paused is
// preferred; its published direction comes from the manager's own resolver, so
// a PLAY/PAUSE derived from it cannot invert the manager's next toggle. When no
// provider is playing or paused — the daemon bundles no Bluetooth provider app,
// so that is the normal case for a connected phone — the source is controlled
// through the public BluetoothMediaService on the same socket, which resolves
// its own direction. There is no private transport or second control plane:
// both paths are the daemon's normal public services.
const mediaPlayerService = "capsulad.contracts.media.v1.MediaPlayerService"

type mediaAction int

const (
	mediaToggle mediaAction = iota
	mediaPrevious
	mediaNext
)

type systemMediaController struct {
	d     *Daemon
	queue chan mediaAction

	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	started   bool
	closed    bool
	conn      *grpc.ClientConn
	closeOnce sync.Once
	closeDone chan struct{}

	// executeAction is replaced by tests to prove close/queue ordering without
	// a live bus. Production leaves it nil and uses execute.
	executeAction func(context.Context, mediaAction)
}

// audioStreamEvidence is implemented by a Bluetooth manager that can accept the
// sink owner's packet-receipt evidence for an ambiguous toggle direction.
type audioStreamEvidence interface {
	SetAudioStreamProbe(func(address string) bool)
}

func newSystemMediaController(d *Daemon) *systemMediaController {
	// An acquired A2DP transport is absent from BlueZ's object tree and some
	// sources expose a MediaControl1 without any MediaPlayer1.Status. The sink's
	// own receipt state is then the only honest evidence that the source is
	// playing, so the circle tap pauses instead of sending a second Play to a
	// source that is already playing (which looks like nothing happened).
	if evidence, ok := d.bt.(audioStreamEvidence); ok {
		evidence.SetAudioStreamProbe(d.bluetoothReceivingAudio)
	}
	return &systemMediaController{
		d: d, queue: make(chan mediaAction, 8),
		done: make(chan struct{}), closeDone: make(chan struct{}),
	}
}

// Run serializes transport commands so a quick sequence of swipes cannot be
// reordered by concurrent provider calls. Its child context is canceled by
// Close, which also waits for Run before releasing the gRPC client.
func (s *systemMediaController) Run(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return
	}
	s.cancel = cancel
	s.started = true
	s.mu.Unlock()
	defer close(s.done)
	defer cancel()
	for {
		select {
		case <-runCtx.Done():
			return
		case action := <-s.queue:
			if runCtx.Err() != nil {
				return
			}
			if s.executeAction != nil {
				s.executeAction(runCtx, action)
				continue
			}
			s.execute(runCtx, action)
		}
	}
}

func (s *systemMediaController) Toggle()   { s.enqueue(mediaToggle) }
func (s *systemMediaController) Previous() { s.enqueue(mediaPrevious) }
func (s *systemMediaController) Next()     { s.enqueue(mediaNext) }

func (s *systemMediaController) enqueue(action mediaAction) {
	select {
	case <-s.d.ctx.Done():
		return
	default:
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	dropped := false
	select {
	case s.queue <- action:
	default:
		dropped = true
	}
	s.mu.Unlock()
	if dropped {
		s.d.logs.Warnf("media", "", "system media command dropped: queue is full")
	}
}

// clientConn always returns a connection, including after Close (it then
// returns the closed client rather than nil). Run is the only caller during
// normal operation, and Close waits for Run first.
func (s *systemMediaController) clientConn() *grpc.ClientConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		s.conn = s.d.localClientConn()
	}
	return s.conn
}

// Close cancels a running command, waits for Run to exit and only then closes
// the client. Queued commands are never dispatched after close.
func (s *systemMediaController) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		cancel := s.cancel
		started := s.started
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if started {
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				s.d.logs.Warnf("media", "", "system media worker did not stop within 5s")
			}
		}
		s.mu.Lock()
		conn := s.conn
		s.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		close(s.closeDone)
	})
	<-s.closeDone
}

func (s *systemMediaController) execute(parent context.Context, action mediaAction) {
	if parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	conn := s.clientConn()
	if conn == nil {
		s.d.logs.Warnf("media", "", "system media client is unavailable")
		return
	}
	ifaces, err := frameworkv1.NewAppInterfaceServiceClient(conn).ListInterfaces(ctx,
		&frameworkv1.ListAppInterfacesRequest{RunningOnly: true})
	if err != nil {
		if s.controlBluetooth(ctx, conn, action) {
			return
		}
		s.d.logs.Debugf("media", "", "system media discovery failed: %v", err)
		return
	}
	type candidate struct {
		appID string
		state mediav1.PlaybackState
		rank  int
		id    string
	}
	var best *candidate
	seen := map[string]bool{}
	for _, iface := range ifaces.GetInterfaces() {
		if iface.GrpcService != mediaPlayerService || seen[iface.AppId] {
			continue
		}
		seen[iface.AppId] = true
		players, err := s.listPlayers(ctx, conn, iface.AppId)
		if err != nil {
			s.d.logs.Debugf("media", "", "media provider %s is unavailable: %v", iface.AppId, err)
			continue
		}
		for _, p := range players {
			rank := 1
			switch p.PlaybackState {
			case mediav1.PlaybackState_PLAYBACK_PLAYING:
				rank = 3
			case mediav1.PlaybackState_PLAYBACK_PAUSED:
				rank = 2
			}
			if best == nil || rank > best.rank {
				best = &candidate{appID: iface.AppId, state: p.PlaybackState, rank: rank, id: p.PlayerId}
			}
		}
	}
	// A standard player that is actually playing or paused is the current
	// media and wins over the Bluetooth source. When no standard player is
	// active, the connected Bluetooth source is the coherent fallback: the
	// device is a Bluetooth speaker with no provider app installed.
	if best == nil || best.rank < 2 {
		if s.controlBluetooth(ctx, conn, action) {
			return
		}
	}
	if best == nil {
		s.d.logs.Debugf("media", "", "no standard media player or connected Bluetooth source; system key ignored")
		return
	}
	rpc := "Play"
	switch action {
	case mediaToggle:
		if best.state == mediav1.PlaybackState_PLAYBACK_PLAYING {
			rpc = "Pause"
		}
	case mediaPrevious:
		rpc = "SkipPrevious"
	case mediaNext:
		rpc = "SkipNext"
	}
	req, _ := proto.Marshal(&mediav1.PlayerRequest{PlayerId: best.id})
	end, err := s.routedUnary(ctx, conn, best.appID, rpc, req)
	if err != nil {
		s.d.logs.Warnf("media", "", "system media %s on %s failed: %v", rpc, best.appID, err)
		return
	}
	if end != nil && end.GrpcStatusCode != 0 {
		s.d.logs.Warnf("media", "", "system media %s on %s rejected (%d): %s", rpc, best.appID, end.GrpcStatusCode, end.Detail)
		return
	}
	s.d.logs.Infof("media", "", "system media %s accepted by %s player %s", rpc, best.appID, best.id)
}

// controlBluetooth uses the public BluetoothMediaService over the same socket.
// It is the zero-provider path; a playing or paused standard provider is always
// preferred. The service itself rejects a command when no source is present, so
// no private transport or second control plane is introduced.
func (s *systemMediaController) controlBluetooth(ctx context.Context, conn *grpc.ClientConn, action mediaAction) bool {
	client := frameworkv1.NewBluetoothMediaServiceClient(conn)
	state, err := client.GetState(ctx, &frameworkv1.GetBluetoothMediaStateRequest{})
	if err != nil {
		s.d.logs.Debugf("media", "", "Bluetooth media state unavailable: %v", err)
		return false
	}
	if !state.GetPresent() {
		return false
	}
	command := frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_TOGGLE
	switch action {
	case mediaPrevious:
		command = frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PREVIOUS
	case mediaNext:
		command = frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_NEXT
	}
	after, err := client.Command(ctx, &frameworkv1.BluetoothMediaCommandRequest{Command: command})
	if err != nil {
		s.d.logs.Warnf("media", "", "Bluetooth media command failed: %v", err)
		return false
	}
	// BlueZ answers a transport command when it queues the AV/C passthrough, not
	// when the peer acts on it, so this states what was sent, not what happened.
	s.d.logs.Infof("media", "", "Bluetooth media %s sent to %s (playing=%t)",
		command, after.GetDeviceName(), after.GetPlaying())
	return true
}

func (s *systemMediaController) listPlayers(ctx context.Context, conn *grpc.ClientConn, appID string) ([]*mediav1.PlayerStatus, error) {
	req, _ := proto.Marshal(&mediav1.ListPlayersRequest{})
	raw, end, err := s.routedUnaryResponse(ctx, conn, appID, "ListPlayers", req)
	if err != nil {
		return nil, err
	}
	if end != nil && end.GrpcStatusCode != 0 {
		return nil, fmt.Errorf("ListPlayers rejected (%d): %s", end.GrpcStatusCode, end.Detail)
	}
	var resp mediav1.ListPlayersResponse
	if len(raw) == 0 {
		return nil, nil
	}
	if err := proto.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Players, nil
}

func (s *systemMediaController) routedUnary(ctx context.Context, conn *grpc.ClientConn, appID, rpc string, request []byte) (*frameworkv1.AppCallEnd, error) {
	_, end, err := s.routedUnaryResponse(ctx, conn, appID, rpc, request)
	return end, err
}

// routedUnaryResponse performs one unary app-bus call over the daemon's own
// public socket, exactly like the Web UI. It returns the first response frame.
func (s *systemMediaController) routedUnaryResponse(ctx context.Context, conn *grpc.ClientConn, appID, rpc string, request []byte) ([]byte, *frameworkv1.AppCallEnd, error) {
	stream, err := frameworkv1.NewAppBusServiceClient(conn).Call(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{
			TargetAppId: appID, GrpcService: mediaPlayerService, RpcName: rpc, TimeoutMs: 2500,
		},
	}}); err != nil {
		return nil, nil, err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{
		RequestMessage: request,
	}}); err != nil {
		return nil, nil, err
	}
	_ = stream.CloseSend()
	var raw []byte
	for {
		resp, err := stream.Recv()
		if err != nil {
			return raw, nil, err
		}
		switch m := resp.Message.(type) {
		case *frameworkv1.RoutedCallResponse_ResponseMessage:
			if raw == nil {
				raw = m.ResponseMessage
			}
		case *frameworkv1.RoutedCallResponse_End:
			return raw, m.End, nil
		}
	}
}
