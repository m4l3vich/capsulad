package radio

import (
	"context"
	"fmt"
	"sync"
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
	a := &App{
		ctx:              ctx,
		cancel:           cancel,
		logf:             t.Logf,
		phase:            PhaseStopped,
		stations:         ParseStations(DefaultStations, nil),
		stationNumber:    1,
		now:              time.Now,
		displayWindow:    defaultDisplayWindow,
		pendingStations:  make(map[int]int),
		recentWrites:     make(map[uint64]int),
		settingsReady:    make(chan struct{}),
		speakerAvailable: true,
		watchers:         make(map[uint64]chan struct{}),
		drawCh:           make(chan struct{}, 1),
		displayDone:      make(chan struct{}),
		exitCh:           make(chan struct{}),
		exitWake:         make(chan struct{}, 1),
	}
	go a.exitLoop()
	return a
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

// Dismiss can become routable as soon as AppReady, before the asynchronous
// settings snapshot arrives. With no explicit selection, the in-memory launch
// default must not be flushed over the station saved in the daemon: the app has
// no revision or dirty selection to justify a compare-and-swap write.
func TestEarlyDismissDoesNotPersistUntouchedSelection(t *testing.T) {
	a := testApp(t)
	writes := make(chan struct{}, 1)
	a.updateStationFn = func(context.Context, int, uint64) (uint64, error) {
		writes <- struct{}{}
		return 0, nil
	}

	if ok, _ := a.Control("dismiss"); !ok {
		t.Fatal("dismiss was refused before settings ready")
	}
	close(a.displayDone)
	a.Close()

	select {
	case <-writes:
		t.Fatal("early Dismiss overwrote the saved station with the launch default")
	default:
	}
	a.mu.Lock()
	number, dirty, revision := a.stationNumber, a.selectionDirty, a.settingsRevision
	a.mu.Unlock()
	if number != 1 || dirty || revision != 0 {
		t.Fatalf("early Dismiss state = station %d dirty %t revision %d, want untouched defaults", number, dirty, revision)
	}
}

// An explicit selection made before the first snapshot is kept, but it is not
// written with expected revision zero: that would be an unconditional write
// the app cannot compare-and-swap. Once the snapshot supplies a revision, the
// pending selection is written against it.
func TestDirtySelectionWaitsForInitializedRevision(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stations = []Station{
		{Name: "A", URL: "https://a.test/a.mp3"},
		{Name: "B", URL: "https://b.test/b.mp3"},
	}
	a.mu.Unlock()
	if ok, detail := a.Control("next"); !ok || detail != "selected B" {
		t.Fatalf("next = %t %q, want the explicit selection B", ok, detail)
	}
	a.mu.Lock()
	dirty := a.selectionDirty
	a.mu.Unlock()
	if !dirty {
		t.Fatal("next did not mark the selection dirty")
	}

	type write struct {
		station  int
		revision uint64
	}
	writes := make(chan write, 1)
	a.updateStationFn = func(_ context.Context, station int, revision uint64) (uint64, error) {
		writes <- write{station: station, revision: revision}
		return revision + 1, nil
	}

	a.flushSelection(50 * time.Millisecond)
	select {
	case got := <-writes:
		t.Fatalf("uninitialized revision wrote station %d with revision %d", got.station, got.revision)
	default:
	}

	a.HandleSettings(settingsChanged(5, map[string]string{"station": "1"}))
	select {
	case got := <-writes:
		if got.station != 2 || got.revision != 5 {
			t.Fatalf("pending write = station %d revision %d, want station 2 revision 5", got.station, got.revision)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the snapshot did not retry the pending selection")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		number, persisted, dirty := a.stationNumber, a.persistedStation, a.selectionDirty
		a.mu.Unlock()
		if number == 2 && persisted == 2 && !dirty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending selection = station %d persisted %d dirty %t, want 2/2/false", number, persisted, dirty)
		}
		time.Sleep(time.Millisecond)
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
func TestDisplayWindowShowsStationThenReleasesBaseline(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()

	state := a.displayState()
	if !state.displayUntil.Equal(start.Add(defaultDisplayWindow)) {
		t.Fatalf("displayUntil = %v, want %v", state.displayUntil, start.Add(defaultDisplayWindow))
	}
	out := displayOutputFor(state, start)
	if !out.hold {
		t.Fatal("tuning must show the station number immediately")
	}
	if out.colon {
		t.Fatal("the station number must not light the colon")
	}
	if len(out.cells) != 4 || out.cells[2].Mask != segmentDigits[0] || out.cells[3].Mask != segmentDigits[1] {
		t.Fatalf("frame cells = %+v, want station 1", out.cells)
	}

	if !displayOutputFor(state, start.Add(defaultDisplayWindow-time.Millisecond)).hold {
		t.Fatal("ownership ended before the 10 second window")
	}
	released := displayOutputFor(state, start.Add(defaultDisplayWindow))
	if released.hold || released.cells != nil {
		t.Fatalf("after the window = %+v, want release without a frame", released)
	}

	// Releasing the display must not disturb playback state.
	a.mu.Lock()
	phase, session := a.phase, a.session
	a.mu.Unlock()
	if phase != PhaseBuffering || session != s {
		t.Fatalf("release changed audio state: phase=%v session=%p want %p", phase, session, s)
	}
}

func TestAlwaysDisplayKeepsStationNumberUntilStop(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.alwaysDisplay = true
	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()

	state := a.displayState()
	for _, at := range []time.Time{start, start.Add(defaultDisplayWindow), start.Add(24 * time.Hour)} {
		if !displayOutputFor(state, at).hold {
			t.Fatalf("always display released at %v", at)
		}
	}
}

func TestToggleAlwaysDisplayDuringRunUsesTuningDeadline(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()

	state := a.displayState()
	state.alwaysDisplay = true
	if !displayOutputFor(state, start.Add(time.Hour)).hold {
		t.Fatal("turning always-display on must keep the number")
	}
	state.alwaysDisplay = false
	if !displayOutputFor(state, start.Add(5*time.Second)).hold {
		t.Fatal("turning it off before the deadline must keep the number until the tuning deadline")
	}
	if displayOutputFor(state, start.Add(defaultDisplayWindow)).hold {
		t.Fatal("turning it off must not extend the tuning deadline")
	}
}

func TestRetuneReplacesDisplayDeadline(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.mu.Lock()
	first := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "first")
	a.mu.Unlock()
	defer first.stop()

	secondStart := start.Add(9 * time.Second)
	a.mu.Lock()
	a.now = func() time.Time { return secondStart }
	second := a.startSessionLocked(Station{Name: "B", URL: "https://b.test/b.mp3"}, "second")
	a.mu.Unlock()
	defer second.stop()

	state := a.displayState()
	if !state.displayUntil.Equal(secondStart.Add(defaultDisplayWindow)) {
		t.Fatalf("retune deadline = %v, want %v", state.displayUntil, secondStart.Add(defaultDisplayWindow))
	}
	// The first attempt's old deadline must not release the newer one.
	if !displayOutputFor(state, start.Add(defaultDisplayWindow)).hold {
		t.Fatal("the first attempt's deadline released the retuned display")
	}
	if displayOutputFor(state, secondStart.Add(defaultDisplayWindow)).hold {
		t.Fatal("the retuned display outlived its own deadline")
	}
}

func TestErrorUsesTuningWindowThenBaseline(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	a.failSession(s, "speaker failed")

	state := a.displayState()
	if state.phase != PhaseError {
		t.Fatalf("phase = %v, want error", state.phase)
	}
	early := displayOutputFor(state, start.Add(time.Second))
	if !early.hold || early.colon {
		t.Fatalf("error frame = %+v, want dashes without colon", early)
	}
	if len(early.cells) != 4 {
		t.Fatalf("error cells = %d, want 4", len(early.cells))
	}
	for _, cell := range early.cells {
		if cell.Mask != segmentDash {
			t.Fatalf("error cell = %+v, want dash", cell)
		}
	}
	if displayOutputFor(state, start.Add(defaultDisplayWindow)).hold {
		t.Fatal("error kept the display past the tuning window")
	}
}

func TestLateErrorDoesNotReacquireReleasedDisplay(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()

	// The stream dies after the transient ownership window. The radio must not
	// grab the display again just to show the error pattern; with nothing left
	// to show, the run ends immediately.
	a.mu.Lock()
	a.displayUntil = start.Add(-time.Second)
	a.mu.Unlock()
	a.failSession(s, "late failure")
	state := a.displayState()
	if displayOutputFor(state, start.Add(defaultDisplayWindow+time.Second)).hold {
		t.Fatal("a late error reacquired the segment display")
	}
	select {
	case <-a.exitCh:
	case <-time.After(time.Second):
		t.Fatal("a late error did not end the run")
	}
}

func TestStopReleasesDisplayOwnership(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.alwaysDisplay = true
	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()

	a.stop("stop")
	state := a.displayState()
	if state.phase != PhaseStopped || !state.displayUntil.IsZero() {
		t.Fatalf("stop state = %+v", state)
	}
	if displayOutputFor(state, start).hold {
		t.Fatal("stopped radio kept display ownership")
	}
}

func TestDisplayFrameNeverLightsColon(t *testing.T) {
	until := time.Date(2026, 1, 1, 12, 0, 10, 0, time.UTC)
	now := until.Add(-time.Second)
	for _, phase := range []Phase{PhaseBuffering, PhasePlaying, PhasePaused, PhaseError} {
		state := displayState{phase: phase, station: 42, displayUntil: until}
		out := displayOutputFor(state, now)
		if !out.hold || out.colon {
			t.Fatalf("phase %v frame = %+v, want visible without colon", phase, out)
		}
		if len(out.cells) != 4 {
			t.Fatalf("phase %v cells = %d, want 4", phase, len(out.cells))
		}
		switch phase {
		case PhaseError:
			continue
		case PhasePaused:
			if out.cells[0].Mask != 0 || out.cells[1].Mask != segmentPauseLeft ||
				out.cells[2].Mask != segmentPauseRight || out.cells[3].Mask != 0 {
				t.Fatalf("paused cells = %+v, want the middle pause bars", out.cells)
			}
			for _, cell := range out.cells {
				if cell.DecimalPoint {
					t.Fatalf("paused cell = %+v, want no decimal point", cell)
				}
			}
		default:
			if out.cells[2].Mask != segmentDigits[4] || out.cells[3].Mask != segmentDigits[2] {
				t.Fatalf("phase %v cells = %+v, want station 42", phase, out.cells)
			}
			if out.cells[3].DecimalPoint {
				t.Fatalf("phase %v decimal point set", phase)
			}
		}
	}
}

func TestWaitForDisplayChangeWakesAtDeadlineWithoutDraw(t *testing.T) {
	a := testApp(t)
	// No draw can satisfy this wait; the future deadline alone must wake it.
	a.drawCh = make(chan struct{})
	state := displayState{
		phase:        PhasePlaying,
		station:      1,
		displayUntil: time.Now().Add(25 * time.Millisecond),
	}

	started := time.Now()
	woke := make(chan bool, 1)
	go func() { woke <- a.waitForDisplayChange(state) }()

	select {
	case result := <-woke:
		if !result {
			t.Fatal("waitForDisplayChange returned false for a live context")
		}
		if elapsed := time.Since(started); elapsed < 10*time.Millisecond {
			t.Fatalf("waitForDisplayChange returned after %v, before the future deadline", elapsed)
		}
	case <-time.After(2 * time.Second):
		// Release the goroutine before failing so the test leaves nothing
		// blocked on the empty draw channel.
		a.cancel()
		select {
		case <-woke:
		case <-time.After(time.Second):
			t.Fatal("waitForDisplayChange did not exit after cancellation")
		}
		t.Fatal("waitForDisplayChange did not wake at its future deadline")
	}
}

// --- display loop ownership proof ------------------------------------------

// fakeEventLog records release order. It is test-only; the production display
// host is unchanged.
type fakeEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *fakeEventLog) add(event string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *fakeEventLog) snapshot() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	events := make([]string, len(l.events))
	copy(events, l.events)
	return events
}

type fakeFrame struct {
	cells []segmentCell
	colon bool
}

type fakePresentation struct {
	id      string
	log     *fakeEventLog
	mu      sync.Mutex
	frames  []fakeFrame
	closed  bool
	doneCh  chan struct{}
	once    sync.Once
	frame   chan struct{}
	closedC chan struct{}
}

func newFakePresentation(id string, log *fakeEventLog) *fakePresentation {
	return &fakePresentation{
		id:      id,
		log:     log,
		doneCh:  make(chan struct{}),
		frame:   make(chan struct{}, 1),
		closedC: make(chan struct{}, 1),
	}
}

func (p *fakePresentation) presentationID() string { return p.id }
func (p *fakePresentation) done() <-chan struct{}  { return p.doneCh }
func (p *fakePresentation) send(cells []segmentCell, colon bool) error {
	p.mu.Lock()
	p.frames = append(p.frames, fakeFrame{cells: append([]segmentCell(nil), cells...), colon: colon})
	p.mu.Unlock()
	select {
	case p.frame <- struct{}{}:
	default:
	}
	return nil
}

func (p *fakePresentation) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.once.Do(func() {
		p.log.add("presentation")
		close(p.doneCh)
	})
	select {
	case p.closedC <- struct{}{}:
	default:
	}
}

func (p *fakePresentation) snapshot() ([]fakeFrame, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	frames := make([]fakeFrame, len(p.frames))
	copy(frames, p.frames)
	return frames, p.closed
}

type fakeCapture struct {
	log     *fakeEventLog
	doneCh  chan struct{}
	once    sync.Once
	closedC chan struct{}
}

func (c *fakeCapture) Done() <-chan struct{} { return c.doneCh }
func (c *fakeCapture) Close() {
	c.once.Do(func() {
		c.log.add("capture")
		close(c.doneCh)
	})
	select {
	case c.closedC <- struct{}{}:
	default:
	}
}

type fakeDisplayHost struct {
	opened   chan *fakePresentation
	captured chan *fakeCapture
	log      *fakeEventLog
}

func newFakeDisplayHost() *fakeDisplayHost {
	return &fakeDisplayHost{
		opened:   make(chan *fakePresentation, 1),
		captured: make(chan *fakeCapture, 1),
		log:      &fakeEventLog{},
	}
}

func (h *fakeDisplayHost) open(context.Context) (segmentSink, error) {
	p := newFakePresentation("presentation-1", h.log)
	h.opened <- p
	return p, nil
}

func (h *fakeDisplayHost) capture(context.Context, string) (captureSink, error) {
	c := &fakeCapture{
		log:     h.log,
		doneCh:  make(chan struct{}),
		closedC: make(chan struct{}, 1),
	}
	h.captured <- c
	return c, nil
}

func TestDisplayLoopReleasesWithoutSendingBlankFrame(t *testing.T) {
	a := testApp(t)
	host := newFakeDisplayHost()
	a.display = host
	// The initial acquisition must never race a short deadline under suite
	// load. Expiry is triggered explicitly once the presentation and its
	// dependent capture are known to be selected.
	a.displayWindow = time.Hour
	go a.displayLoop()

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()
	a.requestDraw()

	var presentation *fakePresentation
	select {
	case presentation = <-host.opened:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not open a presentation")
	}
	var capture *fakeCapture
	select {
	case capture = <-host.captured:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not acquire the dependent capture")
	}
	select {
	case <-presentation.frame:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not publish the station frame")
	}
	select {
	case <-capture.Done():
		t.Fatal("capture was released while the display window was open")
	default:
	}

	// Controlled expiry: the tuning deadline is now in the past, and the draw
	// wakes the loop so it re-evaluates without waiting on a wall-clock timer.
	a.mu.Lock()
	a.displayUntil = time.Now().Add(-time.Second)
	a.mu.Unlock()
	a.requestDraw()

	select {
	case <-presentation.closedC:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not release the presentation after the window")
	}
	select {
	case <-capture.closedC:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not release the dependent capture")
	}

	// Releasing the overlay must drop the dependent capture first, then the
	// presentation; a presentation left without its capture would keep the
	// display routed to the radio instead of restoring the baseline.
	if events := host.log.snapshot(); len(events) != 2 || events[0] != "capture" || events[1] != "presentation" {
		t.Fatalf("release order = %v, want [capture presentation]", events)
	}

	frames, closed := presentation.snapshot()
	if !closed {
		t.Fatal("presentation was not closed")
	}
	for _, frame := range frames {
		if len(frame.cells) == 0 || frame.colon {
			t.Fatalf("display frame = %+v, want a station frame and never a blank/colon frame", frame)
		}
	}

	a.cancel()
	select {
	case <-a.displayDone:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not stop")
	}
}

func TestDisplayLoopTogglesOffDuringActiveRun(t *testing.T) {
	a := testApp(t)
	host := newFakeDisplayHost()
	a.display = host
	a.displayWindow = 30 * time.Millisecond
	a.alwaysDisplay = true
	go a.displayLoop()

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()
	a.requestDraw()

	var presentation *fakePresentation
	select {
	case presentation = <-host.opened:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not open a presentation")
	}
	select {
	case <-presentation.frame:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not publish the station frame")
	}

	// Past the tuning window, "always display" must still hold the display.
	time.Sleep(100 * time.Millisecond)
	if _, closed := presentation.snapshot(); closed {
		t.Fatal("always-display released the station number")
	}

	a.mu.Lock()
	a.alwaysDisplay = false
	a.mu.Unlock()
	a.requestDraw()
	select {
	case <-presentation.closedC:
	case <-time.After(2 * time.Second):
		t.Fatal("turning always-display off did not release the expired window")
	}

	a.cancel()
	select {
	case <-a.displayDone:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not stop")
	}
}

func TestAlwaysDisplaySettingDoesNotDisturbPlaybackOrDeadline(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	deadline := a.displayUntil
	a.mu.Unlock()
	defer s.stop()

	a.HandleSettings(settingsChanged(2, map[string]string{"always_display": "true"}))
	a.mu.Lock()
	always, phase, session, until := a.alwaysDisplay, a.phase, a.session, a.displayUntil
	a.mu.Unlock()
	if !always || phase != PhaseBuffering || session != s || !until.Equal(deadline) {
		t.Fatalf("setting change disturbed the run: always=%t phase=%v session=%p until=%v", always, phase, session, until)
	}

	a.HandleSettings(settingsChanged(3, map[string]string{"always_display": "false"}))
	a.mu.Lock()
	always, phase, session = a.alwaysDisplay, a.phase, a.session
	a.mu.Unlock()
	if always || phase != PhaseBuffering || session != s {
		t.Fatalf("turning the setting off disturbed the run: always=%t phase=%v session=%p", always, phase, session)
	}
}

// --- idle run exit ----------------------------------------------------------

// A pause is a bounded state: the radio shows the pause sign for the whole
// hold, even with "Always display station number" on, and then ends the run.
func TestPauseShowsBarsThenExits(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.pauseWindow = 40 * time.Millisecond

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()

	if ok, detail := a.Control("pause"); !ok || detail != "paused" {
		t.Fatalf("pause = %t %q", ok, detail)
	}
	state := a.displayState()
	if state.phase != PhasePaused {
		t.Fatalf("phase = %v, want paused", state.phase)
	}
	if !state.displayUntil.Equal(start.Add(a.pauseWindow)) {
		t.Fatalf("pause deadline = %v, want %v", state.displayUntil, start.Add(a.pauseWindow))
	}
	out := displayOutputFor(state, start)
	if !out.hold || out.colon {
		t.Fatalf("paused output = %+v, want visible without colon", out)
	}
	if len(out.cells) != 4 || out.cells[1].Mask != segmentPauseLeft || out.cells[2].Mask != segmentPauseRight {
		t.Fatalf("paused cells = %+v, want the middle pause bars", out.cells)
	}
	for _, cell := range out.cells {
		if cell.DecimalPoint {
			t.Fatalf("paused cell = %+v, want no decimal point", cell)
		}
	}
	// The setting cannot turn the pause hold into a resident run.
	state.alwaysDisplay = true
	if !displayOutputFor(state, start.Add(a.pauseWindow-time.Millisecond)).hold {
		t.Fatal("paused display ended before the hold")
	}
	if displayOutputFor(state, start.Add(a.pauseWindow)).hold {
		t.Fatal("paused display outlived the hold")
	}

	select {
	case <-a.exitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("paused run did not signal its exit")
	}
	state = a.displayState()
	if state.phase != PhaseStopped || !a.exiting {
		t.Fatalf("expired pause = phase %v exiting %t, want stopped/true", state.phase, a.exiting)
	}
	if displayOutputFor(state, start).hold {
		t.Fatal("expired pause still owned the display")
	}
}

// Resume cancels the armed pause expiry through the same session transition
// a Play uses; the old timer must not end the newer stream.
func TestResumeCancelsPauseExpiry(t *testing.T) {
	a := testApp(t)
	a.pauseWindow = 150 * time.Millisecond

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	a.pause()
	s.stop()

	time.Sleep(20 * time.Millisecond)
	a.mu.Lock()
	resumed := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "resume")
	exitAt := a.exitAt
	a.mu.Unlock()
	defer resumed.stop()

	if !exitAt.IsZero() {
		t.Fatalf("resume left an expiry armed at %v", exitAt)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case <-a.exitCh:
		t.Fatal("the paused expiry ended the resumed run")
	default:
	}
	a.mu.Lock()
	phase, current := a.phase, a.session
	a.mu.Unlock()
	if phase != PhaseBuffering || current != resumed {
		t.Fatalf("resumed state = phase %v session %p, want buffering/%p", phase, current, resumed)
	}
}

// A late or duplicate wake must not restart the exit timer at full length.
// The loop always recomputes the remainder from the armed absolute deadline,
// so the pause sign and the run exit stay in step.
func TestExitTimerUsesRemainingHoldAfterWake(t *testing.T) {
	a := testApp(t)
	a.pauseWindow = 600 * time.Millisecond

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	a.pause()
	s.stop()
	start := time.Now()

	time.Sleep(350 * time.Millisecond)
	a.wakeExit()
	select {
	case <-a.exitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the stop hold did not expire")
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("late wake restarted the full hold: exit after %v, want about %v", elapsed, a.pauseWindow)
	}
}

// Resume during a playback Stop hold clears the halt marker, cancels the
// armed expiry, and reports a fresh buffering session.
func TestResumeCancelsStopHold(t *testing.T) {
	a := testApp(t)
	a.pauseWindow = 200 * time.Millisecond

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	if ok, _ := a.Control("stop"); !ok {
		t.Fatal("stop was refused")
	}
	s.stop()
	time.Sleep(20 * time.Millisecond)

	a.mu.Lock()
	resumed := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "resume")
	exitAt, halted := a.exitAt, a.halted
	a.mu.Unlock()
	defer resumed.stop()
	if !exitAt.IsZero() || halted {
		t.Fatalf("resume left stop hold: exitAt=%v halted=%t", exitAt, halted)
	}
	time.Sleep(250 * time.Millisecond)
	select {
	case <-a.exitCh:
		t.Fatal("the stop hold ended the resumed run")
	default:
	}
	if status := a.mediaStatus(); status["playbackState"] != "PLAYBACK_BUFFERING" {
		t.Fatalf("resumed playbackState = %v", status["playbackState"])
	}
}

// Stop during an existing pause converts the media state without extending
// the one pause deadline.
func TestStopDuringPauseKeepsDeadline(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.pauseWindow = 40 * time.Millisecond
	a.alwaysDisplay = true

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()
	if ok, _ := a.Control("pause"); !ok {
		t.Fatal("pause was refused")
	}
	before := a.displayState().displayUntil
	if ok, detail := a.Control("stop"); !ok || detail != "stopped" {
		t.Fatalf("stop after pause = %t %q", ok, detail)
	}
	state := a.displayState()
	if !state.displayUntil.Equal(before) {
		t.Fatalf("stop extended the pause deadline from %v to %v", before, state.displayUntil)
	}
	if !a.halted {
		t.Fatal("stop after pause did not mark the stopped media state")
	}
}

// A settings edit during a playback Stop hold updates the stored selection but
// must not start audio or cancel the hold: only Play resumes a stopped radio.
func TestSettingsChangeDoesNotRetuneHaltedStopHold(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.pauseWindow = time.Minute
	a.mu.Lock()
	a.stations = []Station{
		{Name: "A", URL: "https://a.test/a.mp3"},
		{Name: "B", URL: "https://b.test/b.mp3"},
	}
	s := a.startSessionLocked(a.stations[0], "test")
	a.mu.Unlock()
	defer s.stop()
	if ok, detail := a.Control("stop"); !ok || detail != "stopped" {
		t.Fatalf("stop = %t %q", ok, detail)
	}
	before := a.displayState()

	a.HandleSettings(settingsChanged(2, map[string]string{"station": "2"}))
	a.HandleSettings(settingsChanged(3, map[string]string{
		"stations": `"A | https://a.test/a-new.mp3\nB | https://b.test/b-new.mp3"`,
	}))
	time.Sleep(20 * time.Millisecond)

	state := a.displayState()
	a.mu.Lock()
	current, halted, number := a.session, a.halted, a.stationNumber
	a.mu.Unlock()
	if current != nil || !halted || state.phase != PhasePaused {
		t.Fatalf("settings edit changed the stop hold: session=%v halted=%t phase=%v", current, halted, state.phase)
	}
	if number != 2 {
		t.Fatalf("station = %d, want the updated selection 2", number)
	}
	if !state.exitAt.Equal(before.exitAt) || !state.displayUntil.Equal(before.displayUntil) {
		t.Fatalf("settings edit moved the hold: exit %v -> %v display %v -> %v",
			before.exitAt, state.exitAt, before.displayUntil, state.displayUntil)
	}
}

// A settings retune is queued as a goroutine, so it must re-check the state it
// was queued from under the same lock that publishes the new session. A Stop
// or a newer snapshot between those points wins; the delayed retune does
// nothing.
func TestSettingsRetuneRequiresCurrentState(t *testing.T) {
	t.Run("stop-hold", func(t *testing.T) {
		a := testApp(t)
		a.pauseWindow = time.Minute
		a.mu.Lock()
		a.stations = []Station{
			{Name: "A", URL: "https://a.test/a.mp3"},
			{Name: "B", URL: "https://b.test/b.mp3"},
		}
		current := a.startSessionLocked(a.stations[0], "first")
		a.stationNumber = 2
		a.settingsRevision = 4
		a.settingsRetuneGen++
		guard := retuneGuard{
			settings: a.settingsRetuneGen,
			state:    a.exitSeq,
			session:  current,
		}
		a.mu.Unlock()
		defer current.stop()

		if ok, _ := a.Control("stop"); !ok {
			t.Fatal("stop was refused")
		}
		before := a.displayState()
		if ok, _ := a.retuneFromSettings(guard); ok {
			t.Fatal("a pending settings retune published during a Stop hold")
		}
		state := a.displayState()
		a.mu.Lock()
		currentSession, halted := a.session, a.halted
		a.mu.Unlock()
		if currentSession != nil || !halted || state.phase != PhasePaused {
			t.Fatalf("stale retune changed the stop hold: session=%v halted=%t phase=%v", currentSession, halted, state.phase)
		}
		if !state.exitAt.Equal(before.exitAt) || !state.displayUntil.Equal(before.displayUntil) {
			t.Fatalf("stale retune moved the hold: exit %v -> %v display %v -> %v",
				before.exitAt, state.exitAt, before.displayUntil, state.displayUntil)
		}
	})

	t.Run("newer-snapshot", func(t *testing.T) {
		a := testApp(t)
		a.mu.Lock()
		a.stations = []Station{
			{Name: "A", URL: "https://a.test/a.mp3"},
			{Name: "B", URL: "https://b.test/b.mp3"},
		}
		current := a.startSessionLocked(a.stations[0], "first")
		a.stationNumber = 2
		a.settingsRevision = 4
		a.settingsRetuneGen++
		guard := retuneGuard{
			settings: a.settingsRetuneGen,
			state:    a.exitSeq,
			session:  current,
		}
		a.mu.Unlock()
		defer current.stop()

		// The newer snapshot selects the station already playing, so it needs
		// no retune of its own; it still supersedes the queued one.
		a.HandleSettings(settingsChanged(5, map[string]string{"station": "1"}))
		if ok, _ := a.retuneFromSettings(guard); ok {
			t.Fatal("a superseded settings snapshot published a retune")
		}
		a.mu.Lock()
		currentSession, phase, number := a.session, a.phase, a.stationNumber
		a.mu.Unlock()
		if currentSession != current || phase != PhaseBuffering || number != 1 {
			t.Fatalf("superseded retune changed state: session=%p want %p phase=%v station=%d",
				currentSession, current, phase, number)
		}
	})
}

// A valid settings edit still retunes the active selection; the guard only
// rejects stale state. The speaker probe failure keeps this test off the
// network while proving the queued retune reached publication.
func TestSettingsChangeRetunesActiveSelection(t *testing.T) {
	a := testApp(t)
	a.speakerAvailable = false
	a.mu.Lock()
	a.stations = []Station{
		{Name: "A", URL: "https://a.test/a.mp3"},
		{Name: "B", URL: "https://b.test/b.mp3"},
	}
	s := a.startSessionLocked(a.stations[0], "test")
	a.mu.Unlock()
	defer s.stop()

	a.HandleSettings(settingsChanged(2, map[string]string{"station": "2"}))
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		current, phase, number, detail := a.session, a.phase, a.stationNumber, a.detail
		a.mu.Unlock()
		if current == nil && phase == PhaseError && number == 2 &&
			detail == "no speaker endpoint on this device" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("settings retune state = session %v phase %v station %d detail %q",
				current, phase, number, detail)
		}
		time.Sleep(time.Millisecond)
	}
}

// A station step on an idle run keeps it available for the following Play
// instead of letting the bare-launch grace expire underneath the user.
func TestIdleStepRefreshesIdleExit(t *testing.T) {
	a := testApp(t)
	a.idleWindow = 150 * time.Millisecond
	a.mu.Lock()
	a.armExitLocked(a.idleWindow, "idle")
	a.mu.Unlock()

	time.Sleep(30 * time.Millisecond)
	a.mu.Lock()
	// The step's compare-and-swap write is covered elsewhere; keep this test
	// on the idle-window rule without a live app bus.
	a.persistedStation = 2
	a.mu.Unlock()
	if ok, detail := a.Control("next"); !ok {
		t.Fatalf("next = %t %q", ok, detail)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-a.exitCh:
		t.Fatal("a stepped idle run expired without its refreshed window")
	default:
	}
	select {
	case <-a.exitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the refreshed idle window never expired")
	}
}

// Playback Stop uses the same bounded hold as Pause: the stream closes, the
// pause sign owns the display for the pause window, and Play can resume before
// the run exits. The media contract reports the hold as STOPPED, not PAUSED.
func TestPlaybackStopShowsBarsThenExits(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := testApp(t)
	a.now = func() time.Time { return start }
	a.pauseWindow = 40 * time.Millisecond
	a.alwaysDisplay = true

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()

	if ok, detail := a.Control("stop"); !ok || detail != "stopped" {
		t.Fatalf("stop = %t %q", ok, detail)
	}
	state := a.displayState()
	if state.phase != PhasePaused {
		t.Fatalf("phase = %v, want paused hold", state.phase)
	}
	if !state.displayUntil.Equal(start.Add(a.pauseWindow)) {
		t.Fatalf("stop deadline = %v, want %v", state.displayUntil, start.Add(a.pauseWindow))
	}
	out := displayOutputFor(state, start)
	if !out.hold || out.colon || len(out.cells) != 4 ||
		out.cells[1].Mask != segmentPauseLeft || out.cells[2].Mask != segmentPauseRight {
		t.Fatalf("stopped output = %+v, want the pause bars", out)
	}
	if status := a.mediaStatus(); status["playbackState"] != "PLAYBACK_STOPPED" {
		t.Fatalf("playbackState = %v, want STOPPED", status["playbackState"])
	}
	actions, _ := a.mediaStatus()["supportedActions"].([]any)
	if len(actions) == 0 || actions[0] != "MEDIA_PLAY" {
		t.Fatalf("supportedActions = %v, want MEDIA_PLAY first", actions)
	}
	select {
	case <-a.exitCh:
		t.Fatal("the stop hold ended before its deadline")
	default:
	}
	select {
	case <-a.exitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the stop hold did not signal its exit")
	}
	state = a.displayState()
	if state.phase != PhaseStopped || !a.exiting {
		t.Fatalf("expired stop = phase %v exiting %t, want stopped/true", state.phase, a.exiting)
	}
	if displayOutputFor(state, start).hold {
		t.Fatal("expired stop still owned the display")
	}
}

// Dismiss is administrative: it releases the display and ends the run without
// a hold, and a new Play cannot revive the exited run.
func TestDismissEndsTheRunImmediately(t *testing.T) {
	a := testApp(t)
	a.alwaysDisplay = true
	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()

	if ok, _ := a.Control("dismiss"); !ok {
		t.Fatal("dismiss was refused")
	}
	state := a.displayState()
	if state.phase != PhaseStopped || !state.displayUntil.IsZero() {
		t.Fatalf("dismiss state = %+v", state)
	}
	if displayOutputFor(state, time.Now()).hold {
		t.Fatal("dismiss kept display ownership")
	}
	select {
	case <-a.exitCh:
	case <-time.After(time.Second):
		t.Fatal("dismiss did not signal the run exit")
	}
	if ok, _ := a.Control("play"); ok {
		t.Fatal("dismiss accepted a new play after the run ended")
	}
}

// A terminal error ends the run when its indication ends. With the setting off
// that is the 10 s tuning window; with it on the error is bounded by one idle
// window, never an indefinite resident process.
func TestTerminalErrorExitsWhenItsWindowEnds(t *testing.T) {
	t.Run("tuning-window", func(t *testing.T) {
		a := testApp(t)
		a.mu.Lock()
		s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
		a.displayUntil = time.Now().Add(20 * time.Millisecond)
		a.mu.Unlock()

		a.failSession(s, "boom")
		select {
		case <-a.exitCh:
		case <-time.After(2 * time.Second):
			t.Fatal("terminal error did not signal its exit at the tuning deadline")
		}
		if state := a.displayState(); state.phase != PhaseStopped {
			t.Fatalf("expired error phase = %v, want stopped", state.phase)
		}
	})

	t.Run("always-display", func(t *testing.T) {
		start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		a := testApp(t)
		a.now = func() time.Time { return start }
		a.alwaysDisplay = true
		a.idleWindow = 20 * time.Millisecond
		a.mu.Lock()
		s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
		a.mu.Unlock()

		a.failSession(s, "boom")
		state := a.displayState()
		if !displayOutputFor(state, start.Add(10*time.Millisecond)).hold {
			t.Fatal("always-display error released before the run exit")
		}
		if displayOutputFor(state, start.Add(a.idleWindow)).hold {
			t.Fatal("always-display error outlived the run")
		}
		select {
		case <-a.exitCh:
		case <-time.After(2 * time.Second):
			t.Fatal("always-display error did not end the run")
		}
	})
}

// The terminal exit path releases the dependent capture before the
// presentation, so the selected baseline returns and no blank frame is sent.
func TestExitReleasesPresentationAndCapture(t *testing.T) {
	a := testApp(t)
	host := newFakeDisplayHost()
	a.display = host
	a.displayWindow = time.Hour
	go a.displayLoop()

	a.mu.Lock()
	s := a.startSessionLocked(Station{Name: "A", URL: "https://a.test/a.mp3"}, "test")
	a.mu.Unlock()
	defer s.stop()
	a.requestDraw()

	var presentation *fakePresentation
	select {
	case presentation = <-host.opened:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not open a presentation")
	}
	var capture *fakeCapture
	select {
	case capture = <-host.captured:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not acquire the dependent capture")
	}
	select {
	case <-presentation.frame:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not publish a frame")
	}

	a.stop("stop")
	select {
	case <-a.exitCh:
	case <-time.After(time.Second):
		t.Fatal("stop did not signal the exit")
	}
	a.Close()
	select {
	case <-a.displayDone:
	case <-time.After(2 * time.Second):
		t.Fatal("display loop did not stop")
	}
	if events := host.log.snapshot(); len(events) != 2 || events[0] != "capture" || events[1] != "presentation" {
		t.Fatalf("release order = %v, want [capture presentation]", events)
	}
	if _, closed := presentation.snapshot(); !closed {
		t.Fatal("presentation was not closed on run exit")
	}
	select {
	case <-capture.closedC:
	default:
		t.Fatal("capture was not closed on run exit")
	}
}

// An accepted next/previous selection must survive an immediate Dismiss.
// Close cancels the app context (which aborts the asynchronous write that was
// already in flight), releases the outputs, and then flushes the newest
// selection with a fresh bounded context.
func TestCloseFlushesAcceptedSelection(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stations = []Station{
		{Name: "A", URL: "https://a.test/a.mp3"},
		{Name: "B", URL: "https://b.test/b.mp3"},
	}
	a.stationNumber = 2
	a.persistedStation = 1
	a.settingsRevision = 4
	a.selectionDirty = true
	a.mu.Unlock()

	first := make(chan struct{})
	calls := 0
	written := 0
	a.updateStationFn = func(ctx context.Context, station int, revision uint64) (uint64, error) {
		calls++
		if calls == 1 {
			close(first)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		written = station
		return revision + 1, nil
	}
	go a.persistSelection()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("the asynchronous settings write did not start")
	}
	close(a.displayDone) // testApp has no display loop to finish
	a.Close()

	if written != 2 {
		t.Fatalf("shutdown flushed station %d, want the accepted 2", written)
	}
	a.mu.Lock()
	persisted := a.persistedStation
	a.mu.Unlock()
	if persisted != 2 {
		t.Fatalf("persistedStation = %d, want 2", persisted)
	}
}

// controlledStationStore is a minimal authoritative app-settings store. It
// enforces the same compare-and-swap revision rule as the daemon, so a test
// can reproduce a write that commits while its RPC acknowledgment is lost.
type controlledStationStore struct {
	mu       sync.Mutex
	station  int
	revision uint64
}

func (s *controlledStationStore) update(station int, expected uint64) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected != s.revision {
		return 0, fmt.Errorf("app settings revision %d is stale (current %d)", expected, s.revision)
	}
	s.station = station
	s.revision++
	return s.revision, nil
}

func (s *controlledStationStore) get() (int, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.station, s.revision
}

// A write can reach the daemon, commit, and still look cancelled in the app:
// Dismiss cancels the RPC before its response arrives. The cached
// persistedStation then still names the previous station, so the shutdown
// flush must not trust it. It has to fence the uncertain write with a fresh
// compare-and-swap write of the newest accepted selection.
func TestCloseFencesCommittedWriteWithoutAck(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stations = []Station{
		{Name: "A", URL: "https://a.test/a.mp3"},
		{Name: "B", URL: "https://b.test/b.mp3"},
	}
	a.stationNumber = 1
	a.persistedStation = 1
	a.settingsRevision = 4
	a.mu.Unlock()

	store := &controlledStationStore{station: 1, revision: 4}
	committed := make(chan struct{})
	var commitOnce sync.Once
	a.updateStationFn = func(ctx context.Context, station int, revision uint64) (uint64, error) {
		if station != 2 {
			return store.update(station, revision)
		}
		if _, err := store.update(station, revision); err != nil {
			return 0, err
		}
		// The daemon stored station 2, but the app only ever sees the
		// cancellation of this RPC.
		commitOnce.Do(func() { close(committed) })
		<-ctx.Done()
		return 0, ctx.Err()
	}
	a.readRevisionFn = func(context.Context) (uint64, bool) {
		_, revision := store.get()
		return revision, true
	}

	if ok, detail := a.Control("next"); !ok {
		t.Fatalf("next = %t %q", ok, detail)
	}
	select {
	case <-committed:
	case <-time.After(2 * time.Second):
		t.Fatal("the first station write did not commit")
	}
	// Step back to the saved station before the write result is known.
	if ok, detail := a.Control("previous"); !ok {
		t.Fatalf("previous = %t %q", ok, detail)
	}
	close(a.displayDone) // testApp has no display loop to finish
	a.Close()

	if station, revision := store.get(); station != 1 || revision != 6 {
		t.Fatalf("stored station/revision = %d/%d, want 1/6", station, revision)
	}
	a.mu.Lock()
	persisted, dirty, unsettled := a.persistedStation, a.selectionDirty, a.unsettledRevision
	a.mu.Unlock()
	if persisted != 1 || dirty || unsettled != 0 {
		t.Fatalf("selection state = persisted %d dirty %t unsettled %d, want 1/false/0", persisted, dirty, unsettled)
	}
}

// The writer must sample the current selection and arm the uncertain revision
// under one continuous a.mu hold. If it unlocks in between, a Previous or a
// same-revision echo can clear selectionDirty while the sampled CAS is still
// about to be sent; that obsolete CAS then commits with no pending flush.
// TryLock from the sampling seam proves the mutex is still held.
func TestPersistSamplesAndArmsUnsettledUnderOneLock(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stationNumber = 2
	a.persistedStation = 1
	a.settingsRevision = 4
	a.selectionDirty = true
	a.mu.Unlock()

	sampled := make(chan struct{}, 1)
	released := false
	a.persistSampledFn = func() {
		if a.mu.TryLock() {
			released = true
			a.mu.Unlock()
		}
		select {
		case sampled <- struct{}{}:
		default:
		}
	}
	a.updateStationFn = func(_ context.Context, _ int, revision uint64) (uint64, error) {
		return revision + 1, nil
	}

	a.persistSelection()
	select {
	case <-sampled:
	default:
		t.Fatal("the persistence writer did not sample the selection")
	}
	if released {
		t.Fatal("the persistence writer released a.mu between sampling and arming the uncertain write")
	}
}

// A snapshot at or below the expected revision of an unsettled write cannot
// prove which station is durable. Applying it would clear the dirty bit while
// the commit is still in flight, so shutdown would skip the final write. A
// snapshot past that revision is authoritative and does settle the selection.
func TestUnsettledWriteSnapshotCannotClearSelection(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stationNumber = 1
	a.persistedStation = 0
	a.settingsRevision = 4
	a.unsettledRevision = 4
	a.selectionDirty = true
	a.mu.Unlock()
	// Keep the snapshot-triggered retry in flight so it cannot acknowledge
	// this simulated unsettled write before the assertions below.
	a.updateStationFn = func(ctx context.Context, _ int, _ uint64) (uint64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}

	a.HandleSettings(settingsChanged(4, map[string]string{"station": "1"}))
	a.mu.Lock()
	number, persisted, dirty := a.stationNumber, a.persistedStation, a.selectionDirty
	a.mu.Unlock()
	if number != 1 || persisted != 0 || !dirty {
		t.Fatalf("stale snapshot state = station %d persisted %d dirty %t, want 1/0/true", number, persisted, dirty)
	}

	a.HandleSettings(settingsChanged(5, map[string]string{"station": "1"}))
	a.mu.Lock()
	number, persisted, dirty, unsettled := a.stationNumber, a.persistedStation, a.selectionDirty, a.unsettledRevision
	a.mu.Unlock()
	if number != 1 || persisted != 1 || dirty || unsettled != 0 {
		t.Fatalf("settling snapshot state = station %d persisted %d dirty %t unsettled %d, want 1/1/false/0",
			number, persisted, dirty, unsettled)
	}
}

// The shutdown flush is bounded: an unresponsive configuration service must
// not hold the process after its audio and display are already released.
func TestFlushSelectionIsBounded(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stationNumber = 2
	a.persistedStation = 1
	a.settingsRevision = 1
	a.selectionDirty = true
	a.mu.Unlock()
	a.updateStationFn = func(ctx context.Context, _ int, _ uint64) (uint64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}

	start := time.Now()
	a.flushSelection(40 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bounded flush took %v", elapsed)
	}
}

// Reconciliation after a stale revision must stay inside the flush deadline
// even when the configuration service never answers the refresh read.
func TestFlushSelectionReconcileIsBounded(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stationNumber = 1
	a.persistedStation = 0
	a.settingsRevision = 4
	a.selectionDirty = true
	a.mu.Unlock()

	a.updateStationFn = func(context.Context, int, uint64) (uint64, error) {
		return 0, fmt.Errorf("app settings revision 4 is stale (current 5)")
	}
	reconciled := make(chan struct{}, 1)
	a.readRevisionFn = func(ctx context.Context) (uint64, bool) {
		reconciled <- struct{}{}
		<-ctx.Done()
		return 0, false
	}

	start := time.Now()
	a.flushSelection(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bounded reconcile took %v", elapsed)
	}
	select {
	case <-reconciled:
	default:
		t.Fatal("the stale write did not trigger a revision reconcile")
	}
}

// A write that ignores cancellation must not hold the flush past its own
// deadline: the gate is bail-able, unlike a mutex.
func TestFlushSelectionDoesNotWaitForStuckWrite(t *testing.T) {
	a := testApp(t)
	a.mu.Lock()
	a.stationNumber = 2
	a.persistedStation = 1
	a.settingsRevision = 1
	a.selectionDirty = true
	a.mu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	a.updateStationFn = func(context.Context, int, uint64) (uint64, error) {
		close(started)
		<-release
		return 0, nil
	}
	go a.persistSelection()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the asynchronous settings write did not start")
	}
	start := time.Now()
	a.flushSelection(40 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("flush waited %v for a stuck write", elapsed)
	}
	close(release)
}

// A bare launch of an already-idle run is a user asking for the radio to be
// available; it refreshes the idle window without starting audio.
func TestBareInvocationRefreshesIdleExit(t *testing.T) {
	a := testApp(t)
	a.client = &capsulad.Client{}
	close(a.settingsReady)
	a.idleWindow = 150 * time.Millisecond
	a.mu.Lock()
	a.armExitLocked(a.idleWindow, "idle")
	a.mu.Unlock()

	time.Sleep(30 * time.Millisecond)
	a.HandleInvocation(context.Background(), &capsulad.LaunchInvocation{ID: "inv-1"})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-a.exitCh:
		t.Fatal("the bare invocation did not refresh the idle window")
	default:
	}
	select {
	case <-a.exitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the refreshed idle window never expired")
	}
}
