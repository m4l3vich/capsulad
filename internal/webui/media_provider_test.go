package webui

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	mediav1 "capsulad/gen/go/capsulad/contracts/media/v1"
	frameworkv1 "capsulad/gen/go/capsulad/framework/v1"
	webv1 "capsulad/gen/go/capsulad/web/v1"
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
		// The sink's own packet receipts prove playback; Playing stays false
		// because a MediaControl1-only source publishes no AVRCP status.
		ObservedPlaying: true,
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

var errUnreadable = errors.New("player list unavailable")

func overviewProvider(overview *webv1.OverviewView, kind webv1.MediaProviderKind) *webv1.MediaProviderView {
	for _, provider := range overview.GetProviders() {
		if provider.GetKind() == kind {
			return provider
		}
	}
	return nil
}

func mediaPlayer(id string, state mediav1.PlaybackState, title string) *mediav1.PlayerStatus {
	return &mediav1.PlayerStatus{
		PlayerId: id, PlaybackState: state, Title: protoString(title),
		SupportedActions: []mediav1.MediaAction{
			mediav1.MediaAction_MEDIA_PLAY, mediav1.MediaAction_MEDIA_PAUSE,
			mediav1.MediaAction_MEDIA_SKIP_NEXT, mediav1.MediaAction_MEDIA_SKIP_PREVIOUS,
		},
	}
}

// waitOverview reads overview events until check accepts one. The read
// deadline turns a missing live update into a test failure instead of a hung
// suite.
func waitOverview(t *testing.T, conn *wsTestConn, check func(*webv1.OverviewView) bool) *webv1.OverviewView {
	t.Helper()
	if err := conn.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.conn.SetReadDeadline(time.Time{}) }()
	for {
		frame := conn.read()
		if overview := frame.GetEvent().GetOverview(); overview != nil && check(overview) {
			return overview
		}
	}
}

// TestOverviewNowPlayingKeepsOnlyPlayingPlayers is the projection regression:
// a paused sibling and a stopped provider must not become rows, while a
// playing sibling stays visible. The last transition to paused leaves the
// overview with no media at all.
func TestOverviewNowPlayingKeepsOnlyPlayingPlayers(t *testing.T) {
	env := newTestEnv(t)
	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("paused", mediav1.PlaybackState_PLAYBACK_PAUSED, "Paused track"),
		mediaPlayer("playing", mediav1.PlaybackState_PLAYBACK_PLAYING, "Playing track"),
	)
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 80, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 80).GetOverview()
	if len(overview.GetProviders()) != 1 {
		t.Fatalf("providers = %v, want the one provider with a playing player", overview.GetProviders())
	}
	provider := overview.GetProviders()[0]
	if provider.GetAppId() != "org.example.player" {
		t.Fatalf("provider = %v", provider)
	}
	players := provider.GetPlayers()
	if len(players) != 1 || players[0].GetPlayerId() != "playing" ||
		players[0].GetPlaybackState() != "PLAYBACK_PLAYING" {
		t.Fatalf("players = %v, want only the playing sibling", players)
	}
	raw, err := proto.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("Paused track")) {
		t.Fatalf("overview carries a paused player: %q", raw)
	}

	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("paused", mediav1.PlaybackState_PLAYBACK_PAUSED, "Paused track"),
		mediaPlayer("playing", mediav1.PlaybackState_PLAYBACK_PAUSED, "Playing track"),
	)
	conn.send(&webv1.ClientFrame{RequestId: 81, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview = conn.waitSnapshot(t, 81).GetOverview()
	if len(overview.GetProviders()) != 0 {
		t.Fatalf("providers = %v, want none after the final player paused", overview.GetProviders())
	}
}

// TestOverviewNowPlayingOmitsUnreadableProvider proves an errored provider is
// provider discovery, not a Now Playing row: the summary is empty rather than
// an error card the user cannot act on.
func TestOverviewNowPlayingOmitsUnreadableProvider(t *testing.T) {
	env := newTestEnv(t)
	env.back.mu.Lock()
	env.back.mediaErr = map[string]error{"org.example.player": errUnreadable}
	env.back.mu.Unlock()
	conn, _, _ := connectSession(t, env)

	conn.send(&webv1.ClientFrame{RequestId: 82, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 82).GetOverview()
	if len(overview.GetProviders()) != 0 {
		t.Fatalf("providers = %v, want none when the provider cannot be read", overview.GetProviders())
	}
}

// TestOverviewNowPlayingOmitsBluetoothCommandInference is the strictness
// regression: `direction_playing` can be true from command memory alone, so an
// acknowledged Play the phone has not acted on must not create a row. Only
// observed playback (here, not observed) qualifies.
func TestOverviewNowPlayingOmitsBluetoothCommandInference(t *testing.T) {
	env := newTestEnv(t)
	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("local.player", mediav1.PlaybackState_PLAYBACK_STOPPED, "Aurora"))
	env.back.setBluetoothMedia(&frameworkv1.BluetoothMediaState{
		Present: true, Playing: false, DeviceName: "Phone", Title: "Scarlet Fire",
		DirectionKnown: true, DirectionPlaying: true, ObservedPlaying: false,
		Revision: 9,
	})
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 83, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 83).GetOverview()
	if len(overview.GetProviders()) != 0 {
		t.Fatalf("providers = %v, want none for command-memory direction only", overview.GetProviders())
	}
}

// TestOverviewNowPlayingShowsObservedBluetoothPlayback keeps the
// MediaControl1-only phone: it publishes no AVRCP `playing`, but sink receipts
// make playback observed, so it is Now Playing with its transport actions.
func TestOverviewNowPlayingShowsObservedBluetoothPlayback(t *testing.T) {
	env := newTestEnv(t)
	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("local.player", mediav1.PlaybackState_PLAYBACK_STOPPED, "Aurora"))
	env.back.setBluetoothMedia(connectedBluetoothSource())
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 84, Body: &webv1.ClientFrame_SnapshotRequest{
		SnapshotRequest: &webv1.SnapshotRequest{Topic: webv1.Topic_TOPIC_OVERVIEW},
	}})
	overview := conn.waitSnapshot(t, 84).GetOverview()
	bluetooth := overviewProvider(overview, webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA)
	if bluetooth == nil || len(bluetooth.GetPlayers()) != 1 {
		t.Fatalf("Bluetooth provider = %v, want one observed playing player", bluetooth)
	}
	player := bluetooth.GetPlayers()[0]
	if player.GetPlaybackState() != "PLAYBACK_PLAYING" || player.GetTitle() != "Scarlet Fire" {
		t.Fatalf("Bluetooth player = %v", player)
	}
}

// TestOverviewNowPlayingFollowsProviderPlayback covers the dev-media flow and
// the radio/AirPlay lifecycle: a READY provider with no playing player is
// absent, starts playing outside the browser, then disappears again when it
// pauses. The WatchPlayers feed is the only trigger; no polling rebuilds the
// page.
func TestOverviewNowPlayingFollowsProviderPlayback(t *testing.T) {
	env := newTestEnv(t)
	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("local.player", mediav1.PlaybackState_PLAYBACK_STOPPED, "Aurora"))
	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 85, Body: &webv1.ClientFrame_Subscribe{Subscribe: &webv1.Subscribe{
		Topic: webv1.Topic_TOPIC_OVERVIEW,
	}}})
	initial := conn.waitSnapshot(t, 85).GetOverview()
	if len(initial.GetProviders()) != 0 {
		t.Fatalf("initial providers = %v, want none while the fixture is stopped", initial.GetProviders())
	}

	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("local.player", mediav1.PlaybackState_PLAYBACK_PLAYING, "Aurora"))
	playing := waitOverview(t, conn, func(overview *webv1.OverviewView) bool {
		provider := overviewProvider(overview, webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_APP)
		return provider != nil && len(provider.GetPlayers()) == 1 &&
			provider.GetPlayers()[0].GetPlaybackState() == "PLAYBACK_PLAYING"
	})
	if title := playing.GetProviders()[0].GetPlayers()[0].GetTitle(); title != "Aurora" {
		t.Fatalf("live player title = %q, want Aurora", title)
	}

	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("local.player", mediav1.PlaybackState_PLAYBACK_PAUSED, "Aurora"))
	waitOverview(t, conn, func(overview *webv1.OverviewView) bool {
		return len(overview.GetProviders()) == 0
	})
}

// TestOverviewNowPlayingFollowsBluetoothPlayback covers the source transition
// without a browser command: a phone starts and stops playing while the
// Overview is open.
func TestOverviewNowPlayingFollowsBluetoothPlayback(t *testing.T) {
	env := newTestEnv(t)
	env.back.setMediaPlayers("org.example.player",
		mediaPlayer("local.player", mediav1.PlaybackState_PLAYBACK_STOPPED, "Aurora"))
	paused := connectedBluetoothSource()
	paused.ObservedPlaying, paused.Playing, paused.DirectionPlaying = false, false, false
	env.back.setBluetoothMedia(paused)

	conn, _, _ := connectSession(t, env)
	conn.send(&webv1.ClientFrame{RequestId: 86, Body: &webv1.ClientFrame_Subscribe{Subscribe: &webv1.Subscribe{
		Topic: webv1.Topic_TOPIC_OVERVIEW,
	}}})
	initial := conn.waitSnapshot(t, 86).GetOverview()
	if len(initial.GetProviders()) != 0 {
		t.Fatalf("initial providers = %v, want none while the source is paused", initial.GetProviders())
	}

	env.back.setBluetoothMedia(connectedBluetoothSource())
	waitOverview(t, conn, func(overview *webv1.OverviewView) bool {
		provider := overviewProvider(overview, webv1.MediaProviderKind_MEDIA_PROVIDER_KIND_FRAMEWORK_BLUETOOTH_MEDIA)
		return provider != nil && len(provider.GetPlayers()) == 1
	})

	paused = connectedBluetoothSource()
	paused.ObservedPlaying, paused.Playing, paused.DirectionPlaying = false, false, false
	env.back.setBluetoothMedia(paused)
	waitOverview(t, conn, func(overview *webv1.OverviewView) bool {
		return len(overview.GetProviders()) == 0
	})
}
