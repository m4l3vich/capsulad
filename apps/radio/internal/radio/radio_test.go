package radio

import (
	"context"
	"testing"
	"time"

	"capsulad/sdk/capsulad"
)

func settingsChanged(revision uint64, values map[string]string) *capsulad.SettingsChanged {
	event := &capsulad.SettingsChanged{Revision: revision}
	for key, value := range values {
		event.Values = append(event.Values, capsulad.SettingValue{Key: key, ValueJSON: value})
	}
	return event
}

func testApp(t *testing.T) *App {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &App{
		ctx:              ctx,
		cancel:           cancel,
		logf:             t.Logf,
		phase:            PhaseStopped,
		stations:         ParseStations(DefaultStations, nil),
		stationNumber:    1,
		pendingStations:  make(map[int]int),
		recentWrites:     make(map[uint64]int),
		settingsReady:    make(chan struct{}),
		speakerAvailable: true,
		watchers:         make(map[uint64]chan struct{}),
		drawCh:           make(chan struct{}, 1),
		displayDone:      make(chan struct{}),
	}
}

func TestParseStations(t *testing.T) {
	text := "# comment\n\nSomaFM | https://example.test/a.mp3\n" +
		"https://example.test/b.mp3\n" +
		"bad line\n" +
		"ftp://example.test/d.mp3\n" +
		"Radio Paradise | https://example.test/c.mp3\n" +
		"NameOnly | not-a-url\n"
	stations := ParseStations(text, nil)
	if len(stations) != 3 {
		t.Fatalf("stations = %d, want 3: %+v", len(stations), stations)
	}
	if stations[0].Name != "SomaFM" || stations[0].URL != "https://example.test/a.mp3" {
		t.Fatalf("station 1 = %+v", stations[0])
	}
	if stations[1].Name != "https://example.test/b.mp3" || stations[1].URL != stations[1].Name {
		t.Fatalf("bare URL station = %+v", stations[1])
	}
	if stations[2].Name != "Radio Paradise" || stations[2].URL != "https://example.test/c.mp3" {
		t.Fatalf("named station = %+v", stations[2])
	}
}

// A name containing a pipe is not part of the "Name | URL" contract: the
// separator is the first pipe and everything after it must be the http(s) URL,
// exactly as the original setting parser behaved. Such a line is skipped.
func TestParseStationsRejectsPipeInName(t *testing.T) {
	stations := ParseStations("A | B | https://example.test/c.mp3", nil)
	if len(stations) != 0 {
		t.Fatalf("stations = %+v, want none", stations)
	}
}

func TestParseStationsTruncatesAtLimit(t *testing.T) {
	text := ""
	for i := 0; i < MaxStations+5; i++ {
		text += "S | https://example.test/" + formatInt(i) + ".mp3\n"
	}
	if got := len(ParseStations(text, nil)); got != MaxStations {
		t.Fatalf("stations = %d, want %d", got, MaxStations)
	}
}

func TestSelectTextAndStep(t *testing.T) {
	stations := []Station{
		{Name: "SomaFM", URL: "https://a.test/1.mp3"},
		{Name: "Radio Paradise", URL: "https://b.test/2.mp3"},
	}
	if number, ok := SelectText(stations, "2"); !ok || number != 2 {
		t.Fatalf("numeric selection = %d/%t", number, ok)
	}
	if number, ok := SelectText(stations, "radio paradise"); !ok || number != 2 {
		t.Fatalf("name selection = %d/%t", number, ok)
	}
	if number, ok := SelectText(stations, "https://a.test/1.mp3"); !ok || number != 1 {
		t.Fatalf("url selection = %d/%t", number, ok)
	}
	if _, ok := SelectText(stations, "missing"); ok {
		t.Fatal("unknown station selected")
	}
	if got := StepNumber(1, -1, 2); got != 2 {
		t.Fatalf("wrap previous = %d, want 2", got)
	}
	if got := StepNumber(2, 1, 2); got != 1 {
		t.Fatalf("wrap next = %d, want 1", got)
	}
	if got := StepNumber(0, 1, 2); got != 2 {
		t.Fatalf("out-of-range step = %d, want 2", got)
	}
}

func TestSettingsUpdateSelectionWithoutPlayback(t *testing.T) {
	a := testApp(t)
	a.HandleSettings(settingsChanged(1, map[string]string{
		"stations": `"# comment\nA | https://a.test/a.mp3\nB | https://b.test/b.mp3"`,
		"station":  "2",
		"autoplay": "false",
	}))
	a.mu.Lock()
	number, count, phase := a.stationNumber, len(a.stations), a.phase
	persisted := a.persistedStation
	a.mu.Unlock()
	if number != 2 || count != 2 || phase != PhaseStopped {
		t.Fatalf("selection=%d stations=%d phase=%v", number, count, phase)
	}
	if persisted != 2 {
		t.Fatalf("persistedStation = %d, want 2", persisted)
	}
	if got := a.missingStationReason(); got != "" {
		t.Fatalf("unexpected missing reason %q", got)
	}
}

func TestStationBeyondListIsReportedNotClamped(t *testing.T) {
	a := testApp(t)
	a.HandleSettings(settingsChanged(1, map[string]string{
		"stations": `"A | https://a.test/a.mp3"`,
		"station":  "9",
	}))
	a.mu.Lock()
	number, count := a.stationNumber, len(a.stations)
	_, hasStation := a.currentStationLocked()
	a.mu.Unlock()
	if number != 9 || count != 1 || hasStation {
		t.Fatalf("station=%d count=%d hasStation=%t", number, count, hasStation)
	}
	if got := a.missingStationReason(); got != "station 9 is not in the 1 configured stations" {
		t.Fatalf("missing reason = %q", got)
	}
}

func TestStationOutsideManifestBoundsKeepsPrevious(t *testing.T) {
	a := testApp(t)
	a.HandleSettings(settingsChanged(1, map[string]string{"station": "0"}))
	a.HandleSettings(settingsChanged(2, map[string]string{"station": "100"}))
	a.mu.Lock()
	number := a.stationNumber
	a.mu.Unlock()
	if number != 1 {
		t.Fatalf("station = %d, want the previous 1", number)
	}
}

func TestStaleSessionFailureCannotClearNewerSession(t *testing.T) {
	a := testApp(t)
	stations := []Station{{Name: "A", URL: "https://a.test/a.mp3"}}

	a.mu.Lock()
	first := a.startSessionLocked(stations[0], "first")
	a.mu.Unlock()
	a.mu.Lock()
	second := a.startSessionLocked(stations[0], "second")
	a.mu.Unlock()
	defer first.stop()
	defer second.stop()

	a.failSession(first, "late failure from the old session")
	a.mu.Lock()
	current, phase := a.session, a.phase
	a.mu.Unlock()
	if current != second || phase != PhaseBuffering {
		t.Fatalf("stale failure changed the radio: session=%p phase=%v", current, phase)
	}

	a.finishSession(first)
	a.mu.Lock()
	current = a.session
	a.mu.Unlock()
	if current != second {
		t.Fatalf("stale completion cleared the newer session")
	}

	a.failSession(second, "speaker failed")
	a.mu.Lock()
	current, phase, detail := a.session, a.phase, a.detail
	a.mu.Unlock()
	if current != nil || phase != PhaseError || detail != "speaker failed" {
		t.Fatalf("current failure not applied: session=%v phase=%v detail=%q", current, phase, detail)
	}
}

func TestStaleOwnWriteEchoDoesNotRevertNewerSelection(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stations = []Station{
		{Name: "A", URL: "https://a.test/a.mp3"},
		{Name: "B", URL: "https://b.test/b.mp3"},
		{Name: "C", URL: "https://c.test/c.mp3"},
	}
	a.stationNumber = 3
	a.pendingStations[2] = 1
	a.recentWrites[7] = 2
	a.mu.Unlock()

	a.HandleSettings(settingsChanged(7, map[string]string{"station": "2"}))
	a.mu.Lock()
	number := a.stationNumber
	a.mu.Unlock()
	if number != 3 {
		t.Fatalf("station reverted to %d, want the newer selection 3", number)
	}
}

func TestExternalStationChangeStillApplies(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stations = []Station{
		{Name: "A", URL: "https://a.test/a.mp3"},
		{Name: "B", URL: "https://b.test/b.mp3"},
	}
	a.stationNumber = 1
	a.mu.Unlock()
	a.HandleSettings(settingsChanged(12, map[string]string{"station": "2"}))
	a.mu.Lock()
	number := a.stationNumber
	a.mu.Unlock()
	if number != 2 {
		t.Fatalf("external selection = %d, want 2", number)
	}
}

func TestMediaStatusIdleStillExposesSelection(t *testing.T) {
	a := testApp(t)
	status := a.mediaStatus()
	if status["playbackState"] != "PLAYBACK_STOPPED" {
		t.Fatalf("state = %v", status["playbackState"])
	}
	if status["playerId"] != "radio" || status["appName"] != "Internet radio" {
		t.Fatalf("identity = %v / %v", status["playerId"], status["appName"])
	}
	if status["sourceName"] != "SomaFM Groove Salad" {
		t.Fatalf("sourceName = %v", status["sourceName"])
	}
	actions, ok := status["supportedActions"].([]any)
	if !ok || len(actions) != 3 || actions[0] != "MEDIA_PLAY" {
		t.Fatalf("supportedActions = %v", status["supportedActions"])
	}
}

func TestMediaStatusPlayingUsesMetadata(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.phase = PhasePlaying
	a.nowArtist = "Artist"
	a.nowTitle = "Title"
	a.revision = 7
	a.mu.Unlock()
	status := a.mediaStatus()
	if status["playbackState"] != "PLAYBACK_PLAYING" || status["title"] != "Title" ||
		status["artist"] != "Artist" || status["mediaName"] != "Title" || status["revision"] != "7" {
		t.Fatalf("status = %#v", status)
	}
	actions, _ := status["supportedActions"].([]any)
	if len(actions) != 3 || actions[0] != "MEDIA_PAUSE" {
		t.Fatalf("supportedActions = %v", status["supportedActions"])
	}
}

func TestSessionIdleForUsesMonotonicProgress(t *testing.T) {
	a := testApp(t)
	s := newSession(a, 1, Station{Name: "A", URL: "https://a.test/a.mp3"})
	defer s.stop()
	if s.idleFor() > time.Second {
		t.Fatalf("fresh session idleFor = %v", s.idleFor())
	}
	s.touch()
	if s.idleFor() > time.Second {
		t.Fatalf("touched session idleFor = %v", s.idleFor())
	}
}

func TestChunkBytes(t *testing.T) {
	if got := chunkBytes(48000); got != 38400 {
		t.Fatalf("chunkBytes(48000) = %d, want 38400", got)
	}
	if got := chunkBytes(8000); got != 6400 {
		t.Fatalf("chunkBytes(8000) = %d, want 6400", got)
	}
	if got := chunkBytes(192000); got != 153600 {
		t.Fatalf("chunkBytes(192000) = %d, want 153600", got)
	}
	if got := chunkBytes(1000000); got != maxChunkBytes {
		t.Fatalf("chunkBytes(large) = %d", got)
	}
}