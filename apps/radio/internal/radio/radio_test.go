package radio

import (
	"context"
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
	return &App{
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
	// grab the display again just to show the error pattern.
	a.failSession(s, "late failure")
	state := a.displayState()
	if displayOutputFor(state, start.Add(defaultDisplayWindow+time.Second)).hold {
		t.Fatal("a late error reacquired the segment display")
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
		if phase == PhaseError {
			continue
		}
		if len(out.cells) != 4 || out.cells[2].Mask != segmentDigits[4] || out.cells[3].Mask != segmentDigits[2] {
			t.Fatalf("phase %v cells = %+v, want station 42", phase, out.cells)
		}
		if got := out.cells[3].DecimalPoint; got != (phase == PhasePaused) {
			t.Fatalf("phase %v decimal point = %t", phase, got)
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
