package webui

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
)

// The overview's media field is a now-playing projection, so it has to change
// when a provider starts or stops playing without the tab polling. The public
// WatchPlayers stream is the provider's own change feed, including for a READY
// provider that currently has no playing player: a fixture or radio tuner that
// starts from stopped must be visible without first appearing in a snapshot.
// The connected Bluetooth source has the equivalent framework WatchState
// stream. Both are tracked only while their provider is READY; the run watch
// reconciles the set, so no timer polls a stopped app.
const (
	// Routed app calls are bounded by the daemon's app bus (five minutes).
	// Renew below that bound so a long-lived tab keeps its feed, and treat the
	// end as a renewal point rather than a terminal failure.
	playerWatchTimeout = 4 * time.Minute
	mediaWatchRetryMin = time.Second
	mediaWatchRetryMax = 5 * time.Second
)

// overviewMediaWatch owns the per-subscription playback watchers. Their events
// only request a rebuild of the authoritative overview; the projection itself
// is read from the providers, so a coalesced event cannot leave the page with a
// partially applied player set.
type overviewMediaWatch struct {
	s       *Server
	ctx     context.Context
	refresh func()
	kickCh  chan struct{}

	watches map[string]context.CancelFunc
}

func newOverviewMediaWatch(s *Server, ctx context.Context, refresh func()) *overviewMediaWatch {
	return &overviewMediaWatch{
		s: s, ctx: ctx, refresh: refresh,
		kickCh:  make(chan struct{}, 1),
		watches: map[string]context.CancelFunc{},
	}
}

// kick requests a provider-set reconciliation without blocking the run watcher.
func (w *overviewMediaWatch) kick() {
	select {
	case w.kickCh <- struct{}{}:
	default:
	}
}

func (w *overviewMediaWatch) run() {
	// A connected source can start playing at any moment, including while the
	// page has no media app at all. The stream itself is the change feed.
	go w.watchBluetooth()
	w.kick()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.kickCh:
			w.sync()
		}
	}
}

// sync reconciles the watched providers with the READY application set. A new
// feed starts from a baseline read taken before the subscription, so a
// transition in the gap between the overview snapshot and WatchPlayers is
// detected by the first event rather than lost.
func (w *overviewMediaWatch) sync() {
	targets, err := w.s.mediaWatchTargets(w.ctx)
	if err != nil {
		// A failed listing says nothing about the current providers; keep the
		// existing feeds instead of tearing down a working projection.
		return
	}
	wanted := make(map[string]bool, len(targets))
	for _, appID := range targets {
		wanted[appID] = true
		if _, ok := w.watches[appID]; ok {
			continue
		}
		players, err := w.s.listPlayers(w.ctx, appID)
		baseline, known := "", false
		if err == nil {
			baseline, known = playingSignature(playerStatusMap(players)), true
		}
		ctx, cancel := context.WithCancel(w.ctx)
		w.watches[appID] = cancel
		go w.watchPlayers(ctx, appID, baseline, known)
	}
	for appID, cancel := range w.watches {
		if wanted[appID] {
			continue
		}
		cancel()
		delete(w.watches, appID)
	}
}

// mediaWatchTargets lists the READY applications that declare the standard
// media contract. Installed-but-stopped and still-starting apps are excluded:
// the daemon cannot route to them, so subscribing would only retry.
func (s *Server) mediaWatchTargets(ctx context.Context) ([]string, error) {
	apps, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListApplications(ctx,
		&frameworkv1.ListApplicationsRequest{})
	if err != nil {
		return nil, err
	}
	runs, err := frameworkv1.NewApplicationServiceClient(s.Conn).ListRuns(ctx,
		&frameworkv1.ListRunsRequest{})
	if err != nil {
		return nil, err
	}
	ready := map[string]bool{}
	for _, run := range runs.GetRuns() {
		if run.GetState() == frameworkv1.AppRunState_APP_READY {
			ready[run.GetAppId()] = true
		}
	}
	var out []string
	for _, app := range apps.GetApplications() {
		if providesMedia(app) && ready[app.GetAppId()] {
			out = append(out, app.GetAppId())
		}
	}
	sort.Strings(out)
	return out, nil
}

// watchPlayers follows one provider's public WatchPlayers stream. The baseline
// is the playing set read just before this subscription; every event, including
// the first snapshot, is compared with it. A renewal compares against the last
// signature, so a transition missed while the routed call was ending still
// updates the page.
func (w *overviewMediaWatch) watchPlayers(ctx context.Context, appID, baseline string, baselineKnown bool) {
	players := map[string]*mediav1.PlayerStatus{}
	lastSignature := baseline
	haveSignature := baselineKnown
	retry := mediaWatchRetryMin
	for ctx.Err() == nil {
		streamed := false
		err := w.s.streamPlayers(ctx, appID, func(event *mediav1.PlayerEvent) {
			applyPlayerEvent(players, event)
			signature := playingSignature(players)
			if !haveSignature || signature != lastSignature {
				w.refresh()
			}
			lastSignature, haveSignature = signature, true
			streamed = true
		})
		if ctx.Err() != nil {
			return
		}
		if streamed {
			retry = mediaWatchRetryMin
		}
		if w.s.Logs != nil && err != nil {
			w.s.Logs.Debugf("webui", appID, "media watch ended: %v", err)
		}
		if !waitMediaRetry(ctx, retry) {
			return
		}
		retry = nextMediaRetry(retry)
	}
}

// streamPlayers reads one server-streaming WatchPlayers call over the app bus.
// The request is half-closed after the single request message; the provider
// owns the response side until the call deadline, provider stop or ctx cancel.
func (s *Server) streamPlayers(ctx context.Context, appID string, onEvent func(*mediav1.PlayerEvent)) error {
	stream, err := frameworkv1.NewAppBusServiceClient(s.Conn).Call(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{
			TargetAppId: appID, GrpcService: mediaService, RpcName: "WatchPlayers",
			TimeoutMs: uint32(playerWatchTimeout / time.Millisecond),
		},
	}}); err != nil {
		return err
	}
	request, err := proto.Marshal(&mediav1.WatchPlayersRequest{})
	if err != nil {
		return err
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{
		RequestMessage: request,
	}}); err != nil {
		return err
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		switch message := resp.GetMessage().(type) {
		case *frameworkv1.RoutedCallResponse_ResponseMessage:
			var event mediav1.PlayerEvent
			if err := proto.Unmarshal(message.ResponseMessage, &event); err != nil {
				return err
			}
			onEvent(&event)
		case *frameworkv1.RoutedCallResponse_End:
			end := message.End
			if end.GetGrpcStatusCode() == 0 {
				return nil
			}
			detail := strings.TrimSpace(end.GetDetail())
			if detail == "" {
				detail = "media watch ended"
			}
			return status.Error(codes.Code(end.GetGrpcStatusCode()), detail)
		}
	}
}

func playerStatusMap(players []*mediav1.PlayerStatus) map[string]*mediav1.PlayerStatus {
	out := make(map[string]*mediav1.PlayerStatus, len(players))
	for _, player := range players {
		out[player.GetPlayerId()] = player
	}
	return out
}

func applyPlayerEvent(players map[string]*mediav1.PlayerStatus, event *mediav1.PlayerEvent) {
	switch value := event.GetEvent().(type) {
	case *mediav1.PlayerEvent_Snapshot:
		clear(players)
		for _, player := range value.Snapshot.GetPlayers() {
			players[player.GetPlayerId()] = player
		}
	case *mediav1.PlayerEvent_Updated:
		if id := value.Updated.GetPlayerId(); id != "" {
			players[id] = value.Updated
		}
	case *mediav1.PlayerEvent_Removed:
		delete(players, value.Removed.GetPlayerId())
	}
}

// playingSignature is the part of a provider's state that the Now Playing card
// renders. Position and revision are deliberately absent: a position tick must
// not rebuild the whole overview. Paused/stopped players are absent too, so a
// hidden player's metadata change is not a visible change.
func playingSignature(players map[string]*mediav1.PlayerStatus) string {
	ids := make([]string, 0, len(players))
	for id, player := range players {
		if player.GetPlaybackState() == mediav1.PlaybackState_PLAYBACK_PLAYING {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		player := players[id]
		b.WriteString(id)
		b.WriteByte(0x1f)
		b.WriteString(player.GetAppName())
		b.WriteByte(0x1f)
		b.WriteString(player.GetSourceName())
		b.WriteByte(0x1f)
		b.WriteString(player.GetMediaName())
		b.WriteByte(0x1f)
		b.WriteString(player.GetTitle())
		b.WriteByte(0x1f)
		b.WriteString(player.GetArtist())
		b.WriteByte(0x1f)
		b.WriteString(player.GetAlbum())
		b.WriteByte(0x1f)
		b.WriteString(strconv.FormatUint(player.GetDurationMs(), 10))
		b.WriteByte(0x1f)
		for _, action := range player.GetSupportedActions() {
			b.WriteString(strconv.Itoa(int(action)))
			b.WriteByte(',')
		}
		b.WriteByte(0x1e)
	}
	return b.String()
}

// watchBluetooth follows the framework's own source state. Only observed
// playback is part of the Now Playing projection, so a paused source and a
// command-memory "playing" direction both produce the absent signature.
func (w *overviewMediaWatch) watchBluetooth() {
	lastSignature := ""
	haveSignature := false
	retry := mediaWatchRetryMin
	for w.ctx.Err() == nil {
		stream, err := frameworkv1.NewBluetoothMediaServiceClient(w.s.Conn).WatchState(w.ctx,
			&frameworkv1.WatchBluetoothMediaStateRequest{})
		streamed := false
		if err == nil {
			for {
				state, recvErr := stream.Recv()
				if recvErr != nil {
					err = recvErr
					break
				}
				signature := bluetoothPlayingSignature(state)
				if haveSignature && signature != lastSignature {
					w.refresh()
				}
				lastSignature, haveSignature = signature, true
				streamed = true
			}
		}
		if w.ctx.Err() != nil {
			return
		}
		switch status.Code(err) {
		case codes.Unimplemented, codes.Unavailable:
			// This platform publishes no Bluetooth media surface; its presence
			// does not change without a daemon restart, so retrying would only
			// poll an answer that cannot change.
			return
		}
		if streamed {
			retry = mediaWatchRetryMin
		}
		if w.s.Logs != nil && err != nil {
			w.s.Logs.Debugf("webui", "", "Bluetooth media watch ended: %v", err)
		}
		if !waitMediaRetry(w.ctx, retry) {
			return
		}
		retry = nextMediaRetry(retry)
	}
}

// bluetoothPlayingSignature is the visible part of the framework source. The
// source's address never crosses this boundary, and position/revision changes
// are not a visible change worth a full overview rebuild.
func bluetoothPlayingSignature(state *frameworkv1.BluetoothMediaState) string {
	if !state.GetPresent() || !state.GetObservedPlaying() {
		return ""
	}
	var b strings.Builder
	b.WriteString(state.GetDeviceName())
	b.WriteByte(0x1f)
	b.WriteString(state.GetTitle())
	b.WriteByte(0x1f)
	b.WriteString(state.GetArtist())
	b.WriteByte(0x1f)
	b.WriteString(state.GetAlbum())
	b.WriteByte(0x1f)
	b.WriteString(strconv.FormatUint(state.GetDurationMs(), 10))
	return b.String()
}

func waitMediaRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextMediaRetry(delay time.Duration) time.Duration {
	delay *= 2
	if delay > mediaWatchRetryMax {
		return mediaWatchRetryMax
	}
	return delay
}
