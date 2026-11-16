package webui

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	frameworkv1 "capsulad/gen/go/capsula/framework/v1"
	webv1 "capsulad/gen/go/capsula/web/v1"
)

// connectedBluetoothSource is a playing phone that exposes AVRCP metadata, as
// the framework's BluetoothMediaService reports it. The resolved direction is
// what a source exposing only MediaControl1 can never publish as `playing`.
func connectedBluetoothSource() *frameworkv1.BluetoothMediaState {
	return &frameworkv1.BluetoothMediaState{
		Present:          true,
		Playing:          false,
		DeviceAddress:    "AA:BB:CC:DD:EE:FF",
		DeviceName:       "Phone",
		Title:            "Scarlet Fire",
		Artist:           "Otis McDonald",
		Album:            "Single",
		DurationMs:       215000,
		PositionMs:       42000,
		Revision:         7,
		DirectionKnown:   true,
		DirectionPlaying: true,
	}
}

// TestOverviewRendersConnectedBluetoothSource is the regression for the field
// report: a connected phone was playing while the Web UI said "Nothing is
// playing", because the only Bluetooth provider was a bundled application whose
// discovery depended on the app registry and a live run. The source is now a
// framework provider read from BluetoothMediaService, so a connected source
// renders one player with its metadata and transport actions.
func TestOverviewRendersConnectedBluetoothSource(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.btMedia = connectedBluetoothSource()
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 70, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 70).GetOverview()

	var bluetooth *webv1.MediaProviderView
	for _, provider := range overview.GetProviders() {
		if provider.GetKind() == webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA {
			bluetooth = provider
		}
	}
	if bluetooth == nil {
		t.Fatalf("overview has no framework Bluetooth provider: %v", overview.GetProviders())
	}
	if bluetooth.GetAppId() != "" {
		t.Fatalf("framework provider pretends to be an app: app_id %q", bluetooth.GetAppId())
	}
	if bluetooth.GetName() != "Bluetooth" {
		t.Fatalf("framework provider name = %q", bluetooth.GetName())
	}
	players := bluetooth.GetPlayers()
	if len(players) != 1 {
		t.Fatalf("framework provider players = %v, want one", players)
	}
	player := players[0]
	if player.GetPlayerId() != "bluetooth" {
		t.Fatalf("player id = %q", player.GetPlayerId())
	}
	// The source publishes no AVRCP status (Playing=false); the resolved
	// direction is playing, and that is what the transport buttons must show.
	if player.GetPlaybackState() != "PLAYBACK_PLAYING" {
		t.Fatalf("playback state = %q, want PLAYBACK_PLAYING from the resolved direction", player.GetPlaybackState())
	}
	if player.GetTitle() != "Scarlet Fire" || player.GetArtist() != "Otis McDonald" ||
		player.GetAlbum() != "Single" {
		t.Fatalf("metadata = %q/%q/%q", player.GetTitle(), player.GetArtist(), player.GetAlbum())
	}
	if player.GetSourceName() != "Phone" || player.GetAppName() != "Bluetooth" {
		t.Fatalf("identity = %q/%q", player.GetAppName(), player.GetSourceName())
	}
	if player.GetDurationMs() != 215000 || player.GetPositionMs() != 42000 || player.GetRevision() != 7 {
		t.Fatalf("position/revision = %d/%d/%d",
			player.GetPositionMs(), player.GetDurationMs(), player.GetRevision())
	}
	wantActions := []string{"PLAY", "PAUSE", "SKIP_NEXT", "SKIP_PREVIOUS"}
	got := player.GetSupportedActions()
	if len(got) != len(wantActions) {
		t.Fatalf("supported actions = %v", got)
	}
	for i, want := range wantActions {
		if got[i] != want {
			t.Fatalf("supported actions = %v, want %v", got, wantActions)
		}
	}

	// The browser boundary: the source's address never crosses the socket.
	raw, err := proto.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("AA:BB:CC:DD:EE:FF")) {
		t.Fatalf("overview leaks the Bluetooth address: %q", raw)
	}

	// A third-party provider keeps its application identity and is still
	// listed exactly as before.
	var app *webv1.MediaProviderView
	for _, provider := range overview.GetProviders() {
		if provider.GetAppId() == "org.example.player" {
			app = provider
		}
	}
	if app == nil || app.GetKind() != webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_APP {
		t.Fatalf("application provider = %v", overview.GetProviders())
	}
}

// TestOverviewOmitsBluetoothProviderWithoutSource proves the framework provider
// is not a permanent empty row: with no connected source the overview carries no
// Bluetooth provider at all.
func TestOverviewOmitsBluetoothProviderWithoutSource(t *testing.T) {
	env := newTestEnv(t)
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 71, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 71).GetOverview()
	for _, provider := range overview.GetProviders() {
		if provider.GetKind() == webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA {
			t.Fatalf("overview lists a Bluetooth provider without a source: %v", provider)
		}
	}
	if len(overview.GetProviders()) != 1 || overview.GetProviders()[0].GetAppId() != "org.example.player" {
		t.Fatalf("providers = %v, want only the running application provider", overview.GetProviders())
	}
}

// TestFrameworkMediaCommandUsesBluetoothService proves the browser's transport
// buttons for the framework provider reach BluetoothMediaService.Command rather
// than routing to an application that does not exist.
func TestFrameworkMediaCommandUsesBluetoothService(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.btMedia = connectedBluetoothSource()
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 72, Body: &webv1.ClientFrame_Command{Command: &webv1.Command{
		Op: &webv1.Command_Media{Media: &webv1.MediaCommand{
			Kind:     webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA,
			PlayerId: "bluetooth",
			Action:   webv1.MediaAction_MEDIA_ACTION_PAUSE,
		}},
	}}})
	result := conn.waitResult(t, 72)
	if media := result.GetMedia(); media == nil || !media.GetAccepted() {
		t.Fatalf("media result = %v", result)
	}
	env.back.mu.Lock()
	commands := append([]frameworkv1.BluetoothMediaCommand(nil), env.back.btCommands...)
	env.back.mu.Unlock()
	if len(commands) != 1 || commands[0] != frameworkv1.BluetoothMediaCommand_BLUETOOTH_MEDIA_PAUSE {
		t.Fatalf("Bluetooth commands = %v, want one PAUSE", commands)
	}
}
