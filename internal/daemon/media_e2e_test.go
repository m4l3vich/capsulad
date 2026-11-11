package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsula/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	"capsulad/internal/bluetooth"
)

const mediaService = "capsula.contracts.media.v1.MediaPlayerService"

// TestMediaProviderRoutedStreaming exercises the real media sample app through
// the app bus: interface discovery, a unary ListPlayers, a server-streaming
// WatchPlayers whose first event must be a snapshot, and command-driven state
// updates arriving on that same stream.
func TestMediaProviderRoutedStreaming(t *testing.T) {
	binary := buildModuleApp(t, "apps/media")
	manifest, err := os.ReadFile(filepath.Join(moduleRoot(t), "apps", "media", "manifest.toml"))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := os.ReadFile(filepath.Join(moduleRoot(t), "apps", "media", "schemas", "media.protoset"))
	if err != nil {
		t.Fatal(err)
	}
	d, _, _ := newTestDaemonWith(t, appInstall{
		id: "org.capsula.media", binaryName: "media", binaryPath: binary,
		manifest: manifest, files: map[string][]byte{"schemas/media.protoset": descriptor},
	})
	conn := dialTest(t, d.cfg.SocketPath)
	_ = waitReady(t, d, "org.capsula.media", 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ifaces, err := frameworkv1.NewAppInterfaceServiceClient(conn).ListInterfaces(ctx,
		&frameworkv1.ListAppInterfacesRequest{RunningOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	advertised := false
	for _, iface := range ifaces.GetInterfaces() {
		if iface.AppId == "org.capsula.media" && iface.GrpcService == mediaService {
			advertised = true
		}
	}
	if !advertised {
		t.Fatalf("media app did not advertise %s: %v", mediaService, ifaces.GetInterfaces())
	}

	listReq, _ := proto.Marshal(&mediav1.ListPlayersRequest{})
	raw := routedCall(t, conn, "org.capsula.media", mediaService, "ListPlayers", [][]byte{listReq})
	if len(raw) != 1 {
		t.Fatalf("ListPlayers returned %d messages", len(raw))
	}
	var list mediav1.ListPlayersResponse
	if err := proto.Unmarshal(raw[0], &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Players) != 1 || list.Players[0].GetPlayerId() != "local.player" {
		t.Fatalf("unexpected players: %v", list.Players)
	}
	if got := list.Players[0].GetPlaybackState(); got != mediav1.PlaybackState_PLAYBACK_STOPPED {
		t.Fatalf("fresh player state = %v", got)
	}

	streamCtx, streamCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer streamCancel()
	stream, err := frameworkv1.NewAppBusServiceClient(conn).Call(streamCtx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_Start{
		Start: &frameworkv1.RoutedCallStart{TargetAppId: "org.capsula.media", GrpcService: mediaService, RpcName: "WatchPlayers", TimeoutMs: 12000},
	}}); err != nil {
		t.Fatal(err)
	}
	watchReq, _ := proto.Marshal(&mediav1.WatchPlayersRequest{})
	if err := stream.Send(&frameworkv1.RoutedCallRequest{Message: &frameworkv1.RoutedCallRequest_RequestMessage{RequestMessage: watchReq}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	nextEvent := func() *mediav1.PlayerEvent {
		t.Helper()
		for {
			resp, err := stream.Recv()
			if err != nil {
				t.Fatalf("watch stream: %v", err)
			}
			switch m := resp.Message.(type) {
			case *frameworkv1.RoutedCallResponse_ResponseMessage:
				var event mediav1.PlayerEvent
				if err := proto.Unmarshal(m.ResponseMessage, &event); err != nil {
					t.Fatal(err)
				}
				return &event
			case *frameworkv1.RoutedCallResponse_End:
				t.Fatalf("watch stream ended: %d %s", m.End.GetGrpcStatusCode(), m.End.GetDetail())
			}
		}
	}
	snapshot := nextEvent()
	if snapshot.GetSnapshot() == nil {
		t.Fatalf("first watch event is not a snapshot: %v", snapshot)
	}
	if got := len(snapshot.GetSnapshot().GetPlayers()); got != 1 {
		t.Fatalf("snapshot has %d players", got)
	}
	initialRevision := snapshot.GetSnapshot().GetPlayers()[0].GetRevision()

	playReq, _ := proto.Marshal(&mediav1.PlayerRequest{PlayerId: "local.player"})
	raw = routedCall(t, conn, "org.capsula.media", mediaService, "Play", [][]byte{playReq})
	var accepted mediav1.PlayerCommandResult
	if err := proto.Unmarshal(raw[0], &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.GetAccepted() {
		t.Fatalf("play rejected: %s", accepted.GetDetail())
	}

	waitState := func(want mediav1.PlaybackState) *mediav1.PlayerStatus {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatalf("no update reached %v", want)
			default:
			}
			event := nextEvent()
			status := event.GetUpdated()
			if status == nil {
				continue
			}
			if status.GetPlaybackState() == want {
				return status
			}
		}
	}
	playing := waitState(mediav1.PlaybackState_PLAYBACK_PLAYING)
	if playing.GetRevision() <= initialRevision {
		t.Fatalf("revision did not advance: %d -> %d", initialRevision, playing.GetRevision())
	}
	if playing.GetMediaName() == "" {
		t.Fatal("playing status has no media name")
	}

	raw = routedCall(t, conn, "org.capsula.media", mediaService, "Pause", [][]byte{playReq})
	if err := proto.Unmarshal(raw[0], &accepted); err != nil {
		t.Fatal(err)
	}
	waitState(mediav1.PlaybackState_PLAYBACK_PAUSED)

	// A connected Bluetooth source appears as a second player through the same
	// contract, with AVRCP metadata and commands relayed by the framework.
	mem, ok := d.bt.(*bluetooth.Memory)
	if !ok {
		t.Fatalf("simulator Bluetooth manager is %T", d.bt)
	}
	mem.SimulateConnect(bluetooth.Device{Address: "AA:BB:CC:DD:EE:FF", Name: "Test source"}, bluetooth.MediaState{
		Present: true, Playing: true, Title: "Scarlet Fire", Artist: "Otis McDonald",
		Album: "Single", DurationMS: 215000, PositionMS: 42000,
	})
	listPlayers := func() []*mediav1.PlayerStatus {
		t.Helper()
		req, _ := proto.Marshal(&mediav1.ListPlayersRequest{})
		reply := routedCall(t, conn, "org.capsula.media", mediaService, "ListPlayers", [][]byte{req})
		if len(reply) != 1 {
			t.Fatalf("ListPlayers returned %d messages", len(reply))
		}
		var list mediav1.ListPlayersResponse
		if err := proto.Unmarshal(reply[0], &list); err != nil {
			t.Fatal(err)
		}
		return list.GetPlayers()
	}
	var btPlayer *mediav1.PlayerStatus
	waitFor(t, 5*time.Second, "bluetooth player", func() bool {
		for _, player := range listPlayers() {
			if player.GetPlayerId() == "bluetooth" {
				btPlayer = player
				return true
			}
		}
		return false
	})
	if btPlayer.GetTitle() != "Scarlet Fire" || btPlayer.GetArtist() != "Otis McDonald" {
		t.Fatalf("bluetooth metadata = %+v", btPlayer)
	}
	if btPlayer.GetPlaybackState() != mediav1.PlaybackState_PLAYBACK_PLAYING {
		t.Fatalf("bluetooth state = %v", btPlayer.GetPlaybackState())
	}

	btReq, _ := proto.Marshal(&mediav1.PlayerRequest{PlayerId: "bluetooth"})
	raw = routedCall(t, conn, "org.capsula.media", mediaService, "Pause", [][]byte{btReq})
	if err := proto.Unmarshal(raw[0], &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.GetAccepted() {
		t.Fatalf("bluetooth pause rejected: %s", accepted.GetDetail())
	}
	state, err := mem.Media(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Playing {
		t.Fatal("bluetooth pause did not reach the source")
	}

	mem.SimulateDisconnect("AA:BB:CC:DD:EE:FF")
	waitFor(t, 5*time.Second, "bluetooth player removal", func() bool {
		for _, player := range listPlayers() {
			if player.GetPlayerId() == "bluetooth" {
				return false
			}
		}
		return true
	})
}
